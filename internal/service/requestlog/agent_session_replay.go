// Replay assembly for tool sessions: the pure dedup pass that turns one
// session's chronological request sequence into a replay transcript — one
// block per request carrying ONLY what that request newly contributed (its
// new caller-side messages plus its own assistant reply), so a long
// session reads as one continuous conversation instead of N copies of an
// ever-growing history. No storage, no clock, no error surface: every
// degraded input (unparseable body, capped body, missing capture) degrades
// to a well-shaped block so a single bad row can never take the whole
// replay down.
package requestlog

import (
	"encoding/json"
	"reflect"
	"time"
)

// Replay-source tags for the assistant half of a replay block. Exactly one
// applies per block, by priority: a recorded inline response body (inline),
// else the sent-SSE capture (stream), else the assistant copies the next
// request resends as history (resend), else nothing was recorded (none).
const (
	ReplaySourceInline = "inline"
	ReplaySourceStream = "stream"
	ReplaySourceResend = "resend"
	ReplaySourceNone   = "none"
)

// AgentSessionReplayInput is one request's contribution to the assembly:
// the row metadata the replay block echoes, plus the three captured text
// halves with their truncation flags. The bodies arrive exactly as the
// caller read them (already under the inline cap); keeping them in the
// input rather than fetching inside the assembly is what makes the pass
// table-testable.
type AgentSessionReplayInput struct {
	// Row metadata echoed onto the replay block verbatim.
	RequestID    string
	CreatedAt    time.Time
	ModelName    string
	StatusClass  string
	IsStream     bool
	InputTokens  int
	OutputTokens int
	DurationMs   int64
	ProviderName string

	// RequestBody is the caller-side body JSON. RequestBodyTruncated marks
	// that the inline copy was cut at the storage cap, so the text is NOT
	// the full original and message parsing degrades to an empty array.
	RequestBody          string
	RequestBodyTruncated bool
	// ResponseBody is the inline (non-streaming) response body; empty means
	// the response was not recorded. ResponseBodyTruncated marks the cap.
	ResponseBody          string
	ResponseBodyTruncated bool
	// StreamBody is the sent-SSE capture text (merged from disk by the
	// caller); empty means no capture exists. StreamBodyTruncated marks
	// the cap.
	StreamBody          string
	StreamBodyTruncated bool
}

// AgentSessionReplayAssistant is the reply half of one replay block: where
// the reply text came from, whether that text was capped, and the text
// itself. Raw is polymorphic by source — the response body string for
// inline, the SSE capture string for stream, the resent assistant message
// objects for resend (possibly several, in order), and nil for none (the
// frontend renders an explicit "not recorded" placeholder, never a fake
// empty reply).
type AgentSessionReplayAssistant struct {
	Source    string `json:"source"`
	Truncated bool   `json:"truncated"`
	Raw       any    `json:"raw"`
}

