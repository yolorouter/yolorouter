package chat

import (
	"encoding/json"
	"fmt"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"strings"
)

// ResponseDecoder decodes OpenAI Chat Completions responses into IR.
type ResponseDecoder struct{}

func (ResponseDecoder) DecodeResponse(body json.RawMessage) (*protocols.IRResponse, error) {
	var resp struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string           `json:"role"`
				Content          string           `json:"content"`
				ReasoningContent string           `json:"reasoning_content,omitempty"`
				ToolCalls        []map[string]any `json:"tool_calls,omitempty"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
				// OpenRouter documents a cache-WRITE count nested here, beside
				// cached_tokens: "Number of tokens written to the cache. This
				// appears on the first request when establishing a new cache
				// entry." It is the standard spelling and takes precedence over
				// the top-level alias below.
				//
				// A pointer so "field absent" is distinguishable from "field
				// present and 0". An explicit zero is an assertion that there
				// was no cache write, and it must win over a stale non-zero
				// alias — an int would read it as absent and bill the alias.
				CacheWriteTokens *int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details,omitempty"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
			// Non-standard cache-write breakdown; see
			// protocols.CacheWriteAliasField. Set by gateways fronting an
			// Anthropic model (this one included, see openAIWireUsage). Like
			// cached_tokens it sits INSIDE prompt_tokens, so it is recorded,
			// never added.
			CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		} `json:"usage,omitempty"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}

	irResp := protocols.NewIRResponse(resp.ID, resp.Model)

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		irResp.Content = choice.Message.Content
		irResp.ReasoningContent = choice.Message.ReasoningContent
		irResp.StopReason = mapToIRStopReason(choice.FinishReason)

		for _, tc := range choice.Message.ToolCalls {
			fn, _ := tc["function"].(map[string]interface{})
			if fn == nil {
				continue
			}
			id, _ := tc["id"].(string)
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			irResp.ToolCalls = append(irResp.ToolCalls, protocols.IRToolCall{
				ID: id, Name: name, Arguments: args,
			})
		}
	}

	if resp.Usage != nil {
		irResp.Usage = protocols.IRUsage{
			PromptTokens:          resp.Usage.PromptTokens,
			CompletionTokens:      resp.Usage.CompletionTokens,
			TotalTokens:           resp.Usage.TotalTokens,
			CacheIncludedInPrompt: true,
		}
		if resp.Usage.PromptTokensDetails != nil && resp.Usage.PromptTokensDetails.CachedTokens > 0 {
			irResp.Usage.CacheReadTokens = resp.Usage.PromptTokensDetails.CachedTokens
		}
		// reasoning_tokens is a breakdown OF completion_tokens (OpenAI counts
		// reasoning inside the output total), so it is recorded but NOT added.
		if resp.Usage.CompletionTokensDetails != nil {
			irResp.Usage.ReasoningTokens = resp.Usage.CompletionTokensDetails.ReasoningTokens
		}
		if resp.Usage.PromptCacheHitTokens > 0 {
			// DeepSeek uses prompt_cache_hit_tokens instead of prompt_tokens_details.cached_tokens
			irResp.Usage.CacheReadTokens = resp.Usage.PromptCacheHitTokens
		}
		// prompt_cache_miss_tokens is deliberately NOT mapped to CacheWriteTokens.
		// DeepSeek splits the prompt into hit + miss (hit + miss == prompt_tokens);
		// "miss" is the part that was NOT served from cache, not a cache-creation
		// charge — DeepSeek has no cache-write concept. Mapping it made
		// NetPromptTokens compute prompt - hit - miss == 0 for every DeepSeek
		// request, so the entire input was billed at cache-write price (unset, i.e.
		// free, on most non-Anthropic models). The miss portion is already covered
		// by PromptTokens - CacheReadTokens.
		//
		// Cache WRITE has two spellings in the wild and they mean the same
		// breakdown of prompt_tokens, so exactly one is taken — never summed.
		// OpenRouter's nested cache_write_tokens is the documented contract and
		// wins; the top-level alias is what new-api-style gateways (including
		// this one's own egress) emit and serves as the fallback.
		//
		// Precedence is by field PRESENCE, not by value: an explicitly reported
		// 0 asserts "no cache write" and must beat a stale non-zero alias. A
		// negative value is likewise carried through rather than masked, so the
		// billing gate can refuse the whole record.
		irResp.Usage.CacheWriteTokens = resp.Usage.CacheCreationInputTokens
		if resp.Usage.PromptTokensDetails != nil && resp.Usage.PromptTokensDetails.CacheWriteTokens != nil {
			irResp.Usage.CacheWriteTokens = *resp.Usage.PromptTokensDetails.CacheWriteTokens
		}
		// Set the verdict at the IR exit so every consumer reads Invalid instead
		// of re-judging on data the conversion has since distorted. See
		// IRUsage.IsIncoherent.
		irResp.Usage.Invalid = irResp.Usage.IsIncoherent()
	}

	return irResp, nil
}

