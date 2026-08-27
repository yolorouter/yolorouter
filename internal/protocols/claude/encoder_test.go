package claude

import (
	"encoding/json"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
)

// --- Claude Request Encoder ---

func TestClaudeEncodeRequest_BasicChat(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model:  "claude-3-5-sonnet-20241022",
		System: "You are helpful.",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Hello"}}},
		},
		Stream: protocols.IRStreamConfig{Enabled: true},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if req["model"] != "claude-3-5-sonnet-20241022" {
		t.Errorf("model = %v", req["model"])
	}
	if req["system"] != "You are helpful." {
		t.Errorf("system = %v", req["system"])
	}
	if req["stream"] != true {
		t.Error("stream should be true")
	}
	if req["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens = %v, want 4096", req["max_tokens"])
	}

	msgs, ok := req["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not an array")
	}
	if len(msgs) != 1 {
		t.Errorf("messages count = %d, want 1", len(msgs))
	}
	msg := msgs[0].(map[string]interface{})
	if msg["role"] != "user" {
		t.Errorf("role = %q", msg["role"])
	}
}

func TestClaudeEncodeRequest_MaxTokensOverride(t *testing.T) {
	maxTokens := 8192
	irReq := &protocols.IRRequest{
		Model: "claude-3-opus",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Hi"}}},
		},
		Generation: protocols.IRGenerationConfig{MaxTokens: &maxTokens},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	if req["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v, want 8192", req["max_tokens"])
	}
}

func TestClaudeEncodeRequest_Thinking(t *testing.T) {
	budget := 10000
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Think hard"}}},
		},
		Generation: protocols.IRGenerationConfig{TopP: float64Ptr(0.9)},
		Reasoning:  protocols.IRReasoningConfig{Enabled: true, BudgetTokens: &budget},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	thinking, ok := req["thinking"].(map[string]interface{})
	if !ok {
		t.Fatal("thinking should be set")
	}
	if thinking["type"] != "enabled" {
		t.Errorf("thinking.type = %v", thinking["type"])
	}
	// No caller max_tokens, so max_tokens is the 4096 default; the requested
	// 10000 budget is clamped under it to max_tokens-1 (4095), matching the
	// reference optimizer's rule (respect the caller's max_tokens cap).
	if thinking["budget_tokens"] != float64(4095) {
		t.Errorf("budget_tokens = %v, want 4095 (clamped under the 4096 default max_tokens)", thinking["budget_tokens"])
	}
	if req["max_tokens"] != float64(4096) {
		t.Errorf("max_tokens must stay at the 4096 default (not raised), got %v", req["max_tokens"])
	}

	if req["temperature"] != 1.0 {
		t.Errorf("temperature should be forced to 1.0 when thinking enabled, got %v", req["temperature"])
	}
	if _, hasTopP := req["top_p"]; hasTopP {
		t.Error("top_p should be removed when thinking enabled")
	}
}

