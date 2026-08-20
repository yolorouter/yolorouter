package responses

import (
	"encoding/json"
	"errors"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"strings"
)

// Sentinel error for an explicit failure declared mid-stream by the upstream Responses SSE.
// Deliberately carries no raw payload: prompt fragments, tool arguments and other sensitive
// content must not leak into application logs. The handler only needs the error to route
var (
	errResponsesStreamFailed     = errors.New("upstream responses stream failed (response.failed event)")
	errResponsesStreamErrorEvent = errors.New("upstream responses stream error event")
)

// StreamDecoder parses an OpenAI Responses API SSE event stream into IR stream deltas.
// Upstream SSE event types follow the ResponsesEvent* constants (see the OpenAI Responses
//
// A small state machine tracks the current message / reasoning / function_call item and
// emits the corresponding IR delta per event type.
// upstreamErr records an explicit upstream error (response.failed / top-level error event);
// Finish() returns it so the IR pipeline can route the in-stream failure back to the handler
type StreamDecoder struct {
	// whether message_start was already emitted
	startEmitted bool
	// call_id of the currently active function_call item (indexed by output_index)
	activeFunctionCalls map[int]functionCallState
	usage               protocols.IRUsage
	usageEmitted        bool
	doneEmitted         bool
	upstreamErr         error
}

type functionCallState struct {
	CallID string
	Name   string
}

func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{
		activeFunctionCalls: make(map[int]functionCallState),
	}
}

// responsesStreamEventWire parses the JSON of a single SSE data frame.
type responsesStreamEventWire struct {
	Type     string            `json:"type"`
	Response *responsesRespMin `json:"response,omitempty"`
	Item     *responsesItemMin `json:"item,omitempty"`
	Delta    string            `json:"delta,omitempty"`
	Text     string            `json:"text,omitempty"`
	// index fields (used by events such as function_call_arguments.delta)
	OutputIndex *int   `json:"output_index,omitempty"`
	ItemID      string `json:"item_id,omitempty"`
	CallID      string `json:"call_id,omitempty"`
	Name        string `json:"name,omitempty"`
	Arguments   string `json:"arguments,omitempty"`
}

type responsesRespMin struct {
	ID     string          `json:"id,omitempty"`
	Model  string          `json:"model,omitempty"`
	Status string          `json:"status,omitempty"`
	Usage  *responsesUsage `json:"usage,omitempty"`
	Error  *struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason,omitempty"`
	} `json:"incomplete_details,omitempty"`
}

type responsesItemMin struct {
	Type   string `json:"type,omitempty"`
	ID     string `json:"id,omitempty"`
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
}

