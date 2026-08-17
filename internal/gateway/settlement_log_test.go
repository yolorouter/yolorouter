package gateway

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// captureWarnings runs fn with the logger writing to a file at warn level and
// returns what came out. Warn level, so a line appearing at all is the
// assertion: anything the product logs below warn is filtered by the logger
// itself and cannot reach an operator's alerting.
func captureWarnings(t *testing.T, fn func()) string {
	t.Helper()
	logFile := filepath.Join(t.TempDir(), "warn.log")
	logger.Init(logger.Config{Level: "warn", Filename: logFile, Console: false})
	t.Cleanup(func() {
		_ = logger.Sync()
		logger.Init(logger.Config{Filename: os.DevNull})
	})

	fn()
	_ = logger.Sync()

	b, err := os.ReadFile(logFile)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read log: %v", err)
	}
	return string(b)
}

// TestAnOrdinaryEndingDoesNotWakeAnybody pins which settlements are worth a
// warning.
//
// The level has to follow the delivery's own account of itself. Keyed on
// whether an error object happened to be in hand instead, two of the most
// ordinary endings there are shout: a provider that reliably omits the stream
// terminator carries errStreamNoDoneTerminator on every SUCCESSFUL request, and
// a caller pressing stop carries one per cancellation. An operator who sees a
// warning per success stops reading warnings.
//
// The table is pulled taut from both ends on purpose. Quieting those two is
// worth nothing if it also quiets the ending they are hardest to tell apart
// from — a caller shown a 200 who received part of an answer, which every
// persisted column records as a success.
func TestAnOrdinaryEndingDoesNotWakeAnybody(t *testing.T) {
	cases := []struct {
		name     string
		delivery fact.Delivery
		wantWarn bool
	}{
		{
			// The caller holds a complete answer; the provider simply never
			// said so in the way this protocol expects.
			name: "a stream that ended without its terminator",
			delivery: fact.Truncated(200, 200, fact.FaultUpstream, "stream_no_done",
				errStreamNoDoneTerminator),
			wantWarn: false,
		},
		{
			name: "a caller who pressed stop",
			delivery: fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient,
				"client_disconnected", errors.New("context canceled")),
			wantWarn: false,
		},
		{
			// Same event as above, built by the path that has no error to hand.
			// It must land at the same level as its twin.
			name: "the same caller, reported without an error",
			delivery: fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient,
				"client_disconnected", nil),
			wantWarn: false,
		},
		{
			name:     "a plain success",
			delivery: fact.Succeeded(200),
			wantWarn: false,
		},
		{
			// The caller was refused, and received that refusal whole. One
			// warning here is one per malformed request somebody sent us.
			name: "a provider that refused the caller's request",
			delivery: fact.Rejected(400, fact.FaultUpstream,
				"upstream_client_error_400", nil),
			wantWarn: false,
		},
		{
			// Reads like a success everywhere else: committed 200, billed 200,
			// no status anybody can look at says otherwise. The caller was shown
			// a completion and handed part of one.
			name: "a stream that broke with the caller still reading",
			delivery: fact.Truncated(200, 200, fact.FaultUpstream,
				"stream_partial: connection reset by peer", errors.New("connection reset by peer")),
			wantWarn: true,
		},
		{
			// Same shape reached from the other end of the chain: bytes were
			// already on the wire when the last candidate ran out.
			name: "a request that ran out of candidates part-way through",
			delivery: fact.Truncated(200, 200, fact.FaultUpstream,
				"partial_then_exhausted", nil),
			wantWarn: true,
		},
		{
			// Identical in every field to the terminator case at the top of
			// this table except the reason. That is the whole point: nothing
			// else about these two endings differs, so the reason is the only
			// thing that can carry one of them to somebody and leave the other
			// alone.
			name: "a stream that ended without saying it had finished",
			delivery: fact.Truncated(200, 200, fact.FaultUpstream,
				"stream_ended_unannounced", errStreamEndedUnannounced),
			wantWarn: true,
		},
		{
			// Nothing was served and it is ours: worth a look.
			name: "a delivery the kernel could not read",
			delivery: fact.Undelivered(500, fact.VerdictSettled, fact.FaultGateway,
				invalidDeliveryReason, errors.New("impossible")),
			wantWarn: true,
		},
		{
			name: "a provider that failed the whole request",
			delivery: fact.Undelivered(502, fact.VerdictSettled, fact.FaultUpstream,
				"upstream_server_error", errors.New("boom")),
			wantWarn: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureWarnings(t, func() {
				(&Service{}).logSettlement(&Exchange{requestID: "req-under-test"}, tc.delivery)
			})
			warned := strings.Contains(out, "req-under-test")
			if warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log said: %s", warned, tc.wantWarn, out)
			}
			if tc.wantWarn && !strings.Contains(out, tc.delivery.FailReason) {
				t.Errorf("the warning does not name why: %s", out)
			}
		})
	}
}

