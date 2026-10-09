package router

// Video-modality tests for the console Playground run endpoint: the one
// modality whose relay exchange is only the SUBMIT half. A run under the
// video tab submits a task through the real relay (the Ark task dialect —
// the one vendor branch with no model-name gate, so one test override of
// the base detector opens the whole submit-then-poll dialect to a local
// stub), then the run endpoint BRIDGES the task to terminal inside the
// page session: the column's terminal event carries the late settlement's
// figures (seconds × tier price, projected onto the submit's audit row by
// the task domain's own completing observation) and the console playback
// reference; a stopped wait stops the bridged polling only, the task
// itself keeps running and settles when completion is next observed.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// fakeArkVideoUpstream stands in for an upstream that speaks the Ark task
// dialect: submit accepts a task and answers its id; a task completes on
// the upstream's own clock (delay after submit, independent of polling —
// the fact the stop-wait assertions lean on); poll answers queued until
// the deadline and succeeded with a video_url afterwards; the clip bytes
// are served from the upstream's own result URL. It counts polls per task
// and records every submit body it received.
type fakeArkVideoUpstream struct {
	server *httptest.Server
	bodies *[]string

	mu        sync.Mutex
	tasks     map[string]arkFakeTask
	nextID    int
	polls     map[string]*atomic.Int32
	clipBytes []byte
	delay     time.Duration
}

type arkFakeTask struct {
	duration int
	doneAt   time.Time
}

