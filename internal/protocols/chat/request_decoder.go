package chat

import (
	"encoding/json"
	"fmt"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"math"
	"strconv"
	"strings"
)

// RequestDecoder decodes OpenAI Chat Completions requests into IR.
type RequestDecoder struct{}

func (RequestDecoder) Protocol() protocols.ProtocolID { return protocols.ProtocolOpenAI }

func (d RequestDecoder) DecodeRequest(body json.RawMessage, model string, isStream bool) (*protocols.IRRequest, error) {
	var req struct {
		Model       string          `json:"model"`
		Messages    json.RawMessage `json:"messages"`
		Stream      bool            `json:"stream"`
		Temperature *float64        `json:"temperature,omitempty"`
		// MaxTokens and MaxCompletionTokens are the two live spellings of the
		// output ceiling; the reasoning models require the second one and
		// reject the first, and current SDKs send it. Both are read as
		// raw rather than as an int because a ceiling is a number, not an
		// integer literal: a client whose arithmetic is floating point sends
		// 9000.0 or 9e3, and an int field does not merely drop those — it fails
		// to unmarshal, which fails the whole request. Reading only max_tokens
		// would drop the ceiling of exactly the newest and most expensive
		// requests, and it would disappear silently: the field is simply absent
		// from whatever this request is re-encoded into.
		MaxTokens           json.RawMessage `json:"max_tokens,omitempty"`
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens,omitempty"`
		TopP                *float64        `json:"top_p,omitempty"`
		TopK                *int            `json:"top_k,omitempty"`
		TopA                *float64        `json:"top_a,omitempty"`
		MinP                *float64        `json:"min_p,omitempty"`
		Seed                *int64          `json:"seed,omitempty"`
		// Stop is OpenAI's "stop" field, which the API accepts as EITHER a
		// single string OR an array of strings — decoded via decodeStopSequences
		// below rather than a fixed shape, since a plain json.RawMessage array
		// element type would reject the scalar form outright.
		Stop              json.RawMessage `json:"stop,omitempty"`
		PresencePenalty   *float64        `json:"presence_penalty,omitempty"`
		FrequencyPenalty  *float64        `json:"frequency_penalty,omitempty"`
		RepetitionPenalty *float64        `json:"repetition_penalty,omitempty"`
		LogProbs          *bool           `json:"logprobs,omitempty"`
		TopLogProbs       *int            `json:"top_logprobs,omitempty"`
		Tools             json.RawMessage `json:"tools,omitempty"`
		ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
		ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
		StreamOptions     *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options,omitempty"`
		ReasoningEffort string `json:"reasoning_effort,omitempty"`
		Reasoning       *struct {
			BudgetTokens *int    `json:"budget_tokens,omitempty"`
			Effort       *string `json:"effort,omitempty"`
		} `json:"reasoning,omitempty"`
	}

	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse openai request: %w", err)
	}

	irReq := &protocols.IRRequest{
		Model:          model,
		Stream:         protocols.IRStreamConfig{Enabled: isStream, IncludeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage},
		Vendor:         protocols.NewVendorBag(),
		SourceProtocol: string(protocols.ProtocolOpenAI),
		RawBody:        body,
	}

	// Messages: role:"system" entries are extracted into IRRequest.System (mirroring the
	// responses/claude/gemini decoders) rather than kept as RoleSystem entries in Messages,
	// so that egress encoders that only read IRRequest.System (claude, gemini) see them.
	messages, system := decodeOpenAIMessages(req.Messages)
	irReq.Messages = messages
	irReq.System = system

	// Generation config
	// AllowExtendedParams=true: ingress is OpenAI Chat, so non-standard extended params
	// (top_k/top_a/min_p/repetition_penalty) were explicitly sent by the caller and should
	// be forwarded to the egress provider as-is.
	maxTokens, err := ceilingFrom(req.MaxTokens, "max_tokens")
	if err != nil {
		return nil, err
	}
	maxCompletionTokens, err := ceilingFrom(req.MaxCompletionTokens, "max_completion_tokens")
	if err != nil {
		return nil, err
	}

	irReq.Generation = protocols.IRGenerationConfig{
		Temperature:         req.Temperature,
		MaxTokens:           pickCeiling(maxTokens, maxCompletionTokens),
		TopP:                req.TopP,
		TopK:                req.TopK,
		TopA:                req.TopA,
		MinP:                req.MinP,
		Seed:                req.Seed,
		PresencePenalty:     req.PresencePenalty,
		FrequencyPenalty:    req.FrequencyPenalty,
		RepetitionPenalty:   req.RepetitionPenalty,
		LogProbs:            req.LogProbs,
		TopLogProbs:         req.TopLogProbs,
		AllowExtendedParams: true,
	}
	irReq.Generation.StopSequences = decodeStopSequences(req.Stop)

	// Tools
	irReq.Tools = decodeOpenAITools(req.Tools)
	irReq.ToolChoice, irReq.ToolChoiceName = decodeOpenAIToolChoice(req.ToolChoice)

	// Reasoning: support both reasoning_effort string and reasoning object ({budget_tokens, effort}).
	// reasoning object takes priority over reasoning_effort string when both are present.
	// Validation: only enable when effort is one of known values OR budget_tokens > 0.
	if req.Reasoning != nil {
		validEffort := req.Reasoning.Effort != nil && isKnownReasoningEffort(*req.Reasoning.Effort)
		validBudget := req.Reasoning.BudgetTokens != nil && *req.Reasoning.BudgetTokens > 0
		if validEffort || validBudget {
			rc := protocols.IRReasoningConfig{Enabled: true}
			if validEffort {
				rc.Effort = *req.Reasoning.Effort
			} else if validBudget {
				// budget-only: derive the effort string from the budget so that
				// cross-protocol conversion (e.g. Chat -> Responses egress) can
				// encode it correctly.
				rc.Effort = budgetToEffort(*req.Reasoning.BudgetTokens)
			}
			if validBudget {
				rc.BudgetTokens = req.Reasoning.BudgetTokens
			} else {
				// Derive budget from effort when no explicit budget provided
				budget := effortToBudget(rc.Effort)
				rc.BudgetTokens = &budget
			}
			irReq.Reasoning = rc
		}
	}
	if !irReq.Reasoning.Enabled && req.ReasoningEffort != "" && isKnownReasoningEffort(req.ReasoningEffort) {
		budget := effortToBudget(req.ReasoningEffort)
		irReq.Reasoning = protocols.IRReasoningConfig{
			Enabled:      true,
			BudgetTokens: &budget,
			Effort:       req.ReasoningEffort,
		}
	}

	// Response format
	if req.ResponseFormat != nil {
		irReq.ResponseFormat = decodeOpenAIResponseFormat(req.ResponseFormat)
	}

	return irReq, nil
}

