// Package videotask owns the video task state machine: who may flip a
// task's status, when a poll is due, and what a provider change does to
// tasks still in flight. The submit path creates a row and returns; every
// later observation — the caller's poll, the reaper's sweep — goes through
// here, so the one-way rules have one home.
package videotask

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// Querier is how the service asks an upstream about one task. The real
// implementation speaks the provider's native task dialect (submit-then-
// poll); the interface exists so the state machine is testable and
// dialect-agnostic. A Querier receives the task's own snapshots — its
// provider, its destination version, its upstream task id — and answers
// in the normalized vocabulary; mapping vendor spellings (including a
// vendor's "unknown task" onto expiry) is the implementation's job.
type Querier interface {
	QueryTask(ctx context.Context, task model.VideoTask) (QueryResult, error)
}

// QueryResult is one upstream observation, normalized. Status is a
// model.VideoTask* spelling; the URL and usage fields are only read when
// it is completed.
type QueryResult struct {
	Status       string
	ResultURL    string
	CoverURL     string
	UsageSeconds int
	ErrorCode    string
	ErrorMessage string
}

// microsPerYuan is the micro-yuan per yuan fixed point this package
// prices in — the same 1:1,000,000 the gateway's cost ledger uses (its
// microsPerUnit); restated locally because the ledger's constant lives
// across a package boundary this service does not import.
const microsPerYuan = 1_000_000

// Poll pacing: one upstream query per task per interval, no matter how
// eagerly the caller polls. Two seconds is the SDK create_and_poll rhythm
// made safe — an answer newer than the interval is served from the row.
const DefaultPollInterval = 2 * time.Second

// ZombieHorizon is how long a non-terminal task may live before the
// reaper expires it without another query. Seven days covers the longest
// upstream task-record window in the supported vendor set; a task older
// than that cannot be asked about anywhere, terminal or not.
const ZombieHorizon = 7 * 24 * time.Hour

// ErrQuerierUnavailable is what a nil querier answers: the task domain is
// wired before any dialect can query upstreams, and a poll arriving in
// that window is told so rather than pretending the upstream was asked.
var ErrQuerierUnavailable = errors.New("no video task querier is wired")

// Service is the task state machine over the store. Stateless between
// calls except for the in-process poll single-flight, which exists so
// concurrent GETs of one hot task cost one upstream query, not N.
type Service struct {
	db       *gorm.DB
	querier  Querier
	interval time.Duration

	mu      sync.Mutex
	flights map[string]*sync.Mutex
}

// NewService builds the state machine. querier may be nil until a dialect
// is wired; polls then fail with ErrQuerierUnavailable, and everything
// that does not ask an upstream (create, read, expiry) still works.
func NewService(db *gorm.DB, querier Querier) *Service {
	return &Service{db: db, querier: querier, interval: DefaultPollInterval, flights: map[string]*sync.Mutex{}}
}

// NewTaskID mints a caller-facing job id: vid_ plus 128 random bits in
// hex. Unguessable by construction, so the ownership check can answer a
// foreign id with 404 without leaking even existence.
func NewTaskID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand refusing means the platform's entropy source is
		// broken; a predictable id would be worse than no service.
		panic("videotask: entropy source failed: " + err.Error())
	}
	return "vid_" + hex.EncodeToString(raw[:])
}

// BudgetExceededError says a submit would push its key past the budget
// limit, and by how much — the numbers the caller is told, computed from
// the same reads the gate made.
type BudgetExceededError struct {
	Limit    int64
	Spent    int64
	InFlight int64
	Ask      int64
}

func (e *BudgetExceededError) Error() string {
	return fmt.Sprintf("video budget exceeded: limit %d, spent %d, in-flight %d, this task %d", e.Limit, e.Spent, e.InFlight, e.Ask)
}

// priceMicros resolves what one accepted task costs at submit time: the
// candidate's own per-second tier price, read through the
// size-to-resolution map, multiplied by the seconds the caller asked for.
// Unpriced returns ok=false — a candidate with no table for this
// resolution bills nothing and reserves nothing, the same "unpriced is
// not free, it is unknown" reading the image settlement takes.
func (s *Service) priceMicros(ctx context.Context, task *model.VideoTask) (int64, bool, error) {
	cand, err := repository.FindModelCandidateByID(s.db.WithContext(ctx), task.CandidateID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// The candidate row vanished between routing and this create —
			// deleted mid-flight, or a fixture with no pricing surface.
			// Read as unpriced rather than failed: the task still exists,
			// it simply bills nothing, which is the lenient direction the
			// image settlement takes for a table it cannot resolve.
			return 0, false, nil
		}
		return 0, false, err
	}
	if model.NormalizeBillingMode(cand.BillingMode) != model.BillingModeVideo {
		return 0, false, nil
	}
	resolution, _, ok := videos.MapDashScopeSize(task.Size)
	if !ok {
		return 0, false, nil
	}
	price, ok := model.ParseVideoPricingTiers(cand.VideoPricingTiers).ResolveSellPrice(resolution)
	if !ok || price < 0 {
		return 0, false, nil
	}
	return int64(math.Round(price * microsPerYuan)), true, nil
}

