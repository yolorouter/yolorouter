package claude

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// RequestEncoder encodes IR into Anthropic Claude Messages API request format.
type RequestEncoder struct{}

func (RequestEncoder) Protocol() protocols.ProtocolID { return protocols.ProtocolClaude }

func (RequestEncoder) EgressPath(_ string, _ bool) string {
	return "/v1/messages"
}

func (RequestEncoder) SetupRequest(req *http.Request, apiKey string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
}

func (RequestEncoder) EncodeRequest(irReq *protocols.IRRequest) (json.RawMessage, error) {
	req := map[string]interface{}{
		"model":      irReq.Model,
		"max_tokens": 4096,
	}

	// System
	if irReq.System != "" {
		req["system"] = irReq.System
	}

	// Messages
	claudeMsgs := encodeClaudeMessages(irReq.Messages)
	if len(claudeMsgs) > 0 {
		req["messages"] = claudeMsgs
	}

	// Generation config
	if irReq.Generation.MaxTokens != nil && *irReq.Generation.MaxTokens > 0 {
		req["max_tokens"] = *irReq.Generation.MaxTokens
	}
	if irReq.Generation.Temperature != nil {
		req["temperature"] = *irReq.Generation.Temperature
	}
	if irReq.Generation.TopP != nil {
		req["top_p"] = *irReq.Generation.TopP
	}
	if irReq.Generation.TopK != nil {
		req["top_k"] = *irReq.Generation.TopK
	}
	if len(irReq.Generation.StopSequences) > 0 {
		req["stop_sequences"] = irReq.Generation.StopSequences
	}

	// Stream
	if irReq.Stream.Enabled {
		req["stream"] = true
	}

	// Thinking
	if irReq.Reasoning.Enabled {
		maxTokens := 4096
		if v, ok := req["max_tokens"].(int); ok {
			maxTokens = v
		}
		switch {
		case isAdaptiveModel(irReq.Model):
			applyAdaptiveThinking(req, irReq.Reasoning.Effort)
			// Anthropic requires temperature=1 and top_p unset for every
			// thinking mode, including adaptive.
			req["temperature"] = 1.0
			delete(req, "top_p")
		case maxTokens <= legacyThinkingMinBudget:
			// Anthropic's legacy thinking needs budget_tokens >=
			// legacyThinkingMinBudget AND max_tokens > budget_tokens: with
			// max_tokens at or under the floor no budget satisfies both.
			// Respecting the caller's output cap wins: send the request
			// without thinking and leave the caller's sampling parameters
			// untouched.
		default:
			budget := 4096
			if irReq.Reasoning.BudgetTokens != nil {
				budget = *irReq.Reasoning.BudgetTokens
			}
			req["thinking"] = map[string]interface{}{
				"type":          "enabled",
				"budget_tokens": clampLegacyThinkingBudget(budget, maxTokens),
			}
			// When thinking is enabled, temperature must be 1 and top_p must be unset
			req["temperature"] = 1.0
			delete(req, "top_p")
		}
	}

	// Tools
	if len(irReq.Tools) > 0 {
		tools := make([]interface{}, 0, len(irReq.Tools))
		for _, t := range irReq.Tools {
			tool := map[string]interface{}{
				"name": t.Name,
			}
			if t.Description != "" {
				tool["description"] = t.Description
			}
			if len(t.Parameters) > 0 {
				tool["input_schema"] = json.RawMessage(t.Parameters)
			}
			tools = append(tools, tool)
		}
		req["tools"] = tools
	}

	// Tool choice
	switch irReq.ToolChoice {
	case protocols.ToolChoiceNone:
		req["tool_choice"] = map[string]interface{}{"type": "none"}
	case protocols.ToolChoiceRequired:
		req["tool_choice"] = map[string]interface{}{"type": "any"}
	case protocols.ToolChoiceNamed:
		req["tool_choice"] = map[string]interface{}{
			"type": "tool",
			"name": irReq.ToolChoiceName,
		}
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal claude request: %w", err)
	}
	return data, nil
}

func encodeClaudeMessages(messages []protocols.IRMessage) []interface{} {
	var result []interface{}

	i := 0
	for i < len(messages) {
		msg := messages[i]
		switch msg.Role {
		case protocols.RoleSystem:
			i++
		case protocols.RoleUser:
			result = append(result, encodeClaudeUserMessage(msg))
			i++
		case protocols.RoleAssistant:
			result = append(result, encodeClaudeAssistantMessage(msg))
			i++
		case protocols.RoleTool:
			// Anthropic requires all tool_result blocks for a turn to live in a
			// single user message. Merge consecutive RoleTool messages into one
			// user message with multiple tool_result blocks.
			var blocks []interface{}
			for i < len(messages) && messages[i].Role == protocols.RoleTool {
				blocks = append(blocks, encodeClaudeToolResultBlocks(messages[i])...)
				i++
			}
			result = append(result, map[string]interface{}{
				"role":    "user",
				"content": blocks,
			})
		default:
			i++
		}
	}

	return result
}

