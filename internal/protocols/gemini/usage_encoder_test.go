package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// TestBuildGeminiUsage_GrossPromptTokens locks the usage rule that
// promptTokenCount is "the total effective prompt size meaning this includes
// the number of tokens in the cached content". An Anthropic upstream reports a
// NET prompt (CacheIncludedInPrompt=false), so the cache must be added back —
// otherwise a fully-cached request reports promptTokenCount=0 while
// cachedContentTokenCount is non-zero, which is self-contradictory.
func TestBuildGeminiUsage_GrossPromptTokens(t *testing.T) {
	meta := buildGeminiUsage(protocols.IRUsage{
		PromptTokens:     0,
		CompletionTokens: 38,
		CacheWriteTokens: 344,
		CacheReadTokens:  6521,
		TotalTokens:      6903,
	})

	if got := meta["promptTokenCount"]; got != 6865 {
		t.Errorf("promptTokenCount = %v, want 6865 (0 net + 344 write + 6521 read)", got)
	}
	if got := meta["cachedContentTokenCount"]; got != 6521 {
		t.Errorf("cachedContentTokenCount = %v, want 6521", got)
	}
	if got := meta["totalTokenCount"]; got != 6903 {
		t.Errorf("totalTokenCount = %v, want 6903", got)
	}
	if meta["cachedContentTokenCount"].(int) > meta["promptTokenCount"].(int) {
		t.Error("cachedContentTokenCount must not exceed promptTokenCount")
	}
}

