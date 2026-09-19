package responses

import (
	"encoding/json"
	"fmt"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"sort"
	"strings"
)

// --- Responses Request Decoder ---

// RequestDecoder decodes an OpenAI Responses API request into IR.
type RequestDecoder struct{}

func (RequestDecoder) Protocol() protocols.ProtocolID { return protocols.ProtocolResponses }

func (RequestDecoder) DecodeRequest(body json.RawMessage, model string, isStream bool) (*protocols.IRRequest, error) {
	var req struct {
		Model           string          `json:"model"`
		Input           json.RawMessage `json:"input,omitempty"`
		Instructions    json.RawMessage `json:"instructions,omitempty"`
		MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
		Temperature     *float64        `json:"temperature,omitempty"`
		TopP            *float64        `json:"top_p,omitempty"`
		Tools           json.RawMessage `json:"tools,omitempty"`
		ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
		Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("parse responses request: %w", err)
	}

	irReq := &protocols.IRRequest{
		Model:  model,
		Stream: protocols.IRStreamConfig{Enabled: isStream},
		Generation: protocols.IRGenerationConfig{
			Temperature: req.Temperature,
			TopP:        req.TopP,
			MaxTokens:   req.MaxOutputTokens,
		},
		SourceProtocol: "responses",
	}

	// instructions → system
	if len(req.Instructions) > 0 {
		var instrStr string
		if json.Unmarshal(req.Instructions, &instrStr) == nil && instrStr != "" {
			irReq.System = instrStr
		}
	}

	// input → messages
	messages, systemFromInput, err := decodeResponsesInput(req.Input)
	if err != nil {
		return nil, err
	}
	irReq.Messages = messages
	if systemFromInput != "" {
		if irReq.System != "" {
			irReq.System = systemFromInput + "\n\n" + irReq.System
		} else {
			irReq.System = systemFromInput
		}
	}

	// tools
	if len(req.Tools) > 0 {
		tools, err := decodeResponsesTools(req.Tools)
		if err == nil {
			irReq.Tools = tools
		}
	}

	// tool_choice
	if len(req.ToolChoice) > 0 {
		irReq.ToolChoice, irReq.ToolChoiceName = decodeResponsesToolChoice(req.ToolChoice)
	}

	// reasoning
	if len(req.Reasoning) > 0 {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(req.Reasoning, &reasoning) == nil && reasoning.Effort != "" {
			irReq.Reasoning = protocols.IRReasoningConfig{
				Enabled: true,
				Effort:  reasoning.Effort,
			}
		}
	}

	return irReq, nil
}

