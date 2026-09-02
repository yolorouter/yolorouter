package videotask

// The state machine battery, against a stub querier: lifecycle, one-way
// transitions, ownership, poll pacing, single-flight, the reaper, and the
// provider-change hook. No dialect is involved — that is the point of the
// seam this package exists to own.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// stubQuerier scripts one answer per call and counts them.
type stubQuerier struct {
	calls    atomic.Int64
	mu       sync.Mutex
	answers  []QueryResult
	blockFor time.Duration
}

func (q *stubQuerier) QueryTask(_ context.Context, _ model.VideoTask) (QueryResult, error) {
	q.calls.Add(1)
	if q.blockFor > 0 {
		time.Sleep(q.blockFor)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.answers) == 0 {
		return QueryResult{}, errors.New("stub querier has no scripted answer")
	}
	ans := q.answers[0]
	q.answers = q.answers[1:]
	return ans, nil
}

func newTask(t *testing.T, apiKeyID, providerID uint) *model.VideoTask {
	t.Helper()
	return &model.VideoTask{
		APIKeyID: apiKeyID, ModelID: 1, ModelName: "sora-2",
		CandidateID: 1, ProviderID: providerID, ProviderModelName: "wan2.7-t2v",
		ProviderTaskID: "up-1", DestinationVersion: 1,
		Size: "720x1280", Seconds: 8,
	}
}

func TestStatusSpellingsMatchDialect(t *testing.T) {
	// The model layer and the dialect layer each spell the six states on
	// purpose (the model imports nothing); this pin is what keeps the two
	// vocabularies from drifting apart silently.
	pairs := map[string]string{
		model.VideoTaskPending:    videos.StatusPending,
		model.VideoTaskProcessing: videos.StatusProcessing,
		model.VideoTaskCompleted:  videos.StatusCompleted,
		model.VideoTaskFailed:     videos.StatusFailed,
		model.VideoTaskCancelled:  videos.StatusCancelled,
		model.VideoTaskExpired:    videos.StatusExpired,
	}
	for modelSpelling, dialectSpelling := range pairs {
		if modelSpelling != dialectSpelling {
			t.Fatalf("model %q != dialect %q: the state machine and the wire mapping disagree", modelSpelling, dialectSpelling)
		}
	}
}

func TestLifecyclePendingProcessingCompleted(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	q := &stubQuerier{answers: []QueryResult{
		{Status: model.VideoTaskProcessing},
		{Status: model.VideoTaskCompleted, ResultURL: "https://up.test/v.mp4", CoverURL: "https://up.test/c.jpg", UsageSeconds: 8},
	}}
	svc := NewService(db, q)
	now := time.Now()
	task := newTask(t, 1, 1)
	if err := svc.Create(context.Background(), task, now); err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.ID == "" || task.Status != model.VideoTaskPending {
		t.Fatalf("create must mint an id and set pending, got %q %q", task.ID, task.Status)
	}

	first, err := svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if first.Status != model.VideoTaskProcessing {
		t.Fatalf("first poll must observe processing, got %q", first.Status)
	}
	second, err := svc.Get(context.Background(), 1, task.ID, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if second.Status != model.VideoTaskCompleted || second.ResultURL == "" || second.UsageSeconds != 8 {
		t.Fatalf("completion must carry result and usage, got %+v", second)
	}
	if q.calls.Load() != 2 {
		t.Fatalf("two due polls must mean two upstream queries, got %d", q.calls.Load())
	}
}

func TestTerminalTasksAreNeverRequeried(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	q := &stubQuerier{answers: []QueryResult{
		{Status: model.VideoTaskFailed, ErrorCode: "upstream_rejected", ErrorMessage: "content policy"},
	}}
	svc := NewService(db, q)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	first, _ := svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))
	if first.Status != model.VideoTaskFailed || first.ErrorCode != "upstream_rejected" {
		t.Fatalf("failure must carry the upstream's error, got %+v", first)
	}
	for i := 0; i < 3; i++ {
		again, _ := svc.Get(context.Background(), 1, task.ID, now.Add(time.Duration(i+2)*time.Second))
		if again.Status != model.VideoTaskFailed {
			t.Fatalf("terminal state must hold, got %q", again.Status)
		}
	}
	if q.calls.Load() != 1 {
		t.Fatalf("a terminal task must not be queried again, got %d calls", q.calls.Load())
	}
}

func TestQuerierErrorIsNotATaskFailure(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	q := &stubQuerier{} // no scripted answers: every call errors
	svc := NewService(db, q)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	got, err := svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.VideoTaskPending {
		t.Fatalf("a failed poll must leave the task's last known state, got %q", got.Status)
	}
}

func TestOwnershipIsAFortyFour(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewService(db, nil)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	if _, err := svc.Get(context.Background(), 2, task.ID, now); !errors.Is(err, repository.ErrVideoTaskNotFound) {
		t.Fatalf("a foreign key must read as not-found, got %v", err)
	}
}

