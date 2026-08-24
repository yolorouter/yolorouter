package modeladmin

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DefaultProbeWorkers bounds how many mapping probes run at once. Each probe
// is a few real upstream round trips, so the cap is what keeps a full-catalog
// import from hammering a provider with hundreds of parallel test requests.
const DefaultProbeWorkers = 4

// ProbeQueue verifies imported candidates in the background: bulk import
// stores mappings disabled and untested, hands their ids here, and each probe
// that passes auto-enables its mapping while each that fails leaves it
// disabled with the reason persisted. The queue is in-process and deliberately
// not durable — the candidate rows are the state of record, and startup
// recovery (RecoverPending) re-derives the queue from them, so probes lost to
// a crash or shutdown are picked up automatically on the next boot (a manual
// retest or re-import still works too).
type ProbeQueue struct {
	svc     *ModelService
	workers int
	// RetryDelay spaces out re-scheduling of jobs whose outcome never reached
	// the database (ErrProbeOutcomeUnrecorded): long enough not to hammer a
	// database that is down, short enough that recovery is prompt. Set before
	// Start; NewProbeQueue installs the default.
	RetryDelay time.Duration

	mu      sync.Mutex
	pending map[uint]struct{} // queued or currently being probed
	active  map[uint]struct{} // currently being probed by a worker
	// rerun marks ids that were re-enqueued while their probe was IN FLIGHT.
	// The in-flight run read state that predates the re-request (a re-import
	// may have just cleared the row's attempt record), so dropping the request
	// as a duplicate would leave the row untested with no queued work — the
	// worker re-appends these to the FIFO when it finishes instead. Ids that
	// are merely queued still deduplicate: their coming probe covers the
	// request.
	rerun map[uint]struct{}
	fifo  []uint

	// signal wakes the dispatcher after an enqueue. Capacity 1 is enough
	// because the dispatcher is the only receiver and always drains the FIFO
	// before sleeping again; it fans work out to the workers through the
	// blocking jobs channel, so every idle worker gets fed. (An earlier
	// design had workers receive this signal directly — then one enqueue of
	// N ids woke exactly one worker and the batch drained serially.)
	signal chan struct{}
	jobs   chan uint

	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

func NewProbeQueue(svc *ModelService, workers int) *ProbeQueue {
	if workers <= 0 {
		workers = DefaultProbeWorkers
	}
	return &ProbeQueue{
		svc:        svc,
		workers:    workers,
		RetryDelay: 5 * time.Second,
		pending:    make(map[uint]struct{}),
		active:     make(map[uint]struct{}),
		rerun:      make(map[uint]struct{}),
		signal:     make(chan struct{}, 1),
		jobs:       make(chan uint),
	}
}

// How CandidateQueueStates describes a pending candidate: accepted and waiting
// for a worker, or on a worker's upstream round trips right now.
const (
	QueueStateQueued  = "queued"
	QueueStateProbing = "probing"
)

// CandidateQueueStates reports where each given candidate currently sits in
// this queue — QueueStateProbing for ids a worker is on, QueueStateQueued for
// ids still waiting — and leaves ids the queue holds nothing for out of the
// map entirely. It exists so the import progress view can tell "waiting its
// turn" apart from "being verified right now"; a candidate whose verdict has
// committed drops out, at which point the row's own columns carry the state.
func (q *ProbeQueue) CandidateQueueStates(ids []uint) map[uint]string {
	q.mu.Lock()
	defer q.mu.Unlock()
	states := make(map[uint]string)
	for _, id := range ids {
		if _, ok := q.active[id]; ok {
			states[id] = QueueStateProbing
			continue
		}
		if _, ok := q.pending[id]; ok {
			states[id] = QueueStateQueued
		}
	}
	return states
}

// RecoverPending re-enqueues every mapping still waiting for its first probe
// outcome. The queue is in-memory and non-durable, so probes queued before a
// crash — or committed by an import request that raced a shutdown's queue
// stop — would otherwise stay "waiting" until someone re-imported them by
// hand. Called once at startup: the untested-unstamped rows in the database
// are the durable form of the queue, and this derives the in-memory one from
// them. Duplicate probing across instances is settled by the commit's
// token CAS.
func (q *ProbeQueue) RecoverPending() error {
	ids, err := repository.ListUnprobedCandidateIDs(q.svc.db)
	if err != nil {
		return err
	}
	q.Enqueue(ids...)
	return nil
}

// RecoverPendingInBackground runs RecoverPending until it succeeds, retrying
// with backoff so a transient database error at boot does not orphan the
// durable pending rows until the NEXT restart. The loop lives inside the
// queue's own lifecycle (context and wait group): shutdown cancels the wait
// and Stop/StopWithin account for the goroutine, so a struggling database
// never leaves a sleeper querying a closed pool or enqueueing into a stopped
// queue. Must be called after Start.
func (q *ProbeQueue) RecoverPendingInBackground() {
	q.mu.Lock()
	started := q.started
	q.mu.Unlock()
	if !started {
		return
	}
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		for delay := 5 * time.Second; ; delay = min(delay*2, 5*time.Minute) {
			if q.ctx.Err() != nil {
				return
			}
			err := q.RecoverPending()
			if err == nil {
				return
			}
			logger.Warn("probe queue: recovering pending mappings failed, will retry",
				zap.Duration("retry_in", delay), zap.Error(err))
			select {
			case <-time.After(delay):
			case <-q.ctx.Done():
				return
			}
		}
	}()
}

