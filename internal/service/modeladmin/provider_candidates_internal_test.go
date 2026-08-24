package modeladmin

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// The torn-snapshot predicate must catch both shapes whose database snapshot
// may predate a verdict that landed during the request: the untested-unstamped
// unqueued pair, and a row that held a queue position when the request began
// but shows none now (its stale terminal verdict would stop the pollers).
func TestCandidateRowLooksTorn(t *testing.T) {
	stamp := time.Now().UTC()
	cases := []struct {
		name      string
		row       ProviderCandidateView
		wasQueued bool
		want      bool
	}{
		{"untested unstamped unqueued ARMED (a probe is owed)", ProviderCandidateView{
			VerificationStatus: model.ModelVerificationStatusUntested, AutoEnableOnPass: true,
		}, false, true},
		{"untested unstamped unqueued unarmed — a lasting legal state (manual save-as-disabled), not a tear", ProviderCandidateView{
			VerificationStatus: model.ModelVerificationStatusUntested,
		}, false, false},
		{"stale terminal verdict that was queued at request start", ProviderCandidateView{
			VerificationStatus: model.ModelVerificationStatusFailed, LastTestedAt: &stamp,
		}, true, true},
		{"settled row never queued", ProviderCandidateView{
			VerificationStatus: model.ModelVerificationStatusFailed, LastTestedAt: &stamp,
		}, false, false},
		{"row still visibly queued", ProviderCandidateView{
			VerificationStatus: model.ModelVerificationStatusUntested, QueueState: "queued",
		}, false, false},
	}
	for _, tc := range cases {
		if got := candidateRowLooksTorn(tc.row, tc.wasQueued); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
