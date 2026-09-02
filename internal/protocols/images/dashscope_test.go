package images

// DashScope dialect tests. This dialect ships in two codebases that must
// stay honest about the same wire behaviour: URL joining by origin, the size
// separator, request encoding, response decoding with its two failure
// classes, and error normalization.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUpstreamURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
	}{
		{
			name:    "compatible-mode base joins by origin",
			baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
			want:    "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation",
		},
		{
			name:    "bare host",
			baseURL: "https://dashscope.aliyuncs.com",
			want:    "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation",
		},
		{
			name:    "trailing slash",
			baseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1/",
			want:    "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation",
		},
		{
			name:    "workspace domain joins by origin",
			baseURL: "https://bp13m4xxxx.cn-beijing.maas.aliyuncs.com/compatible-mode/v1",
			want:    "https://bp13m4xxxx.cn-beijing.maas.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpstreamURL(tc.baseURL); got != tc.want {
				t.Errorf("UpstreamURL(%q) = %q, want %q", tc.baseURL, got, tc.want)
			}
		})
	}
}

func TestIsDashScopeBase(t *testing.T) {
	for base, want := range map[string]bool{
		"https://dashscope.aliyuncs.com/compatible-mode/v1": true,
		"https://dashscope-intl.aliyuncs.com":               true,
		"https://api.dashscope.aliyuncs.com":                true,
		"https://bp13m4xxxx.cn-beijing.maas.aliyuncs.com":   true,
		"https://ws-01.cn-hangzhou.maas.aliyuncs.com/v1":    true,
		"https://example.maas.aliyuncs.com.evil.com":        false,
		"https://maas.aliyuncs.com":                         false,
		"https://api.openai.com/v1":                         false,
		"http://localhost:8080":                             false,
		"://not-a-url":                                      false,
	} {
		if got := IsDashScopeBase(base); got != want {
			t.Errorf("IsDashScopeBase(%q) = %v, want %v", base, got, want)
		}
	}
}