// decodeStopSequences decodes OpenAI's "stop" field, which the API accepts as
// EITHER a single string OR an array of strings. A single string yields one
// stop sequence; an array yields one entry per string element (non-string
// elements are ignored, matching the previous array-only decoder's leniency);
// an absent or null field yields no stop sequences.
func decodeStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return nil
	}
	var out []string
	for _, s := range arr {
		var str string
		if json.Unmarshal(s, &str) == nil {
			out = append(out, str)
		}
	}
	return out
}

// decodeOpenAIMessages decodes the OpenAI Chat "messages" array into IR messages.
// It also extracts any role:"system" messages into a separate system string
// (joined with "\n\n" if there is more than one) instead of leaving them as
// RoleSystem entries in the returned messages, matching the other protocol decoders.
func decodeOpenAIMessages(raw json.RawMessage) ([]protocols.IRMessage, string) {
	type openaiMessage struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		ReasoningContent string          `json:"reasoning_content,omitempty"`
		ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
		ToolCallID       string          `json:"tool_call_id,omitempty"`
	}

	var msgs []openaiMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, ""
	}

	var result []protocols.IRMessage
	var systemParts []string
	for _, m := range msgs {
		irMsg := protocols.IRMessage{ToolCallID: m.ToolCallID}

		switch m.Role {
		case "system", "developer":
			// OpenAI's "developer" role carries the same system-level
			// instruction precedence as "system" (it replaced "system" for
			// o1-class models); coercing it to a user message via the
			// default branch would lose that precedence on cross-protocol
			// routing, so it is folded into System exactly like "system".
			if text := extractOpenAISystemText(m.Content); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		case "tool":
			irMsg.Role = protocols.RoleTool
			irMsg.Content = []protocols.IRContentBlock{
				protocols.BlockToolResult{
					ToolUseID: m.ToolCallID,
					Content:   m.Content,
				},
			}
		case "assistant":
			irMsg.Role = protocols.RoleAssistant
			irMsg.Content = decodeOpenAIAssistantContent(m.Content, m.ReasoningContent)
			irMsg.ToolCalls = decodeOpenAIToolCalls(m.ToolCalls)
		default: // "user"
			irMsg.Role = protocols.RoleUser
			irMsg.Content = decodeOpenAIUserContent(m.Content)
		}

		result = append(result, irMsg)
	}
	return result, strings.Join(systemParts, "\n\n")
}

