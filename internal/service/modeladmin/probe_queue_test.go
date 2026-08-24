package modeladmin_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/internal/service/providerclient/providerclienttest"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedUntestedCandidate stores a disabled+untested mapping in the exact state
// a bulk import leaves rows in — including the armed auto-enable promise the
// queue's pass-enables key on — and returns its id.
func seedUntestedCandidate(t *testing.T, svc *modeladmin.ModelService, db *gorm.DB, providerID uint, name string) uint {
	t.Helper()
	now := time.Now().UTC()
	view, err := svc.CreateModel(modeladmin.CreateModelInput{Name: name}, now)
	if err != nil {
		t.Fatalf("seed CreateModel failed: %v", err)
	}
	cand, err := svc.CreateModelCandidate(context.Background(), view.ID, modeladmin.CreateCandidateInput{
		ProviderID: providerID, InputPrice: 1, OutputPrice: 2,
	}, now)
	if err != nil {
		t.Fatalf("seed CreateModelCandidate failed: %v", err)
	}
	// Arm the import promise directly: the manual create path deliberately
	// does not arm rows, and these tests model bulk-imported ones. Arming
	// aligns armed_at with the row clock in the same write, the way every
	// real arming write does (both set explicitly — GORM would otherwise
	// auto-touch updated_at and break the alignment).
	armedAt := time.Now().UTC()
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", cand.ID).
		Updates(map[string]interface{}{"auto_enable_on_pass": true, "armed_at": armedAt, "updated_at": armedAt}).Error; err != nil {
		t.Fatalf("seed arm candidate failed: %v", err)
	}
	return cand.ID
}

func loadCandidate(t *testing.T, db *gorm.DB, id uint) model.ModelCandidate {
	t.Helper()
	var c model.ModelCandidate
	if err := db.First(&c, id).Error; err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	return c
}

func TestProbeQueuedCandidateFailurePersistsReasonAndStaysDisabled(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	client.Result = providerclient.TestResult{
		Outcome: providerclient.TestModelNotFound, DurationMs: 7,
		Detail: "upstream returned 404: model does not exist",
	}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	c := loadCandidate(t, db, id)
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("a failed probe must leave the candidate disabled, got %d", c.ManagementStatus)
	}
	if c.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected verification failed, got %d", c.VerificationStatus)
	}
	if c.LastTestError == nil || *c.LastTestError != "upstream returned 404: model does not exist" {
		t.Fatalf("expected the probe's diagnostic persisted as the failure reason, got %v", c.LastTestError)
	}
	if c.LastTestedAt == nil {
		t.Fatal("expected the probe to stamp last_tested_at")
	}
}

func TestProbeQueuedCandidateSuccessEnablesAndClearsFailureReason(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// First probe fails and records a reason; the retried probe passes and
	// must both enable the mapping and clear the stale reason.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound, Detail: "temporarily missing"}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("failing probe errored: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess, DurationMs: 3}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("passing probe errored: %v", err)
	}

	c := loadCandidate(t, db, id)
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("a passing probe must auto-enable the imported candidate, got %d", c.ManagementStatus)
	}
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected verification passed, got %d", c.VerificationStatus)
	}
	if c.LastTestError != nil {
		t.Fatalf("a passing probe must clear the stored failure reason, got %q", *c.LastTestError)
	}
}

// A probe the queue cannot even start (the provider has no usable key) must
// leave something observable behind: the queue is about to forget the id, and
// an untouched row reads as "waiting for its probe" — both progress pollers
// would then poll it forever. The reason lands in last_test_error with an
// attempt timestamp, which the progress views read as a settled "unconfirmed"
// row, while verification stays Untested so a retest or re-import can recover it.
func TestProbeQueuedCandidateRecordsAbandonmentWhenProbeCannotStart(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // provider row with no keys at all
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("an abandoned probe must not fabricate a verdict, got verification=%d", c.VerificationStatus)
	}
	if c.LastTestedAt == nil {
		t.Fatal("expected the abandoned attempt to be stamped so pollers stop waiting on the row")
	}
	if c.LastTestError == nil || *c.LastTestError == "" {
		t.Fatal("expected the abandonment reason to be persisted for the operator")
	}
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the row to stay disabled, got %d", c.ManagementStatus)
	}
}

// The production client reports a request killed mid-flight as an inconclusive
// RESULT (unreachable) with a nil error — not as an error. If the context died
// because the process is shutting down, that result is a cancellation artifact,
// not an upstream verdict, and committing it would stamp the row with a
// misleading attempt exactly where restart recovery expects untouched rows.
func TestProbeQueuedCandidateDoesNotCommitWhenContextCancelledMidProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	ctx, cancel := context.WithCancel(context.Background())
	// Shutdown arrives while the probe request is upstream; the client then
	// reports "unreachable" with no error, the way the real one does.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestUnreachable, Detail: "context canceled"}
	client.SideEffect = cancel

	if err := svc.ProbeQueuedCandidate(ctx, id, time.Now().UTC()); err == nil {
		t.Fatal("expected the cancelled probe to surface an error instead of a verdict")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt != nil || c.LastTestError != nil || c.LastTestResult != nil {
		t.Fatalf("a shutdown-cancelled probe must leave the row untouched, got tested_at=%v error=%v result=%v",
			c.LastTestedAt, c.LastTestError, c.LastTestResult)
	}
	if c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected verification untouched, got %d", c.VerificationStatus)
	}
}

