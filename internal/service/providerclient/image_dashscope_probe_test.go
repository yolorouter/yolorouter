package providerclient

// The image probe on a DashScope-compatible base must speak the native
// multimodal-generation dialect — the same one the gateway's image delivery
// uses for those hosts. The OpenAI-shaped images path answers 404 there, so
// probing it would measure a failure no routed request would ever hit.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

// withDashScopeBase forces the probe's dashscope-host detection on for
// one test: a local httptest server can never carry the real hostname.
func withDashScopeBase(t *testing.T) {
	t.Helper()
	previous := isDashScopeBase
	isDashScopeBase = func(string) bool { return true }
	t.Cleanup(func() { isDashScopeBase = previous })
}

// Mirrors the endpoint as actually observed: image items carry only the URL,
// no type tag.
const nativeSuccessBody = `{"request_id":"t-1","output":{"choices":[{"finish_reason":"stop","message":{"content":[{"image":"https://img.example/t.png"}]}}],"finished":true},"usage":{"input_tokens":4,"output_tokens":5},"created":1}`

func TestImageGenerationProbesDashScopeNativeEndpoint(t *testing.T) {
	withDashScopeBase(t)

	var gotPath, gotAuth, gotContentType string
	var gotBody map[string]any
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(nativeSuccessBody))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL+"/compatible-mode/v1", "sk-test", "wan2.7-image")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("expected success against the native endpoint, got %+v", res)
	}
	if gotPath != images.GenerationPath {
		t.Fatalf("probe must hit the native generation path from the origin, got %q", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("native endpoint expects bearer auth, got %q", gotAuth)
	}
	if !strings.Contains(gotContentType, "json") {
		t.Fatalf("native probe must send JSON, got %q", gotContentType)
	}
	if gotBody["model"] != "wan2.7-image" {
		t.Fatalf("native body must carry the model id, got %v", gotBody["model"])
	}
	input, _ := gotBody["input"].(map[string]any)
	messages, _ := input["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("native body must carry one user message, got %v", input)
	}
}

func TestImageGenerationDashScopeBusinessErrorFails(t *testing.T) {
	withDashScopeBase(t)

	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"request_id":"t-2","code":"InvalidParameter","message":"n must be positive"}`))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL+"/compatible-mode/v1", "sk-test", "wan2.7-image")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome == TestSuccess {
		t.Fatalf("a 200-carried business refusal must not pass, got %+v", res)
	}
	if !strings.Contains(res.Detail, "InvalidParameter") || !strings.Contains(res.Detail, "n must be positive") {
		t.Fatalf("the refusal must surface the upstream's own code and message, got %q", res.Detail)
	}
}

func TestImageGenerationDashScopeNon200ClassifiesByStatus(t *testing.T) {
	withDashScopeBase(t)

	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":"InvalidModel","message":"model not exist"}`))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "wan2.7-image")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestModelNotFound {
		t.Fatalf("a 404 must classify as model-not-found, got %+v", res)
	}
	if !strings.Contains(res.Detail, "model not exist") {
		t.Fatalf("the upstream message must survive into the detail, got %q", res.Detail)
	}
}

// The OpenAI-shaped branch of the same probe: success must require an
// actual images answer, not just any parseable JSON 200.
func TestImageGenerationOpenAIShapeRequiresDataArray(t *testing.T) {
	t.Run("data array passes, empty included", func(t *testing.T) {
		c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"data":[]}`))
		})
		defer srv.Close()
		res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "some-image-model")
		if err != nil {
			t.Fatalf("probe errored: %v", err)
		}
		if res.Outcome != TestSuccess {
			t.Fatalf("an empty data array proves the endpoint recognized the request, got %+v", res)
		}
	})
	t.Run("chat-shaped 200 is not an images answer", func(t *testing.T) {
		c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
		})
		defer srv.Close()
		res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "some-image-model")
		if err != nil {
			t.Fatalf("probe errored: %v", err)
		}
		if res.Outcome == TestSuccess {
			t.Fatalf("a 200 without a data array must not certify an image mapping, got %+v", res)
		}
	})
}

func TestImageGenerationDashScopeUnparseable200IsNotCertified(t *testing.T) {
	withDashScopeBase(t)

	// A 200 that is neither a native image answer nor a business refusal —
	// a chat-shaped body. Certifying it would repeat, on the native branch,
	// the hole the OpenAI branch closed: the shared classifier's 200
	// validator recognizes chat bodies.
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"}}]}`))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "wan2.7-image")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome == TestSuccess {
		t.Fatalf("a chat-shaped 200 must not certify a native image mapping, got %+v", res)
	}
	if res.Outcome != TestUpstreamError || res.Detail == "" {
		t.Fatalf("expected an upstream error carrying the decode failure, got %+v", res)
	}
}