// StreamDecoder decodes OpenAI SSE stream chunks into IR deltas.
type StreamDecoder struct {
	buffer     string
	started    bool
	done       bool
	toolArgBuf map[int]string // index -> accumulated arguments
}

func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{
		toolArgBuf: make(map[int]string),
	}
}

func (d *StreamDecoder) DecodeChunk(raw string) ([]protocols.IRStreamDelta, error) {
	d.buffer += raw
	var deltas []protocols.IRStreamDelta

	for {
		pos, sepLen := protocols.SSEFrameEnd(d.buffer)
		if pos < 0 {
			break
		}
		block := d.buffer[:pos]
		d.buffer = d.buffer[pos+sepLen:]

		for _, line := range strings.Split(block, "\n") {
			data, ok := protocols.SSEDataPayload(line)
			if !ok {
				continue
			}
			if data == "[DONE]" {
				if !d.done {
					d.done = true
					deltas = append(deltas, protocols.DeltaDone{StopReason: "stop"})
				}
				continue
			}
			chunk := json.RawMessage(data)
			deltas = append(deltas, d.parseChunk(chunk)...)
		}
	}

	return deltas, nil
}

func (d *StreamDecoder) Finish() ([]protocols.IRStreamDelta, error) {
	if strings.TrimSpace(d.buffer) == "" {
		return nil, nil
	}
	remaining := d.buffer
	d.buffer = ""
	return d.DecodeChunk(remaining + "\n\n")
}