// SnapshotStates returns the queue position of EVERY id currently held —
// unlike CandidateQueueStates it needs no id list, so a caller can take the
// snapshot BEFORE reading the database rows and later tell whether a row that
// now shows no queue position had one when the request began (a worker
// finished in between, and the row snapshot may predate its verdict).
func (q *ProbeQueue) SnapshotStates() map[uint]string {
	q.mu.Lock()
	defer q.mu.Unlock()
	states := make(map[uint]string, len(q.pending))
	for id := range q.pending {
		states[id] = QueueStateQueued
	}
	for id := range q.active {
		states[id] = QueueStateProbing
	}
	return states
}

// Enqueue queues candidate ids for probing. Ids already queued (or currently
// being probed) are dropped: one verdict per pending mapping, however many
// times an import re-submits it.
func (q *ProbeQueue) Enqueue(ids ...uint) {
	q.mu.Lock()
	for _, id := range ids {
		if _, dup := q.pending[id]; dup {
			// In-flight duplicates become a rerun (see the field comment);
			// queued ones are simply covered by their coming probe.
			if _, probing := q.active[id]; probing {
				q.rerun[id] = struct{}{}
			}
			continue
		}
		q.pending[id] = struct{}{}
		q.fifo = append(q.fifo, id)
	}
	q.mu.Unlock()
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// PendingCount reports how many candidates are queued or in flight.
func (q *ProbeQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// Start launches the dispatcher and workers. Idempotent: a second call is a
// no-op rather than an orphaning of the first generation's cancel func. The
// queue stops when Stop is called or when the given context is cancelled —
// both cancel in-flight upstream probes.
func (q *ProbeQueue) Start(ctx context.Context) {
	q.mu.Lock()
	if q.started {
		q.mu.Unlock()
		return
	}
	q.started = true
	q.ctx, q.cancel = context.WithCancel(ctx)
	q.mu.Unlock()
	q.wg.Add(1)
	go q.dispatch()
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker()
	}
}

// Stop cancels in-flight probes and waits for the dispatcher and every worker
// to exit. Safe to call on a queue that was never started. Terminal: a
// stopped queue is not restartable (Start stays a no-op afterwards) — the
// process is shutting down, there is nothing to restart for.
func (q *ProbeQueue) Stop() {
	q.mu.Lock()
	started := q.started
	q.mu.Unlock()
	if !started {
		return
	}
	q.cancel()
	q.wg.Wait()
}

// StopWithin is Stop on the shutdown budget: it cancels in-flight probes and
// waits up to timeout for the dispatcher and workers to exit, reporting false
// if any were still running at the deadline. Shutdown runs against a
// supervisor's kill grace, and a probe stuck in a call that ignores
// cancellation must not be allowed to hold the whole process past it — the
// leaked worker dies with the process moments later. Safe to call repeatedly
// and on a queue that was never started.
func (q *ProbeQueue) StopWithin(timeout time.Duration) bool {
	q.mu.Lock()
	started := q.started
	q.mu.Unlock()
	if !started {
		return true
	}
	q.cancel()
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// dispatch is the single consumer of the enqueue signal: it drains the FIFO
// into the blocking jobs channel, which hands each id to whichever worker is
// free — so a single bulk enqueue saturates every worker, not just one.
func (q *ProbeQueue) dispatch() {
	defer q.wg.Done()
	for {
		id, ok := q.pop()
		if ok {
			select {
			case q.jobs <- id:
			case <-q.ctx.Done():
				return
			}
			continue
		}
		select {
		case <-q.ctx.Done():
			return
		case <-q.signal:
		}
	}
}

func (q *ProbeQueue) worker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.ctx.Done():
			return
		case id := <-q.jobs:
			q.process(id)
		}
	}
}

