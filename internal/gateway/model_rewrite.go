package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// passthroughRequestBody prepares the same-protocol passthrough request body
// bound for the upstream, branching by egress protocol on how (or whether)
// the caller's external model name needs to become the candidate's
// provider_model_name in the outgoing bytes.
//
// OpenAI, Claude, and Responses requests all carry "model" as a top-level
// body field, so rewriteModelField swaps in the provider model name there,
// same as always. A native Gemini request has no such field at all — the
// model only ever appears in the URL path (EgressPath builds
// /v1beta/models/{providerModelName}:generateContent, applied by the caller
// right after this function returns) — so the captured request body is forwarded
// completely unchanged: injecting a top-level "model" key would add a field
// the real Gemini API's protojson-based request parser rejects as unknown,
// turning a passthrough request into a hard failure.
func passthroughRequestBody(egressProtocol protocols.ProtocolID, body []byte, providerModelName string) ([]byte, error) {
	if egressProtocol == protocols.ProtocolGemini {
		return body, nil
	}
	return rewriteModelField(body, providerModelName)
}

// ─────────────────── Same-protocol response rewriting ─────────────────────
//
// A same-protocol (ingress == egress) response is forwarded to the caller
// byte-for-byte rather than round-tripped through the IR, so the IR codec's
// field allowlist cannot silently drop a vendor-specific field the IR does not
// model. The helpers below are what a plain copy would still get wrong: the
// provider's own name for the model appears throughout the response, and it
// must never reach the caller, who asked for a different name and will send
// that name back on the next turn.
//
// Each protocol hides the name somewhere else — a top-level field, one frame
// of a stream, a nested envelope — so there is one rewriter per protocol and
// a dispatcher over them. None of them writes to the client or records
// anything; they take bytes and return bytes, and the caller decides what to
// do with the result.

// passthroughRewriteNonStreamResponse rewrites a non-stream same-protocol
// upstream response's model field back to the external name and extracts
// usage, branching by egress protocol so the live OpenAI path is untouched.
//
// For an OpenAI egress this is exactly RewriteNonStreamResponse (unchanged
// production behavior). For a Claude or Responses egress, the model-field
// rewrite is rewriteModelField (it operates on any JSON body with a
// top-level "model" field, which both response shapes have). For a Gemini
// egress, the provider's model name is NOT in a top-level "model" field at
// all -- a Gemini generateContent response instead echoes it back in
// "modelVersion" (see internal/protocols/gemini/response.go's
// ResponseDecoder) -- so rewriteGeminiResponseModelVersion is used instead,
// which rewrites that field in place and, critically, does NOT add a
// top-level "model" key the real Gemini response shape never has.
//
// Usage in every non-OpenAI branch is extracted via that protocol's own
// ResponseDecoder instead of the OpenAI-shaped extractUsage -- Claude
// reports input_tokens/output_tokens under a "usage" object and Gemini
// reports promptTokenCount/candidatesTokenCount under "usageMetadata",
// neither of which extractUsage's prompt_tokens/completion_tokens field
// names ever match, so without this branch a non-OpenAI passthrough
// response was byte-forwarded correctly but silently reported no usage (no
// billing).
//
// A decode error here does NOT fail the request: the caller-facing bytes are
// already correctly rewritten and about to be written to the client
// regardless, so usage is simply left nil (unknown, not zero) rather than
// failing this candidate over -- the response itself is not malformed, only
// unparseable for the gateway's own cost accounting.
func passthroughRewriteNonStreamResponse(egressProtocol protocols.ProtocolID, body []byte, externalModel string) ([]byte, *protocols.IRUsage, error) {
	if egressProtocol == protocols.ProtocolOpenAI {
		return RewriteNonStreamResponse(body, externalModel)
	}
	var rewritten []byte
	var err error
	if egressProtocol == protocols.ProtocolGemini {
		rewritten, err = rewriteGeminiResponseModelVersion(body, externalModel)
	} else {
		rewritten, err = rewriteModelField(body, externalModel)
	}
	if err != nil {
		return nil, nil, err
	}
	var usage *protocols.IRUsage
	if irResp, decErr := codecsFor(egressProtocol).ResponseDecoder.DecodeResponse(json.RawMessage(body)); decErr == nil && irResp != nil {
		usage = reportedUsage(&irResp.Usage)
	}
	return rewritten, usage, nil
}