// TestGeminiUsage_ThoughtsRoundTrip locks the thinking-token accounting across
// both directions. Gemini counts thoughts OUTSIDE candidatesTokenCount
// (totalTokenCount = prompt + thoughts + candidates), while OpenAI counts
// reasoning INSIDE completion_tokens. The decoder folds thoughts into
// CompletionTokens — without it the billing layer, which charges output on
// CompletionTokens, silently under-charges every thinking-model request — and
// the encoder splits them back apart.
func TestGeminiUsage_ThoughtsRoundTrip(t *testing.T) {
	body := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "hi"}]}, "finishReason": "STOP"}],
		"usageMetadata": {
			"promptTokenCount": 100, "candidatesTokenCount": 30,
			"thoughtsTokenCount": 20, "totalTokenCount": 150
		}
	}`)

	resp, err := ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50 (30 candidates + 20 thoughts)", resp.Usage.CompletionTokens)
	}
	if resp.Usage.ReasoningTokens != 20 {
		t.Errorf("ReasoningTokens = %d, want 20", resp.Usage.ReasoningTokens)
	}
	if got := resp.Usage.GrossTotalTokens(); got != 150 {
		t.Errorf("GrossTotalTokens() = %d, want 150 (prompt 100 + completion 50)", got)
	}

	// Encoding back to Gemini must restore the original split.
	meta := buildGeminiUsage(resp.Usage)
	if meta["candidatesTokenCount"] != 30 {
		t.Errorf("candidatesTokenCount = %v, want 30 (thoughts excluded again)", meta["candidatesTokenCount"])
	}
	if meta["thoughtsTokenCount"] != 20 {
		t.Errorf("thoughtsTokenCount = %v, want 20", meta["thoughtsTokenCount"])
	}
	if meta["totalTokenCount"] != 150 {
		t.Errorf("totalTokenCount = %v, want 150", meta["totalTokenCount"])
	}
}

// TestGeminiUsage_ToolUsePromptTokensAreBilled covers server-side tool usage
// (Google Search, code execution), which Gemini reports in a SEPARATE
// toolUsePromptTokenCount. It is real billable input, and totalTokenCount's
// documented formula ("prompt + thoughts + response candidates") does not name
// it — so leaving it unparsed made the canonical total identity silently drop
// those tokens from the client's usage, relay_logs, gift-token deduction and
// cost calculation alike.
//
// The numbers below are the real upstream shape: 10 + 20 + 5 + 100 = 135, which
// is exactly what the upstream reports as totalTokenCount — confirming tool-use
// tokens do land inside Gemini's total.
func TestGeminiUsage_ToolUsePromptTokensAreBilled(t *testing.T) {
	body := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "hi"}]}, "finishReason": "STOP"}],
		"usageMetadata": {
			"promptTokenCount": 10, "candidatesTokenCount": 20,
			"thoughtsTokenCount": 5, "toolUsePromptTokenCount": 100,
			"totalTokenCount": 135
		}
	}`)

	resp, err := ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.PromptTokens != 110 {
		t.Errorf("PromptTokens = %d, want 110 (10 prompt + 100 tool-use)", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 25 {
		t.Errorf("CompletionTokens = %d, want 25 (20 candidates + 5 thoughts)", resp.Usage.CompletionTokens)
	}
	// The canonical total must match what the upstream itself reported —
	// nothing dropped, nothing invented.
	if got := resp.Usage.GrossTotalTokens(); got != 135 {
		t.Errorf("GrossTotalTokens() = %d, want 135 (equals the upstream totalTokenCount)", got)
	}
}

// TestBuildGeminiUsage_NoThoughtsKeyWhenAbsent keeps the wire clean for
// non-thinking models: thoughtsTokenCount must not appear as a zero.
func TestBuildGeminiUsage_NoThoughtsKeyWhenAbsent(t *testing.T) {
	meta := buildGeminiUsage(protocols.IRUsage{
		PromptTokens: 100, CompletionTokens: 30, TotalTokens: 130, CacheIncludedInPrompt: true,
	})
	if _, ok := meta["thoughtsTokenCount"]; ok {
		t.Error("thoughtsTokenCount must be omitted when there are no thinking tokens")
	}
	if meta["candidatesTokenCount"] != 30 {
		t.Errorf("candidatesTokenCount = %v, want 30", meta["candidatesTokenCount"])
	}
}

// TestBuildGeminiUsage_GrossUpstreamNotDoubleCounted guards the reverse case:
// an OpenAI/Gemini upstream already reports a gross prompt, so the cache must
// not be added a second time.
func TestBuildGeminiUsage_GrossUpstreamNotDoubleCounted(t *testing.T) {
	meta := buildGeminiUsage(protocols.IRUsage{
		PromptTokens:          2006,
		CompletionTokens:      10,
		CacheReadTokens:       1920,
		TotalTokens:           2016,
		CacheIncludedInPrompt: true,
	})

	if got := meta["promptTokenCount"]; got != 2006 {
		t.Errorf("promptTokenCount = %v, want 2006 (already gross, must not re-add cache)", got)
	}
}

// TestFinishReason_TruncationOutranksToolCalls covers the same rule on the
// Gemini egress. Falling back to "STOP" for a truncated or filtered run would
// present partial, possibly invalid tool-call arguments as a clean finish.
func TestFinishReason_TruncationOutranksToolCalls(t *testing.T) {
	cases := []struct {
		name         string
		reason       string
		hasToolCalls bool
		want         string
	}{
		{"truncated mid tool call", "length", true, "MAX_TOKENS"},
		{"filtered mid tool call", "content_filter", true, "SAFETY"},
		{"clean stop with tool calls", "stop", true, "STOP"},
		{"truncated without tool calls", "length", false, "MAX_TOKENS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mapToGeminiFinishReason(tc.reason, tc.hasToolCalls); got != tc.want {
				t.Errorf("mapToGeminiFinishReason(%q, %v) = %q, want %q",
					tc.reason, tc.hasToolCalls, got, tc.want)
			}
		})
	}
}

// TestMapFromGeminiFinishReason_SafetyNormalised covers Gemini's several
// distinct safety terminations. Lower-casing them left "safety"/"recitation" in
// the IR, where no egress encoder recognises them: the abnormal-termination
// guard never fired, so a blocked response could be reported as a clean
// tool-call finish, and Chat/Claude emitted values outside their own enums.
func TestMapFromGeminiFinishReason_SafetyNormalised(t *testing.T) {
	cases := map[string]string{
		"STOP":               "stop",
		"MAX_TOKENS":         "length",
		"SAFETY":             "content_filter",
		"RECITATION":         "content_filter",
		"BLOCKLIST":          "content_filter",
		"PROHIBITED_CONTENT": "content_filter",
		"SPII":               "content_filter",
	}
	for wire, want := range cases {
		if got := mapFromGeminiFinishReason(wire); got != want {
			t.Errorf("mapFromGeminiFinishReason(%q) = %q, want %q", wire, got, want)
		}
	}

	// End-to-end: a SAFETY block carrying tool calls must still surface as a
	// safety stop on the Gemini egress, not a clean STOP.
	if got := mapToGeminiFinishReason(mapFromGeminiFinishReason("SAFETY"), true); got != "SAFETY" {
		t.Errorf("SAFETY round-trip with tool calls = %q, want SAFETY", got)
	}
}

