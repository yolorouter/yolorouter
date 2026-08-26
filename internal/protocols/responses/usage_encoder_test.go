package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// TestStreamDecoder_IncompleteCarriesUsage covers a run truncated by
// max_output_tokens: it terminates with response.incomplete rather than
// response.completed, and that event carries the full usage. Skipping it left
// the priciest requests unbilled, gave the client no token counts, and made the
// relay's hasMeaningfulUsage false (which also stops benign post-DONE read
// errors from being excused, mislabeling healthy providers as 502).
func TestStreamDecoder_IncompleteCarriesUsage(t *testing.T) {
	d := NewStreamDecoder()
	deltas, err := d.DecodeChunk(`{"type":"response.incomplete","response":{"usage":{
		"input_tokens":1000,"output_tokens":4096,"total_tokens":5096,
		"input_tokens_details":{"cached_tokens":256},
		"output_tokens_details":{"reasoning_tokens":64}}}}`)
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}

	var usage *protocols.IRUsage
	var sawDone bool
	for _, delta := range deltas {
		switch v := delta.(type) {
		case protocols.DeltaUsage:
			u := v.Usage
			usage = &u
		case protocols.DeltaDone:
			sawDone = true
		}
	}

	if usage == nil {
		t.Fatal("response.incomplete must emit DeltaUsage — a truncated run is still billable")
	}
	if usage.PromptTokens != 1000 || usage.CompletionTokens != 4096 {
		t.Errorf("usage = %+v, want prompt 1000 / completion 4096", *usage)
	}
	if usage.CacheReadTokens != 256 || usage.ReasoningTokens != 64 {
		t.Errorf("usage details lost: %+v", *usage)
	}
	if !sawDone {
		t.Error("response.incomplete must still emit DeltaDone")
	}
}

// TestStreamDecoder_UsageEmittedOnce guards against duplicate usage frames when
// a stream sends more than one terminal event carrying identical usage.
func TestStreamDecoder_UsageEmittedOnce(t *testing.T) {
	d := NewStreamDecoder()
	first, _ := d.DecodeChunk(`{"type":"response.completed","response":{"usage":{
		"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`)
	second, _ := d.DecodeChunk(`{"type":"response.done","response":{"usage":{
		"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`)

	count := 0
	for _, delta := range append(append([]protocols.IRStreamDelta{}, first...), second...) {
		if _, ok := delta.(protocols.DeltaUsage); ok {
			count++
		}
	}
	if count != 1 {
		t.Errorf("emitted %d DeltaUsage, want exactly 1", count)
	}
}

// TestStreamDecoder_LaterTerminalEventEnrichesUsage proves the collection is a
// merge rather than first-wins. response.completed may carry only the token
// counts while a later response.done fills in the cached / reasoning
// breakdown; latching onto the first frame would discard the richer data and
// under-report billing.
func TestStreamDecoder_LaterTerminalEventEnrichesUsage(t *testing.T) {
	d := NewStreamDecoder()
	if _, err := d.DecodeChunk(`{"type":"response.completed","response":{"usage":{
		"input_tokens":1000,"output_tokens":50,"total_tokens":1050}}}`); err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	out, _ := d.DecodeChunk(`{"type":"response.done","response":{"usage":{
		"input_tokens":1000,"output_tokens":50,"total_tokens":1050,
		"input_tokens_details":{"cached_tokens":256},
		"output_tokens_details":{"reasoning_tokens":32}}}}`)

	var last *protocols.IRUsage
	for _, delta := range out {
		if u, ok := delta.(protocols.DeltaUsage); ok {
			v := u.Usage
			last = &v
		}
	}
	if last == nil {
		t.Fatal("the enriching terminal event must emit an updated DeltaUsage")
	}
	if last.CacheReadTokens != 256 {
		t.Errorf("CacheReadTokens = %d, want 256 (arrived only in the second event)", last.CacheReadTokens)
	}
	if last.ReasoningTokens != 32 {
		t.Errorf("ReasoningTokens = %d, want 32 (arrived only in the second event)", last.ReasoningTokens)
	}
	// Counts from the first event must survive the merge.
	if last.PromptTokens != 1000 || last.CompletionTokens != 50 {
		t.Errorf("earlier counts lost: %+v", *last)
	}
}