// Shutdown is different from abandonment: a probe cancelled because the
// process is stopping must leave the row exactly as the import stored it —
// restart recovery (manual retest or re-import) depends on the row still
// reading untested-with-no-attempt. Covered separately by
// TestProbeQueueStopCancelsInFlightProbeAndReturns; this test pins the
// distinction at the ProbeQueuedCandidate level.
func TestProbeQueuedCandidateLeavesRowUntouchedOnShutdownCancellation(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.Err = context.Canceled
	if err := svc.ProbeQueuedCandidate(ctx, id, time.Now().UTC()); err == nil {
		t.Fatal("expected the cancelled probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt != nil || c.LastTestError != nil {
		t.Fatalf("a shutdown-cancelled probe must leave the row untouched, got tested_at=%v error=%v", c.LastTestedAt, c.LastTestError)
	}
}

// A probe's verdict is seconds of paid upstream round trips; losing it to one
// transient database hiccup would strand the row untested — the progress
// pollers would wait on it forever — or leave a passed mapping disabled, and
// the background queue has no operator watching to click retry. Persisting the
// outcome therefore retries a bounded number of times before giving up.
func TestProbeQueuedCandidateRetriesTransientPersistFailure(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// The first TWO UPDATEs issued after this point (the verdict commit and its
	// immediate retry) die with a transient error, so the recovery has to go
	// through a backoff wait; everything afterwards succeeds.
	failures := 0
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail-first-update", func(tx *gorm.DB) {
		if failures < 2 {
			failures++
			_ = tx.AddError(errors.New("transient database blip"))
		}
	}); err != nil {
		t.Fatalf("register fault-injection callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:fail-first-update"); err != nil {
			t.Fatalf("remove fault-injection callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("expected the retried persist to succeed, got %v", err)
	}

	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the verdict to survive a transient commit failure, got verification=%d", c.VerificationStatus)
	}
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the passing candidate enabled, got management_status=%d", c.ManagementStatus)
	}
}

// The abandonment note is itself a persistence write: if it dies on a
// transient database error the job is forgotten with the row still reading
// "waiting for its probe", which is the exact forever-pending state the note
// exists to prevent. It must retry like every other probe-outcome write.
func TestProbeQueuedCandidateRetriesAbandonmentWrite(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// The first attempt to persist the abandonment dies; the retry succeeds.
	failures := 0
	if err := db.Callback().Update().Before("gorm:update").Register("test:fail-first-update", func(tx *gorm.DB) {
		if failures < 1 {
			failures++
			_ = tx.AddError(errors.New("transient database blip"))
		}
	}); err != nil {
		t.Fatalf("register fault-injection callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Update().Remove("test:fail-first-update"); err != nil {
			t.Fatalf("remove fault-injection callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt == nil || c.LastTestError == nil {
		t.Fatalf("expected the abandonment note to survive a transient write failure, got tested_at=%v error=%v",
			c.LastTestedAt, c.LastTestError)
	}
}

// The worker's fresh retry after a discarded commit can itself fail to START
// — the same admin action that advanced the token can also remove the
// provider's last usable key. That failure must fall through to the
// abandonment stamp: keeping the FIRST attempt's "ran" would skip it, and the
// queue would drop an untested row with no attempt record — the exact
// forever-pending state the stamp exists to prevent.
func TestProbeQueuedCandidateStampsAbandonmentWhenRetryCannotStart(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// While the first probe is upstream, an explicit disable advances the
	// token (discarding the probe's commit) and the provider's keys disappear
	// (so the retry probe cannot start).
	fired := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if fired {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.SetModelCandidateManagementStatusAdvancingProbeToken(session, id,
			model.ModelCandidateStatusDisabled, "competitor-run", time.Now().UTC()); err != nil {
			t.Errorf("simulate concurrent explicit disable: %v", err)
		}
		if err := session.Where("provider_id = ?", prov.ID).Delete(&model.ProviderKey{}).Error; err != nil {
			t.Errorf("simulate keys removed: %v", err)
		}
	}

	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable retry to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("an abandoned retry must not fabricate a verdict, got verification=%d", c.VerificationStatus)
	}
	if c.LastTestedAt == nil {
		t.Fatal("expected the abandoned attempt stamped so pollers stop waiting on the row")
	}
	if c.LastTestError == nil || *c.LastTestError == "" {
		t.Fatal("expected the abandonment reason persisted for the operator")
	}
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the explicit disable to stand, got %d", c.ManagementStatus)
	}
}

// The abandonment stamp itself commits under the token read BEFORE the probe
// was attempted. An explicit disable landing in between advances the token,
// and a stamp that gives up on the resulting guard miss leaves the row
// waiting forever. The stamp must re-read the row and land under the fresh
// token instead.
func TestProbeQueuedCandidateRecordsAbandonmentDespiteConcurrentTokenAdvance(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// Right after the worker's initial row read, an explicit disable advances
	// the probe token. Fires once, on that first read.
	fired := false
	if err := db.Callback().Query().After("gorm:query").Register("test:competitor-after-first-read", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.SetModelCandidateManagementStatusAdvancingProbeToken(session, id,
			model.ModelCandidateStatusDisabled, "competitor-run", time.Now().UTC()); err != nil {
			t.Errorf("simulate concurrent explicit disable: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:competitor-after-first-read"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt == nil || c.LastTestError == nil {
		t.Fatalf("expected the abandonment stamp to land despite the concurrent token advance, got tested_at=%v error=%v",
			c.LastTestedAt, c.LastTestError)
	}
	if c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("an abandoned probe must not fabricate a verdict, got verification=%d", c.VerificationStatus)
	}
}

// When the write that discarded this run's commit was a REQUEUE, the fresh
// generation already has its own scheduled run (the rerun marker on this
// instance, or the enqueuing instance's own queue) — the worker must not ALSO
// probe inline, or the new generation runs twice, spending upstream quota for
// nothing.
func TestProbeQueuedCandidateLeavesRequeuedGenerationToItsOwnRun(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	fired := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if fired {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.ClearCandidatesProbeResidue(session, []uint{id}, "rq-requeue-run", time.Now().UTC()); err != nil {
			t.Errorf("simulate requeue: %v", err)
		}
	}

	before := client.CallCountFor("basic")
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}
	if got := client.CallCountFor("basic") - before; got != 1 {
		t.Fatalf("expected exactly one probe (the requeued generation runs separately), got %d", got)
	}
	if c := loadCandidate(t, db, id); c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected the row left to the requeued generation, got verification=%d", c.VerificationStatus)
	}
}