// extractOpenAISystemText extracts plain text from an OpenAI message "content" field,
// which may be either a plain string or an array of content parts (e.g. [{"type":"text","text":"..."}]).
func extractOpenAISystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "")
	}
	return ""
}

func decodeOpenAIAssistantContent(content json.RawMessage, reasoning string) []protocols.IRContentBlock {
	var blocks []protocols.IRContentBlock

	if reasoning != "" {
		blocks = append(blocks, protocols.BlockThinking{Thinking: reasoning})
	}

	// Try string content first
	var str string
	if json.Unmarshal(content, &str) == nil {
		if str != "" {
			blocks = append(blocks, protocols.BlockText{Text: str})
		}
		return blocks
	}

	// Array content
	var parts []map[string]interface{}
	if json.Unmarshal(content, &parts) != nil {
		return blocks
	}

	for _, p := range parts {
		t, _ := p["type"].(string)
		switch t {
		case "text":
			if text, ok := p["text"].(string); ok && text != "" {
				blocks = append(blocks, protocols.BlockText{Text: text})
			}
		case "image_url":
			if img := parseImageURLPart(p); img != nil {
				blocks = append(blocks, *img)
			}
		}
	}
	return blocks
}

func decodeOpenAIUserContent(content json.RawMessage) []protocols.IRContentBlock {
	var str string
	if json.Unmarshal(content, &str) == nil {
		return []protocols.IRContentBlock{protocols.BlockText{Text: str}}
	}

	var parts []map[string]interface{}
	if json.Unmarshal(content, &parts) != nil {
		return []protocols.IRContentBlock{protocols.BlockText{Text: string(content)}}
	}

	var blocks []protocols.IRContentBlock
	for _, p := range parts {
		t, _ := p["type"].(string)
		switch t {
		case "text":
			if text, ok := p["text"].(string); ok {
				blocks = append(blocks, protocols.BlockText{Text: text})
			}
		case "image_url":
			if img := parseImageURLPart(p); img != nil {
				blocks = append(blocks, *img)
			}
		default:
			// Pass through as text
			if text, ok := p["text"].(string); ok {
				blocks = append(blocks, protocols.BlockText{Text: text})
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, protocols.BlockText{Text: string(content)})
	}
	return blocks
}

func decodeOpenAIToolCalls(raw json.RawMessage) []protocols.IRToolCall {
	if len(raw) == 0 {
		return nil
	}
	type toolCall struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	var tcs []toolCall
	if json.Unmarshal(raw, &tcs) != nil {
		return nil
	}
	result := make([]protocols.IRToolCall, len(tcs))
	for i, tc := range tcs {
		result[i] = protocols.IRToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
	}
	return result
}

func decodeOpenAITools(raw json.RawMessage) []protocols.IRToolSpec {
	if len(raw) == 0 {
		return nil
	}
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	specs := make([]protocols.IRToolSpec, 0, len(tools))
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		params := t.Function.Parameters
		if params == nil {
			params = json.RawMessage("{}")
		}
		specs = append(specs, protocols.IRToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
		})
	}
	return specs
}

func decodeOpenAIToolChoice(raw json.RawMessage) (protocols.IRToolChoice, string) {
	if len(raw) == 0 {
		return protocols.ToolChoiceAuto, ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		switch str {
		case "none":
			return protocols.ToolChoiceNone, ""
		case "required":
			return protocols.ToolChoiceRequired, ""
		default:
			return protocols.ToolChoiceAuto, ""
		}
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Function.Name != "" {
		return protocols.ToolChoiceNamed, obj.Function.Name
	}
	return protocols.ToolChoiceAuto, ""
}

func decodeOpenAIResponseFormat(raw json.RawMessage) *protocols.IRResponseFormat {
	if len(raw) == 0 {
		return nil
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
			Strict bool            `json:"strict"`
		} `json:"json_schema"`
	}
	if json.Unmarshal(raw, &rf) != nil {
		return nil
	}
	switch rf.Type {
	case "json_object":
		return &protocols.IRResponseFormat{Type: "json_object"}
	case "json_schema":
		if rf.JSONSchema != nil {
			return &protocols.IRResponseFormat{
				Type:   "json_schema",
				Name:   rf.JSONSchema.Name,
				Schema: rf.JSONSchema.Schema,
				Strict: rf.JSONSchema.Strict,
			}
		}
	}
	return nil
}