// TestMapFromGeminiFinishReason_UnknownPassesThrough documents the deliberate
// choice to pass unmapped values through lower-cased instead of folding them
// into a synthetic IR value — the same default new-api's reasonmap uses. The
// safety terminations ARE mapped, because each has a real equivalent in every
// target protocol; MALFORMED_FUNCTION_CALL and friends do not.
func TestMapFromGeminiFinishReason_UnknownPassesThrough(t *testing.T) {
	for _, wire := range []string{"MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL", "OTHER"} {
		want := strings.ToLower(wire)
		if got := mapFromGeminiFinishReason(wire); got != want {
			t.Errorf("mapFromGeminiFinishReason(%q) = %q, want %q", wire, got, want)
		}
	}
	if got := mapFromGeminiFinishReason(""); got != "" {
		t.Errorf("empty finishReason must stay empty, got %q", got)
	}
}

// CompletionTokens has to cover them — but the two Gemini backends disagree on
// where they live. Vertex AI reports thoughtsTokenCount alongside
// candidatesTokenCount; the Google AI endpoint folds them into
// candidatesTokenCount. Nothing in the response says which convention applies,
// so the decoder derives output from totalTokenCount - promptTokenCount, which
// is correct under both. Adding candidates + thoughts unconditionally would
// double-charge thinking on the folded-in backend.
func TestGeminiThinkingTokenAccounting(t *testing.T) {
	cases := []struct {
		name           string
		usageMeta      string
		wantPrompt     int
		wantCompletion int
		wantReasoning  int
	}{
		{
			// Vertex AI shape: 100 prompt + 10 answer + 40 thinking = 150.
			name:           "thoughts reported separately from candidates",
			usageMeta:      `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":40,"totalTokenCount":150}`,
			wantPrompt:     100,
			wantCompletion: 50,
			wantReasoning:  40,
		},
		{
			// Tool results are input fed back to the model, and Google counts
			// them in the total: prompt 100 + tools 200 + answer 10 + thinking
			// 40 = 350. Leaving them in the subtraction would bill 250 output
			// tokens instead of 50, at the (higher) output rate.
			name:           "tool-use prompt billed as input",
			usageMeta:      `{"promptTokenCount":100,"toolUsePromptTokenCount":200,"candidatesTokenCount":10,"thoughtsTokenCount":40,"totalTokenCount":350}`,
			wantPrompt:     300,
			wantCompletion: 50,
			wantReasoning:  40,
		},
		{
			// Tool use without thinking — the common function-calling shape.
			name:           "tool-use prompt without thinking",
			usageMeta:      `{"promptTokenCount":100,"toolUsePromptTokenCount":200,"candidatesTokenCount":10,"totalTokenCount":310}`,
			wantPrompt:     300,
			wantCompletion: 10,
			wantReasoning:  0,
		},
		{
			// Google AI shape: candidates already covers the 40 thinking tokens,
			// so total is 100 + 50. Summing would wrongly yield 90.
			name:           "thoughts already folded into candidates",
			usageMeta:      `{"promptTokenCount":100,"candidatesTokenCount":50,"thoughtsTokenCount":40,"totalTokenCount":150}`,
			wantPrompt:     100,
			wantCompletion: 50,
			wantReasoning:  40,
		},
		{
			// No thinking at all — the ordinary case must stay untouched.
			name:           "non-thinking model",
			usageMeta:      `{"promptTokenCount":100,"candidatesTokenCount":10,"totalTokenCount":110}`,
			wantPrompt:     100,
			wantCompletion: 10,
			wantReasoning:  0,
		},
		{
			// Total missing: fall back to the sum rather than reporting no output.
			name:           "total omitted falls back to sum",
			usageMeta:      `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":40}`,
			wantPrompt:     100,
			wantCompletion: 50,
			wantReasoning:  40,
		},
		{
			// A total at or below the prompt can't be right; the sum is the
			// safer reading.
			name:           "total not larger than prompt falls back to sum",
			usageMeta:      `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":40,"totalTokenCount":100}`,
			wantPrompt:     100,
			wantCompletion: 50,
			wantReasoning:  40,
		},
		{
			// promptTokenCount already includes the cached portion, so a cache
			// hit must not shift the subtraction.
			name:           "cached prompt does not distort the subtraction",
			usageMeta:      `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":40,"cachedContentTokenCount":80,"totalTokenCount":150}`,
			wantPrompt:     100,
			wantCompletion: 50,
			wantReasoning:  40,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := json.RawMessage(`{
				"candidates": [{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],
				"usageMetadata":` + tc.usageMeta + `,
				"modelVersion":"gemini-2.5-pro"
			}`)
			irResp, err := ResponseDecoder{}.DecodeResponse(body)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			if irResp.Usage.PromptTokens != tc.wantPrompt {
				t.Errorf("PromptTokens = %d, want %d", irResp.Usage.PromptTokens, tc.wantPrompt)
			}
			if irResp.Usage.CompletionTokens != tc.wantCompletion {
				t.Errorf("CompletionTokens = %d, want %d", irResp.Usage.CompletionTokens, tc.wantCompletion)
			}
			if irResp.Usage.ReasoningTokens != tc.wantReasoning {
				t.Errorf("ReasoningTokens = %d, want %d", irResp.Usage.ReasoningTokens, tc.wantReasoning)
			}
			// The triple must be self-consistent whichever branch produced the
			// completion count, otherwise the gateway rejects the whole usage
			// as incoherent and bills nothing.
			if got := irResp.Usage.PromptTokens + irResp.Usage.CompletionTokens; got > irResp.Usage.TotalTokens {
				t.Errorf("prompt+completion = %d exceeds total %d", got, irResp.Usage.TotalTokens)
			}

			// The streaming path must agree exactly — both decoders share
			// usageMetadata, and this is what keeps them from drifting.
			dec := NewStreamDecoder()
			deltas, _ := dec.DecodeChunk("data: {\"usageMetadata\":" + tc.usageMeta + ",\"candidates\":[]}\n\n")
			found := false
			for _, d := range deltas {
				u, ok := d.(protocols.DeltaUsage)
				if !ok {
					continue
				}
				found = true
				if u.Usage.CompletionTokens != tc.wantCompletion {
					t.Errorf("stream CompletionTokens = %d, want %d", u.Usage.CompletionTokens, tc.wantCompletion)
				}
			}
			if !found {
				t.Error("missing protocols.DeltaUsage")
			}
		})
	}
}