// When the write that discarded this run's commit was ANOTHER PROBE's verdict
// (recognizable by the moved attempt stamp), that verdict stands — probing
// again would spend upstream quota to second-guess a result that is seconds
// old, and scale with the number of workers that raced.
func TestProbeQueuedCandidateYieldsToACompetingVerdict(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// While this worker's probe is upstream, a competing worker lands a
	// decisive failure (advancing the token and stamping the attempt).
	fired := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if fired {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		var row model.ModelCandidate
		if err := session.First(&row, id).Error; err != nil {
			t.Errorf("competitor read failed: %v", err)
			return
		}
		failed := model.ModelVerificationStatusFailed
		notFound := 2
		if _, err := repository.CommitModelCandidateProbeResults(session, id, row.ProviderModelName,
			row.LastProbeRunID, "competitor-run", repository.CandidateProbeCommit{
				VerificationStatus: &failed, LastTestResult: &notFound, WriteLastTestError: true,
			}, time.Now().UTC()); err != nil {
			t.Errorf("competitor commit failed: %v", err)
		}
	}

	before := client.CallCountFor("basic")
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}
	if got := client.CallCountFor("basic") - before; got != 1 {
		t.Fatalf("expected the competing verdict to stand without a re-probe, got %d probes", got)
	}
	if c := loadCandidate(t, db, id); c.VerificationStatus != model.ModelVerificationStatusFailed {
		t.Fatalf("expected the competitor's verdict to stand, got verification=%d", c.VerificationStatus)
	}
}

// A retarget while the job is still QUEUED must also yield: the worker's
// first read already sees the renamed row, and the revoked promise is what
// says no one wants it probed — spending an upstream call there would break
// the disabled-retarget contract from the queue side.
func TestProbeQueuedCandidateYieldsWhenRetargetedWhileQueued(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// Before the worker gets to the job, the admin retargets the mapping
	// (saved as disabled — the shape that deliberately does not probe).
	if err := repository.UpdateModelCandidate(db, id, "renamed-target", 1, 2,
		nil, nil, 0, true, false, time.Now().UTC()); err != nil {
		t.Fatalf("retarget failed: %v", err)
	}

	before := client.CallCountFor("basic")
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}
	if got := client.CallCountFor("basic") - before; got != 0 {
		t.Fatalf("expected no probe against the retargeted row, got %d", got)
	}
	if c := loadCandidate(t, db, id); c.VerificationStatus != model.ModelVerificationStatusUntested || c.LastTestedAt != nil {
		t.Fatalf("expected the retargeted row left untouched, got verification=%d stamp=%v", c.VerificationStatus, c.LastTestedAt)
	}
}

// A job scheduled for target A must never probe target B: a retarget during
// the probe (saved as disabled — the shape that deliberately does NOT probe)
// discards A's commit via the name guard, and the inline retry must yield
// instead of spending an uninvited upstream call on B.
func TestProbeQueuedCandidateYieldsWhenRetargetedMidProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	fired := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if fired {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.UpdateModelCandidate(session, id, "renamed-target", 1, 2,
			nil, nil, 0, true, false, time.Now().UTC()); err != nil {
			t.Errorf("simulate retarget: %v", err)
		}
	}

	before := client.CallCountFor("basic")
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}
	if got := client.CallCountFor("basic") - before; got != 1 {
		t.Fatalf("expected no probe against the retargeted name, got %d probes", got)
	}
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusUntested {
		t.Fatalf("expected the retargeted row left untouched, got verification=%d", c.VerificationStatus)
	}
}

