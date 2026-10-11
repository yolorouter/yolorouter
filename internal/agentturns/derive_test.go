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
// user_input catch-all that keeps the classification total. The READ-SIDE
// MIRROR of this table lives in frontend/src/utils/turnEvents.test.ts —
// the two tables pin the SAME arm set one-to-one; if either grows, both
// do, so a classification change here fails the frontend mirror too.
func TestClassifyAddedMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{"plain user text", `{"role":"user","content":"fix the bug"}`, EventUserInput},
		{"human text in content blocks", `{"role":"user","content":[{"type":"text","text":"看看这个仓库"}]}`, EventUserInput},
		{"user null content", `{"role":"user","content":null}`, EventUserInput},
		{"non-object element", `"a bare string message"`, EventUserInput},
		{"no role", `{"content":"anonymous"}`, EventUserInput},
		{"unrecognized role", `{"role":"developer","content":"house rules"}`, EventUserInput},
		{"openai tool role", `{"role":"tool","tool_call_id":"c1","content":"42"}`, EventToolResult},
		{"anthropic tool_result block", `{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}`, EventToolResult},
		{"tool_result among text blocks", `{"role":"user","content":[{"type":"text","text":"and"},{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}`, EventToolResult},
		{"system-reminder wrapper", `{"role":"user","content":[{"type":"text","text":"<system-reminder>todo list</system-reminder>"}]}`, EventInjected},
		{"system-reminder string content", `{"role":"user","content":"<system-reminder>you have 2 tasks</system-reminder>"}`, EventInjected},
		{"command-name wrapper", `{"role":"user","content":"<command-name>/clear</command-name>"}`, EventInjected},
		{"command-message wrapper", `{"role":"user","content":"<command-message>compact</command-message>"}`, EventInjected},
		{"caveat prefix", `{"role":"user","content":"Caveat: The messages below were generated..."}`, EventInjected},
		{"ide_selection block", `{"role":"user","content":[{"type":"text","text":"<ide_selection>lines 1 to 1 of a.jsonl</ide_selection>"}]}`, EventInjected},
		{"ide_opened_file string content", `{"role":"user","content":"<ide_opened_file>The user opened the file b.jsonl in the IDE."}`, EventInjected},
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