func (q *ProbeQueue) pop() (uint, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.fifo) == 0 {
		return 0, false
	}
	id := q.fifo[0]
	q.fifo = q.fifo[1:]
	return id, true
}

func (q *ProbeQueue) process(id uint) {
	q.mu.Lock()
	q.active[id] = struct{}{}
	q.mu.Unlock()
	// The id stays in pending until the probe finishes, so a re-enqueue of an
	// in-flight mapping never runs two probes at once — but one re-enqueued
	// mid-probe is owed a fresh run and goes back on the FIFO instead of being
	// forgotten.
	defer func() {
		q.mu.Lock()
		delete(q.active, id)
		_, again := q.rerun[id]
		if again {
			delete(q.rerun, id)
			q.fifo = append(q.fifo, id) // stays in pending
		} else {
			delete(q.pending, id)
		}
		q.mu.Unlock()
		if again {
			select {
			case q.signal <- struct{}{}:
			default:
			}
		}
	}()
	if err := q.svc.ProbeQueuedCandidate(q.ctx, id, time.Now().UTC()); err != nil {
		if errors.Is(err, ErrProbeOutcomeUnrecorded) && q.ctx.Err() == nil {
			// The outcome never reached the database (an outage outlasting the
			// job's own retries) and nothing else will revisit the row — it
			// would read "waiting for its probe" forever. Re-schedule after a
			// delay; the loop is broken by shutdown, and by the job itself
			// once any outcome (verdict, abandonment stamp) lands.
			logger.Warn("probe queue: probe outcome unrecorded, re-scheduling",
				zap.Uint("candidate_id", id), zap.Error(err))
			q.scheduleRetry(id)
			return
		}
		// A probe that could not run (provider without a usable key, shutdown
		// cancellation) leaves the row untested; the reason is logged rather
		// than persisted because no upstream verdict was obtained.
		logger.Warn("probe queue: candidate probe did not complete",
			zap.Uint("candidate_id", id), zap.Error(err))
	}
}

// scheduleRetry re-enqueues a job after RetryDelay, giving a struggling
// database room to recover instead of hammering it back-to-back. Shutdown
// abandons the wait — those rows stay as the import stored them, which is
// what restart recovery keys on.
func (q *ProbeQueue) scheduleRetry(id uint) {
	go func() {
		select {
		case <-time.After(q.RetryDelay):
			q.Enqueue(id)
		case <-q.ctx.Done():
		}
	}()
}

// ErrProbeOutcomeUnrecorded marks a queue job whose outcome never reached the
// database — the row still reads "waiting for its probe" and nothing else
// will revisit it. It wraps persistence failures that exhausted their retries
// (the initial row read, the verdict commit, the abandonment stamp): the
// queue re-schedules such jobs after a delay instead of forgetting them, so a
// database outage longer than one job's retry window does not strand imports
// as forever-pending. Errors that LEFT A RECORD (an abandonment stamp that
// stuck) or that mean shutdown are not wrapped — those rows are settled or
// deliberately untouched.
var ErrProbeOutcomeUnrecorded = errors.New("probe outcome not recorded")