// parseImageURLPart extracts a protocols.BlockImage from an image_url content part.
func parseImageURLPart(p map[string]interface{}) *protocols.BlockImage {
	imgURL, _ := p["image_url"].(map[string]interface{})
	if imgURL == nil {
		return nil
	}
	url, _ := imgURL["url"].(string)
	isURL := true
	mediaType := "image/png"
	if strings.HasPrefix(url, "data:image/") {
		if idx := strings.Index(url, ";"); idx > 0 {
			mediaType = url[5:idx]
		}
		if dataIdx := findDataAfterBase64(url); dataIdx > 0 {
			url = url[dataIdx:]
			isURL = false
		}
	}
	return &protocols.BlockImage{MediaType: mediaType, Data: url, IsURL: isURL}
}

func findDataAfterBase64(url string) int {
	const marker = ";base64,"
	idx := 0
	for i := 0; i < len(url)-len(marker); i++ {
		if url[i:i+len(marker)] == marker {
			idx = i + len(marker)
			break
		}
	}
	return idx
}

func isKnownReasoningEffort(effort string) bool {
	switch effort {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}

func effortToBudget(effort string) int {
	switch effort {
	case "low":
		return 1000
	case "medium":
		return 10000
	case "high":
		return 80000
	default:
		return 0
	}
}

func budgetToEffort(budget int) string {
	switch {
	case budget <= 1000:
		return "low"
	case budget < 50000:
		return "medium"
	default:
		return "high"
	}
}

// ceilingFrom turns one stated ceiling into the integer the rest of the
// pipeline works in, or reports that the caller stated something that is not a
// ceiling at all.
//
// A wrong JSON type is an error rather than a silently ignored field. A quoted
// "4096" is the case that matters: json.Number would accept it, and then the
// request's validity would depend on where it was routed — a passthrough
// candidate forwards the string as written while a converted one normalises it
// to a number. A caller who sent the wrong type learns that from one rejection
// instead of from an upstream that only sometimes complains.
func ceilingFrom(raw json.RawMessage, field string) (*int, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		// Absent, or explicitly no preference. Neither states a ceiling, and
		// inventing one here would cap a request the caller left uncapped.
		return nil, nil
	}

	if strings.HasPrefix(s, `"`) {
		return nil, fmt.Errorf("parse openai request: %s must be a number, not a string", field)
	}

	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("parse openai request: %s: %w", field, err)
	}

	// The common spelling, and the only one that needs no arithmetic.
	if i, err := n.Int64(); err == nil {
		if i <= 0 {
			return nil, errNonPositiveCeiling(field, s)
		}
		v := saturateInt64(i)
		return &v, nil
	}

	// Non-positive, however it is spelled. A ceiling of zero or less states an
	// impossible request, and leaving it to travel is what makes validity
	// depend on the route: every encoder writes the ceiling only when it is
	// positive, so a converted candidate silently drops it (Claude then
	// substitutes its own default) while a same-protocol candidate forwards it
	// verbatim for the upstream to reject. Claude ingress already refuses these
	// on arrival; this is the same rule, applied where it was missing.
	//
	// A zero mantissa is answered without reading the exponent, which makes the
	// exponent irrelevant however enormous it is written.
	if strings.HasPrefix(s, "-") || !hasNonZeroMantissa(s) {
		return nil, errNonPositiveCeiling(field, s)
	}

	// Integrality is decided from the token's own digits, before any value is
	// built. Deciding it later would mean deciding it after the magnitude gate
	// below, which answers enormous values without looking at their digits at
	// all — and a fractional ceiling with 309 digits would then be accepted
	// while a fractional one with four digits is refused.
	if !isIntegralToken(s) {
		// A ceiling counts tokens, so a fractional one states nothing that can
		// be honoured. Refusing beats rounding: rounded down, 0.5 becomes the
		// zero every encoder omits; rounded up, the caller is given a cap they
		// did not ask for. It also keeps validity from depending on the route,
		// since a same-protocol candidate forwards the body as written while a
		// converted one would send whatever this rounded it to.
		return nil, fmt.Errorf("parse openai request: %s must be a whole number of tokens, got %s", field, abbreviateToken(s))
	}

	// How big is this, roughly? A dozen bytes of exponent can denote a number
	// with a million digits, and building that exactly would let a tiny field
	// cost megabytes — worse, math/big refuses outright past its own exponent
	// limit, which would turn "enormous" into a parse error at a threshold
	// nobody chose. float64 answers magnitude cheaply, and magnitude is all
	// that is left to ask.
	if _, ferr := strconv.ParseFloat(s, 64); ferr != nil {
		// The token is already a valid JSON number and already known positive,
		// so the only way to fail is being past float64's range — an enormous
		// stated ceiling, over every limit there is, which is what pinning it
		// to the top says.
		v := math.MaxInt
		return &v, nil
	}

	// Within float64's range and integral, so the exact value is read from the
	// digits rather than from f: float64 rounds 4095.9999999999999 up to 4096,
	// and a ceiling must never come back larger than it was stated.

	v := integralValue(s)
	return &v, nil
}

