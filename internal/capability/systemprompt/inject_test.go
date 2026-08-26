package systemprompt

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// stubView stands in for the exchange.
//
// The capability declares the view it needs, so a test answers exactly those
// three questions — no exchange to construct, no kernel to start, and no way
// for a test to accidentally depend on state the capability never reads.
type stubView struct {
	enabled bool
	prompt  string
	chat    bool
}

func (v stubView) CustomSystemPromptEnabled() bool { return v.enabled }
func (v stubView) CustomSystemPrompt() string      { return v.prompt }
func (v stubView) IsChatEndpoint() bool            { return v.chat }

// recordingSink captures what was reported, so a test can check the capability
// does not claim an injection that did not happen.
type recordingSink struct {
	facts   []fact.Fact
	records []fact.Record
}

func (s *recordingSink) Report(f ...fact.Fact) { s.facts = append(s.facts, f...) }
func (s *recordingSink) Note(r ...fact.Record) { s.records = append(s.records, r...) }

func apply(t *testing.T, v stubView, proto protocols.ProtocolID, body []byte) ([]byte, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	out, err := New().RewriteEgress(context.Background(), v, proto, body, sink)
	if err != nil {
		t.Fatalf("RewriteEgress returned an error: %v", err)
	}
	return out, sink
}

func TestDisabledPromptLeavesBodyAlone(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, sink := apply(t, stubView{chat: true}, protocols.ProtocolOpenAI, body)
	if string(out) != string(body) {
		t.Fatal("a disabled prompt must return the body unchanged")
	}
	if len(sink.records) != 0 {
		t.Errorf("nothing was injected, so nothing should be recorded: %v", sink.records)
	}
}

func TestPromptAppendsToExistingSystemText(t *testing.T) {
	v := stubView{chat: true, enabled: true, prompt: "BE CONCISE"}
	body := []byte(`{"messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"hi"}]}`)
	out, sink := apply(t, v, protocols.ProtocolOpenAI, body)
	if !strings.Contains(string(out), "BE CONCISE") || !strings.Contains(string(out), "You are helpful.") {
		t.Fatalf("expected both the original and the custom text, got %s", out)
	}
	if len(sink.records) != 1 {
		t.Fatalf("want exactly one record for one injection, got %d", len(sink.records))
	}
	rec, ok := sink.records[0].(fact.SystemPromptInjected)
	if !ok {
		t.Fatalf("recorded the wrong type: %T", sink.records[0])
	}
	if rec.ExtraChars != len("BE CONCISE") {
		t.Errorf("ExtraChars = %d, want %d", rec.ExtraChars, len("BE CONCISE"))
	}
	if len(sink.facts) != 0 {
		t.Errorf("injection must not steer the relay, but facts were reported: %v", sink.facts)
	}
}

// A route outside the chat allowlist has no system text to speak of; injecting
// there would corrupt a request the caller meant literally.
func TestNonChatRouteIsSkipped(t *testing.T) {
	v := stubView{chat: false, enabled: true, prompt: "X"}
	body := []byte(`{"models":["x"]}`)
	out, sink := apply(t, v, protocols.ProtocolOpenAI, body)
	if string(out) != string(body) {
		t.Fatal("a non-chat route must not be injected")
	}
	if len(sink.records) != 0 {
		t.Errorf("nothing was injected, so nothing should be recorded: %v", sink.records)
	}
}

// A body this capability cannot parse may still be one an upstream accepts.
// Rewriting it blind is the one outcome worse than not injecting at all, so the
// body is returned untouched — and, just as importantly, nothing is recorded,
// because a record claiming an injection the body contradicts is worse than no
// record.
func TestMalformedBodyIsReturnedUntouchedAndUnrecorded(t *testing.T) {
	v := stubView{chat: true, enabled: true, prompt: "X"}
	for _, b := range [][]byte{nil, []byte(``), []byte(`null`), []byte(`not json`), []byte(`{}`)} {
		out, sink := apply(t, v, protocols.ProtocolOpenAI, b)
		if string(out) != string(b) {
			t.Fatalf("malformed body must be unchanged: in=%q out=%q", b, out)
		}
		if len(sink.records) != 0 {
			t.Errorf("in=%q: nothing was injected, so nothing should be recorded: %v", b, sink.records)
		}
	}
}

