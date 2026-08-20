package gemini

import (
	"encoding/json"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"strings"
)

// ResponseEncoder encodes IR responses into Gemini generateContent format.
type ResponseEncoder struct{}

func (ResponseEncoder) EncodeResponse(resp *protocols.IRResponse) json.RawMessage {
	parts := buildGeminiParts(resp)

	finishReason := mapToGeminiFinishReason(resp.StopReason, len(resp.ToolCalls) > 0)

	geminiResp := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content": map[string]interface{}{
					"role":  "model",
					"parts": parts,
				},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": buildGeminiUsage(resp.Usage),
	}

	data, _ := json.Marshal(geminiResp)
	return data
}

// StreamEncoder encodes IR deltas into Gemini SSE format.
type StreamEncoder struct {
	usage      protocols.IRUsage
	model      string
	toolArgBuf map[int]string // index -> accumulated arguments
	toolNames  map[int]string
	stopReason string
	hasStop    bool
}

func NewStreamEncoder() *StreamEncoder {
	return &StreamEncoder{
		toolArgBuf: make(map[int]string),
		toolNames:  make(map[int]string),
	}
}

func (e *StreamEncoder) EncodeDeltas(deltas []protocols.IRStreamDelta) []protocols.SSEEvent {
	var events []protocols.SSEEvent

	for _, delta := range deltas {
		switch d := delta.(type) {
		case protocols.DeltaMessageStart:
			e.model = d.Model
		case protocols.DeltaText:
			events = append(events, geminiTextChunk(d.Text, e.model))
		case protocols.DeltaThinking:
			// The Gemini protocol marks thinking content with parts[].thought=true so
			// client SDKs can distinguish the reasoning summary from regular output.
			// Treating DeltaThinking as a plain DeltaText would surface the reasoning
			// process to the user as ordinary text, which is inconsistent with the
			// Codex / Claude codec paths.
			events = append(events, geminiThoughtChunk(d.Text, e.model))
		case protocols.DeltaToolCallStart:
			e.toolNames[d.Index] = d.Name
			e.toolArgBuf[d.Index] = ""
		case protocols.DeltaToolCallArgs:
			e.toolArgBuf[d.Index] += d.Arguments
			name, ok := e.toolNames[d.Index]
			if !ok {
				continue
			}
			// Only emit when we have complete JSON
			var argsMap map[string]interface{}
			if json.Unmarshal([]byte(e.toolArgBuf[d.Index]), &argsMap) == nil {
				events = append(events, geminiToolCallChunk(name, argsMap))
			}
		case protocols.DeltaUsage:
			// Field-level merge (via IRUsage.Merge): avoids a later completion-only
			// usage frame zeroing out PromptTokens / cache fields that were already
			// collected, when the upstream splits usage across several partial chunks.
			// Matches the claude / responses / chat encoder behavior.
			e.usage.Merge(d.Usage)
		case protocols.DeltaUnknown:
			var value json.RawMessage
			if json.Unmarshal(d.Raw, &value) == nil {
				events = append(events, protocols.SSEEvent{Data: string(value)})
			}
		case protocols.DeltaDone:
			e.stopReason = d.StopReason
			e.hasStop = true
		}
	}

	return events
}

func (e *StreamEncoder) EncodeDone() []protocols.SSEEvent {
	if !e.hasStop {
		return nil
	}
	finishReason := mapToGeminiFinishReason(e.stopReason, len(e.toolNames) > 0)
	usageMeta := buildGeminiUsage(e.usage)
	finishChunk := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content":      map[string]interface{}{"role": "model", "parts": []interface{}{}},
				"finishReason": finishReason,
			},
		},
		"usageMetadata": usageMeta,
	}
	data, _ := json.Marshal(finishChunk)
	return []protocols.SSEEvent{{Data: string(data)}}
}

func (e *StreamEncoder) Usage() protocols.IRUsage {
	return e.usage
}

// --- helpers ---

func buildGeminiParts(resp *protocols.IRResponse) []interface{} {
	var parts []interface{}

	// Thinking content
	if resp.ReasoningContent != "" {
		parts = append(parts, map[string]interface{}{
			"text":    resp.ReasoningContent,
			"thought": true,
		})
	}

	// Text content
	if resp.Content != "" {
		parts = append(parts, map[string]interface{}{"text": resp.Content})
	}

	// Tool calls
	for _, tc := range resp.ToolCalls {
		var args map[string]interface{}
		if json.Unmarshal([]byte(tc.Arguments), &args) != nil {
			args = map[string]interface{}{}
		}
		parts = append(parts, map[string]interface{}{
			"functionCall": map[string]interface{}{
				"name": tc.Name,
				"args": args,
			},
		})
	}

	if len(parts) == 0 {
		parts = append(parts, map[string]interface{}{"text": ""})
	}
	return parts
}

