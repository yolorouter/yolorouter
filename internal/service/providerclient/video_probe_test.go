package providerclient

// The video probe battery: a mapping is healthy when the task
// conversation works — a submit the upstream accepts and one query it
// answers — not when a render finishes inside the probe's budget.

import (
	"net/http"
	"strings"
	"testing"
)

func TestVideoGenerationProbePassesOnRunningTask(t *testing.T) {
	withDashScopeBase(t)
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/services/aigc/video-generation/video-synthesis":
			if r.Header.Get("X-DashScope-Async") != "enable" {
				t.Errorf("submit must carry the async header, got %q", r.Header.Get("X-DashScope-Async"))
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				t.Errorf("submit must carry bearer auth, got %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"output":{"task_id":"t-1","task_status":"PENDING"},"request_id":"r"}`))
		case "/api/v1/tasks/t-1":
			// RUNNING answers the probe: the protocol works, the render
			// simply outlasts any sane probe budget.
			_, _ = w.Write([]byte(`{"output":{"task_status":"RUNNING"},"usage":{}}`))
		default:
			t.Errorf("unexpected probe path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL+"/compatible-mode/v1", "sk-test", "wan2.7-t2v")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("a submit+query round trip must pass, got %+v", res)
	}
}

func TestVideoGenerationProbeClassifiesSubmitFailures(t *testing.T) {
	withDashScopeBase(t)
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`))
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL, "sk-bad", "wan2.7-t2v")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestAuthFailed {
		t.Fatalf("a 401 submit must classify as auth failure, got %+v", res)
	}
}

func TestVideoGenerationProbeRefusesTasklessAcceptance(t *testing.T) {
	withDashScopeBase(t)
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"task_status":"PENDING"}}`))
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL, "sk-test", "wan2.7-t2v")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestUpstreamError {
		t.Fatalf("a 200 without a task id is not a healthy mapping, got %+v", res)
	}
}

func TestVideoGenerationProbeFailsOnDeadQuery(t *testing.T) {
	withDashScopeBase(t)
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/services/aigc/video-generation/video-synthesis":
			_, _ = w.Write([]byte(`{"output":{"task_id":"t-1","task_status":"PENDING"}}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL, "sk-test", "wan2.7-t2v")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestUpstreamError || !strings.Contains(res.Detail, "task query") {
		t.Fatalf("a failed task query must fail the probe with its own words, got %+v", res)
	}
}

func TestVideoGenerationProbeNonDashScopeBaseSaysSo(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("a non-dashscope base must not be probed over HTTP")
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL, "sk-test", "sora-2")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestUpstreamError || !strings.Contains(res.Detail, "dashscope") {
		t.Fatalf("the probe must name what it cannot do, got %+v", res)
	}
}