// The abandonment stamp is A's record and must not land on B: a retarget
// after the worker's read replaces the mapping, and stamping the renamed row
// would seed B with a diagnostic about A — and burn a token B's own probe may
// be racing to commit under.
func TestProbeQueuedCandidateAbandonmentYieldsToARetarget(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	fired := false
	if err := db.Callback().Query().After("gorm:query").Register("test:retarget-after-first-read", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.UpdateModelCandidate(session, id, "renamed-target", 1, 2,
			nil, nil, 0, true, false, time.Now().UTC()); err != nil {
			t.Errorf("simulate retarget: %v", err)
		}
	}); err != nil {
		t.Fatalf("register retarget callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:retarget-after-first-read"); err != nil {
			t.Fatalf("remove retarget callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt != nil {
		t.Fatal("expected no abandonment stamp on the retargeted row")
	}
}

// The requeued generation's OWN worker must still stamp its abandonment: the
// yield applies only to a token that appeared after this run's read (someone
// else's fresh generation), not to the very token this run started from —
// otherwise an unstartable requeued probe leaves the row unstamped and armed,
// polled forever with no diagnostic.
func TestProbeQueuedCandidateStampsAbandonmentForItsOwnRequeuedGeneration(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")
	// The row is a requeued generation: its token lives in the requeue
	// namespace, and THIS worker's job was scheduled for it.
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", id).
		Update("last_probe_run_id", "rq-own-generation").Error; err != nil {
		t.Fatalf("seed requeue token: %v", err)
	}

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt == nil {
		t.Fatal("expected the requeued generation's own abandonment stamped")
	}
	if c.LastTestError == nil || *c.LastTestError == "" {
		t.Fatal("expected the abandonment reason persisted")
	}
}

// An abandonment stamp must never land on a REQUEUED generation: the requeue's
// own scheduled run owns that row's outcome, and stamping it would both
// replace the requeue token and set last_tested_at — taking the row out of
// startup recovery exactly where the fresh generation might still need it.
func TestProbeQueuedCandidateAbandonmentYieldsToARequeuedGeneration(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// Right after the worker's initial row read, a re-import requeues the row.
	fired := false
	if err := db.Callback().Query().After("gorm:query").Register("test:requeue-after-first-read", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		if tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.ClearCandidatesProbeResidue(session, []uint{id}, "rq-fresh-generation", time.Now().UTC()); err != nil {
			t.Errorf("simulate requeue: %v", err)
		}
	}); err != nil {
		t.Fatalf("register requeue callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:requeue-after-first-read"); err != nil {
			t.Fatalf("remove requeue callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.LastTestedAt != nil {
		t.Fatal("expected no abandonment stamp over the requeued generation")
	}
	if c.LastProbeRunID != "rq-fresh-generation" {
		t.Fatalf("expected the requeue token preserved, got %q", c.LastProbeRunID)
	}
}

// A re-import requeue landing while the worker is already probing renews the
// auto-enable promise and touches the row — which forfeits the in-flight
// pass's enable leg. The rerun that requeue scheduled then finds the row
// already Passed: it must FULFILL the still-armed promise (enable without
// another probe) instead of skipping and stranding the row Passed+Disabled.
func TestProbeQueuedCandidateFulfillsRenewedPromiseAfterMidProbeRequeue(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// While the first probe is upstream, a re-import requeues the mapping.
	fired := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if fired {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.ClearCandidatesProbeResidue(session, []uint{id}, "rq-requeue-run", time.Now().UTC()); err != nil {
			t.Errorf("simulate requeue residue clear: %v", err)
		}
	}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("first ProbeQueuedCandidate failed: %v", err)
	}

	// The rerun the requeue scheduled.
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("rerun ProbeQueuedCandidate failed: %v", err)
	}

	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the pass recorded, got verification=%d", c.VerificationStatus)
	}
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the renewed promise fulfilled by the rerun, got management=%d", c.ManagementStatus)
	}
	if c.AutoEnableOnPass {
		t.Fatal("expected the fulfilled promise consumed")
	}
}

// The in-memory queue dies with the process; startup recovery re-derives it
// from the durable rows — every untested mapping with no attempt stamp goes
// back in, while rows that already hold any outcome (a verdict, an
// abandonment stamp) are settled and stay out.
func TestProbeQueueRecoverPendingEnqueuesUnprobedRowsOnly(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	waiting := seedUntestedCandidate(t, svc, db, prov.ID, "model-waiting")
	stamped := seedUntestedCandidate(t, svc, db, prov.ID, "model-stamped")
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", stamped).
		Update("last_tested_at", time.Now().UTC()).Error; err != nil {
		t.Fatalf("stamp candidate: %v", err)
	}

	// A manually created candidate saved as disabled: untested and unstamped
	// by deliberate choice — storing without touching the upstream is that
	// path's documented meaning, and a restart must not spend probes on it.
	// It is recognizable by the missing auto-enable promise: only import and
	// requeue (the flows that owe a probe) arm rows.
	manualView, err := svc.CreateModel(modeladmin.CreateModelInput{Name: "model-manual"}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed manual model: %v", err)
	}
	manual, err := svc.CreateModelCandidate(context.Background(), manualView.ID, modeladmin.CreateCandidateInput{
		ProviderID: prov.ID, InputPrice: 1, OutputPrice: 2,
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed manual candidate: %v", err)
	}

	// A Passed+Disabled row still carrying the promise is owed its ENABLE:
	// the fulfill path delivers it without a probe, but only if recovery
	// re-enqueues the row after a restart — pollers watch this state.
	owedEnable := seedUntestedCandidate(t, svc, db, prov.ID, "model-owed-enable")
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", owedEnable).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"last_tested_at":      time.Now().UTC(),
		}).Error; err != nil {
		t.Fatalf("seed owed-enable candidate: %v", err)
	}

	queue := modeladmin.NewProbeQueue(svc, 1) // unstarted: recovery only fills the queue
	if err := queue.RecoverPending(); err != nil {
		t.Fatalf("RecoverPending failed: %v", err)
	}
	states := queue.CandidateQueueStates([]uint{waiting, stamped, manual.ID, owedEnable})
	if states[waiting] != "queued" {
		t.Fatalf("expected the unprobed mapping re-enqueued, got %q", states[waiting])
	}
	if states[stamped] != "" {
		t.Fatalf("expected the stamped mapping left out, got %q", states[stamped])
	}
	if states[manual.ID] != "" {
		t.Fatalf("expected the deliberately unprobed manual mapping left out, got %q", states[manual.ID])
	}
	if states[owedEnable] != "queued" {
		t.Fatalf("expected the Passed+Disabled+armed mapping re-enqueued for its owed enable, got %q", states[owedEnable])
	}
}

// When BOTH the first commit and the retry's commit lose their token CAS to
// back-to-back admin writes, the job must not be silently dropped: the row
// can be left untested with no attempt stamp, and only a re-scheduled run
// (the writes will have stopped by then) can settle it.
func TestProbeQueuedCandidateFlagsUnrecordedWhenBothCommitsAreDiscarded(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// EVERY probe attempt is raced by an explicit no-op disable that advances
	// the token before the attempt's commit lands.
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	races := 0
	client.SideEffect = func() {
		races++
		session := db.Session(&gorm.Session{NewDB: true})
		if err := repository.SetModelCandidateManagementStatusAdvancingProbeToken(session, id,
			model.ModelCandidateStatusDisabled, fmt.Sprintf("competitor-run-%d", races), time.Now().UTC()); err != nil {
			t.Errorf("simulate concurrent explicit disable: %v", err)
		}
	}

	err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC())
	if err == nil {
		t.Fatal("expected the doubly-discarded job to surface an error")
	}
	if !errors.Is(err, modeladmin.ErrProbeOutcomeUnrecorded) {
		t.Fatalf("expected ErrProbeOutcomeUnrecorded so the queue re-schedules, got %v", err)
	}
}