// buildGeminiUsage builds the Gemini usageMetadata object.
//
// promptTokenCount is the GROSS input (cache included). When cachedContent is
// set, promptTokenCount still includes every token in the cached content.
// Emitting the IR's raw PromptTokens would drop the whole cache portion for
// Anthropic upstreams, whose input count is net.
//
// Gemini has no cache-WRITE field of its own, so CacheWriteTokens is carried in
// the non-standard protocols.CacheWriteAliasField below.
func buildGeminiUsage(u protocols.IRUsage) map[string]interface{} {
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
	// The IR keeps a gross CompletionTokens (reasoning included, OpenAI's
	// convention). Gemini splits them: totalTokenCount is documented as
	// "prompt + thoughts + response candidates", so candidatesTokenCount must
	// exclude thinking. Split them back apart here, keeping the sum intact so
	// totalTokenCount still equals prompt + thoughts + candidates.
	candidates := u.CompletionTokens - u.ReasoningTokens
	if candidates < 0 {
		candidates = 0
	}
	meta := map[string]interface{}{
		"promptTokenCount":     u.GrossPromptTokens(),
		"candidatesTokenCount": candidates,
		"totalTokenCount":      u.GrossTotalTokens(),
	}
	if u.ReasoningTokens > 0 {
		meta["thoughtsTokenCount"] = u.ReasoningTokens
	}
	if u.CacheReadTokens > 0 {
		meta["cachedContentTokenCount"] = u.CacheReadTokens
	}
	// Non-standard cache-write breakdown; see protocols.CacheWriteAliasField.
	// Deliberately snake_case among Gemini's camelCase fields — it is not a
	// Google field and should not look like one.
	if u.CacheWriteTokens > 0 {
		meta[protocols.CacheWriteAliasField] = u.CacheWriteTokens
	}
	return meta
}

func geminiTextChunk(text, model string) protocols.SSEEvent {
	return geminiPartChunk(map[string]interface{}{"text": text}, model)
}

// geminiThoughtChunk emits a part with thought:true so client SDKs recognize
// it as reasoning content (rather than mixing it into the regular output shown
// to the user). Matches the official Gemini API behavior on thinking-capable models.
func geminiThoughtChunk(text, model string) protocols.SSEEvent {
	return geminiPartChunk(map[string]interface{}{"text": text, "thought": true}, model)
}

func geminiPartChunk(part map[string]interface{}, model string) protocols.SSEEvent {
	chunk := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content": map[string]interface{}{
					"role":  "model",
					"parts": []interface{}{part},
				},
			},
		},
	}
	if model != "" {
		chunk["modelVersion"] = model
	}
	data, _ := json.Marshal(chunk)
	return protocols.SSEEvent{Data: string(data)}
}

func geminiToolCallChunk(name string, args map[string]interface{}) protocols.SSEEvent {
	chunk := map[string]interface{}{
		"candidates": []interface{}{
			map[string]interface{}{
				"content": map[string]interface{}{
					"role": "model",
					"parts": []interface{}{
						map[string]interface{}{
							"functionCall": map[string]interface{}{
								"name": name,
								"args": args,
							},
						},
					},
				},
			},
		},
	}
	data, _ := json.Marshal(chunk)
	return protocols.SSEEvent{Data: string(data)}
}

func mapToGeminiFinishReason(reason string, hasToolCalls bool) string {
	// Abnormal terminations outrank the tool-call inference: returning "STOP"
	// for a run that was actually truncated (or filtered) would present partial,
	// possibly invalid tool-call arguments as a clean finish.
	if protocols.IRStopReasonIsAbnormal(reason) {
		if reason == "content_filter" {
			return "SAFETY"
		}
		return "MAX_TOKENS"
	}
	if hasToolCalls {
		return "STOP"
	}
	switch reason {
	case "stop", "tool_calls":
		return "STOP"
	case "length", "max_tokens":
		return "MAX_TOKENS"
	case "content_filter":
		return "SAFETY"
	case "error":
		// Explicit upstream stream failure (the ResponsesStreamDecoder has
		// already emitted protocols.DeltaDone{stop_reason="error"}): the Gemini
		// wire protocol has no standard "error" finishReason, but OTHER conveys
		// an abnormal termination far more honestly than misreporting STOP,
		// which would let the client believe it received a complete response.
		return "OTHER"
	default:
		return "STOP"
	}
}