// ProbeQueuedCandidate probes one stored mapping the way the import flow
// promised: a pass auto-enables it, a probe that ran and failed persists the
// diagnostic and leaves it disabled, and a probe that could not run at all
// leaves the row untouched for a later manual retest.
func (s *ModelService) ProbeQueuedCandidate(ctx context.Context, candidateID uint, now time.Time) error {
	// The read retries alongside the writes (probePersistBackoff): a transient
	// failure here would silently drop the queued job with the row still
	// reading "waiting for its probe".
	c, err := retryProbePersist(ctx, func() (*model.ModelCandidate, error) {
		return repository.FindModelCandidateByID(s.db, candidateID)
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Deleted while queued — nothing to verify.
			return nil
		}
		if ctx.Err() == nil {
			err = errors.Join(ErrProbeOutcomeUnrecorded, err)
		}
		return err
	}
	if c.VerificationStatus == model.ModelVerificationStatusPassed {
		// Already verified — the queue's job here is done, whatever the enable
		// toggle says. A row that earned its pass while queued (an admin
		// retested it by hand) is either enabled and routable, or was passed
		// and then DELIBERATELY disabled; probing it anyway would pass again
		// and re-enable a mapping the admin just switched off.
		//
		// A row still ARMED is the exception: the promise outlived a pass
		// whose enable leg was forfeited to a concurrent row write (a requeue
		// renewing this very promise touches the row and does exactly that).
		// An explicit disable would have cleared the flag, so armed+Passed
		// means the enable is still owed — deliver it without another probe.
		// Both preconditions are re-checked inside the statement.
		if c.AutoEnableOnPass {
			applied, err := retryProbePersist(ctx, func() (bool, error) {
				return repository.FulfillArmedEnableIfVerified(s.db, c.ID, now)
			})
			if err != nil {
				if ctx.Err() == nil {
					err = errors.Join(ErrProbeOutcomeUnrecorded, err)
				}
				return err
			}
			if !applied {
				// The fulfill's own predicate missed: the row moved between the
				// read above and the write — for example an armed retest landed
				// a failure in that window. Dropping the job here would strand
				// whatever the row now needs, so re-read: a row still owed a
				// probe (untested, unstamped, armed) falls through to the probe
				// below; anything else is settled or owned elsewhere.
				fresh, ferr := retryProbePersist(ctx, func() (*model.ModelCandidate, error) {
					return repository.FindModelCandidateByID(s.db, candidateID)
				})
				if ferr != nil {
					if errors.Is(ferr, gorm.ErrRecordNotFound) {
						return nil
					}
					if ctx.Err() == nil {
						ferr = errors.Join(ErrProbeOutcomeUnrecorded, ferr)
					}
					return ferr
				}
				if fresh.VerificationStatus == model.ModelVerificationStatusUntested &&
					fresh.LastTestedAt == nil && fresh.AutoEnableOnPass {
					c = fresh
				} else {
					return nil
				}
			} else {
				return nil
			}
		} else {
			return nil
		}
	}
	// The armed flag is this job's standing order: every enqueue source
	// (import, requeue, startup recovery) arms the row, and every write that
	// means "do not probe this" — an explicit disable, a retarget saved as
	// disabled — revokes it. A row that arrives here unarmed, untested and
	// unstamped is a job whose order was cancelled while it sat queued;
	// probing it would spend an uninvited upstream call and stamp a row its
	// owner deliberately left unprobed.
	if !c.AutoEnableOnPass && c.VerificationStatus == model.ModelVerificationStatusUntested && c.LastTestedAt == nil {
		return nil
	}
	// Armed-mode enable: whether a pass may enable the row is decided by the
	// row's own auto_enable_on_pass flag inside the commit statement, so an
	// admin's explicit disable — which clears the flag — wins whether it lands
	// before this probe starts or while it is in flight, and the verdict still
	// lands either way.
	// Tracks the token and target this run's LATEST attempt was based on: the
	// abandonment path needs them to tell "the generation/mapping this very
	// job was scheduled for" (stamp it) apart from "a requeue or retarget
	// that appeared after the read" (yield to it).
	lastReadRunID := c.LastProbeRunID
	lastReadName := c.ProviderModelName
	_, applied, ran, err := s.probeAndCommitExistingCandidate(ctx, c.ID, c.ProviderID, c.ProviderModelName, c.ManagementStatus, c.LastProbeRunID, newProbeRunID(), false, true, c.UpdatedAt, now)
	if err == nil && ran && !applied && ctx.Err() == nil {
		// The commit was discarded: an explicit admin write (or another probe)
		// advanced the row's token mid-flight. Leaving the row as-is could
		// strand it untested with no queued work — the progress pollers would
		// wait forever — so ONE fresh attempt re-reads the row and probes
		// against its current state; a second discard means the row is being
		// actively operated on, and whoever owns it also owns its recovery.
		fresh, ferr := retryProbePersist(ctx, func() (*model.ModelCandidate, error) {
			return repository.FindModelCandidateByID(s.db, candidateID)
		})
		if ferr != nil {
			if ctx.Err() == nil {
				ferr = errors.Join(ErrProbeOutcomeUnrecorded, ferr)
			}
			return ferr
		}
		if isRequeueRunID(fresh.LastProbeRunID) && fresh.LastProbeRunID != c.LastProbeRunID {
			// The write that discarded this commit was a requeue: the fresh
			// generation already has its own scheduled run — the rerun marker
			// this instance's queue set when the id was re-enqueued mid-probe,
			// or the enqueuing instance's own queue — and probing inline here
			// would run that generation twice, spending upstream quota for
			// nothing. If the owning run is lost anyway (its process dies),
			// startup recovery re-derives the job from the still-armed row.
			// The inequality keeps the yield to tokens that appeared AFTER
			// this run's read: this run's own requeue generation is its own
			// to finish (a discard while holding an rq- token means the name
			// guard missed, handled above, or a later write moved the token).
			return nil
		}
		if !timePtrEqual(fresh.LastTestedAt, c.LastTestedAt) {
			// The write that discarded this commit was another probe's verdict
			// (probe commits always move the attempt stamp; admin status
			// writes never touch it). That verdict is seconds old and stands —
			// re-probing would spend upstream quota to second-guess it, scaling
			// with however many workers raced on the row.
			return nil
		}
		if fresh.ProviderModelName != c.ProviderModelName {
			// The row was RETARGETED mid-probe: this job was scheduled for the
			// old mapping, and the new one belongs to whoever stored it — a
			// retarget saved as disabled deliberately does not probe, and
			// spending an uninvited upstream call on it here would break that
			// contract.
			return nil
		}
		if fresh.VerificationStatus != model.ModelVerificationStatusPassed {
			// The retry's ran/err REPLACE the first attempt's: the same admin
			// action that advanced the token can also have removed the
			// provider's last usable key, and a retry that could not start must
			// fall through to the abandonment stamp below — keeping the first
			// attempt's ran=true would skip it and drop the row with no
			// attempt record.
			var retryApplied bool
			lastReadRunID = fresh.LastProbeRunID
			lastReadName = fresh.ProviderModelName
			_, retryApplied, ran, err = s.probeAndCommitExistingCandidate(ctx, fresh.ID, fresh.ProviderID, fresh.ProviderModelName, fresh.ManagementStatus, fresh.LastProbeRunID, newProbeRunID(), false, true, fresh.UpdatedAt, now)
			if err == nil && ran && !retryApplied && ctx.Err() == nil {
				// Discarded AGAIN: back-to-back admin writes advanced the token
				// under both attempts. Silently returning would drop the job
				// with the row possibly still unstamped and untested — the
				// forever-pending shape. Re-check the row: if some other write
				// settled it, done; otherwise hand the job back to the queue's
				// delayed retry, which runs after the burst of writes has
				// passed.
				settled, serr := retryProbePersist(ctx, func() (*model.ModelCandidate, error) {
					return repository.FindModelCandidateByID(s.db, candidateID)
				})
				if serr != nil {
					if errors.Is(serr, gorm.ErrRecordNotFound) {
						return nil
					}
					if ctx.Err() == nil {
						serr = errors.Join(ErrProbeOutcomeUnrecorded, serr)
					}
					return serr
				}
				if settled.VerificationStatus == model.ModelVerificationStatusUntested && settled.LastTestedAt == nil {
					return errors.Join(ErrProbeOutcomeUnrecorded,
						errors.New("probe verdict discarded twice by concurrent writes"))
				}
			}
		}
	}
	if err != nil && !ran && ctx.Err() == nil {
		// The probe could not start at all (no usable key on the provider, an
		// unresolvable provider row) and the queue is about to forget this id.
		// An untouched row would read as "still waiting for its probe" — the
		// progress views would poll it forever — so the reason is persisted as
		// an attempted-but-unconfirmed run: attempt stamped, reason stored,
		// verification left Untested so a manual retest or a re-import can
		// recover it. Shutdown cancellation is excluded on purpose: those rows
		// must stay exactly as the import stored them, which is what restart
		// recovery keys on.
		if cErr := s.recordProbeAbandonment(ctx, candidateID, lastReadRunID, lastReadName, err.Error(), now); cErr != nil {
			logger.Warn("probe queue: failed to record abandoned probe",
				zap.Uint("candidate_id", candidateID), zap.Error(cErr))
			if ctx.Err() == nil {
				// Nothing reached the database: the row still reads "waiting"
				// and the queue must re-schedule rather than forget it.
				return errors.Join(ErrProbeOutcomeUnrecorded, err, cErr)
			}
		}
		// The stamp stuck (or shutdown is in progress): the row is settled —
		// or deliberately untouched — and the original error is only worth a
		// log line, not a retry.
		return err
	}
	if err != nil && ran && ctx.Err() == nil {
		// A verdict was obtained but never persisted (the commit exhausted its
		// retries): the row still reads "waiting for its probe" and only a
		// re-scheduled run can settle it.
		return errors.Join(ErrProbeOutcomeUnrecorded, err)
	}
	return err
}