// A database outage that outlasts one job's retry window must not silently
// drop the job: the row stays "waiting for its probe" and nothing would ever
// revisit it. Such failures come back wrapped in ErrProbeOutcomeUnrecorded so
// the queue can re-schedule them.
func TestProbeQueuedCandidateFlagsUnrecordedOutcomeOnPersistentReadFailure(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// Every candidate read dies — the outage outlasts the retry schedule.
	if err := db.Callback().Query().Before("gorm:query").Register("test:db-down", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "model_candidates" {
			_ = tx.AddError(errors.New("database gone"))
		}
	}); err != nil {
		t.Fatalf("register outage callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:db-down"); err != nil {
			t.Fatalf("remove outage callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC())
	if err == nil {
		t.Fatal("expected the persistent read failure to surface")
	}
	if !errors.Is(err, modeladmin.ErrProbeOutcomeUnrecorded) {
		t.Fatalf("expected ErrProbeOutcomeUnrecorded so the queue re-schedules, got %v", err)
	}
}

// A probe that could not start but whose abandonment stamp STUCK left a
// record — the row reads settled-unconfirmed and must not be re-scheduled.
func TestProbeQueuedCandidateDoesNotFlagUnrecordedWhenAbandonmentStampSticks(t *testing.T) {
	svc, db, client := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC())
	if err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}
	if errors.Is(err, modeladmin.ErrProbeOutcomeUnrecorded) {
		t.Fatalf("a stamped abandonment is settled and must not be re-scheduled, got %v", err)
	}
	if c := loadCandidate(t, db, id); c.LastTestedAt == nil {
		t.Fatal("expected the abandonment stamped")
	}
}

// End to end: the queue re-schedules a job whose outcome was lost to a
// transient database outage, and the retry lands the verdict once the
// database is back.
func TestProbeQueueRequeuesJobWhoseOutcomeWasUnrecorded(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// Every candidate read fails until the worker has exhausted the first
	// job's whole retry schedule; the database then recovers. The reads are
	// counted without touching the database so the test's own polling cannot
	// consume the outage.
	var reads atomic.Int32
	var healed atomic.Bool
	if err := db.Callback().Query().Before("gorm:query").Register("test:db-outage", func(tx *gorm.DB) {
		if healed.Load() || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "model_candidates" {
			return
		}
		reads.Add(1)
		_ = tx.AddError(errors.New("database gone"))
	}); err != nil {
		t.Fatalf("register outage callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:db-outage"); err != nil {
			t.Fatalf("remove outage callback: %v", err)
		}
	}()

	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	queue := modeladmin.NewProbeQueue(svc, 1)
	queue.RetryDelay = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue.Start(ctx)
	defer queue.StopWithin(2 * time.Second)
	queue.Enqueue(id)

	// The first job retries its initial read four times before giving up.
	waitUntil := time.Now().Add(10 * time.Second)
	for reads.Load() < 4 && time.Now().Before(waitUntil) {
		time.Sleep(10 * time.Millisecond)
	}
	if reads.Load() < 4 {
		t.Fatal("expected the first job to exhaust its read retries")
	}
	healed.Store(true)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var c model.ModelCandidate
		if err := db.First(&c, id).Error; err == nil && c.VerificationStatus == model.ModelVerificationStatusPassed {
			if c.ManagementStatus != model.ModelCandidateStatusEnabled {
				t.Fatalf("expected the retried job to enable the armed row, got management=%d", c.ManagementStatus)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("expected the re-scheduled job to land the verdict after the outage")
}

// The abandonment stamp must not break the armed promise it leaves behind: it
// bumps the row clock, and without realignment the still-armed row is
// misaligned — the retest an operator runs after fixing the key would then
// pass without enabling, stranding the mapping Passed+Disabled.
func TestProbeQueuedCandidateAbandonmentKeepsArmedPromiseUsable(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// The provider's only key disappears; the queued probe cannot start and
	// stamps its abandonment.
	var key model.ProviderKey
	if err := db.Where("provider_id = ?", prov.ID).First(&key).Error; err != nil {
		t.Fatalf("read provider key: %v", err)
	}
	if err := db.Delete(&model.ProviderKey{}, key.ID).Error; err != nil {
		t.Fatalf("remove provider key: %v", err)
	}
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}
	if c := loadCandidate(t, db, id); c.LastTestedAt == nil {
		t.Fatal("expected the abandonment stamped")
	}

	// The operator restores the key and retests; the pass must still deliver
	// the import's enable.
	key.ID = 0
	if err := db.Create(&key).Error; err != nil {
		t.Fatalf("restore provider key: %v", err)
	}
	if _, _, err := svc.RetestModelCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("retest errored: %v", err)
	}
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the retest to pass, got verification=%d", c.VerificationStatus)
	}
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the armed promise fulfilled by the post-recovery retest, got management=%d", c.ManagementStatus)
	}
}