// StreamDecoder decodes Gemini SSE/JSON Lines into IR deltas.
// Used when reading from a Gemini upstream.
type StreamDecoder struct {
	buffer      string
	first       bool
	toolCallIdx int
}

func NewStreamDecoder() *StreamDecoder {
	return &StreamDecoder{first: true}
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
			payload, ok := protocols.SSEDataPayload(line)
			if !ok || payload == "" {
				continue
			}
			deltas = append(deltas, d.parseGeminiChunk(json.RawMessage(payload))...)
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

// usageMetadata is the token-accounting block both the streaming and the
// non-streaming decoder read. Shared so the two paths can never drift into
// reporting different totals for the same response.
type usageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
	// Thinking-model reasoning tokens. Whether candidatesTokenCount already
	// contains them depends on which backend answered — the Google AI endpoint
	// folds them in, Vertex AI reports them alongside — so this field can never
	// be added to candidatesTokenCount unconditionally. See toIRUsage.
	ThoughtsTokenCount int `json:"thoughtsTokenCount,omitempty"`
	// Tokens from tool-execution results fed back to the model. Google
	// documents these as input, and totalTokenCount counts them, so they bill
	// at the input rate and must be excluded when deriving output from the
	// total.
	ToolUsePromptTokenCount int `json:"toolUsePromptTokenCount,omitempty"`
	// Non-standard cache-write breakdown; see protocols.CacheWriteAliasField.
	// Gemini has no cache-write concept of its own, so this only ever appears
	// when the upstream is another gateway fronting an Anthropic model. Like
	// cachedContentTokenCount it sits INSIDE promptTokenCount.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CachedContentTokenCount  int `json:"cachedContentTokenCount,omitempty"`
}

// toIRUsage converts the block, reporting false when any count is negative.
// The negative check has to happen HERE rather than downstream: folding
// thoughts into candidates sums two numbers, and a sum hides the sign of its
// parts (10 + -5 looks like a perfectly ordinary 5). Rejecting the whole block
// leaves usage unknown, which bills nothing — strictly safer than billing a
// plausible-looking number derived from garbage.
func (m *usageMetadata) toIRUsage() (protocols.IRUsage, bool) {
	if m.PromptTokenCount < 0 || m.CandidatesTokenCount < 0 || m.ThoughtsTokenCount < 0 ||
		m.TotalTokenCount < 0 || m.CachedContentTokenCount < 0 || m.ToolUsePromptTokenCount < 0 ||
		m.CacheCreationInputTokens < 0 {
		return protocols.IRUsage{}, false
	}
	prompt := m.promptTokens()
	completion := m.completionTokens()
	// In the normal case completion was derived from the total, so this holds
	// already. It only bites when the total was unusable and completion fell
	// back to the sum: reporting the stated total then would hand downstream a
	// triple whose parts exceed their own total, which the gateway's coherence
	// check reads as garbage and refuses to bill at all.
	total := max(m.TotalTokenCount, prompt+completion)
	return protocols.IRUsage{
		PromptTokens:          prompt,
		CompletionTokens:      completion,
		ReasoningTokens:       m.ThoughtsTokenCount,
		TotalTokens:           total,
		CacheReadTokens:       m.CachedContentTokenCount,
		CacheWriteTokens:      m.CacheCreationInputTokens,
		CacheIncludedInPrompt: true,
	}, true
}

// promptTokens returns the full billable input. Tool-execution results are
// input by nature — Google describes them as "provided back to the model as
// input" — so they belong on the input line rather than being left out (which
// would drop them from the bill) or swept into the output derivation below
// (which would charge them at the output rate, typically several times higher).
func (m *usageMetadata) promptTokens() int {
	return m.PromptTokenCount + m.ToolUsePromptTokenCount
}

