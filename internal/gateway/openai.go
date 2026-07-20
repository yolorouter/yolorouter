package gateway

import (
	"encoding/json"
	"fmt"
)

// parsedRequest is the gateway's one-pass view of an OpenAI chat-completions
// request: just the fields it needs to route and validate (PRD §6.5.3 steps
// 4-7). Everything else in the raw body is forwarded to the upstream
// untouched by rewriteModelField.
type parsedRequest struct {
	Model            string
	Stream           bool
	WantsStreamUsage bool              // caller set stream_options.include_usage=true
	Messages         []json.RawMessage // parsed from the "messages" array
	Tools            []parsedTool      // parsed from the "tools" array
}

type parsedTool struct {
	Type string `json:"type"`
}

// hasTools reports whether the request carries a non-empty tools array —
// used by the candidate capability filter (PRD §6.5.3 step 7: a request with
// tools must skip candidates whose supports_function_calling is false).
func (p *parsedRequest) hasTools() bool { return len(p.Tools) > 0 }

// validate checks the structural invariants the gateway itself cares about
// (PRD §6.5.3 step 6): messages must be a non-empty array, and any tool
// definition must be type=function (PRD §6.5.10 — only function tools are
// supported in v0.1). Unknown/extended fields are NOT validated here — they
// pass through to the upstream.
func (p *parsedRequest) validate() error {
	if len(p.Messages) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	for i, t := range p.Tools {
		if t.Type != "function" {
			return fmt.Errorf("tools[%d]: only type=function is supported (PRD §6.5.10)", i)
		}
	}
	return nil
}

