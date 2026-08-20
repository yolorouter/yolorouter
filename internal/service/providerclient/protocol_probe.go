package providerclient

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
	"github.com/yolorouter/yolorouter/internal/protocols/claude"
	"github.com/yolorouter/yolorouter/internal/protocols/gemini"
	"github.com/yolorouter/yolorouter/internal/protocols/responses"
)

// probeSpec gathers everything the credential-test client knows about one
// wire protocol: which codec drives the URL and headers, how to build the
// probe bodies, how to read a model-catalogue page, and how to read the
// upstream's answers. It exists so that knowledge lives in ONE entry per
// protocol instead of one case per operation — adding a protocol means
// adding an entry to probeSpecs, not editing ten switch statements.
//
// The per-operation functions in provider_client.go keep their names and
// signatures and delegate here, so their call sites and tests are untouched.
type probeSpec struct {
	// encoder is the exact same request encoder runtime dispatch uses, so a
	// provider that passes its credential test is guaranteed to be hit the
	// same way in production.
	encoder protocols.RequestEncoder

	// basicPayload / streamingPayload / functionCallingPayload build the
	// minimal probe bodies. A payload only needs enough shape to avoid a
	// spurious 400 — see the per-protocol notes on each entry.
	basicPayload           func(model string) map[string]interface{}
	streamingPayload       func(model string) map[string]interface{}
	functionCallingPayload func(model string) map[string]interface{}

	// parseModelPage extracts one page of model ids from a 200 catalogue
	// body plus the pagination cursor (param name + value), "" when done.
	parseModelPage func(body []byte) (ids []string, nextParam, nextValue string)

	// modelsPath is the catalogue endpoint path, resolved through the same
	// version-aware joiner as every other probe URL (gemini's catalogue
	// lives at /models under /v1beta; everyone else serves /v1/models).
	modelsPath string

	// successCertifiable states whether validSuccessBody genuinely validates
	// this protocol's success shape. When false — the zero value, so a new
	// entry stays uncertifiable until someone writes its validator — a 200 on
	// the basic credential test reports TestVerificationUnsupported instead
	// of TestSuccess: the request was not rejected outright, but a key must
	// not be authorized against a destination that was never truly verified.
	successCertifiable bool

	// validStreamBody judges a 200 streaming response, returning an
	// admin-facing detail when it fails. An entry whose SSE shape is not yet
	// modelled uses unverifiedStreamPass and says so on the entry.
	validStreamBody func(resp *http.Response, durationMs int64) (ok bool, detail string)

	// validToolCallBody judges a 200 tool-calling response body. Entries
	// whose tool-call shape is not yet modelled fall back to the parseable-
	// JSON-without-error leniency check.
	validToolCallBody func(body []byte) bool

	// validSuccessBody judges a JSON 200 body (the shared content-type check
	// has already run). modelScopedError / quotaError / modelNotFoundError
	// read structure out of error bodies; extractMessage pulls the
	// human-readable error text, "" when the shape is not recognised.
	validSuccessBody   func(body []byte) bool
	modelScopedError   func(body []byte, model string) bool
	quotaError         func(body []byte) bool
	modelNotFoundError func(body []byte) bool
	extractMessage     func(body []byte) string
}

// probeSpecs is the per-protocol table. The protocol vocabulary itself is
// owned by internal/providerproto; a registry-completeness test holds this
// table to exactly that set, so adding a protocol there stays red until its
// probe entry exists here.
var probeSpecs = map[protocols.ProtocolID]probeSpec{
	protocols.ProtocolOpenAI:    openAIProbe,
	protocols.ProtocolClaude:    claudeProbe,
	protocols.ProtocolGemini:    geminiProbe,
	protocols.ProtocolResponses: responsesProbe,
}

// probeSpecFor resolves proto's spec, falling back to OpenAI for unknown
// protocols — mirroring providerproto.TypeOf's own default.
func probeSpecFor(proto protocols.ProtocolID) probeSpec {
	if spec, ok := probeSpecs[proto]; ok {
		return spec
	}
	return openAIProbe
}

