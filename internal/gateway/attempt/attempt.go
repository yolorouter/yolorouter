// Package attempt owns the state that describes the attempt the gateway is
// currently making: which candidate, which provider, which key, the URL the
// request was dispatched to, and the verdict the attempt left behind for the
// terminal to quote. The fields are unexported and every write goes through a method, so
// the compiler holds what used to be a convention spread over one relay file:
// one owner, and no way to update half the family.
//
// The family has two lifetimes, and the split is deliberate:
//
//   - The verdict is cleared BEFORE a loop's early exits (ClearVerdict), so a
//     chain that runs out of budget or keys is never reported as ending on a
//     verdict some earlier attempt produced.
//   - Candidate, provider, and URL are reset only when a candidate is
//     actually entered (BeginCandidate) — an exit that fires before that
//     keeps the previous attempt's values on purpose, because the audit row
//     records the last attempt actually made, and wiping them would file an
//     exhausted chain under no provider at all.
package attempt

import (
	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/model"
)

// State is the current attempt's identity and outcome. The zero value is
// ready to use.
type State struct {
	candidate *model.ModelCandidate
	// provider is set only once the candidate's provider proved usable, so a
	// candidate dropped early never shows a provider it did not reach.
	provider *model.Provider
	// key is the provider key the current rotation step holds, set as each
	// key is entered, so any exit taken after the entry reports this key
	// rather than an earlier one.
	key *model.ProviderKey
	// upstreamURL is the redacted URL of the current dispatch; empty until a
	// request was actually built.
	upstreamURL string
	// verdict is what the most recent attempt asked the terminal to quote.
	verdict decision.StickyVerdict
}

// BeginCandidate enters a candidate: the whole state is replaced, so a field
// added to this struct later is reset here by construction rather than by
// someone remembering to extend a list of clears.
func (s *State) BeginCandidate(cand *model.ModelCandidate) {
	*s = State{candidate: cand}
}

// ClearVerdict drops the previous attempt's verdict without touching the
// rest of the state. Called before a loop's early exits — and before the
// skip checks that can pass a key over without dispatching it — so the exit
// paths that never reach a new attempt cannot quote an old one.
func (s *State) ClearVerdict() { s.verdict = decision.StickyVerdict{} }

// BindProvider records that this candidate's provider proved usable.
func (s *State) BindProvider(p *model.Provider) { s.provider = p }

// BindKey records the key the rotation loop is currently holding.
func (s *State) BindKey(k *model.ProviderKey) { s.key = k }

// BeginUpstreamAttempt clears the dispatch URL so a build that fails before
// sending never inherits the previous attempt's URL in its record.
func (s *State) BeginUpstreamAttempt() { s.upstreamURL = "" }

// SetUpstreamURL records the (already redacted) URL this attempt dispatched
// to. Overwritten per attempt — the last write wins, matching the audit
// row's "successful attempt, else the last attempt" rule.
func (s *State) SetUpstreamURL(redacted string) { s.upstreamURL = redacted }

// HoldVerdict records the verdict this attempt asks the terminal to quote if
// the chain then runs out.
func (s *State) HoldVerdict(v decision.StickyVerdict) { s.verdict = v }

// Candidate is the candidate being attempted, nil before the first one.
func (s *State) Candidate() *model.ModelCandidate { return s.candidate }

// Provider is the usable provider of the current candidate, nil until bound.
func (s *State) Provider() *model.Provider { return s.provider }

// Key is the provider key the current rotation step holds, nil before the
// first key of a candidate is entered.
func (s *State) Key() *model.ProviderKey { return s.key }

// UpstreamURL is the redacted URL of the last dispatch, empty when none was
// built.
func (s *State) UpstreamURL() string { return s.upstreamURL }

// Verdict is the held verdict; ask Held() whether it says anything.
func (s *State) Verdict() decision.StickyVerdict { return s.verdict }