// Create records one accepted task: priced at the submit-time tier (the
// snapshot every later settlement charges against), budget-checked
// against the key's limit with every unfinished task's reservation
// counted, pending, and on the clock. ID and horizon are filled here when
// empty so a caller cannot mint its own.
func (s *Service) Create(ctx context.Context, task *model.VideoTask, now time.Time) error {
	if task.ID == "" {
		task.ID = NewTaskID()
	}
	if task.Status == "" {
		task.Status = model.VideoTaskPending
	}
	if task.ExpiresAt == nil {
		horizon := now.Add(ZombieHorizon)
		task.ExpiresAt = &horizon
	}
	task.UpstreamSubmittedAt = now

	if unit, priced, err := s.priceMicros(ctx, task); err != nil {
		return err
	} else if priced {
		ask := unit * int64(task.Seconds)
		task.EstimatedMicros = ask
		if err := s.checkBudget(ctx, task, ask); err != nil {
			return err
		}
	}
	return repository.CreateVideoTask(s.db.WithContext(ctx), task)
}

// checkBudget is the submit-time upper bound: what the key has spent,
// plus what its unfinished tasks have reserved, plus what this task will
// cost, against the limit. Exact because a video's seconds and tier price
// are both known at submit — the one billing shape that can promise its
// bound. Unpriced tasks skip the gate: there is no number to hold.
func (s *Service) checkBudget(ctx context.Context, task *model.VideoTask, ask int64) error {
	key, err := repository.FindAPIKeyByID(s.db.WithContext(ctx), task.APIKeyID)
	if err != nil {
		return err
	}
	if key.BudgetLimitMicros == nil || *key.BudgetLimitMicros <= 0 {
		return nil
	}
	inFlight, err := repository.SumInFlightVideoEstimated(s.db.WithContext(ctx), task.APIKeyID)
	if err != nil {
		return err
	}
	// The boundary matches the kernel's own admission gate: reaching the
	// limit exactly is allowed and the NEXT ask is refused — its
	// spent >= limit check and this spent+inFlight+ask > limit check are
	// the same rule stated at the two places a budget can be touched.
	if key.BudgetSpentMicros+inFlight+ask > *key.BudgetLimitMicros {
		return &BudgetExceededError{Limit: *key.BudgetLimitMicros, Spent: key.BudgetSpentMicros, InFlight: inFlight, Ask: ask}
	}
	return nil
}

// Get returns one task for its owner, refreshing it from upstream if the
// poll interval has elapsed. A foreign or unknown id is the same miss —
// ErrNotFound — so the caller can answer 404 without confirming which.
func (s *Service) Get(ctx context.Context, apiKeyID uint, id string, now time.Time) (*model.VideoTask, error) {
	task, err := repository.FindVideoTaskForOwner(s.db.WithContext(ctx), apiKeyID, id)
	if err != nil {
		return nil, err
	}
	if model.VideoTaskTerminal(task.Status) {
		// The settlement backstop: a task whose completion was recorded
		// but whose charge never landed — a crash between the two writes,
		// or a settle that errored — is settled here, on read, from the
		// same snapshot. The billed compare-and-set keeps it once-only.
		if task.Status == model.VideoTaskCompleted && !task.Billed {
			s.settle(ctx, task, now)
		}
		return task, nil
	}
	if task.ExpiresAt != nil && now.After(*task.ExpiresAt) {
		// The window closed between sweeps; expire on sight rather than
		// paying one more upstream query for a task nothing can answer.
		_, _ = repository.SaveVideoTaskPollResult(s.db.WithContext(ctx), task.ID, terminalUpdate(model.VideoTaskExpired, "task_expired", now), now)
		task.Status = model.VideoTaskExpired
		return task, nil
	}
	if !s.pollDue(task, now) {
		return task, nil
	}
	s.refresh(ctx, task, now)
	return task, nil
}

// pollDue reports whether the interval has elapsed since the last poll.
// A never-polled task is always due.
func (s *Service) pollDue(task *model.VideoTask, now time.Time) bool {
	return task.LastPolledAt == nil || now.Sub(*task.LastPolledAt) >= s.interval
}