// rewriteGeminiResponseModelVersion parses body as a JSON object and, if it
// carries a top-level "modelVersion" field (the field a Gemini
// generateContent response echoes the serving model name in), rewrites it to
// externalModel -- the Gemini counterpart of rewriteModelField's "model"
// rewrite. Unlike rewriteModelField, this deliberately does NOT add the
// field when it is absent: a real Gemini response body has no top-level
// "model" field at all, so a body that (unexpectedly) omits "modelVersion"
// is forwarded unchanged rather than gaining a field the wire shape never
// has.
func rewriteGeminiResponseModelVersion(body []byte, externalModel string) ([]byte, error) {
	return rewriteJSONStringField(body, "modelVersion", externalModel, true)
}

// rewriteSSEDataLineJSON applies mutate to the JSON object carried by a data
// SSE line. Returns the rewritten line and true only if it was a data line
// whose payload parsed and mutate reported a change; otherwise returns the
// original line and false. Preserves the trailing newline bytes.
//
// What counts as a data line — and where its payload starts — comes from the
// shared prefix rule, the same one isDataLine and the decoders answer with.
// Two answers to "is this a data line" is one too many: a provider that omits
// the space after the colon would have its frame counted as data — committing
// the response — while every rewrite below quietly did nothing, and the
// provider's own name for the model would go out to a caller who never heard
// of it.
func rewriteSSEDataLineJSON(line []byte, mutate func(obj map[string]json.RawMessage) bool) ([]byte, bool) {
	start, ok := protocols.SSEDataPayloadStart(line)
	if !ok {
		return line, false
	}
	// Whichever framing the provider used is put back verbatim. This function
	// renames a model; reformatting the caller's stream on the way past is not
	// its business, and a caller diffing against the provider would see us do it.
	prefix := line[:start]
	payload := line[start:]
	trimmed := bytes.TrimRight(payload, "\r\n")
	trailing := payload[len(trimmed):]

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return line, false
	}
	if !mutate(envelope) {
		return line, false
	}
	newEnvelopeJSON, err := json.Marshal(envelope)
	if err != nil {
		return line, false
	}
	newLine := make([]byte, 0, len(prefix)+len(newEnvelopeJSON)+len(trailing))
	newLine = append(newLine, prefix...)
	newLine = append(newLine, newEnvelopeJSON...)
	newLine = append(newLine, trailing...)
	return newLine, true
}