// TestStreamDecoder_SingleDoneKeepsFirstStopReason covers a truncated run that
// is followed by a generic response.done. The old guard only inspected deltas
// from the current DecodeChunk call, and each SSE line is its own call, so it
// could never see across events: the client got two terminal frames and the
// "max_tokens" truncation reason was overwritten with "stop", making a cut-off
// answer look cleanly finished.
func TestStreamDecoder_SingleDoneKeepsFirstStopReason(t *testing.T) {
	d := NewStreamDecoder()
	first, _ := d.DecodeChunk(`{"type":"response.incomplete","response":{"usage":{
		"input_tokens":10,"output_tokens":4096,"total_tokens":4106}}}`)
	second, _ := d.DecodeChunk(`{"type":"response.done","response":{"usage":{
		"input_tokens":10,"output_tokens":4096,"total_tokens":4106}}}`)

	var reasons []string
	for _, delta := range append(append([]protocols.IRStreamDelta{}, first...), second...) {
		if done, ok := delta.(protocols.DeltaDone); ok {
			reasons = append(reasons, done.StopReason)
		}
	}
	if len(reasons) != 1 {
		t.Fatalf("emitted %d DeltaDone (%v), want exactly 1", len(reasons), reasons)
	}
	// "length" is the IR vocabulary for truncation, shared with the claude and
	// gemini decoders. Leaking the wire word "max_output_tokens"/"max_tokens"
	// through would make the Chat encoder emit an illegal finish_reason and the
	// Gemini encoder fall back to "STOP".
	if reasons[0] != "length" {
		t.Errorf("stop reason = %q, want length (truncation, normalised, not overwritten by a later stop)", reasons[0])
	}
}

// TestResponseDecoder_TruncationNormalisedToLength covers the non-streaming
// path: incomplete_details.reason arrives as the wire word "max_output_tokens"
// and must be normalised to the IR value before any egress encoder sees it.
func TestResponseDecoder_TruncationNormalisedToLength(t *testing.T) {
	body := []byte(`{
		"id": "resp_1", "object": "response", "model": "gpt-5", "status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [],
		"usage": {"input_tokens": 10, "output_tokens": 4096, "total_tokens": 4106}
	}`)
	resp, err := ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.StopReason != "length" {
		t.Errorf("StopReason = %q, want length", resp.StopReason)
	}
}

// TestStreamEncoder_TruncatedStreamUsesIncompleteEvent locks the terminal SSE
// event type. The Responses API defines response.incomplete as its own event;
// flipping only the nested status while still labelling the frame
// "response.completed" lets any SDK that switches on the event discriminant
// read a max-output truncation as a clean finish.
func TestStreamEncoder_TruncatedStreamUsesIncompleteEvent(t *testing.T) {
	e := NewStreamEncoder()
	e.EncodeDeltas([]protocols.IRStreamDelta{
		protocols.DeltaMessageStart{ID: "resp_1", Model: "gpt-5"},
		protocols.DeltaText{Text: "half"},
		protocols.DeltaUsage{Usage: protocols.IRUsage{PromptTokens: 10, CompletionTokens: 4096, TotalTokens: 4106}},
		protocols.DeltaDone{StopReason: "length"},
	})
	events := e.EncodeDone()
	if len(events) == 0 {
		t.Fatal("expected a terminal event")
	}

	var got struct {
		Type     string `json:"type"`
		Response struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	last := events[len(events)-1]
	if err := json.Unmarshal([]byte(last.Data), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", last.Data, err)
	}
	if got.Type != "response.incomplete" {
		t.Errorf("event type = %q, want response.incomplete", got.Type)
	}
	if got.Response.Status != "incomplete" {
		t.Errorf("status = %q, want incomplete", got.Response.Status)
	}
	if got.Response.IncompleteDetails == nil || got.Response.IncompleteDetails.Reason != "max_output_tokens" {
		t.Errorf("incomplete_details = %+v, want reason max_output_tokens", got.Response.IncompleteDetails)
	}

	// A normal finish must still use response.completed.
	ok := NewStreamEncoder()
	ok.EncodeDeltas([]protocols.IRStreamDelta{
		protocols.DeltaMessageStart{ID: "resp_2", Model: "gpt-5"},
		protocols.DeltaDone{StopReason: "stop"},
	})
	okEvents := ok.EncodeDone()
	if len(okEvents) == 0 {
		t.Fatal("expected a terminal event")
	}
	var okGot struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(okEvents[len(okEvents)-1].Data), &okGot)
	if okGot.Type != "response.completed" {
		t.Errorf("normal finish event type = %q, want response.completed", okGot.Type)
	}
}