// TestGeminiNegativeUsageRejected: folding thoughts into candidates sums two
// numbers, and a sum hides the sign of its parts — candidates=10 with
// thoughts=-5 would look like an ordinary 5 by the time the gateway's coherence
// check sees it. The block must therefore be rejected at decode time, leaving
// usage unknown (which bills nothing) instead of billing a laundered figure.
func TestGeminiNegativeUsageRejected(t *testing.T) {
	bodies := map[string]string{
		"negative thoughts":   `{"promptTokenCount":100,"candidatesTokenCount":10,"thoughtsTokenCount":-5,"totalTokenCount":105}`,
		"negative candidates": `{"promptTokenCount":100,"candidatesTokenCount":-10,"totalTokenCount":90}`,
		"negative prompt":     `{"promptTokenCount":-100,"candidatesTokenCount":10,"totalTokenCount":10}`,
		"negative cached":     `{"promptTokenCount":100,"candidatesTokenCount":10,"cachedContentTokenCount":-80,"totalTokenCount":110}`,
		"negative total":      `{"promptTokenCount":100,"candidatesTokenCount":10,"totalTokenCount":-1}`,
	}
	for name, um := range bodies {
		t.Run(name, func(t *testing.T) {
			body := json.RawMessage(`{
				"candidates": [{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],
				"usageMetadata":` + um + `,
				"modelVersion":"gemini-2.5-pro"
			}`)
			irResp, err := ResponseDecoder{}.DecodeResponse(body)
			if err != nil {
				t.Fatalf("DecodeResponse: %v", err)
			}
			// All-zero usage is what the gateway reads as "unknown", never as
			// a free request (its reported-usage gate maps it to nil).
			if irResp.Usage.PromptTokens != 0 || irResp.Usage.CompletionTokens != 0 || irResp.Usage.TotalTokens != 0 {
				t.Errorf("expected usage left unset, got %+v", irResp.Usage)
			}
			// Counts alone are not enough: an unmarked zero triple is a
			// coherent "0 tokens", which the wire encoders serialise as an
			// all-zero object instead of null. The non-streaming path must
			// carry the same verdict the streaming one does.
			if !irResp.Usage.Invalid {
				t.Errorf("rejected block must be marked Invalid, got %+v", irResp.Usage)
			}

			// The streaming path emits a MARKED delta rather than nothing.
			// Emitting nothing was the earlier contract and it lost the
			// verdict: this decoder rejects before IRUsage.Merge ever sees the
			// frame, so an earlier valid frame's counts stayed in the
			// accumulator and the following DeltaDone billed them as coherent.
			dec := NewStreamDecoder()
			chunk := "data: {\"usageMetadata\":" + um + ",\"candidates\":[]}\n\n"
			deltas, _ := dec.DecodeChunk(chunk)
			var sawUsage bool
			for _, d := range deltas {
				u, ok := d.(protocols.DeltaUsage)
				if !ok {
					continue
				}
				sawUsage = true
				if !u.Usage.Invalid {
					t.Errorf("rejected block must be marked Invalid, got %+v", u.Usage)
				}
				if u.Usage.PromptTokens != 0 || u.Usage.CompletionTokens != 0 || u.Usage.TotalTokens != 0 {
					t.Errorf("a rejected block must carry no counts, got %+v", u.Usage)
				}
				// The mark has to survive the accumulation every relay performs.
				acc := protocols.IRUsage{PromptTokens: 500, CompletionTokens: 5}
				acc.Merge(u.Usage)
				if !acc.Invalid {
					t.Error("Invalid must propagate through Merge, or earlier counts stay billable")
				}
			}
			if !sawUsage {
				t.Error("expected a marked usage delta so the rejection reaches the accumulator")
			}
		})
	}
}