func decodeResponsesInput(input json.RawMessage) ([]protocols.IRMessage, string, error) {
	if len(input) == 0 {
		return nil, "", fmt.Errorf("input is empty")
	}

	// Simple string input
	var inputStr string
	if json.Unmarshal(input, &inputStr) == nil {
		return []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: inputStr}}},
		}, "", nil
	}

	var items []struct {
		Type      string          `json:"type,omitempty"`
		Role      string          `json:"role,omitempty"`
		Content   json.RawMessage `json:"content,omitempty"`
		CallID    string          `json:"call_id,omitempty"`
		Name      string          `json:"name,omitempty"`
		Arguments string          `json:"arguments,omitempty"`
		Output    json.RawMessage `json:"output,omitempty"`
	}
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, "", fmt.Errorf("parse responses input: %w", err)
	}

	var systemParts []string
	var messages []protocols.IRMessage

	for _, item := range items {
		switch {
		case item.Role == "system" || item.Role == "developer":
			text := extractTextFromResponsesContent(item.Content)
			if text != "" {
				systemParts = append(systemParts, text)
			}

		case item.Type == "function_call":
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			callID := item.CallID
			if callID == "" {
				callID = "call_" + protocols.RandomString(12)
			}
			// Merge into last assistant message if possible
			if len(messages) > 0 && messages[len(messages)-1].Role == protocols.RoleAssistant {
				last := &messages[len(messages)-1]
				last.ToolCalls = append(last.ToolCalls, protocols.IRToolCall{
					ID:        callID,
					Name:      item.Name,
					Arguments: args,
				})
			} else {
				messages = append(messages, protocols.IRMessage{
					Role: protocols.RoleAssistant,
					ToolCalls: []protocols.IRToolCall{
						{ID: callID, Name: item.Name, Arguments: args},
					},
				})
			}

		case item.Type == "function_call_output":
			content := decodeResponsesToolOutput(item.Output)
			// A tool that returned nothing still needs a visible result, or the
			// model is left guessing whether the call ran at all.
			if isBlankJSON(content) {
				content = json.RawMessage(`"(empty)"`)
			}
			messages = append(messages, protocols.IRMessage{
				Role:       protocols.RoleTool,
				ToolCallID: item.CallID,
				Content: []protocols.IRContentBlock{
					protocols.BlockToolResult{
						ToolUseID: item.CallID,
						Content:   content,
					},
				},
			})

		case item.Role == "user":
			text := extractTextFromResponsesContent(item.Content)
			messages = append(messages, protocols.IRMessage{
				Role:    protocols.RoleUser,
				Content: []protocols.IRContentBlock{protocols.BlockText{Text: text}},
			})

		case item.Role == "assistant":
			text := extractTextFromResponsesContent(item.Content)
			messages = append(messages, protocols.IRMessage{
				Role:    protocols.RoleAssistant,
				Content: []protocols.IRContentBlock{protocols.BlockText{Text: text}},
			})

		default:
			if len(item.Content) > 0 {
				text := extractTextFromResponsesContent(item.Content)
				if text != "" {
					messages = append(messages, protocols.IRMessage{
						Role:    protocols.RoleUser,
						Content: []protocols.IRContentBlock{protocols.BlockText{Text: text}},
					})
				}
			}
		}
	}

	var system string
	if len(systemParts) > 0 {
		system = strings.Join(systemParts, "\n\n")
	}
	return messages, system, nil
}

// responsesContentPart is one element of the content-part list form shared by a
// message's `content` and a function_call_output's `output`.
type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// decodeResponsesContentParts decodes the content-part list form. ok is false
// when the value is not a list of parts at all, leaving each caller to decide
// what that means.
func decodeResponsesContentParts(raw json.RawMessage) (parts []responsesContentPart, ok bool) {
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	return parts, true
}

// jsonValueKind returns the first meaningful byte of a JSON value — '"' for a
// string, '[' for an array — or 0 when there is nothing to read. Dispatching on
// it beats unmarshalling speculatively: every failed attempt re-validates the
// whole value, and a tool output carrying a screenshot is hundreds of kilobytes
// to rescan.
func jsonValueKind(raw json.RawMessage) byte {
	for _, b := range raw {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}

// isBlankJSON reports whether a JSON value carries nothing a model could read.
func isBlankJSON(raw json.RawMessage) bool {
	switch string(raw) {
	case "", `""`, "null":
		return true
	}
	return false
}

// decodeResponsesToolOutput normalizes a function_call_output's `output` into
// the JSON value a tool result carries downstream. The field is either a string
// or a list of output content parts (text, image, file), so reading it as a
// string alone rejected any request whose tool returned a screenshot — agent
// clients return them routinely.
//
// A string, and any structured value the spec does not describe, keeps its
// original bytes: re-decoding would copy a payload that reaches hundreds of
// kilobytes for nothing. Image and file parts collapse into a short marker,
// since no protocol this converts to can carry one inside a tool result and
// their data URLs would otherwise be spent as unreadable prompt.
func decodeResponsesToolOutput(raw json.RawMessage) json.RawMessage {
	if jsonValueKind(raw) == '[' {
		if parts, ok := decodeResponsesContentParts(raw); ok {
			var out []string
			for _, p := range parts {
				switch {
				case p.Text != "":
					out = append(out, p.Text)
				case p.Type == "input_image":
					out = append(out, "[image output omitted]")
				case p.Type == "input_file":
					out = append(out, "[file output omitted]")
				}
			}
			if flattened, err := json.Marshal(strings.Join(out, "\n")); err == nil {
				return flattened
			}
		}
	}
	return raw
}

func extractTextFromResponsesContent(raw json.RawMessage) string {
	switch jsonValueKind(raw) {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	case '[':
		parts, _ := decodeResponsesContentParts(raw)
		var texts []string
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "")
	}
	return ""
}

