package gateway

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

func TestReportedUsageGate(t *testing.T) {
	tests := []struct {
		name string
		in   *protocols.IRUsage
		want *protocols.IRUsage
	}{
		{
			name: "nil input",
			in:   nil,
			want: nil,
		},
		{
			name: "all-zero usage treated as unknown",
			in:   &protocols.IRUsage{},
			want: nil,
		},
		{
			// Every field, including CacheIncludedInPrompt, maps through
			// verbatim. netPromptTokens (log.go) — not this conversion —
			// derives the net input from PromptTokens and the flag.
			name: "full mapping preserves fields and flag",
			in: &protocols.IRUsage{
				PromptTokens:          100,
				CompletionTokens:      50,
				TotalTokens:           150,
				CacheWriteTokens:      10,
				CacheReadTokens:       20,
				CacheIncludedInPrompt: true,
			},
			want: &protocols.IRUsage{
				PromptTokens:          100,
				CompletionTokens:      50,
				TotalTokens:           150,
				CacheWriteTokens:      10,
				CacheReadTokens:       20,
				CacheIncludedInPrompt: true,
			},
		},
		{
			name: "only completion tokens known is not treated as empty",
			in:   &protocols.IRUsage{CompletionTokens: 50},
			want: &protocols.IRUsage{CompletionTokens: 50},
		},
		{
			// An exchange can spend money without spending tokens. The provider
			// runs searches on its own initiative and charges for them
			// separately; the count arrives once, in the usage the response
			// ends with. Judging emptiness by the token fields alone drops the
			// record whole, and nothing downstream can re-derive the count
			// because the body it was read from is gone by then.
			name: "searches with no tokens is usage, not an empty record",
			in:   &protocols.IRUsage{WebSearchCount: 3},
			want: &protocols.IRUsage{WebSearchCount: 3},
		},
		{
			// Same shape, different line: reasoning tokens are their own
			// dimension and were dropped by the same test.
			name: "reasoning with no other counts is usage too",
			in:   &protocols.IRUsage{ReasoningTokens: 12},
			want: &protocols.IRUsage{ReasoningTokens: 12},
		},
		{
			// An impossible record that states no quantity at all is still
			// nothing reported. The verdict has nothing to attach to: there are
			// no counts to withhold from pricing and none to show an operator,
			// so admitting it would only turn "could not be priced" into
			// "priced at zero" — a claim a dashboard adds up.
			//
			// The verdict matters when it arrives WITH counts, and that case is
			// covered where it can actually go wrong: the delivery round trip,
			// in the observer file.
			name: "an impossible record stating no quantity is still nothing reported",
			in:   &protocols.IRUsage{Invalid: true},
			want: nil,
		},
		{
			// Cache counts alone answer "worth putting back on the wire",
			// not "the upstream stated a billable quantity". Admitting a
			// cache-only record would start pricing a shape this gate has
			// always dropped — the trap swapping the emptiness check for
			// protocols.HasAnyUsage would fall into.
			name: "cache-only record is still nothing reported",
			in:   &protocols.IRUsage{CacheReadTokens: 40, CacheWriteTokens: 7},
			want: nil,
		},
		// Regression for the cache-inclusion billing bug: computeCost
		// (log.go) does PromptTokens - CacheReadTokens, assuming OpenAI
		// semantics where PromptTokens already includes cache-read. The
		// Claude decoder reports input_tokens EXCLUDING cache-read and
		// leaves CacheIncludedInPrompt at its zero value (false); without
		// normalizing here, computeCost double-subtracts cache-read and
		// undercharges. false must add CacheReadTokens into PromptTokens;
		// true (chat/gemini/responses, which already include it) must be
		// passed through unchanged.
		{
			name: "claude semantics (flag false) passes prompt through as net input",
			in: &protocols.IRUsage{
				PromptTokens:          100,
				CacheReadTokens:       30,
				CacheIncludedInPrompt: false,
			},
			want: &protocols.IRUsage{
				PromptTokens:          100,
				CacheReadTokens:       30,
				CacheIncludedInPrompt: false,
			},
		},
		{
			name: "openai/gemini/responses semantics (flag true) passes prompt and flag through",
			in: &protocols.IRUsage{
				PromptTokens:          100,
				CacheReadTokens:       30,
				CacheIncludedInPrompt: true,
			},
			want: &protocols.IRUsage{
				PromptTokens:          100,
				CacheReadTokens:       30,
				CacheIncludedInPrompt: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reportedUsage(tt.in)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("reportedUsage() = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("reportedUsage() = nil, want %+v", tt.want)
			}
			if *got != *tt.want {
				t.Fatalf("reportedUsage() = %+v, want %+v", *got, *tt.want)
			}
		})
	}
}

// TestReportedUsageReturnsAnIndependentSnapshot pins the clone semantics the
// streaming path depends on: the pump takes a snapshot of a live accumulator
// that keeps merging frames afterwards, and a gate that returned the shared
// pointer would let later frames rewrite a record that was already handed
// over — the usage a disconnect settles on would silently keep growing.
func TestReportedUsageReturnsAnIndependentSnapshot(t *testing.T) {
	acc := protocols.IRUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	snap := reportedUsage(&acc)
	if snap == nil {
		t.Fatal("reportedUsage() = nil for a record with counts")
	}
	acc.Merge(protocols.IRUsage{CompletionTokens: 50, TotalTokens: 60})
	if snap.CompletionTokens != 5 || snap.TotalTokens != 15 {
		t.Fatalf("snapshot mutated by a later merge: %+v — the gate must clone, not share", *snap)
	}
}
