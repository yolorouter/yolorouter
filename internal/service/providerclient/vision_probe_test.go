package providerclient

// The image-input chat probe battery: a mapping takes vision when a minimal
// image-carrying chat request comes back as a certified completion, and the
// probe maps a 400/415 — the upstream refusing the request's own shape — to
// TestRequestRejected, the one decisive negative the vision whitelist reads.
// Every other failure classifies through the shared rules, because auth
// failures, rate limits and 5xx answers say nothing about vision either way.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// The status-driven outcomes, asserted against the OpenAI wire shape; the
// per-protocol body tests below hold the other three protocols to the same
// statuses where their shapes differ.
func TestImageInputChatClassifiesStatuses(t *testing.T) {
	openAISuccess := `{"choices":[{"message":{"role":"assistant","content":"blue"}}]}`

	tests := []struct {
		name       string
		status     int
		body       string
		contentype string
		want       TestOutcome
	}{
		{"certified completion passes", http.StatusOK, openAISuccess, "application/json", TestSuccess},
		{"error-carrying 200 is ambiguous", http.StatusOK, `{"error":{"message":"overloaded"}}`, "application/json", TestUpstreamError},
		{"404 means the model is not served", http.StatusNotFound, `{"error":{"message":"model not found"}}`, "application/json", TestModelNotFound},
		{"400 is a request-shape rejection", http.StatusBadRequest, `{"error":{"message":"image input is not supported for this model"}}`, "application/json", TestRequestRejected},
		{"415 is a request-shape rejection", http.StatusUnsupportedMediaType, `unsupported media type`, "text/plain", TestRequestRejected},
		{"401 says nothing about vision", http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, "application/json", TestAuthFailed},
		{"429 says nothing about vision", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, "application/json", TestRateLimited},
		{"500 says nothing about vision", http.StatusInternalServerError, `upstream exploded`, "text/plain", TestUpstreamError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				if tt.contentype != "" {
					w.Header().Set("Content-Type", tt.contentype)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			})
			defer srv.Close()

			res, err := c.TestImageInputChat(t.Context(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-5-vision")
			if err != nil {
				t.Fatalf("probe errored: %v", err)
			}
			if res.Outcome != tt.want {
				t.Fatalf("outcome = %v, want %v (detail %q)", res.Outcome, tt.want, res.Detail)
			}
		})
	}
}

// The rejection detail must carry the upstream's own message: the verdict
// says the model refused the image, the detail says what the upstream
// actually complained about.
func TestImageInputChatRejectionCarriesUpstreamDetail(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"this model does not support image input"}}`))
	})
	defer srv.Close()

	res, err := c.TestImageInputChat(t.Context(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "text-only")
	if err != nil {
		t.Fatalf("probe errored: %v", err)
	}
	if res.Outcome != TestRequestRejected {
		t.Fatalf("outcome = %v, want TestRequestRejected", res.Outcome)
	}
	if !strings.Contains(res.Detail, "does not support image input") {
		t.Fatalf("detail %q does not carry the upstream message", res.Detail)
	}
}

// Each protocol's probe body must carry the image in that protocol's own
// spelling, so a refusal from a protocol-correct request can only be about
// the image itself.
func TestImageInputChatSendsEachProtocolsOwnImageShape(t *testing.T) {
	tests := []struct {
		name  string
		proto protocols.ProtocolID
		// success is the 200 body that protocol's own validator certifies.
		success string
		// wantParts are substrings the wire body must carry.
		wantParts []string
	}{
		{
			name:      "openai rides the chat path with a data-URI image part",
			proto:     protocols.ProtocolOpenAI,
			success:   `{"choices":[{"message":{"role":"assistant","content":"blue"}}]}`,
			wantParts: []string{`"type":"image_url"`, `"data:image/png;base64,`, visionProbePrompt},
		},
		{
			name:      "claude rides the messages path with a base64 source block",
			proto:     protocols.ProtocolClaude,
			success:   `{"type":"message","role":"assistant","content":[{"type":"text","text":"blue"}]}`,
			wantParts: []string{`"type":"image"`, `"media_type":"image/png"`, `"data":"` + visionProbeImageBase64, visionProbePrompt},
		},
		{
			name:      "gemini rides generateContent with an inlineData part",
			proto:     protocols.ProtocolGemini,
			success:   `{"candidates":[{"content":{"parts":[{"text":"blue"}]}}]}`,
			wantParts: []string{`"inlineData"`, `"mimeType":"image/png"`, `"data":"` + visionProbeImageBase64, visionProbePrompt},
		},
		{
			name:      "responses rides the responses path with an input_image part",
			proto:     protocols.ProtocolResponses,
			success:   `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"blue"}]}]}`,
			wantParts: []string{`"type":"input_image"`, `"data:image/png;base64,`, visionProbePrompt},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body string
			c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
				body = readAll(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.success))
			})
			defer srv.Close()

			res, err := c.TestImageInputChat(t.Context(), tt.proto, srv.URL, "sk-test", "vision-model")
			if err != nil {
				t.Fatalf("probe errored: %v", err)
			}
			if res.Outcome != TestSuccess {
				t.Fatalf("outcome = %v, want TestSuccess (detail %q)", res.Outcome, res.Detail)
			}
			for _, want := range tt.wantParts {
				if !strings.Contains(body, want) {
					t.Errorf("probe body does not carry %s: %s", want, body)
				}
			}
		})
	}
}

// The inline image is a real PNG, not a placeholder string: a gateway that
// content-inspects uploads (or a provider that sniffs bytes) must see a
// decodable image, or a working vision model could fail its probe on image
// validity rather than answer the question asked.
func TestImageInputChatCarriesADecodablePNG(t *testing.T) {
	if len(visionProbeImage) == 0 {
		t.Fatal("vision probe image is empty")
	}
	if string(visionProbeImage[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("vision probe image does not start with the PNG signature: %x", visionProbeImage[:8])
	}
}
