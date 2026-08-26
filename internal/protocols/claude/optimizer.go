package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// JSONHasTrailingContent reports whether dec has any non-whitespace content
// left after decoding one top-level JSON value (a second value, stray bytes, a
// lone ']' or '}', etc.). dec.More() misses trailing ']'/'}' because it
// returns false for them, so this instead requires that decoding once more
// strictly returns io.EOF (trailing whitespace/newlines still count as EOF).
//
// Exported so the gateway can reuse the same trailing-content check when
// decoding strict one-object bodies for custom system prompt injection,
// keeping a single source of truth for what "reject trailing JSON" means.
func JSONHasTrailingContent(dec *json.Decoder) bool {
	var rest json.RawMessage
	return !errors.Is(dec.Decode(&rest), io.EOF)
}

func isAdaptiveModel(model string) bool {
	lower := strings.ToLower(strings.ReplaceAll(model, ".", "-"))
	for _, needle := range []string{
		"opus-4-8", "opus-4-7", "opus-4-6", "sonnet-4-6",
		"fable-5", "mythos-5", "mythos-preview",
	} {
		// exact match: needle must be followed by a separator or the string's end,
		// preventing sonnet-4-60 from being misidentified as sonnet-4-6
		if strings.Contains(lower, needle+"-") || strings.HasSuffix(lower, needle) {
			return true
		}
	}
	return false
}

// OptimizeBody applies three transformations to an already-encoded Claude request
// body in a single parse/marshal pass:
//  1. Thinking type upgrade (only when irReq.Reasoning.Enabled == true)
//  2. Custom system prompt append (only when customPrompt != ""; must happen before
//     cache_control markers are added)
//  3. cache_control breakpoint injection (only when withCacheControl == true)
//
// Returns (the modified body, whether thinking was actually injected).
func OptimizeBody(body []byte, irReq *protocols.IRRequest, withCacheControl bool, customPrompt string) ([]byte, bool) {
	var m map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return body, false
	}

	// A null / non-object top-level body decodes to m == nil; assigning into a nil
	// map during injection would panic, so return the body unchanged and let the
	// upstream reject it.
	if m == nil {
		return body, false
	}
	// Reject trailing content (a second JSON value / stray bytes after the body):
	// we must not silently forward only the first object and drop the rest.
	if JSONHasTrailingContent(dec) {
		return body, false
	}

	thinkingInjected := upgradeThinking(m, irReq)
	if customPrompt != "" {
		injectCustomSystemPromptClaude(m, customPrompt)
	}
	if withCacheControl {
		injectCacheControl(m)
	}

	result, err := json.Marshal(m)
	if err != nil {
		return body, thinkingInjected
	}
	return result, thinkingInjected
}

// InjectCacheControl injects up to 4 ephemeral cache_control breakpoints into an
// already-encoded Claude request body. Used on the passthrough path (irReq is
// nil): it only injects the custom prompt + cache_control and never touches thinking.
func InjectCacheControl(body []byte, customPrompt string) []byte {
	var m map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return body
	}
	if m == nil {
		return body
	}
	if JSONHasTrailingContent(dec) {
		return body
	}
	if customPrompt != "" {
		injectCustomSystemPromptClaude(m, customPrompt)
	}
	injectCacheControl(m)
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

// InjectCustomSystemPromptOnly appends only the custom system prompt, without
// adding any cache_control markers. Used for Claude-compatible providers whose
// ProviderType isn't anthropic (they have no prompt-caching concept).
func InjectCustomSystemPromptOnly(body []byte, customPrompt string) []byte {
	if customPrompt == "" {
		return body
	}
	var m map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return body
	}
	if m == nil {
		return body
	}
	if JSONHasTrailingContent(dec) {
		return body
	}
	// A skipped injection (malformed system field) must return the ORIGINAL
	// bytes: re-encoding would shuffle key order and whitespace on a body this
	// call chose to preserve, and a caller comparing bytes would read that
	// cosmetic rewrite as an injection that never happened.
	if !injectCustomSystemPromptClaude(m, customPrompt) {
		return body
	}
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

// InjectBetaHeaders writes the anthropic-beta request header (append semantics)
// based on the model and whether thinking was injected. Adaptive models don't
// need an extra beta header; only legacy manual thinking needs the
// interleaved-thinking beta.
func InjectBetaHeaders(req *http.Request, irReq *protocols.IRRequest, thinkingInjected bool) {
	if !thinkingInjected || isAdaptiveModel(irReq.Model) {
		return
	}
	appendBeta(req, "interleaved-thinking-2025-05-14")
}