// completionTokens returns the full billable output — the answer plus any
// thinking tokens, which bill at the same output rate.
//
// Deriving it from the total is the only formula that holds on both backends.
// The Google AI endpoint already counts thinking inside candidatesTokenCount
// while Vertex AI reports the two side by side, so adding them unconditionally
// double-charges thinking on the former, and no field in the response says
// which convention is in force. Subtracting the input side sidesteps the
// question. Google defines the total as
//
//	prompt + candidates + tool_use_prompt + thoughts
//
// so total - (prompt + tool_use_prompt) leaves exactly the generated tokens
// under either convention. promptTokenCount already includes
// cachedContentTokenCount, so a cache hit doesn't distort the subtraction.
//
// The sum is the fallback for responses that omit the total (or report one at
// or below the input, which can't be right).
//
// The subtraction is additionally floored at candidatesTokenCount, because a
// positive result is not by itself proof that the total was current. Some
// OpenAI-compatible fronts fill totalTokenCount from a stale snapshot while the
// per-part counts in the SAME payload are up to date — prompt=100,
// candidates=100, thoughts=0, total=150 subtracts to 50 even though the
// response already accounts for 100 generated tokens. Nothing downstream can
// notice: the record is coherent by every rule and simply bills half the output.
//
// The floor is candidatesTokenCount ALONE, never candidates + thoughts. Adding
// them is exactly the double-count the subtraction exists to avoid: on the
// Google AI endpoint thinking is already inside candidatesTokenCount, so the sum
// would charge it twice, and no field says which convention is in force.
// Candidates on its own is safe under both — it equals the whole output there,
// and is a strict part of it on Vertex — so it can only ever raise a
// short-changed result back to something the payload itself vouches for.
func (m *usageMetadata) completionTokens() int {
	if prompt := m.promptTokens(); m.TotalTokenCount > prompt {
		return max(m.TotalTokenCount-prompt, m.CandidatesTokenCount)
	}
	return m.CandidatesTokenCount + m.ThoughtsTokenCount
}

func (d *StreamDecoder) parseGeminiChunk(raw json.RawMessage) []protocols.IRStreamDelta {
	var chunk struct {
		UsageMetadata *usageMetadata `json:"usageMetadata"`
		ModelVersion  string         `json:"modelVersion"`
		Candidates    []struct {
			Content *struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
	}

	if json.Unmarshal(raw, &chunk) != nil {
		return nil
	}

	var deltas []protocols.IRStreamDelta

	if d.first {
		d.first = false
		deltas = append(deltas, protocols.DeltaMessageStart{
			ID:    "gen-" + protocols.RandomString(16),
			Model: chunk.ModelVersion,
		})
	}

	for _, cand := range chunk.Candidates {
		if cand.Content != nil {
			for _, partRaw := range cand.Content.Parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(partRaw, &part) != nil {
					continue
				}

				if textRaw, ok := part["text"]; ok {
					var text string
					_ = json.Unmarshal(textRaw, &text)
					if thoughtRaw, ok := part["thought"]; ok {
						var thought bool
						if json.Unmarshal(thoughtRaw, &thought) == nil && thought {
							deltas = append(deltas, protocols.DeltaThinking{Text: text})
							continue
						}
					}
					if text != "" {
						deltas = append(deltas, protocols.DeltaText{Text: text})
					}
				}

				if fcRaw, ok := part["functionCall"]; ok {
					var fc struct {
						Name string          `json:"name"`
						Args json.RawMessage `json:"args"`
					}
					if json.Unmarshal(fcRaw, &fc) == nil {
						id := "call_" + protocols.RandomString(12)
						deltas = append(deltas, protocols.DeltaToolCallStart{
							Index: d.toolCallIdx, ID: id, Name: fc.Name,
						})
						d.toolCallIdx++
						if string(fc.Args) != "" && string(fc.Args) != "{}" {
							deltas = append(deltas, protocols.DeltaToolCallArgs{
								Index: d.toolCallIdx - 1, Arguments: string(fc.Args),
							})
						}
					}
				}
			}
		}

		if cand.FinishReason != "" {
			// Shared with the non-streaming decoder so the two paths can never
			// drift: SAFETY family normalised to content_filter, others
			// lower-cased. See mapFromGeminiFinishReason.
			deltas = append(deltas, protocols.DeltaDone{StopReason: mapFromGeminiFinishReason(cand.FinishReason)})
		}
	}

	if chunk.UsageMetadata != nil {
		// A rejected block still emits a delta, marked Invalid. Emitting
		// nothing was the earlier behaviour and it is not enough: this decoder
		// rejects BEFORE IRUsage.Merge ever sees the frame, so the verdict
		// never reaches the accumulator — whatever an earlier valid frame
		// contributed stays in place, and the DeltaDone that follows completes
		// the stream and bills those stale counts as coherent.
		u, ok := chunk.UsageMetadata.toIRUsage()
		if !ok {
			u = protocols.IRUsage{Invalid: true}
		} else {
			// Same full verdict the non-streaming path applies at its exit
			// (and chat's streaming decoder applies per frame): a Gemini
			// frame is a genuine single snapshot, so the growth-sensitive
			// reasoning-subset rule is safe here — without it, a stale-total
			// shape slips through streaming while the identical payload is
			// refused on the non-streaming path.
			u.Invalid = u.IsIncoherent()
		}
		deltas = append(deltas, protocols.DeltaUsage{Usage: u})
	}

	return deltas
}