// The abandonment stamp reconciles against the row's CURRENT state, and a row
// that some other write already settled (a manual retest landing a verdict in
// the same window) must be left alone: the stamp would otherwise replace a
// real outcome with an abandonment note committed under the fresh token.
func TestProbeQueuedCandidateDoesNotOverwriteASettledRowWithAbandonment(t *testing.T) {
	svc, db, _ := newTestModelService(t)
	prov := seedEnabledProvider(t, db, "prov-keyless") // no keys: the probe cannot start
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// Right after the worker's initial row read, a manual retest lands a
	// decisive passing verdict with its own token and attempt stamp.
	fired := false
	if err := db.Callback().Query().After("gorm:query").Register("test:retest-after-first-read", func(tx *gorm.DB) {
		if fired || tx.Error != nil {
			return
		}
		fired = true
		session := db.Session(&gorm.Session{NewDB: true})
		var row model.ModelCandidate
		if err := session.First(&row, id).Error; err != nil {
			t.Errorf("competitor read failed: %v", err)
			return
		}
		passed := model.ModelVerificationStatusPassed
		success := int(providerclient.TestSuccess)
		if _, err := repository.CommitModelCandidateProbeResults(session, id, row.ProviderModelName, row.LastProbeRunID, "competitor-run",
			repository.CandidateProbeCommit{VerificationStatus: &passed, LastTestResult: &success, WriteLastTestError: true},
			time.Now().UTC()); err != nil {
			t.Errorf("competitor commit failed: %v", err)
		}
	}); err != nil {
		t.Fatalf("register competitor callback: %v", err)
	}
	defer func() {
		if err := db.Callback().Query().Remove("test:retest-after-first-read"); err != nil {
			t.Fatalf("remove competitor callback: %v", err)
		}
	}()

	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err == nil {
		t.Fatal("expected the unstartable probe to surface its error")
	}

	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the retest's verdict to stand, got verification=%d", c.VerificationStatus)
	}
	if c.LastTestResult == nil || *c.LastTestResult != int(providerclient.TestSuccess) {
		t.Fatalf("expected the retest's result untouched, got %v", c.LastTestResult)
	}
	if c.LastTestError != nil {
		t.Fatalf("expected no abandonment note over the settled row, got %q", *c.LastTestError)
	}
	if c.LastProbeRunID != "competitor-run" {
		t.Fatalf("expected the retest's token to remain, got %q", c.LastProbeRunID)
	}
}

// An admin's EXPLICIT status write during an in-flight probe must win even
// when it does not change the stored value: an imported row is already
// disabled, so "keep it disabled" writes the same number — a value-based CAS
// cannot see it, and the passing probe's enable would undo the admin's
// instruction moments after they gave it. The disable revokes the row's
// auto-enable promise and advances the probe token; the worker's one fresh
// retry then lands the VERDICT (progress completes) without the enable.
func TestProbeQueuedCandidateRespectsExplicitNoOpDisableDuringProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// While the queued probe is upstream, the admin explicitly disables the
	// (already disabled) mapping — a write that changes no stored value. Fires
	// once: the worker's retry probe must run against the post-disable row.
	disabled := false
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	client.SideEffect = func() {
		if disabled {
			return
		}
		disabled = true
		if err := svc.SetCandidateStatus(id, false, time.Now().UTC()); err != nil {
			t.Errorf("explicit disable during probe: %v", err)
		}
	}

	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	c := loadCandidate(t, db, id)
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the admin's explicit disable to stand, got management_status=%d", c.ManagementStatus)
	}
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the verdict to land anyway so progress can settle, got verification=%d", c.VerificationStatus)
	}
}

// A disable that lands while the probe is still QUEUED cancels the standing
// order: the worker's first read sees the revoked promise on an untested,
// unstamped row and yields — probing a mapping its admin just switched off
// would spend an upstream call nobody wants and stamp a row deliberately
// left off. The row reads as idle (terminal) everywhere, so progress views
// settle rather than wait.
func TestProbeQueuedCandidateRespectsDisableThatLandedWhileQueued(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// The admin disables the imported mapping BEFORE the worker gets to it.
	if err := svc.SetCandidateStatus(id, false, time.Now().UTC()); err != nil {
		t.Fatalf("explicit disable failed: %v", err)
	}

	before := client.CallCountFor("basic")
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	if got := client.CallCountFor("basic") - before; got != 0 {
		t.Fatalf("expected no probe against the cancelled order, got %d", got)
	}
	c := loadCandidate(t, db, id)
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the disable to stand, got management_status=%d", c.ManagementStatus)
	}
	if c.VerificationStatus != model.ModelVerificationStatusUntested || c.LastTestedAt != nil {
		t.Fatalf("expected the cancelled row left untouched, got verification=%d stamp=%v", c.VerificationStatus, c.LastTestedAt)
	}
}

// A candidate that earned its passing verdict while still waiting in the queue
// needs nothing from it — and probing anyway is not harmless: if an admin
// manually retested it to Passed and then deliberately disabled it, the
// worker's probe would pass and re-enable a mapping the admin just switched
// off. Already-Passed rows are skipped outright, whatever their toggle says.
func TestProbeQueuedCandidateSkipsAlreadyPassedRowEvenWhenDisabled(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// While the id sat queued: a manual retest passed it, then the admin
	// disabled it on purpose. An explicit disable always revokes the
	// auto-enable promise (clears the armed flag) — that revocation is what
	// tells the worker this Passed row is deliberately off, not owed an
	// enable.
	landed := time.Now().UTC()
	if err := db.Model(&model.ModelCandidate{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"verification_status": model.ModelVerificationStatusPassed,
			"management_status":   model.ModelCandidateStatusDisabled,
			"auto_enable_on_pass": false,
			"last_tested_at":      landed,
		}).Error; err != nil {
		t.Fatalf("seed passed+disabled state: %v", err)
	}

	before := client.CallCountFor("basic")
	client.Result = providerclient.TestResult{Outcome: providerclient.TestSuccess}
	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	if got := client.CallCountFor("basic") - before; got != 0 {
		t.Fatalf("expected no probe against an already-passed row, got %d", got)
	}
	c := loadCandidate(t, db, id)
	if c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("expected the admin's disable to stand, got management_status=%d", c.ManagementStatus)
	}
}