func TestConvertSize(t *testing.T) {
	for in, want := range map[string]string{
		"1024x1024": "1024*1024",
		"1280X720":  "1280*720",
		"":          "",
		"1024*1024": "1024*1024",
	} {
		if got := ConvertSize(in); got != want {
			t.Errorf("ConvertSize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeRequest(t *testing.T) {
	t.Run("encodes prompt, model, n and converted size", func(t *testing.T) {
		body, err := EncodeRequest("a cat", "wan2.7-image", 2, "1024x1024")
		if err != nil {
			t.Fatalf("EncodeRequest error: %v", err)
		}
		var req dashScopeReq
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if req.Model != "wan2.7-image" {
			t.Errorf("model = %q, want wan2.7-image", req.Model)
		}
		if len(req.Input.Messages) != 1 || req.Input.Messages[0].Role != "user" {
			t.Fatalf("messages = %+v, want one user message", req.Input.Messages)
		}
		if len(req.Input.Messages[0].Content) != 1 || req.Input.Messages[0].Content[0].Text != "a cat" {
			t.Errorf("content = %+v, want the prompt text", req.Input.Messages[0].Content)
		}
		if req.Parameters.N != 2 {
			t.Errorf("n = %d, want 2", req.Parameters.N)
		}
		if req.Parameters.Size != "1024*1024" {
			t.Errorf("size = %q, want 1024*1024", req.Parameters.Size)
		}
	})

	t.Run("n at or below zero defaults to one", func(t *testing.T) {
		body, _ := EncodeRequest("test", "wan2.7-image", 0, "")
		var req dashScopeReq
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if req.Parameters.N != 1 {
			t.Errorf("n = %d, want 1", req.Parameters.N)
		}
	})

	t.Run("empty size is omitted", func(t *testing.T) {
		body, _ := EncodeRequest("test", "wan2.7-image", 1, "")
		if strings.Contains(string(body), `"size"`) {
			t.Errorf("empty size should be omitted from JSON, got: %s", body)
		}
	})
}

func TestDecodeResponse(t *testing.T) {
	t.Run("single image", func(t *testing.T) {
		raw := `{
			"request_id": "req-001",
			"output": {"choices": [{"message": {"role": "assistant",
				"content": [{"type": "image", "image": "https://img.example.com/1.jpg"}]}}], "finished": true},
			"usage": {"image_count": 1, "input_tokens": 35, "output_tokens": 2}
		}`
		body, count, err := DecodeResponse([]byte(raw))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 1 {
			t.Errorf("imageCount = %d, want 1", count)
		}
		var resp Response
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal openai response: %v", err)
		}
		if len(resp.Data) != 1 || resp.Data[0].URL != "https://img.example.com/1.jpg" {
			t.Errorf("data = %+v, want the one image URL", resp.Data)
		}
	})

	t.Run("live shape omits the type tag on image items", func(t *testing.T) {
		// The endpoint as actually observed answers with content items that
		// carry only the image URL; requiring a type tag misread this as a
		// no-images protocol error.
		raw := `{"request_id": "req-live", "output": {"choices": [{"finish_reason": "stop",
			"message": {"content": [{"image": "https://img.example.com/live.png"}]}}], "finished": true},
			"usage": {"input_tokens": 35, "output_tokens": 2}}`
		body, count, err := DecodeResponse([]byte(raw))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 1 {
			t.Errorf("imageCount = %d, want 1", count)
		}
		var resp Response
		if err := json.Unmarshal(body, &resp); err != nil {
			t.Fatalf("unmarshal openai response: %v", err)
		}
		if len(resp.Data) != 1 || resp.Data[0].URL != "https://img.example.com/live.png" {
			t.Errorf("data = %+v, want the one image URL", resp.Data)
		}
	})

	t.Run("multiple choices count as multiple images", func(t *testing.T) {
		raw := `{"output": {"choices": [
			{"message": {"content": [{"type": "image", "image": "https://img.example.com/1.jpg"}]}},
			{"message": {"content": [{"type": "image", "image": "https://img.example.com/2.jpg"}]}}
		], "finished": true}}`
		_, count, err := DecodeResponse([]byte(raw))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 2 {
			t.Errorf("imageCount = %d, want 2", count)
		}
	})

	t.Run("non-image content entries are filtered", func(t *testing.T) {
		raw := `{"output": {"choices": [{"message": {"content": [
			{"type": "text", "text": "some text"},
			{"type": "image", "image": "https://img.example.com/1.jpg"}
		]}}], "finished": true}}`
		_, count, err := DecodeResponse([]byte(raw))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if count != 1 {
			t.Errorf("imageCount = %d, want 1 (text entries filtered)", count)
		}
	})

	t.Run("business error when code is non-empty", func(t *testing.T) {
		raw := `{"request_id": "req-002", "output": {"choices": []}, "code": "InvalidInput", "message": "prompt is too long"}`
		_, _, err := DecodeResponse([]byte(raw))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !IsBusinessError(err) {
			t.Errorf("expected BusinessError, got: %T %v", err, err)
		}
	})

	t.Run("empty choices is a protocol error, not a business one", func(t *testing.T) {
		_, _, err := DecodeResponse([]byte(`{"output": {"choices": [], "finished": true}}`))
		if err == nil {
			t.Fatal("expected error for empty choices")
		}
		if IsBusinessError(err) {
			t.Errorf("empty choices should not be BusinessError, got: %v", err)
		}
	})

	t.Run("content without images is a protocol error", func(t *testing.T) {
		_, _, err := DecodeResponse([]byte(`{"output": {"choices": [{"message": {"content": [{"type": "text", "text": "hi"}]}}]}}`))
		if err == nil {
			t.Fatal("expected error when no image content")
		}
		if IsBusinessError(err) {
			t.Errorf("no-image content should not be BusinessError, got: %v", err)
		}
	})

	t.Run("unparseable body is a protocol error", func(t *testing.T) {
		_, _, err := DecodeResponse([]byte("not json"))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
		if IsBusinessError(err) {
			t.Error("JSON parse error should not be BusinessError")
		}
	})
}

func TestNormalizeError(t *testing.T) {
	t.Run("dashscope error shape carries code and message", func(t *testing.T) {
		errBody, ct := NormalizeError(422, []byte(`{"code":"InvalidInput","message":"prompt required","request_id":"r1"}`))
		if ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		var out map[string]any
		if err := json.Unmarshal(errBody, &out); err != nil {
			t.Fatalf("unmarshal error body: %v", err)
		}
		errObj, ok := out["error"].(map[string]any)
		if !ok {
			t.Fatal("missing error object")
		}
		msg, _ := errObj["message"].(string)
		if !strings.Contains(msg, "InvalidInput") || !strings.Contains(msg, "prompt required") {
			t.Errorf("message should carry code and text, got: %q", msg)
		}
	})

	t.Run("unparseable body falls back to the status", func(t *testing.T) {
		errBody, _ := NormalizeError(500, []byte("internal server error"))
		var out map[string]any
		if err := json.Unmarshal(errBody, &out); err != nil {
			t.Fatalf("unmarshal error body: %v", err)
		}
		msg := out["error"].(map[string]any)["message"].(string)
		if !strings.Contains(msg, "500") {
			t.Errorf("fallback message should contain the status, got: %q", msg)
		}
	})
}