// abbreviateToken bounds what a rejection quotes back.
//
// The quoted value travels further than the error: it is written into the
// caller's response and stored as the request's failure reason. A number may be
// as long as the body allows, so echoing it whole turns one oversized field
// into an oversized response and an oversized audit row — paid per rejected
// request. The opening digits are what makes the message useful; the rest only
// makes it expensive.
//
// Byte slicing is safe here because a JSON number is ASCII by grammar.
func abbreviateToken(s string) string {
	if len(s) <= maxEchoedToken {
		return s
	}
	return s[:maxEchoedToken] + "...(truncated)"
}

// maxEchoedToken is long enough to show a ceiling anyone actually meant to
// send, and short enough that a rejection costs nothing to carry.
const maxEchoedToken = 40

// errNonPositiveCeiling is one sentence in one place, so that every spelling of
// a non-positive ceiling is refused for the same stated reason.
func errNonPositiveCeiling(field, token string) error {
	return fmt.Errorf("parse openai request: %s must be at least one token, got %s", field, abbreviateToken(token))
}

// integralValue reads the exact value of a positive, integral token whose
// magnitude is already known to fit in float64.
//
// It works from the SIGNIFICANT digits, never from the token's length. That is
// the whole point: "1." followed by a million zeros denotes 1, and a parser
// that builds what it reads would spend seconds and gigabytes arriving at that
// answer — from a body well inside the request size limit, and again for every
// candidate that decodes. Trimming is a scan; what gets built is at most the
// twenty digits an int can hold.
func integralValue(s string) int {
	mantissa, exponent := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent = s[:i], s[i+1:]
	}
	integer, fraction := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		integer, fraction = mantissa[:i], mantissa[i+1:]
	}
	integer = strings.TrimLeft(strings.TrimPrefix(integer, "+"), "0")
	fraction = strings.TrimRight(fraction, "0")

	exp, ok := exponentOf(exponent)
	if !ok {
		// Only a hugely positive exponent can reach here: the token is
		// positive, integral, and within float64's range, so a hugely negative
		// one would have been refused as fractional.
		return math.MaxInt
	}

	// Leading zeros are not digits of the value: 0.00000000000000000001e20 is
	// 1, and counting its twenty written zeros against the range would answer
	// an enormous ceiling for a request that asked for one token.
	digits := strings.TrimLeft(integer+fraction, "0")

	// How far the exponent still has to move the point. Positive, it owes
	// zeros; negative, it consumes them — and they are there to consume,
	// because a negative shift only survives isIntegralToken when the digits
	// end in at least that many zeros (which is also why this cannot cut into
	// a significant digit).
	shift := exp - len(fraction)

	if shift < 0 {
		digits = digits[:len(digits)+shift]
		shift = 0
	}

	if len(digits)+shift > maxIntDigits {
		// More digits than any int holds, so it is over every limit there is.
		return math.MaxInt
	}
	// The error is deliberately not consulted, and the value alone is enough.
	// Only one failure can reach here: nineteen digits is within the budget yet
	// can still exceed int64, and ParseInt answers that by returning the
	// maximum magnitude of the requested size — already the saturation this
	// wants. The other failure, a malformed number, cannot occur: what is
	// parsed is a digit string assembled from a token the JSON decoder already
	// accepted, and it is never empty (leading zeros are trimmed, so the first
	// digit is significant, and a negative shift only consumes trailing zeros).
	v, _ := strconv.ParseInt(digits+strings.Repeat("0", shift), 10, 64)
	return saturateInt64(v)
}