func (d *StreamDecoder) parseChunk(raw json.RawMessage) []protocols.IRStreamDelta {
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string           `json:"content"`
				ReasoningContent string           `json:"reasoning_content"`
				ToolCalls        []map[string]any `json:"tool_calls,omitempty"`
				Role             string           `json:"role"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
				// OpenRouter documents a cache-WRITE count nested here, beside
				// cached_tokens: "Number of tokens written to the cache. This
				// appears on the first request when establishing a new cache
				// entry." It is the standard spelling and takes precedence over
				// the top-level alias below.
				//
				// A pointer so "field absent" is distinguishable from "field
				// present and 0". An explicit zero is an assertion that there
				// was no cache write, and it must win over a stale non-zero
				// alias — an int would read it as absent and bill the alias.
				CacheWriteTokens *int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details,omitempty"`
			PromptCacheHitTokens  int `json:"prompt_cache_hit_tokens,omitempty"`
			PromptCacheMissTokens int `json:"prompt_cache_miss_tokens,omitempty"`
			// Non-standard cache-write breakdown; see
			// protocols.CacheWriteAliasField. Set by gateways fronting an
			// Anthropic model (this one included, see openAIWireUsage). Like
			// cached_tokens it sits INSIDE prompt_tokens, so it is recorded,
			// never added.
			CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
		} `json:"usage,omitempty"`
	}

	if json.Unmarshal(raw, &chunk) != nil {
		return nil
	}

	var deltas []protocols.IRStreamDelta

	// Message start
	if !d.started && chunk.ID != "" {
		d.started = true
		deltas = append(deltas, protocols.DeltaMessageStart{ID: chunk.ID, Model: chunk.Model})
	}

	// Content
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]

		if choice.Delta.ReasoningContent != "" {
			deltas = append(deltas, protocols.DeltaThinking{Text: choice.Delta.ReasoningContent})
		}
		if choice.Delta.Content != "" {
			deltas = append(deltas, protocols.DeltaText{Text: choice.Delta.Content})
		}

		// Tool calls with argument accumulation
		for _, tc := range choice.Delta.ToolCalls {
			fn, _ := tc["function"].(map[string]interface{})
			if fn == nil {
				continue
			}
			idx := 0
			if v, ok := tc["index"].(float64); ok {
				idx = int(v)
			}
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			id, _ := tc["id"].(string)

			if name != "" || id != "" {
				if id == "" {
					id = "call_" + protocols.RandomString(12)
				}
				d.toolArgBuf[idx] = ""
				deltas = append(deltas, protocols.DeltaToolCallStart{
					Index: idx, ID: id, Name: name,
				})
			}
			if args != "" {
				d.toolArgBuf[idx] += args
				deltas = append(deltas, protocols.DeltaToolCallArgs{
					Index: idx, Arguments: args,
				})
			}
		}

		if choice.FinishReason != nil && *choice.FinishReason != "" && !d.done {
			d.done = true
			deltas = append(deltas, protocols.DeltaDone{StopReason: mapToIRStopReason(*choice.FinishReason)})
		}
	}

	// Usage.
	//
	// The gate deliberately does NOT require prompt/completion to be non-zero: a
	// fully cache-hit request legitimately reports prompt_tokens:0 with
	// prompt_cache_hit_tokens:N, and some gateways report only total_tokens.
	// Dropping those chunks left encoder.Usage() at zero — nothing billed, the
	// client's include_usage frame suppressed, and a benign post-DONE read error
	// no longer excused, turning a complete response into a 502.
	if chunk.Usage != nil {
		usage := protocols.IRUsage{
			PromptTokens:          chunk.Usage.PromptTokens,
			CompletionTokens:      chunk.Usage.CompletionTokens,
			TotalTokens:           chunk.Usage.TotalTokens,
			CacheIncludedInPrompt: true,
		}
		if chunk.Usage.PromptTokensDetails != nil {
			usage.CacheReadTokens = chunk.Usage.PromptTokensDetails.CachedTokens
		}
		// Breakdown of completion_tokens, not an addition — see the
		// non-streaming decoder above.
		if chunk.Usage.CompletionTokensDetails != nil {
			usage.ReasoningTokens = chunk.Usage.CompletionTokensDetails.ReasoningTokens
		}
		if chunk.Usage.PromptCacheHitTokens > 0 {
			// DeepSeek uses prompt_cache_hit_tokens instead of prompt_tokens_details.cached_tokens
			usage.CacheReadTokens = chunk.Usage.PromptCacheHitTokens
		}
		// prompt_cache_miss_tokens is deliberately NOT mapped to CacheWriteTokens
		// — see the non-streaming decoder above for the full rationale. The
		// cache_creation_input_tokens alias, by contrast, IS a genuine
		// cache-write count. OpenRouter's nested cache_write_tokens is the
		// documented spelling and wins over the top-level alias; exactly one is
		// taken, never summed. See the non-streaming decoder above.
		usage.CacheWriteTokens = chunk.Usage.CacheCreationInputTokens
		if chunk.Usage.PromptTokensDetails != nil && chunk.Usage.PromptTokensDetails.CacheWriteTokens != nil {
			usage.CacheWriteTokens = *chunk.Usage.PromptTokensDetails.CacheWriteTokens
		}
		// Marked rather than dropped: IRUsage.Merge folds frames into one
		// accumulated record, so dropping this one would leave the previous
		// frame's counts standing and a finish_reason in the same chunk would
		// still complete the stream and bill them. Merge propagates Invalid
		// one-way, so the verdict travels with the record. IsIncoherent (not
		// HasNegativeCount) so the cache-exceeds-prompt impossibility is caught
		// here too, matching what the billing gate decides.
		usage.Invalid = usage.IsIncoherent()
		deltas = append(deltas, protocols.DeltaUsage{Usage: usage})
	}

	return deltas
}

// mapToIRStopReason normalises OpenAI's finish_reason onto the IR vocabulary
// (stop | length | tool_calls | content_filter).
func mapToIRStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "function_call":
		// Deprecated alias of tool_calls, still legal in OpenAI's enum and
		// emitted by some compatible upstreams.
		return "tool_calls"
	case "max_tokens", "model_length", "max_output_tokens":
		// OpenAI's own word is "length", but compat gateways routinely echo the
		// Anthropic / vendor spelling. These are KNOWN synonyms, so folding them
		// in is what new-api's reasonmap does too — and it matters: leaving them
		// verbatim kept IRStopReasonIsAbnormal from recognising a truncation, so
		// a run cut off mid-tool-call was re-encoded as a clean "tool_calls" and
		// the client was told the partial arguments were safe to execute.
		return "length"
	case "safety", "recitation":
		return "content_filter"
	}
	// Genuinely unknown values still pass through verbatim: minting a synthetic
	// marker would oblige every egress encoder to invent a representation, and
	// OpenAI's enum has none. Only established synonyms are normalised.
	return reason
}