// TestSplitUserMessageForAdded pins the block-level split of multi-block
// user messages — the shapes the 2026-10-11 main-session walkthrough caught
// in the wild: an IDE-coupled client rides its editor context and the
// human's words as SIBLING text blocks of one user message. The splitter
// must pull the machine blocks out as their own injected elements, keep
// the human blocks as user_input, preserve block order, and leave every
// shape it cannot improve byte-identical (single element, original object).
func TestSplitUserMessageForAdded(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		// want is the expected element sequence: each element's JSON and
		// the class the taxonomy assigns it.
		want []struct {
			el    string
			class string
		}
	}{
		{
			name: "ide context and human text split into two elements",
			msg:  `{"role":"user","content":[{"type":"text","text":"<ide_selection>picked lines</ide_selection>"},{"type":"text","text":"fix it"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"<ide_selection>picked lines</ide_selection>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"fix it"}]}`, EventUserInput},
			},
		},
		{
			name: "pure injection yields injected elements and no user_input",
			msg:  `{"role":"user","content":[{"type":"text","text":"<ide_selection>picked</ide_selection>"},{"type":"text","text":"<ide_opened_file>opened x.jsonl</ide_opened_file>"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"<ide_selection>picked</ide_selection>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"<ide_opened_file>opened x.jsonl</ide_opened_file>"}]}`, EventInjected},
			},
		},
		{
			// The stored body keeps Go's HTML-safe escaping ("\u003c" for
			// "<"); json.Unmarshal — the decode ParseReplayBody runs before
			// any classification — restores the character, so the marker
			// table must and does match on the decoded text.
			name: "escaped \\u003c wire form judges on the decoded text",
			msg:  `{"role":"user","content":[{"type":"text","text":"\u003cide_selection\u003epicked\u003c/ide_selection\u003e"},{"type":"text","text":"real words"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"<ide_selection>picked</ide_selection>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"real words"}]}`, EventUserInput},
			},
		},
		{
			// Interleaved machine blocks all extract; the human's blocks —
			// wherever they sit — stay ONE element (one message, one
			// bubble): the re-ask shape the real session showed
			// ([ide, q1, ide, q2] arrives as one message quoting two
			// questions).
			name: "interleaved injection extracts every machine block, human blocks merge into one element",
			msg:  `{"role":"user","content":[{"type":"text","text":"<ide_selection>a</ide_selection>"},{"type":"text","text":"first question"},{"type":"text","text":"<system-reminder>r</system-reminder>"},{"type":"text","text":"second question"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"<ide_selection>a</ide_selection>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"first question"},{"type":"text","text":"second question"}]}`, EventUserInput},
			},
		},
		{
			// The shape the main-session walkthrough actually caught: a
			// classic-led message carrying a reminder, an ide block, and
			// the human's words — the whole-message rule used to bury the
			// words inside one injected event.
			name: "reminder-led message with buried human text still rescues the words",
			msg:  `{"role":"user","content":[{"type":"text","text":"<system-reminder>todo</system-reminder>"},{"type":"text","text":"<ide_opened_file>x.jsonl</ide_opened_file>"},{"type":"text","text":"查看需求说明书"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"<system-reminder>todo</system-reminder>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"<ide_opened_file>x.jsonl</ide_opened_file>"}]}`, EventInjected},
				{`{"role":"user","content":[{"type":"text","text":"查看需求说明书"}]}`, EventUserInput},
			},
		},
		{
			name: "no injected blocks keep the original object",
			msg:  `{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, EventUserInput},
			},
		},
		{
			name: "non-text blocks ride the kept run",
			msg:  `{"role":"user","content":[{"type":"image","source":{"type":"base64"}},{"type":"text","text":"look"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"image","source":{"type":"base64"}},{"type":"text","text":"look"}]}`, EventUserInput},
			},
		},
		{
			// String content has no block boundaries to split at; the
			// whole-message marker rule still applies (here: injected).
			name: "string content stays one whole element",
			msg:  `{"role":"user","content":"<ide_opened_file>opened</ide_opened_file> plain"}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":"<ide_opened_file>opened</ide_opened_file> plain"}`, EventInjected},
			},
		},
		{
			name: "tool_result body never splits",
			msg:  `{"role":"user","content":[{"type":"text","text":"<ide_selection>x</ide_selection>"},{"type":"tool_result","tool_use_id":"t","content":"ok"}]}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"user","content":[{"type":"text","text":"<ide_selection>x</ide_selection>"},{"type":"tool_result","tool_use_id":"t","content":"ok"}]}`, EventToolResult},
			},
		},
		{
			name: "non-user role and non-object elements pass through",
			msg:  `{"role":"developer","content":"<ide_selection>x</ide_selection>"}`,
			want: []struct {
				el    string
				class string
			}{
				{`{"role":"developer","content":"<ide_selection>x</ide_selection>"}`, EventUserInput},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			els := SplitUserMessageForAdded(parseJSON(t, tc.msg))
			if len(els) != len(tc.want) {
				t.Fatalf("split into %d elements, want %d: %+v", len(els), len(tc.want), els)
			}
			for i, w := range tc.want {
				if !reflect.DeepEqual(els[i], parseJSON(t, w.el)) {
					t.Fatalf("element %d = %+v, want %+v", i, els[i], parseJSON(t, w.el))
				}
				if got := ClassifyAddedMessage(els[i]); got != w.class {
					t.Fatalf("element %d class = %q, want %q", i, got, w.class)
				}
			}
		})
	}

	// The unsplit shapes must return the ORIGINAL object, not a rebuilt
	// twin — content stays byte-identical for everything the marker table
	// does not name.
	orig := parseJSON(t, `{"role":"user","content":[{"type":"text","text":"plain"}]}`)
	els := SplitUserMessageForAdded(orig)
	if len(els) != 1 || !reflect.DeepEqual(els[0], orig) {
		t.Fatalf("unsplit message = %+v, want the original object untouched", els)
	}
}