// TestBuildGeminiUsage_RefusesReasoningExceedingCompletion covers the
// cross-protocol shape that made the reasoning-subset rule necessary. An
// OpenAI-shaped upstream reporting 100 reasoning tokens inside a 10-token
// completion reaches this encoder after the decoder settled the verdict; without
// that verdict the split below clamps candidates at 0 and still publishes
// thoughts=100 against a total derived from the 10-token completion, emitting a
// usageMetadata whose own parts overshoot its stated total.
func TestBuildGeminiUsage_RefusesReasoningExceedingCompletion(t *testing.T) {
	u := protocols.IRUsage{
		PromptTokens:          5,
		CompletionTokens:      10,
		TotalTokens:           15,
		ReasoningTokens:       100,
		CacheIncludedInPrompt: true,
	}
	// The verdict every decoder settles at its exit.
	u.Invalid = u.IsIncoherent()
	if !u.Invalid {
		t.Fatalf("reasoning exceeding completion must be incoherent before it reaches the encoder, got %+v", u)
	}
	if meta := buildGeminiUsage(u); meta != nil {
		t.Errorf("Gemini encoder must publish nothing for a record it cannot split coherently, got %v", meta)
	}
}

// TestBuildGeminiUsage_ThoughtsSumStaysIntact is the invariant the split exists
// to preserve, asserted directly rather than implied: Gemini documents
// totalTokenCount as prompt + thoughts + candidates, so for any record the
// encoder does publish, those three must add back up to the total it states.
func TestBuildGeminiUsage_ThoughtsSumStaysIntact(t *testing.T) {
	u := protocols.IRUsage{
		PromptTokens:          100,
		CompletionTokens:      50,
		TotalTokens:           150,
		ReasoningTokens:       20,
		CacheIncludedInPrompt: true,
	}
	u.Invalid = u.IsIncoherent()
	meta := buildGeminiUsage(u)
	if meta == nil {
		t.Fatal("a coherent thinking record must be published, got nil")
	}
	prompt := meta["promptTokenCount"].(int)
	candidates := meta["candidatesTokenCount"].(int)
	thoughts := meta["thoughtsTokenCount"].(int)
	total := meta["totalTokenCount"].(int)
	if prompt+candidates+thoughts != total {
		t.Errorf("prompt(%d) + candidates(%d) + thoughts(%d) = %d, want totalTokenCount %d",
			prompt, candidates, thoughts, prompt+candidates+thoughts, total)
	}
}

