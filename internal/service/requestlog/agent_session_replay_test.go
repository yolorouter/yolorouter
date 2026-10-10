// Table-driven tests for the replay assembly pure function
// (agent_session_replay.go): prefix dedup, the added-role filter, the
// four-way assistant source priority, the deduplicated system timeline
// across both dialects, and the degradation rules (pure retry, unparseable
// body, capped body). No storage is involved — every case is an input
// slice plus expected output, so the assembly is pinned at its exact
// input/output contract.
package requestlog

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// replayMsg builds one message object as raw JSON so every case exercises
// the real parse path rather than hand-built map values.
func replayMsg(role, text string) string {
	return fmt.Sprintf(`{"role":%q,"content":%q}`, role, text)
}

// replayBody wraps message JSON objects into a request body object.
func replayBody(msgs ...string) string {
	return `{"model":"test-model","messages":[` + strings.Join(msgs, ",") + `]}`
}

// replayParse parses a JSON literal into the generic value the assembly
// compares, for asserting against Added / Assistant.Raw / System.Raw.
func replayParse(t *testing.T, raw string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatalf("replayParse(%q): %v", raw, err)
	}
	return v
}

// replayParsedList parses a comma-separated list of JSON objects into the
// generic slice shape Added and resend Raw use.
func replayParsedList(t *testing.T, raws ...string) []any {
	t.Helper()
	out := make([]any, 0, len(raws))
	for _, raw := range raws {
		out = append(out, replayParse(t, raw))
	}
	return out
}

// replayInput is the minimal carrier for one test request; cases override
// fields on top of it.
func replayInput(requestID, requestBody string) AgentSessionReplayInput {
	return AgentSessionReplayInput{
		RequestID:   requestID,
		CreatedAt:   time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
		RequestBody: requestBody,
	}
}

// TestAssembleReplayPrefixDedup pins step 2 of the assembly: each block's
// new input is exactly the tail suffix left after the longest structurally
// equal prefix shared with the previous request's messages is stripped,
// and the first request (no predecessor) keeps everything. The third
// request breaks the common prefix midway, so its suffix carries both the
// differing element and everything after it.
func TestAssembleReplayPrefixDedup(t *testing.T) {
	uHello := replayMsg("user", "hello")
	uWorld := replayMsg("user", "world")
	uChanged := replayMsg("user", "changed my mind")
	uTail := replayMsg("user", "tail")
	inputs := []AgentSessionReplayInput{
		replayInput("r1", replayBody(uHello)),
		replayInput("r2", replayBody(uHello, uWorld)),
		// Common prefix with r2 is [hello] only — the second element
		// differs, so the suffix keeps BOTH trailing messages.
		replayInput("r3", replayBody(uHello, uChanged, uTail)),
	}

	blocks, _ := AssembleAgentSessionReplay(inputs)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	want := [][]any{
		replayParsedList(t, uHello),          // first request: suffix = all
		replayParsedList(t, uWorld),          // prefix [hello] stripped
		replayParsedList(t, uChanged, uTail), // prefix stops at first difference
	}
	for i, w := range want {
		if !reflect.DeepEqual(blocks[i].Added, w) {
			t.Errorf("block %d Added = %v, want %v", i, blocks[i].Added, w)
		}
	}
}