// abandonmentStampAttempts bounds recordProbeAbandonment's read-then-commit
// loop. Each extra pass is only needed when yet another writer advanced the
// token inside the read→commit window, so a couple of passes cover realistic
// contention while still terminating.
const abandonmentStampAttempts = 3

// recordProbeAbandonment stamps a queued probe that could not start, under the
// row's CURRENT probe token. The token the worker read before probing may
// already be stale — an explicit disable or an edit advances it — and a stamp
// committed under the stale token is silently discarded, leaving the row with
// no attempt record and the progress views polling it forever. So each attempt
// re-reads the row and commits under what it finds, stopping early when the
// row was deleted or some other write already settled it (a verdict or an
// attempt stamp means it no longer reads "waiting").
func (s *ModelService) recordProbeAbandonment(ctx context.Context, candidateID uint, expectedRunID, expectedName, reason string, now time.Time) error {
	detail := "probe could not start: " + reason
	// Armed mode so the stamp treats the promise the way every other
	// non-pass commit does: an ALIGNED promise re-aligns with the clock this
	// write bumps (the operator who fixes the key and retests still gets the
	// import's enable), a misaligned one is revoked. Without this the stamp
	// would leave the armed row misaligned, and the post-recovery retest
	// would pass without enabling.
	commit := repository.CandidateProbeCommit{
		LastTestError: &detail, WriteLastTestError: true,
		EnableOnPassWhenArmed: true,
	}
	for attempt := 0; attempt < abandonmentStampAttempts; attempt++ {
		row, err := retryProbePersist(ctx, func() (*model.ModelCandidate, error) {
			return repository.FindModelCandidateByID(s.db, candidateID)
		})
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				// Deleted while queued — nothing left to reconcile.
				return nil
			}
			return err
		}
		if row.VerificationStatus != model.ModelVerificationStatusUntested || row.LastTestedAt != nil {
			// Another write already settled the row; stamping over it would
			// replace a real outcome with an abandonment note.
			return nil
		}
		if row.ProviderModelName != expectedName {
			// The row was retargeted after this run's read: the stamp is the
			// OLD mapping's record and must not land on the new one — it would
			// seed the renamed row with a diagnostic about a target it never
			// was, and burn a token its own probe may be racing to commit
			// under.
			return nil
		}
		if isRequeueRunID(row.LastProbeRunID) && row.LastProbeRunID != expectedRunID {
			// A requeue that appeared AFTER this run's read owns the row now:
			// its own scheduled run decides the outcome, and stamping here
			// would replace the requeue token AND set the attempt stamp —
			// taking the row out of startup recovery exactly where the fresh
			// generation might still need it. The requeue token this very job
			// was scheduled FOR is not a yield: its own abandonment is this
			// job's to record.
			return nil
		}
		applied, err := s.commitProbeResultsDurably(ctx, candidateID, row.ProviderModelName, row.LastProbeRunID, newProbeRunID(), commit, now)
		if err != nil {
			return err
		}
		if applied {
			return nil
		}
		// Guard miss: yet another write advanced the token inside this
		// attempt's read→commit window — loop to re-read the fresh state.
	}
	return errors.New("candidate kept changing while stamping the abandoned probe")
}