// TestGeminiUsage_StaleTotalCannotUndercutCandidates covers the shape an
// OpenAI-compatible front produces when it fills totalTokenCount from a stale
// snapshot while the per-part counts are current. The subtraction still returns
// a positive number, so nothing downstream would question it — the record is
// coherent by every rule and simply bills half the output the payload itself
// reports.
func TestGeminiUsage_StaleTotalCannotUndercutCandidates(t *testing.T) {
	body := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "hi"}]}, "finishReason": "STOP"}],
		"usageMetadata": {
			"promptTokenCount": 100, "candidatesTokenCount": 100,
			"thoughtsTokenCount": 0, "totalTokenCount": 150
		}
	}`)

	resp, err := ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.CompletionTokens != 100 {
		t.Errorf("CompletionTokens = %d, want 100: candidatesTokenCount already vouches for 100 generated tokens, a lagging total must not argue it down",
			resp.Usage.CompletionTokens)
	}
}

// TestGeminiUsage_ThinkingInsideCandidatesIsNotDoubleCounted is the constraint
// that decides HOW the floor is applied. On the Google AI endpoint thinking is
// already counted inside candidatesTokenCount, so flooring at
// candidates + thoughts would charge those tokens twice — and no field in the
// response says which backend convention is in force. Flooring at candidates
// alone is safe under both.
func TestGeminiUsage_ThinkingInsideCandidatesIsNotDoubleCounted(t *testing.T) {
	body := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "hi"}]}, "finishReason": "STOP"}],
		"usageMetadata": {
			"promptTokenCount": 100, "candidatesTokenCount": 100,
			"thoughtsTokenCount": 20, "totalTokenCount": 200
		}
	}`)

	resp, err := ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.CompletionTokens != 100 {
		t.Errorf("CompletionTokens = %d, want 100 (total 200 - prompt 100); 120 would double-charge the thinking already inside candidatesTokenCount",
			resp.Usage.CompletionTokens)
	}
}

// TestGeminiUsage_VertexSplitStillUsesTheTotal guards the other convention:
// where thoughts are reported BESIDE candidates, the total-based derivation must
// still win over the candidates floor, or thinking tokens would stop being
// billed at all.
func TestGeminiUsage_VertexSplitStillUsesTheTotal(t *testing.T) {
	body := []byte(`{
		"candidates": [{"content": {"role": "model", "parts": [{"text": "hi"}]}, "finishReason": "STOP"}],
		"usageMetadata": {
			"promptTokenCount": 100, "candidatesTokenCount": 30,
			"thoughtsTokenCount": 20, "totalTokenCount": 150
		}
	}`)

	resp, err := ResponseDecoder{}.DecodeResponse(body)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if resp.Usage.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50 (30 candidates + 20 thoughts, derived from the total)",
			resp.Usage.CompletionTokens)
	}
}

// TestGeminiStreaming_CoherentBlockGetsFullVerdict locks the streaming path to
// the same completeness verdict the non-streaming path applies at its exit: a
// block that parses (ok=true) yet violates the reasoning-subset rule
// (thoughts > completion) must be marked Invalid on the wire, not emitted as a
// coherent-looking record. Before this test, only the !ok rejection was marked;
// the stale-total shape sailed through streaming while the identical payload
// was refused on the non-streaming path.
func TestGeminiStreaming_CoherentBlockGetsFullVerdict(t *testing.T) {
	dec := NewStreamDecoder()
	chunk := "data: {\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":10,\"thoughtsTokenCount\":200,\"totalTokenCount\":150},\"candidates\":[]}\n\n"
	deltas, _ := dec.DecodeChunk(chunk)
	var sawUsage bool
	for _, d := range deltas {
		u, ok := d.(protocols.DeltaUsage)
		if !ok {
			continue
		}
		sawUsage = true
		if !u.Usage.Invalid {
			t.Errorf("streaming block with thoughts(%d) > completion(%d) must be marked Invalid, got %+v",
				u.Usage.ReasoningTokens, u.Usage.CompletionTokens, u.Usage)
		}
	}
	if !sawUsage {
		t.Error("expected a usage delta carrying the verdict")
	}
}