// AgentSessionReplayBlock is one request's replay block: the metadata the
// timeline labels need, the messages this request newly contributed, and
// the reply attribution.
type AgentSessionReplayBlock struct {
	RequestID    string    `json:"request_id"`
	CreatedAt    time.Time `json:"created_at"`
	ModelName    string    `json:"model_name"`
	StatusClass  string    `json:"status_class"`
	IsStream     bool      `json:"is_stream"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	DurationMs   int64     `json:"duration_ms"`
	ProviderName string    `json:"provider_name"`
	// Added holds this request's NEW caller-side messages as raw JSON
	// objects: everything after the longest structurally equal prefix
	// shared with the previous request's messages, minus the assistant and
	// system roles — the previous turn's reply arrives via Assistant (or as
	// the next block's resend), and system prompts have their own channel.
	// An empty array means a pure retry or no new input.
	Added []any `json:"added"`
	// Assistant is the reply attribution for this turn (the four sources
	// are documented on AgentSessionReplayAssistant).
	Assistant AgentSessionReplayAssistant `json:"assistant"`
	// AddedTruncated marks that the request body was cut at the inline
	// storage cap, so Added degrades to an empty array: the replay shows
	// the turn's metadata and reply with a truncation notice rather than a
	// half-parsed guess at the input.
	AddedTruncated bool `json:"added_truncated"`
}

// AgentSessionReplaySystem is one entry of the deduplicated system-prompt
// timeline: the value, the request it was first seen on, and whether the
// body it came from was capped. Raw is polymorphic by dialect — a string
// or block array for the Anthropic top-level system field, the role=system
// message objects for the OpenAI dialect.
type AgentSessionReplaySystem struct {
	SeenAtRequest string `json:"seen_at_request"`
	Truncated     bool   `json:"truncated"`
	Raw           any    `json:"raw"`
}

// AssembleAgentSessionReplay turns a session's chronological request
// sequence into the two replay halves: one block per request, plus the
// deduplicated system-prompt timeline. The pass runs in three steps over
// the whole sequence:
//
//  1. Parse every request body's top-level messages array and system
//     value. The Anthropic and OpenAI dialects share the shape here (a
//     messages array of role-tagged objects), so elements compare
//     structurally and no dialect semantics are needed.
//  2. new_i = messages_i minus the longest structurally equal prefix it
//     shares with messages_{i-1} (the first request keeps everything).
//  3. Per request: added_i = new_i minus the assistant and system roles;
//     assistant_i sources by priority inline > stream > resend > none,
//     where resend lifts the assistant copies the NEXT request's new
//     suffix carries (a client resending an unrecorded reply as history);
//     the system timeline appends a record whenever this request's system
//     value differs from the last recorded one.
//
// The filter in step 3 plus the priority in its assistant rule make every
// assistant message appear exactly once: when the reply was recorded, the
// next request's resent copy is dropped from added and the recorded text
// carries the turn; when it was not, the copy itself is promoted to the
// turn's reply.
func AssembleAgentSessionReplay(inputs []AgentSessionReplayInput) (blocks []AgentSessionReplayBlock, system []AgentSessionReplaySystem) {
	blocks = make([]AgentSessionReplayBlock, 0, len(inputs))
	system = make([]AgentSessionReplaySystem, 0)
	if len(inputs) == 0 {
		return blocks, system
	}

	type replayParsedBody struct {
		messages  []any
		system    any
		hasSystem bool
	}
	parsed := make([]replayParsedBody, len(inputs))
	for i := range inputs {
		parsed[i].messages, parsed[i].system, parsed[i].hasSystem =
			parseReplayBody(inputs[i].RequestBody, inputs[i].RequestBodyTruncated)
	}

	// newSuffix[i] is messages_i after the longest structural common prefix
	// with messages_{i-1} is stripped (all of messages_0 for the first
	// request, whose prefix is empty by definition).
	newSuffix := make([][]any, len(inputs))
	for i := range parsed {
		var prev []any
		if i > 0 {
			prev = parsed[i-1].messages
		}
		newSuffix[i] = parsed[i].messages[commonStructPrefixLen(prev, parsed[i].messages):]
	}

	var lastSystem any
	hasLastSystem := false
	for i := range inputs {
		in := &inputs[i]

		added := make([]any, 0, len(newSuffix[i]))
		for _, m := range newSuffix[i] {
			if role := replayMessageRole(m); role != "assistant" && role != "system" {
				added = append(added, m)
			}
		}

		// The system timeline: requests without a system value do not
		// participate at all — the channel exists to show prompts that
		// exist, and a valueless turn would only inject noise records, so
		// dedup compares the values that are present.
		if parsed[i].hasSystem && (!hasLastSystem || !reflect.DeepEqual(lastSystem, parsed[i].system)) {
			system = append(system, AgentSessionReplaySystem{
				SeenAtRequest: in.RequestID,
				Truncated:     in.RequestBodyTruncated,
				Raw:           parsed[i].system,
			})
			lastSystem, hasLastSystem = parsed[i].system, true
		}

		blocks = append(blocks, AgentSessionReplayBlock{
			RequestID:      in.RequestID,
			CreatedAt:      in.CreatedAt,
			ModelName:      in.ModelName,
			StatusClass:    in.StatusClass,
			IsStream:       in.IsStream,
			InputTokens:    in.InputTokens,
			OutputTokens:   in.OutputTokens,
			DurationMs:     in.DurationMs,
			ProviderName:   in.ProviderName,
			Added:          added,
			Assistant:      replayAssistant(in, newSuffix, i),
			AddedTruncated: in.RequestBodyTruncated,
		})
	}
	return blocks, system
}

// replayAssistant attributes one block's reply. Priority: a recorded inline
// response body, then the SSE capture, then the assistant copies the next
// request's new suffix resends, else none. Truncated only ever applies to
// the two captured-text sources — a resend carries parsed message objects
// (a capped body parses to nothing, so no copies could come from one), and
// none carries no text at all.
func replayAssistant(in *AgentSessionReplayInput, newSuffix [][]any, i int) AgentSessionReplayAssistant {
	if in.ResponseBody != "" {
		return AgentSessionReplayAssistant{
			Source:    ReplaySourceInline,
			Truncated: in.ResponseBodyTruncated,
			Raw:       in.ResponseBody,
		}
	}
	if in.StreamBody != "" {
		return AgentSessionReplayAssistant{
			Source:    ReplaySourceStream,
			Truncated: in.StreamBodyTruncated,
			Raw:       in.StreamBody,
		}
	}
	if i+1 < len(newSuffix) {
		if copies := replayAssistantMessages(newSuffix[i+1]); len(copies) > 0 {
			return AgentSessionReplayAssistant{
				Source: ReplaySourceResend,
				Raw:    copies,
			}
		}
	}
	return AgentSessionReplayAssistant{Source: ReplaySourceNone}
}

// parseReplayBody extracts the two replay-relevant halves of one request
// body: the top-level messages array and the system value. A capped body is
// not the full original and an unparseable one carries no structure at
// all, so both degrade to "no messages, no system" while the block keeps
// its metadata. The system value is the Anthropic top-level system field
// (string or block array) when present, else the OpenAI dialect's
// role=system messages collected in order.
func parseReplayBody(body string, truncated bool) (messages []any, systemValue any, hasSystem bool) {
	if truncated || body == "" {
		return nil, nil, false
	}
	var top map[string]any
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return nil, nil, false
	}
	if raw, ok := top["messages"].([]any); ok {
		messages = raw
	}
	if sys, ok := top["system"]; ok && sys != nil {
		return messages, sys, true
	}
	var systemMsgs []any
	for _, m := range messages {
		if replayMessageRole(m) == "system" {
			systemMsgs = append(systemMsgs, m)
		}
	}
	if len(systemMsgs) > 0 {
		return messages, systemMsgs, true
	}
	return messages, nil, false
}

// replayMessageRole returns a message object's role string, or "" when the
// element is not an object or carries no string role — such an element is
// neither assistant nor system, so it stays in added.
func replayMessageRole(m any) string {
	obj, ok := m.(map[string]any)
	if !ok {
		return ""
	}
	role, _ := obj["role"].(string)
	return role
}

// replayAssistantMessages picks the assistant-role messages out of one
// request's new suffix, in order — the copies a client resends when it
// replays a reply our store never recorded as history.
func replayAssistantMessages(suffix []any) []any {
	var out []any
	for _, m := range suffix {
		if replayMessageRole(m) == "assistant" {
			out = append(out, m)
		}
	}
	return out
}

// commonStructPrefixLen returns the length of the longest prefix a and b
// share under element-wise structural equality (parsed JSON values: maps,
// slices, strings, float64 numbers, bools). Zero when the first elements
// already differ; min(len(a), len(b)) when one is a prefix of the other,
// which is what makes an identical retry produce an empty new suffix.
func commonStructPrefixLen(a, b []any) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if !reflect.DeepEqual(a[i], b[i]) {
			return i
		}
	}
	return n
}