func decodeResponsesTools(raw json.RawMessage) ([]protocols.IRToolSpec, error) {
	var tools []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name,omitempty"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	}
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, err
	}
	var out []protocols.IRToolSpec
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		params := t.Parameters
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, protocols.IRToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		})
	}
	return out, nil
}

func decodeResponsesToolChoice(raw json.RawMessage) (protocols.IRToolChoice, string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return protocols.ToolChoiceAuto, ""
		case "required":
			return protocols.ToolChoiceRequired, ""
		case "none":
			return protocols.ToolChoiceNone, ""
		default:
			return protocols.ToolChoiceAuto, ""
		}
	}

	var m struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Type == "function" {
		name := m.Name
		if name == "" {
			name = m.Function.Name
		}
		if name != "" {
			return protocols.ToolChoiceNamed, name
		}
	}
	return protocols.ToolChoiceAuto, ""
}

// --- Responses Response Encoder ---

// ResponseEncoder encodes IR responses into OpenAI Responses API format.
type ResponseEncoder struct{}

func (ResponseEncoder) EncodeResponse(resp *protocols.IRResponse) json.RawMessage {
	var outputs []interface{}

	// Output-item status, shared by the message and tool-call items below. A
	// truncated or filtered run may have produced partial text or unparseable
	// arguments; marking such items "completed" tells a client reading
	// output items that they are final and safe to use, while the response-level
	// status says "incomplete" — self-contradictory frames.
	itemStatus := "completed"
	if protocols.IRStopReasonIsAbnormal(resp.StopReason) {
		itemStatus = "incomplete"
	}

	// Reasoning content
	if resp.ReasoningContent != "" {
		outputs = append(outputs, map[string]interface{}{
			"type": "reasoning",
			"id":   generateResponsesItemID(),
			"summary": []interface{}{
				map[string]interface{}{"type": "summary_text", "text": resp.ReasoningContent},
			},
		})
	}

	// Text content
	if resp.Content != "" {
		outputs = append(outputs, map[string]interface{}{
			"type":    "message",
			"id":      generateResponsesMessageID(),
			"role":    "assistant",
			"content": makeResponsesContentParts(resp.Content),
			"status":  itemStatus,
		})
	}

	// Tool calls
	for _, tc := range resp.ToolCalls {
		args := tc.Arguments
		if args == "" {
			args = "{}"
		}
		callID := tc.ID
		if callID == "" {
			callID = "call_" + protocols.RandomString(24)
		}
		outputs = append(outputs, map[string]interface{}{
			"type":      "function_call",
			"id":        generateResponsesItemID(),
			"call_id":   callID,
			"name":      tc.Name,
			"arguments": args,
			"status":    itemStatus,
		})
	}

	// Empty fallback
	if len(outputs) == 0 {
		outputs = append(outputs, map[string]interface{}{
			"type":    "message",
			"id":      generateResponsesMessageID(),
			"role":    "assistant",
			"content": makeResponsesContentParts(""),
			// Same itemStatus as the real items: this branch is exactly the one
			// that fires when a run was content-filtered or truncated before any
			// visible token, so it is the ONLY output item — claiming
			// "completed" next to a top-level "incomplete" is the contradiction
			// this change exists to remove.
			"status": itemStatus,
		})
	}

	status := "completed"
	var incompleteDetails interface{}
	// Mirrors makeCompleted: both truncation and content filtering are
	// "incomplete" terminations, not clean finishes.
	if protocols.IRStopReasonIsAbnormal(resp.StopReason) {
		status = "incomplete"
		reason := "content_filter"
		if resp.StopReason == "length" {
			reason = "max_output_tokens"
		}
		incompleteDetails = map[string]interface{}{"reason": reason}
	}

	usage := responsesWireUsage(resp.Usage)

	result := map[string]interface{}{
		"id":                 resp.ID,
		"object":             "response",
		"model":              resp.Model,
		"status":             status,
		"output":             outputs,
		"usage":              usage,
		"incomplete_details": incompleteDetails,
	}

	data, _ := json.Marshal(result)
	return data
}

func makeResponsesContentParts(text string) []interface{} {
	return []interface{}{
		map[string]interface{}{
			"type":        "output_text",
			"text":        text,
			"annotations": []interface{}{},
		},
	}
}