// maxIntDigits is the most decimal digits an int64 can hold. A token with more
// than this is past the range whatever its digits say, so it never needs to be
// assembled to be answered.
const maxIntDigits = 19

// isIntegralToken reports whether a JSON number denotes a whole number, judged
// from the token itself so that nothing has to be built to find out.
//
// The rule is one comparison: a value is whole when its exponent shifts the
// decimal point at least as far as it has fractional digits to consume — and,
// where it has none, when a negative exponent does not eat into the trailing
// zeros it would have to borrow from.
func isIntegralToken(s string) bool {
	mantissa, exponent := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa, exponent = s[:i], s[i+1:]
	}

	integer, fraction := mantissa, ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		integer, fraction = mantissa[:i], mantissa[i+1:]
	}
	// Trailing zeros in the fraction are not fractional digits: 4096.0 is a
	// whole number written with a decimal point.
	fraction = strings.TrimRight(fraction, "0")

	exp, ok := exponentOf(exponent)
	if !ok {
		// An exponent too large to hold is enormous in one direction or the
		// other. Positive, it shifts past any fraction; negative, it pushes
		// every digit below the point. (A zero mantissa never reaches here.)
		return !strings.HasPrefix(exponent, "-")
	}

	if fraction != "" {
		return exp >= len(fraction)
	}
	if exp >= 0 {
		return true
	}
	// No fractional digits, but a negative exponent has to borrow from the
	// integer side: 1000e-3 is whole, 100e-3 is not.
	integer = strings.TrimLeft(strings.TrimLeft(integer, "+-"), "0")
	return len(integer)-len(strings.TrimRight(integer, "0")) >= -exp
}

// exponentOf reads the exponent part, reporting !ok when it is too large to
// hold — a magnitude the caller of this helper answers on its own terms.
func exponentOf(exponent string) (int, bool) {
	if exponent == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(strings.TrimPrefix(exponent, "+"), 10, 32)
	if err != nil {
		return 0, false
	}
	return int(v), true
}

// hasNonZeroMantissa reports whether the token states a nonzero value, looking
// only at the mantissa. The exponent must be excluded: "0e5" is zero however
// large the 5 is, and reading it as nonzero would refuse a ceiling of zero that
// the caller is entitled to state.
func hasNonZeroMantissa(s string) bool {
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		s = s[:i]
	}
	return strings.ContainsAny(s, "123456789")
}

// saturateInt64 narrows an int64 to int, pinning rather than wrapping. On a
// 64-bit build the two are the same width and nothing is pinned; the guard is
// what keeps a 32-bit build from wrapping an enormous ceiling into a small one.
func saturateInt64(i int64) int {
	if i > int64(math.MaxInt) {
		return math.MaxInt
	}
	if i < int64(math.MinInt) {
		return math.MinInt
	}
	return int(i)
}

// pickCeiling settles which of OpenAI's two spellings of the output ceiling
// applies when a request carries both.
//
// The lower one wins. OpenAI's own precedence depends on the model — the
// reasoning models honour max_completion_tokens and reject max_tokens outright,
// older ones know only max_tokens — and the decoder does not know which model
// the request will end up at, since a candidate can rewrite it. Taking the
// lower is the only choice that cannot raise a ceiling the caller stated: a
// request that says "at most 4096" under either name is not asking for more
// than 4096, whichever spelling the destination reads.
//
// This is a choice, not a rule read off a specification: no upstream defines
// what a body carrying both spellings means, because no upstream reads both.
// It changes what this decoder used to do — max_tokens was taken and the other
// name ignored — and it changes it only for requests that state both, which a
// client sends by mistake or in transition. Erring low keeps the mistake cheap.
func pickCeiling(maxTokens, maxCompletionTokens *int) *int {
	switch {
	case maxTokens == nil:
		return maxCompletionTokens
	case maxCompletionTokens == nil:
		return maxTokens
	case *maxCompletionTokens < *maxTokens:
		return maxCompletionTokens
	default:
		return maxTokens
	}
}
