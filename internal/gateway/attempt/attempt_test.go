package attempt

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/gateway/rows"
)

// fullState builds a State with every field populated, through the public
// writes, so the reset tests below cannot pass by only clearing the fields
// that existed when they were written.
func fullState() State {
	var s State
	s.BeginCandidate(&rows.ModelCandidate{ID: 1})
	s.BindProvider(&rows.Provider{ID: 2})
	s.SetUpstreamURL("https://api.example.com/v1/chat")
	s.HoldVerdict(decision.StickyVerdict{Status: 429, ErrType: "rate_limit_error", Reason: "throttled"})
	return s
}

// TestBeginCandidateReplacesTheWholeState is the property that retired the
// hand-kept field inventory: entering a candidate resets EVERYTHING except
// the candidate being entered. A partial reset — one that preserves any old
// field — reintroduces exactly the staleness the whole-value replacement
// exists to make impossible, one field at a time.
func TestBeginCandidateReplacesTheWholeState(t *testing.T) {
	s := fullState()
	next := &rows.ModelCandidate{ID: 9}
	s.BeginCandidate(next)

	if s.Candidate() != next {
		t.Errorf("Candidate() = %v, want the candidate just entered", s.Candidate())
	}
	if s.Provider() != nil {
		t.Errorf("Provider() = %v, want nil: the previous candidate's provider survived entry", s.Provider())
	}
	if s.UpstreamURL() != "" {
		t.Errorf("UpstreamURL() = %q, want empty: the previous candidate's URL survived entry", s.UpstreamURL())
	}
	if s.Verdict().Held() {
		t.Errorf("Verdict() = %+v, want nothing held: the previous candidate's verdict survived entry", s.Verdict())
	}
}

// TestClearVerdictTouchesNothingElse: the verdict clears before a loop's
// early exits, where the rest of the state must SURVIVE — an exhausted chain
// keeps the last attempt's identity for the audit row. A clear that also
// wiped the identity would file that chain under no provider at all.
func TestClearVerdictTouchesNothingElse(t *testing.T) {
	s := fullState()
	s.ClearVerdict()

	if s.Verdict().Held() {
		t.Errorf("Verdict() = %+v, want cleared", s.Verdict())
	}
	if s.Candidate() == nil || s.Provider() == nil || s.UpstreamURL() == "" {
		t.Errorf("ClearVerdict wiped identity it must preserve: candidate=%v provider=%v url=%q",
			s.Candidate(), s.Provider(), s.UpstreamURL())
	}
}

// TestBeginUpstreamAttemptClearsOnlyTheURL: a rebuilt attempt must not
// inherit the previous attempt's dispatch URL, but the candidate identity
// and any held verdict belong to lifetimes this call has no business ending.
func TestBeginUpstreamAttemptClearsOnlyTheURL(t *testing.T) {
	s := fullState()
	s.BeginUpstreamAttempt()

	if s.UpstreamURL() != "" {
		t.Errorf("UpstreamURL() = %q, want empty", s.UpstreamURL())
	}
	if s.Candidate() == nil || s.Provider() == nil {
		t.Errorf("BeginUpstreamAttempt wiped candidate identity: candidate=%v provider=%v",
			s.Candidate(), s.Provider())
	}
	if !s.Verdict().Held() {
		t.Error("BeginUpstreamAttempt dropped a held verdict; only ClearVerdict and " +
			"BeginCandidate may end that lifetime")
	}
}