// TestClaudeEncodeRequest_ThinkingBudgetClampedUnderMaxTokens is a regression
// test: Anthropic requires max_tokens > thinking.budget_tokens, but max_tokens
// defaults to 4096 while a requested budget can be much larger (up to 80000
// for effort=high), and OptimizeBody (which would otherwise reconcile this)
// is not wired into dispatch in this version. The encoder enforces the
// invariant directly: clamp the thinking budget to max_tokens-1 (floor 1024),
// leaving the caller's max_tokens cap untouched. Covers: no caller max_tokens
// (4096 default), a budget below the 1024 floor, and explicit caller caps
// at/below the budget.
func TestClaudeEncodeRequest_ThinkingBudgetClampedUnderMaxTokens(t *testing.T) {
	cases := []struct {
		name         string
		budget       int
		callerMaxTok *int // nil = caller didn't send max_tokens (4096 default)
		wantBudget   int  // clamped budget_tokens the encoder must emit
		wantMaxTok   int  // max_tokens must stay at this value (never raised)
	}{
		{name: "no caller max_tokens, medium-effort budget", budget: 10000, callerMaxTok: nil, wantBudget: 4095, wantMaxTok: 4096},
		{name: "no caller max_tokens, high-effort budget", budget: 80000, callerMaxTok: nil, wantBudget: 4095, wantMaxTok: 4096},
		{name: "no caller max_tokens, sub-floor budget", budget: 1000, callerMaxTok: nil, wantBudget: 1024, wantMaxTok: 4096}, // floored up to 1024, still < 4096
		{name: "caller max_tokens smaller than budget", budget: 10000, callerMaxTok: intPtr(2000), wantBudget: 1999, wantMaxTok: 2000},
		{name: "caller max_tokens equal to budget", budget: 10000, callerMaxTok: intPtr(10000), wantBudget: 9999, wantMaxTok: 10000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			irReq := &protocols.IRRequest{
				Model: "claude-3-5-sonnet-20241022",
				Messages: []protocols.IRMessage{
					{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Think hard"}}},
				},
				Generation: protocols.IRGenerationConfig{MaxTokens: tc.callerMaxTok},
				Reasoning:  protocols.IRReasoningConfig{Enabled: true, BudgetTokens: &tc.budget},
			}

			enc := RequestEncoder{}
			data, err := enc.EncodeRequest(irReq)
			if err != nil {
				t.Fatalf("EncodeRequest: %v", err)
			}

			var req map[string]interface{}
			if err := json.Unmarshal(data, &req); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			thinking, ok := req["thinking"].(map[string]interface{})
			if !ok {
				t.Fatal("thinking should be set")
			}
			gotBudget, _ := thinking["budget_tokens"].(float64)
			if int(gotBudget) != tc.wantBudget {
				t.Fatalf("budget_tokens = %v, want %d (clamped under max_tokens)", thinking["budget_tokens"], tc.wantBudget)
			}
			gotMaxTok, ok := req["max_tokens"].(float64)
			if !ok {
				t.Fatal("max_tokens should be a number")
			}
			if int(gotMaxTok) != tc.wantMaxTok {
				t.Errorf("max_tokens = %v, want %d (caller cap must not be raised)", gotMaxTok, tc.wantMaxTok)
			}
			// The invariant that motivated the fix: budget must be < max_tokens.
			if int(gotBudget) >= int(gotMaxTok) {
				t.Errorf("budget_tokens %v must be strictly less than max_tokens %v", gotBudget, gotMaxTok)
			}
		})
	}
}

// TestClaudeEncodeRequest_ThinkingPreservesSufficientCallerMaxTokens verifies
// that when the caller's own max_tokens already comfortably exceeds the
// thinking budget, the encoder leaves it untouched instead of always forcing
// budget+headroom.
func TestClaudeEncodeRequest_ThinkingPreservesSufficientCallerMaxTokens(t *testing.T) {
	budget := 1000
	maxTokens := 20000
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Think a bit"}}},
		},
		Generation: protocols.IRGenerationConfig{MaxTokens: &maxTokens},
		Reasoning:  protocols.IRReasoningConfig{Enabled: true, BudgetTokens: &budget},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	if req["max_tokens"] != float64(20000) {
		t.Errorf("max_tokens = %v, want unchanged 20000 (already exceeds the budget)", req["max_tokens"])
	}
}

func TestClaudeEncodeRequest_Tools(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Weather?"}}},
		},
		Tools: []protocols.IRToolSpec{
			{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
			},
		},
		ToolChoice: protocols.ToolChoiceRequired,
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	tools, ok := req["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", req["tools"])
	}
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "get_weather" {
		t.Errorf("tool name = %v", tool["name"])
	}
	if tool["description"] != "Get weather" {
		t.Errorf("tool description = %v", tool["description"])
	}
	if tool["input_schema"] == nil {
		t.Error("input_schema should be set")
	}

	tc, ok := req["tool_choice"].(map[string]interface{})
	if !ok || tc["type"] != "any" {
		t.Errorf("tool_choice = %v", req["tool_choice"])
	}
}

func TestClaudeEncodeRequest_ToolResult(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Weather?"}}},
			{
				Role: protocols.RoleAssistant,
				ToolCalls: []protocols.IRToolCall{
					{ID: "toolu_abc", Name: "get_weather", Arguments: `{"city":"Beijing"}`},
				},
			},
			{
				Role: protocols.RoleTool,
				Content: []protocols.IRContentBlock{
					protocols.BlockToolResult{
						ToolUseID: "toolu_abc",
						Content:   json.RawMessage(`"Sunny, 25°C"`),
					},
				},
			},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs, ok := req["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not an array")
	}
	if len(msgs) != 3 {
		t.Fatalf("messages count = %d, want 3", len(msgs))
	}

	// Third message should be user role with tool_result block
	toolMsg := msgs[2].(map[string]interface{})
	if toolMsg["role"] != "user" {
		t.Errorf("tool result message role = %q, want 'user'", toolMsg["role"])
	}
	content, ok := toolMsg["content"].([]interface{})
	if !ok || len(content) != 1 {
		t.Fatalf("tool result content = %v", toolMsg["content"])
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "tool_result" {
		t.Errorf("block type = %q", block["type"])
	}
	if block["tool_use_id"] != "toolu_abc" {
		t.Errorf("tool_use_id = %v", block["tool_use_id"])
	}
}