func encodeClaudeUserMessage(msg protocols.IRMessage) interface{} {
	var blocks []interface{}
	for _, b := range msg.Content {
		switch v := b.(type) {
		case protocols.BlockText:
			blocks = append(blocks, map[string]interface{}{
				"type": "text",
				"text": v.Text,
			})
		case protocols.BlockImage:
			if v.IsURL {
				blocks = append(blocks, map[string]interface{}{
					"type": "image",
					"source": map[string]interface{}{
						"type": "url",
						"url":  v.Data,
					},
				})
			} else {
				blocks = append(blocks, map[string]interface{}{
					"type": "image",
					"source": map[string]interface{}{
						"type":       "base64",
						"media_type": v.MediaType,
						"data":       v.Data,
					},
				})
			}
		case protocols.BlockToolResult:
			blocks = append(blocks, encodeOneToolResultBlock(v))
		}
	}

	if len(blocks) == 1 {
		if bt, ok := blocks[0].(map[string]interface{}); ok && bt["type"] == "text" {
			return map[string]interface{}{
				"role":    "user",
				"content": bt["text"],
			}
		}
	}

	if len(blocks) == 0 {
		return map[string]interface{}{
			"role":    "user",
			"content": "...",
		}
	}

	return map[string]interface{}{
		"role":    "user",
		"content": blocks,
	}
}

func encodeClaudeAssistantMessage(msg protocols.IRMessage) interface{} {
	var blocks []interface{}

	for _, b := range msg.Content {
		switch v := b.(type) {
		case protocols.BlockText:
			if v.Text != "" {
				blocks = append(blocks, map[string]interface{}{
					"type": "text",
					"text": v.Text,
				})
			}
		case protocols.BlockThinking:
			blocks = append(blocks, map[string]interface{}{
				"type":     "thinking",
				"thinking": v.Thinking,
			})
		case protocols.BlockToolUse:
			block := map[string]interface{}{
				"type":  "tool_use",
				"id":    v.ID,
				"name":  v.Name,
				"input": json.RawMessage(v.Input),
			}
			blocks = append(blocks, block)
		}
	}

	for _, tc := range msg.ToolCalls {
		var input json.RawMessage
		if tc.Arguments != "" {
			input = json.RawMessage(tc.Arguments)
		} else {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, map[string]interface{}{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": input,
		})
	}

	if len(blocks) == 0 {
		return map[string]interface{}{
			"role":    "assistant",
			"content": []interface{}{map[string]interface{}{"type": "text", "text": ""}},
		}
	}

	return map[string]interface{}{
		"role":    "assistant",
		"content": blocks,
	}
}

func encodeOneToolResultBlock(tr protocols.BlockToolResult) map[string]interface{} {
	block := map[string]interface{}{
		"type":        "tool_result",
		"tool_use_id": tr.ToolUseID,
	}
	if tr.IsError {
		block["is_error"] = true
	}
	if content := extractClaudeToolResultContent(tr.Content); content != nil {
		block["content"] = content
	}
	return block
}

func encodeClaudeToolResultBlocks(msg protocols.IRMessage) []interface{} {
	var blocks []interface{}
	for _, b := range msg.Content {
		if tr, ok := b.(protocols.BlockToolResult); ok {
			blocks = append(blocks, encodeOneToolResultBlock(tr))
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": msg.ToolCallID,
			"content":     "",
		})
	}

	return blocks
}

func extractClaudeToolResultContent(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []ToolResultSubBlock
	if json.Unmarshal(raw, &blocks) == nil && len(blocks) > 0 {
		var result []interface{}
		for _, b := range blocks {
			if b.Type == "text" {
				result = append(result, map[string]interface{}{
					"type": "text",
					"text": b.Text,
				})
			} else if b.Type == "image" && b.Source != nil {
				result = append(result, map[string]interface{}{
					"type":   "image",
					"source": b.Source,
				})
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	return string(raw)
}

type ToolResultSubBlock struct {
	Type   string          `json:"type"`
	Text   string          `json:"text,omitempty"`
	Source json.RawMessage `json:"source,omitempty"`
}