// DecodeChunk parses one SSE data line (with any "data: " prefix already stripped) into IR
//
// deltas. Upstream Responses SSE frames have the form "event: <type>\ndata: <json>\n\n";
// callers hand us the data line, and the event type is already carried in the JSON "type"
func (d *StreamDecoder) DecodeChunk(raw string) ([]protocols.IRStreamDelta, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	// tolerate an upstream that still has a glued-on "data:" prefix
	if payload, ok := protocols.SSEDataPayload(raw); ok {
		raw = payload
	}
	if raw == "[DONE]" || raw == "" {
		return nil, nil
	}

	var evt responsesStreamEventWire
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		// single-frame parse failure: return an Unknown delta and let the caller decide whether
		return []protocols.IRStreamDelta{protocols.DeltaUnknown{Raw: json.RawMessage(raw)}}, nil
	}

	var out []protocols.IRStreamDelta

	// Once the upstream has explicitly failed (response.failed / error event), stop emitting any
	// further deltas — this prevents a later "success terminator" frame such as response.done
	if d.upstreamErr != nil {
		return nil, nil
	}

	switch evt.Type {
	case "response.created", "response.in_progress":
		if !d.startEmitted && evt.Response != nil {
			out = append(out, protocols.DeltaMessageStart{ID: evt.Response.ID, Model: evt.Response.Model})
			d.startEmitted = true
		}
	case "response.output_item.added":
		// a function_call item is starting: record its call_id and name
		if evt.Item != nil && evt.Item.Type == "function_call" {
			idx := -1
			if evt.OutputIndex != nil {
				idx = *evt.OutputIndex
			}
			d.activeFunctionCalls[idx] = functionCallState{
				CallID: evt.Item.CallID,
				Name:   evt.Item.Name,
			}
			out = append(out, protocols.DeltaToolCallStart{
				ID:    evt.Item.CallID,
				Name:  evt.Item.Name,
				Index: idx,
			})
		}
	case "response.output_text.delta":
		if evt.Delta != "" {
			out = append(out, protocols.DeltaText{Text: evt.Delta})
		}
	case "response.reasoning_summary_text.delta":
		if evt.Delta != "" {
			out = append(out, protocols.DeltaThinking{Text: evt.Delta})
		}
	case "response.function_call_arguments.delta":
		if evt.Delta != "" {
			idx := -1
			if evt.OutputIndex != nil {
				idx = *evt.OutputIndex
			}
			out = append(out, protocols.DeltaToolCallArgs{Index: idx, Arguments: evt.Delta})
		}
	case "response.completed":
		out = d.appendUsageDelta(out, evt.Response)
		out = d.appendTerminal(out, evt.Response)
	case "response.failed":
		// Keep whatever usage the failed response carries: the user is not
		// charged (Finish returns an error, so the handler settles as failure),
		// but provider_cost / account_cost and dispatch analytics must still
		// reflect the tokens the upstream actually consumed. The non-streaming
		// ResponseDecoder already preserves failure usage; this path used to
		// drop it. Still deliberately NO DeltaDone — see below.
		out = d.appendUsageDelta(out, evt.Response)
		// Upstream explicit failure: record the type only, never raw content
		// (avoids leaking prompt fragments / tool args into application logs).
		// **Do not emit protocols.DeltaDone**: emitting Done would make the ingress encoder write a
		// "looks successful" terminator frame (the counterpart of Claude message_stop / Chat
		// finish_reason=stop / Gemini finishReason=STOP), so the client would treat a failed
		// request as a complete response, inconsistent with the server settling it as a 502.
		// upstreamErr is
		d.failStream()
	case "response.incomplete":
		// A run truncated by max_output_tokens ends with response.incomplete
		// instead of response.completed, and that event carries the full usage
		// too. Skipping it left the priciest requests (output limit fully
		// consumed) with zero usage: unbilled, no token counts for the client,
		// and hasMeaningfulUsage=false in the relay, which also stops benign
		// post-DONE read errors from being excused and mislabels healthy
		// providers as 502.
		out = d.appendUsageDelta(out, evt.Response)
		out = d.appendDone(out, responsesIncompleteReason(evt.Response))
	case "response.done":
		// Fallback DONE: usage is still merged (this frame may be richer than the
		// previous terminal event); the terminator itself is deduped by appendDone.
		out = d.appendUsageDelta(out, evt.Response)
		out = d.appendTerminal(out, evt.Response)
	case "error":
		// top-level error event: also treated as a failure, no Done emitted
		d.failStreamWith(errResponsesStreamErrorEvent)
		out = append(out, protocols.DeltaUnknown{Raw: json.RawMessage(raw)})
	}

	return out, nil
}

// Finish is called when the stream ends. If an upstream failure/error event was captured
// mid-stream, it returns that error; the IR pipeline propagates it back to the handler so the
func (d *StreamDecoder) Finish() ([]protocols.IRStreamDelta, error) {
	return nil, d.upstreamErr
}