func newFakeArkVideoUpstream(t *testing.T, delay time.Duration) *fakeArkVideoUpstream {
	t.Helper()
	fake := &fakeArkVideoUpstream{
		bodies: &[]string{}, tasks: map[string]arkFakeTask{}, polls: map[string]*atomic.Int32{},
		clipBytes: []byte("\x00\x00\x00\x18ftypmp42FAKE-CLIP-BYTES-FOR-E2E"),
		delay:     delay,
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeArkVideoUpstream) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/v3/contents/generations/tasks" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("cgt-mock-%d", f.nextID)
		var submit struct {
			Duration int `json:"duration"`
		}
		_ = json.Unmarshal(body, &submit)
		if submit.Duration == 0 {
			submit.Duration = 4
		}
		f.tasks[id] = arkFakeTask{duration: submit.Duration, doneAt: time.Now().Add(f.delay)}
		f.polls[id] = &atomic.Int32{}
		*f.bodies = append(*f.bodies, string(body))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
	case strings.HasPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/")
		f.mu.Lock()
		task, ok := f.tasks[id]
		counter := f.polls[id]
		f.mu.Unlock()
		if !ok || counter == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"NotFound","message":"no such task"}}`))
			return
		}
		counter.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if time.Now().Before(task.doneAt) {
			_, _ = w.Write([]byte(`{"id":"` + id + `","status":"queued"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"` + id + `","status":"succeeded","duration":` +
			fmt.Sprint(task.duration) + `,"content":{"video_url":"` + f.server.URL + `/__clip/` + id + `.mp4"}}`))
	case strings.HasPrefix(r.URL.Path, "/__clip/"):
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(f.clipBytes)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// completionDeadline exposes when a task's clip becomes ready on the
// upstream's own clock — the stop-wait test waits for it before its later
// observation, so the observation reads a completion that exists.
func (f *fakeArkVideoUpstream) completionDeadline(id string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tasks[id].doneAt
}

// pollCount sums the upstream polls observed so far.
func (f *fakeArkVideoUpstream) pollCount(id string) int32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if counter := f.polls[id]; counter != nil {
		return counter.Load()
	}
	return 0
}

// upstreamTaskID returns the first submitted task's upstream id — the
// single-column tests address their task through the gateway's own job id,
// so this is only the submit-order anchor for poll counting.
func (f *fakeArkVideoUpstream) upstreamTaskID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("cgt-mock-%d", f.nextID)
}

// submitCount reports how many submits the upstream has served so far, read
// under the same lock the serving path appends through — the disconnect
// test's signal that the run's submit exchange has genuinely reached the
// upstream (a plain len(*f.bodies) read from the test goroutine would race
// with the in-flight handler's append the same way an unsynchronized body
// read does).
func (f *fakeArkVideoUpstream) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(*f.bodies)
}

// syncRecorder is an httptest.ResponseRecorder behind one mutex, for tests
// that must read the PARTIAL body of a stream whose handler goroutine is
// still writing (acting on the channel while it is open is the whole point
// of the stop-wait test). A bare recorder's bytes.Buffer is unsynchronized:
// a concurrent String() while the handler writes is a data race the race
// detector flags — and a memory-model violation that can read a torn
// buffer when Buffer.grow swaps the slice header under the reader. Every
// write, flush and snapshot here goes through the same lock; once the
// handler goroutine has returned (the served channel closed, which
// happens-before the read) the underlying recorder is safe to touch
// directly again.
type syncRecorder struct {
	mu  sync.Mutex
	rec *httptest.ResponseRecorder
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{rec: httptest.NewRecorder()}
}

func (s *syncRecorder) Header() http.Header { return s.rec.Header() }

func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Write(b)
}

// WriteString satisfies io.StringWriter the way the recorder does — the
// aggregate stream's event writer prefers it.
func (s *syncRecorder) WriteString(str string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.WriteString(str)
}

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.WriteHeader(code)
}

// Flush is not optional: gin's writer Flush type-asserts that its
// underlying http.ResponseWriter implements http.Flusher, and every
// aggregate event is flushed as it is written.
func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

// snapshot returns the body written so far, under the same lock the
// writers hold — the safe way to inspect a stream mid-flight.
func (s *syncRecorder) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec.Body.String()
}

// replaceArkBaseForTest points the gateway's Ark base detector at the fake
// upstream's URL for the duration of one test — the same seam the
// gateway's in-package tests use, exported for exactly this cross-package
// real-chain need: a local httptest server never carries the vendor
// hostname the dialect routes on.
func replaceArkBaseForTest(t *testing.T, upstream *fakeArkVideoUpstream) {
	t.Helper()
	restore := gateway.ReplaceArkBaseDetectorForTest(func(baseURL string) bool {
		return baseURL == upstream.server.URL
	})
	t.Cleanup(restore)
}

// seedPlaygroundVideoModel seeds the provider / provider key / model /
// candidate quadruple for a VIDEO-billed upstream: the model declares
// video output, and the candidate prices per second through a tier table
// (720P at 0.5/s — a 4-second clip at the 1280x720 size settles
// 2,000,000 micros), so the figures a done event reports are real late
// settlement output rather than an unpriced accident.
func seedPlaygroundVideoModel(t *testing.T, db *gorm.DB, provider, modelName, providerModelName, baseURL string) *model.Model {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: provider, ProviderType: "openai", BaseURL: baseURL,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider %s: %v", provider, err)
	}
	secrets := crypto.NewSecretBox(testutil.ProviderMasterKey())
	enc, err := secrets.Encrypt("sk-upstream-" + provider)
	if err != nil {
		t.Fatalf("encrypt upstream key: %v", err)
	}
	pk := &model.ProviderKey{
		ProviderID: p.ID, Label: "k1", EncryptedKey: enc, KeyPrefix: "sk-upstream-" + provider,
		SortOrder: 1, TestModel: "m", ManagementStatus: model.ProviderKeyStatusEnabled,
		VerificationStatus:           model.VerificationStatusPassed,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(pk).Error; err != nil {
		t.Fatalf("seed provider key %s: %v", provider, err)
	}
	m := &model.Model{
		Name: modelName, ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: `["video"]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model %s: %v", modelName, err)
	}
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: providerModelName,
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		BillingMode:       model.BillingModeVideo,
		VideoPricingTiers: `{"tiers":[{"resolution":"720P","purchase_price":0.4,"sell_price":0.5}]}`,
		ManagementStatus:  model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate %s: %v", providerModelName, err)
	}
	return m
}

// videoRunBody builds a video-modality run request body with the exposed
// axes set.
func videoRunBody(keyID uint, modelName string, seconds int, size string) string {
	return fmt.Sprintf(`{"key_id":%d,"modality":"video","models":[%q],"request":{"prompt":"a calico cat on stage"},"params":{"seconds":%d,"size":%q}}`,
		keyID, modelName, seconds, size)
}