// --- Responses Stream Encoder ---

type responsesToolState struct {
	outputIdx int
	itemID    string
	callID    string
	name      string
	args      string
}

// StreamEncoder encodes IR deltas into Responses API SSE events.
type StreamEncoder struct {
	responseID        string
	model             string
	seqNum            int
	usage             protocols.IRUsage
	createdSent       bool
	completed         bool
	pendingStopReason string // stop reason received from DeltaDone, emitted only at EncodeDone via response.completed
	hasStop           bool   // whether a DeltaDone was received (distinguishes a legitimate empty stopReason from none at all)
	outputIndex       int

	// Message state
	messageAdded    bool
	messageItemID   string
	accumulatedText string

	// Reasoning state
	reasoningAdded  bool
	reasoningItemID string

	// Tool call state (keyed by IR index)
	toolStates map[int]*responsesToolState
}

func NewStreamEncoder() *StreamEncoder {
	return &StreamEncoder{
		toolStates: make(map[int]*responsesToolState),
	}
}

func (e *StreamEncoder) EncodeDeltas(deltas []protocols.IRStreamDelta) []protocols.SSEEvent {
	var events []protocols.SSEEvent

	for _, delta := range deltas {
		switch d := delta.(type) {
		case protocols.DeltaMessageStart:
			e.model = d.Model
			e.responseID = d.ID
			if e.responseID == "" {
				e.responseID = generateResponsesResponseID()
			}

		case protocols.DeltaText:
			events = append(events, e.ensureCreated()...)
			events = append(events, e.ensureMessage()...)
			e.accumulatedText += d.Text
			events = append(events, e.makeEvent("response.output_text.delta", map[string]interface{}{
				"output_index":  e.outputIndex,
				"content_index": 0,
				"delta":         d.Text,
				"item_id":       e.messageItemID,
			}))

		case protocols.DeltaThinking:
			events = append(events, e.ensureCreated()...)
			events = append(events, e.closeMessage("")...)
			if !e.reasoningAdded {
				e.reasoningAdded = true
				e.reasoningItemID = generateResponsesItemID()
				events = append(events, e.makeEvent("response.output_item.added", map[string]interface{}{
					"output_index": e.outputIndex,
					"item": map[string]interface{}{
						"type": "reasoning",
						"id":   e.reasoningItemID,
					},
				}))
			}
			events = append(events, e.makeEvent("response.reasoning_summary_text.delta", map[string]interface{}{
				"output_index":  e.outputIndex,
				"summary_index": 0,
				"delta":         d.Text,
				"item_id":       e.reasoningItemID,
			}))

		case protocols.DeltaToolCallStart:
			events = append(events, e.ensureCreated()...)
			events = append(events, e.closeReasoning()...)
			events = append(events, e.closeMessage("")...)
			toolItemID := generateResponsesItemID()
			callID := d.ID
			if callID == "" {
				callID = "call_" + protocols.RandomString(24)
			}
			idx := e.outputIndex
			e.outputIndex++
			e.toolStates[d.Index] = &responsesToolState{
				outputIdx: idx,
				itemID:    toolItemID,
				callID:    callID,
				name:      d.Name,
			}
			events = append(events, e.makeEvent("response.output_item.added", map[string]interface{}{
				"output_index": idx,
				"item": map[string]interface{}{
					"type":    "function_call",
					"id":      toolItemID,
					"call_id": callID,
					"name":    d.Name,
					"status":  "in_progress",
				},
			}))

		case protocols.DeltaToolCallArgs:
			ts, ok := e.toolStates[d.Index]
			if !ok {
				continue
			}
			ts.args += d.Arguments
			events = append(events, e.makeEvent("response.function_call_arguments.delta", map[string]interface{}{
				"output_index": ts.outputIdx,
				"delta":        d.Arguments,
				"item_id":      ts.itemID,
				"call_id":      ts.callID,
				"name":         ts.name,
			}))

		case protocols.DeltaUsage:
			// Field-level merge (IRUsage.Merge overwrites only non-zero fields): an upstream may send
			// partial usage chunks across multiple frames (e.g. some azure / litellm deployments split
			// prompt usage and completion usage into two frames on reasoning models). A whole-struct
			// last-wins assignment would let a later completion-only chunk clobber the collected
			// PromptTokens / cache fields to 0, under-reporting input_tokens in both the client SSE
			// and the billing layer.
			e.usage.Merge(d.Usage)

		case protocols.DeltaDone:
			// Intermediate block terminations (output_item.done etc.) are emitted immediately —
			// they do not affect the client. response.completed is deferred to EncodeDone: when a
			// chat upstream carries finish_reason + usage in one frame, the decoder emits DeltaDone
			// before DeltaUsage, so calling makeCompleted immediately would fill
			// response.completed.usage with zeros and the client would not read the real tokens.
			events = append(events, e.closeAllTools(d.StopReason)...)
			events = append(events, e.closeReasoning()...)
			events = append(events, e.closeMessage(d.StopReason)...)
			e.pendingStopReason = d.StopReason
			e.hasStop = true

		case protocols.DeltaUnknown:
			var value json.RawMessage
			if json.Unmarshal(d.Raw, &value) == nil {
				events = append(events, protocols.SSEEvent{Data: string(value)})
			}
		}
	}

	return events
}