// appendUsageDelta merges usage from any terminal event and emits a DeltaUsage
// when that actually added information.
//
// Merges instead of first-wins: a run can send response.completed carrying only
// token counts and then response.done carrying the cached / reasoning
// breakdown (or the reverse). Latching onto whichever arrived first would throw
// the richer one away and under-report billing. IRUsage.Merge only overwrites
// non-zero fields, so a later sparser frame can never blank out counts already
// collected.
func (d *StreamDecoder) appendUsageDelta(out []protocols.IRStreamDelta, resp *responsesRespMin) []protocols.IRStreamDelta {
	if resp == nil || resp.Usage == nil {
		return out
	}
	before := d.usage
	d.collectUsage(resp.Usage)
	if d.usageEmitted && d.usage == before {
		// Nothing new — do not emit a duplicate usage frame.
		return out
	}
	d.usageEmitted = true
	// Marked rather than dropped — see the chat decoder: IRUsage.Merge would
	// otherwise leave an earlier frame's counts standing and let a terminal
	// event bill them as coherent.
	//
	// Merge judges each incoming src frame on its own, but the ACCUMULATED
	// record can still be impossible once frames combine (one frame's prompt,
	// another's oversized cache). Re-weighing the merged result here is what
	// catches that.
	//
	// IsIncoherentMidStream, not IsIncoherent: this record is stitched from
	// every frame seen so far and more may follow, so only the rules that hold
	// on a half-finished stream may be applied. The reasoning-subset rule is
	// excluded because both reasoning and completion counts grow, and latching
	// a verdict on the intermediate state would condemn a stream whose final
	// counts turn out coherent.
	if d.usage.IsIncoherentMidStream() {
		d.usage.Invalid = true
	}
	return append(out, protocols.DeltaUsage{Usage: d.usage})
}

// responsesIncompleteReason maps response.incomplete's incomplete_details.reason
// onto the IR stop-reason vocabulary.
//
// The schema allows more than truncation here — content_filter is equally
// valid. Hardcoding "length" reported a safety block as a token cut-off. The
// IR values are the ones every other decoder uses, so egress encoders can map
// them onto each protocol's own enum: leaking the wire word through would make
// the Chat encoder emit a finish_reason outside OpenAI's enum
// (stop|length|tool_calls|content_filter) and make the Gemini encoder fall
// through to "STOP", presenting a truncated answer as cleanly finished.
//
// Defaults to "length": max_output_tokens is the overwhelmingly common cause,
// and mislabelling an unknown reason as truncation is far safer than letting it
// look like a clean stop.
func responsesIncompleteReason(resp *responsesRespMin) string {
	reason := ""
	if resp != nil && resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	return normalizeResponsesIncompleteReason(reason)
}

// normalizeResponsesIncompleteReason maps incomplete_details.reason onto the IR
// vocabulary, shared by the streaming and non-streaming decoders so the two can
// never drift apart.
//
// Unknown reasons map to "length", NOT passed through verbatim. This looks like
// it contradicts the project-wide rule that unknown upstream values are
// forwarded as-is (mapFromGeminiFinishReason etc.), but the two situations carry
// different information:
//
//   - An unknown finishReason tells us nothing about whether the run ended
//     abnormally, so inventing a classification would be a guess.
//   - response.incomplete has ALREADY declared the run unfinished. Only the
//     sub-reason is unknown, and abnormality is not in question.
//
// Forwarding the raw sub-reason lost exactly that: IRStopReasonIsAbnormal
// recognises only length / content_filter, so a `server_error` incomplete became
// a non-abnormal stop reason — the Gemini egress encoded it as STOP and the Chat
// egress let a tool-call inference overwrite it with "tool_calls", telling the
// client that arguments from an unfinished run were safe to execute.
//
// Between the two failure modes the choice is not close: mislabelling a
// server_error as truncation costs an inaccurate log line and possibly a futile
// retry with a higher max_tokens; losing abnormality costs a client executing
// half-written tool calls. "length" is also the overwhelmingly common real cause
// and the one value every target protocol can express (MAX_TOKENS / max_tokens).
func normalizeResponsesIncompleteReason(reason string) string {
	if strings.ToLower(strings.TrimSpace(reason)) == "content_filter" {
		return "content_filter"
	}
	return "length"
}