// TestAssembleReplayAddedRoleFilter pins the added half of step 3: only
// messages whose role is neither assistant nor system count as new input.
// A user message carrying a tool_result block survives (it IS new input);
// the assistant copy is excluded — it belongs to the reply channel (here
// promoted to the previous turn's resend since nothing was recorded) — and
// the role=system message is excluded because system prompts have their
// own timeline.
func TestAssembleReplayAddedRoleFilter(t *testing.T) {
	uQ1 := replayMsg("user", "run the tests")
	assistantCopy := replayMsg("assistant", "they pass")
	uPlain := replayMsg("user", "good, deploy")
	uToolResult := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"exit 0"}]}`
	sysMsg := replayMsg("system", "always answer in verse")
	inputs := []AgentSessionReplayInput{
		replayInput("r1", replayBody(uQ1)),
		replayInput("r2", replayBody(uQ1, assistantCopy, uPlain, uToolResult, sysMsg)),
	}

	blocks, system := AssembleAgentSessionReplay(inputs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	want := replayParsedList(t, uPlain, uToolResult)
	if !reflect.DeepEqual(blocks[1].Added, want) {
		t.Errorf("r2 Added = %v, want only the plain user message and the tool_result carrier %v", blocks[1].Added, want)
	}
	// The excluded assistant copy is promoted to r1's reply (resend), not
	// silently lost — the exact-once guarantee.
	if blocks[0].Assistant.Source != ReplaySourceResend {
		t.Errorf("r1 assistant source = %q, want %q", blocks[0].Assistant.Source, ReplaySourceResend)
	}
	// The excluded system message lands on the timeline instead.
	if len(system) != 1 || system[0].SeenAtRequest != "r2" {
		t.Errorf("system timeline = %+v, want exactly one record seen at r2", system)
	}
}

// TestAssembleReplayAssistantSourcePriority pins the four mutually
// exclusive sources of a block's assistant reply and their priority: a
// recorded inline response body wins (and the next request's resent copy
// is dropped so the reply appears exactly once); without inline, the SSE
// capture carries the turn; without either, the next request's resent
// assistant copies are promoted to resend; with nothing anywhere, none.
func TestAssembleReplayAssistantSourcePriority(t *testing.T) {
	uQ1 := replayMsg("user", "question one")
	assistantCopy := replayMsg("assistant", "the answer")
	uQ2 := replayMsg("user", "question two")
	inlineBody := `{"id":"msg_1","content":[{"type":"text","text":"the answer"}]}`
	streamCapture := "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"the \"}}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"answer\"}}\n\n"

	cases := []struct {
		name        string
		inputs      []AgentSessionReplayInput
		wantSource  string
		wantTrunc   bool
		wantRaw     any
		wantNextAdd []any
	}{
		{
			name: "inline response beats the resent copy",
			inputs: []AgentSessionReplayInput{
				{RequestID: "r1", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), RequestBody: replayBody(uQ1), ResponseBody: inlineBody},
				{RequestID: "r2", CreatedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC), RequestBody: replayBody(uQ1, assistantCopy, uQ2)},
			},
			wantSource:  ReplaySourceInline,
			wantRaw:     inlineBody,
			wantNextAdd: replayParsedList(t, uQ2),
		},
		{
			name: "stream capture when no inline response",
			inputs: []AgentSessionReplayInput{
				{RequestID: "r1", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), RequestBody: replayBody(uQ1), StreamBody: streamCapture},
				{RequestID: "r2", CreatedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC), RequestBody: replayBody(uQ1, assistantCopy, uQ2)},
			},
			wantSource:  ReplaySourceStream,
			wantRaw:     streamCapture,
			wantNextAdd: replayParsedList(t, uQ2),
		},
		{
			name: "resent copies when nothing was recorded",
			inputs: []AgentSessionReplayInput{
				{RequestID: "r1", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), RequestBody: replayBody(uQ1)},
				{RequestID: "r2", CreatedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC), RequestBody: replayBody(uQ1, assistantCopy, uQ2)},
			},
			wantSource:  ReplaySourceResend,
			wantRaw:     replayParsedList(t, assistantCopy),
			wantNextAdd: replayParsedList(t, uQ2),
		},
		{
			name: "none when nothing was recorded and nothing is resent",
			inputs: []AgentSessionReplayInput{
				{RequestID: "r1", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), RequestBody: replayBody(uQ1)},
			},
			wantSource: ReplaySourceNone,
			wantRaw:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks, _ := AssembleAgentSessionReplay(tc.inputs)
			if len(blocks) == 0 {
				t.Fatalf("blocks is empty")
			}
			got := blocks[0].Assistant
			if got.Source != tc.wantSource {
				t.Errorf("assistant source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Truncated != tc.wantTrunc {
				t.Errorf("assistant truncated = %v, want %v", got.Truncated, tc.wantTrunc)
			}
			if !reflect.DeepEqual(got.Raw, tc.wantRaw) {
				t.Errorf("assistant raw = %#v, want %#v", got.Raw, tc.wantRaw)
			}
			if tc.wantNextAdd != nil && len(blocks) > 1 {
				if !reflect.DeepEqual(blocks[1].Added, tc.wantNextAdd) {
					t.Errorf("next block Added = %v, want %v (the resent copy must not repeat)", blocks[1].Added, tc.wantNextAdd)
				}
			}
		})
	}
}

// TestAssembleReplayResendGivesAllCopiesInOrder pins the multi-copy resend
// case: when the next request's new suffix carries more than one resent
// assistant message (several turns piled up while recording was down),
// resend yields ALL of them in their original order, and the caller-side
// messages interleaved between them still land on the next block's added.
func TestAssembleReplayResendGivesAllCopiesInOrder(t *testing.T) {
	uQ1 := replayMsg("user", "first question")
	a1 := replayMsg("assistant", "first answer")
	uMid := replayMsg("user", "interlude")
	a2 := `{"role":"assistant","content":[{"type":"text","text":"second answer"}],"model":"test-model"}`
	uEnd := replayMsg("user", "closing question")
	inputs := []AgentSessionReplayInput{
		replayInput("r1", replayBody(uQ1)),
		replayInput("r2", replayBody(uQ1, a1, uMid, a2, uEnd)),
	}

	blocks, _ := AssembleAgentSessionReplay(inputs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	got := blocks[0].Assistant
	if got.Source != ReplaySourceResend {
		t.Fatalf("r1 assistant source = %q, want %q", got.Source, ReplaySourceResend)
	}
	wantCopies := replayParsedList(t, a1, a2)
	if !reflect.DeepEqual(got.Raw, wantCopies) {
		t.Errorf("resend raw = %v, want both copies in order %v", got.Raw, wantCopies)
	}
	wantAdded := replayParsedList(t, uMid, uEnd)
	if !reflect.DeepEqual(blocks[1].Added, wantAdded) {
		t.Errorf("r2 Added = %v, want %v", blocks[1].Added, wantAdded)
	}
}

// TestAssembleReplaySystemTimeline pins the system channel across both
// dialect shapes: the first request's system value is always recorded,
// repeats are skipped, a changed value appends a record on the request
// that introduced it, a pure form switch (top-level string to block array
// of the same text) counts as a change, and the OpenAI dialect's
// role=system messages are the value with the same dedup semantics.
func TestAssembleReplaySystemTimeline(t *testing.T) {
	uQ1 := replayMsg("user", "hello")
	sysV1 := "You are helpful."
	sysV2 := "You are terse."
	sysV2Blocks := `[{"type":"text","text":"You are terse."}]`
	openAISys := replayMsg("system", "Answer in JSON.")

	anthropic := func(sys string, msgs ...string) string {
		return `{"system":` + sys + `,"messages":[` + strings.Join(msgs, ",") + `]}`
	}
	inputs := []AgentSessionReplayInput{
		replayInput("r1", anthropic(`"`+sysV1+`"`, uQ1)),                                                 // first: always recorded
		replayInput("r2", anthropic(`"`+sysV1+`"`, uQ1, replayMsg("user", "2"))),                         // same value: skipped
		replayInput("r3", anthropic(`"`+sysV2+`"`, uQ1, replayMsg("user", "2"), replayMsg("user", "3"))), // changed: appended
		// Same text as r3 but in block form: a form switch IS a change.
		replayInput("r4", anthropic(sysV2Blocks, uQ1, replayMsg("user", "2"), replayMsg("user", "3"), replayMsg("user", "4"))),
		// OpenAI dialect: system travels inside messages as role=system.
		replayInput("r5", replayBody(openAISys, uQ1)),
		// Same OpenAI value again: skipped.
		replayInput("r6", replayBody(openAISys, uQ1, replayMsg("user", "later"))),
	}

	blocks, system := AssembleAgentSessionReplay(inputs)
	want := []AgentSessionReplaySystem{
		{SeenAtRequest: "r1", Truncated: false, Raw: replayParse(t, `"`+sysV1+`"`)},
		{SeenAtRequest: "r3", Truncated: false, Raw: replayParse(t, `"`+sysV2+`"`)},
		{SeenAtRequest: "r4", Truncated: false, Raw: replayParse(t, sysV2Blocks)},
		{SeenAtRequest: "r5", Truncated: false, Raw: replayParse(t, `[`+openAISys+`]`)},
	}
	if !reflect.DeepEqual(system, want) {
		t.Fatalf("system timeline = %+v, want %+v", system, want)
	}
	// The OpenAI system message never leaks into added (r5's suffix is
	// [system, user] against r4's Anthropic messages — both are new, only
	// the user half belongs to the conversation flow).
	if wantAdded := replayParsedList(t, uQ1); !reflect.DeepEqual(blocks[4].Added, wantAdded) {
		t.Errorf("r5 Added = %v, want %v (role=system stays on the timeline channel)", blocks[4].Added, wantAdded)
	}
}

// TestAssembleReplayPureRetry pins the pure-retry shape: a request whose
// messages are structurally identical to the previous request's (the
// client retried) contributes no new input — an empty added array, not
// null — and its assistant half is its OWN response, an error body shipped
// as-is so the failed turn stays visible in the replay.
func TestAssembleReplayPureRetry(t *testing.T) {
	uQ1 := replayMsg("user", "flaky question")
	errBody := `{"error":{"message":"upstream exploded","type":"server_error"}}`
	inputs := []AgentSessionReplayInput{
		{RequestID: "r1", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), RequestBody: replayBody(uQ1), ResponseBody: `{"content":[{"type":"text","text":"first try"}]}`},
		{RequestID: "r2", CreatedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC), RequestBody: replayBody(uQ1), ResponseBody: errBody},
	}

	blocks, _ := AssembleAgentSessionReplay(inputs)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(blocks))
	}
	retry := blocks[1]
	if retry.Added == nil || len(retry.Added) != 0 {
		t.Errorf("pure retry Added = %#v, want non-nil empty array", retry.Added)
	}
	if retry.Assistant.Source != ReplaySourceInline {
		t.Errorf("pure retry assistant source = %q, want %q", retry.Assistant.Source, ReplaySourceInline)
	}
	if retry.Assistant.Raw != errBody {
		t.Errorf("pure retry assistant raw = %v, want the error body verbatim %q", retry.Assistant.Raw, errBody)
	}
}

// TestAssembleReplayUnparseableBodyDegrades pins the parse-failure
// degradation: a request body that is garbage JSON, empty, or not a JSON
// object at all yields a block with an empty added array while the row
// metadata still ships — and the rest of the session assembles normally.
// The healthy request AFTER a degraded one keeps all of its messages,
// because an unparseable predecessor leaves no prefix to strip.
func TestAssembleReplayUnparseableBodyDegrades(t *testing.T) {
	uQ1 := replayMsg("user", "healthy request")
	healthyAt := time.Date(2026, 10, 9, 12, 3, 0, 0, time.UTC)
	inputs := []AgentSessionReplayInput{
		{RequestID: "r-bad", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), RequestBody: "not json at all {", ModelName: "test-model", StatusClass: "success"},
		{RequestID: "r-empty", CreatedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC), RequestBody: "", ModelName: "test-model", StatusClass: "failed"},
		{RequestID: "r-nonobject", CreatedAt: time.Date(2026, 10, 9, 12, 2, 0, 0, time.UTC), RequestBody: "[1,2,3]", ModelName: "test-model", StatusClass: "success"},
		{RequestID: "r-good", CreatedAt: healthyAt, RequestBody: replayBody(uQ1), ModelName: "test-model", StatusClass: "success"},
	}

	blocks, _ := AssembleAgentSessionReplay(inputs)
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4 (a bad row must not take the session down)", len(blocks))
	}
	degraded := []struct {
		id          string
		statusClass string
	}{{"r-bad", "success"}, {"r-empty", "failed"}, {"r-nonobject", "success"}}
	for i, want := range degraded {
		b := blocks[i]
		if b.RequestID != want.id {
			t.Errorf("block %d request_id = %q, want %q (metadata survives the bad body)", i, b.RequestID, want.id)
		}
		if b.ModelName != "test-model" || b.StatusClass != want.statusClass {
			t.Errorf("block %s metadata degraded: model=%q status_class=%q, want (test-model, %q)", want.id, b.ModelName, b.StatusClass, want.statusClass)
		}
		if b.Added == nil || len(b.Added) != 0 {
			t.Errorf("block %s Added = %#v, want non-nil empty array", want.id, b.Added)
		}
	}
	good := blocks[3]
	if !good.CreatedAt.Equal(healthyAt) {
		t.Errorf("r-good created_at = %v, want %v", good.CreatedAt, healthyAt)
	}
	if want := replayParsedList(t, uQ1); !reflect.DeepEqual(good.Added, want) {
		t.Errorf("r-good Added = %v, want %v (the session recovers after the bad rows)", good.Added, want)
	}
}

// TestAssembleReplayTruncationMarkers pins the truncation flags: a request
// body cut at the inline cap sets added_truncated and degrades message
// parsing to an empty array, while the response-body and stream-capture
// truncation flags pass through onto the assistant half untouched.
func TestAssembleReplayTruncationMarkers(t *testing.T) {
	uQ1 := replayMsg("user", "huge question")
	stream := "data: {\"delta\":\"chunk\"}\n\n"
	inputs := []AgentSessionReplayInput{
		// Body cut at the cap: the text is not the full original, so the
		// messages half degrades to empty and the flag says so.
		{
			RequestID: "r-req-trunc", CreatedAt: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC),
			RequestBody: `{"messages":[{"role":"user","content":"aaaa`, RequestBodyTruncated: true,
		},
		// Inline response present but capped: source inline, flag passes through.
		{
			RequestID: "r-resp-trunc", CreatedAt: time.Date(2026, 10, 9, 12, 1, 0, 0, time.UTC),
			RequestBody: replayBody(uQ1), ResponseBody: `{"content":[{"type":"text","text":"truncated ans`, ResponseBodyTruncated: true,
		},
		// Stream capture only and capped: source stream, flag passes through.
		{
			RequestID: "r-stream-trunc", CreatedAt: time.Date(2026, 10, 9, 12, 2, 0, 0, time.UTC),
			RequestBody: replayBody(replayMsg("user", "another")), StreamBody: stream, StreamBodyTruncated: true,
		},
	}

	blocks, _ := AssembleAgentSessionReplay(inputs)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3", len(blocks))
	}
	reqTrunc := blocks[0]
	if !reqTrunc.AddedTruncated {
		t.Errorf("capped request body: added_truncated = false, want true")
	}
	if reqTrunc.Added == nil || len(reqTrunc.Added) != 0 {
		t.Errorf("capped request body: Added = %#v, want non-nil empty array", reqTrunc.Added)
	}
	respTrunc := blocks[1]
	if respTrunc.Assistant.Source != ReplaySourceInline || !respTrunc.Assistant.Truncated {
		t.Errorf("capped response body: assistant = %+v, want source inline with truncated=true", respTrunc.Assistant)
	}
	streamTrunc := blocks[2]
	if streamTrunc.Assistant.Source != ReplaySourceStream || !streamTrunc.Assistant.Truncated {
		t.Errorf("capped stream capture: assistant = %+v, want source stream with truncated=true", streamTrunc.Assistant)
	}
}

// TestAssembleReplayEmptySession pins the empty-input contract: no requests
// yields empty (never nil) slices, so the endpoint serializes [] rather
// than null for both halves.
func TestAssembleReplayEmptySession(t *testing.T) {
	blocks, system := AssembleAgentSessionReplay(nil)
	if blocks == nil || len(blocks) != 0 {
		t.Errorf("blocks = %#v, want non-nil empty", blocks)
	}
	if system == nil || len(system) != 0 {
		t.Errorf("system = %#v, want non-nil empty", system)
	}
}
