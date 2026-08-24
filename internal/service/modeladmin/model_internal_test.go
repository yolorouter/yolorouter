package modeladmin

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
)

// These tests exercise unexported classification internals directly; the
// rest of the suite drives the exported surface from the external test
// package next door.

// classifyCapabilityResult only ever confirms. Every row here is a case that once
// produced a false negative, or would have: because these flags are shown to the
// admin rather than used for routing, an unconfirmed capability is reported as
// unknown instead of being guessed at.
func TestClassifyCapabilityResult(t *testing.T) {
	cases := []struct {
		name   string
		result providerclient.TestResult
		want   *bool
	}{
		{name: "success confirms", result: providerclient.TestResult{Outcome: providerclient.TestSuccess}, want: boolPtr(true)},
		// A 200 whose stream never completed looks like "streaming is missing",
		// but the validator returns this for a mid-stream reset too.
		{name: "failed validation is unknown", result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError}, want: nil},
		{name: "a refusal is unknown", result: providerclient.TestResult{Outcome: providerclient.TestUpstreamError}, want: nil},
		{name: "rate limited is unknown", result: providerclient.TestResult{Outcome: providerclient.TestRateLimited}, want: nil},
		{name: "auth failure is unknown", result: providerclient.TestResult{Outcome: providerclient.TestAuthFailed}, want: nil},
		{name: "wrong model name is unknown", result: providerclient.TestResult{Outcome: providerclient.TestModelNotFound}, want: nil},
		{name: "quota exhaustion is unknown", result: providerclient.TestResult{Outcome: providerclient.TestQuotaUnavailable}, want: nil},
		{name: "unreachable is unknown", result: providerclient.TestResult{Outcome: providerclient.TestUnreachable}, want: nil},
		{name: "an uncertifiable protocol is unknown", result: providerclient.TestResult{Outcome: providerclient.TestVerificationUnsupported}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCapabilityResult(tc.result)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("expected unknown, got %v", *got)
			case tc.want != nil && got == nil:
				t.Fatalf("expected %v, got unknown", *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("expected %v, got %v", *tc.want, *got)
			}
		})
	}
}

func TestComputeModelRunningStatusAvailableWhenFirstCandidateRoutable(t *testing.T) {
	status := computeModelRunningStatus(model.ModelSchedulingModeFailover, []CandidateView{
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
		{VerificationStatus: model.ModelVerificationStatusUntested, Routable: false},
	})
	if status != ModelRunningStatusAvailable {
		t.Fatalf("expected available, got %q", status)
	}
}

func TestComputeModelRunningStatusDegradedWhenOnlyLaterCandidateRoutable(t *testing.T) {
	status := computeModelRunningStatus(model.ModelSchedulingModeFailover, []CandidateView{
		{VerificationStatus: model.ModelVerificationStatusFailed, Routable: false},
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
	})
	if status != ModelRunningStatusDegraded {
		t.Fatalf("expected degraded, got %q", status)
	}
}

func TestComputeModelRunningStatusUnavailableWhenNoCandidateRoutable(t *testing.T) {
	status := computeModelRunningStatus(model.ModelSchedulingModeFailover, []CandidateView{
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: false},
		{VerificationStatus: model.ModelVerificationStatusFailed, Routable: false},
	})
	if status != ModelRunningStatusUnavailable {
		t.Fatalf("expected unavailable, got %q", status)
	}
}

// A balanced model has no designated head: the verdict must not depend on
// sort_order, a dead candidate among live ones reads as degraded even when
// the head is healthy, and the empty pre-migration mode keeps the failover
// reading.
func TestComputeModelRunningStatusBalancedIgnoresOrder(t *testing.T) {
	mixed := []CandidateView{
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: false},
	}
	swapped := []CandidateView{mixed[1], mixed[0]}
	if got := computeModelRunningStatus(model.ModelSchedulingModeBalanced, mixed); got != ModelRunningStatusDegraded {
		t.Fatalf("balanced with a dead candidate = %q, want degraded even with a healthy head", got)
	}
	if got := computeModelRunningStatus(model.ModelSchedulingModeBalanced, swapped); got != ModelRunningStatusDegraded {
		t.Fatalf("balanced verdict changed with order: swapped = %q, want degraded", got)
	}
	allUp := []CandidateView{
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
		{VerificationStatus: model.ModelVerificationStatusPassed, Routable: true},
	}
	if got := computeModelRunningStatus(model.ModelSchedulingModeBalanced, allUp); got != ModelRunningStatusAvailable {
		t.Fatalf("balanced with every candidate live = %q, want available", got)
	}
	// The failover reading survives for the empty legacy value: a healthy
	// head is available no matter the tail.
	if got := computeModelRunningStatus("", mixed); got != ModelRunningStatusAvailable {
		t.Fatalf("legacy empty mode = %q, want the failover reading (available)", got)
	}
}

// The bool the console greys a row with and the reason it explains it with must
// not be able to disagree: one is derived from the other.
func TestRoutableIsExactlyTheAbsenceOfAReason(t *testing.T) {
	blocked := buildCandidateView(model.ModelCandidate{}, "p", CandidateBlockedByUnverified)
	if blocked.Routable {
		t.Error("a candidate with a reason not to route is marked routable")
	}
	clear := buildCandidateView(model.ModelCandidate{}, "p", "")
	if !clear.Routable {
		t.Error("a candidate with no reason not to route is marked unroutable")
	}
	if clear.BlockedBy != "" {
		t.Errorf("a routable candidate reports %q", clear.BlockedBy)
	}
}

func boolPtr(v bool) *bool { return &v }