// responsesDoneStatus reports the status carried by a terminal event, if any.
func responsesDoneStatus(resp *responsesRespMin) string {
	if resp == nil {
		return ""
	}
	// Only an error with actual content counts. A compat relay declaring
	// `Error ErrorObj` as a value type (to satisfy the schema, where `error` is
	// required-but-nullable) serialises `"error":{"code":"","message":""}` on a
	// perfectly successful response; treating that as a failure would retroactively
	// fail a fully delivered stream.
	if resp.Error != nil && (resp.Error.Code != "" || resp.Error.Message != "") {
		return "failed"
	}
	return strings.ToLower(strings.TrimSpace(resp.Status))
}

// appendTerminal classifies a terminal event (response.completed /
// response.done) by the status it carries.
//
// Both go through here so the two can never disagree about identical data: a
// compat implementation that puts all terminal state on one event sends
// response.completed with status=incomplete, and classifying only response.done
// would emit DeltaDone("stop") for it — latching doneEmitted and permanently
// discarding the real truncation reason that arrives on the later
// response.incomplete.
//
// A blacklist, matching the non-streaming decoder: only an explicit
// failed/cancelled fails the stream, and only when the stream had not already
// finished — a bookkeeping frame trailing a successful completion must never
// retroactively fail it, which would skip EncodeDone and leave the client with
// no terminator, the user unbilled and a healthy provider circuit-broken.
func (d *StreamDecoder) appendTerminal(out []protocols.IRStreamDelta, resp *responsesRespMin) []protocols.IRStreamDelta {
	status := responsesDoneStatus(resp)
	switch {
	case responsesStatusIsNonServed(status):
		d.failStream()
		return out
	case status == "incomplete":
		return d.appendDone(out, responsesIncompleteReason(resp))
	default:
		// completed / absent / unknown. Absent is the common case: most
		// upstreams' terminal event carries no status, and the field is not in
		// the Response schema's required list.
		return d.appendDone(out, "stop")
	}
}

// failStream marks the stream as failed, but never retroactively: once a
// terminal frame has already been forwarded the client holds a complete answer,
// and failing then would skip EncodeDone — leaving that answer without a
// terminator while the user goes unbilled and a healthy provider is
// circuit-broken. Bookkeeping frames that trail a successful completion are
// therefore ignored.
func (d *StreamDecoder) failStream() { d.failStreamWith(errResponsesStreamFailed) }

func (d *StreamDecoder) failStreamWith(err error) {
	if d.doneEmitted {
		return
	}
	d.upstreamErr = err
}

// appendDone emits at most one DeltaDone per stream and keeps the FIRST stop
// reason.
//
// A truncated run ends with response.incomplete (max_tokens) and may still be
// followed by a generic response.done. The previous guard only scanned the
// deltas produced by the current DecodeChunk call, and each SSE line is a
// separate call, so it could never see across events: the client received two
// terminal frames and the "max_tokens" truncation reason was overwritten by a
// later "stop", making a cut-off answer look cleanly finished.
func (d *StreamDecoder) appendDone(out []protocols.IRStreamDelta, reason string) []protocols.IRStreamDelta {
	if d.doneEmitted {
		return out
	}
	d.doneEmitted = true
	return append(out, protocols.DeltaDone{StopReason: reason})
}

func (d *StreamDecoder) collectUsage(u *responsesUsage) {
	if u == nil {
		return
	}
	// The FRAME gets the full verdict before it is folded in. This is the only
	// place holding one upstream snapshot: Merge deliberately applies the
	// weaker mid-stream verdict, because by the time a record reaches it the
	// caller may be handing over a running total rather than a frame (this
	// decoder does exactly that on the way out). Settling the frame here and
	// letting Invalid propagate is what keeps a self-contradicting snapshot
	// from being waved through.
	frame := responsesUsageToIR(u)
	frame.Invalid = frame.IsIncoherent()
	// Field-level merge so a later, sparser terminal event cannot zero out
	// counts an earlier one already provided. Shares responsesUsageToIR with the
	// non-streaming decoder so neither path can drift on which fields it copies.
	d.usage.Merge(frame)
}