// ResponseDecoder decodes a Gemini generateContent JSON response into IR.
type ResponseDecoder struct{}

func (ResponseDecoder) DecodeResponse(body json.RawMessage) (*protocols.IRResponse, error) {
	var resp struct {
		Candidates []struct {
			Content *struct {
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata *usageMetadata `json:"usageMetadata"`
		ModelVersion  string         `json:"modelVersion"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	irResp := &protocols.IRResponse{
		ID:    "chatcmpl-" + protocols.RandomString(24),
		Model: resp.ModelVersion,
		Usage: protocols.IRUsage{CacheIncludedInPrompt: true},
	}

	for _, cand := range resp.Candidates {
		irResp.StopReason = mapFromGeminiFinishReason(cand.FinishReason)
		if cand.Content == nil {
			continue
		}
		for _, partRaw := range cand.Content.Parts {
			var part map[string]json.RawMessage
			if json.Unmarshal(partRaw, &part) != nil {
				continue
			}

			if textRaw, ok := part["text"]; ok {
				var text string
				_ = json.Unmarshal(textRaw, &text)
				if thoughtRaw, ok := part["thought"]; ok {
					var thought bool
					if json.Unmarshal(thoughtRaw, &thought) == nil && thought {
						irResp.ReasoningContent += text
						continue
					}
				}
				irResp.Content += text
			}

			if fcRaw, ok := part["functionCall"]; ok {
				var fc struct {
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}
				if json.Unmarshal(fcRaw, &fc) == nil {
					args := string(fc.Args)
					if args == "" {
						args = "{}"
					}
					irResp.ToolCalls = append(irResp.ToolCalls, protocols.IRToolCall{
						ID:        "call_" + protocols.RandomString(12),
						Name:      fc.Name,
						Arguments: args,
					})
				}
			}
		}
	}

	if resp.UsageMetadata != nil {
		// Set the verdict at the IR exit so every consumer reads Invalid
		// instead of re-judging. See IRUsage.IsIncoherent.
		u, ok := resp.UsageMetadata.toIRUsage()
		if !ok {
			// A rejected block is unknown usage, not zero-cost usage, and it
			// has to say so: a bare zero value reads as a coherent "0 tokens"
			// downstream, which the wire encoders serialise as an all-zero
			// object instead of null and the billing gate treats as free but
			// measured. Marking it matches what the streaming path emits for
			// the same rejection.
			u = protocols.IRUsage{Invalid: true}
		} else {
			u.Invalid = u.IsIncoherent()
		}
		irResp.Usage = u
	}

	return irResp, nil
}

// mapFromGeminiFinishReason normalises Gemini's finishReason onto the IR
// vocabulary (stop | length | tool_calls | content_filter | error).
//
// Gemini has several distinct safety terminations. Lower-casing them instead of
// mapping them let "safety" / "recitation" reach the IR verbatim, where no
// egress encoder recognises them: the abnormal-termination guard did not fire,
// so a blocked response could be reported as a clean tool-call finish, and the
// Chat/Claude encoders emitted values outside their own enums.
func mapFromGeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII",
		"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT", "IMAGE_RECITATION", "MODEL_ARMOR":
		return "content_filter"
	case "":
		return ""
	default:
		// MALFORMED_FUNCTION_CALL, UNEXPECTED_TOOL_CALL, OTHER, and anything
		// Google adds later. Passed through lower-cased rather than folded into
		// a synthetic value — same as new-api's reasonmap default. The safety
		// terminations above are mapped because they have a real equivalent in
		// every target protocol; these do not.
		return strings.ToLower(reason)
	}
}