// TestStreamDecoder_FailedKeepsPartialUsage covers the failure path: the user
// is not charged (Finish returns an error), but provider_cost and dispatch
// analytics must still see the tokens the upstream actually consumed. No
// terminal DeltaDone may be emitted, or the client would read the failed run as
// a complete response.
func TestStreamDecoder_FailedKeepsPartialUsage(t *testing.T) {
	d := NewStreamDecoder()
	out, err := d.DecodeChunk(`{"type":"response.failed","response":{"usage":{
		"input_tokens":500,"output_tokens":20,"total_tokens":520}}}`)
	if err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}

	var usage *protocols.IRUsage
	for _, delta := range out {
		switch v := delta.(type) {
		case protocols.DeltaUsage:
			u := v.Usage
			usage = &u
		case protocols.DeltaDone:
			t.Error("response.failed must not emit DeltaDone — that is a success terminator")
		}
	}
	if usage == nil {
		t.Fatal("response.failed must still surface the usage the upstream consumed")
	}
	if usage.PromptTokens != 500 || usage.CompletionTokens != 20 {
		t.Errorf("usage = %+v, want prompt 500 / completion 20", *usage)
	}
	if _, ferr := d.Finish(); ferr == nil {
		t.Error("Finish must report the upstream failure")
	}
}

// TestResponsesWireUsage_GrossInputTokens locks the Responses API rule that
// input_tokens_details is "a detailed breakdown of the input tokens", making
// cached_tokens a strict subset of input_tokens. An Anthropic upstream reports
// a NET input count, so the cache must be added back before emitting.
func TestResponsesWireUsage_GrossInputTokens(t *testing.T) {
	usage := responsesWireUsage(protocols.IRUsage{
		PromptTokens:     0,
		CompletionTokens: 38,
		CacheWriteTokens: 344,
		CacheReadTokens:  6521,
		TotalTokens:      6903,
	})

	if got := usage["input_tokens"]; got != 6865 {
		t.Errorf("input_tokens = %v, want 6865 (0 net + 344 write + 6521 read)", got)
	}
	if got := usage["total_tokens"]; got != 6903 {
		t.Errorf("total_tokens = %v, want 6903", got)
	}

	details, ok := usage["input_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatal("input_tokens_details missing")
	}
	if details["cached_tokens"].(int) > usage["input_tokens"].(int) {
		t.Errorf("invalid usage: cached_tokens %v > input_tokens %v",
			details["cached_tokens"], usage["input_tokens"])
	}
}

