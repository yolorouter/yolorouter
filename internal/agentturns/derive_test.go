// Table tests for the write-time derivation's pure halves: the added
// message classification (total and lossless), the system timeline's
// deduplication, and the resend supplement's guard. The end-to-end parity
// with the whole-session pass lives in store_test.go and in the transcript
// read-path tests (internal/service/requestlog).
package agentturns

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// parseJSON is the tests' shortcut for building comparable values.
func parseJSON(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("parseJSON(%q): %v", raw, err)
	}
	return v
}

// TestClassifyAddedMessage pins the taxonomy's split: tool results by both
// dialects' shapes, machine injections by the marker table, and the
// user_input catch-all that keeps the classification total.
func TestClassifyAddedMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"plain user text", `{"role":"user","content":"fix the bug"}`, EventUserInput},
		{"user null content", `{"role":"user","content":null}`, EventUserInput},
		{"non-object element", `"a bare string message"`, EventUserInput},
		{"no role", `{"content":"anonymous"}`, EventUserInput},
		{"openai tool role", `{"role":"tool","tool_call_id":"c1","content":"42"}`, EventToolResult},
		{"anthropic tool_result block", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}`, EventToolResult},
		{"tool_result among text blocks", `{"role":"user","content":[{"type":"text","text":"and"},{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}`, EventToolResult},
		{"system-reminder wrapper", `{"role":"user","content":[{"type":"text","text":"<system-reminder>todo list</system-reminder>"}]}`, EventInjected},
		{"system-reminder string content", `{"role":"user","content":"<system-reminder>you have 2 tasks</system-reminder>"}`, EventInjected},
		{"command-name wrapper", `{"role":"user","content":"<command-name>/clear</command-name>"}`, EventInjected},
		{"caveat prefix", `{"role":"user","content":"Caveat: The messages below were generated..."}`, EventInjected},
		{"marker mid-text is not injection", `{"role":"user","content":"the docs say <system-reminder> is a wrapper"}`, EventUserInput},
		{"leading whitespace before marker", `{"role":"user","content":"\n  <system-reminder>reminder</system-reminder>"}`, EventInjected},
		{"empty text", `{"role":"user","content":""}`, EventUserInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyAddedMessage(parseJSON(t, tc.msg)); got != tc.want {
				t.Fatalf("ClassifyAddedMessage(%s) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// TestMessageCarriesToolUse pins the tool_call marker's test: only an
// assistant message whose content blocks carry a tool_use is marked.
func TestMessageCarriesToolUse(t *testing.T) {
	yes := `{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/a"}}]}`
	no := `{"role":"assistant","content":[{"type":"text","text":"plain"}]}`
	stringContent := `{"role":"assistant","content":"text reply"}`
	if !MessageCarriesToolUse(parseJSON(t, yes)) {
		t.Fatalf("tool_use copy not marked")
	}
	if MessageCarriesToolUse(parseJSON(t, no)) {
		t.Fatalf("text-only copy marked as tool call")
	}
	if MessageCarriesToolUse(parseJSON(t, stringContent)) {
		t.Fatalf("string-content copy marked as tool call")
	}
	if MessageCarriesToolUse("not an object") {
		t.Fatalf("non-object marked")
	}
}

// deriveCase is one derivation scenario: two adjacent bodies plus the
// baseline, with the expected events described declaratively.
type deriveCase struct {
	name     string
	ownBody  string
	prevBody string
	prev     *PrevBody // nil = first request of the session
	baseline SystemBaseline
	// prevWantNoSupplement asserts no ForPrev events are derived (the
	// predecessor recorded its reply inline).
	prevWantNoSupplement bool
	wantTypes            []string // own-batch event types in order (system→messages→assistant)
	// wantAdded is the `added` array the whole-session pass computes for
	// ownBody against prevBody — the losslessness oracle.
	wantAdded []string
}

// TestDeriveTurnEventsAddedIsLossless pins the hard contract: for every
// suffix shape, the user_input/tool_result/injected events concatenate —
// in order, parsed — to exactly the array AssembleAgentSessionReplay keeps
// in `added` for the same two bodies. The assistant row and any supplement
// ride after.
func TestDeriveTurnEventsAddedIsLossless(t *testing.T) {
	cases := []deriveCase{
		{
			name:      "first request keeps everything",
			ownBody:   `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"go"}]}`,
			prev:      nil,
			wantTypes: []string{EventUserInput, EventUserInput, EventAssistantReply},
			wantAdded: []string{`{"role":"user","content":"hi"}`, `{"role":"user","content":"go"}`},
		},
		{
			name:    "suffix of one with mixed kinds",
			ownBody: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"go"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]},{"role":"user","content":"<system-reminder>note</system-reminder>"},{"role":"user","content":"after"}]}`,
			prev:    &PrevBody{RequestBody: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"go"}]}`},
			// prev HAS an inline reply recorded → the resent copy of "yo"
			// is dropped by the pass, and no supplement is written.
			prevWantNoSupplement: true,
			wantTypes:            []string{EventToolResult, EventInjected, EventUserInput, EventAssistantReply},
			wantAdded: []string{
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}`,
				`{"role":"user","content":"<system-reminder>note</system-reminder>"}`,
				`{"role":"user","content":"after"}`,
			},
		},
		{
			name:      "identical retry yields empty suffix",
			ownBody:   `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`,
			prev:      &PrevBody{RequestBody: `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`},
			wantTypes: []string{EventAssistantReply},
			wantAdded: nil,
		},
		{
			name:      "unparseable body derives only the assistant row",
			ownBody:   `{"messages":[`,
			prev:      &PrevBody{RequestBody: `{"messages":[]}`},
			wantTypes: []string{EventAssistantReply},
			wantAdded: nil,
		},
		{
			name:      "capped own body degrades like the read paths",
			ownBody:   `{"messages":[{"role":"user","content":"` + strings.Repeat("x", MaxInlineBodyBytes+64) + `"}]}`,
			prev:      &PrevBody{RequestBody: `{"messages":[]}`},
			wantTypes: []string{EventAssistantReply},
			wantAdded: nil, // the pass parses a capped body to nothing
		},
		{
			name:      "capped PREV body empties the diff base (whole own array is new)",
			ownBody:   `{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`,
			prev:      &PrevBody{RequestBody: `{"messages":[{"role":"user","content":"` + strings.Repeat("y", MaxInlineBodyBytes+64) + `"}]}`},
			wantTypes: []string{EventUserInput, EventUserInput, EventAssistantReply},
			wantAdded: []string{`{"role":"user","content":"a"}`, `{"role":"user","content":"b"}`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := DeriveTurnEvents(OwnBody{RequestBody: tc.ownBody}, tc.prev, tc.baseline)

			// Losslessness: the pass's added for the same pair.
			ownTrunc := len(tc.ownBody) > MaxInlineBodyBytes
			ownMsgs, _, _ := ParseReplayBody(tc.ownBody, ownTrunc)
			var prevMsgs []any
			prevTrunc := tc.prev != nil && len(tc.prev.RequestBody) > MaxInlineBodyBytes
			if tc.prev != nil {
				prevMsgs, _, _ = ParseReplayBody(tc.prev.RequestBody, prevTrunc)
			}
			oracleBlocks, _ := AssembleAgentSessionReplay([]AgentSessionReplayInput{
				{RequestBody: tc.prevBodyOrEmpty(), RequestBodyTruncated: prevTrunc},
				{RequestBody: tc.ownBody, RequestBodyTruncated: ownTrunc},
			})
			_ = ownMsgs
			_ = prevMsgs
			var oracleAdded []any
			if len(oracleBlocks) == 2 {
				oracleAdded = oracleBlocks[1].Added
			}

			gotAdded := []any{}
			var ownTypes []string
			for _, ev := range events {
				if ev.ForPrev {
					continue
				}
				ownTypes = append(ownTypes, ev.EventType)
				switch ev.EventType {
				case EventUserInput, EventToolResult, EventInjected:
					gotAdded = append(gotAdded, parseJSON(t, ev.Content))
				}
			}
			if !reflect.DeepEqual(gotAdded, oracleAdded) {
				t.Fatalf("derived added = %+v, want the pass's added %+v", gotAdded, oracleAdded)
			}
			if !reflect.DeepEqual(ownTypes, tc.wantTypes) {
				t.Fatalf("own event types = %v, want %v", ownTypes, tc.wantTypes)
			}
			// The declared wantAdded is a second, human-readable oracle.
			want := []any{}
			for _, w := range tc.wantAdded {
				want = append(want, parseJSON(t, w))
			}
			if !reflect.DeepEqual(gotAdded, want) {
				t.Fatalf("derived added = %+v, want %+v", gotAdded, want)
			}
			if tc.prevWantNoSupplement {
				for _, ev := range events {
					if ev.ForPrev {
						t.Fatalf("supplement written although the predecessor recorded an inline reply: %+v", ev)
					}
				}
			}
		})
	}
}

// prevBodyOrEmpty returns the case's previous body as the pass input shape.
func (tc *deriveCase) prevBodyOrEmpty() string {
	if tc.prev == nil {
		return ""
	}
	return tc.prev.RequestBody
}

// TestDeriveTurnEventsSystemTimeline pins the system channel: a value is
// recorded only when it differs from the baseline, with the baseline's
// next ordinal; a capped body records nothing.
func TestDeriveTurnEventsSystemTimeline(t *testing.T) {
	const body = `{"system":"S-two","messages":[{"role":"user","content":"q"}]}`

	// No baseline: the first value records at ordinal 0.
	events := DeriveTurnEvents(OwnBody{RequestBody: body}, nil, SystemBaseline{})
	sys := findSystemEvent(t, events)
	if sys == nil || sys.Ordinal != 0 || sys.Raw != "S-two" {
		t.Fatalf("first system event = %+v, want ordinal 0 with S-two", sys)
	}

	// Same value as baseline: nothing recorded.
	events = DeriveTurnEvents(OwnBody{RequestBody: body}, nil, SystemBaseline{Raw: "S-two", Has: true, NextOrdinal: 3})
	if findSystemEvent(t, events) != nil {
		t.Fatalf("unchanged system value recorded a timeline entry")
	}

	// Different value: recorded at the baseline's next ordinal.
	events = DeriveTurnEvents(OwnBody{RequestBody: body}, nil, SystemBaseline{Raw: "S-one", Has: true, NextOrdinal: 3})
	sys = findSystemEvent(t, events)
	if sys == nil || sys.Ordinal != 3 || sys.Raw != "S-two" {
		t.Fatalf("changed system event = %+v, want ordinal 3 with S-two", sys)
	}

	// OpenAI dialect: role=system messages are the system value.
	events = DeriveTurnEvents(OwnBody{RequestBody: `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"q"}]}`}, nil, SystemBaseline{})
	sys = findSystemEvent(t, events)
	if sys == nil {
		t.Fatalf("openai-dialect system not recorded")
	}
	want := []any{map[string]any{"role": "system", "content": "be brief"}}
	if !reflect.DeepEqual(sys.Raw.([]any), want) && !reflect.DeepEqual([]any{sys.Raw}, want) {
		// The pass records the collected system messages as an array; the
		// envelope keeps that shape verbatim.
		t.Fatalf("openai system raw = %+v, want the collected message", sys.Raw)
	}
}

// findSystemEvent picks the system_prompt event out of a derivation, nil
// when there is none.
func findSystemEvent(t *testing.T, events []TurnEvent) *systemEventContent {
	t.Helper()
	for _, ev := range events {
		if ev.EventType != EventSystemPrompt {
			continue
		}
		var envelope systemEventContent
		if err := json.Unmarshal([]byte(ev.Content), &envelope); err != nil {
			t.Fatalf("system envelope: %v", err)
		}
		if ev.ContentBytes != int64(len(mustMarshalJSON(envelope.Raw))) {
			t.Fatalf("system content_bytes = %d, want the raw's serialized size %d", ev.ContentBytes, len(mustMarshalJSON(envelope.Raw)))
		}
		return &envelope
	}
	return nil
}

// TestDeriveTurnEventsSupplement pins the resend supplement: written only
// for a predecessor without an inline capture, attributed ForPrev, carrying
// the copies in order plus tool_call markers for the tool_use-bearing ones
// alone.
func TestDeriveTurnEventsSupplement(t *testing.T) {
	// The successor's suffix resends two assistant messages: a plain reply
	// and a tool_use copy.
	const ownBody = `{"messages":[` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","content":"here you go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"path":"/a"}}]},` +
		`{"role":"user","content":"next"}` +
		`]}`
	const prevBody = `{"messages":[{"role":"user","content":"q"}]}`

	// Predecessor recorded its reply inline → no supplement.
	events := DeriveTurnEvents(OwnBody{RequestBody: ownBody}, &PrevBody{RequestBody: prevBody, HasInlineReply: true}, SystemBaseline{})
	for _, ev := range events {
		if ev.ForPrev {
			t.Fatalf("supplement written for an inline-recorded predecessor: %+v", ev)
		}
	}

	// Predecessor did not → supplement carries both copies and marks the
	// tool_use one.
	events = DeriveTurnEvents(OwnBody{RequestBody: ownBody}, &PrevBody{RequestBody: prevBody}, SystemBaseline{})
	var resend *TurnEvent
	var toolCalls []TurnEvent
	for i, ev := range events {
		if !ev.ForPrev {
			continue
		}
		if ev.Seq < SupplementSeqBase {
			t.Fatalf("supplement event below the supplement seq base: %+v", ev)
		}
		if ev.EventType == EventAssistantReply {
			resend = &events[i]
		}
		if ev.EventType == EventToolCall {
			toolCalls = append(toolCalls, ev)
		}
	}
	if resend == nil {
		t.Fatalf("no resend supplement derived")
	}
	var envelope assistantEventContent
	if err := json.Unmarshal([]byte(resend.Content), &envelope); err != nil || envelope.Source != ReplaySourceResend {
		t.Fatalf("resend envelope = %+v (err %v), want source resend", envelope, err)
	}
	if len(envelope.Raw) != 2 {
		t.Fatalf("resend copies = %d, want 2 (both assistant messages in order)", len(envelope.Raw))
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool_call events = %d, want exactly the tool_use-bearing copy", len(toolCalls))
	}

	// First request of a session: no predecessor, no supplement ever.
	events = DeriveTurnEvents(OwnBody{RequestBody: ownBody}, nil, SystemBaseline{})
	for _, ev := range events {
		if ev.ForPrev {
			t.Fatalf("supplement written without a predecessor: %+v", ev)
		}
	}
}

// TestDeriveTurnEventsAssistantRowSource pins the informational source on
// the request's own assistant row: inline > stream > none from the capture
// presences — the read path re-derives it live, so this pins only what the
// row self-describes.
func TestDeriveTurnEventsAssistantRowSource(t *testing.T) {
	const body = `{"messages":[{"role":"user","content":"q"}]}`
	cases := []struct {
		name string
		own  OwnBody
		want string
	}{
		{"inline", OwnBody{RequestBody: body, HasInlineReply: true, HasStreamCapture: true}, ReplaySourceInline},
		{"stream", OwnBody{RequestBody: body, HasStreamCapture: true}, ReplaySourceStream},
		{"none", OwnBody{RequestBody: body}, ReplaySourceNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := DeriveTurnEvents(tc.own, nil, SystemBaseline{})
			var last *TurnEvent
			for i := range events {
				if events[i].EventType == EventAssistantReply && !events[i].ForPrev {
					last = &events[i]
				}
			}
			if last == nil {
				t.Fatalf("no own assistant row derived")
			}
			var envelope assistantEventContent
			if err := json.Unmarshal([]byte(last.Content), &envelope); err != nil {
				t.Fatalf("assistant envelope: %v", err)
			}
			if envelope.Source != tc.want {
				t.Fatalf("assistant source = %q, want %q", envelope.Source, tc.want)
			}
		})
	}
}
