package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestGeminiIngressToOpenAIUpstream_NonStream is an end-to-end proof that a
// native Gemini ingress request (/v1beta/models/{model}:generateContent)
// routed to an openai-type provider takes the cross-protocol IR round trip:
// the upstream must see an OpenAI Chat Completions request (path, Bearer
// auth), and the caller must receive a Gemini-shaped response (candidates +
// usageMetadata), never the upstream's own OpenAI shape or its
// provider_model_name.
func TestGeminiIngressToOpenAIUpstream_NonStream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var sawPath, sawAuth bool
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = strings.HasSuffix(r.URL.Path, "/chat/completions")
		sawAuth = r.Header.Get("Authorization") == "Bearer sk-openai-upstream"
		upstreamBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hello from openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:generateContent", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// --- upstream received an OpenAI-shaped request ---
	if !sawPath {
		t.Error("upstream did not receive a request ending in /chat/completions")
	}
	if !sawAuth {
		t.Error("upstream did not receive Authorization: Bearer sk-openai-upstream")
	}
	if !strings.Contains(string(upstreamBody), `"messages"`) {
		t.Errorf("upstream body missing OpenAI-shaped messages field: %s", upstreamBody)
	}

	// --- client received a Gemini-shaped response ---
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid Gemini JSON: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Candidates) != 1 || len(resp.Candidates[0].Content.Parts) != 1 || resp.Candidates[0].Content.Parts[0].Text != "hello from openai" {
		t.Fatalf("client candidates = %+v, want one candidate with text %q", resp.Candidates, "hello from openai")
	}
	if resp.UsageMetadata.PromptTokenCount != 10 || resp.UsageMetadata.CandidatesTokenCount != 4 || resp.UsageMetadata.TotalTokenCount != 14 {
		t.Errorf("client usageMetadata = %+v, want prompt=10 candidates=4 total=14 (mapped from OpenAI prompt_tokens/completion_tokens)", resp.UsageMetadata)
	}
	if strings.Contains(w.Body.String(), "gpt-4o-real") {
		t.Errorf("openai provider model name leaked into the client response: %s", w.Body.String())
	}
}

// TestGeminiIngressToOpenAIUpstream_Stream is the streaming counterpart:
// the upstream speaks OpenAI SSE (chat.completion.chunk + [DONE]), and the
// client — having sent a native Gemini :streamGenerateContent request — must
// receive Gemini SSE frames (bare "data: {...candidates...}", no [DONE]
// terminator), with the concatenated text and final usageMetadata correct,
// and the OpenAI provider model name must not leak.
func TestGeminiIngressToOpenAIUpstream_Stream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`data: {"id":"chatcmpl-xyz","model":"gpt-4o-real","choices":[{"delta":{"content":"Hello"},"finish_reason":null}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-xyz","model":"gpt-4o-real","choices":[{"delta":{"content":" world"},"finish_reason":null}]}` + "\n\n")
		write(`data: {"id":"chatcmpl-xyz","model":"gpt-4o-real","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}` + "\n\n")
		write("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:streamGenerateContent", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Gemini ingress must not emit the OpenAI [DONE] terminator: %s", body)
	}
	if strings.Contains(body, "gpt-4o-real") {
		t.Errorf("openai provider model name leaked into the client stream: %s", body)
	}

	var text strings.Builder
	var sawUsage bool
	var promptTokens, candidatesTokens, totalTokens int
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var chunk struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
				TotalTokenCount      int `json:"totalTokenCount"`
			} `json:"usageMetadata"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		for _, cand := range chunk.Candidates {
			for _, part := range cand.Content.Parts {
				text.WriteString(part.Text)
			}
		}
		if chunk.UsageMetadata != nil {
			sawUsage = true
			promptTokens = chunk.UsageMetadata.PromptTokenCount
			candidatesTokens = chunk.UsageMetadata.CandidatesTokenCount
			totalTokens = chunk.UsageMetadata.TotalTokenCount
		}
	}
	if text.String() != "Hello world" {
		t.Errorf("concatenated client stream text = %q, want %q", text.String(), "Hello world")
	}
	if !sawUsage {
		t.Fatalf("client Gemini stream missing a usageMetadata frame: %s", body)
	}
	if promptTokens != 8 || candidatesTokens != 3 || totalTokens != 11 {
		t.Errorf("client usageMetadata = prompt=%d candidates=%d total=%d, want prompt=8 candidates=3 total=11", promptTokens, candidatesTokens, totalTokens)
	}
}

// TestGeminiIngressToGeminiProvider_Passthrough confirms a native Gemini
// ingress request against a provider_type="gemini" provider is forwarded as
// same-protocol byte passthrough (no IR round trip): the upstream must
// receive the caller's own Gemini-shaped body (contents intact, no injected
// top-level "model" field — a native Gemini request has no such field, and
// the provider model name only ever reaches the upstream via the URL path)
// with the candidate's provider_model_name in the URL and the Gemini auth
// header, and the client must receive the upstream's Gemini response content
// and usage verbatim with "modelVersion" rewritten back to the external
// name, never the provider's own model name.
func TestGeminiIngressToGeminiProvider_Passthrough(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	var sawPath, sawAuthHeader, sawContents, sawNoTopLevelModel bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = strings.HasSuffix(r.URL.Path, "gemini-2.0-flash-real:generateContent")
		sawAuthHeader = r.Header.Get("x-goog-api-key") == "sk-gemini-upstream"
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(body, &parsed)
		sawContents = len(parsed["contents"]) > 0
		_, hasModel := parsed["model"]
		sawNoTopLevelModel = !hasModel

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"passthrough ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9},"modelVersion":"gemini-2.0-flash-real"}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createGeminiProvider(t, db, "gemini-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-gemini-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gemini-2.0-flash-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:generateContent", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// --- upstream received the native Gemini request ---
	if !sawPath {
		t.Error("upstream did not receive a :generateContent path carrying the provider model name")
	}
	if !sawAuthHeader {
		t.Error("upstream did not receive x-goog-api-key carrying the provider key")
	}
	if !sawContents {
		t.Error("upstream body missing non-empty contents (caller body was not forwarded verbatim)")
	}
	if !sawNoTopLevelModel {
		t.Error("upstream request body has an injected top-level \"model\" field — a native Gemini request must never carry one")
	}

	// --- client received the upstream's Gemini response verbatim ---
	var resp struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int `json:"promptTokenCount"`
			CandidatesTokenCount int `json:"candidatesTokenCount"`
			TotalTokenCount      int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
		ModelVersion string `json:"modelVersion"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("client response not valid Gemini JSON: %v; body=%s", err, w.Body.String())
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].Content.Parts[0].Text != "passthrough ok" {
		t.Fatalf("client candidates = %+v, want one candidate with text %q", resp.Candidates, "passthrough ok")
	}
	if resp.UsageMetadata.PromptTokenCount != 7 || resp.UsageMetadata.CandidatesTokenCount != 2 || resp.UsageMetadata.TotalTokenCount != 9 {
		t.Errorf("client usageMetadata = %+v, want prompt=7 candidates=2 total=9 (passthrough, unchanged)", resp.UsageMetadata)
	}
	if resp.ModelVersion != "gemini-2.0-flash" {
		t.Errorf("client response modelVersion = %q, want external name %q (passthrough must rewrite modelVersion, not add a top-level model field)", resp.ModelVersion, "gemini-2.0-flash")
	}
	if strings.Contains(w.Body.String(), "gemini-2.0-flash-real") {
		t.Errorf("provider model name leaked into the client response: %s", w.Body.String())
	}
}