// InjectBetaHeadersFromBody handles beta header injection on the passthrough path
// (irReq is nil). If the body contains thinking:{type:"enabled"} and the model is
// legacy, it adds the interleaved-thinking beta.
func InjectBetaHeadersFromBody(req *http.Request, body []byte, model string) {
	if isAdaptiveModel(model) {
		return
	}
	var m map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return
	}
	th, ok := m["thinking"].(map[string]interface{})
	if !ok {
		return
	}
	if thType, _ := th["type"].(string); thType == "enabled" {
		appendBeta(req, "interleaved-thinking-2025-05-14")
	}
}

// adaptiveEffort maps IR reasoning effort to Anthropic output_config.effort.
// "low"/"medium"/"high" map literally; an unset or unknown value returns "max"
// (highest quality).
func adaptiveEffort(effort string) string {
	switch effort {
	case "low", "medium", "high":
		return effort
	default:
		return "max"
	}
}

func appendBeta(req *http.Request, beta string) {
	var parts []string
	for _, v := range req.Header.Values("anthropic-beta") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" && p != beta {
				parts = append(parts, p)
			}
		}
	}
	parts = append(parts, beta)
	req.Header.Set("anthropic-beta", strings.Join(parts, ","))
}

// jsonInt reads an integer value from a map, handling both json.Number (UseNumber
// mode) and float64.
func jsonInt(m map[string]interface{}, key string, defaultVal int) int {
	switch n := m[key].(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i)
		}
	case float64:
		return int(n)
	}
	return defaultVal
}

// upgradeThinking handles the thinking upgrade logic and returns whether
// thinking was actually injected.
func upgradeThinking(m map[string]interface{}, irReq *protocols.IRRequest) bool {
	if irReq == nil || !irReq.Reasoning.Enabled {
		return false
	}

	maxTokens := jsonInt(m, "max_tokens", 4096)
	adaptive := isAdaptiveModel(irReq.Model)

	if !adaptive && maxTokens <= 1024 {
		// Legacy thinking requires budget_tokens >= 1024 < max_tokens; adaptive has no
		// such constraint, so it skips this guard.
		// Restore the sampling parameters encoder.go forced when it wrote thinking.
		delete(m, "thinking")
		if irReq.Generation.Temperature != nil {
			m["temperature"] = *irReq.Generation.Temperature
		} else {
			delete(m, "temperature")
		}
		if irReq.Generation.TopP != nil {
			m["top_p"] = *irReq.Generation.TopP
		}
		return false
	}

	if adaptive {
		m["thinking"] = map[string]interface{}{"type": "adaptive"}
		effort := adaptiveEffort(irReq.Reasoning.Effort)
		// merge rather than a full replace, to preserve any other output_config
		// fields already present in the body
		oc, ok := m["output_config"].(map[string]interface{})
		if !ok {
			oc = map[string]interface{}{}
		}
		oc["effort"] = effort
		m["output_config"] = oc
		// Anthropic requires temperature=1 and top_p unset for every thinking mode
		// (including adaptive). encoder.go already enforces this when
		// Reasoning.Enabled==true, so the optimizer doesn't need to repeat it.
	} else {
		// Prefer the budget explicitly set on irReq, then let the value encoder.go
		// wrote into the body override it
		budget := 4096
		if irReq.Reasoning.BudgetTokens != nil {
			budget = *irReq.Reasoning.BudgetTokens
		}
		if thinking, ok := m["thinking"].(map[string]interface{}); ok {
			if bt := jsonInt(thinking, "budget_tokens", 0); bt > 0 {
				budget = bt
			}
		}
		if budget < 1024 {
			budget = 1024
		}
		if upper := maxTokens - 1; budget > upper {
			budget = upper
		}
		m["thinking"] = map[string]interface{}{
			"type":          "enabled",
			"budget_tokens": budget,
		}
	}
	return true
}