// TestPlaygroundVideoRunRealChain: one video column runs the real chain —
// submit through the relay, the bridge polls the task domain to terminal,
// and the column's done event carries the LATE settlement's figures: the
// audit row the completing observation just projected (4s × 0.5/s =
// 2,000,000 micros, usage_seconds 4) and the console playback reference.
// The upstream saw the submit in the Ark dialect's own encoding — the
// prompt, the mapped resolution/ratio, the duration — and nothing the
// page does not expose. The playback route serves the clip's bytes to the
// owning account only, and none of the channel's bytes leaked the key.
func TestPlaygroundVideoRunRealChain(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := newFakeArkVideoUpstream(t, 1200*time.Millisecond)
	replaceArkBaseForTest(t, upstream)
	seedPlaygroundVideoModel(t, db, "pg-vid-a", "vid-model-a", "vid-model-a-real", upstream.server.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	seedConsoleUser(t, db, "pg-other", model.RoleMember, "tok-pg-other")
	key, plaintext := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-video-key-0001", nil)

	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", videoRunBody(key.ID, "vid-model-a", 4, "1280x720"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}

	// The submit acknowledgement arrives first: the column's audit id is
	// on the stream while the task renders (the stop-waiting control and
	// the request-log cross-reference both need it pre-terminal).
	submitted := eventsOfKind(t, w, "submitted")
	if len(submitted) != 1 {
		t.Fatalf("expected exactly one submitted event, got %d: %s", len(submitted), w.Body.String())
	}
	var sub struct {
		Slot      int    `json:"slot"`
		RequestID string `json:"request_id"`
	}
	parseAs(t, submitted[0], &sub)
	if sub.Slot != 0 || sub.RequestID == "" {
		t.Fatalf("submitted event must name slot 0 and the audit id: %s", submitted[0])
	}
	if chunks := eventsOfKind(t, w, "chunk"); len(chunks) != 0 {
		t.Fatalf("a video column must not emit chunk events, got %d", len(chunks))
	}
	dones := eventsOfKind(t, w, "done")
	if len(dones) != 1 {
		t.Fatalf("expected one done for the video column, got %d: %s", len(dones), w.Body.String())
	}
	var done struct {
		Slot      int    `json:"slot"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
		Usage     struct {
			Seconds int `json:"seconds"`
		} `json:"usage"`
		CostMicros int64 `json:"cost_micros"`
		CostKnown  bool  `json:"cost_known"`
		Video      *struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
			Seconds     int    `json:"seconds"`
		} `json:"video"`
	}
	parseAs(t, dones[0], &done)
	if done.Slot != 0 || done.Status != http.StatusOK || done.RequestID != sub.RequestID {
		t.Fatalf("done event mismatch: %s", dones[0])
	}
	// The late settlement's figures: 4 delivered seconds at the seeded
	// 0.5/s tier price — the audit row's own arithmetic, read back after
	// the completing observation projected it.
	if done.Usage.Seconds != 4 {
		t.Fatalf("done usage must count 4 delivered seconds, got %+v: %s", done.Usage, dones[0])
	}
	if !done.CostKnown || done.CostMicros != 2000000 {
		t.Fatalf("done cost must be the late settlement 2000000 micros, got known=%v micros=%d", done.CostKnown, done.CostMicros)
	}
	if done.Video == nil || done.Video.URL != "/api/admin/playground/video/"+sub.RequestID || done.Video.ContentType != "video/mp4" || done.Video.Seconds != 4 {
		t.Fatalf("done must carry the console playback reference for its audit id, got %+v: %s", done.Video, dones[0])
	}

	// Parameter pass-through and the reverse assertion: the upstream saw
	// the submit in the Ark dialect's encoding — the routed model, the
	// prompt, the mapped resolution and ratio, the duration — and nothing
	// the page does not expose.
	if len(*upstream.bodies) != 1 {
		t.Fatalf("expected exactly one submit upstream, got %d", len(*upstream.bodies))
	}
	saw := (*upstream.bodies)[0]
	for _, want := range []string{
		`"model":"vid-model-a-real"`,     // routed to the candidate's provider name
		`"text":"a calico cat on stage"`, // the prompt inside the content array
		`"resolution":"720p"`,            // the size axis, mapped
		`"ratio":"16:9"`,                 // the size's ratio, mapped
		`"duration":4`,                   // the seconds axis, the dialect's own key
	} {
		if !strings.Contains(saw, want) {
			t.Errorf("upstream submit body missing %q; body: %s", want, saw)
		}
	}
	for _, banned := range []string{
		"seconds", "size", // the console's own axis spellings must not leak verbatim
		"temperature", "max_tokens", "system", "messages", // the TEXT family
		`"n":`, "format", "voice", "input", // the IMAGE/AUDIO families
		"stream",
	} {
		if strings.Contains(saw, banned) {
			t.Errorf("upstream submit body contains unexposed parameter %q; body: %s", banned, saw)
		}
	}

	// The audit row: playground source, and the settlement digest the
	// completing observation back-filled onto the submit's own row.
	var row model.RequestLog
	if err := db.Where("request_id = ?", sub.RequestID).First(&row).Error; err != nil {
		t.Fatalf("read the submit's audit row: %v", err)
	}
	if row.Source != "playground" || row.StatusCode != http.StatusOK {
		t.Fatalf("video audit row = source %q status %d, want playground 200", row.Source, row.StatusCode)
	}
	if row.CostMicros != 2000000 || !row.CostKnown || row.UsageSeconds != 4 {
		t.Fatalf("video audit row must carry the settlement digest (2000000 micros, known, 4s), got %d known=%v secs=%d", row.CostMicros, row.CostKnown, row.UsageSeconds)
	}
	after, err := repository.FindAPIKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if after.BudgetSpentMicros != 2000000 {
		t.Fatalf("budget_spent_micros = %d, want 2000000 (the task's settled cost)", after.BudgetSpentMicros)
	}

	// The playback route: the owning account streams the clip's bytes from
	// the upstream's result URL; another account's id answers the same
	// nothing.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/playground/video/"+sub.RequestID, nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	pw := httptest.NewRecorder()
	r.ServeHTTP(pw, req)
	if pw.Code != http.StatusOK || pw.Body.String() != string(upstream.clipBytes) {
		t.Fatalf("owner playback = %d %q, want the clip bytes", pw.Code, pw.Body.String())
	}
	if ct := pw.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Fatalf("playback content type = %q, want video/mp4", ct)
	}
	foreign := httptest.NewRequest(http.MethodGet, "/api/admin/playground/video/"+sub.RequestID, nil)
	foreign.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-other"})
	fw := httptest.NewRecorder()
	r.ServeHTTP(fw, foreign)
	if fw.Code != http.StatusNotFound {
		t.Fatalf("a foreign account's playback must 404, got %d", fw.Code)
	}

	// And none of the run's response bytes leaked the key plaintext.
	assertNoPlaintext(t, "video run", scanResponseBytes(w), plaintext)
}

// TestPlaygroundVideoStopWait: the video column's escape hatch. While the
// task renders, the operator stops waiting: the bridge stops polling (the
// upstream's poll counter freezes), the channel carries the honest
// wait_stopped acknowledgement — never a done, never a cancel_ack — and
// the TASK ITSELF keeps running: it completes on the upstream's clock,
// and the next observation (here: the gateway's own job resource route,
// the same read a later caller or the reconciliation pass makes) settles
// its full cost onto the submit's audit row.
func TestPlaygroundVideoStopWait(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := newFakeArkVideoUpstream(t, 4*time.Second)
	replaceArkBaseForTest(t, upstream)
	seedPlaygroundVideoModel(t, db, "pg-vid-sw", "vid-sw", "vid-sw-real", upstream.server.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	seedConsoleUser(t, db, "pg-other", model.RoleMember, "tok-pg-other")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-video-stop-001", nil)

	r := newRelayTestRouter(t, db)
	// The synced recorder: this test reads the stream WHILE the handler
	// goroutine may still be writing (the wait must be stopped in flight),
	// and a bare recorder's buffer is not safe to read concurrently.
	w := newSyncRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		req := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run",
			strings.NewReader(videoRunBody(key.ID, "vid-sw", 4, "1280x720")))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
		r.ServeHTTP(w, req)
	}()

	// Wait for the submit acknowledgement on the in-flight stream — its
	// request id is the stop-waiting control's target. Every partial read
	// goes through the recorder's lock-protected snapshot; the served-case
	// reads are safe because the channel close happens-after the handler's
	// last write.
	requestID := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if match := findSubmittedRequestID(w.snapshot()); match != "" {
			requestID = match
			break
		}
		select {
		case <-served:
			t.Fatalf("the run finished before a submitted event was observable: %s", w.rec.Body.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if requestID == "" {
		t.Fatal("no submitted event observed on the in-flight stream")
	}

	// Another account cannot stop this operator's wait: the switch is
	// owner-pinned like every other playground surface.
	foreign := httptest.NewRequest(http.MethodPost, "/api/admin/playground/video/stop-wait",
		strings.NewReader(`{"request_id":"`+requestID+`"}`))
	foreign.Header.Set("Content-Type", "application/json")
	foreign.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-other"})
	fw := httptest.NewRecorder()
	r.ServeHTTP(fw, foreign)
	if fw.Code != http.StatusNotFound {
		t.Fatalf("a foreign stop-wait must 404, got %d %s", fw.Code, fw.Body.String())
	}

	// The owner stops the wait.
	stop := httptest.NewRequest(http.MethodPost, "/api/admin/playground/video/stop-wait",
		strings.NewReader(`{"request_id":"`+requestID+`"}`))
	stop.Header.Set("Content-Type", "application/json")
	stop.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	sw := httptest.NewRecorder()
	r.ServeHTTP(sw, stop)
	if sw.Code != http.StatusOK {
		t.Fatalf("the owner's stop-wait must answer 200, got %d %s", sw.Code, sw.Body.String())
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the run channel did not close after the wait stopped")
	}

	// The honest acknowledgement — and ONLY that: no done for a clip that
	// was never observed terminal, no cancel_ack for a task that was not
	// cancelled. (The handler goroutine is gone — served closed — so the
	// underlying recorder is safe to read directly again.)
	stops := eventsOfKind(t, w.rec, "wait_stopped")
	if len(stops) != 1 {
		t.Fatalf("expected one wait_stopped event, got %d: %s", len(stops), w.rec.Body.String())
	}
	var stopped struct {
		Slot      int    `json:"slot"`
		RequestID string `json:"request_id"`
	}
	parseAs(t, stops[0], &stopped)
	if stopped.Slot != 0 || stopped.RequestID != requestID {
		t.Fatalf("wait_stopped must name slot 0 and the audit id: %s", stops[0])
	}
	if got := eventsOfKind(t, w.rec, "done"); len(got) != 0 {
		t.Fatalf("a stopped wait must not report done, got %d", len(got))
	}
	if got := eventsOfKind(t, w.rec, "cancel_ack"); len(got) != 0 {
		t.Fatalf("a stopped wait is not a cancelled task, got %d cancel_ack", len(got))
	}

	// The bridged polling stopped: the upstream's poll counter for this
	// task freezes. (The domain's own interval is 2s, so a window past it
	// proves the freeze rather than the pacing.)
	taskRow, err := repository.FindVideoTaskByRequestID(db, requestID)
	if err != nil {
		t.Fatalf("resolve the stopped task by its audit id: %v", err)
	}
	upstreamID := upstream.upstreamTaskID()
	frozen := upstream.pollCount(upstreamID)
	time.Sleep(2500 * time.Millisecond)
	if got := upstream.pollCount(upstreamID); got != frozen {
		t.Fatalf("the bridged polling must stop after stop-wait: polls went %d -> %d", frozen, got)
	}

	// The upstream's clip deadline passes with nobody polling (the freeze
	// window above already covers most of it).
	if dl := upstream.completionDeadline(upstreamID); time.Now().Before(dl) {
		time.Sleep(time.Until(dl) + 200*time.Millisecond)
	}

	// The task itself kept running: it completed on the upstream's clock,
	// and the settlement has NOT landed — nobody observed the completion
	// yet.
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		t.Fatalf("read the submit's audit row: %v", err)
	}
	if row.CostKnown || row.CostMicros != 0 {
		t.Fatalf("an unobserved completion must not have settled, got %d known=%v", row.CostMicros, row.CostKnown)
	}

	// The next observation settles it — the gateway's own job resource
	// route, the same lazy poll any later caller (or the reconciliation
	// pass) makes. The bill is the full one: the wait stopping changed
	// nothing about the cost.
	getJob := httptest.NewRequest(http.MethodGet, "/v1/videos/"+taskRow.ID, nil)
	getJob.Header.Set("X-Api-Key", "sk-yr-pg-video-stop-001")
	gw := httptest.NewRecorder()
	r.ServeHTTP(gw, getJob)
	if gw.Code != http.StatusOK || !strings.Contains(gw.Body.String(), `"status":"completed"`) {
		t.Fatalf("the later observation must answer the completed job, got %d %s", gw.Code, gw.Body.String())
	}
	var settled model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&settled).Error; err != nil {
		t.Fatalf("reload the audit row: %v", err)
	}
	if !settled.CostKnown || settled.CostMicros != 2000000 || settled.UsageSeconds != 4 {
		t.Fatalf("the later observation must settle the full cost (2000000 micros, 4s), got %d known=%v secs=%d", settled.CostMicros, settled.CostKnown, settled.UsageSeconds)
	}
	after, err := repository.FindAPIKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if after.BudgetSpentMicros != 2000000 {
		t.Fatalf("budget_spent_micros = %d, want 2000000 (the task settled at full cost)", after.BudgetSpentMicros)
	}
}

// TestPlaygroundVideoDisconnectLeavesTask: the video leg of the console
// disconnect — the aggregate channel's caller leaves while the bridge is
// polling the task. The bridge exits in SILENCE (no wait_stopped — that
// acknowledgement belongs to the stop-waiting control on a channel that is
// still open; no cancel_ack — unlike text/image/audio, a video column past
// its submit is a task nobody can terminate) and no done is invented for a
// task nobody observed terminal. The TASK ITSELF is untouched: the bridged
// polling freezes, the task completes on the upstream's own clock, and the
// next observation (the gateway's own job resource route — the same lazy
// poll any later caller or the reconciliation pass makes) settles its full
// cost onto the submit's audit row.
func TestPlaygroundVideoDisconnectLeavesTask(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := newFakeArkVideoUpstream(t, 4*time.Second)
	replaceArkBaseForTest(t, upstream)
	seedPlaygroundVideoModel(t, db, "pg-vid-dx", "vid-dx", "vid-dx-real", upstream.server.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-video-dc-001", nil)

	// The caller's own context is the run channel's context: cancelling it
	// is the page-session disconnect, the same ctx cancellation the run
	// handler derives every column's relay context from.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run",
		strings.NewReader(videoRunBody(key.ID, "vid-dx", 4, "1280x720")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	r := newRelayTestRouter(t, db)
	served := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(served)
	}()

	// The disconnect must land INSIDE the bridge, not on the submit
	// exchange: wait until the upstream saw the submit AND the bridge's
	// first poll of it reached the upstream (the task domain serves a young
	// row from the row itself, so the first upstream poll is a tick or two
	// out), so the cancelled wait exercises the caller-left exit rather
	// than the submit exchange's own shared cancel choreography.
	deadline := time.Now().Add(10 * time.Second)
	for upstream.submitCount() < 1 && time.Now().Before(deadline) {
		select {
		case <-served:
			t.Fatalf("the run ended before the submit reached the upstream: %s", w.Body.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	upstreamID := upstream.upstreamTaskID()
	for upstream.pollCount(upstreamID) < 1 && time.Now().Before(deadline) {
		select {
		case <-served:
			t.Fatalf("the run ended before the bridge began polling: %s", w.Body.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if upstream.pollCount(upstreamID) < 1 {
		t.Fatal("the bridge never polled the upstream before the deadline")
	}

	// The console caller leaves.
	cancel()
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the run channel did not close after the caller disconnected")
	}

	// The channel's whole content — the submit acknowledgement is there,
	// and NOTHING after the disconnect: the bridge exits silently, and no
	// terminal event is invented for a task nobody watched finish.
	requestID := findSubmittedRequestID(w.Body.String())
	if requestID == "" {
		t.Fatalf("the submitted event must be on the channel before the disconnect: %s", w.Body.String())
	}
	if got := eventsOfKind(t, w, "wait_stopped"); len(got) != 0 {
		t.Fatalf("a disconnect is not a stop-wait: %d wait_stopped events, want silence", len(got))
	}
	if got := eventsOfKind(t, w, "cancel_ack"); len(got) != 0 {
		t.Fatalf("disconnecting is not cancelling a video task, got %d cancel_ack", len(got))
	}
	if got := eventsOfKind(t, w, "done"); len(got) != 0 {
		t.Fatalf("no done may be invented for an unobserved task, got %d", len(got))
	}
	// And the disconnect wrote no aborted-exchange row either: a video
	// column past its submit never runs the shared settle choreography, so
	// the caller-gone fail_reason (text/image/audio's durable record) is
	// absent here — the task row and the late settlement are the record.
	var abortedRows []model.RequestLog
	if err := db.Where("api_key_id = ? AND fail_reason = ?", key.ID, "client_disconnected").Find(&abortedRows).Error; err != nil {
		t.Fatalf("query aborted rows: %v", err)
	}
	if len(abortedRows) != 0 {
		t.Fatalf("a disconnected video wait is not an aborted exchange, got %d client_disconnected rows", len(abortedRows))
	}

	// The bridged polling stopped: the upstream's poll counter freezes (a
	// window past the domain's 2s interval proves the freeze, not pacing).
	frozen := upstream.pollCount(upstreamID)
	time.Sleep(2500 * time.Millisecond)
	if got := upstream.pollCount(upstreamID); got != frozen {
		t.Fatalf("the bridged polling must stop after the disconnect: polls went %d -> %d", frozen, got)
	}

	// The task itself kept running: the clip's deadline passes with nobody
	// polling, and the next observation — the gateway's own job resource
	// route, the same lazy poll any later caller or the reconciliation pass
	// makes — settles the full cost. The disconnect changed nothing about
	// the task or the bill.
	if dl := upstream.completionDeadline(upstreamID); time.Now().Before(dl) {
		time.Sleep(time.Until(dl) + 200*time.Millisecond)
	}
	taskRow, err := repository.FindVideoTaskByRequestID(db, requestID)
	if err != nil {
		t.Fatalf("resolve the disconnected task by its audit id: %v", err)
	}
	getJob := httptest.NewRequest(http.MethodGet, "/v1/videos/"+taskRow.ID, nil)
	getJob.Header.Set("X-Api-Key", "sk-yr-pg-video-dc-001")
	gw := httptest.NewRecorder()
	r.ServeHTTP(gw, getJob)
	if gw.Code != http.StatusOK || !strings.Contains(gw.Body.String(), `"status":"completed"`) {
		t.Fatalf("the later observation must answer the completed job, got %d %s", gw.Code, gw.Body.String())
	}
	var settled model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&settled).Error; err != nil {
		t.Fatalf("read the submit's audit row: %v", err)
	}
	if !settled.CostKnown || settled.CostMicros != 2000000 || settled.UsageSeconds != 4 {
		t.Fatalf("the later observation must settle the full cost (2000000 micros, 4s), got %d known=%v secs=%d", settled.CostMicros, settled.CostKnown, settled.UsageSeconds)
	}
	after, err := repository.FindAPIKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if after.BudgetSpentMicros != 2000000 {
		t.Fatalf("budget_spent_micros = %d, want 2000000 (the task settled at full cost)", after.BudgetSpentMicros)
	}
}

// TestPlaygroundVideoRunValidatesBeforeStream: the video modality's body
// is validated before the stream starts — a run without a prompt, or with
// an axis outside the dialect's vocabulary, answers the console envelope
// and a real 400, and no column (and no upstream submit) ever ran.
func TestPlaygroundVideoRunValidatesBeforeStream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := newFakeArkVideoUpstream(t, time.Second)
	replaceArkBaseForTest(t, upstream)
	seedPlaygroundVideoModel(t, db, "pg-vid-v", "vid-v", "vid-v-real", upstream.server.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-video-val-0001", nil)

	r := newRelayTestRouter(t, db)
	cases := map[string]string{
		"blank prompt":                   fmt.Sprintf(`{"key_id":%d,"modality":"video","models":["vid-v"],"request":{"prompt":"   "}}`, key.ID),
		"size outside the vocabulary":    videoRunBody(key.ID, "vid-v", 4, "512x512"),
		"seconds outside the vocabulary": fmt.Sprintf(`{"key_id":%d,"modality":"video","models":["vid-v"],"request":{"prompt":"a cat"},"params":{"seconds":5,"size":"1280x720"}}`, key.ID),
	}
	for label, body := range cases {
		w := postPlaygroundRun(r, "tok-pg-member", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: must be refused with 400, got %d body %s", label, w.Code, w.Body.String())
		}
		if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("%s: the refusal must not open the stream", label)
		}
	}
	if len(*upstream.bodies) != 0 {
		t.Fatalf("a refused run must not reach the upstream, got %d submits", len(*upstream.bodies))
	}
}

// findSubmittedRequestID extracts the first submitted event's request id out
// of a partially-written aggregate stream — the stop-waiting test's way
// of learning the column's audit id while the channel is still open.
func findSubmittedRequestID(streamSoFar string) string {
	for _, block := range strings.Split(streamSoFar, "\n\n") {
		if !strings.HasPrefix(block, "event: submitted\n") {
			continue
		}
		var payload struct {
			RequestID string `json:"request_id"`
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(line[len("data: "):]), &payload); err == nil {
					return payload.RequestID
				}
			}
		}
	}
	return ""
}
