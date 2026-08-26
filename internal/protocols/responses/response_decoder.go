package responses

import (
	"encoding/json"
	"fmt"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"strings"
)

// ResponseDecoder parses an OpenAI Responses API non-streaming response into IR.
// Used to reverse-decode responses after any client protocol is routed to a Responses upstream.
type ResponseDecoder struct{}

// responsesResponseWire mirrors the Responses API wire format (for decoding).
// Kept private: protocol-layer wire types are not exposed outside; callers only see IR.
type responsesResponseWire struct {
	ID                string                   `json:"id"`
	Object            string                   `json:"object"`
	Model             string                   `json:"model"`
	Status            *string                  `json:"status,omitempty"`
	Output            []responsesOutputItem    `json:"output"`
	Usage             *responsesUsage          `json:"usage,omitempty"`
	IncompleteDetails *responsesIncompleteWire `json:"incomplete_details,omitempty"`
	Error             *responsesErrorWire      `json:"error,omitempty"`
}

type responsesOutputItem struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"id,omitempty"`
	Role      string                 `json:"role,omitempty"`
	Content   []responsesPartWire    `json:"content,omitempty"`
	Status    string                 `json:"status,omitempty"`
	Summary   []responsesSummaryWire `json:"summary,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
}

type responsesPartWire struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type responsesSummaryWire struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type responsesUsage struct {
	InputTokens         int                     `json:"input_tokens"`
	OutputTokens        int                     `json:"output_tokens"`
	TotalTokens         int                     `json:"total_tokens"`
	InputTokensDetails  *responsesInputDetails  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *responsesOutputDetails `json:"output_tokens_details,omitempty"`
	// Non-standard cache-write breakdown; see protocols.CacheWriteAliasField.
	// Like cached_tokens it sits INSIDE input_tokens, so it is recorded rather
	// than added.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

type responsesInputDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
	// Cache WRITE, nested beside cached_tokens. This is OpenAI's OWN field, not
	// an extension: the official OpenAPI spec declares
	// ResponseUsage.input_tokens_details.cache_write_tokens and lists it as
	// REQUIRED. An earlier revision of this comment called it an extension —
	// that was wrong, and the mistake came from checking a third party's
	// rendition of the schema instead of the vendor spec itself.
	//
	// A pointer so "field absent" is distinguishable from "field present and
	// 0"; precedence over the top-level alias is by presence, not by value.
	CacheWriteTokens *int `json:"cache_write_tokens"`
}

type responsesOutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// responsesUsageToIR converts a wire usage object into IRUsage.
//
// Single conversion point for every path — success, non-streaming failure and
// streaming — so none of them can quietly drop a field. The failure path used
// to copy only input/output/total: because NewIRResponse marks usage as
// gross-input (CacheIncludedInPrompt), the missing cached_tokens made
// netPromptTokens treat the whole input as regular input instead of deducting
// the cache-read portion, systematically over-stating provider_cost /
// account_cost and channel margin analysis even though the user is not charged.
func responsesUsageToIR(u *responsesUsage) protocols.IRUsage {
	var ir protocols.IRUsage
	if u == nil {
		return ir
	}
	ir.PromptTokens = u.InputTokens
	ir.CompletionTokens = u.OutputTokens
	ir.TotalTokens = u.TotalTokens
	// Set unconditionally, matching the chat and gemini decoders. Responses is an
	// OpenAI-convention protocol: input_tokens ALWAYS includes cached tokens,
	// cache hit or not. Gating it on CachedTokens > 0 was safe only while callers
	// merged field-wise (Merge migrates the flag one-way false→true); the
	// non-streaming path now assigns this struct wholesale over a value that
	// NewIRResponse had pre-set to true, so gating it silently flipped every
	// cache-miss response to the Anthropic (net-input) convention.
	ir.CacheIncludedInPrompt = true
	if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens > 0 {
		ir.CacheReadTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		ir.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	// Exactly one cache-write spelling is taken, never summed: the nested
	// breakdown is the standard one and wins, the top-level alias is the
	// fallback for new-api-style peers. Precedence is by field PRESENCE, so an
	// explicitly reported 0 beats a stale non-zero alias, and a negative value
	// survives for the billing gate to refuse.
	ir.CacheWriteTokens = u.CacheCreationInputTokens
	if u.InputTokensDetails != nil && u.InputTokensDetails.CacheWriteTokens != nil {
		ir.CacheWriteTokens = *u.InputTokensDetails.CacheWriteTokens
	}
	return ir
}

type responsesIncompleteWire struct {
	Reason string `json:"reason"`
}

type responsesErrorWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (ResponseDecoder) DecodeResponse(body json.RawMessage) (*protocols.IRResponse, error) {
	var wire responsesResponseWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("parse responses response: %w", err)
	}

	// An upstream may declare status=failed or error!=nil inside an HTTP 200 body. Not detecting
	// that would let a failed response be encoded to the
	// client as a success — the circuit breaker would record a success and it would be billed as
	// a 2xx. Even on failure, wire.Usage (if the upstream sent it) is extracted so provider-cost
	// analysis does not lose data; (partialResp, error) is returned so the caller routes the
	// request onto the failure path (502, not billed).
	// Reject only EXPLICIT non-served terminal states — deliberately a blacklist,
	// not a whitelist.
	//
	// The risk here is sharply asymmetric. On the same-protocol passthrough path
	// on the same-protocol passthrough path the upstream's 200 and its
	// full body are written to the client BEFORE this decoder ever runs, so a
	// false rejection means: the client holds a perfect answer, we bill nothing,
	// and a healthy provider takes a circuit-breaker failure. A false ACCEPT only
	// means one anomalous response gets billed.
	//
	// "status" is also absent from the schema's required list
	// (id/object/created_at/error/incomplete_details/instructions/model/tools/
	// output/parallel_tool_calls/metadata/tool_choice/temperature/top_p), and a
	// Go-based compat relay serialising `Status string` without omitempty emits
	// `"status": ""`. Both an absent and an empty status must therefore be served.
	status := ""
	if wire.Status != nil {
		status = *wire.Status
	}
	if wire.Error != nil || StatusIsNonServed(status) {
		partial := protocols.NewIRResponse(wire.ID, wire.Model)
		if wire.Usage != nil {
			partial.Usage = responsesUsageToIR(wire.Usage)
			// Same verdict-at-exit rule as the success path, so a partial record
			// on the failure path carries the same Invalid the billing gate reads.
			partial.Usage.Invalid = partial.Usage.IsIncoherent()
		}
		if wire.Error != nil {
			return partial, fmt.Errorf("upstream responses returned error: code=%q message=%q", wire.Error.Code, wire.Error.Message)
		}
		return partial, fmt.Errorf("upstream responses status=%q is not a served terminal state", status)
	}

	resp := protocols.NewIRResponse(wire.ID, wire.Model)

	// Walk the output array, concatenating text/reasoning content and tool calls
	var textBuf, reasoningBuf string
	for _, item := range wire.Output {
		switch item.Type {
		case "message":
			for _, p := range item.Content {
				switch p.Type {
				case "output_text":
					textBuf += p.Text
				case "refusal":
					textBuf += p.Refusal
				}
			}
		case "reasoning":
			for _, s := range item.Summary {
				if s.Type == "summary_text" || s.Type == "" {
					reasoningBuf += s.Text
				}
			}
		case "function_call":
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			resp.ToolCalls = append(resp.ToolCalls, protocols.IRToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: args,
			})
		}
	}
	resp.Content = textBuf
	resp.ReasoningContent = reasoningBuf

	// usage
	if wire.Usage != nil {
		resp.Usage = responsesUsageToIR(wire.Usage)
		// Set the verdict at the IR exit so every consumer reads Invalid instead
		// of re-judging on data the conversion has since distorted. See
		// IRUsage.IsIncoherent.
		resp.Usage.Invalid = resp.Usage.IsIncoherent()
	}

	// Stop reason inference: when status is "incomplete", prefer incomplete_details.reason;
	// otherwise return "tool_use" if there are tool calls; otherwise leave it empty
	// (the client encoder decides the default value).
	switch status {
	case "incomplete":
		// status=incomplete is abnormal regardless of whether the server
		// bothered to send incomplete_details — checking for that object first
		// let a null one fall through to the tool-call inference, turning a
		// truncated run into an apparently clean tool finish.
		reason := ""
		if wire.IncompleteDetails != nil {
			reason = wire.IncompleteDetails.Reason
		}
		resp.StopReason = normalizeResponsesIncompleteReason(reason)
	default:
		// completed, absent, empty, or any future value that was not rejected
		// above. Only such a run may be attributed to a tool call.
		// IR vocabulary is "tool_calls"; each egress encoder maps it onto its
		// own protocol's word (Anthropic's is "tool_use").
		if len(resp.ToolCalls) > 0 {
			resp.StopReason = "tool_calls"
		} else {
			resp.StopReason = "stop"
		}
	}

	return resp, nil
}

// StatusIsNonServed reports whether an EXPLICIT status means the request was
// not served and must not be settled as a success.
//
// Shared by the streaming and non-streaming decoders so the two can never give
// opposite verdicts on the same payload (they briefly did: one listed queued /
// in_progress, the other did not). Exported because the provider credential
// test judges success bodies by the same list — a probe must never certify a
// status the runtime decoders would refuse to serve.
//
// Deliberately only failed | cancelled:
//   - queued / in_progress are legitimate 200 bodies for `background: true`
//     runs, where the client EXPECTS a non-terminal status and polls. Rejecting
//     them turned a correct response into a 502 with no billing and a
//     circuit-breaker hit on a healthy channel.
//   - absent / null / empty / unknown values are served too. The risk is
//     asymmetric: on the same-protocol passthrough path the client already has
//     the full body before this runs, so a false rejection is far more expensive
//     than a false accept.
func StatusIsNonServed(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "cancelled":
		return true
	}
	return false
}