// TestUserInputRealText pins the fold's comparison key: string content
// verbatim, block arrays reduced to their text blocks — and only the KEPT
// blocks once the splitter has run, which is what "the same input" means
// when a client resends the same words with fresh machine context.
func TestUserInputRealText(t *testing.T) {
	if got := UserInputRealText(parseJSON(t, `{"role":"user","content":"X"}`)); got != "X" {
		t.Fatalf("string content real text = %q, want X", got)
	}
	if got := UserInputRealText(parseJSON(t, `{"role":"user","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}`)); got != "a\nb" {
		t.Fatalf("blocks real text = %q, want a\\nb", got)
	}
	if got := UserInputRealText(parseJSON(t, `{"role":"user","content":null}`)); got != "" {
		t.Fatalf("null content real text = %q, want empty", got)
	}
	if got := UserInputRealText("stray"); got != "" {
		t.Fatalf("non-object real text = %q, want empty", got)
	}
}

// TestDeriveTurnEventsUserInputDedup pins the duplicate-input fold: a
// user_input whose real text equals the session's last KEPT user_input is
// the resent copy an interrupted client produced and lands no event; a
// different text — or a repeat that is not adjacent — keeps its event.
// Injected and tool_result elements never fold, identical or not; and an
// EMPTY real text never folds either way — "" is every textless message's
// comparison key (two different pure-image inputs share it without being
// the same words), so an empty key is never same-text evidence.
func TestDeriveTurnEventsUserInputDedup(t *testing.T) {
	cases := []struct {
		name     string
		userBase UserInputBaseline
		ownBody  string
		// wantUserTexts is the real-text sequence of the user_input events
		// the derivation emits (assistant rows excluded).
		wantUserTexts []string
	}{
		{
			name:     "resent duplicate of the previous input folds",
			userBase: UserInputBaseline{Text: "X", Has: true},
			ownBody:  `{"messages":[{"role":"user","content":[{"type":"text","text":"<ide_opened_file>fresh context</ide_opened_file>"},{"type":"text","text":"X"}]}]}`,
			// The injected block still lands; only the duplicate text folds.
			wantUserTexts: nil,
		},
		{
			name:          "different text keeps its event",
			userBase:      UserInputBaseline{Text: "X", Has: true},
			ownBody:       `{"messages":[{"role":"user","content":"Y"}]}`,
			wantUserTexts: []string{"Y"},
		},
		{
			name:          "first input of a session always keeps",
			userBase:      UserInputBaseline{},
			ownBody:       `{"messages":[{"role":"user","content":"X"}]}`,
			wantUserTexts: []string{"X"},
		},
		{
			name:          "adjacent same-text pair inside one request folds the second",
			ownBody:       `{"messages":[{"role":"user","content":"X"},{"role":"user","content":"X"}]}`,
			wantUserTexts: []string{"X"},
		},
		{
			name:          "non-adjacent repeats all keep (X → Y → X)",
			ownBody:       `{"messages":[{"role":"user","content":"X"},{"role":"user","content":"Y"},{"role":"user","content":"X"}]}`,
			wantUserTexts: []string{"X", "Y", "X"},
		},
		{
			name:          "the fold state advances on the kept input (Y then resent Y)",
			userBase:      UserInputBaseline{Text: "X", Has: true},
			ownBody:       `{"messages":[{"role":"user","content":"Y"},{"role":"user","content":"Y"}]}`,
			wantUserTexts: []string{"Y"},
		},
		{
			name:          "identical injected messages never fold",
			ownBody:       `{"messages":[{"role":"user","content":"<system-reminder>same</system-reminder>"},{"role":"user","content":"<system-reminder>same</system-reminder>"}]}`,
			wantUserTexts: nil,
		},
		{
			// Two DIFFERENT pure-image messages extract the same "" key
			// without being the same input: the second must land, not
			// collapse as a phantom resent copy.
			name:          "two different pure-image inputs never fold (shared empty key)",
			ownBody:       `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUFB"}}]},{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QkJC"}}]}]}`,
			wantUserTexts: []string{"", ""},
		},
		{
			// The stored-row baseline after a kept pure image is
			// Text ""/Has true (fetchLastUserInput reads the row's real
			// text verbatim); that empty baseline must not eat the next
			// pure-image message.
			name:          "empty-key baseline never folds a pure-image input",
			userBase:      UserInputBaseline{Text: "", Has: true},
			ownBody:       `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QkJC"}}]}]}`,
			wantUserTexts: []string{""},
		},
		{
			// An empty-text keep still ADVANCES the state (to ""), keeping
			// the derivation's fold in lockstep with the row the pass-side
			// folder and fetchLastUserInput would read; the fold re-arms on
			// the next non-empty input — Y after an image folds only
			// against a kept Y, never against the image's "" key.
			name:          "empty-text keep advances the state and the fold re-arms on words",
			ownBody:       `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUFB"}}]},{"role":"user","content":"Y"},{"role":"user","content":"Y"}]}`,
			wantUserTexts: []string{"", "Y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := DeriveTurnEvents(OwnBody{RequestBody: tc.ownBody}, nil, SystemBaseline{}, tc.userBase)
			var gotTexts []string
			var injected int
			for _, ev := range events {
				switch ev.EventType {
				case EventUserInput:
					gotTexts = append(gotTexts, UserInputRealText(parseJSON(t, ev.Content)))
				case EventInjected:
					injected++
				}
			}
			if !reflect.DeepEqual(gotTexts, tc.wantUserTexts) {
				t.Fatalf("kept user inputs = %q, want %q", gotTexts, tc.wantUserTexts)
			}
			// The folded cases still carry their injected halves: folding
			// drops the duplicate WORDS, never the context blocks.
			if strings.Contains(tc.ownBody, "ide_opened_file") && injected != 1 {
				t.Fatalf("injected events = %d, want 1 (the fresh context block)", injected)
			}
			if strings.Contains(tc.ownBody, "<system-reminder>same") && injected != 2 {
				t.Fatalf("identical injected messages folded: %d events, want 2", injected)
			}
		})
	}
}

