package gateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// ingressMeta is the gateway's one-pass view of a caller request, extracted
// by peekIngress before any candidate is chosen: just enough to route
// (Model, Stream) and to filter/validate (WantsStreamUsage, HasTools), and
// to answer the capability strict questions (HasReasoning). validate
// closes over whatever protocol-specific bookkeeping each peek*
// function's local parse produced, so the structural invariant check below
// runs without a second body parse and without a protocol switch here.
type ingressMeta struct {
	Model            string
	Stream           bool
	WantsStreamUsage bool
	HasTools         bool
	// HasReasoning reports whether the body carries the protocol's
	// reasoning parameter, judged by field presence rather than value
	// validity: a same-protocol landing forwards the field whatever shape
	// it has, so both the strict entry refusal and the egress clear police
	// presence, not well-formedness.
	HasReasoning bool

	// validate checks the structural invariants the gateway cares about
	// before picking a candidate: messages must be non-empty for every
	// protocol, plus whatever else that protocol's decoder is lenient about
	// at the top level (for Claude, max_tokens must be present). This is a
	// lightweight pre-check; the full body structure (message content shape,
	// tool schema, ...) is checked once by validateIngressBody after a
	// candidate model is resolved. Each peek* function assigns its own
	// closure over its local parsed values; OpenAI reuses
	// parsedRequest.validate() verbatim instead of duplicating its rules.
	validate func() error
}

// countJSONArray parses an optional JSON array field, returning its element
// count (0 for absent or null) or an error if present but not an array.
func countJSONArray(raw json.RawMessage, fieldName string) (int, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0, fmt.Errorf("%s must be an array: %w", fieldName, err)
	}
	return len(arr), nil
}

// peekIngress extracts routing/filtering metadata from a caller body without
// doing the full IR decode. OpenAI reuses parseRequest (the same parse the
// OpenAI hot path already does); Claude and Responses do their own
// lightweight top-level parse of their own request shape (model/stream/
// messages-or-input/tools/...) since their wire formats differ from
// OpenAI's. Gemini carries neither model nor stream in the body at all --
// both come from the URL path (see parseGeminiPath) -- so pathModel/
// pathStream supply them; every other protocol ignores these two
// parameters and reads model/stream from the body instead.
func peekIngress(ingress protocols.ProtocolID, body []byte, pathModel string, pathStream bool) (*ingressMeta, error) {
	switch ingress {
	case protocols.ProtocolClaude:
		return peekClaudeIngress(body)
	case protocols.ProtocolGemini:
		return peekGeminiIngress(body, pathModel, pathStream)
	case protocols.ProtocolResponses:
		return peekResponsesIngress(body)
	default:
		return peekOpenAIIngress(body)
	}
}

func peekOpenAIIngress(body []byte) (*ingressMeta, error) {
	p, err := parseRequest(body)
	if err != nil {
		return nil, err
	}
	return &ingressMeta{
		Model:            p.Model,
		Stream:           p.Stream,
		WantsStreamUsage: p.WantsStreamUsage,
		HasTools:         p.hasTools(),
		HasReasoning:     p.HasReasoning,
		validate:         p.validate,
	}, nil
}

// rawReasoningPresent answers the OpenAI family's two spellings of a
// reasoning parameter: the reasoning_effort string (an explicitly empty
// string reads as removed, same as an absent field) and the reasoning
// object. Raw values, not typed ones, so a value with an unexpected JSON
// type still counts as present — it is forwarded the same way.
func rawReasoningPresent(effort, object json.RawMessage) bool {
	if len(effort) > 0 && string(effort) != "null" && string(effort) != `""` {
		return true
	}
	return len(object) > 0 && string(object) != "null"
}

// claudeIngressWire is the top-level shape peekIngress reads from a Claude
// Messages API request. MaxTokens is a pointer purely to distinguish
// "absent" from "present with value 0" for the max_tokens-required check;
// the actual numeric value (and everything else) is re-decoded in full by
// validateIngressBody via claude.RequestDecoder.
type claudeIngressWire struct {
	Model     string          `json:"model"`
	Stream    bool            `json:"stream"`
	Messages  json.RawMessage `json:"messages"`
	Tools     json.RawMessage `json:"tools"`
	Thinking  json.RawMessage `json:"thinking"`
	MaxTokens *int            `json:"max_tokens"`
}