var openAIProbe = probeSpec{
	encoder: chat.RequestEncoder{},
	basicPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
		}
	},
	streamingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model":      model,
			"stream":     true,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
	},
	functionCallingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model": model,
			"messages": []map[string]string{
				{"role": "user", "content": "What's the weather in Beijing?"},
			},
			"tools": []map[string]interface{}{
				{
					"type": "function",
					"function": map[string]interface{}{
						"name":        weatherToolName,
						"description": weatherToolDescription,
						"parameters":  weatherToolParameters(),
					},
				},
			},
		}
	},
	parseModelPage:     parseDataModelPage,
	modelsPath:         "/v1/models",
	successCertifiable: true,
	validStreamBody:    openAIStreamBody,
	validToolCallBody:  isValidToolCallsBody,
	validSuccessBody:   isValidOpenAIChatSuccessBody,
	modelScopedError:   openAIModelScopedError,
	quotaError:         openAIQuotaError,
	modelNotFoundError: openAIModelNotFoundError,
	extractMessage:     openAIErrorMessage,
}

// anthropic requires max_tokens on every request.
var claudeProbe = probeSpec{
	encoder: claude.RequestEncoder{},
	basicPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
	},
	streamingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model":      model,
			"max_tokens": 1,
			"stream":     true,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		}
	},
	functionCallingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model":      model,
			"max_tokens": 64,
			"messages": []map[string]string{
				{"role": "user", "content": "What's the weather in Beijing?"},
			},
			"tools": []map[string]interface{}{
				{
					"name":         weatherToolName,
					"description":  weatherToolDescription,
					"input_schema": weatherToolParameters(),
				},
			},
		}
	},
	parseModelPage: parseDataModelPage,
	modelsPath:     "/v1/models",
	// claude's non-streaming success body is genuinely validated; its SSE
	// delta shape (content_block_delta / message_stop) and tool_use blocks
	// are not yet modelled, so streaming and tool-calling use the deferred
	// checks.
	successCertifiable: true,
	validStreamBody:    unverifiedStreamPass,
	validToolCallBody:  isParseableJSONObjectWithoutError,
	validSuccessBody:   isValidClaudeSuccessBody,
	modelScopedError:   claudeModelScopedError,
	quotaError:         claudeQuotaError,
	modelNotFoundError: claudeModelNotFoundError,
	extractMessage:     claudeErrorMessage,
}

// gemini/responses bodies are structurally minimal — enough request shape to
// avoid a spurious 400. Their body validators are deferred: a parseable JSON
// object carrying no top-level "error" is accepted as success, and their
// error bodies are not parsed (a 429 classifies as rate-limited via the
// status fallback rather than quota-unavailable; model-scoped detection
// always answers false, which only affects verification_status write rules,
// never whether the test passed).
var geminiProbe = probeSpec{
	encoder: gemini.RequestEncoder{},
	basicPayload: func(model string) map[string]interface{} {
		return geminiPingBody()
	},
	// gemini's generateContent request body is the same either way for
	// streaming; only the endpoint differs.
	streamingPayload: func(model string) map[string]interface{} {
		return geminiPingBody()
	},
	functionCallingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"contents": []map[string]interface{}{
				{"role": "user", "parts": []map[string]string{{"text": "What's the weather in Beijing?"}}},
			},
			"tools": []map[string]interface{}{
				{
					"functionDeclarations": []map[string]interface{}{
						{
							"name":        weatherToolName,
							"description": weatherToolDescription,
							"parameters":  weatherToolParameters(),
						},
					},
				},
			},
		}
	},
	parseModelPage: parseGeminiModelPage,
	modelsPath:     "/models", // resolves under /v1beta via the version-aware joiner
	// successCertifiable stays false: with only the leniency check for a
	// success body, a 200 must report "cannot certify yet" rather than
	// authorize a key against a destination that was never truly verified.
	successCertifiable: false,
	validStreamBody:    unverifiedStreamPass,
	validToolCallBody:  isParseableJSONObjectWithoutError,
	validSuccessBody:   isParseableJSONObjectWithoutError,
	modelScopedError:   neverModelScoped,
	quotaError:         neverQuotaError,
	modelNotFoundError: neverModelNotFound,
	extractMessage:     openAIErrorMessage, // gemini error shapes are unmodelled; the OpenAI parse is the best-effort read
}