// rewriteClaudeMessageStartModel rewrites the model field nested in a Claude
// message_start SSE data line to the external model name, so a passthrough
// Claude stream never leaks the provider's internal model name. Any line that
// is not a "data: " line, not a message_start frame, or not parseable is
// returned unchanged (ok=false) so the rest of the stream is still forwarded
// byte-for-byte. Preserves the trailing newline(s).
func rewriteClaudeMessageStartModel(line []byte, externalModel string) ([]byte, bool) {
	// Fast pre-check to skip the JSON parse on every other frame — a Claude
	// stream sends exactly one message_start per response, so this substring
	// check is false for the (many) content_block_delta/message_delta lines.
	if !bytes.Contains(line, []byte("message_start")) {
		return line, false
	}
	return rewriteSSEDataLineJSON(line, func(envelope map[string]json.RawMessage) bool {
		var typ string
		if err := json.Unmarshal(envelope["type"], &typ); err != nil || typ != "message_start" {
			return false
		}
		msgRaw, ok := envelope["message"]
		if !ok {
			return false
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(msgRaw, &message); err != nil {
			return false
		}
		modelJSON, err := json.Marshal(externalModel)
		if err != nil {
			return false
		}
		message["model"] = modelJSON
		newMsgJSON, err := json.Marshal(message)
		if err != nil {
			return false
		}
		envelope["message"] = newMsgJSON
		return true
	})
}

// rewriteGeminiStreamModelVersion rewrites a Gemini stream chunk's top-level
// "modelVersion" field to the external model name. Unlike Claude's single
// message_start frame, every Gemini streamGenerateContent chunk carries
// "modelVersion", so this is applied to every forwarded line (no once-flag).
// Any line that is
// not a "data: " line, has no "modelVersion" field (the cheap substring
// pre-check below skips the JSON parse for the common case), or fails to
// parse is returned unchanged (ok=false) so the rest of the stream is still
// forwarded byte-for-byte. Preserves the trailing newline(s).
func rewriteGeminiStreamModelVersion(line []byte, externalModel string) ([]byte, bool) {
	if !bytes.Contains(line, []byte("modelVersion")) {
		return line, false
	}
	return rewriteSSEDataLineJSON(line, func(envelope map[string]json.RawMessage) bool {
		if _, present := envelope["modelVersion"]; !present {
			return false
		}
		modelJSON, err := json.Marshal(externalModel)
		if err != nil {
			return false
		}
		envelope["modelVersion"] = modelJSON
		return true
	})
}

// rewriteResponsesStreamModel rewrites the nested "response.model" field
// carried by a Responses SSE envelope event (response.created,
// response.in_progress, response.completed — see
// internal/protocols/responses/encoder.go's ensureCreated/makeCompleted;
// every other Responses event type has no top-level "response" object at
// all) to the external model name. Applied to every forwarded line, gated
// only on whether that line's envelope actually nests a "model" field under
// "response" — not on a fixed event-type allowlist — so this also covers
// any other envelope event a real upstream might emit the model on. Any
// line that is not a "data: " line, has no "response"/"model" shape (the
// cheap substring pre-check below skips the JSON parse for the common
// per-token delta events), or fails to parse is returned unchanged
// (ok=false). Preserves the trailing newline(s).
func rewriteResponsesStreamModel(line []byte, externalModel string) ([]byte, bool) {
	if !bytes.Contains(line, []byte(`"response"`)) || !bytes.Contains(line, []byte(`"model"`)) {
		return line, false
	}
	return rewriteSSEDataLineJSON(line, func(envelope map[string]json.RawMessage) bool {
		respRaw, ok := envelope["response"]
		if !ok {
			return false
		}
		var respObj map[string]json.RawMessage
		if err := json.Unmarshal(respRaw, &respObj); err != nil {
			return false
		}
		if _, present := respObj["model"]; !present {
			return false
		}
		modelJSON, err := json.Marshal(externalModel)
		if err != nil {
			return false
		}
		respObj["model"] = modelJSON
		newRespJSON, err := json.Marshal(respObj)
		if err != nil {
			return false
		}
		envelope["response"] = newRespJSON
		return true
	})
}

// rewritePassthroughStreamModel rewrites the model name embedded in one
// upstream SSE line back to the external caller-facing name, dispatching by
// egress protocol on where (and how often) that protocol's stream shape
// carries it: Claude nests it once, in the message_start frame
// (claudeModelRewritten latches true the first time that frame is
// successfully rewritten, so later lines — none of which carry the model in
// practice — skip the attempt); Gemini repeats "modelVersion" at the top
// level of every chunk, so it is checked (and, if present, rewritten) on
// every line; Responses nests it under "response.model" on the envelope
// events that carry a "response" object. Any egress protocol not listed here
// (OpenAI, whose frames are rewritten as they are scanned and so never reach
// this) or any line the relevant helper doesn't recognize is returned
// completely unchanged.
func rewritePassthroughStreamModel(egressProtocol protocols.ProtocolID, line []byte, externalModel string, claudeModelRewritten *bool) []byte {
	switch egressProtocol {
	case protocols.ProtocolClaude:
		if !*claudeModelRewritten {
			if newLine, ok := rewriteClaudeMessageStartModel(line, externalModel); ok {
				*claudeModelRewritten = true
				return newLine
			}
		}
	case protocols.ProtocolGemini:
		if newLine, ok := rewriteGeminiStreamModelVersion(line, externalModel); ok {
			return newLine
		}
	case protocols.ProtocolResponses:
		if newLine, ok := rewriteResponsesStreamModel(line, externalModel); ok {
			return newLine
		}
	}
	return line
}