func peekClaudeIngress(body []byte) (*ingressMeta, error) {
	var raw claudeIngressWire
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse claude request: %w", err)
	}

	msgCount, err := countJSONArray(raw.Messages, "messages")
	if err != nil {
		return nil, err
	}

	toolCount, err := countJSONArray(raw.Tools, "tools")
	if err != nil {
		return nil, err
	}
	hasTools := toolCount > 0

	maxTokens := raw.MaxTokens
	hasReasoning := len(raw.Thinking) > 0 && string(raw.Thinking) != "null"
	return &ingressMeta{
		Model:  raw.Model,
		Stream: raw.Stream,
		// Claude streaming always carries usage in message_delta/message_stop;
		// unlike OpenAI there is no caller opt-in equivalent to
		// stream_options.include_usage, so this is unconditionally true.
		WantsStreamUsage: true,
		HasTools:         hasTools,
		HasReasoning:     hasReasoning,
		validate: func() error {
			if msgCount == 0 {
				return fmt.Errorf("messages must be a non-empty array")
			}
			if maxTokens == nil {
				return fmt.Errorf("max_tokens is required")
			}
			if *maxTokens <= 0 {
				return fmt.Errorf("max_tokens must be a positive integer")
			}
			return nil
		},
	}, nil
}

// geminiIngressWire is the top-level shape peekGeminiIngress reads from a
// native Gemini generateContent/streamGenerateContent request body. Unlike
// every other ingress protocol, Gemini carries neither model nor stream in
// the body -- both are encoded in the URL path (see parseGeminiPath) -- so
// this only needs the fields the path can't supply.
type geminiIngressWire struct {
	Contents         json.RawMessage `json:"contents"`
	Tools            json.RawMessage `json:"tools"`
	GenerationConfig json.RawMessage `json:"generationConfig"`
}

// peekGeminiIngress parses the top-level shape of a Gemini request body.
// model and isStream are NOT read from the body (Gemini has no such fields
// on the wire) -- they come from pathModel/pathStream, extracted from the
// URL by parseGeminiPath in Handle before this is called.
func peekGeminiIngress(body []byte, pathModel string, pathStream bool) (*ingressMeta, error) {
	var raw geminiIngressWire
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse gemini request: %w", err)
	}

	contentCount, err := countJSONArray(raw.Contents, "contents")
	if err != nil {
		return nil, err
	}

	toolCount, err := countJSONArray(raw.Tools, "tools")
	if err != nil {
		return nil, err
	}
	hasTools := toolCount > 0

	// Gemini's reasoning parameter nests one level down, inside
	// generationConfig.thinkingConfig; both levels must be present and
	// non-null. A malformed generationConfig reads as absent — the full
	// structural decode below still gets to refuse the shape.
	hasReasoning := false
	if len(raw.GenerationConfig) > 0 && string(raw.GenerationConfig) != "null" {
		var gc struct {
			ThinkingConfig json.RawMessage `json:"thinkingConfig"`
		}
		if json.Unmarshal(raw.GenerationConfig, &gc) == nil {
			hasReasoning = len(gc.ThinkingConfig) > 0 && string(gc.ThinkingConfig) != "null"
		}
	}

	return &ingressMeta{
		Model:  pathModel,
		Stream: pathStream,
		// Gemini's usageMetadata is unconditionally attached to the final SSE
		// chunk of a streamGenerateContent response (and to every
		// non-streaming response); there is no caller opt-in field on the
		// wire (unlike OpenAI's stream_options.include_usage) that gates it,
		// so this is unconditionally true, mirroring gemini.RequestDecoder's
		// own IRStreamConfig.IncludeUsage:true.
		WantsStreamUsage: true,
		HasTools:         hasTools,
		HasReasoning:     hasReasoning,
		validate: func() error {
			if contentCount == 0 {
				return fmt.Errorf("contents must be a non-empty array")
			}
			return nil
		},
	}, nil
}

// responsesIngressWire is the top-level shape peekResponsesIngress reads
// from an OpenAI Responses API request body. Unlike Gemini, Responses
// carries model and stream in the body itself, same as OpenAI Chat and
// Claude.
type responsesIngressWire struct {
	Model     string          `json:"model"`
	Stream    bool            `json:"stream"`
	Input     json.RawMessage `json:"input"`
	Tools     json.RawMessage `json:"tools"`
	Reasoning json.RawMessage `json:"reasoning"`
}

