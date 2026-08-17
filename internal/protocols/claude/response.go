package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"strings"
)

// webSearchesIn reads the provider's count of searches it ran on its own
// initiative out of a usage object's server_tool_use member.
//
// It takes the raw bytes and decodes them separately, rather than the member
// being typed on the usage struct itself, and that separation is the whole
// point. Typed inline, a provider sending web_search_requests as "2" or the
// member as an array fails the enclosing Unmarshal — and the enclosing object
// is the usage, so the token counts and the stop reason go down with it. A
// stream frame lost that way settles the request with zero completion tokens
// and no finish reason. Reading one optional extra must not be able to do that,
// so a shape this function cannot read is simply no searches.
//
// Absent means the provider ran none: "no server_tool_use key" and
// "web_search_requests: 0" are the same fact and must not be two branches.
//
// web_fetch_requests sits alongside web_search_requests in the same object and
// is deliberately not read: there is no IR field to put it in, and a count
// nothing downstream can price or record would be worse than the documented
// absence.
func webSearchesIn(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var m struct {
		WebSearchRequests int `json:"web_search_requests"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return 0
	}
	return m.WebSearchRequests
}

// ResponseDecoder decodes a Claude Messages API JSON response into IR.
type ResponseDecoder struct{}

func (ResponseDecoder) DecodeResponse(body json.RawMessage) (*protocols.IRResponse, error) {
	var resp struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text,omitempty"`
			ID       string          `json:"id,omitempty"`
			Name     string          `json:"name,omitempty"`
			Input    json.RawMessage `json:"input,omitempty"`
			Thinking string          `json:"thinking,omitempty"`
		} `json:"content"`
		Usage struct {
			InputTokens              int             `json:"input_tokens"`
			OutputTokens             int             `json:"output_tokens"`
			CacheCreationInputTokens int             `json:"cache_creation_input_tokens,omitempty"`
			CacheReadInputTokens     int             `json:"cache_read_input_tokens,omitempty"`
			ServerToolUse            json.RawMessage `json:"server_tool_use,omitempty"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	irResp := &protocols.IRResponse{
		ID:    resp.ID,
		Model: resp.Model,
		Usage: protocols.IRUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens,
			CacheWriteTokens: resp.Usage.CacheCreationInputTokens,
			CacheReadTokens:  resp.Usage.CacheReadInputTokens,
			WebSearchCount:   webSearchesIn(resp.Usage.ServerToolUse),
		},
	}
	// Set the verdict at the IR exit so every consumer reads Invalid instead of
	// re-judging. Claude is the net-convention protocol (input_tokens excludes
	// cache), so IsIncoherent here effectively checks negatives — but routing it
	// through the single predicate keeps this decoder in step with the others.
	// See IRUsage.IsIncoherent.
	irResp.Usage.Invalid = irResp.Usage.IsIncoherent()

	irResp.StopReason = claudeMapStopReason(resp.StopReason)

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			irResp.Content += block.Text
		case "tool_use":
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			irResp.ToolCalls = append(irResp.ToolCalls, protocols.IRToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: args,
			})
		case "thinking":
			irResp.ReasoningContent += block.Thinking
		}
	}

	return irResp, nil
}

// StreamDecoder decodes Claude SSE events into IR deltas.
type StreamDecoder struct {
	usage       protocols.IRUsage
	id          string
	model       string
	started     bool
	completed   bool   // the stream is only considered naturally finished after message_stop
	doneEmitted bool   // whether DeltaDone was already emitted during message_delta (standard upstream path)
	upstreamErr error  // explicit upstream type:"error" event; makes Finish return an error so the caller cannot settle a failed run as success
	blockType   string // current content_block type
	blockIndex  int
	toolNames   map[int]string
	toolCallIdx int
	toolIdxMap  map[int]int // content block index -> tool call index
}

func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{
		toolNames:  make(map[int]string),
		toolIdxMap: make(map[int]int),
	}
}

func (d *StreamDecoder) DecodeChunk(raw string) ([]protocols.IRStreamDelta, error) {
	line := strings.TrimSpace(raw)

	if line == "" || strings.HasPrefix(line, "event:") {
		return nil, nil
	}

	// SSE makes the space after the colon optional (Anthropic-compatible
	// upstreams ship `data:{...}`), and isDataLine on the forwarding side
	// already honours that — the decoder must agree or it reads a whole
	// delivered stream as one that said nothing.
	payload, ok := strings.CutPrefix(line, "data:")
	if !ok {
		return nil, nil
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, nil
	}

	var event struct {
		Type    string `json:"type"`
		Index   int    `json:"index,omitempty"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
			} `json:"usage"`
		} `json:"message,omitempty"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
			StopReason  string `json:"stop_reason,omitempty"`
			Thinking    string `json:"thinking,omitempty"`
		} `json:"delta,omitempty"`
		ContentBlock *struct {
			Type  string          `json:"type"`
			ID    string          `json:"id,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content_block,omitempty"`
		Usage *struct {
			InputTokens              *int            `json:"input_tokens,omitempty"`
			OutputTokens             int             `json:"output_tokens"`
			CacheCreationInputTokens int             `json:"cache_creation_input_tokens,omitempty"`
			CacheReadInputTokens     int             `json:"cache_read_input_tokens,omitempty"`
			ServerToolUse            json.RawMessage `json:"server_tool_use,omitempty"`
		} `json:"usage,omitempty"`
	}
	if json.Unmarshal([]byte(payload), &event) != nil {
		return nil, nil
	}

	var deltas []protocols.IRStreamDelta

	switch event.Type {
	case "message_start":
		if event.Message != nil {
			d.id = event.Message.ID
			d.model = event.Message.Model
			d.started = true
			deltas = append(deltas, protocols.DeltaMessageStart{
				ID:    d.id,
				Model: d.model,
			})
			if event.Message.Usage != nil {
				d.usage = protocols.IRUsage{
					PromptTokens:     event.Message.Usage.InputTokens,
					CompletionTokens: 0,
					TotalTokens:      event.Message.Usage.InputTokens + event.Message.Usage.CacheCreationInputTokens + event.Message.Usage.CacheReadInputTokens,
					CacheWriteTokens: event.Message.Usage.CacheCreationInputTokens,
					CacheReadTokens:  event.Message.Usage.CacheReadInputTokens,
				}
			}
		}

	case "content_block_start":
		if event.ContentBlock != nil {
			d.blockType = event.ContentBlock.Type
			d.blockIndex = event.Index
			if event.ContentBlock.Type == "tool_use" {
				d.toolNames[event.Index] = event.ContentBlock.Name
				d.toolIdxMap[event.Index] = d.toolCallIdx
				deltas = append(deltas, protocols.DeltaToolCallStart{
					Index: d.toolCallIdx,
					ID:    event.ContentBlock.ID,
					Name:  event.ContentBlock.Name,
				})
				d.toolCallIdx++
			}
		}

	case "content_block_delta":
		if event.Delta == nil {
			break
		}
		switch event.Delta.Type {
		case "text_delta":
			if event.Delta.Text != "" {
				deltas = append(deltas, protocols.DeltaText{Text: event.Delta.Text})
			}
		case "thinking_delta":
			if event.Delta.Thinking != "" {
				deltas = append(deltas, protocols.DeltaThinking{Text: event.Delta.Thinking})
			}
		case "input_json_delta":
			name := d.toolNames[event.Index]
			tcIdx := d.toolIdxMap[event.Index]
			if name != "" && event.Delta.PartialJSON != "" {
				deltas = append(deltas, protocols.DeltaToolCallArgs{
					Index:     tcIdx,
					Arguments: event.Delta.PartialJSON,
				})
			}
		}

	case "content_block_stop":
		// nothing to do

	case "message_delta":
		if event.Delta != nil && event.Delta.StopReason != "" {
			reason := claudeMapStopReason(event.Delta.StopReason)
			deltas = append(deltas, protocols.DeltaDone{StopReason: reason})
			d.doneEmitted = true
		}
		if event.Usage != nil {
			d.usage.CompletionTokens = event.Usage.OutputTokens
			// Fallback backfill: gateway may emit net input_tokens on message_delta
			// to compensate for message_start's input=0 (upstream usage arrives
			// at stream end). Only backfill when message_start left input at 0;
			// never overwrite a real value. Background in stream_encoder.go
			// claudeUsageMap / emitMessageDelta doc.
			if event.Usage.InputTokens != nil && *event.Usage.InputTokens > 0 && d.usage.PromptTokens == 0 {
				d.usage.PromptTokens = *event.Usage.InputTokens
			}
			// Anthropic updates the final cache token counts in message_delta (on a full cache hit message_start carries input_tokens=0)
			if event.Usage.CacheCreationInputTokens > 0 {
				d.usage.CacheWriteTokens = event.Usage.CacheCreationInputTokens
			}
			if event.Usage.CacheReadInputTokens > 0 {
				d.usage.CacheReadTokens = event.Usage.CacheReadInputTokens
			}
			newTotal := d.usage.PromptTokens + event.Usage.OutputTokens + d.usage.CacheWriteTokens + d.usage.CacheReadTokens
			if d.usage.TotalTokens == 0 || d.usage.TotalTokens < newTotal {
				d.usage.TotalTokens = newTotal
			}
			// Never regresses to zero, and never silently discards a number
			// that was actually there.
			//
			// The count is cumulative for the whole stream, so a frame that
			// omits server_tool_use — or carries a shape this cannot read — is
			// saying nothing new rather than saying no searches happened, and
			// overwriting with its zero would drop searches already reported.
			// Both of those arrive here as 0, which is why 0 does not assign.
			//
			// A NEGATIVE does assign. It is a count the provider stated and it
			// cannot be true, and the coherence rules downstream exist to catch
			// exactly that. Dropping it here would make this path disagree with
			// the whole-response one, which keeps it — and the disagreement
			// runs the wrong way: the stream would launder an impossible number
			// into a record that reads as fine.
			if n := webSearchesIn(event.Usage.ServerToolUse); n != 0 {
				d.usage.WebSearchCount = n
			}
			deltas = append(deltas, protocols.DeltaUsage{Usage: d.usage})
		}

	case "message_stop":
		// terminal event: mark the stream as naturally finished so Finish can fall back to emitting the usage carried in message_start
		d.completed = true

	case "ping":
		// ignore

	case "error":
		// Explicit upstream error events (e.g. {"type":"error","error":{"type":"overloaded_error","message":"..."}})
		// must make Finish return an error: the caller then takes the failure path (502, user not billed)
		// instead of treating the trailing EOF as a success and quietly settling a failed stream as
		// successful. When a stream carries multiple error events, the first one wins.
		if d.upstreamErr == nil {
			var errPayload struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if jerr := json.Unmarshal([]byte(payload), &errPayload); jerr == nil && errPayload.Error.Message != "" {
				d.upstreamErr = fmt.Errorf("upstream claude error event: %s: %s", errPayload.Error.Type, errPayload.Error.Message)
			} else {
				d.upstreamErr = errors.New("upstream claude error event")
			}
		}
	}

	return deltas, nil
}

func (d *StreamDecoder) Finish() ([]protocols.IRStreamDelta, error) {
	// Explicit upstream error events take priority: the caller takes the failure path, but partial
	// usage is still passed through so provider cost is recorded. No DeltaDone is emitted, so the
	// relay helper does not treat the stream as successful (IRStreamRelay skips EncodeDone on
	// finishErr, and sawDone must not be raised either).
	if d.upstreamErr != nil {
		var out []protocols.IRStreamDelta
		if d.usage.PromptTokens > 0 || d.usage.CompletionTokens > 0 || d.usage.CacheWriteTokens > 0 || d.usage.CacheReadTokens > 0 {
			out = append(out, protocols.DeltaUsage{Usage: d.usage})
		}
		return out, d.upstreamErr
	}
	// Fall back to emitting the terminal signal only after message_stop (d.completed=true) —
	// prevents a truncated stream from being settled as a billable success.
	//
	// Both upstream shapes must be covered:
	// 1) Standard Claude: message_delta already emitted DeltaDone + full usage, so this only
	//    appends one DeltaUsage (last-wins does not affect encoder.Usage); DeltaDone is not
	//    re-emitted.
	// 2) Gateway-fronted Anthropic: jumps straight from content_block_stop to message_stop with
	//    no message_delta, so a DeltaDone must be emitted here (so the relay helper's sawDone
	//    marks the stream complete) plus a DeltaUsage (passing message_start's input_tokens to
	//    the billing layer).
	if !d.completed || !d.started {
		return nil, nil
	}
	var out []protocols.IRStreamDelta
	if !d.doneEmitted {
		out = append(out, protocols.DeltaDone{StopReason: "stop"})
		d.doneEmitted = true
	}
	if d.usage.PromptTokens > 0 || d.usage.CompletionTokens > 0 || d.usage.CacheWriteTokens > 0 || d.usage.CacheReadTokens > 0 {
		out = append(out, protocols.DeltaUsage{Usage: d.usage})
	}
	return out, nil
}

// claudeMapStopReason maps Claude API stop_reason to IR stop_reason.
// claudeMapStopReason normalises Anthropic's stop_reason onto the IR
// vocabulary (stop | length | tool_calls | content_filter | error).
//
// Anthropic's enum is end_turn | max_tokens | stop_sequence | tool_use |
// pause_turn | refusal | model_context_window_exceeded. Lower-casing unknown
// values instead of mapping them let "refusal" reach the IR verbatim, where no
// egress encoder recognises it: the abnormal-termination guard did not fire, so
// a safety refusal could be masked by a tool-call inference, and encoders
// emitted a value outside their own protocol's enum.
func claudeMapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "refusal":
		// "streaming classifiers intervene to handle potential policy
		// violations" — the same concept OpenAI calls content_filter.
		return "content_filter"
	default:
		// Anything Anthropic adds later passes through lower-cased, matching
		// new-api's reasonmap default. The values above are mapped because each
		// has a real equivalent in the other protocols.
		if reason != "" {
			return strings.ToLower(reason)
		}
		return "stop"
	}
}

// irToClaudeStopReason maps the IR vocabulary onto Anthropic's stop_reason
// enum (end_turn | max_tokens | stop_sequence | tool_use | pause_turn |
// refusal | model_context_window_exceeded).
//
// content_filter MUST be translated: it is not in Anthropic's enum, so passing
// it through produced wire a strict SDK rejects. "refusal" is the documented
// equivalent ("streaming classifiers intervene to handle potential policy
// violations").
func irToClaudeStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return reason
	}
}
