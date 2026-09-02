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
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
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

// Create records one accepted task: pending, on the clock, with the
// zombie horizon already set. ID and horizon are filled here when empty
// so a caller cannot mint its own.
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
	return repository.CreateVideoTask(s.db.WithContext(ctx), task)
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
		}
	}
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
// horizon moves to expired. Rows are never deleted here or anywhere —
// the row is the billing evidence.
func (s *Service) SweepExpired(ctx context.Context, now time.Time) (int64, error) {
	return repository.ExpireStaleVideoTasks(s.db.WithContext(ctx), now)
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
