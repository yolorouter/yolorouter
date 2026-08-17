package gateway

import (
	"reflect"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// TestEveryCountSurvivesTheDeliveryHop is the machine check for a mistake this
// package has made twice.
//
// The counts a request is billed on are copied by hand between two vocabularies
// — protocols.IRUsage and the delivery's fact.UsageReported — and back again on
// settlement. A field left out of one of those copy sites compiles, passes
// every other test, and shows up only as a wrong number in a bill: reasoning
// tokens went missing that way once, the stated total a second time. Neither
// was caught by a test; both were found by reading the code.
//
// So the check is built to fail rather than to pass. Every count is a distinct
// prime, so a crossed wire moves a value somewhere it does not belong instead
// of cancelling out, and the whole struct is compared back rather than the
// fields somebody thought to list.
//
// The two flags need their own cases. They are the only same-typed pair here,
// and a fixture setting both to true cannot tell a correct mapping from one
// that swaps them — the trip returns the same pair either way, while a real
// record carrying only ONE of them would come back saying the other. So the
// flags are varied across cases while the counts stay fixed.
func TestEveryCountSurvivesTheDeliveryHop(t *testing.T) {
	// Distinct primes so a crossed wire cannot cancel out, and coherent as a
	// record (reasoning below completion, cache within prompt) so the trip is
	// the only thing under test.
	counts := protocols.IRUsage{
		PromptTokens:     101,
		CompletionTokens: 97,
		TotalTokens:      211,
		CacheWriteTokens: 19,
		CacheReadTokens:  17,
		ReasoningTokens:  23,
		WebSearchCount:   29,
	}

	for _, tc := range []struct {
		name                  string
		cacheIncludedInPrompt bool
		invalid               bool
	}{
		{"both flags set", true, true},
		{"only the cache convention", true, false},
		{"only the impossibility verdict", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := counts
			sent.CacheIncludedInPrompt = tc.cacheIncludedInPrompt
			sent.Invalid = tc.invalid

			// One case must state every field. A zero anywhere in it would let a
			// dropped field look identical to one that was carried, which is
			// exactly the failure this test exists to catch — and it is what
			// turns this red when a tenth field is added and nobody maps it.
			if tc.cacheIncludedInPrompt && tc.invalid {
				v := reflect.ValueOf(sent)
				for i := 0; i < v.NumField(); i++ {
					if v.Field(i).IsZero() {
						t.Fatalf("the fixture leaves %s at its zero value: a field this test does "+
							"not distinguish is a field it cannot prove survived the trip",
							v.Type().Field(i).Name)
					}
				}
			}

			got := usageFromReport(usageReportOf(&sent))
			if got == nil {
				t.Fatal("the usage did not survive the delivery hop at all")
			}
			if !reflect.DeepEqual(*got, sent) {
				t.Errorf("counts changed across the delivery hop:\n sent = %+v\n back = %+v\n"+
					"a field that differs was dropped or crossed by usageReportOf or "+
					"usageFromReport, and the request is billed on what came back", sent, *got)
			}
		})
	}
}