// parseRequest is the single JSON-decode pass the gateway does on a caller
// body (one unmarshal into the raw shape, then one each for the messages and
// tools sub-arrays). Handle keeps the returned *parsedRequest and threads it
// through validate + the capability filter, instead of re-parsing the body
// at each step.
func parseRequest(body []byte) (*parsedRequest, error) {
	var raw struct {
		Model         string          `json:"model"`
		Stream        bool            `json:"stream"`
		Messages      json.RawMessage `json:"messages"`
		Tools         json.RawMessage `json:"tools"`
		StreamOptions json.RawMessage `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}
	p := &parsedRequest{Model: raw.Model, Stream: raw.Stream}
	if len(raw.Messages) > 0 && string(raw.Messages) != "null" {
		if err := json.Unmarshal(raw.Messages, &p.Messages); err != nil {
			return nil, fmt.Errorf("messages must be an array: %w", err)
		}
	}
	if len(raw.Tools) > 0 && string(raw.Tools) != "null" {
		if err := json.Unmarshal(raw.Tools, &p.Tools); err != nil {
			return nil, fmt.Errorf("tools must be an array: %w", err)
		}
	}
	// PRD §1114: the gateway always asks the upstream for final usage (for
	// cost accounting) even when the caller didn't — but only FORWARDS that
	// usage when the caller set stream_options.include_usage=true. Capture
	// the caller's intent here so EnsureStreamUsageInjection knows whether
	// to inject and StreamUpstreamToClient knows whether to strip.
	if len(raw.StreamOptions) > 0 && string(raw.StreamOptions) != "null" {
		var so struct {
			IncludeUsage *bool `json:"include_usage"`
		}
		if err := json.Unmarshal(raw.StreamOptions, &so); err != nil {
			return nil, fmt.Errorf("stream_options must be an object: %w", err)
		}
		if so.IncludeUsage != nil && *so.IncludeUsage {
			p.WantsStreamUsage = true
		}
	}
	return p, nil
}

// PeekRequest extracts model + stream from a caller body. Kept as a public
// convenience for callers that only need those two fields (and for tests);
// the production hot path (Handle) calls parseRequest once and reads the
// fields directly off the parsed struct.
func PeekRequest(body []byte) (model string, isStream bool, err error) {
	p, err := parseRequest(body)
	if err != nil {
		return "", false, err
	}
	return p.Model, p.Stream, nil
}

// ValidateRequest checks the request's structural invariants. Public
// convenience wrapper over parseRequest + validate; Handle uses the parsed
// struct directly to avoid a second decode.
func ValidateRequest(body []byte) error {
	p, err := parseRequest(body)
	if err != nil {
		return err
	}
	return p.validate()
}

// HasTools reports whether the request carries a non-empty tools array.
// Public convenience wrapper; Handle reads parsedRequest.hasTools() instead.
func HasTools(body []byte) bool {
	p, err := parseRequest(body)
	if err != nil {
		return false
	}
	return p.hasTools()
}

// rewriteModelField parses body as a JSON object, replaces just the "model"
// field, and re-serializes — every other field is preserved verbatim via
// json.RawMessage, so unknown/extended OpenAI params pass through untouched.
// Used for the request (external name -> provider_model_name) and the
// non-stream response (provider name -> external name).
func rewriteModelField(body []byte, newModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse openai object: %w", err)
	}
	if m == nil {
		// body was literal "null" — json.Unmarshal returns nil error but
		// leaves m nil, and writing m["model"] would panic on a nil map.
		// Forward unchanged rather than crash the request.
		return body, nil
	}
	modelJSON, err := json.Marshal(newModel)
	if err != nil {
		return nil, err
	}
	m["model"] = modelJSON
	return json.Marshal(m)
}

// RewriteRequestModel swaps the caller's external model name for the
// candidate's provider_model_name in the upstream-bound body.
func RewriteRequestModel(body []byte, providerModelName string) ([]byte, error) {
	return rewriteModelField(body, providerModelName)
}

// EnsureStreamUsageInjection forces stream_options.include_usage=true on a
// stream request bound for the upstream when the caller did NOT already
// request usage (PRD §1114: the system always requests final usage from the
// upstream for its own cost accounting; the injected usage is stripped from
// forwarded frames in StreamUpstreamToClient when the caller didn't ask).
// Returns body unchanged for non-stream requests or when the caller already
// requested usage.
func EnsureStreamUsageInjection(body []byte, isStream, callerWantsUsage bool) ([]byte, error) {
	if !isStream || callerWantsUsage {
		return body, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse openai object for usage injection: %w", err)
	}
	if m == nil {
		return body, nil
	}
	// Preserve any existing stream_options members, then set include_usage.
	var existing map[string]json.RawMessage
	if raw, ok := m["stream_options"]; ok && len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &existing)
	}
	if existing == nil {
		existing = map[string]json.RawMessage{}
	}
	existing["include_usage"] = []byte("true")
	soJSON, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	m["stream_options"] = soJSON
	return json.Marshal(m)
}

// wireUsage decodes the usage object with pointer fields so a missing
// prompt_tokens / completion_tokens member is distinguishable from a
// legitimate zero. OpenAI-compatible upstreams occasionally return {} or a
// partial object; treating those as "known zero" would let computeCost
// record cost_known=true cost_cents=0 — showing the request as free, which
// violates GATE-21 / PRD §6.7.6 (a missing usage must NOT be recorded as 0
// cost). Only when BOTH prompt and completion counts are present is the
// usage considered known.
type wireUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
}

func (w *wireUsage) toUsage() *Usage {
	if w == nil || w.PromptTokens == nil || w.CompletionTokens == nil {
		return nil
	}
	u := &Usage{
		PromptTokens:     *w.PromptTokens,
		CompletionTokens: *w.CompletionTokens,
	}
	if w.TotalTokens != nil {
		u.TotalTokens = *w.TotalTokens
	} else {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	return u
}

// extractUsage pulls prompt/completion/total tokens out of an
// OpenAI-compatible response body. Returns nil if no usage object is present
// OR if the object lacks both required token counts — the caller treats nil
// as "unknown", never as zero (GATE-21 / PRD §6.7.6: a missing usage must
// not be recorded as 0 cost).
func extractUsage(body []byte) *Usage {
	var resp struct {
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return resp.Usage.toUsage()
}

// RewriteNonStreamResponse swaps the upstream response's model field back to
// the external name and extracts usage. The body is returned rewritten so
// the handler can write it to the client in one shot; usage is separate so
// the relay loop can compute cost without re-parsing.
func RewriteNonStreamResponse(body []byte, externalModel string) ([]byte, *Usage, error) {
	rewritten, err := rewriteModelField(body, externalModel)
	if err != nil {
		return nil, nil, err
	}
	return rewritten, extractUsage(body), nil
}