// An unrecognised egress protocol is a route this capability has no injection
// format for. Declining is correct; guessing is not.
func TestUnknownEgressProtocolDeclines(t *testing.T) {
	v := stubView{chat: true, enabled: true, prompt: "X"}
	body := []byte(`{"messages":[]}`)
	out, sink := apply(t, v, protocols.ProtocolID("something-else"), body)
	if string(out) != string(body) {
		t.Fatal("an unknown egress protocol must leave the body unchanged")
	}
	if len(sink.records) != 0 {
		t.Errorf("nothing was injected, so nothing should be recorded: %v", sink.records)
	}
}

// enabledView is the common case for the shape tests below: switched on, chat
// route, a fixed prompt. The tests vary only the body.
var enabledView = stubView{chat: true, enabled: true, prompt: "PROMPT"}

// mustInject runs the rewriter and fails the test unless exactly one injection
// was recorded — the shape tests all expect the injection to happen and differ
// only in where the text lands.
func mustInject(t *testing.T, proto protocols.ProtocolID, body string) map[string]interface{} {
	t.Helper()
	out, sink := apply(t, enabledView, proto, []byte(body))
	if len(sink.records) != 1 {
		t.Fatalf("want exactly one injection record, got %d (body %s)", len(sink.records), out)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return m
}

// mustDecline runs the rewriter and fails the test unless the body came back
// byte-identical with nothing recorded — the preserve tests all expect that.
func mustDecline(t *testing.T, proto protocols.ProtocolID, body string) {
	t.Helper()
	out, sink := apply(t, enabledView, proto, []byte(body))
	if string(out) != body {
		t.Fatalf("body must be preserved byte-for-byte:\n in=%s\nout=%s", body, out)
	}
	if len(sink.records) != 0 {
		t.Errorf("nothing was injected, so nothing should be recorded: %v", sink.records)
	}
}

// ─── OpenAI chat shapes ───

func TestChatPrependsASystemMessageWhenNoneExists(t *testing.T) {
	m := mustInject(t, protocols.ProtocolOpenAI, `{"messages":[{"role":"user","content":"hi"}]}`)
	msgs := m["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("want a prepended system message, got %d messages", len(msgs))
	}
	first := msgs[0].(map[string]interface{})
	if first["role"] != "system" || first["content"] != "PROMPT" {
		t.Errorf("prepended message = %v", first)
	}
}

func TestChatAppendsAPartToArrayContent(t *testing.T) {
	m := mustInject(t, protocols.ProtocolOpenAI,
		`{"messages":[{"role":"system","content":[{"type":"text","text":"orig"}]},{"role":"user","content":"hi"}]}`)
	sys := m["messages"].([]interface{})[0].(map[string]interface{})
	parts := sys["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("want the part appended, got %d parts", len(parts))
	}
	last := parts[1].(map[string]interface{})
	if last["type"] != "text" || last["text"] != "PROMPT" {
		t.Errorf("appended part = %v", last)
	}
}

// A system message whose content has an unexpected type is a malformed request.
// Overwriting the content would silently destroy what the caller wrote, so the
// prompt travels in a fresh message instead and the original stays intact.
func TestChatMalformedContentGetsAFreshMessageInstead(t *testing.T) {
	m := mustInject(t, protocols.ProtocolOpenAI,
		`{"messages":[{"role":"system","content":42},{"role":"user","content":"hi"}]}`)
	msgs := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("want a fresh message prepended, got %d messages", len(msgs))
	}
	if msgs[0].(map[string]interface{})["content"] != "PROMPT" {
		t.Errorf("fresh message = %v", msgs[0])
	}
	if orig, _ := msgs[1].(map[string]interface{})["content"].(float64); orig != 42 {
		t.Errorf("original malformed content must be preserved, got %v", msgs[1])
	}
}