// See geminiProbe's note: the deferred validators apply here too.
var responsesProbe = probeSpec{
	encoder: responses.RequestEncoder{},
	basicPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model": model,
			"input": "ping",
		}
	},
	streamingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model":  model,
			"input":  "ping",
			"stream": true,
		}
	},
	functionCallingPayload: func(model string) map[string]interface{} {
		return map[string]interface{}{
			"model": model,
			"input": "What's the weather in Beijing?",
			"tools": []map[string]interface{}{
				{
					"type":        "function",
					"name":        weatherToolName,
					"description": weatherToolDescription,
					"parameters":  weatherToolParameters(),
				},
			},
		}
	},
	parseModelPage:     parseDataModelPage,
	modelsPath:         "/v1/models",
	successCertifiable: false,
	validStreamBody:    unverifiedStreamPass,
	validToolCallBody:  isParseableJSONObjectWithoutError,
	validSuccessBody:   isParseableJSONObjectWithoutError,
	modelScopedError:   neverModelScoped,
	quotaError:         neverQuotaError,
	modelNotFoundError: neverModelNotFound,
	extractMessage:     openAIErrorMessage,
}

func geminiPingBody() map[string]interface{} {
	return map[string]interface{}{
		"contents": []map[string]interface{}{
			{"role": "user", "parts": []map[string]string{{"text": "ping"}}},
		},
	}
}

// parseDataModelPage reads the {"data":[{"id":...}],"has_more","last_id"}
// catalogue shape openai/anthropic/responses share (only Anthropic actually
// paginates; OpenAI omits has_more).
func parseDataModelPage(body []byte) (ids []string, nextParam, nextValue string) {
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", ""
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	if parsed.HasMore && parsed.LastID != "" {
		return out, "after_id", parsed.LastID
	}
	return out, "", ""
}

// parseGeminiModelPage reads {"models":[{"name":"models/<id>"}],
// "nextPageToken"}, stripping the "models/" prefix.
func parseGeminiModelPage(body []byte) (ids []string, nextParam, nextValue string) {
	var parsed struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, "", ""
	}
	out := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if id := strings.TrimPrefix(m.Name, "models/"); id != "" {
			out = append(out, id)
		}
	}
	if parsed.NextPageToken != "" {
		return out, "pageToken", parsed.NextPageToken
	}
	return out, "", ""
}

// openAIModelScopedError: a structured error.param naming the model, or the
// model's own name inside the message text.
func openAIModelScopedError(body []byte, model string) bool {
	parsed := parseErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	if parsed.Error.Param == "model" {
		return true
	}
	return model != "" && strings.Contains(strings.ToLower(parsed.Error.Message), strings.ToLower(model))
}

// claudeModelScopedError: Anthropic carries no structured param field, so
// only the message-text heuristic applies.
func claudeModelScopedError(body []byte, model string) bool {
	parsed := parseClaudeErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	return model != "" && strings.Contains(strings.ToLower(parsed.Error.Message), strings.ToLower(model))
}

func openAIQuotaError(body []byte) bool {
	parsed := parseErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	if parsed.Error.Code == "insufficient_quota" {
		return true
	}
	lower := strings.ToLower(parsed.Error.Message)
	return strings.Contains(lower, "quota") || strings.Contains(lower, "billing")
}

func claudeQuotaError(body []byte) bool {
	parsed := parseClaudeErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	lower := strings.ToLower(parsed.Error.Message)
	return strings.Contains(lower, "quota") || strings.Contains(lower, "billing") || strings.Contains(lower, "credit")
}

func openAIModelNotFoundError(body []byte) bool {
	parsed := parseErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	if parsed.Error.Code == "model_not_found" {
		return true
	}
	lower := strings.ToLower(parsed.Error.Message)
	return strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist"))
}

func claudeModelNotFoundError(body []byte) bool {
	parsed := parseClaudeErrorBody(body)
	if parsed == nil || parsed.Error == nil {
		return false
	}
	lower := strings.ToLower(parsed.Error.Message)
	return strings.Contains(lower, "model") && (strings.Contains(lower, "not found") || strings.Contains(lower, "does not exist"))
}

func openAIErrorMessage(body []byte) string {
	if e := parseErrorBody(body); e != nil && e.Error != nil {
		return strings.TrimSpace(e.Error.Message)
	}
	return ""
}

func claudeErrorMessage(body []byte) string {
	if e := parseClaudeErrorBody(body); e != nil && e.Error != nil {
		return strings.TrimSpace(e.Error.Message)
	}
	return ""
}

func neverModelScoped(_ []byte, _ string) bool { return false }
func neverQuotaError(_ []byte) bool            { return false }
func neverModelNotFound(_ []byte) bool         { return false }
