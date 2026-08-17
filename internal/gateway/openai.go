package gateway

import (
	"encoding/json"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// parsedRequest is the gateway's one-pass view of an OpenAI chat-completions
// request: just the fields it needs to route and validate. Everything else
// in the raw body is forwarded to the upstream untouched by
// rewriteModelField.
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
// used by the candidate capability filter (a request with tools must skip
// candidates whose supports_function_calling is false).
func (p *parsedRequest) hasTools() bool { return len(p.Tools) > 0 }

// validate checks the structural invariants the gateway itself cares about:
// messages must be a non-empty array, and any tool definition must be
// type=function (only function tools are supported in v0.1). Unknown/extended
// fields are NOT validated here — they
// pass through to the upstream.
func (p *parsedRequest) validate() error {
	if len(p.Messages) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	for i, t := range p.Tools {
		if t.Type != "function" {
			return fmt.Errorf("tools[%d]: only type=function is supported", i)
		}
	}
	return nil
}

// parseRequest is the single JSON-decode pass the gateway does on an OpenAI
// ingress caller body (one unmarshal into the raw shape, then one each for
// the messages and tools sub-arrays). peekIngress wraps this for the OpenAI
// ingress and threads the resulting *parsedRequest through ingressMeta so
// Handle validates + filters candidates without re-parsing the body at each
// step.
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
	// The gateway always asks the upstream for final usage (for
	// cost accounting) even when the caller didn't — but only FORWARDS that
	// usage when the caller set stream_options.include_usage=true. Capture
	// the caller's intent here so EnsureStreamUsageInjection knows whether
	// to inject and the stream pump knows whether to strip.
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
// the production hot path (Handle) calls peekIngress (which wraps
// parseRequest for the OpenAI ingress) once and reads the fields directly
// off the returned ingressMeta.
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

// rewriteJSONStringField sets a top-level string field to newValue in a JSON
// object body. When requirePresent is true, an absent field leaves the body
// unchanged (never adds it); when false, the field is set even if absent. A
// body that is literal "null" is returned unchanged.
func rewriteJSONStringField(body []byte, field, newValue string, requirePresent bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse json object: %w", err)
	}
	if m == nil {
		// body was literal "null" — json.Unmarshal returns nil error but
		// leaves m nil, and writing m[field] would panic on a nil map.
		// Forward unchanged rather than crash the request.
		return body, nil
	}
	if requirePresent {
		if _, present := m[field]; !present {
			return body, nil
		}
	}
	valueJSON, err := json.Marshal(newValue)
	if err != nil {
		return nil, err
	}
	m[field] = valueJSON
	return json.Marshal(m)
}

// rewriteModelField parses body as a JSON object, replaces just the "model"
// field, and re-serializes — every other field is preserved verbatim via
// json.RawMessage, so unknown/extended OpenAI params pass through untouched.
// Used for the request (external name -> provider_model_name) and the
// non-stream response (provider name -> external name).
func rewriteModelField(body []byte, newModel string) ([]byte, error) {
	return rewriteJSONStringField(body, "model", newModel, false)
}

// RewriteRequestModel swaps the caller's external model name for the
// candidate's provider_model_name in the upstream-bound body.
func RewriteRequestModel(body []byte, providerModelName string) ([]byte, error) {
	return rewriteModelField(body, providerModelName)
}