func TestClaudeEncodeRequest_MultipleToolResultsMerged(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Use tools"}}},
			{
				Role: protocols.RoleAssistant,
				ToolCalls: []protocols.IRToolCall{
					{ID: "call_00", Name: "tool_a", Arguments: `{}`},
					{ID: "call_01", Name: "tool_b", Arguments: `{}`},
				},
			},
			{
				Role: protocols.RoleTool,
				Content: []protocols.IRContentBlock{
					protocols.BlockToolResult{ToolUseID: "call_00", Content: json.RawMessage(`"result_a"`)},
				},
			},
			{
				Role: protocols.RoleTool,
				Content: []protocols.IRContentBlock{
					protocols.BlockToolResult{ToolUseID: "call_01", Content: json.RawMessage(`"result_b"`)},
				},
			},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs, ok := req["messages"].([]interface{})
	if !ok {
		t.Fatal("messages is not an array")
	}
	// user, assistant, merged-tool-results — must be 3, not 4
	if len(msgs) != 3 {
		t.Fatalf("messages count = %d, want 3 (tool results must be merged)", len(msgs))
	}

	toolMsg := msgs[2].(map[string]interface{})
	if toolMsg["role"] != "user" {
		t.Errorf("merged tool message role = %q, want 'user'", toolMsg["role"])
	}
	content, ok := toolMsg["content"].([]interface{})
	if !ok || len(content) != 2 {
		t.Fatalf("merged tool content len = %d, want 2", len(content))
	}
	b0 := content[0].(map[string]interface{})
	b1 := content[1].(map[string]interface{})
	if b0["tool_use_id"] != "call_00" || b1["tool_use_id"] != "call_01" {
		t.Errorf("tool_use_ids = [%v %v], want [call_00 call_01]", b0["tool_use_id"], b1["tool_use_id"])
	}
	if b0["content"] != "result_a" || b1["content"] != "result_b" {
		t.Errorf("content values = [%v %v], want [result_a result_b]", b0["content"], b1["content"])
	}
}

func TestClaudeEncodeRequest_ToolResultIsError(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Run"}}},
			{
				Role:      protocols.RoleAssistant,
				ToolCalls: []protocols.IRToolCall{{ID: "call_err", Name: "risky", Arguments: `{}`}},
			},
			{
				Role: protocols.RoleTool,
				Content: []protocols.IRContentBlock{
					protocols.BlockToolResult{ToolUseID: "call_err", Content: json.RawMessage(`"timeout"`), IsError: true},
				},
			},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs := req["messages"].([]interface{})
	toolMsg := msgs[2].(map[string]interface{})
	blocks := toolMsg["content"].([]interface{})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	block := blocks[0].(map[string]interface{})
	if block["is_error"] != true {
		t.Errorf("is_error = %v, want true", block["is_error"])
	}
	if block["content"] != "timeout" {
		t.Errorf("content = %v, want 'timeout'", block["content"])
	}
}

func TestClaudeEncodeRequest_UserTextSimplified(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Hello"}}},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs := req["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})

	// Single text block should be simplified to string content
	if msg["content"] != "Hello" {
		t.Errorf("simple text should be string, got %T: %v", msg["content"], msg["content"])
	}
}

func TestClaudeEncodeRequest_ImageBase64(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{
				Role: protocols.RoleUser,
				Content: []protocols.IRContentBlock{
					protocols.BlockImage{MediaType: "image/png", Data: "base64data", IsURL: false},
				},
			},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs := req["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})
	content, _ := msg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content blocks = %d", len(content))
	}
	block := content[0].(map[string]interface{})
	source, ok := block["source"].(map[string]interface{})
	if !ok {
		t.Fatal("image should have source")
	}
	if source["type"] != "base64" {
		t.Errorf("source type = %v", source["type"])
	}
}

