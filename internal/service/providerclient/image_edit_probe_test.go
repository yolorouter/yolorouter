package providerclient

// The edit-shaped probe: a model whose name says "edit" requires a
// reference image, so its connectivity probe attaches one — multipart to
// the edits endpoint on an OpenAI-compatible base, native multimodal-generation
// with a data-URI image on a DashScope base. A model outside the edit
// family keeps the plain generation probe.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

const openAIEditProbeSuccess = `{"created":1700000000,"data":[{"url":"https://example.test/edited.png"}]}`

// An edit-shaped model on an OpenAI-compatible base is probed through the
// edits endpoint, with the reference image as a real multipart file part.
func TestEditShapedModelProbesOpenAIEditsEndpoint(t *testing.T) {
	var gotPath string
	var gotCT string
	var gotForm map[string][]string
	var gotFile []byte
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(gotCT)
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("probe content type = %q, want multipart", gotCT)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		form, err := multipart.NewReader(r.Body, params["boundary"]).ReadForm(8 << 20)
		if err != nil {
			t.Errorf("probe body did not parse: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotForm = form.Value
		files := form.File["image"]
		if len(files) != 1 {
			t.Errorf("probe files = %d, want the reference image", len(files))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, _ := files[0].Open()
		gotFile, _ = io.ReadAll(f)
		_ = f.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(openAIEditProbeSuccess))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "qwen-image-edit")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("expected success, got %+v", res)
	}
	if gotPath != "/v1/images/edits" {
		t.Fatalf("probe path = %q, want the edits endpoint", gotPath)
	}
	if gotForm["model"][0] != "qwen-image-edit" || gotForm["prompt"][0] != imageProbePrompt {
		t.Fatalf("probe form = %v, want the model and the probe prompt", gotForm)
	}
	if len(gotFile) == 0 || !bytes.HasPrefix(gotFile, []byte("\x89PNG")) {
		t.Fatalf("reference image = %d bytes starting %v, want a rendered PNG", len(gotFile), gotFile[:4])
	}
}

// An edit-shaped model on a DashScope base is probed through the native
// dialect with the reference image as a base64 data URI content item ahead
// of the prompt.
func TestEditShapedModelProbesDashScopeNativeWithImage(t *testing.T) {
	withDashScopeImageBase(t)

	var gotPath string
	var gotBody map[string]any
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode probe body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(nativeSuccessBody))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL+"/compatible-mode/v1", "sk-test", "qwen-image-edit")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("expected success against the native endpoint, got %+v", res)
	}
	if gotPath != images.GenerationPath {
		t.Fatalf("probe path = %q, want the native multimodal-generation endpoint", gotPath)
	}
	input, _ := gotBody["input"].(map[string]any)
	messages, _ := input["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("native body must carry one user message, got %v", input)
	}
	msg, _ := messages[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content items = %d, want the image then the prompt", len(content))
	}
	img, _ := content[0].(map[string]any)
	prefix := "data:image/png;base64,"
	if !strings.HasPrefix(img["image"].(string), prefix) {
		t.Fatalf("image item = %v, want a png data URI", img["image"])
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(img["image"].(string), prefix))
	if err != nil || !bytes.HasPrefix(decoded, []byte("\x89PNG")) {
		t.Fatalf("data URI payload is not the rendered PNG: %v", err)
	}
	text, _ := content[1].(map[string]any)
	if text["text"] != imageProbePrompt {
		t.Fatalf("prompt item = %v", text)
	}
}

// A model outside the edit family keeps the generation probe, even on an
// OpenAI-compatible base: the branch is the name, not the modality.
func TestNonEditModelKeepsGenerationProbe(t *testing.T) {
	var gotPath string
	var gotCT string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(openAIEditProbeSuccess))
	})
	defer srv.Close()

	res, err := c.TestImageGeneration(t.Context(), srv.URL, "sk-test", "wan2.7-image")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestSuccess {
		t.Fatalf("expected success, got %+v", res)
	}
	if gotPath != "/v1/images/generations" {
		t.Fatalf("probe path = %q, want the generations endpoint", gotPath)
	}
	if !strings.Contains(gotCT, "json") {
		t.Fatalf("generation probe must send JSON, got %q", gotCT)
	}
}