func (e *StreamEncoder) EncodeDone() []protocols.SSEEvent {
	// Only called when the caller considers the stream truly complete (finishErr == nil).
	// By then every DeltaUsage has been applied to e.usage, so response.completed usage is
	// accurate.
	if e.completed || !e.hasStop {
		return nil
	}
	e.completed = true
	return []protocols.SSEEvent{e.makeCompleted(e.pendingStopReason)}
}

func (e *StreamEncoder) Usage() protocols.IRUsage {
	return e.usage
}

func (e *StreamEncoder) ensureCreated() []protocols.SSEEvent {
	if e.createdSent {
		return nil
	}
	e.createdSent = true
	respObj := map[string]interface{}{
		"id":     e.responseID,
		"object": "response",
		"model":  e.model,
		"status": "in_progress",
		"output": []interface{}{},
	}
	return []protocols.SSEEvent{
		e.makeEvent("response.created", map[string]interface{}{"response": respObj}),
		e.makeEvent("response.in_progress", map[string]interface{}{"response": respObj}),
	}
}

func (e *StreamEncoder) ensureMessage() []protocols.SSEEvent {
	if e.messageAdded {
		return nil
	}
	e.messageAdded = true
	e.messageItemID = generateResponsesMessageID()
	e.accumulatedText = ""
	idx := e.outputIndex
	return []protocols.SSEEvent{
		e.makeEvent("response.output_item.added", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"type":   "message",
				"id":     e.messageItemID,
				"role":   "assistant",
				"status": "in_progress",
				// Empty on purpose: OutputMessageContent only admits
				// output_text | refusal, and "input_text" is a REQUEST-side part
				// type — emitting it here makes a strict SDK reject the response
				// at the very first output item. The official streaming example
				// opens the item with no content and fills it via the
				// response.content_part.added event emitted right below.
				"content": []interface{}{},
			},
		}),
		e.makeEvent("response.content_part.added", map[string]interface{}{
			"item_id":       e.messageItemID,
			"output_index":  idx,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": ""},
		}),
	}
}

// closeMessage finalises the assistant message item.
//
// stopReason decides the item status for the same reason closeAllTools does:
// a client that reconstructs the answer from response.output_item.done (the
// documented way to read final items) would otherwise accept a half-written
// message as complete, because the trailing response.incomplete arrives after
// it. Flipping only the response-level status and the tool item — as an earlier
// pass did — leaves self-contradictory frames on the wire.
func (e *StreamEncoder) closeMessage(stopReason string) []protocols.SSEEvent {
	if !e.messageAdded {
		return nil
	}
	itemStatus := "completed"
	if protocols.IRStopReasonIsAbnormal(stopReason) {
		itemStatus = "incomplete"
	}
	idx := e.outputIndex
	id := e.messageItemID
	text := e.accumulatedText
	e.messageAdded = false
	e.messageItemID = ""
	e.accumulatedText = ""
	e.outputIndex++
	return []protocols.SSEEvent{
		e.makeEvent("response.output_text.done", map[string]interface{}{
			"output_index":  idx,
			"content_index": 0,
			"item_id":       id,
			"text":          text,
		}),
		e.makeEvent("response.content_part.done", map[string]interface{}{
			"item_id":       id,
			"output_index":  idx,
			"content_index": 0,
			"part":          map[string]interface{}{"type": "output_text", "text": text},
		}),
		e.makeEvent("response.output_item.done", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"type":    "message",
				"id":      id,
				"role":    "assistant",
				"status":  itemStatus,
				"content": makeResponsesContentParts(text),
			},
		}),
	}
}