// TestADeliveryHandedOnRecordsWhyItWasHandedOn pins the one record a failover
// delivery ever gets.
//
// A delivery that sends the request to the next candidate never reaches
// settlement, so no fail_reason column is ever written for it and no settlement
// log line names it. This observation is the only place it lands. Fault alone
// says who is to blame without saying what was decided or what went wrong, and
// the reason codes are assembled with some care at the point of failure — a
// body that would not decode, a rewrite that could not proceed, a response over
// the cap. Dropping them leaves an operator with "upstream" and nothing else.
func TestADeliveryHandedOnRecordsWhyItWasHandedOn(t *testing.T) {
	svc := &Service{}
	rc := &Exchange{requestID: "req-handed-on"}
	handedOn := fact.HandedOn(fact.FaultUpstream,
		"ir_decode: invalid character 'n'", errors.New("decode failed"))

	svc.checkAndNote(rc, &handedOn, newExchangeSink(rc))

	var noted *fact.DeliveryObserved
	for _, e := range rc.timeline.All() {
		if v, ok := e.Record.(fact.DeliveryObserved); ok {
			noted = &v
		}
	}
	if noted == nil {
		t.Fatal("the delivery was never observed; nothing records this one at all")
	}
	if noted.Verdict != "next_candidate" {
		t.Errorf("verdict = %q, want next_candidate: without it the record cannot say "+
			"whether the request went on or ended here", noted.Verdict)
	}
	if noted.FailReason != "ir_decode: invalid character 'n'" {
		t.Errorf("fail reason = %q, want the code assembled at the failure — "+
			"this record is the only place it is ever written", noted.FailReason)
	}
}

// TestSettleFilesAgainstTheAppendedAttempt pins the numbering choice the
// againstRecordedAttempt option makes. The zero value files under the number
// of the attempt that comes next — right when no attempt was appended in the
// same breath as the settlement. againstRecordedAttempt renumbers onto the
// record just appended; filed the other way at the end of a chain, the audit
// row points at an attempt that never runs, and nothing downstream can tell.
func TestSettleFilesAgainstTheAppendedAttempt(t *testing.T) {
	settle := func(opts settleOptions) int {
		rc := &Exchange{requestID: "req-numbering"}
		rc.attempts = append(rc.attempts, AttemptRecord{})
		captureWarnings(t, func() {
			(&Service{}).settle(rc, callerGone("client_disconnected"), time.Now(), opts)
		})
		seen := -1
		for _, e := range rc.timeline.All() {
			if _, ok := e.Record.(fact.DeliveryObserved); ok {
				seen = e.Attempt
			}
		}
		if seen == -1 {
			t.Fatal("the settlement produced no DeliveryObserved entry to number")
		}
		return seen
	}
	if got := settle(settleOptions{}); got != 1 {
		t.Fatalf("zero-value settle filed under attempt %d, want 1 (the attempt that comes next)", got)
	}
	if got := settle(settleOptions{againstRecordedAttempt: true}); got != 0 {
		t.Fatalf("againstRecordedAttempt settle filed under attempt %d, want 0 (the attempt just appended)", got)
	}
}