// peekResponsesIngress parses the top-level shape of a Responses API
// request body.
func peekResponsesIngress(body []byte) (*ingressMeta, error) {
	var raw responsesIngressWire
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}

	toolCount, err := countJSONArray(raw.Tools, "tools")
	if err != nil {
		return nil, err
	}
	hasTools := toolCount > 0

	hasInput := len(raw.Input) > 0 && string(raw.Input) != "null"
	hasReasoning := len(raw.Reasoning) > 0 && string(raw.Reasoning) != "null"
	return &ingressMeta{
		Model:  raw.Model,
		Stream: raw.Stream,
		// responses.StreamEncoder (internal/protocols/responses/encoder.go)
		// unconditionally attaches usage to the response.completed event via
		// its own accumulated e.usage -- there is no caller opt-in request
		// field (no Responses equivalent of OpenAI Chat's
		// stream_options.include_usage) gating it, so this is unconditionally
		// true, same as Claude and Gemini.
		WantsStreamUsage: true,
		HasTools:         hasTools,
		HasReasoning:     hasReasoning,
		validate: func() error {
			if !hasInput {
				return fmt.Errorf("input is required")
			}
			return nil
		},
	}, nil
}

// validateIngressBody runs the full protocol-specific structural decode on a
// caller body -- the same decode the request will eventually go through on
// the hot path -- and returns its error, if any. Call this once (after model
// lookup, before the candidate loop) with a placeholder externalModel; a
// successful decode means the body is structurally valid for its ingress
// protocol (message content shapes, tool schemas, max_tokens>0, etc.), so a
// malformed body is rejected here as a client error instead of surfacing
// later as an upstream failure once a candidate has already been picked.
func validateIngressBody(ingress protocols.ProtocolID, body []byte, externalModel string, isStream bool) error {
	_, err := codecsFor(ingress).RequestDecoder.DecodeRequest(json.RawMessage(body), externalModel, isStream)
	return err
}

// parseGeminiPath extracts the model name and streaming flag from a native
// Gemini ingress path, e.g.
// "/v1beta/models/gemini-2.0-flash:generateContent". The model is split
// from the action on the LAST colon (a model name itself never contains a
// colon, but splitting on the last one is the robust choice regardless) and
// URL-decoded, since a model name may itself be percent-encoded. ok is false
// for anything that isn't exactly /v1beta/models/{model}:{action} with a
// recognized action and a non-empty model: a path missing the prefix, a
// segment with no colon, an unknown action, or an empty (or
// empty-after-decoding) model.
//
// Limitation: a model name containing a "/" (e.g. a tuned model's
// "tunedModels/xyz" resource name, which the real Gemini API would expect
// percent-encoded as "tunedModels%2Fxyz" in the path segment) is NOT
// supported end to end, even though this function itself would happily
// url.PathUnescape one if handed it directly. In practice it never is: this
// is only ever called with c.Request.URL.Path (relay.go), and net/http
// percent-decodes "%2F" into a literal "/" in URL.Path before gin ever
// routes the request (gin's default matches against the decoded Path, not
// RawPath). The router's ":modelaction" param matches exactly one
// "/"-delimited segment (internal/router/router.go), so a request for a
// slash-containing model 404s at the routing layer and never reaches this
// function at all. Standard (non-tuned) Gemini model names never contain a
// slash, so this is not a practical gap for this version; see
// TestGeminiRouteWithSlashInModelSegmentDoesNotRoute
// (internal/router/router_test.go) for the routing-layer proof.
func parseGeminiPath(path string) (model string, stream bool, ok bool) {
	rest, matched := strings.CutPrefix(path, geminiIngressPathPrefix)
	if !matched {
		return "", false, false
	}

	idx := strings.LastIndex(rest, ":")
	if idx < 0 {
		return "", false, false
	}
	encodedModel, action := rest[:idx], rest[idx+1:]

	switch action {
	case "generateContent":
		stream = false
	case "streamGenerateContent":
		stream = true
	default:
		return "", false, false
	}

	if encodedModel == "" {
		return "", false, false
	}
	decodedModel, err := url.PathUnescape(encodedModel)
	if err != nil || decodedModel == "" {
		return "", false, false
	}

	return decodedModel, stream, true
}