// refresh performs one lazy upstream observation under the task's
// single-flight lock: concurrent GETs of the same task serialize here,
// and the stamp claim inside decides whether the winner still owes the
// upstream a query. A lost race, an unavailable querier, or an upstream
// error leaves the task as it was — a poll failure is not a task failure.
func (s *Service) refresh(ctx context.Context, task *model.VideoTask, now time.Time) {
	s.mu.Lock()
	flight, ok := s.flights[task.ID]
	if !ok {
		flight = &sync.Mutex{}
		s.flights[task.ID] = flight
	}
	s.mu.Unlock()

	flight.Lock()
	defer flight.Unlock()

	var prev time.Time
	if task.LastPolledAt != nil {
		prev = *task.LastPolledAt
	}
	claimed, err := repository.ClaimVideoTaskPoll(s.db.WithContext(ctx), task.APIKeyID, task.ID, prev, now)
	if err != nil || !claimed {
		return
	}
	if s.querier == nil {
		return
	}
	result, err := s.querier.QueryTask(ctx, *task)
	if err != nil {
		return
	}
	updates := map[string]any{"status": result.Status, "error_code": result.ErrorCode, "error_message": result.ErrorMessage, "updated_at": now}
	switch result.Status {
	case model.VideoTaskCompleted:
		updates["result_url"] = result.ResultURL
		updates["cover_url"] = result.CoverURL
		updates["usage_seconds"] = result.UsageSeconds
		updates["upstream_completed_at"] = now
	case model.VideoTaskPending, model.VideoTaskProcessing, model.VideoTaskFailed, model.VideoTaskCancelled, model.VideoTaskExpired:
		// No result fields beyond the status and its error text.
	default:
		// A querier answering outside the vocabulary is a dialect bug;
		// recording it verbatim would poison the state machine, so the
		// observation is dropped and the task keeps its last known state.
		return
	}
	// The one-way guard lives in the store's WHERE clause: if the task
	// became terminal while the query was in flight, this update matches
	// zero rows and the terminal state stands.
	applied, err := repository.SaveVideoTaskPollResult(s.db.WithContext(ctx), task.ID, updates, now)
	if err != nil {
		return
	}
	if applied {
		task.Status = result.Status
		task.ErrorCode = result.ErrorCode
		task.ErrorMessage = result.ErrorMessage
		if result.Status == model.VideoTaskCompleted {
			task.ResultURL = result.ResultURL
			task.CoverURL = result.CoverURL
			task.UsageSeconds = result.UsageSeconds
			task.UpstreamCompletedAt = &now
			s.settle(ctx, task, now)
		}
	}
}

// settle charges a freshly completed task exactly once, from the
// submit-time snapshot: the unit price is the estimated bound divided by
// the seconds asked (exact by construction — the bound was built as
// seconds × unit), and the bill is the seconds actually observed times
// that unit. A crash between the observation and this call loses nothing:
// the next poll of a completed task is a no-op read, and the billed
// compare-and-set in the store is what makes the once-only.
func (s *Service) settle(ctx context.Context, task *model.VideoTask, now time.Time) {
	micros := int64(0)
	if task.EstimatedMicros > 0 && task.Seconds > 0 {
		unit := task.EstimatedMicros / int64(task.Seconds)
		micros = int64(task.UsageSeconds) * unit
	}
	if _, err := repository.ChargeVideoTask(s.db.WithContext(ctx), task.ID, micros, now); err != nil {
		// A settlement failure is logged by nobody on purpose: the row is
		// still unbilled, and the next poll's completion re-runs this —
		// the CAS is the retry. A sweeper-side reconciliation pass can be
		// added if a deployment ever shows unbilled completed rows.
		return
	}
	task.Billed = true
	task.BilledMicros = micros
}

// terminalUpdate is the service-side spelling of a terminal transition's
// field set, mirroring the repository's terminalVideoTaskUpdate so the
// retire-a-task triples have one wording per layer rather than three per
// codebase.
func terminalUpdate(status, code string, now time.Time) map[string]any {
	return map[string]any{"status": status, "error_code": code,
		"error_message": "the upstream task window closed before completion", "updated_at": now}
}

// SweepExpired is the reaper's tick: every non-terminal task past its
// horizon moves to expired, and every completed-but-unsettled row gets
// its charge — the reconciliation half of the once-only settle, so a
// task nobody polls again still bills. Rows are never deleted here or
// anywhere — the row is the billing evidence.
func (s *Service) SweepExpired(ctx context.Context, now time.Time) (int64, error) {
	moved, err := repository.ExpireStaleVideoTasks(s.db.WithContext(ctx), now)
	if err != nil {
		return moved, err
	}
	unbilled, err := repository.ListUnbilledCompletedVideoTasks(s.db.WithContext(ctx))
	if err != nil {
		return moved, err
	}
	for i := range unbilled {
		s.settle(ctx, &unbilled[i], now)
	}
	return moved, nil
}

// ExpireProviderTasks is the provider-change hook: after a provider's
// destination version moves, every in-flight task issued by an older
// destination is expired — its upstream task id is unqueryable at the new
// destination, and pretending otherwise would strand it until the
// horizon anyway.
func (s *Service) ExpireProviderTasks(ctx context.Context, providerID uint, newDestinationVersion int, now time.Time) (int64, error) {
	return repository.ExpireProviderInFlightVideoTasks(s.db.WithContext(ctx), providerID, newDestinationVersion, now)
}

// StartReaper runs SweepExpired on a ticker until the context is
// cancelled. Started by serve assembly, not by NewService, so tests and
// short-lived commands decide for themselves whether a sweeper runs.
func (s *Service) StartReaper(ctx context.Context, every time.Duration) {
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if _, err := s.SweepExpired(ctx, now); err != nil {
					// Logged by nothing here on purpose: the next tick
					// retries, and a sweeper that spammed the log every
					// minute during a transient database hiccup would be
					// its own incident.
					continue
				}
			}
		}
	}()
}