// TestResponsesWireUsage_RequiredDetailsAlwaysPresent locks the schema's
// required list (input_tokens_details, output_tokens_details, and their
// cached_tokens / reasoning_tokens members): they must be emitted even when
// zero, or a strict-validating downstream rejects every cross-protocol
// response. Also covers reasoning_tokens, which the decoders collect into the
// IR but the encoder used to drop.
func TestResponsesWireUsage_RequiredDetailsAlwaysPresent(t *testing.T) {
	usage := responsesWireUsage(protocols.IRUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
	})

	in, ok := usage["input_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatal("input_tokens_details must be present even with no cache")
	}
	if _, ok := in["cached_tokens"]; !ok {
		t.Error("cached_tokens is required inside input_tokens_details")
	}
	out, ok := usage["output_tokens_details"].(map[string]interface{})
	if !ok {
		t.Fatal("output_tokens_details must be present even with no reasoning")
	}
	if _, ok := out["reasoning_tokens"]; !ok {
		t.Error("reasoning_tokens is required inside output_tokens_details")
	}

	withReasoning := responsesWireUsage(protocols.IRUsage{
		PromptTokens: 10, CompletionTokens: 90, TotalTokens: 100, ReasoningTokens: 64,
	})
	got := withReasoning["output_tokens_details"].(map[string]interface{})["reasoning_tokens"]
	if got != 64 {
		t.Errorf("reasoning_tokens = %v, want 64 (decoded into IR, must reach the wire)", got)
	}
}

// TestResponsesWireUsage_GrossUpstreamNotDoubleCounted guards the reverse
// case: an upstream already reporting gross input must pass through untouched.
func TestResponsesWireUsage_GrossUpstreamNotDoubleCounted(t *testing.T) {
	usage := responsesWireUsage(protocols.IRUsage{
		PromptTokens:          2006,
		CompletionTokens:      10,
		CacheReadTokens:       1920,
		TotalTokens:           2016,
		CacheIncludedInPrompt: true,
	})

	if got := usage["input_tokens"]; got != 2006 {
		t.Errorf("input_tokens = %v, want 2006 (already gross, must not re-add cache)", got)
	}
}

