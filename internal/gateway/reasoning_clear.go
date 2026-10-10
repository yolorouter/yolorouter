package gateway

// The model-capability reasoning egress clear. Where the strict entry
// refusal answers a reasoning parameter aimed at the ADDRESSED model with a
// 400, this rule looks the other way — at the candidate the chain is about
// to send to — and, when that landing is declared (false) not to reason,
// strips the reasoning configuration from the outbound body instead of
// refusing anything. The request still goes out; it goes out as the
// non-reasoning request the landing can actually serve. That asymmetry is
// the point: the caller's addressed model is the caller's error to be told
// about, a failover landing is not.
//
// The evaluation point is per candidate, at PrepareUpstream time, so a
// failover re-asks the question for the new landing — the scenario that
// motivated the rule: a reasoning request routed to a provider whose model
// rejects thinking parameters answers 400 upstream, and the walk burns a
// round trip and quota learning it. Today every candidate of a request
// carries the addressed model's declaration (the row is model-level), so
// every landing answers the same; the per-candidate shape is deliberate,
// so a per-candidate declaration slots in without moving this seam.
//
// Both body forms are covered. On the IR path the clear zeroes the
// reasoning config between the ingress decode and the egress encode, so
// every encoder — Claude's thinking injection, Gemini's thinkingConfig,
// OpenAI's reasoning_effort — finds nothing to emit. On the same-protocol
// passthrough path, where the caller's bytes travel with no IR, the
// matching field is deleted from the forwarded JSON directly. A body with
// nothing to strip comes back byte-for-byte unchanged.

import (
	"encoding/json"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/settings"
)

// reasoningClearDue answers whether this landing candidate's declaration
// strips the request's reasoning configuration. Only an explicit false:
// nil (undeclared) keeps today's behaviour — an admin who never said the
// model cannot reason must not have the gateway editing reasoning requests
// on their behalf — and true is support, nothing to do. The strict entry
// flag is deliberately not consulted: the two rules answer different
// questions (the caller's addressed model versus the request's landing),
// and either may be on with the other off.
func reasoningClearDue(cand Candidate, ce settings.CapabilityEnforcementSetting) bool {
	if !ce.ReasoningEgressClear {
		return false
	}
	return cand.DeclaredReasoning != nil && !*cand.DeclaredReasoning
}

// clearIRReasoning zeroes the IR's reasoning config ahead of the egress
// encode. The request encoders emit their protocol's reasoning field only
// when Reasoning.Enabled is set, so the zero value is the whole strip — no
// encoder has a reasoning shape the zero config can still produce.
func clearIRReasoning(ir *protocols.IRRequest) {
	ir.Reasoning = protocols.IRReasoningConfig{}
}

// stripPassthroughReasoning deletes the reasoning fields from a
// same-protocol passthrough body. Each protocol keeps its reasoning
// configuration somewhere different — Claude a top-level thinking object,
// Gemini a thinkingConfig nested inside generationConfig, OpenAI Chat the
// reasoning_effort string and the budgeted reasoning object, Responses a
// reasoning object — and the clear speaks each dialect where it lives. A
// body carrying none of them, an egress protocol with no known reasoning
// field, or a JSON literal rather than an object comes back byte-for-byte
// unchanged: no re-marshal happens without a deletion, so key order and
// formatting the caller chose survive untouched.
func stripPassthroughReasoning(body []byte, egress protocols.ProtocolID) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse %s passthrough body for reasoning clear: %w", egress, err)
	}
	if m == nil {
		return body, nil
	}
	switch egress {
	case protocols.ProtocolOpenAI:
		// The wire spells reasoning twice — the reasoning_effort string and
		// the budgeted reasoning object — and the passthrough forwards
		// whichever the caller sent, so both must go.
		if !rawFieldPresent(m, "reasoning_effort") && !rawFieldPresent(m, "reasoning") {
			return body, nil
		}
		delete(m, "reasoning_effort")
		delete(m, "reasoning")
	case protocols.ProtocolResponses:
		if !rawFieldPresent(m, "reasoning") {
			return body, nil
		}
		delete(m, "reasoning")
	case protocols.ProtocolClaude:
		if !rawFieldPresent(m, "thinking") {
			return body, nil
		}
		delete(m, "thinking")
	case protocols.ProtocolGemini:
		changed, err := stripGeminiThinkingConfig(m)
		if err != nil || !changed {
			return body, err
		}
	default:
		// An egress protocol with no known reasoning field has nothing to
		// strip; the bytes stand.
		return body, nil
	}
	return json.Marshal(m)
}

// stripGeminiThinkingConfig removes generationConfig.thinkingConfig and
// writes the amended generationConfig back. changed is false — and the body
// untouched — when the field is absent, or when a generationConfig that
// exists is not a JSON object: the caller put something structurally odd
// where the config would nest, and declining leaves bytes a lenient
// upstream might still accept intact, the same decline rule the temperature
// policy takes for the same shape.
func stripGeminiThinkingConfig(m map[string]json.RawMessage) (bool, error) {
	raw, ok := m["generationConfig"]
	if !ok || string(raw) == "null" {
		return false, nil
	}
	var gc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gc); err != nil || gc == nil {
		return false, nil
	}
	if !rawFieldPresent(gc, "thinkingConfig") {
		return false, nil
	}
	delete(gc, "thinkingConfig")
	encoded, err := json.Marshal(gc)
	if err != nil {
		return false, err
	}
	m["generationConfig"] = encoded
	return true, nil
}

// rawFieldPresent reports a JSON object field that is set and non-null: a
// field the caller explicitly nulled is one they removed, not one they
// sent, so it reads as absent to every presence question in this family.
func rawFieldPresent(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	return ok && string(raw) != "null"
}