// A queued probe that was already upstream when a manual retest committed its
// verdict must discard its own: both paths write the same row, and the queue
// probe started against state the retest has since replaced. Letting it land
// would revert a fresh pass to a stale failure.
func TestProbeQueuedCandidateDiscardsVerdictWhenARetestLandsMidProbe(t *testing.T) {
	providerService, db, client := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), client)
	id := seedUntestedCandidate(t, svc, db, prov.ID, "some-model")

	// The queued probe will come back with a decisive failure...
	client.Result = providerclient.TestResult{Outcome: providerclient.TestModelNotFound, Detail: "stale verdict"}
	// ...but while it is upstream, a manual retest lands a pass.
	landed := time.Now().UTC()
	client.SideEffect = func() {
		if err := db.Model(&model.ModelCandidate{}).Where("id = ?", id).
			Updates(map[string]interface{}{
				"verification_status": model.ModelVerificationStatusPassed,
				"management_status":   model.ModelCandidateStatusEnabled,
				"last_tested_at":      landed,
				"last_probe_run_id":   "manual-retest-run",
			}).Error; err != nil {
			t.Errorf("simulate concurrent retest: %v", err)
		}
	}

	if err := svc.ProbeQueuedCandidate(context.Background(), id, time.Now().UTC()); err != nil {
		t.Fatalf("ProbeQueuedCandidate failed: %v", err)
	}

	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusPassed {
		t.Fatalf("expected the retest's fresher pass to stand, got verification=%d", c.VerificationStatus)
	}
	if c.LastTestError != nil {
		t.Fatalf("expected no stale failure reason, got %q", *c.LastTestError)
	}
	if c.ManagementStatus != model.ModelCandidateStatusEnabled {
		t.Fatalf("expected the candidate to stay enabled, got %d", c.ManagementStatus)
	}
}

// blockingClient is a ProviderClient whose basic probe blocks until released,
// tracking how many basic probes run concurrently — the observable the
// worker-cap and graceful-stop tests need. outcome overrides the released
// probe's result; the zero value is TestSuccess, keeping older tests as-is.
type blockingClient struct {
	mu         sync.Mutex
	cur        int
	max        int
	basicCalls int
	outcome    providerclient.TestOutcome
	release    chan struct{}
}

func (b *blockingClient) TestChatCompletion(ctx context.Context, _ protocols.ProtocolID, _, _, _ string) (providerclient.TestResult, error) {
	b.mu.Lock()
	b.cur++
	b.basicCalls++
	if b.cur > b.max {
		b.max = b.cur
	}
	outcome := b.outcome
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.cur--; b.mu.Unlock() }()
	select {
	case <-b.release:
		return providerclient.TestResult{Outcome: outcome}, nil
	case <-ctx.Done():
		return providerclient.TestResult{}, ctx.Err()
	}
}

func (b *blockingClient) basicCallCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.basicCalls
}

func (b *blockingClient) TestStreamingCompletion(context.Context, protocols.ProtocolID, string, string, string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (b *blockingClient) TestFunctionCalling(context.Context, protocols.ProtocolID, string, string, string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (b *blockingClient) ListModels(context.Context, protocols.ProtocolID, string, string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{}, nil
}

func (b *blockingClient) concurrent() (cur, max int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur, b.max
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestProbeQueueRunsFourWorkersAndDrainsEverything(t *testing.T) {
	providerService, db, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	seedSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), &providerclienttest.Fake{})

	blocking := &blockingClient{release: make(chan struct{})}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), blocking)

	ids := make([]uint, 8)
	for i := range ids {
		ids[i] = seedUntestedCandidate(t, seedSvc, db, prov.ID, "vendor/model-"+string(rune('a'+i)))
	}

	queue := modeladmin.NewProbeQueue(svc, 4)
	queue.Start(context.Background())
	defer queue.Stop()
	// The production shape: the queue idles first (workers asleep waiting for
	// work), THEN a bulk import lands. A wake-up that only rouses one worker
	// would drain this batch serially and never reach four concurrent probes.
	time.Sleep(100 * time.Millisecond)
	queue.Enqueue(ids...)

	// All four workers saturate while probes block, and never more than four.
	waitFor(t, "four concurrent probes", func() bool { cur, _ := blocking.concurrent(); return cur == 4 })
	if _, max := blocking.concurrent(); max > 4 {
		t.Fatalf("worker cap exceeded: %d concurrent probes", max)
	}

	close(blocking.release)
	waitFor(t, "all candidates enabled", func() bool {
		var enabled int64
		if err := db.Model(&model.ModelCandidate{}).
			Where("provider_id = ? AND management_status = ?", prov.ID, model.ModelCandidateStatusEnabled).
			Count(&enabled).Error; err != nil {
			t.Fatalf("count enabled: %v", err)
		}
		return enabled == int64(len(ids))
	})
	if queue.PendingCount() != 0 {
		t.Fatalf("expected an empty queue after draining, got %d pending", queue.PendingCount())
	}
}

// The dialog's progress view tells "waiting for a worker" apart from "a worker
// is on it right now", so the queue must report which side of the jobs channel
// each pending candidate is on — and stop reporting anything once the verdict
// is committed.
func TestProbeQueueReportsQueuedAndProbingStates(t *testing.T) {
	providerService, db, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	seedSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), &providerclienttest.Fake{})

	blocking := &blockingClient{release: make(chan struct{})}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), blocking)
	first := seedUntestedCandidate(t, seedSvc, db, prov.ID, "model-first")
	second := seedUntestedCandidate(t, seedSvc, db, prov.ID, "model-second")

	// One worker: the first id occupies it while the second waits in the FIFO.
	queue := modeladmin.NewProbeQueue(svc, 1)
	queue.Start(context.Background())
	defer queue.Stop()
	queue.Enqueue(first, second)
	waitFor(t, "the first probe to start", func() bool { cur, _ := blocking.concurrent(); return cur == 1 })

	states := queue.CandidateQueueStates([]uint{first, second, 999999})
	if states[first] != modeladmin.QueueStateProbing {
		t.Fatalf("expected the in-flight candidate to report probing, got %q", states[first])
	}
	if states[second] != modeladmin.QueueStateQueued {
		t.Fatalf("expected the waiting candidate to report queued, got %q", states[second])
	}
	if _, ok := states[999999]; ok {
		t.Fatal("expected an unknown id to be absent from the state map")
	}

	close(blocking.release)
	waitFor(t, "the queue to drain", func() bool { return queue.PendingCount() == 0 })
	drained := queue.CandidateQueueStates([]uint{first, second})
	if len(drained) != 0 {
		t.Fatalf("expected no queue state after draining, got %v", drained)
	}
}

