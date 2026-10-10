// The write-time turn derivation: the incremental mirror of
// AssembleAgentSessionReplay. The pass computes a whole session at once;
// this derivation computes ONE request's events from exactly what the
// request-log write path holds at that moment — the request's own body plus
// its predecessor's body (one SELECT + parse) and the session's system
// baseline. The parity tests pin the two halves to the same output: a
// session replayed request-by-request through DeriveTurnEvents reads back
// as the same blocks the whole-session pass produces.
//
// Where the pass's resend fallback reaches FORWARD (block i's reply comes
// from request i+1's resent copies), the derivation inverts the direction:
// request i+1's write appends a supplemental assistant_reply row to request
// i (and tool_call rows for any tool_use the copies carry). The supplement
// is written only when the predecessor recorded no inline response body —
// the pass's inline > stream > resend priority, with the stream half
// decided at read time (a stream capture whose file is unreadable reads as
// empty and falls through to the supplement, exactly as the pass did).
package agentturns

import (
	"encoding/json"
	"reflect"
)

// Seq ranges within one request. The request's own batch is assigned
// 0..n-1 by this derivation; the successor's supplement appends from
// SupplementSeqBase; the degraded pending marker sits at PendingSeq. The
// ranges are disjoint so a commit-order race between a request's own batch
// and its successor's supplement can never interleave the two, and the
// backfill's "does this request have its own batch" guard can name the
// boundary by number.
const (
	SupplementSeqBase int64 = 1024
	PendingSeq        int64 = 2048
)

// OwnBody is the request being written: its captured request body and the
// two reply-capture presences (the body row's response_body and
// stream_body_path being non-empty). The presence flags — not the texts —
// are all the derivation needs: the reply TEXT for inline/stream sources is
// read from the body rows and capture files per page at view time, never
// copied into the turn store.
type OwnBody struct {
	RequestBody      string
	HasInlineReply   bool
	HasStreamCapture bool
}

// PrevBody is the session's chronologically previous request, as the write
// path reads it: its captured body (the prefix-diff base) and whether its
// body row recorded an inline response. Nil means the request is the
// session's first — the diff base is then empty and no supplement applies.
type PrevBody struct {
	RequestBody    string
	HasInlineReply bool
}

// SystemBaseline is the session's recorded system timeline state as of the
// previous request: the last recorded value (nil when none was ever
// recorded) and the ordinal the next change would take. The engine resolves
// it from the session's last system_prompt row; because a value is recorded
// only when it differs from the last recorded one, that row's value IS the
// current baseline by construction.
type SystemBaseline struct {
	Raw         any
	Has         bool
	NextOrdinal int64
}

// TurnEvent is one derived event row before it meets the database: the
// per-request ordinal, the event type, and the content payload (with the
// system row's serialized-size sidecar). ForPrev marks the resend
// supplement rows — they carry the PREDECESSOR's request identity when the
// engine materializes them.
type TurnEvent struct {
	Seq          int64
	EventType    string
	Content      string
	ContentBytes int64
	ForPrev      bool
}

// systemEventContent is the JSON envelope of a system_prompt row: the
// change ordinal (0-based, the entry's index in the session's system
// timeline) and the raw value in the exact shape the pass recorded it (a
// string, block array, or role=system message objects by dialect).
type systemEventContent struct {
	Ordinal int64 `json:"ordinal"`
	Raw     any   `json:"raw"`
}

// assistantEventContent is the JSON envelope of an assistant_reply row.
// Source is the attribution the derivation knew at write time — "resend"
// with the resent copies in Raw for supplement rows, or the request's own
// best guess (inline/stream by capture presence, none otherwise). The read
// path re-derives inline/stream/none LIVE from the body row (a capture can
// land after the turn batch), so the stored source for non-resend rows is
// self-description, not authority.
type assistantEventContent struct {
	Source string `json:"source"`
	// Raw carries the resent assistant message objects (in order) on
	// resend rows; absent otherwise.
	Raw []any `json:"raw,omitempty"`
}

