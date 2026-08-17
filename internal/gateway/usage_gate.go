package gateway

import "github.com/yolorouter/yolorouter/internal/protocols"

// reportedUsage is the gate between "the upstream stated a quantity" and
// "nothing was reported": nil when the record states nothing, an independent
// snapshot otherwise. The distinction exists because IRUsage cannot express
// it by itself — its plain int fields represent "missing" and "present but
// zero" identically, so an all-zero record has to be read as absent here or
// a response that never carried usage would be recorded as a zero-cost row.
//
// "Nothing was reported" is asked of every dimension the upstream can state
// a quantity in, not only the three token counts. Searches the provider ran
// on its own initiative and reasoning tokens are each their own line, and a
// record carrying one of them and no tokens was being dropped whole: the
// usage became nil, no observer was told anything, and the number could not
// be re-derived afterwards because the body it was read from is gone.
//
// Deliberately NOT protocols.HasAnyUsage, which looks like the same
// question and is not. That predicate answers "is there anything worth
// putting back on the wire", so it folds in the cache counts and returns
// false for a record judged impossible. Both of those belong to billing:
// admitting cache-only records here would start pricing a shape this gate
// used to drop, and treating an impossible record as absent would erase the
// verdict instead of carrying it. Whether an upstream SAID something and
// whether what it said can be BILLED are different questions, and this one
// is the first.
//
// The clone is load-bearing, not defensive style: the streaming path calls
// this against a live accumulator that keeps merging frames after the
// snapshot is taken, and a shared pointer would let later frames rewrite a
// record that was already handed over.
func reportedUsage(u *protocols.IRUsage) *protocols.IRUsage {
	if u == nil {
		return nil
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.ReasoningTokens == 0 && u.WebSearchCount == 0 {
		return nil
	}
	c := *u
	return &c
}
