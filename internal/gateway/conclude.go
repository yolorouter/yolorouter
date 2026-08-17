package gateway

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/fact"
)

// concludeExchange is the one end-of-exchange choreography: everything that
// must happen when Handle unwinds, in the one order that makes the books
// balance. Handle arms it as a single defer, so the ordering lives HERE as
// explicit steps instead of being encoded in the registration order of
// scattered defers — where moving one line would silently reorder the ending
// of every request.
//
// The steps, and why each must precede the next:
//
//  1. Safety-net settlement. If nothing settled the exchange (a sub-call
//     panicked, or an exit path missed its settle), settle it now as a
//     gateway-fault 500 — finalize is idempotent (logWritten guard), so a
//     normal exit that already settled makes this a no-op and every request
//     still writes exactly one request_logs row. When bytes already went out,
//     the record says Truncated rather than Undelivered: the caller saw a
//     status, so this ending cannot claim nothing was committed.
//
//  2. Release admissions. A release is a reconciliation against how the
//     request ENDED — a capability deciding whether a reservation became a
//     charge needs the settled outcome, which is why this step cannot run
//     before step 1: on exactly the requests that ended worst it would read a
//     status of zero and a blank reason. The outcome is finalize's own
//     record, not a second literal assembled here, so the releases and the
//     recorders reconcile one ending rather than two accounts that drift.
//
//  3. Record. The recorder pass must see a timeline nothing will append to,
//     and releases append their reversal facts — so recording runs after
//     them, last of the work that matters.
//
//  4. The test hook, because "done" must mean done: a hook firing with
//     releases or recording still pending hands tests a signal to tear down
//     their temp directories while this goroutine is still writing capture
//     files into them.
//
// Steps 2–4 are armed as defers before step 1 runs, so a panic in an earlier
// step still lets the later steps execute while it propagates — the ending of
// a request that ends badly is exactly the one that must still be released
// and recorded.
//
// held arrives by value from Handle's closure, read at unwind time, so
// tickets acquired by the later admission phase are included.
func (s *Service) concludeExchange(c *gin.Context, rc *Exchange, held []heldTicket, start time.Time) {
	defer func() {
		s.recordTerminal(rc)
		if testHookHandleDone != nil {
			testHookHandleDone(rc)
		}
	}()
	defer func() {
		s.releaseAdmissions(rc.requestCtx, rc, held, rc.outcome)
	}()
	if rc.logWritten.Load() {
		return
	}
	d := fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
		fact.FaultGateway, "panic_recovered", nil)
	if c != nil && c.Writer != nil && c.Writer.Written() {
		// Something already went out, so the caller saw a status and this
		// cannot claim nothing was committed.
		d = fact.Truncated(c.Writer.Status(), http.StatusInternalServerError,
			fact.FaultGateway, "panic_recovered", nil)
	}
	s.settle(rc, d, start, settleOptions{})
}