func TestPollIntervalThrottles(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	q := &stubQuerier{answers: []QueryResult{{Status: model.VideoTaskProcessing}}}
	svc := NewService(db, q)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	_, _ = svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))           // due: first
	_, _ = svc.Get(context.Background(), 1, task.ID, now.Add(1500*time.Millisecond)) // not due
	if q.calls.Load() != 1 {
		t.Fatalf("a poll inside the interval must not reach upstream, got %d calls", q.calls.Load())
	}
}

func TestConcurrentGetsSingleFlight(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// The query blocks long enough for every goroutine to pile onto the
	// flight lock; only the claim winner reaches it.
	q := &stubQuerier{answers: []QueryResult{{Status: model.VideoTaskProcessing}}, blockFor: 60 * time.Millisecond}
	svc := NewService(db, q)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))
		}()
	}
	wg.Wait()
	if q.calls.Load() != 1 {
		t.Fatalf("eight concurrent polls must cost one upstream query, got %d", q.calls.Load())
	}
}

func TestSweepExpiresOnlyStaleNonTerminal(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewService(db, nil)
	now := time.Now()

	stale := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), stale, now)
	// Age it past the horizon by rewriting expires_at directly: Create
	// always sets a fresh horizon, and the reaper's input is the stored
	// clock, not the service's memory of it.
	past := now.Add(-time.Hour)
	if err := db.Model(stale).Updates(map[string]any{"expires_at": past, "last_polled_at": nil}).Error; err != nil {
		t.Fatalf("age task: %v", err)
	}
	fresh := newTask(t, 1, 1)
	fresh.ProviderTaskID = "up-2"
	_ = svc.Create(context.Background(), fresh, now)
	terminal := newTask(t, 1, 1)
	terminal.ProviderTaskID = "up-3"
	terminal.Status = model.VideoTaskFailed
	_ = svc.Create(context.Background(), terminal, now)

	moved, err := svc.SweepExpired(context.Background(), now)
	if err != nil || moved != 1 {
		t.Fatalf("sweep must expire exactly the stale task, got moved=%d err=%v", moved, err)
	}
	got, _ := svc.Get(context.Background(), 1, stale.ID, now)
	if got.Status != model.VideoTaskExpired {
		t.Fatalf("stale task must be expired, got %q", got.Status)
	}
	got, _ = svc.Get(context.Background(), 1, fresh.ID, now)
	if got.Status != model.VideoTaskPending {
		t.Fatalf("fresh task must be untouched, got %q", got.Status)
	}
	var terminalCount int64
	db.Model(&model.VideoTask{}).Where("id = ?", terminal.ID).Count(&terminalCount)
	if terminalCount != 1 {
		t.Fatalf("terminal rows must never be deleted, count=%d", terminalCount)
	}
}

func TestProviderDestinationChangeExpiresOldTasks(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewService(db, nil)
	now := time.Now()

	oldDest := newTask(t, 1, 7)
	oldDest.DestinationVersion = 1
	_ = svc.Create(context.Background(), oldDest, now)
	newDest := newTask(t, 1, 7)
	newDest.ProviderTaskID = "up-2"
	newDest.DestinationVersion = 2
	_ = svc.Create(context.Background(), newDest, now)

	moved, err := svc.ExpireProviderTasks(context.Background(), 7, 2, now)
	if err != nil || moved != 1 {
		t.Fatalf("hook must expire exactly the old-destination task, got moved=%d err=%v", moved, err)
	}
	got, _ := svc.Get(context.Background(), 1, oldDest.ID, now)
	if got.Status != model.VideoTaskExpired || got.ErrorCode != "provider_destination_changed" {
		t.Fatalf("old-destination task must expire with the hook's code, got %+v", got)
	}
	got, _ = svc.Get(context.Background(), 1, newDest.ID, now)
	if got.Status != model.VideoTaskPending {
		t.Fatalf("current-destination task must be untouched, got %q", got.Status)
	}
}

func TestOnSightExpiryAfterHorizon(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewService(db, nil)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)
	past := now.Add(-time.Hour)
	if err := db.Model(task).Update("expires_at", past).Error; err != nil {
		t.Fatalf("age task: %v", err)
	}
	got, err := svc.Get(context.Background(), 1, task.ID, now)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != model.VideoTaskExpired {
		t.Fatalf("a task read past its horizon must expire on sight, got %q", got.Status)
	}
}

func TestNilQuerierPollsFailSoftly(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewService(db, nil)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	got, err := svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))
	if err != nil {
		t.Fatalf("get with no querier must not fail the read: %v", err)
	}
	if got.Status != model.VideoTaskPending {
		t.Fatalf("task must keep its state, got %q", got.Status)
	}
}

func TestVocabularyGuardDropsUnknownStatus(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	q := &stubQuerier{answers: []QueryResult{{Status: "succeed"}}} // a vendor spelling, unmapped
	svc := NewService(db, q)
	now := time.Now()
	task := newTask(t, 1, 1)
	_ = svc.Create(context.Background(), task, now)

	got, _ := svc.Get(context.Background(), 1, task.ID, now.Add(time.Second))
	if got.Status != model.VideoTaskPending {
		t.Fatalf("an out-of-vocabulary observation must be dropped, got %q", got.Status)
	}
}