func (e *StreamEncoder) closeReasoning() []protocols.SSEEvent {
	if !e.reasoningAdded {
		return nil
	}
	idx := e.outputIndex
	id := e.reasoningItemID
	e.reasoningAdded = false
	e.reasoningItemID = ""
	e.outputIndex++
	return []protocols.SSEEvent{
		e.makeEvent("response.reasoning_summary_text.done", map[string]interface{}{
			"output_index":  idx,
			"summary_index": 0,
			"item_id":       id,
		}),
		e.makeEvent("response.output_item.done", map[string]interface{}{
			"output_index": idx,
			"item": map[string]interface{}{
				"type": "reasoning",
				"id":   id,
			},
		}),
	}
}

// closeAllTools finalises the open function-call items.
//
// stopReason decides how: response.function_call_arguments.done is defined by
// the schema as "arguments finalized", and output_item status "completed" says
// the call is ready to execute. Emitting either after a truncated or filtered
// run tells the client that partial — possibly unparseable — arguments are
// complete, and it can act on them before the trailing response.incomplete
// event even arrives. On an abnormal stop we therefore skip the .done event
// entirely and mark the item "incomplete".
func (e *StreamEncoder) closeAllTools(stopReason string) []protocols.SSEEvent {
	if len(e.toolStates) == 0 {
		return nil
	}
	itemStatus := "completed"
	if protocols.IRStopReasonIsAbnormal(stopReason) {
		itemStatus = "incomplete"
	}
	// Deterministic order: toolStates is a map, and Go randomises map iteration.
	// That was survivable while the events carried no ordering field, but
	// sequence_number (added in makeEvent) is defined by the schema as the
	// authoritative event order — an SDK reassembling by it would otherwise see
	// output_index 1 finalised before 0, differently on every run.
	states := make([]*responsesToolState, 0, len(e.toolStates))
	for _, ts := range e.toolStates {
		states = append(states, ts)
	}
	// Sort by outputIdx — the field actually emitted as output_index — not by
	// the map key (the upstream's IR tool index). The two are only coincidentally
	// equal: the chat decoder passes tool_calls[].index through verbatim, so a
	// gateway that opens index 2 before index 1 would otherwise finalise the
	// items in reverse output_index order, and sequence_number now makes that
	// ordering authoritative.
	sort.Slice(states, func(i, j int) bool { return states[i].outputIdx < states[j].outputIdx })

	var events []protocols.SSEEvent
	for _, ts := range states {
		// The .done event is emitted even on an abnormal stop. Suppressing it is
		// a DIFFERENT contract from marking the item incomplete: every Responses
		// consumer pairs .delta with .done to close a per-item argument buffer,
		// so withholding it leaves that buffer open — the client either hangs on
		// the item or silently drops arguments the model did partially emit.
		// item status "incomplete" (below) is the schema-sanctioned way to say
		// "these arguments are not final", and it is already applied.
		events = append(events,
			e.makeEvent("response.function_call_arguments.done", map[string]interface{}{
				"output_index": ts.outputIdx,
				"arguments":    ts.args,
				"item_id":      ts.itemID,
				"call_id":      ts.callID,
				"name":         ts.name,
			}))
		events = append(events,
			e.makeEvent("response.output_item.done", map[string]interface{}{
				"output_index": ts.outputIdx,
				"item": map[string]interface{}{
					"type":      "function_call",
					"id":        ts.itemID,
					"call_id":   ts.callID,
					"name":      ts.name,
					"arguments": ts.args,
					"status":    itemStatus,
				},
			}),
		)
	}
	e.toolStates = make(map[int]*responsesToolState)
	return events
}