// An empty system text has nothing worth keeping in front of the prompt, and a
// separator against nothing would ship a leading blank line to the upstream.
func TestEmptySystemTextTakesThePromptWithoutASeparator(t *testing.T) {
	m := mustInject(t, protocols.ProtocolOpenAI,
		`{"messages":[{"role":"system","content":""}]}`)
	sys := m["messages"].([]interface{})[0].(map[string]interface{})
	if sys["content"] != "PROMPT" {
		t.Errorf("content = %q, want the prompt alone with no leading separator", sys["content"])
	}
}

// ─── Responses shapes ───

func TestResponsesPrefersTopLevelInstructions(t *testing.T) {
	m := mustInject(t, protocols.ProtocolResponses,
		`{"instructions":"orig","input":[{"role":"system","content":"sys"}]}`)
	if m["instructions"] != "orig\n\nPROMPT" {
		t.Errorf("instructions = %q", m["instructions"])
	}
	sys := m["input"].([]interface{})[0].(map[string]interface{})
	if sys["content"] != "sys" {
		t.Errorf("input[] must be untouched when instructions took the text: %v", sys)
	}
}

func TestResponsesNonStringInstructionsIsPreserved(t *testing.T) {
	mustDecline(t, protocols.ProtocolResponses, `{"instructions":42,"input":[]}`)
}

// A JSON null instructions is read as "no instructions", not as a malformed
// value: nothing of the caller's is destroyed by writing over a null.
func TestResponsesNullInstructionsIsTreatedAsAbsent(t *testing.T) {
	m := mustInject(t, protocols.ProtocolResponses, `{"instructions":null,"input":[]}`)
	if m["instructions"] != "PROMPT" {
		t.Errorf("instructions = %q", m["instructions"])
	}
}

// Two system-like items, a user item between them: the text must land on the
// LAST one and only there. A single-item body cannot tell last-wins from
// first-wins, so this is the case that pins the priority.
func TestResponsesAppendsToTheLastSystemItem(t *testing.T) {
	m := mustInject(t, protocols.ProtocolResponses,
		`{"input":[{"role":"system","content":"first"},{"role":"user","content":"hi"},{"role":"developer","content":"second"}]}`)
	input := m["input"].([]interface{})
	if first := input[0].(map[string]interface{}); first["content"] != "first" {
		t.Errorf("the earlier system item must stay untouched, got %q", first["content"])
	}
	if dev := input[2].(map[string]interface{}); dev["content"] != "second\n\nPROMPT" {
		t.Errorf("last system-like content = %q", dev["content"])
	}
	if _, exists := m["instructions"]; exists {
		t.Error("instructions must not be created when a system item took the text")
	}
}