// TestStreamDecoder_ContentFilterIsNotTruncation covers the other legal value
// of incomplete_details.reason. Hardcoding "length" reported a safety block as
// a token cut-off, which is both wrong for the user and wrong for any retry
// logic that reacts to truncation by raising max_tokens.
func TestStreamDecoder_ContentFilterIsNotTruncation(t *testing.T) {
	cases := []struct {
		name  string
		frame string
		want  string
	}{
		{
			"content filter",
			`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"content_filter"},"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`,
			"content_filter",
		},
		{
			"max output tokens",
			`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":99,"total_tokens":109}}}`,
			"length",
		},
		{
			"missing details defaults to truncation",
			`{"type":"response.incomplete","response":{"usage":{"input_tokens":10,"output_tokens":99,"total_tokens":109}}}`,
			"length",
		},
		{
			"unknown reason keeps abnormal semantics",
			`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"server_error"},"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}`,
			"length",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewStreamDecoder()
			out, err := d.DecodeChunk(tc.frame)
			if err != nil {
				t.Fatalf("DecodeChunk: %v", err)
			}
			var got string
			for _, delta := range out {
				if done, ok := delta.(protocols.DeltaDone); ok {
					got = done.StopReason
				}
			}
			if got != tc.want {
				t.Errorf("stop reason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStreamEncoder_ContentFilterUsesIncompleteEvent is the reverse direction:
// a content_filter stop arriving from a Chat or Gemini upstream must terminate
// as response.incomplete, not response.completed.
func TestStreamEncoder_ContentFilterUsesIncompleteEvent(t *testing.T) {
	e := NewStreamEncoder()
	e.EncodeDeltas([]protocols.IRStreamDelta{
		protocols.DeltaMessageStart{ID: "resp_1", Model: "gpt-5"},
		protocols.DeltaDone{StopReason: "content_filter"},
	})
	events := e.EncodeDone()
	if len(events) == 0 {
		t.Fatal("expected a terminal event")
	}

	var got struct {
		Type     string `json:"type"`
		Response struct {
			Status            string `json:"status"`
			IncompleteDetails *struct {
				Reason string `json:"reason"`
			} `json:"incomplete_details"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(events[len(events)-1].Data), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "response.incomplete" {
		t.Errorf("event type = %q, want response.incomplete", got.Type)
	}
	if got.Response.IncompleteDetails == nil || got.Response.IncompleteDetails.Reason != "content_filter" {
		t.Errorf("incomplete_details = %+v, want reason content_filter", got.Response.IncompleteDetails)
	}
}

// TestResponseDecoder_FailedKeepsCacheDetails covers the non-streaming failure
// path. NewIRResponse marks usage as gross-input, so dropping cached_tokens
// made netPromptTokens treat the whole input as regular input instead of
// deducting the cache-read portion — systematically over-stating provider_cost
// and channel margin even though the user is not charged.
func TestResponseDecoder_FailedKeepsCacheDetails(t *testing.T) {
	body := []byte(`{
		"id": "resp_f", "object": "response", "model": "gpt-5", "status": "failed",
		"output": [],
		"usage": {
			"input_tokens": 10000, "output_tokens": 20, "total_tokens": 10020,
			"input_tokens_details": {"cached_tokens": 9000},
			"output_tokens_details": {"reasoning_tokens": 8}
		}
	}`)

	partial, err := ResponseDecoder{}.DecodeResponse(body)
	if err == nil {
		t.Fatal("a failed response must surface an error")
	}
	if partial == nil {
		t.Fatal("failed response must still carry partial usage for provider-cost analytics")
	}
	if partial.Usage.CacheReadTokens != 9000 {
		t.Errorf("CacheReadTokens = %d, want 9000 (dropping it over-states provider cost)", partial.Usage.CacheReadTokens)
	}
	if partial.Usage.ReasoningTokens != 8 {
		t.Errorf("ReasoningTokens = %d, want 8", partial.Usage.ReasoningTokens)
	}
	if got := partial.Usage.NetPromptTokens(); got != 1000 {
		t.Errorf("NetPromptTokens() = %d, want 1000 (10000 gross - 9000 cached)", got)
	}
}

// TestStreamEncoder_TruncatedToolCallIsSequencedAndIncomplete covers a
// truncated run that had started emitting tool-call arguments, and doubles as
// the sequence_number guard.
//
// The encoder must not declare the arguments finalised
// (response.function_call_arguments.done means "arguments finalized") nor mark
// the item completed, or the client can execute a half-written call before the
// trailing response.incomplete even arrives. Every event must also carry a
// monotonic sequence_number, which the official schema lists as required on all
// Responses stream events.
func TestStreamEncoder_TruncatedToolCallIsSequencedAndIncomplete(t *testing.T) {
	e := NewStreamEncoder()
	events := e.EncodeDeltas([]protocols.IRStreamDelta{
		protocols.DeltaMessageStart{ID: "resp_e", Model: "gpt-5"},
		protocols.DeltaToolCallStart{Index: 0, ID: "call_1", Name: "run"},
		protocols.DeltaToolCallArgs{Index: 0, Arguments: `{"cmd":"rm -r`},
		protocols.DeltaDone{StopReason: "length"},
	})
	events = append(events, e.EncodeDone()...)

	lastSeq := -1
	sawArgsDone := false
	terminalType := ""
	for _, ev := range events {
		var frame struct {
			Type   string `json:"type"`
			SeqNum *int   `json:"sequence_number"`
			Item   *struct {
				Status string `json:"status"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &frame); err != nil {
			t.Fatalf("unmarshal %q: %v", ev.Data, err)
		}
		if frame.SeqNum == nil {
			t.Errorf("event %q missing required sequence_number", frame.Type)
		} else {
			if *frame.SeqNum <= lastSeq {
				t.Errorf("sequence_number not monotonic: %d after %d", *frame.SeqNum, lastSeq)
			}
			lastSeq = *frame.SeqNum
		}
		if frame.Type == "response.function_call_arguments.done" {
			sawArgsDone = true
		}
		if frame.Type == "response.output_item.done" && frame.Item != nil && frame.Item.Status == "completed" {
			t.Error("tool item marked completed on a truncated run")
		}
		if frame.Type == "response.completed" || frame.Type == "response.incomplete" {
			terminalType = frame.Type
		}
	}

	// The .done event MUST still be emitted: consumers pair .delta with .done to
	// close a per-item argument buffer, so withholding it leaves that buffer open
	// (the client hangs on the item or drops arguments the model did emit).
	// "these arguments are not final" is expressed by item status=incomplete,
	// asserted above — that is the schema-sanctioned signal.
	if !sawArgsDone {
		t.Error("the delta/done pairing must be preserved even on a truncated run")
	}
	if terminalType != "response.incomplete" {
		t.Errorf("terminal event = %q, want response.incomplete", terminalType)
	}
}

// TestStreamDecoder_DoneStatusIsClassified covers standalone response.done,
// which the schema allows to carry any terminal status. Treating it as an
// unconditional success handed the client a success terminator for a failed or
// cancelled run, billed the usage and recorded the channel as healthy.
func TestStreamDecoder_DoneStatusIsClassified(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantDone   bool
		wantReason string
		wantErr    bool
	}{
		{
			"done without status is a normal finish",
			`{"type":"response.done","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			true, "stop", false,
		},
		{
			"done carrying failed must not emit a success terminator",
			`{"type":"response.done","response":{"status":"failed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			false, "", true,
		},
		{
			"done carrying cancelled must not emit a success terminator",
			`{"type":"response.done","response":{"status":"cancelled","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
			false, "", true,
		},
		{
			"done carrying incomplete reports truncation",
			`{"type":"response.done","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":9,"total_tokens":10}}}`,
			true, "length", false,
		},
		{
			"done carrying an error object is a failure",
			`{"type":"response.done","response":{"error":{"code":"server_error","message":"boom"}}}`,
			false, "", true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewStreamDecoder()
			out, err := d.DecodeChunk(tc.frame)
			if err != nil {
				t.Fatalf("DecodeChunk: %v", err)
			}
			gotDone, gotReason := false, ""
			for _, delta := range out {
				if done, ok := delta.(protocols.DeltaDone); ok {
					gotDone, gotReason = true, done.StopReason
				}
			}
			if gotDone != tc.wantDone {
				t.Errorf("emitted DeltaDone = %v, want %v", gotDone, tc.wantDone)
			}
			if tc.wantDone && gotReason != tc.wantReason {
				t.Errorf("stop reason = %q, want %q", gotReason, tc.wantReason)
			}
			if _, ferr := d.Finish(); (ferr != nil) != tc.wantErr {
				t.Errorf("Finish error = %v, want error = %v", ferr, tc.wantErr)
			}
		})
	}
}

// TestNormalizeResponsesIncompleteReason locks the deliberate exception to the
// project-wide "unknown values pass through" rule.
//
// response.incomplete has already declared the run unfinished; only the
// sub-reason is unknown. Forwarding that sub-reason verbatim made
// IRStopReasonIsAbnormal (which knows only length / content_filter) treat a
// server_error incomplete as a NORMAL stop — the Gemini egress then encoded it
// as STOP and the Chat egress let a tool-call inference overwrite it with
// "tool_calls", presenting arguments from an unfinished run as executable.
// Mislabelling the cause is a log-accuracy problem; losing abnormality is a
// correctness one.
func TestNormalizeResponsesIncompleteReason(t *testing.T) {
	cases := map[string]string{
		"max_output_tokens": "length",
		"max_tokens":        "length",
		"content_filter":    "content_filter",
		"CONTENT_FILTER":    "content_filter",
		"":                  "length",
		// Unknown / future reasons keep abnormal semantics rather than passing through.
		"server_error": "length",
		"whatever_new": "length",
	}
	for wire, want := range cases {
		if got := normalizeResponsesIncompleteReason(wire); got != want {
			t.Errorf("normalizeResponsesIncompleteReason(%q) = %q, want %q", wire, got, want)
		}
	}

	// Whatever it maps to must be classified abnormal, or the tool-call
	// inference in the egress encoders can overwrite it.
	for _, wire := range []string{"max_output_tokens", "content_filter", "server_error", ""} {
		if !protocols.IRStopReasonIsAbnormal(normalizeResponsesIncompleteReason(wire)) {
			t.Errorf("incomplete reason %q must stay abnormal after normalisation", wire)
		}
	}
}

// TestResponseDecoder_OnlyExplicitNonServedStatusIsRejected locks the
// deliberately ASYMMETRIC policy on status handling.
//
// On the same-protocol passthrough path the upstream's 200 and full body are
// written to the client BEFORE this decoder runs, so a false rejection means the
// client holds a perfect answer while we bill nothing and circuit-break a
// healthy provider. A false accept merely bills one anomalous response. Hence:
// reject only EXPLICIT non-served states; absent / null / empty / unknown are
// all served. (`status` is not in the schema's required list, and a Go compat
// relay serialising `Status string` without omitempty emits `"status": ""`.)
func TestResponseDecoder_OnlyExplicitNonServedStatusIsRejected(t *testing.T) {
	cases := []struct {
		name      string
		statusRaw string
		wantErr   bool
	}{
		{"field absent", ``, false},
		{"explicit null behaves as absent", `"status": null,`, false},
		{"explicit empty string is served", `"status": "",`, false},
		{"unknown future value is served", `"status": "some_future_state",`, false},
		{"completed", `"status": "completed",`, false},
		{"failed is rejected", `"status": "failed",`, true},
		{"cancelled is rejected", `"status": "cancelled",`, true},
		// queued / in_progress are legitimate 200 bodies for `background: true`
		// runs — the client expects a non-terminal status and polls. Rejecting
		// them turned a correct response into a 502 with no billing and a
		// circuit-breaker hit on a healthy channel.
		{"queued is served (background mode)", `"status": "queued",`, false},
		{"in_progress is served (background mode)", `"status": "in_progress",`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{
				"id": "resp_s", "object": "response", "model": "gpt-5", ` + tc.statusRaw + `
				"output": [{"type":"message","id":"m1","role":"assistant",
					"content":[{"type":"output_text","text":"hi"}]}],
				"usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
			}`)
			_, err := ResponseDecoder{}.DecodeResponse(body)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

// TestStreamEncoder_OutputItemAddedHasNoInputText locks the schema fix:
// OutputMessageContent admits only output_text | refusal, and "input_text" is a
// request-side part type. Emitting it made a strict SDK reject the response at
// the very first output item, before any usage or terminal event mattered.
func TestStreamEncoder_OutputItemAddedHasNoInputText(t *testing.T) {
	e := NewStreamEncoder()
	events := e.EncodeDeltas([]protocols.IRStreamDelta{
		protocols.DeltaMessageStart{ID: "resp_1", Model: "gpt-5"},
		protocols.DeltaText{Text: "hi"},
	})

	for _, ev := range events {
		if strings.Contains(ev.Data, `"input_text"`) {
			t.Errorf("output-side event must never contain input_text: %s", ev.Data)
		}
		var frame struct {
			Type string `json:"type"`
			Item *struct {
				Content []map[string]interface{} `json:"content"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &frame); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if frame.Type == "response.output_item.added" && frame.Item != nil {
			if len(frame.Item.Content) != 0 {
				t.Errorf("output_item.added must open with empty content, got %+v", frame.Item.Content)
			}
		}
	}
}

// TestStreamDecoder_DoneOnlyFailsOnExplicitFailure mirrors the non-streaming
// blacklist on the streaming side: only an explicit failed/cancelled fails the
// stream. Unknown or absent statuses are served, because the client has already
// received the bytes by then.
func TestStreamDecoder_DoneOnlyFailsOnExplicitFailure(t *testing.T) {
	cases := []struct {
		status   string
		wantDone bool
		wantErr  bool
	}{
		{"failed", false, true},
		{"cancelled", false, true},
		{"queued", true, false},
		{"in_progress", true, false},
		{"some_future_state", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			d := NewStreamDecoder()
			out, err := d.DecodeChunk(`{"type":"response.done","response":{"status":"` + tc.status + `","usage":{
				"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			if err != nil {
				t.Fatalf("DecodeChunk: %v", err)
			}
			gotDone := false
			for _, delta := range out {
				if _, ok := delta.(protocols.DeltaDone); ok {
					gotDone = true
				}
			}
			if gotDone != tc.wantDone {
				t.Errorf("emitted DeltaDone = %v, want %v", gotDone, tc.wantDone)
			}
			if _, ferr := d.Finish(); (ferr != nil) != tc.wantErr {
				t.Errorf("Finish error = %v, wantErr = %v", ferr, tc.wantErr)
			}
		})
	}
}

// TestStreamDecoder_TrailingDoneCannotUndoSuccess covers the retroactive-failure
// hole: a bookkeeping response.done arriving after a successful
// response.completed must never turn the stream into a failure, which would
// skip EncodeDone and leave the client with no terminator while the user goes
// unbilled and a healthy provider is circuit-broken.
func TestStreamDecoder_TrailingDoneCannotUndoSuccess(t *testing.T) {
	d := NewStreamDecoder()
	if _, err := d.DecodeChunk(`{"type":"response.completed","response":{"usage":{
		"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`); err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	if _, err := d.DecodeChunk(`{"type":"response.done","response":{"error":{"code":"x","message":"y"}}}`); err != nil {
		t.Fatalf("DecodeChunk: %v", err)
	}
	if _, ferr := d.Finish(); ferr != nil {
		t.Errorf("a trailing response.done must not retroactively fail a completed stream: %v", ferr)
	}
}

// TestStreamDecoder_TrailingFailureCannotUndoSuccess extends the retroactive-
// failure guard to the two paths it originally missed: response.failed and the
// top-level error event both used to set upstreamErr unconditionally, one case
// above and one below the branch that was guarded.
func TestStreamDecoder_TrailingFailureCannotUndoSuccess(t *testing.T) {
	for _, trailing := range []string{
		`{"type":"response.failed","response":{"error":{"code":"x","message":"y"}}}`,
		`{"type":"error","error":{"message":"late bookkeeping"}}`,
	} {
		d := NewStreamDecoder()
		if _, err := d.DecodeChunk(`{"type":"response.completed","response":{"usage":{
			"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`); err != nil {
			t.Fatalf("DecodeChunk: %v", err)
		}
		if _, err := d.DecodeChunk(trailing); err != nil {
			t.Fatalf("DecodeChunk: %v", err)
		}
		if _, ferr := d.Finish(); ferr != nil {
			t.Errorf("a trailing %.30s... must not retroactively fail a completed stream: %v", trailing, ferr)
		}
	}
}

// TestResponsesStatusIsNonServed_SharedByBothDecoders pins the predicate both
// decoders now share. They briefly disagreed — one listed queued/in_progress,
// the other did not — so the same payload settled as success on the streaming
// path and as a 502 on the non-streaming one.
func TestResponsesStatusIsNonServed_SharedByBothDecoders(t *testing.T) {
	cases := map[string]bool{
		"failed": true, "cancelled": true, "FAILED": true, "  cancelled  ": true,
		"queued": false, "in_progress": false, "completed": false,
		"incomplete": false, "": false, "some_future_state": false,
	}
	for status, want := range cases {
		if got := StatusIsNonServed(status); got != want {
			t.Errorf("StatusIsNonServed(%q) = %v, want %v", status, got, want)
		}
	}
}
