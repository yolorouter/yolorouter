package providerclient

// The Kling image probe against a live stub: a submit the stub accepts and
// a task it drives to a terminal succeed state, judged end to end.

import (
	"io"
	"net/http"
	"testing"
)

func TestKlingImageProbePassesOnSucceedTask(t *testing.T) {
	prev := isKlingBase
	isKlingBase = func(string) bool { return true }
	t.Cleanup(func() { isKlingBase = prev })

	var submitBody []byte
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations":
			submitBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"task_id":"951","task_status":"submitted"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/images/generations/951":
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_status":"succeed","final_unit_deduction":"0.02","task_result":{"images":[{"index":0,"url":"https://x.test/1.png"}]}}}`))
		default:
			t.Errorf("unexpected probe route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "kling-v3")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("a kling submit+succeed round trip must pass, got %+v", res)
	}
	if len(submitBody) == 0 {
		t.Fatal("the probe submit must reach the stub")
	}
}

func TestKlingImageProbeReportsFailedTask(t *testing.T) {
	prev := isKlingBase
	isKlingBase = func(string) bool { return true }
	t.Cleanup(func() { isKlingBase = prev })

	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/images/generations":
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_id":"952","task_status":"submitted"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/images/generations/952":
			_, _ = w.Write([]byte(`{"code":0,"data":{"task_status":"failed","task_status_msg":"content risk control"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "kling-v3")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestUpstreamError || res.Detail == "" {
		t.Fatalf("a failed task must fail the probe with its own words, got %+v", res)
	}
}