// EnsureStreamUsageInjection forces stream_options.include_usage=true on a
// stream request bound for the upstream when the caller did NOT already
// request usage (the system always requests final usage from the upstream for
// its own cost accounting; the stream pump strips the injected usage back out
// of the forwarded frames when the caller didn't ask).
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
// record cost_known=true cost_micros=0 — showing the request as free, which
// is wrong (a missing usage must NOT be recorded as 0
// cost). Only when BOTH prompt and completion counts are present is the
// usage considered known.
type wireUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
	// prompt_tokens_details.cached_tokens is OpenAI's cache-READ count (the
	// portion of prompt_tokens already served from cache). Anthropic splits
	// this into cache_creation_input_tokens (WRITE) + cache_read_input_tokens,
	// surfaced via the alias fields below.
	PromptTokensDetails *promptTokensDetails `json:"prompt_tokens_details,omitempty"`
	// Anthropic-style aliases (absent on OpenAI upstreams).
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens,omitempty"`
	// DeepSeek reports its cache-READ count as prompt_cache_hit_tokens rather
	// than nesting it under prompt_tokens_details. Its companion
	// prompt_cache_miss_tokens is the non-cached remainder, which
	// netPromptTokens already derives, so only the hit half is decoded.
	PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens *int `json:"cached_tokens"`
	// OpenRouter documents a cache-WRITE count nested here, beside
	// cached_tokens. It is the standard spelling and takes precedence over the
	// top-level CacheCreationInputTokens alias.
	CacheWriteTokens *int `json:"cache_write_tokens"`
}

func (w *wireUsage) toIRUsage() *protocols.IRUsage {
	if w == nil || w.PromptTokens == nil || w.CompletionTokens == nil {
		return nil
	}
	u := &protocols.IRUsage{
		PromptTokens:     *w.PromptTokens,
		CompletionTokens: *w.CompletionTokens,
		// This wire shape requires top-level prompt_tokens/completion_tokens —
		// the OpenAI-compatible convention where prompt_tokens already counts
		// cache-read tokens. Mark it so netPromptTokens (log.go) subtracts the
		// cached portion to derive the net input for billing and the log row.
		CacheIncludedInPrompt: true,
	}
	if w.TotalTokens != nil {
		u.TotalTokens = *w.TotalTokens
	} else {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	// Cache READ: OpenAI reports it under prompt_tokens_details.cached_tokens;
	// Anthropic reports it directly as cache_read_input_tokens; DeepSeek as
	// prompt_cache_hit_tokens.
	switch {
	case w.PromptTokensDetails != nil && w.PromptTokensDetails.CachedTokens != nil:
		u.CacheReadTokens = *w.PromptTokensDetails.CachedTokens
	case w.CacheReadInputTokens != nil:
		u.CacheReadTokens = *w.CacheReadInputTokens
	case w.PromptCacheHitTokens != nil:
		u.CacheReadTokens = *w.PromptCacheHitTokens
	}
	// Cache WRITE has two spellings and they name the same breakdown of
	// prompt_tokens, so exactly one is taken — never summed. OpenRouter's
	// nested cache_write_tokens is the documented contract and wins; the
	// top-level Anthropic-style alias is the fallback.
	if w.CacheCreationInputTokens != nil {
		u.CacheWriteTokens = *w.CacheCreationInputTokens
	}
	if w.PromptTokensDetails != nil && w.PromptTokensDetails.CacheWriteTokens != nil {
		u.CacheWriteTokens = *w.PromptTokensDetails.CacheWriteTokens
	}
	return u
}

// extractUsage pulls prompt/completion/total tokens out of an
// OpenAI-compatible response body. Returns nil if no usage object is present
// OR if the object lacks both required token counts — the caller treats nil
// as "unknown", never as zero (a missing usage must not be recorded as 0
// cost).
func extractUsage(body []byte) *protocols.IRUsage {
	var resp struct {
		Usage *wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return resp.Usage.toIRUsage()
}

// RewriteNonStreamResponse swaps the upstream response's model field back to
// the external name and extracts usage. The body is returned rewritten so
// the handler can write it to the client in one shot; usage is separate so
// the relay loop can compute cost without re-parsing.
func RewriteNonStreamResponse(body []byte, externalModel string) ([]byte, *protocols.IRUsage, error) {
	rewritten, err := rewriteModelField(body, externalModel)
	if err != nil {
		return nil, nil, err
	}
	return rewritten, extractUsage(body), nil
}