// injectCacheControl injects up to 4 cache_control breakpoints into the body.
// Injection order: end of tools -> end of system -> the last non-thinking block
// of the final assistant message.
func injectCacheControl(m map[string]interface{}) {
	budget := 4 - countCacheControls(m)
	if budget <= 0 {
		return
	}

	// (a) the last item in tools
	if tools, ok := m["tools"].([]interface{}); ok && len(tools) > 0 {
		if tool, ok := tools[len(tools)-1].(map[string]interface{}); ok {
			if addCacheControl(tool) {
				budget--
			}
		}
	}
	if budget <= 0 {
		return
	}

	// (b) the last block in system; normalize a string value to array form first
	if s, ok := m["system"].(string); ok && s != "" {
		m["system"] = []interface{}{map[string]interface{}{"type": "text", "text": s}}
	}
	if sys, ok := m["system"].([]interface{}); ok && len(sys) > 0 {
		if block, ok := sys[len(sys)-1].(map[string]interface{}); ok {
			if addCacheControl(block) {
				budget--
			}
		}
	}
	if budget <= 0 {
		return
	}

	// (c) the last non-thinking/redacted_thinking block of the final assistant message
	msgs, ok := m["messages"].([]interface{})
	if !ok {
		return
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		content, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		for j := len(content) - 1; j >= 0; j-- {
			block, ok := content[j].(map[string]interface{})
			if !ok {
				continue
			}
			bType, _ := block["type"].(string)
			if bType == "thinking" || bType == "redacted_thinking" {
				continue
			}
			addCacheControl(block)
			break
		}
		break
	}
}

// addCacheControl injects an ephemeral breakpoint if the block doesn't already
// have a cache_control, and reports whether it did.
func addCacheControl(block map[string]interface{}) bool {
	if _, has := block["cache_control"]; has {
		return false
	}
	block["cache_control"] = map[string]interface{}{"type": "ephemeral"}
	return true
}

// countFlatList counts elements with a cache_control in a flat list (tools /
// system), returning early once the running count reaches cap.
func countFlatList(items []interface{}, count, cap int) int {
	for _, item := range items {
		if m, ok := item.(map[string]interface{}); ok {
			if _, has := m["cache_control"]; has {
				count++
				if count >= cap {
					return count
				}
			}
		}
	}
	return count
}

// countCacheControls counts the cache_control breakpoints already present in the
// body (capped at 4).
func countCacheControls(m map[string]interface{}) int {
	const max = 4
	count := 0
	if tools, ok := m["tools"].([]interface{}); ok {
		count = countFlatList(tools, count, max)
		if count >= max {
			return count
		}
	}
	if sys, ok := m["system"].([]interface{}); ok {
		count = countFlatList(sys, count, max)
		if count >= max {
			return count
		}
	}
	if msgs, ok := m["messages"].([]interface{}); ok {
		for _, msg := range msgs {
			if message, ok := msg.(map[string]interface{}); ok {
				if content, ok := message["content"].([]interface{}); ok {
					for _, blk := range content {
						if b, ok := blk.(map[string]interface{}); ok {
							if _, has := b["cache_control"]; has {
								count++
								if count >= max {
									return count
								}
							}
						}
					}
				}
			}
		}
	}
	return count
}

// injectCustomSystemPromptClaude appends the custom system prompt to the body's
// system field, and reports whether it actually did.
// Must be called before injectCacheControl (within the same parse/marshal pass,
// see OptimizeBody/InjectCacheControl) so that the appended text can participate
// in deciding which is "the current last system block" for cache_control.
// Always converges to array form and drops the plain-string branch, so callers
// don't depend on injectCacheControl's implicit string-to-array normalization.
//
// The return value matters to a caller whose only job is the injection: when a
// malformed system field makes it skip, re-encoding the body anyway would
// rewrite key order and whitespace on a request this function decided to
// preserve — a cosmetic rewrite that reads as a change to anything comparing
// bytes.
func injectCustomSystemPromptClaude(m map[string]interface{}, customPrompt string) bool {
	block := map[string]interface{}{"type": "text", "text": customPrompt}

	sysVal, exists := m["system"]
	if !exists {
		// system field is entirely absent (a Claude request may omit it): create it.
		m["system"] = []interface{}{block}
		return true
	}
	switch s := sysVal.(type) {
	case string:
		if s == "" {
			// Nothing worth preserving in an empty string: just use the custom prompt
			// (matches the empty-text handling of joinSystemText on the relay side).
			m["system"] = []interface{}{block}
		} else {
			// non-empty string: normalize to an array and append.
			m["system"] = []interface{}{
				map[string]interface{}{"type": "text", "text": s},
				block,
			}
		}
		return true
	case []interface{}:
		m["system"] = append(s, block)
		return true
	default:
		// system is present but has an unexpected type (object/number/bool/null,
		// typically a malformed request): don't overwrite the client's original
		// value; skip injection and let the upstream reject it, matching the
		// preserve semantics of the chat/responses/gemini branches on the relay
		// side. injectCacheControl also only handles system in string/array form,
		// so it won't touch this malformed value either.
		return false
	}
}