func TestResponsesArrayContentGetsAnInputTextPart(t *testing.T) {
	m := mustInject(t, protocols.ProtocolResponses,
		`{"input":[{"role":"system","content":[{"type":"input_text","text":"orig"}]}]}`)
	parts := m["input"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	last := parts[len(parts)-1].(map[string]interface{})
	if last["type"] != "input_text" || last["text"] != "PROMPT" {
		t.Errorf("appended part = %v (the Responses part type is input_text, not text)", last)
	}
}

func TestResponsesFallsBackToInstructionsWhenNoSystemItemExists(t *testing.T) {
	m := mustInject(t, protocols.ProtocolResponses,
		`{"input":[{"role":"user","content":"hi"}]}`)
	if m["instructions"] != "PROMPT" {
		t.Errorf("instructions = %q", m["instructions"])
	}
	if len(m["input"].([]interface{})) != 1 {
		t.Error("input[] must be untouched when instructions took the text")
	}
}

// ─── Gemini shapes ───

func TestGeminiAppendsAPart(t *testing.T) {
	m := mustInject(t, protocols.ProtocolGemini,
		`{"systemInstruction":{"parts":[{"text":"orig"}]},"contents":[]}`)
	parts := m["systemInstruction"].(map[string]interface{})["parts"].([]interface{})
	if len(parts) != 2 || parts[1].(map[string]interface{})["text"] != "PROMPT" {
		t.Errorf("parts = %v", parts)
	}
}

func TestGeminiCreatesTheFieldWhenAbsent(t *testing.T) {
	m := mustInject(t, protocols.ProtocolGemini, `{"contents":[]}`)
	parts := m["systemInstruction"].(map[string]interface{})["parts"].([]interface{})
	if len(parts) != 1 || parts[0].(map[string]interface{})["text"] != "PROMPT" {
		t.Errorf("created field = %v", m["systemInstruction"])
	}
}

// When the caller spelled the field snake_case, the injection follows that
// spelling instead of introducing a camelCase twin beside it.
func TestGeminiHonoursTheSnakeCaseSpelling(t *testing.T) {
	m := mustInject(t, protocols.ProtocolGemini,
		`{"system_instruction":{"parts":[{"text":"orig"}]},"contents":[]}`)
	if _, camel := m["systemInstruction"]; camel {
		t.Fatal("a camelCase twin must not appear beside the caller's snake_case field")
	}
	parts := m["system_instruction"].(map[string]interface{})["parts"].([]interface{})
	if len(parts) != 2 {
		t.Errorf("parts = %v", parts)
	}
}

func TestGeminiMalformedShapesArePreserved(t *testing.T) {
	mustDecline(t, protocols.ProtocolGemini, `{"systemInstruction":"boom","contents":[]}`)
	mustDecline(t, protocols.ProtocolGemini, `{"systemInstruction":{"parts":"boom"},"contents":[]}`)
}

// A JSON null systemInstruction (or null parts) is read as absent, matching the
// null-instructions rule on the Responses side: nothing is destroyed by
// replacing a null.
func TestGeminiNullShapesAreTreatedAsAbsent(t *testing.T) {
	for _, body := range []string{
		`{"systemInstruction":null,"contents":[]}`,
		`{"systemInstruction":{"parts":null},"contents":[]}`,
	} {
		m := mustInject(t, protocols.ProtocolGemini, body)
		parts := m["systemInstruction"].(map[string]interface{})["parts"].([]interface{})
		if len(parts) != 1 {
			t.Fatalf("null shape must be replaced by exactly one fresh part, got %v (in %s)", parts, body)
		}
		// The record alone does not prove the text arrived: assert the part
		// itself, or a rewrite that dropped the prompt would still pass.
		if part := parts[0].(map[string]interface{}); part["text"] != "PROMPT" {
			t.Errorf("parts[0] = %v, want the prompt text (in %s)", part, body)
		}
	}
}

// ─── Claude shapes ───

func TestClaudeAppendsASystemBlock(t *testing.T) {
	m := mustInject(t, protocols.ProtocolClaude,
		`{"system":[{"type":"text","text":"orig"}],"messages":[]}`)
	blocks := m["system"].([]interface{})
	if len(blocks) != 2 {
		t.Fatalf("want the block appended, got %d blocks", len(blocks))
	}
	if blocks[1].(map[string]interface{})["text"] != "PROMPT" {
		t.Errorf("appended block = %v", blocks[1])
	}
}

func TestClaudeNormalizesAStringSystemToBlocks(t *testing.T) {
	m := mustInject(t, protocols.ProtocolClaude, `{"system":"orig","messages":[]}`)
	blocks := m["system"].([]interface{})
	if len(blocks) != 2 || blocks[0].(map[string]interface{})["text"] != "orig" ||
		blocks[1].(map[string]interface{})["text"] != "PROMPT" {
		t.Errorf("system = %v", m["system"])
	}
}

// A malformed system field makes the Claude injector skip — and the body must
// come back byte-identical, not merely semantically equal: a re-encode would
// shuffle key order on a request nothing changed, and the record would then
// claim an injection the body contradicts.
func TestClaudeMalformedSystemIsPreservedByteForByte(t *testing.T) {
	mustDecline(t, protocols.ProtocolClaude, `{"system":42,"messages":[]}`)
}

// ─── Cross-protocol guarantees ───

// A body with trailing content after the JSON object must not be injected:
// re-encoding would forward only the first object and silently drop the rest,
// repairing a request the upstream should have rejected.
func TestTrailingGarbageIsNotTruncated(t *testing.T) {
	mustDecline(t, protocols.ProtocolOpenAI, `{"messages":[{"role":"user","content":"hi"}]} {"x":1}`)
	mustDecline(t, protocols.ProtocolGemini, `{"contents":[]}]`)
	mustDecline(t, protocols.ProtocolClaude, `{"system":"orig","messages":[]} garbage`)
}
