package providerclient

// The video probe battery: a mapping is healthy when the task
// conversation works — a submit the upstream accepts and one query it
// answers — not when a render finishes inside the probe's budget.

import (
	"encoding/json"
	"io"
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

func TestVideoGenerationProbePassesOnArkBase(t *testing.T) {
	// A local server is neither vendor by hostname; both gates flip on
	// for this one test so the ark branch is exercised against a live
	// stub.
	prevArk := isArkBase
	isArkBase = func(string) bool { return true }
	t.Cleanup(func() { isArkBase = prevArk })

	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/contents/generations/tasks":
			if r.Header.Get("X-DashScope-Async") != "" {
				t.Errorf("the ark dialect carries no async header, got %q", r.Header.Get("X-DashScope-Async"))
			}
			_, _ = w.Write([]byte(`{"id":"cgt-1"}`))
		case "/api/v3/contents/generations/tasks/cgt-1":
			_, _ = w.Write([]byte(`{"id":"cgt-1","status":"running"}`))
		default:
			t.Errorf("unexpected probe path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL+"/api/v3", "sk-test", "doubao-seedance-2-0-260128")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("an ark submit+query round trip must pass, got %+v", res)
	}
}

func TestVideoGenerationProbePassesOnKlingBase(t *testing.T) {
	// A local server is no vendor by hostname; the kling gate flips on
	// and the others off so the kling branch is exercised against a live
	// stub.
	prevKling := isKlingBase
	isKlingBase = func(string) bool { return true }
	prevArk := isArkBase
	isArkBase = func(string) bool { return false }
	t.Cleanup(func() {
		isKlingBase = prevKling
		isArkBase = prevArk
	})

	var submitBody []byte
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/text-to-video/kling-3.0-turbo":
			buf, _ := io.ReadAll(r.Body)
			submitBody = buf
			_, _ = w.Write([]byte(`{"code":0,"message":"SUCCEED","data":{"id":"893605","status":"submitted"}}`))
		case r.URL.Path == "/tasks" && r.URL.Query().Get("task_ids") == "893605":
			_, _ = w.Write([]byte(`{"code":0,"data":[{"id":"893605","status":"processing"}]}`))
		default:
			t.Errorf("unexpected probe path %s", r.URL.RequestURI())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	res, err := c.TestVideoGeneration(t.Context(), srv.URL, "sk-test", "kling-3.0-turbo")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("a kling submit+query round trip must pass, got %+v", res)
	}
	// The probe's ask is the cheapest clip the dialect can request — a
	// probe task renders and bills for real.
	var sent struct {
		Settings struct {
			Duration int `json:"duration"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(submitBody, &sent); err != nil {
		t.Fatalf("probe submit body: %v", err)
	}
	if sent.Settings.Duration != 3 {
		t.Fatalf("the kling probe must ask for the 3-second floor, got %d", sent.Settings.Duration)
	}
}