// responsesWireUsage builds the Responses API usage object shared by the
// non-streaming encoder and the streaming response.completed event.
//
// input_tokens is the GROSS input (cache included): the official ResponseUsage
// schema documents input_tokens_details as "a detailed breakdown of the input
// tokens", making cached_tokens a strict subset of input_tokens. Emitting the
// IR's raw PromptTokens would drop the whole cache portion for Anthropic
// upstreams, whose input count is net.
//
// The Responses usage schema has no cache-WRITE field of its own, so
// CacheWriteTokens is carried in the non-standard
// protocols.CacheWriteAliasField below.
func responsesWireUsage(u protocols.IRUsage) map[string]interface{} {
	// A record the gateway itself refused publishes nothing: emitting sanitized
	// counts would hand the client — and any downstream gateway billing from
	// them — numbers we already decided were impossible. null is the wire's
	// existing word for "unknown", and unknown is not zero.
	// Invalid alone: the verdict is settled once at the decoder exit (see
	// IRUsage.IsIncoherent), so this reads the same answer the billing gate
	// reads, instead of re-judging with a narrower predicate and disagreeing.
	if u.Invalid {
		return nil
	}
	// input_tokens_details and output_tokens_details are both in the schema's
	// required list (as are their cached_tokens / reasoning_tokens members), so
	// they are emitted unconditionally rather than only when non-zero — a
	// strict-validating downstream would otherwise reject the response. An
	// earlier revision emitted the write only when non-zero, on the mistaken
	// belief that it was our own extension rather than OpenAI's field.
	inputDetails := map[string]interface{}{
		"cached_tokens":                 u.CacheReadTokens,
		protocols.CacheWriteDetailField: u.CacheWriteTokens,
	}
	return map[string]interface{}{
		"input_tokens":         u.GrossPromptTokens(),
		"output_tokens":        u.CompletionTokens,
		"total_tokens":         u.GrossTotalTokens(),
		"input_tokens_details": inputDetails,
		"output_tokens_details": map[string]interface{}{
			"reasoning_tokens": u.ReasoningTokens,
		},
	}
}

func (e *StreamEncoder) makeCompleted(stopReason string) protocols.SSEEvent {
	status := "completed"
	// The Responses API defines response.incomplete as its own terminal event,
	// not a variant of response.completed. Setting only the nested status while
	// still labelling the event "response.completed" lets any SDK that switches
	// on the event discriminant treat a max-output truncation as a clean finish
	// — exactly the "truncation looks successful" failure this work set out to
	// remove elsewhere.
	eventType := "response.completed"
	var details interface{}
	// content_filter is just as much an "incomplete" termination as truncation;
	// encoding a safety block as response.completed told the client the answer
	// finished cleanly.
	if protocols.IRStopReasonIsAbnormal(stopReason) {
		status = "incomplete"
		eventType = "response.incomplete"
		reason := "content_filter"
		if stopReason == "length" {
			reason = "max_output_tokens"
		}
		// incomplete_details.reason only allows max_output_tokens |
		// content_filter, so anything abnormal that is not truncation is
		// reported as content_filter — claiming a clean completion for an
		// abnormal run is the one thing we must not do.
		details = map[string]interface{}{"reason": reason}
	}
	usage := responsesWireUsage(e.usage)
	return e.makeEvent(eventType, map[string]interface{}{
		"response": map[string]interface{}{
			"id":                 e.responseID,
			"object":             "response",
			"model":              e.model,
			"status":             status,
			"output":             []interface{}{},
			"usage":              usage,
			"incomplete_details": details,
		},
	})
}

func (e *StreamEncoder) makeEvent(eventType string, data map[string]interface{}) protocols.SSEEvent {
	data["type"] = eventType
	// sequence_number is in the required list of every Responses stream event in
	// the official schema. The counter was being incremented but never written,
	// so a strict SDK would reject the whole stream.
	data["sequence_number"] = e.seqNum
	e.seqNum++
	d, _ := json.Marshal(data)
	return protocols.SSEEvent{Data: string(d)}
}

// generateResponsesResponseID and its siblings are the ID generators for
// the Responses API wire shape.
func generateResponsesResponseID() string { return "resp_" + protocols.RandomString(32) }
func generateResponsesItemID() string     { return "rs_" + protocols.RandomString(24) }
func generateResponsesMessageID() string  { return "msg_" + protocols.RandomString(24) }