func TestClaudeEncodeRequest_ImageURL(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{
				Role: protocols.RoleUser,
				Content: []protocols.IRContentBlock{
					protocols.BlockImage{MediaType: "image/png", Data: "https://example.com/img.png", IsURL: true},
				},
			},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs := req["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})
	content, _ := msg["content"].([]interface{})
	block := content[0].(map[string]interface{})
	source := block["source"].(map[string]interface{})
	if source["type"] != "url" {
		t.Errorf("source type = %v", source["type"])
	}
	if source["url"] != "https://example.com/img.png" {
		t.Errorf("source url = %v", source["url"])
	}
}

func TestClaudeEncodeRequest_EmptyMessages(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: nil,
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	if _, ok := req["messages"]; ok {
		t.Error("empty messages should not be set")
	}
}

func TestClaudeEncodeRequest_NamedToolChoice(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Test"}}},
		},
		Tools: []protocols.IRToolSpec{
			{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice:     protocols.ToolChoiceNamed,
		ToolChoiceName: "get_weather",
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	tc := req["tool_choice"].(map[string]interface{})
	if tc["type"] != "tool" {
		t.Errorf("tool_choice type = %v", tc["type"])
	}
	if tc["name"] != "get_weather" {
		t.Errorf("tool_choice name = %v", tc["name"])
	}
}

func TestClaudeEncodeRequest_SystemSkippedInMessages(t *testing.T) {
	irReq := &protocols.IRRequest{
		Model:  "claude-3-5-sonnet-20241022",
		System: "System prompt",
		Messages: []protocols.IRMessage{
			{Role: protocols.RoleSystem, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Ignored"}}},
			{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Hello"}}},
		},
	}

	enc := RequestEncoder{}
	data, err := enc.EncodeRequest(irReq)
	if err != nil {
		t.Fatalf("EncodeRequest: %v", err)
	}

	var req map[string]interface{}
	_ = json.Unmarshal(data, &req)

	msgs := req["messages"].([]interface{})
	for _, m := range msgs {
		msg := m.(map[string]interface{})
		if msg["role"] == "system" {
			t.Error("system messages should be skipped in messages array")
		}
	}
}

// TestCrossProtocolCacheReadPassthrough is an end-to-end contract test for
// cross-protocol cache passthrough. A client hits Anthropic /v1/messages
// (ingress=anthropic); the relay routes to an OpenAI-compatible upstream
// (egress=openai) whose response carries usage.prompt_tokens_details.cached_tokens > 0;
// and the Anthropic-format response returned to the client must therefore
// include cache_read_input_tokens.
//
// This is a real-world case: when the upstream actually returns cache data, the
// relay must not drop the field. When the upstream returns none (cached_tokens=0
// or the field is missing), it must not synthesize a fake value — this is
// governed by omitempty.
func TestCrossProtocolCacheReadPassthrough(t *testing.T) {
	// Simulated real response bytes from an OpenAI-compatible upstream (with cached_tokens=80).
	upstreamResponse := []byte(`{
		"id": "chatcmpl-xyz",
		"model": "gpt-4o",
		"choices": [{
			"message": {"role": "assistant", "content": "Hi from upstream."},
			"finish_reason": "stop"
		}],
		"usage": {
			"prompt_tokens": 100,
			"completion_tokens": 20,
			"total_tokens": 120,
			"prompt_tokens_details": {"cached_tokens": 80}
		}
	}`)

	// Step 1: the chat decoder extracts cached_tokens → IR.CacheReadTokens.
	irResp, err := chat.ResponseDecoder{}.DecodeResponse(upstreamResponse)
	if err != nil {
		t.Fatalf("chat DecodeResponse: %v", err)
	}
	if irResp.Usage.CacheReadTokens != 80 {
		t.Fatalf("IR.CacheReadTokens = %d, want 80 (chat decoder did not extract cached_tokens)", irResp.Usage.CacheReadTokens)
	}

	// Step 2: the claude response encoder writes cache_read_input_tokens.
	clientBytes := ResponseEncoder{}.EncodeResponse(irResp)
	var clientResp map[string]any
	if err := json.Unmarshal(clientBytes, &clientResp); err != nil {
		t.Fatalf("client response unmarshal: %v", err)
	}
	usage, ok := clientResp["usage"].(map[string]any)
	if !ok {
		t.Fatalf("client usage is not an object, got=%T", clientResp["usage"])
	}
	cacheRead, ok := usage["cache_read_input_tokens"].(float64)
	if !ok {
		t.Fatalf("client response usage is missing the cache_read_input_tokens field, got=%v", usage)
	}
	if cacheRead != 80 {
		t.Errorf("cache_read_input_tokens = %v, want 80", cacheRead)
	}
}

// TestCrossProtocolNoCacheNoSynthesize is the reverse contract: when the upstream
// does not return cached_tokens, the client response must not contain
// cache_read_input_tokens (no fabricated data).
func TestCrossProtocolNoCacheNoSynthesize(t *testing.T) {
	upstreamResponse := []byte(`{
		"id": "chatcmpl-xyz",
		"model": "gpt-4o",
		"choices": [{
			"message": {"role": "assistant", "content": "Hi"},
			"finish_reason": "stop"
		}],
		"usage": {"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120}
	}`)
	irResp, err := chat.ResponseDecoder{}.DecodeResponse(upstreamResponse)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if irResp.Usage.CacheReadTokens != 0 {
		t.Errorf("IR.CacheReadTokens should be 0 when there are no cached_tokens, got %d", irResp.Usage.CacheReadTokens)
	}
	clientBytes := ResponseEncoder{}.EncodeResponse(irResp)
	var clientResp map[string]any
	_ = json.Unmarshal(clientBytes, &clientResp)
	usage, _ := clientResp["usage"].(map[string]any)
	if _, has := usage["cache_read_input_tokens"]; has {
		t.Error("when the upstream has no cached_tokens, the client response must not contain cache_read_input_tokens (avoid fabrication)")
	}
}

func float64Ptr(v float64) *float64 { return &v }

// TestClaudeEncodeRequest_ThinkingDroppedWhenCapCannotFitBudget: legacy
// thinking needs budget_tokens >= 1024 AND max_tokens > budget_tokens, so
// with max_tokens at or under 1024 no legal budget exists. The encoder must
// drop thinking and leave the caller's sampling parameters alone. Adaptive
// models carry no budget constraint and keep thinking in adaptive form.
func TestClaudeEncodeRequest_ThinkingDroppedWhenCapCannotFitBudget(t *testing.T) {
	budget := 2048
	temp := 0.5
	topP := 0.9
	encode := func(model string, maxTok int) map[string]interface{} {
		t.Helper()
		irReq := &protocols.IRRequest{
			Model: model,
			Messages: []protocols.IRMessage{
				{Role: protocols.RoleUser, Content: []protocols.IRContentBlock{protocols.BlockText{Text: "Think"}}},
			},
			Generation: protocols.IRGenerationConfig{MaxTokens: &maxTok, Temperature: &temp, TopP: &topP},
			Reasoning:  protocols.IRReasoningConfig{Enabled: true, BudgetTokens: &budget},
		}
		data, err := (RequestEncoder{}).EncodeRequest(irReq)
		if err != nil {
			t.Fatalf("EncodeRequest: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(data, &req); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		return req
	}

	for _, maxTok := range []int{512, 1024} {
		req := encode("claude-3-5-sonnet-20241022", maxTok)
		if _, has := req["thinking"]; has {
			t.Fatalf("max_tokens=%d: thinking = %v, want it dropped — no legal budget fits under this cap", maxTok, req["thinking"])
		}
		if got, _ := req["temperature"].(float64); got != 0.5 {
			t.Fatalf("max_tokens=%d: temperature = %v, the caller's sampling must stay untouched when thinking is dropped", maxTok, req["temperature"])
		}
		if got, _ := req["top_p"].(float64); got != 0.9 {
			t.Fatalf("max_tokens=%d: top_p = %v, the caller's sampling must stay untouched when thinking is dropped", maxTok, req["top_p"])
		}
	}

	// One past the boundary: the floored minimum budget fits.
	req := encode("claude-3-5-sonnet-20241022", 1025)
	thinking, ok := req["thinking"].(map[string]interface{})
	if !ok {
		t.Fatal("max_tokens=1025: thinking should be set")
	}
	if got, _ := thinking["budget_tokens"].(float64); int(got) != 1024 {
		t.Fatalf("max_tokens=1025: budget = %v, want the 1024 floor", thinking["budget_tokens"])
	}

	// Adaptive models have no budget constraint: thinking survives a small
	// max_tokens in adaptive form, which carries no budget_tokens at all.
	req = encode("claude-opus-4-8", 512)
	thinking, ok = req["thinking"].(map[string]interface{})
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("adaptive model: thinking = %v, want the adaptive form", req["thinking"])
	}
	if bt, has := thinking["budget_tokens"]; has {
		t.Fatalf("adaptive model: budget_tokens = %v, adaptive thinking takes no budget", bt)
	}
	oc, _ := req["output_config"].(map[string]interface{})
	if oc["effort"] != "max" {
		t.Fatalf("adaptive model: output_config = %v, want the default max effort", req["output_config"])
	}
}