// deriveCase is one derivation scenario: two adjacent bodies plus the
// baselines, with the expected events described declaratively.
type deriveCase struct {
	name     string
	ownBody  string
	prev     *PrevBody // nil = first request of the session
	baseline SystemBaseline
	// userBase is the session's duplicate-input state entering the own
	// request. The losslessness oracle below walks [prev, own] through the
	// pass starting from an EMPTY fold, so a case whose prev body keeps a
	// user input must derive the matching state here — the same-text resend
	// arm below does exactly this (prev keeps "X", so the fold enters the
	// own request carrying "X").
	userBase UserInputBaseline
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
			// The block-level split, pinned lossless against the pass: the
			// ide block and the human words are separate elements in BOTH
			// halves, in the same order.
			name:      "ide-and-text message splits identically in both halves",
			ownBody:   `{"messages":[{"role":"user","content":[{"type":"text","text":"\u003cide_selection\u003elines 1-1 of a.jsonl\u003c/ide_selection\u003e"},{"type":"text","text":"continue"}]},{"role":"assistant","content":"ok"}]}`,
			prev:      nil,
			wantTypes: []string{EventInjected, EventUserInput, EventAssistantReply},
			wantAdded: []string{
				`{"role":"user","content":[{"type":"text","text":"<ide_selection>lines 1-1 of a.jsonl</ide_selection>"}]}`,
				`{"role":"user","content":[{"type":"text","text":"continue"}]}`,
			},
		},
		{
			// The duplicate-input fold, pinned lossless against the pass the
			// same way the split arm above is: the resent copy of the
			// previous request's words rides FRESH machine context, so the
			// prefix diff keeps the whole message new and only the fold can
			// drop the duplicate words — and it must drop them in BOTH
			// halves, leaving the injected context element alone in added.
			name:      "same-text resend folds identically in both halves",
			ownBody:   `{"messages":[{"role":"user","content":"X"},{"role":"user","content":[{"type":"text","text":"<ide_opened_file>fresh context</ide_opened_file>"},{"type":"text","text":"X"}]}]}`,
			prev:      &PrevBody{RequestBody: `{"messages":[{"role":"user","content":"X"}]}`},
			userBase:  UserInputBaseline{Text: "X", Has: true},
			wantTypes: []string{EventInjected, EventAssistantReply},
			wantAdded: []string{
				`{"role":"user","content":[{"type":"text","text":"<ide_opened_file>fresh context</ide_opened_file>"}]}`,
			},
		},
		{
			// The empty-key rule, pinned lossless against the pass: the
			// predecessor kept a pure image (state advanced to ""), and the
			// own request adds a DIFFERENT pure image — structurally new
			// (the bytes differ, so the prefix diff cannot strip it), so
			// only the fold decides, and the empty key must keep it in BOTH
			// halves: two textless inputs are not a resent duplicate.
			name:      "different pure-image inputs keep identically in both halves",
			ownBody:   `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUFB"}}]},{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QkJC"}}]}]}`,
			prev:      &PrevBody{RequestBody: `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QUFB"}}]}]}`},
			userBase:  UserInputBaseline{Text: "", Has: true},
			wantTypes: []string{EventUserInput, EventAssistantReply},
			wantAdded: []string{
				`{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QkJC"}}]}`,
			},
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
			events := DeriveTurnEvents(OwnBody{RequestBody: tc.ownBody}, tc.prev, tc.baseline, tc.userBase)

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
	events := DeriveTurnEvents(OwnBody{RequestBody: body}, nil, SystemBaseline{}, UserInputBaseline{})
	sys := findSystemEvent(t, events)
	if sys == nil || sys.Ordinal != 0 || sys.Raw != "S-two" {
		t.Fatalf("first system event = %+v, want ordinal 0 with S-two", sys)
	}

	// Same value as baseline: nothing recorded.
	events = DeriveTurnEvents(OwnBody{RequestBody: body}, nil, SystemBaseline{Raw: "S-two", Has: true, NextOrdinal: 3}, UserInputBaseline{})
	if findSystemEvent(t, events) != nil {
		t.Fatalf("unchanged system value recorded a timeline entry")
	}

	// Different value: recorded at the baseline's next ordinal.
	events = DeriveTurnEvents(OwnBody{RequestBody: body}, nil, SystemBaseline{Raw: "S-one", Has: true, NextOrdinal: 3}, UserInputBaseline{})
	sys = findSystemEvent(t, events)
	if sys == nil || sys.Ordinal != 3 || sys.Raw != "S-two" {
		t.Fatalf("changed system event = %+v, want ordinal 3 with S-two", sys)
	}

	// OpenAI dialect: role=system messages are the system value.
	events = DeriveTurnEvents(OwnBody{RequestBody: `{"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"q"}]}`}, nil, SystemBaseline{}, UserInputBaseline{})
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
	events := DeriveTurnEvents(OwnBody{RequestBody: ownBody}, &PrevBody{RequestBody: prevBody, HasInlineReply: true}, SystemBaseline{}, UserInputBaseline{})
	for _, ev := range events {
		if ev.ForPrev {
			t.Fatalf("supplement written for an inline-recorded predecessor: %+v", ev)
		}
	}

	// Predecessor did not → supplement carries both copies and marks the
	// tool_use one.
	events = DeriveTurnEvents(OwnBody{RequestBody: ownBody}, &PrevBody{RequestBody: prevBody}, SystemBaseline{}, UserInputBaseline{})
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
	events = DeriveTurnEvents(OwnBody{RequestBody: ownBody}, nil, SystemBaseline{}, UserInputBaseline{})
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
			events := DeriveTurnEvents(tc.own, nil, SystemBaseline{}, UserInputBaseline{})
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
