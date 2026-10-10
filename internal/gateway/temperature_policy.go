package gateway

// The model-capability temperature policy. Two site switches decide what
// temperature the upstream receives: the default switch injects the
// addressed model's admin-declared default when the caller did not send
// one, the force switch overrides a temperature the caller did send. Both
// stand down when the model declares no default — there is nothing to
// inject with and nothing to override with — and both default to off, so
// an untouched deployment forwards exactly what it forwarded before.
//
// The decision runs on the intermediate representation, between the
// ingress decode and the egress encode, so every cross-protocol conversion
// carries it. The same-protocol passthrough path has no IR by design —
// the caller's bytes are forwarded with only the model field rewritten —
// so the same decision is applied there by JSON field surgery, the
// precedent the model-name rewrite and the stream-usage injection already
// set on exactly these bytes.
//
// A protocol constraint outranks both switches. The Claude encoder forces
// temperature=1.0 whenever thinking is enabled and runs AFTER this policy,
// so a declared default (injected or forced) is simply overwritten — the
// existing override keeps priority. On the Claude passthrough path no
// encoder runs, so the same rule holds by standing down entirely when the
// caller's body carries a thinking configuration: injecting a declared
// default there would hand the upstream a request it refuses.

import (
	"encoding/json"
	"fmt"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/settings"
)

// temperatureDecision is the policy isolated from any wire shape: given
// the model's declared default, the temperature the caller sent (nil =
// absent), and the enforcement switches, it answers whether to touch the
// request at all and with what value.
//
//	declared nil                          -> never touch
//	force on                              -> the declared default, always
//	default on and the caller sent none   -> the declared default
//	anything else                         -> leave the request alone
//
// Force without a declaration is inert rather than an error: the override
// source IS the declaration, so a model without one leaves force with
// nothing to force with. An explicitly sent null counts as not sent — the
// caller removed the parameter, they did not set it.
func temperatureDecision(declared, sent *float64, ce settings.CapabilityEnforcementSetting) (*float64, bool) {
	if declared == nil {
		return nil, false
	}
	if ce.TemperatureForce {
		return declared, true
	}
	if ce.TemperatureDefault && sent == nil {
		return declared, true
	}
	return nil, false
}

// applyTemperaturePolicy writes the decision onto the intermediate
// representation. It runs after the ingress decode (the caller's
// temperature, if any, is ir.Generation.Temperature by then) and before
// the egress encode, so every egress codec — including the Claude
// encoder's thinking temperature floor — sees the policy's result as an
// ordinary value to encode or override.
func applyTemperaturePolicy(ir *protocols.IRRequest, declared *float64, ce settings.CapabilityEnforcementSetting) {
	if v, ok := temperatureDecision(declared, ir.Generation.Temperature, ce); ok {
		ir.Generation.Temperature = v
	}
}

// applyPassthroughTemperature applies the same decision to a
// same-protocol passthrough body, where no IR exists. Where the egress
// protocol carries the temperature as a top-level field (OpenAI Chat,
// Claude Messages, Responses) the field is set or replaced directly;
// Gemini nests it under generationConfig, so that object is created or
// amended. A request the policy declines to touch — switches off, nothing
// declared, caller explicit with force off, or the Claude thinking guard —
// comes back byte-for-byte unchanged.
func applyPassthroughTemperature(body []byte, egress protocols.ProtocolID, declared *float64, ce settings.CapabilityEnforcementSetting) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse %s passthrough body for temperature policy: %w", egress, err)
	}
	if m == nil {
		// A JSON literal rather than an object; admission would have
		// refused anything that reached here shaped like this, so the
		// honest answer is to hand the bytes back untouched.
		return body, nil
	}

	if egress == protocols.ProtocolGemini {
		return geminiGenerationTemperature(body, m, declared, ce)
	}

	// The Claude thinking guard: a body that carries a thinking
	// configuration declares the protocol constraint itself, and on this
	// path no encoder will enforce it after us. Standing down leaves the
	// caller's own sampling state exactly as they sent it.
	if egress == protocols.ProtocolClaude {
		if raw, ok := m["thinking"]; ok && string(raw) != "null" {
			return body, nil
		}
	}

	v, ok := temperatureDecision(declared, rawNumberPtr(m["temperature"]), ce)
	if !ok {
		return body, nil
	}
	encoded, err := json.Marshal(*v)
	if err != nil {
		return nil, fmt.Errorf("encode temperature %v: %w", *v, err)
	}
	m["temperature"] = encoded
	return json.Marshal(m)
}

// geminiGenerationTemperature applies the decision inside Gemini's
// generationConfig object, creating the object when the caller sent none.
// A generationConfig that exists but is not a JSON object makes the policy
// decline — the caller put something structurally odd where the
// temperature would go, and declining leaves a body a lenient upstream
// might still accept intact.
func geminiGenerationTemperature(body []byte, m map[string]json.RawMessage, declared *float64, ce settings.CapabilityEnforcementSetting) ([]byte, error) {
	var gc map[string]json.RawMessage
	if raw, ok := m["generationConfig"]; ok && string(raw) != "null" {
		if err := json.Unmarshal(raw, &gc); err != nil || gc == nil {
			return body, nil
		}
	}
	if gc == nil {
		gc = map[string]json.RawMessage{}
	}
	v, ok := temperatureDecision(declared, rawNumberPtr(gc["temperature"]), ce)
	if !ok {
		return body, nil
	}
	encoded, err := json.Marshal(*v)
	if err != nil {
		return nil, fmt.Errorf("encode temperature %v: %w", *v, err)
	}
	gc["temperature"] = encoded
	gcJSON, err := json.Marshal(gc)
	if err != nil {
		return nil, err
	}
	m["generationConfig"] = gcJSON
	return json.Marshal(m)
}

// rawNumberPtr reads a raw JSON value as a number pointer for the policy's
// "did the caller send one" question. Absent or null read as not-sent —
// the caller removed the parameter rather than setting it — and a
// non-numeric value reads the same way: the injection question is about
// presence, and a value the policy cannot read is one the site's declared
// default may replace (a caller who sent a number keeps it unless force
// is on).
func rawNumberPtr(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v float64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return &v
}