// DeriveTurnEvents derives one request's turn events: the system-prompt
// change (when this request's system value differs from the session's
// baseline), one classified event per new caller-side message, the
// assistant_reply row, and — when this request's new suffix resends
// assistant copies the predecessor never got recorded — the supplement
// rows for that predecessor. Pure: no clock, no storage, no error surface;
// every degraded input degrades the same way the whole-session pass
// degrades it.
func DeriveTurnEvents(own OwnBody, prev *PrevBody, baseline SystemBaseline) []TurnEvent {
	ownTruncated := len(own.RequestBody) > MaxInlineBodyBytes
	ownMessages, ownSystem, ownHasSystem := ParseReplayBody(own.RequestBody, ownTruncated)

	var prevMessages []any
	if prev != nil {
		prevTruncated := len(prev.RequestBody) > MaxInlineBodyBytes
		// The truncated flag mirrors the read paths' degradation: a capped
		// body parses to nothing, so the diff base empties and the whole
		// own array reads as new — the same whole-history fallback the pass
		// produced for an unreadable predecessor body.
		prevMessages, _, _ = ParseReplayBody(prev.RequestBody, prevTruncated)
	}

	suffix := ownMessages[CommonStructPrefixLen(prevMessages, ownMessages):]

	events := make([]TurnEvent, 0, len(suffix)+2)
	var seq int64

	// The system timeline: a value is recorded only when it differs from
	// the last recorded one; requests without a system value do not
	// participate at all (the pass's rule, kept verbatim).
	if ownHasSystem && (!baseline.Has || !reflect.DeepEqual(baseline.Raw, ownSystem)) {
		events = append(events, TurnEvent{
			Seq:          seq,
			EventType:    EventSystemPrompt,
			Content:      mustMarshalJSON(systemEventContent{Ordinal: baseline.NextOrdinal, Raw: ownSystem}),
			ContentBytes: int64(systemRawSerializedSize(ownSystem)),
		})
		seq++
	}

	// The new caller-side messages, classified. The pass kept every
	// non-assistant, non-system suffix element in `added`; the same
	// elements land here in the same order, so concatenating the
	// user_input/tool_result/injected rows' contents reproduces `added`.
	var copies []any
	for _, m := range suffix {
		role := ReplayMessageRole(m)
		if role == "assistant" {
			copies = append(copies, m)
			continue
		}
		if role == "system" {
			continue
		}
		events = append(events, TurnEvent{
			Seq:       seq,
			EventType: ClassifyAddedMessage(m),
			Content:   mustMarshalJSON(m),
		})
		seq++
	}

	// The request's own assistant_reply row. Source is the write-time
	// attribution; the read path treats inline/stream/none as live-derived
	// and only resend rows' content as load-bearing.
	source := ReplaySourceNone
	switch {
	case own.HasInlineReply:
		source = ReplaySourceInline
	case own.HasStreamCapture:
		source = ReplaySourceStream
	}
	events = append(events, TurnEvent{
		Seq:       seq,
		EventType: EventAssistantReply,
		Content:   mustMarshalJSON(assistantEventContent{Source: source}),
	})

	// The resend supplement for the predecessor: its reply was never
	// recorded inline, and this request resends the copies as history — so
	// the copies become the predecessor's reply attribution (plus
	// tool_call markers for any tool_use they carry). Guarded on the
	// inline flag alone: a predecessor whose reply lives only in a stream
	// capture still gets the supplement, because a capture that reads
	// empty at view time must fall through to resend exactly as the pass
	// let it.
	if prev != nil && !prev.HasInlineReply && len(copies) > 0 {
		supSeq := SupplementSeqBase
		events = append(events, TurnEvent{
			Seq:       supSeq,
			EventType: EventAssistantReply,
			Content:   mustMarshalJSON(assistantEventContent{Source: ReplaySourceResend, Raw: copies}),
			ForPrev:   true,
		})
		supSeq++
		for _, m := range copies {
			if MessageCarriesToolUse(m) {
				events = append(events, TurnEvent{
					Seq:       supSeq,
					EventType: EventToolCall,
					Content:   mustMarshalJSON(m),
					ForPrev:   true,
				})
				supSeq++
			}
		}
	}

	return events
}

// systemRawSerializedSize measures one system value's JSON serialization in
// bytes — the number the transcript's metadata list reports as raw_length
// (exactly the bytes the entry adds to the /transcript/system response).
// encoding/json sorts map keys, so the length is deterministic and
// identical to what the read path would re-measure from the stored content.
func systemRawSerializedSize(raw any) int {
	return len(mustMarshalJSON(raw))
}

// mustMarshalJSON serializes a parsed-JSON value. Values that reached this
// package by json.Unmarshal cannot fail to re-marshal (no channels,
// functions, or cycles anywhere in them); an empty string on the
// unreachable error arm would only ever under-report, never invent.
func mustMarshalJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(data)
}