// TestGeminiIngressToGeminiProvider_PassthroughStream is the streaming
// counterpart of TestGeminiIngressToGeminiProvider_Passthrough: a native
// Gemini streamGenerateContent request against a provider_type="gemini"
// provider is forwarded as same-protocol byte passthrough, and every
// forwarded chunk's "modelVersion" field — which the upstream stamps with
// its own provider_model_name — must be rewritten to the external name
// before it reaches the client or the per-request stream capture file used
// for audit/debugging.
func TestGeminiIngressToGeminiProvider_PassthroughStream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte(s))
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello"}]}}],"modelVersion":"gemini-2.0-flash-real"}` + "\n\n")
		write(`data: {"candidates":[{"content":{"role":"model","parts":[{"text":" world"}]}}],"modelVersion":"gemini-2.0-flash-real"}` + "\n\n")
		write(`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9},"modelVersion":"gemini-2.0-flash-real"}` + "\n\n")
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createGeminiProvider(t, db, "gemini-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-gemini-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gemini-2.0-flash-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	dir := t.TempDir()
	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:streamGenerateContent", reqBody)
	c.Set(BodiesDirContextKey, dir)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if strings.Contains(body, "gemini-2.0-flash-real") {
		t.Errorf("provider model name leaked into the client stream: %s", body)
	}
	if !strings.Contains(body, `"modelVersion":"gemini-2.0-flash"`) {
		t.Errorf("client stream missing modelVersion rewritten to the external name: %s", body)
	}

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	capturedBytes, err := os.ReadFile(filepath.Join(dir, captured.requestID+".stream"))
	if err != nil {
		t.Fatalf("read captured stream file: %v", err)
	}
	if strings.Contains(string(capturedBytes), "gemini-2.0-flash-real") {
		t.Errorf("provider model name leaked into the stream capture file: %s", capturedBytes)
	}
	if !bytes.Equal(capturedBytes, w.Body.Bytes()) {
		t.Errorf("captured stream file is not byte-for-byte equal to the client bytes.\ncaptured=%q\nclient=  %q", capturedBytes, w.Body.Bytes())
	}
}

// TestGeminiIngressAllCandidatesFailed_NativeError confirms that when every
// candidate fails (here: the sole candidate's upstream returns 502), the
// caller receives the Gemini-native error envelope
// ({"error":{"code","message","status"}}), not the OpenAI-shaped
// {"error":{"type","message"}} envelope every other ingress protocol falls
// back to by default.
func TestGeminiIngressAllCandidatesFailed_NativeError(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"upstream broken"}`))
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "openai-provider", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	reqBody := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:generateContent", reqBody)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", w.Code, w.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body did not unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if _, hasTopLevelType := body["type"]; hasTopLevelType {
		t.Errorf("body has a Claude-shaped top-level %q field, want the bare Gemini envelope: %v", "type", body)
	}
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf(`"error" field is not an object: %v`, body["error"])
	}
	if code, _ := errObj["code"].(float64); int(code) != http.StatusBadGateway {
		t.Errorf("error.code = %v, want %d", errObj["code"], http.StatusBadGateway)
	}
	if _, hasErrorType := errObj["type"]; hasErrorType {
		t.Errorf("error object has an OpenAI-shaped %q field, want Gemini's code/message/status: %v", "type", errObj)
	}
	if _, hasStatus := errObj["status"]; !hasStatus {
		t.Errorf("error object missing the Gemini-native %q field: %v", "status", errObj)
	}
}