// An id re-enqueued while its probe is IN FLIGHT must be probed again once the
// current run finishes, not dropped as a duplicate: a re-import can land after
// an inconclusive worker persisted its result but before the worker released
// the id, and dropping the request would leave the row untested with no queued
// work — both progress pollers would then wait on it forever. (An id that is
// merely queued still deduplicates: its coming probe covers the new request.)
func TestProbeQueueRerunsIdReEnqueuedWhileProbing(t *testing.T) {
	providerService, db, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	seedSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), &providerclienttest.Fake{})

	// Inconclusive outcome: the rerun genuinely has to hit the upstream again.
	blocking := &blockingClient{release: make(chan struct{}), outcome: providerclient.TestUpstreamError}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), blocking)
	id := seedUntestedCandidate(t, seedSvc, db, prov.ID, "some-model")

	queue := modeladmin.NewProbeQueue(svc, 1)
	queue.Start(context.Background())
	defer queue.Stop()
	queue.Enqueue(id)
	waitFor(t, "the probe to start", func() bool { cur, _ := blocking.concurrent(); return cur == 1 })

	// The re-import's requeue arrives while the worker is mid-probe.
	queue.Enqueue(id)

	close(blocking.release)
	waitFor(t, "the queue to drain", func() bool { return queue.PendingCount() == 0 })
	if got := blocking.basicCallCount(); got != 2 {
		t.Fatalf("expected the re-enqueued id to be probed again after the in-flight run, got %d basic probes", got)
	}
}

// stubbornClient blocks its basic probe until released and IGNORES context
// cancellation — standing in for a probe stuck in a blocking call that no
// cancel reaches. Only the bounded-stop test may use it.
type stubbornClient struct {
	started chan struct{}
	release chan struct{}
}

func (b *stubbornClient) TestChatCompletion(context.Context, protocols.ProtocolID, string, string, string) (providerclient.TestResult, error) {
	close(b.started)
	<-b.release
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (b *stubbornClient) TestStreamingCompletion(context.Context, protocols.ProtocolID, string, string, string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (b *stubbornClient) TestFunctionCalling(context.Context, protocols.ProtocolID, string, string, string) (providerclient.TestResult, error) {
	return providerclient.TestResult{Outcome: providerclient.TestSuccess}, nil
}

func (b *stubbornClient) ListModels(context.Context, protocols.ProtocolID, string, string) (providerclient.ListModelsResult, error) {
	return providerclient.ListModelsResult{}, nil
}

// Shutdown runs on a fixed budget the supervisor enforces with a kill: a probe
// stuck in a call that ignores cancellation must not be allowed to hold the
// whole process past it. StopWithin gives up at its deadline and reports so,
// instead of waiting forever the way Stop does.
func TestProbeQueueStopWithinGivesUpOnAStuckProbe(t *testing.T) {
	providerService, db, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	seedSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), &providerclienttest.Fake{})

	stuck := &stubbornClient{started: make(chan struct{}), release: make(chan struct{})}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), stuck)
	id := seedUntestedCandidate(t, seedSvc, db, prov.ID, "some-model")

	queue := modeladmin.NewProbeQueue(svc, 1)
	queue.Start(context.Background())
	queue.Enqueue(id)
	<-stuck.started

	start := time.Now()
	if queue.StopWithin(150 * time.Millisecond) {
		t.Fatal("expected StopWithin to report the stuck worker instead of claiming a clean stop")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("expected StopWithin to return around its deadline, took %v", elapsed)
	}
	// Unstick the worker so the test process does not leak it.
	close(stuck.release)
	if !queue.StopWithin(5 * time.Second) {
		t.Fatal("expected a clean stop once the probe was released")
	}
}

func TestProbeQueueStopCancelsInFlightProbeAndReturns(t *testing.T) {
	providerService, db, _ := newTestProviderService(t)
	prov := seedEnabledProviderForModelTest(t, providerService, "prov-a")
	seedSvc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), &providerclienttest.Fake{})

	blocking := &blockingClient{release: make(chan struct{})}
	svc := modeladmin.NewModelService(db, testutil.ProviderSecrets(), blocking)
	id := seedUntestedCandidate(t, seedSvc, db, prov.ID, "some-model")

	queue := modeladmin.NewProbeQueue(svc, 4)
	queue.Start(context.Background())
	queue.Enqueue(id)
	waitFor(t, "the probe to start", func() bool { cur, _ := blocking.concurrent(); return cur == 1 })

	done := make(chan struct{})
	go func() { queue.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return: workers were not released by context cancellation")
	}

	// The interrupted probe never obtained a verdict, so the candidate must be
	// left exactly as the import stored it.
	c := loadCandidate(t, db, id)
	if c.VerificationStatus != model.ModelVerificationStatusUntested || c.ManagementStatus != model.ModelCandidateStatusDisabled {
		t.Fatalf("a cancelled probe must leave the row untouched, got verification=%d status=%d", c.VerificationStatus, c.ManagementStatus)
	}
}
