package images

// The named-event SSE vocabulary the OpenAI Images API streams in when a
// generation or edit request asks for stream=true. The protocol frames each
// event as "event: <name>" plus a data line whose JSON repeats the type in a
// "type" field; the JSON is the authoritative copy, so this package parses
// the data payload and never keys on the SSE header.

import (
	"encoding/json"
	"fmt"
)

// Stream event names, both halves of the API.
const (
	EventGenerationPartial   = "image_generation.partial_image"
	EventGenerationCompleted = "image_generation.completed"
	EventEditPartial         = "image_edit.partial_image"
	EventEditCompleted       = "image_edit.completed"
	EventError               = "error"
)

// StreamUsage is the token accounting a completed event carries. It arrives
// only on completed events, and only the last one's numbers are kept: the
// API has not declared whether n>1 streams repeat or accumulate them.
type StreamUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// StreamEvent is one parsed data payload: which event it was, and the usage
// when the event carried one.
type StreamEvent struct {
	Type  string       `json:"type"`
	Usage *StreamUsage `json:"usage,omitempty"`
}

// ParseStreamEvent decodes one SSE data payload into its event. A payload
// that is not JSON this vocabulary lives inside is an error, not a silent
// skip: the caller distinguishes "nothing to parse" (a comment or an event
// header line, which never reaches this function) from "something that
// claimed to be an event and was not".
func ParseStreamEvent(data string) (*StreamEvent, error) {
	var ev StreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, fmt.Errorf("image stream event decode: %w", err)
	}
	if ev.Type == "" {
		return nil, fmt.Errorf("image stream event without a type")
	}
	return &ev, nil
}
