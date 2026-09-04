package gateway

// The video vertical, end to end against a scripted wan upstream: submit
// through the gateway's own Handle, poll through the job resource route,
// download through the content proxy. Everything the tickets before this
// one built — the door, the task domain, the dialect — has to hold hands
// here for any of these to pass.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"
)

// wanUpstream is a scriptable DashScope task upstream: submits return a
// fresh task id, and each task id has a queue of poll answers it serves
// in order.
type wanUpstream struct {
	mu sync.Mutex
	// submits is the queue of submit outcomes; nil entries mean a plain
	// acceptance. Exhausted or nil → plain acceptance.
	submits []string // JSON error body to return instead, "" = accept
	// tasks maps task id → queue of poll response bodies (dashscope
	// route); arkTasks the same for the ark route; klingTasks for the
	// kling query route; minimaxTasks for the minimax query route.
	tasks        map[string][]string
	arkTasks     map[string][]string
	klingTasks   map[string][]string
	minimaxTasks map[string][]string
	// minimaxSubmits queues submit refusals for the minimax route: a
	// non-empty head is served as an error body with minimaxSubmitStatus;
	// "" accepts. minimaxSubmitStatus is 0 (fall back to 402) between
	// scripted refusals.
	minimaxSubmits      []string
	minimaxSubmitStatus int
	// minimaxQueryErrors maps task id → error body served on the query
	// route with status 400 (the vendor's window-exhausted answer).
	minimaxQueryErrors map[string]string
	// media serves /media/* verbatim with the given content type.
	media map[string]string
	// recorded requests
	lastSubmitBody []byte
	lastSubmitPath string
	lastAsyncHdr   string
	lastAuthHeader string
	lastQueryPath  string
	submitHits     int
	queryHits      int
}

func newWanUpstream() *wanUpstream {
	return &wanUpstream{tasks: map[string][]string{}, arkTasks: map[string][]string{}, klingTasks: map[string][]string{}, minimaxTasks: map[string][]string{}, minimaxQueryErrors: map[string]string{}, media: map[string]string{}}
}

func (u *wanUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v1/services/aigc/video-generation/video-synthesis":
		u.submitHits++
		u.lastSubmitPath = r.URL.Path
		u.lastAsyncHdr = r.Header.Get("X-DashScope-Async")
		u.lastAuthHeader = r.Header.Get("Authorization")
		buf := &bytes.Buffer{}
		_, _ = buf.ReadFrom(r.Body)
		u.lastSubmitBody = buf.Bytes()
		if len(u.submits) > 0 {
			errBody := u.submits[0]
			u.submits = u.submits[1:]
			if errBody != "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(errBody))
				return
			}
		}
		id := fmt.Sprintf("up-task-%d", u.submitHits)
		_, _ = w.Write([]byte(`{"output":{"task_id":"` + id + `","task_status":"PENDING"},"request_id":"r-1"}`))
	case r.URL.Path == "/api/v3/contents/generations/tasks":
		u.submitHits++
		u.lastSubmitPath = r.URL.Path
		u.lastAsyncHdr = r.Header.Get("X-DashScope-Async")
		u.lastAuthHeader = r.Header.Get("Authorization")
		buf := &bytes.Buffer{}
		_, _ = buf.ReadFrom(r.Body)
		u.lastSubmitBody = buf.Bytes()
		id := fmt.Sprintf("ark-task-%d", u.submitHits)
		_, _ = w.Write([]byte(`{"id":"` + id + `"}`))
	case strings.HasPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/"):
		u.queryHits++
		u.lastQueryPath = r.URL.Path
		id := strings.TrimPrefix(r.URL.Path, "/api/v3/contents/generations/tasks/")
		queue := u.arkTasks[id]
		if len(queue) == 0 {
			_, _ = w.Write([]byte(`{"id":"` + id + `","status":"running"}`))
			return
		}
		body := queue[0]
		u.arkTasks[id] = queue[1:]
		_, _ = w.Write([]byte(body))
	case strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
		u.queryHits++
		u.lastQueryPath = r.URL.Path
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/")
		queue := u.tasks[id]
		if len(queue) == 0 {
			_, _ = w.Write([]byte(`{"output":{"task_status":"UNKNOWN"}}`))
			return
		}
		body := queue[0]
		u.tasks[id] = queue[1:]
		_, _ = w.Write([]byte(body))
	case strings.HasPrefix(r.URL.Path, "/text-to-video/") || strings.HasPrefix(r.URL.Path, "/image-to-video/"):
		u.submitHits++
		u.lastSubmitPath = r.URL.Path
		u.lastAsyncHdr = r.Header.Get("X-DashScope-Async")
		u.lastAuthHeader = r.Header.Get("Authorization")
		buf := &bytes.Buffer{}
		_, _ = buf.ReadFrom(r.Body)
		u.lastSubmitBody = buf.Bytes()
		id := fmt.Sprintf("kling-task-%d", u.submitHits)
		_, _ = w.Write([]byte(`{"code":0,"message":"SUCCEED","data":{"id":"` + id + `","status":"submitted"}}`))
	case r.URL.Path == "/tasks":
		u.queryHits++
		// The kling query carries its id as a query parameter; the full
		// request URI is what an assertion on that spelling needs.
		u.lastQueryPath = r.URL.RequestURI()
		u.lastAuthHeader = r.Header.Get("Authorization")
		id := r.URL.Query().Get("task_ids")
		queue := u.klingTasks[id]
		if len(queue) == 0 {
			_, _ = w.Write([]byte(`{"code":0,"data":[{"id":"` + id + `","status":"processing"}]}`))
			return
		}
		u.klingTasks[id] = queue[1:]
		_, _ = w.Write([]byte(queue[0]))
	case r.URL.Path == "/v2/video_generation":
		u.submitHits++
		u.lastSubmitPath = r.URL.Path
		u.lastAuthHeader = r.Header.Get("Authorization")
		buf := &bytes.Buffer{}
		_, _ = buf.ReadFrom(r.Body)
		u.lastSubmitBody = buf.Bytes()
		if len(u.minimaxSubmits) > 0 {
			errBody := u.minimaxSubmits[0]
			u.minimaxSubmits = u.minimaxSubmits[1:]
			if errBody != "" {
				// This vendor carries refusals as real statuses, so the
				// scripted refusal needs its own status, not the 400 the
				// dashscope queue hardcodes.
				status := u.minimaxSubmitStatus
				if status == 0 {
					status = http.StatusPaymentRequired
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(errBody))
				return
			}
		}
		id := fmt.Sprintf("mm-task-%d", u.submitHits)
		_, _ = w.Write([]byte(`{"task_id":"` + id + `"}`))
	case strings.HasPrefix(r.URL.Path, "/v2/query/video_generation/"):
		u.queryHits++
		u.lastQueryPath = r.URL.Path
		u.lastAuthHeader = r.Header.Get("Authorization")
		id := strings.TrimPrefix(r.URL.Path, "/v2/query/video_generation/")
		if errBody, scripted := u.minimaxQueryErrors[id]; scripted {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(errBody))
			return
		}
		queue := u.minimaxTasks[id]
		if len(queue) == 0 {
			_, _ = w.Write([]byte(`{"task":{"id":"` + id + `","status":"running"}}`))
			return
		}
		u.minimaxTasks[id] = queue[1:]
		_, _ = w.Write([]byte(queue[0]))
	case strings.HasPrefix(r.URL.Path, "/media/"):
		body, ok := u.media[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte(body))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// videoRig is the video vertical's fixture.
type videoRig struct {
	svc      *Service
	db       *gorm.DB
	key      *model.APIKey
	upstream *wanUpstream
	server   *httptest.Server
	modelID  uint
}

func newVideoRig(t *testing.T, providerModel string) *videoRig {
	t.Helper()
	rig := &videoRig{upstream: newWanUpstream()}
	rig.server = httptest.NewServer(rig.upstream)
	t.Cleanup(rig.server.Close)

	// The DashScope gate matches by hostname; a local test server never
	// carries the real one, so the gate is overridden to this rig's base —
	// the same override the images vertical uses.
	prev := isDashScopeBase
	isDashScopeBase = func(baseURL string) bool { return baseURL == rig.server.URL }
	t.Cleanup(func() { isDashScopeBase = prev })

	rig.db = testutil.NewSQLiteDB(t)
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "video-provider", rig.server.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-video-up", "video-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "video-model", providerModel, false, false, 1)
	setOutputModalities(t, rig.db, m.ID, `["video"]`)
	rig.modelID = m.ID
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	return rig
}

// newVideoArkRig is the rig on the ark dialect: same fake upstream, the
// ark gate flipped on and the dashscope gate off, so Supports and the
// poller both take the ark branch.
func newVideoArkRig(t *testing.T, providerModel string) *videoRig {
	rig := newVideoRig(t, providerModel)
	prevArk := isArkBase
	isArkBase = func(baseURL string) bool { return baseURL == rig.server.URL }
	prevDS := isDashScopeBase
	isDashScopeBase = func(string) bool { return false }
	t.Cleanup(func() {
		isArkBase = prevArk
		isDashScopeBase = prevDS
	})
	return rig
}

// newVideoKlingRig is the rig on the kling dialect: same fake upstream,
// the kling gate flipped on and the other two off.
func newVideoKlingRig(t *testing.T, providerModel string) *videoRig {
	rig := newVideoRig(t, providerModel)
	prevKling := isKlingBase
	isKlingBase = func(baseURL string) bool { return baseURL == rig.server.URL }
	prevDS := isDashScopeBase
	isDashScopeBase = func(string) bool { return false }
	t.Cleanup(func() {
		isKlingBase = prevKling
		isDashScopeBase = prevDS
	})
	return rig
}

// newVideoMiniMaxRig is the rig on the minimax dialect: same fake upstream,
// the minimax gate flipped on and the dashscope gate off.
func newVideoMiniMaxRig(t *testing.T, providerModel string) *videoRig {
	rig := newVideoRig(t, providerModel)
	prevMM := isMiniMaxBase
	isMiniMaxBase = func(baseURL string) bool { return baseURL == rig.server.URL }
	prevDS := isDashScopeBase
	isDashScopeBase = func(string) bool { return false }
	t.Cleanup(func() {
		isMiniMaxBase = prevMM
		isDashScopeBase = prevDS
	})
	return rig
}

func (r *videoRig) submit(t *testing.T, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := newCtxPath("/v1/videos", []byte(body))
	c.Set("request_id", "req-video-e2e")
	c.Set(BodiesDirContextKey, t.TempDir())
	r.svc.Handle(c, r.key)
	return c, w
}

// poll drives the job resource route the way the router would, for the
// rig's caller key or a foreign one.
func (r *videoRig) poll(t *testing.T, key *model.APIKey, jobID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+jobID, nil)
	c.Params = gin.Params{{Key: "id", Value: jobID}}
	c.Set(gatewayAPIKeyKey, key)
	c.Set("request_id", "req-video-poll")
	GetVideoResource(r.svc)(c)
	return w
}

func (r *videoRig) content(t *testing.T, jobID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/videos/"+jobID+"/content", nil)
	c.Params = gin.Params{{Key: "id", Value: jobID}}
	c.Set(gatewayAPIKeyKey, r.key)
	c.Set("request_id", "req-video-content")
	GetVideoContent(r.svc)(c)
	return w
}

// agePoll pulls the poll throttle back so a second observation can happen
// in the same test without sleeping the interval.
func (r *videoRig) agePoll(t *testing.T, jobID string) {
	t.Helper()
	// Read-then-write through gorm rather than raw SQL: the claim's
	// compare-and-set matches the stored stamp by value, and a raw
	// datetime() string would not round-trip to the same bytes gorm binds.
	var task model.VideoTask
	if err := r.db.Where("id = ?", jobID).First(&task).Error; err != nil {
		t.Fatalf("load task: %v", err)
	}
	if task.LastPolledAt == nil {
		t.Fatal("fixture bug: aging a task that was never polled")
	}
	aged := task.LastPolledAt.Add(-10 * time.Second)
	if err := r.db.Model(&model.VideoTask{}).Where("id = ?", jobID).Update("last_polled_at", aged).Error; err != nil {
		t.Fatalf("age poll stamp: %v", err)
	}
}

func jobIDOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil || doc.ID == "" {
		t.Fatalf("no job id in submit answer: %d %s", w.Code, w.Body.String())
	}
	return doc.ID
}

func TestVideoSubmitAndPollEndToEnd(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	rig.upstream.tasks["up-task-1"] = []string{
		`{"output":{"task_status":"RUNNING"},"usage":{}}`,
		`{"output":{"task_status":"SUCCEEDED","video_url":"` + rig.server.URL + `/media/v.mp4"},"usage":{"duration":8,"video_count":1}}`,
	}
	rig.upstream.media["/media/v.mp4"] = "mp4-bytes-here"

	_, w := rig.submit(t, `{"model":"video-model","prompt":"a calico cat at a piano","seconds":8,"size":"1024x1792"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("submit status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var created struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Status  string `json:"status"`
		Size    string `json:"size"`
		Seconds string `json:"seconds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("submit answer is not a job resource: %v", err)
	}
	if created.Object != "video" || created.Status != "queued" || created.Size != "1024x1792" || created.Seconds != "8" {
		t.Fatalf("submit answer shape wrong: %s", w.Body.String())
	}
	jobID := created.ID
	if !strings.HasPrefix(jobID, "vid_") {
		t.Fatalf("job id must be gateway-minted, got %q", jobID)
	}

	// The submit reached the upstream in the native dialect: the async
	// header, the bearer, and the mapped axes.
	if rig.upstream.lastAsyncHdr != "enable" {
		t.Fatalf("submit must carry X-DashScope-Async: enable, got %q", rig.upstream.lastAsyncHdr)
	}
	if !strings.HasPrefix(rig.upstream.lastAuthHeader, "Bearer ") {
		t.Fatalf("submit must carry bearer auth, got %q", rig.upstream.lastAuthHeader)
	}
	var sent map[string]any
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("submit body is not JSON: %v", err)
	}
	if sent["model"] != "wan2.7-t2v" {
		t.Fatalf("submit model = %v, want the candidate's provider name", sent["model"])
	}
	params := sent["parameters"].(map[string]any)
	if params["resolution"] != "1080P" || params["ratio"] != "9:16" || params["duration"] != float64(8) {
		t.Fatalf("size/seconds mapping wrong: %v", params)
	}
	input := sent["input"].(map[string]any)
	if input["prompt"] != "a calico cat at a piano" {
		t.Fatalf("wan2.7 family carries the flat input.prompt, got %v", input)
	}
	if _, ok := input["media"]; ok {
		t.Fatalf("a text-only submit carries no media array, got %v", input)
	}

	// The task row exists in pending with the sanitized snapshot.
	var task model.VideoTask
	if err := rig.db.Where("id = ?", jobID).First(&task).Error; err != nil {
		t.Fatalf("task row missing: %v", err)
	}
	if task.Status != model.VideoTaskPending || task.ProviderTaskID != "up-task-1" || task.APIKeyID != rig.key.ID {
		t.Fatalf("task row wrong: %+v", task)
	}
	if !strings.Contains(task.RequestSnapshot, "calico cat") {
		t.Fatalf("snapshot must keep the prompt: %q", task.RequestSnapshot)
	}

	// Poll 1: in_progress. Poll 2 (throttle aged): completed.
	w = rig.poll(t, rig.key, jobID)
	if w.Code != http.StatusOK {
		t.Fatalf("poll 1 status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"in_progress"`) {
		t.Fatalf("poll 1 must show in_progress: %s", w.Body.String())
	}
	rig.agePoll(t, jobID)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"completed"`) || !strings.Contains(w.Body.String(), `"completed_at"`) {
		t.Fatalf("poll 2 must show completed with a timestamp: %s", w.Body.String())
	}
	var completedDoc struct {
		CompletedAt *int64 `json:"completed_at"`
		ExpiresAt   *int64 `json:"expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &completedDoc); err != nil || completedDoc.CompletedAt == nil || completedDoc.ExpiresAt == nil {
		t.Fatalf("a completed job must carry both timestamps: %s", w.Body.String())
	}
	// The wire's expires_at is the upstream result URL's own 24h window.
	if *completedDoc.ExpiresAt-*completedDoc.CompletedAt != 24*3600 {
		t.Fatalf("expires_at must be completed_at + 24h, got %d..%d", *completedDoc.CompletedAt, *completedDoc.ExpiresAt)
	}
	if rig.upstream.queryHits != 2 {
		t.Fatalf("two due polls must mean two upstream queries, got %d", rig.upstream.queryHits)
	}

	// Content: the upstream's bytes, proxied.
	w = rig.content(t, jobID)
	if w.Code != http.StatusOK || w.Body.String() != "mp4-bytes-here" {
		t.Fatalf("content proxy wrong: %d %q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Type"), "video/mp4") {
		t.Fatalf("content type must be forwarded, got %q", w.Header().Get("Content-Type"))
	}

	// The submit left exactly one request log row — the polls and the
	// content download are task-domain reads, not relay requests.
	var logCount int64
	rig.db.Model(&model.RequestLog{}).Where("model_name = ?", "video-model").Count(&logCount)
	if logCount != 1 {
		t.Fatalf("a submit and its polls must leave one request log row, got %d", logCount)
	}
}

func TestVideoContentWithDeadUpstreamURLIsNotFound(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	// The result URL points at a path this upstream does not serve: the
	// caller's download is a miss, not a hang or a gateway error.
	rig.upstream.tasks["up-task-1"] = []string{
		`{"output":{"task_status":"SUCCEEDED","video_url":"` + rig.server.URL + `/media/gone.mp4"},"usage":{"duration":4}}`,
	}
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
	jobID := jobIDOf(t, w)
	rig.poll(t, rig.key, jobID)
	if w := rig.content(t, jobID); w.Code != http.StatusNotFound {
		t.Fatalf("a dead upstream result URL must 404, got %d", w.Code)
	}
}

func TestVideoLegacyFamilySubmitsFlatForm(t *testing.T) {
	rig := newVideoRig(t, "wan2.6-t2v")
	_, w := rig.submit(t, `{"model":"video-model","prompt":"legacy shape","size":"1280x720"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body %s", w.Code, w.Body.String())
	}
	var sent map[string]any
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("submit body: %v", err)
	}
	input := sent["input"].(map[string]any)
	if _, ok := input["img_url"]; ok {
		t.Fatalf("a text-only submit must not carry img_url: %v", input)
	}
	if input["prompt"] != "legacy shape" {
		t.Fatalf("legacy family carries the flat prompt field, got %v", input)
	}
	params := sent["parameters"].(map[string]any)
	if params["resolution"] != "720P" || params["ratio"] != "16:9" || params["duration"] != float64(4) {
		t.Fatalf("default size/seconds mapping wrong: %v", params)
	}
}

func TestVideoReferenceImageReachesUpstream(t *testing.T) {
	rig := newVideoRig(t, "wan2.6-i2v")
	pixels := "QUJDREVG" + strings.Repeat("h6tF", 300)
	body := `{"model":"video-model","prompt":"animate this","input_reference":{"image_url":"data:image/png;base64,` + pixels + `"}}`
	_, w := rig.submit(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("submit status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(rig.upstream.lastSubmitBody), pixels) {
		t.Fatalf("the reference image must reach the upstream")
	}
	// And neither audit surface keeps the pixels: the task's request
	// snapshot, and the upstream request body the log policy stores —
	// the re-encoded native spelling is where a caller-side-only
	// redactor would let them back in.
	jobID := jobIDOf(t, w)
	var task model.VideoTask
	_ = rig.db.Where("id = ?", jobID).First(&task).Error
	if strings.Contains(task.RequestSnapshot, pixels) {
		t.Fatalf("the snapshot must redact reference pixels")
	}
	var logBody model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-video-e2e").First(&logBody).Error; err != nil {
		t.Fatalf("the submit's log body row must exist: %v", err)
	}
	if strings.Contains(logBody.UpstreamRequestBody, pixels) {
		t.Fatalf("the stored upstream request must redact reference pixels: %.160s", logBody.UpstreamRequestBody)
	}
}

func TestVideoFailurePathsRenderThroughErrorChannel(t *testing.T) {
	cases := []struct {
		name string
		// upstream poll answer after a successful submit
		poll       string
		wantStatus string
		wantCode   string
	}{
		{
			name:       "failed carries the upstream's error",
			poll:       `{"output":{"task_status":"FAILED","code":"InvalidParameter","message":"prompt too short"},"usage":{}}`,
			wantStatus: "failed", wantCode: "InvalidParameter",
		},
		{
			name:       "cancelled maps to failed with the gateway's code",
			poll:       `{"output":{"task_status":"CANCELED"},"usage":{}}`,
			wantStatus: "failed", wantCode: "task_cancelled",
		},
		{
			name:       "unknown maps to expired, rendered as failed",
			poll:       `{"output":{"task_status":"UNKNOWN"},"usage":{}}`,
			wantStatus: "failed", wantCode: "task_expired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVideoRig(t, "wan2.7-t2v")
			rig.upstream.tasks["up-task-1"] = []string{tc.poll}
			_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("submit status = %d, body %s", w.Code, w.Body.String())
			}
			jobID := jobIDOf(t, w)
			w = rig.poll(t, rig.key, jobID)
			var res struct {
				Status string `json:"status"`
				Error  *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatalf("poll answer: %v (%s)", err, w.Body.String())
			}
			if res.Status != tc.wantStatus || res.Error == nil || res.Error.Code != tc.wantCode {
				t.Fatalf("rendering wrong: %s", w.Body.String())
			}
		})
	}
}

func TestVideoOwnershipIsANotFound(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
	jobID := jobIDOf(t, w)
	now := time.Now().UTC()
	foreign := &model.APIKey{KeyHash: ycrypto.HashToken("sk-yr-video-foreign"), KeyPrefix: "sk-yr-video-foreign", Status: model.APIKeyStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := rig.db.Create(foreign).Error; err != nil {
		t.Fatalf("seed foreign key: %v", err)
	}
	w = rig.poll(t, foreign, jobID)
	if w.Code != http.StatusNotFound {
		t.Fatalf("a foreign key must read 404, got %d %s", w.Code, w.Body.String())
	}
}

func TestVideoContentBeforeCompletionIsNotFound(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
	jobID := jobIDOf(t, w)
	if w := rig.content(t, jobID); w.Code != http.StatusNotFound {
		t.Fatalf("content on a pending job must 404, got %d", w.Code)
	}
}

func TestVideoSubmitFailoverToSecondCandidate(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	// A second provider whose submits fail; the candidate ordering puts it
	// first, so the gateway must fail over to the healthy one.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"InternalError","message":"broken"}`))
	}))
	t.Cleanup(broken.Close)
	brokenProvider := createProvider(t, rig.db, "video-broken", broken.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, brokenProvider.ID, "sk-video-broken", "broken-key", 1, true)
	var healthy model.Provider
	if err := rig.db.Where("name = ?", "video-provider").First(&healthy).Error; err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", rig.modelID).Update("sort_order", 2).Error; err != nil {
		t.Fatalf("fixture: %v", err)
	}
	now := time.Now().UTC()
	brokenCand := &model.ModelCandidate{
		ModelID: rig.modelID, ProviderID: brokenProvider.ID, ProviderModelName: "wan2.7-t2v",
		InputPrice: 1, OutputPrice: 2, MaxOutput: 4096,
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed, CreatedAt: now, UpdatedAt: now,
	}
	if err := rig.db.Create(brokenCand).Error; err != nil {
		t.Fatalf("seed broken candidate: %v", err)
	}

	_, w := rig.submit(t, `{"model":"video-model","prompt":"failover me"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("submit must fail over to the healthy candidate, got %d %s", w.Code, w.Body.String())
	}
	if rig.upstream.submitHits != 1 {
		t.Fatalf("the healthy upstream must have received the submit, hits=%d", rig.upstream.submitHits)
	}
}

func TestVideoBusinessRefusalIsAnsweredNotFailedOver(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	// A 400 from the upstream is classified by the kernel; what must NOT
	// happen is a success. The kernel's aggregation answers the caller.
	rig.upstream.submits = []string{`{"code":"InvalidApiKey","message":"bad key"}`}
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("a refused submit must not answer 200, body %s", w.Body.String())
	}
	var taskCount int64
	rig.db.Model(&model.VideoTask{}).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("a refused submit must leave no task row, got %d", taskCount)
	}
}

// priceVideo switches the rig's candidate to per-second video billing
// at one tier for every resolution, and optionally caps the caller key's
// budget.
func (r *videoRig) priceVideo(t *testing.T, sellPrice float64, limitMicros *int64) {
	t.Helper()
	tiers, err := model.MarshalVideoPricingTiers(&model.VideoPricingTiers{Tiers: []model.VideoPricingTier{
		{Resolution: "", PurchasePrice: 0, SellPrice: sellPrice},
	}})
	if err != nil {
		t.Fatalf("marshal tiers: %v", err)
	}
	if err := r.db.Model(&model.ModelCandidate{}).Where("model_id = ?", r.modelID).
		Updates(map[string]any{"billing_mode": model.BillingModeVideo, "video_pricing_tiers": tiers}).Error; err != nil {
		t.Fatalf("price candidate: %v", err)
	}
	if limitMicros != nil {
		if err := r.db.Model(&model.APIKey{}).Where("id = ?", r.key.ID).
			Update("budget_limit_micros", *limitMicros).Error; err != nil {
			t.Fatalf("limit key: %v", err)
		}
	}
}

func (r *videoRig) keySpent(t *testing.T) int64 {
	t.Helper()
	var key model.APIKey
	if err := r.db.Where("id = ?", r.key.ID).First(&key).Error; err != nil {
		t.Fatalf("reload key: %v", err)
	}
	return key.BudgetSpentMicros
}

func TestVideoPricedSubmitSettlesExactlyOnce(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	rig.priceVideo(t, 0.5, nil)
	rig.upstream.tasks["up-task-1"] = []string{
		`{"output":{"task_status":"RUNNING"},"usage":{}}`,
		`{"output":{"task_status":"SUCCEEDED","video_url":"` + rig.server.URL + `/media/v.mp4"},"usage":{"duration":8}}`,
	}

	_, w := rig.submit(t, `{"model":"video-model","prompt":"priced","seconds":8}`)
	if w.Code != http.StatusOK {
		t.Fatalf("priced submit must pass, got %d %s", w.Code, w.Body.String())
	}
	jobID := jobIDOf(t, w)
	var task model.VideoTask
	_ = rig.db.Where("id = ?", jobID).First(&task).Error
	if task.EstimatedMicros != 4_000_000 {
		t.Fatalf("the submit-time bound must land on the row, got %d", task.EstimatedMicros)
	}

	rig.poll(t, rig.key, jobID) // RUNNING
	rig.agePoll(t, jobID)
	rig.poll(t, rig.key, jobID) // SUCCEEDED → settle
	if spent := rig.keySpent(t); spent != 4_000_000 {
		t.Fatalf("completion must charge observed seconds once, spent=%d", spent)
	}
	rig.agePoll(t, jobID)
	rig.poll(t, rig.key, jobID) // completed re-read settles nothing further
	if spent := rig.keySpent(t); spent != 4_000_000 {
		t.Fatalf("re-reading a settled task must not recharge, spent=%d", spent)
	}
}

func TestVideoBudgetExceededAnswers429(t *testing.T) {
	rig := newVideoRig(t, "wan2.7-t2v")
	limit := int64(1_000_000)
	rig.priceVideo(t, 0.5, &limit) // an 8s ask is 4,000,000: over the ceiling

	_, w := rig.submit(t, `{"model":"video-model","prompt":"too rich","seconds":8}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("an over-budget submit must answer 429, got %d %s", w.Code, w.Body.String())
	}
	var taskCount int64
	rig.db.Model(&model.VideoTask{}).Count(&taskCount)
	if taskCount != 0 {
		t.Fatalf("a refused submit must leave no task row, got %d", taskCount)
	}
	if spent := rig.keySpent(t); spent != 0 {
		t.Fatalf("nothing was charged, spent=%d", spent)
	}
}

func TestArkVideoSubmitAndPollEndToEnd(t *testing.T) {
	rig := newVideoArkRig(t, "doubao-seedance-2-0-260128")
	rig.priceVideo(t, 0.5, nil)
	rig.upstream.arkTasks["ark-task-1"] = []string{
		`{"id":"ark-task-1","status":"running"}`,
		`{"id":"ark-task-1","status":"succeeded","content":{"video_url":"` + rig.server.URL + `/media/v.mp4"},"duration":8}`,
	}
	rig.upstream.media["/media/v.mp4"] = "ark-mp4-bytes"

	_, w := rig.submit(t, `{"model":"video-model","prompt":"a lantern festival at night","seconds":8,"size":"1280x720"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ark submit status = %d, body %s", w.Code, w.Body.String())
	}
	// The submit reached the upstream in the ark shape: content[] items,
	// lowercase resolution, the stated ratio of a text-only ask, and no
	// async header — ark's task endpoint is task-only already.
	var sent struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Resolution string `json:"resolution"`
		Ratio      string `json:"ratio"`
		Duration   int    `json:"duration"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("ark submit body: %v", err)
	}
	if len(sent.Content) != 1 || sent.Content[0].Type != "text" || sent.Content[0].Text != "a lantern festival at night" {
		t.Fatalf("ark content shape wrong: %s", rig.upstream.lastSubmitBody)
	}
	if sent.Resolution != "720p" || sent.Ratio != "16:9" || sent.Duration != 8 {
		t.Fatalf("ark knobs wrong: %s", rig.upstream.lastSubmitBody)
	}
	if rig.upstream.lastAsyncHdr != "" {
		t.Fatalf("the ark dialect carries no async header, got %q", rig.upstream.lastAsyncHdr)
	}

	jobID := jobIDOf(t, w)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"in_progress"`) {
		t.Fatalf("ark poll 1 must show in_progress: %s", w.Body.String())
	}
	rig.agePoll(t, jobID)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"completed"`) {
		t.Fatalf("ark poll 2 must show completed: %s", w.Body.String())
	}
	// Settlement rides the echoed duration: 8s × 0.5 = 4,000,000.
	if spent := rig.keySpent(t); spent != 4_000_000 {
		t.Fatalf("ark settlement must charge the echoed duration once, spent=%d", spent)
	}
	w = rig.content(t, jobID)
	if w.Code != http.StatusOK || w.Body.String() != "ark-mp4-bytes" {
		t.Fatalf("ark content proxy wrong: %d %q", w.Code, w.Body.String())
	}
}

func TestArkVideoReferenceAndFailurePaths(t *testing.T) {
	t.Run("reference rides the content array and omits ratio", func(t *testing.T) {
		rig := newVideoArkRig(t, "doubao-seedance-2-0-260128")
		body := `{"model":"video-model","prompt":"animate this","input_reference":{"image_url":"https://example.test/first.png"}}`
		_, w := rig.submit(t, body)
		if w.Code != http.StatusOK {
			t.Fatalf("submit %d %s", w.Code, w.Body.String())
		}
		var sent struct {
			Content []struct {
				Type     string `json:"type"`
				Role     string `json:"role"`
				ImageURL *struct {
					URL string `json:"url"`
				} `json:"image_url"`
			} `json:"content"`
			Ratio string `json:"ratio"`
		}
		if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
			t.Fatalf("body: %v", err)
		}
		if len(sent.Content) != 2 || sent.Content[1].Role != "first_frame" || sent.Content[1].ImageURL == nil ||
			sent.Content[1].ImageURL.URL != "https://example.test/first.png" {
			t.Fatalf("ark reference shape wrong: %s", rig.upstream.lastSubmitBody)
		}
		if sent.Ratio != "" {
			t.Fatalf("an image-referenced ark submit must not state a ratio: %s", rig.upstream.lastSubmitBody)
		}
	})
	for _, tc := range []struct {
		name, poll, wantStatus, wantCode string
	}{
		{
			name:       "failed carries the upstream's error",
			poll:       `{"id":"a","status":"failed","error":{"code":"SensitiveContentDetected","message":"refused"}}`,
			wantStatus: "failed", wantCode: "SensitiveContentDetected",
		},
		{
			name:       "cancelled maps through the error channel",
			poll:       `{"id":"a","status":"cancelled"}`,
			wantStatus: "failed", wantCode: "task_cancelled",
		},
		{
			name:       "expired maps through the error channel",
			poll:       `{"id":"a","status":"expired"}`,
			wantStatus: "failed", wantCode: "task_expired",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVideoArkRig(t, "doubao-seedance-2-0-260128")
			rig.priceVideo(t, 0.5, nil)
			rig.upstream.arkTasks["ark-task-1"] = []string{tc.poll}
			_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("submit %d %s", w.Code, w.Body.String())
			}
			jobID := jobIDOf(t, w)
			w = rig.poll(t, rig.key, jobID)
			var res struct {
				Status string `json:"status"`
				Error  *struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
				t.Fatalf("poll answer: %v (%s)", err, w.Body.String())
			}
			if res.Status != tc.wantStatus || res.Error == nil || res.Error.Code != tc.wantCode {
				t.Fatalf("rendering wrong: %s", w.Body.String())
			}
			if spent := rig.keySpent(t); spent != 0 {
				t.Fatalf("a %s task must bill nothing, spent=%d", tc.wantStatus, spent)
			}
		})
	}
}

func TestKlingVideoSubmitAndPollEndToEnd(t *testing.T) {
	rig := newVideoKlingRig(t, "kling-3.0-turbo")
	rig.priceVideo(t, 0.8, nil)
	rig.upstream.klingTasks["kling-task-1"] = []string{
		`{"code":0,"data":[{"id":"kling-task-1","status":"processing"}]}`,
		`{"code":0,"data":[{"id":"kling-task-1","status":"succeeded","outputs":[{"type":"video","url":"` + rig.server.URL + `/media/v.mp4","duration":"8"}]}]}`,
	}
	rig.upstream.media["/media/v.mp4"] = "kling-mp4-bytes"

	_, w := rig.submit(t, `{"model":"video-model","prompt":"a lantern festival at night","seconds":8,"size":"1024x1792"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("kling submit status = %d, body %s", w.Code, w.Body.String())
	}
	// The submit reached the upstream in the new-design shape: the model
	// rides in the path, settings carries the mapped axes with the stated
	// aspect ratio of a text-only ask, and there is no options block and
	// no async header.
	if rig.upstream.lastSubmitPath != "/text-to-video/kling-3.0-turbo" {
		t.Fatalf("submit path = %q, want the version-in-path route", rig.upstream.lastSubmitPath)
	}
	if rig.upstream.lastAsyncHdr != "" {
		t.Fatalf("the kling dialect carries no async header, got %q", rig.upstream.lastAsyncHdr)
	}
	if !strings.HasPrefix(rig.upstream.lastAuthHeader, "Bearer ") {
		t.Fatalf("submit must carry the single-key bearer, got %q", rig.upstream.lastAuthHeader)
	}
	var sent struct {
		Prompt   string `json:"prompt"`
		Settings struct {
			Resolution  string `json:"resolution"`
			AspectRatio string `json:"aspect_ratio"`
			Duration    int    `json:"duration"`
		} `json:"settings"`
		Contents any `json:"contents"`
		Options  any `json:"options"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("kling submit body: %v", err)
	}
	if sent.Prompt != "a lantern festival at night" {
		t.Fatalf("prompt must ride the flat field, got %q", sent.Prompt)
	}
	if sent.Settings.Resolution != "1080p" || sent.Settings.AspectRatio != "9:16" || sent.Settings.Duration != 8 {
		t.Fatalf("settings knobs wrong: %s", rig.upstream.lastSubmitBody)
	}
	if sent.Contents != nil || sent.Options != nil {
		t.Fatalf("a text-only submit carries neither contents nor options: %s", rig.upstream.lastSubmitBody)
	}

	jobID := jobIDOf(t, w)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"in_progress"`) {
		t.Fatalf("kling poll 1 must show in_progress: %s", w.Body.String())
	}
	// The query asked the one-route shape: the id as a query parameter.
	if !strings.HasPrefix(rig.upstream.lastQueryPath, "/tasks?task_ids=kling-task-1") {
		t.Fatalf("the kling poll must carry the task id as task_ids, got %q", rig.upstream.lastQueryPath)
	}
	rig.agePoll(t, jobID)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"completed"`) {
		t.Fatalf("kling poll 2 must show completed: %s", w.Body.String())
	}
	// Settlement rides the delivered-duration string: 8s × 0.8 = 6,400,000.
	if spent := rig.keySpent(t); spent != 6_400_000 {
		t.Fatalf("kling settlement must charge the delivered seconds once, spent=%d", spent)
	}
	w = rig.content(t, jobID)
	if w.Code != http.StatusOK || w.Body.String() != "kling-mp4-bytes" {
		t.Fatalf("kling content proxy wrong: %d %q", w.Code, w.Body.String())
	}
}

func TestKlingVideoReferenceRidesContents(t *testing.T) {
	rig := newVideoKlingRig(t, "kling-3.0")
	body := `{"model":"video-model","prompt":"animate this","input_reference":{"image_url":"https://example.test/first.png"}}`
	_, w := rig.submit(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	// The reference's presence is the endpoint family.
	if rig.upstream.lastSubmitPath != "/image-to-video/kling-3.0" {
		t.Fatalf("a referenced submit must ride the image-to-video route, got %q", rig.upstream.lastSubmitPath)
	}
	var sent struct {
		Contents []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URL  string `json:"url"`
		} `json:"contents"`
		Settings struct {
			AspectRatio string `json:"aspect_ratio"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("body: %v (%s)", err, rig.upstream.lastSubmitBody)
	}
	if len(sent.Contents) != 2 || sent.Contents[0].Type != "prompt" || sent.Contents[0].Text != "animate this" ||
		sent.Contents[1].Type != "first_frame" || sent.Contents[1].URL != "https://example.test/first.png" {
		t.Fatalf("contents shape wrong: %s", rig.upstream.lastSubmitBody)
	}
	if sent.Settings.AspectRatio != "" {
		t.Fatalf("a referenced submit must let the frame decide the aspect: %s", rig.upstream.lastSubmitBody)
	}
}

func TestKlingVideoReferenceStripsDataURIToBareBase64(t *testing.T) {
	rig := newVideoKlingRig(t, "kling-3.0")
	pixels := "cGxhaW4tcGl4ZWxz"
	body := `{"model":"video-model","prompt":"animate this","input_reference":{"image_url":"data:image/png;base64,` + pixels + `"}}`
	_, w := rig.submit(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(string(rig.upstream.lastSubmitBody), `"url":"`+pixels+`"`) {
		t.Fatalf("a data-URI reference must reach the upstream as bare base64: %s", rig.upstream.lastSubmitBody)
	}
	if strings.Contains(string(rig.upstream.lastSubmitBody), "data:image") {
		t.Fatalf("the data: prefix must not survive: %s", rig.upstream.lastSubmitBody)
	}
}

// A present-but-empty input_reference is the text generation it is: the
// endpoint choice follows content, in lockstep with the encoded body.
func TestKlingEmptyReferenceRidesTextRoute(t *testing.T) {
	rig := newVideoKlingRig(t, "kling-3.0")
	_, w := rig.submit(t, `{"model":"video-model","prompt":"empty ref","input_reference":{}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	if rig.upstream.lastSubmitPath != "/text-to-video/kling-3.0" {
		t.Fatalf("an empty reference must ride the text route, got %q", rig.upstream.lastSubmitPath)
	}
	var sent map[string]any
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if _, ok := sent["contents"]; ok {
		t.Fatalf("an empty reference must encode the text shape: %s", rig.upstream.lastSubmitBody)
	}
}

func TestKlingFailurePaths(t *testing.T) {
	for _, tc := range []struct {
		name, poll, wantStatus, wantCode string
	}{
		{
			name:       "failed carries the upstream's message",
			poll:       `{"code":0,"data":[{"id":"k","status":"failed","message":"content risk control"}]}`,
			wantStatus: "failed", wantCode: "kling_task_failed",
		},
		{
			name:       "an unknown task id is the empty data array",
			poll:       `{"code":0,"message":"SUCCEED","data":[]}`,
			wantStatus: "failed", wantCode: "task_expired",
		},
		{
			name:       "the old API's succeed spelling is not a status",
			poll:       `{"code":0,"data":[{"id":"k","status":"succeed"}]}`,
			wantStatus: "in_progress", wantCode: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVideoKlingRig(t, "kling-3.0")
			rig.priceVideo(t, 0.8, nil)
			// An undocumented status word is a decode error, not a task
			// verdict: the task keeps its state, so that case asserts the
			// poll stays in_progress rather than rendering an error.
			if tc.wantStatus == "in_progress" {
				rig.upstream.klingTasks["kling-task-1"] = []string{
					`{"code":0,"data":[{"id":"k","status":"processing"}]}`,
					tc.poll,
				}
			} else {
				rig.upstream.klingTasks["kling-task-1"] = []string{tc.poll}
			}
			_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("submit %d %s", w.Code, w.Body.String())
			}
			jobID := jobIDOf(t, w)
			rig.poll(t, rig.key, jobID)
			if tc.wantStatus == "in_progress" {
				rig.agePoll(t, jobID)
			}
			w = rig.poll(t, rig.key, jobID)
			if !strings.Contains(w.Body.String(), `"`+tc.wantStatus+`"`) {
				t.Fatalf("rendering wrong: %s", w.Body.String())
			}
			if tc.wantCode != "" && !strings.Contains(w.Body.String(), tc.wantCode) {
				t.Fatalf("error code missing: %s", w.Body.String())
			}
			if spent := rig.keySpent(t); spent != 0 {
				t.Fatalf("a %s task must bill nothing, spent=%d", tc.wantStatus, spent)
			}
		})
	}
}

func TestMiniMaxVideoSubmitAndPollEndToEnd(t *testing.T) {
	rig := newVideoMiniMaxRig(t, "MiniMax-H3")
	rig.priceVideo(t, 0.5, nil)
	rig.upstream.minimaxTasks["mm-task-1"] = []string{
		`{"task":{"id":"mm-task-1","status":"running"}}`,
		`{"task":{"id":"mm-task-1","status":"succeeded","content":{"url":"` + rig.server.URL + `/media/v.mp4"},"resolution":"2K","duration":8,"usage":{"total_seconds":8,"input_seconds":0,"output_seconds":8,"input_image_count":0},"ratio":"9:16","task_type":"generation","modality":"video"}}`,
	}
	rig.upstream.media["/media/v.mp4"] = "minimax-mp4-bytes"

	_, w := rig.submit(t, `{"model":"video-model","prompt":"a paper lantern drifting up a stairwell","seconds":8,"size":"1024x1792"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("minimax submit status = %d, body %s", w.Code, w.Body.String())
	}
	// The submit reached the upstream in the V2 shape: one route, the model
	// in the body, the content array carrying the prompt, the large door
	// size riding at H3's 2K top with the stated ratio, and no watermark
	// knob anywhere.
	if rig.upstream.lastSubmitPath != "/v2/video_generation" {
		t.Fatalf("submit path = %q, want the one V2 route", rig.upstream.lastSubmitPath)
	}
	if !strings.HasPrefix(rig.upstream.lastAuthHeader, "Bearer ") {
		t.Fatalf("submit must carry the bearer key, got %q", rig.upstream.lastAuthHeader)
	}
	var sent struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Resolution string `json:"resolution"`
		Duration   int    `json:"duration"`
		Ratio      string `json:"ratio"`
		Watermark  any    `json:"aigc_watermark"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("minimax submit body: %v (%s)", err, rig.upstream.lastSubmitBody)
	}
	if sent.Model != "MiniMax-H3" || len(sent.Content) != 1 || sent.Content[0].Type != "text" ||
		sent.Content[0].Text != "a paper lantern drifting up a stairwell" {
		t.Fatalf("content shape wrong: %s", rig.upstream.lastSubmitBody)
	}
	if sent.Resolution != "2K" || sent.Ratio != "9:16" || sent.Duration != 8 {
		t.Fatalf("knobs wrong: %s", rig.upstream.lastSubmitBody)
	}
	if sent.Watermark != nil {
		t.Fatalf("aigc_watermark must be omitted: %s", rig.upstream.lastSubmitBody)
	}

	jobID := jobIDOf(t, w)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"in_progress"`) {
		t.Fatalf("minimax poll 1 must show in_progress: %s", w.Body.String())
	}
	// The query asked the path-append shape.
	if rig.upstream.lastQueryPath != "/v2/query/video_generation/mm-task-1" {
		t.Fatalf("the minimax poll must carry the task id in the path, got %q", rig.upstream.lastQueryPath)
	}
	rig.agePoll(t, jobID)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"completed"`) {
		t.Fatalf("minimax poll 2 must show completed: %s", w.Body.String())
	}
	// Settlement rides the stated output seconds: 8 × 0.5 = 4,000,000.
	if spent := rig.keySpent(t); spent != 4_000_000 {
		t.Fatalf("minimax settlement must charge the output seconds once, spent=%d", spent)
	}
	w = rig.content(t, jobID)
	if w.Code != http.StatusOK || w.Body.String() != "minimax-mp4-bytes" {
		t.Fatalf("minimax content proxy wrong: %d %q", w.Code, w.Body.String())
	}
}

func TestMiniMaxVideoReferenceRidesFirstFrame(t *testing.T) {
	rig := newVideoMiniMaxRig(t, "MiniMax-H3")
	body := `{"model":"video-model","prompt":"animate this","input_reference":{"image_url":"https://example.test/first.png"}}`
	_, w := rig.submit(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	// One route for every capability — the reference changes the body, not
	// the endpoint.
	if rig.upstream.lastSubmitPath != "/v2/video_generation" {
		t.Fatalf("the minimax dialect has one submit route, got %q", rig.upstream.lastSubmitPath)
	}
	var sent struct {
		Content []struct {
			Type     string `json:"type"`
			Role     string `json:"role"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
		Ratio string `json:"ratio"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("body: %v (%s)", err, rig.upstream.lastSubmitBody)
	}
	if len(sent.Content) != 2 || sent.Content[0].Type != "text" ||
		sent.Content[1].Type != "image_url" || sent.Content[1].Role != "first_frame" ||
		sent.Content[1].ImageURL.URL != "https://example.test/first.png" {
		t.Fatalf("content shape wrong: %s", rig.upstream.lastSubmitBody)
	}
	if sent.Ratio != "" {
		t.Fatalf("a referenced submit must let the frame decide the aspect: %s", rig.upstream.lastSubmitBody)
	}
}

func TestMiniMaxReferenceDataURIIsNormalizedNotStripped(t *testing.T) {
	rig := newVideoMiniMaxRig(t, "MiniMax-H3")
	pixels := "cGxhaW4tcGl4ZWxz" + strings.Repeat("h6tF", 300)
	body := `{"model":"video-model","prompt":"animate this","input_reference":{"image_url":"data:image/PNG;base64,` + pixels + `"}}`
	_, w := rig.submit(t, body)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	// The dialect's spelling is the data URI itself — kept, with the media
	// type token lowercased to the form the upstream documents.
	if !strings.Contains(string(rig.upstream.lastSubmitBody), `"url":"data:image/png;base64,`+pixels+`"`) {
		t.Fatalf("the data URI must ride whole with a lowercase media type: %.200s", rig.upstream.lastSubmitBody)
	}
	// No seconds stated means the door's default 4 — legal on H3, the one
	// door value the duration gate refuses only for H3-Max.
	var ask struct {
		Duration int `json:"duration"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &ask); err != nil {
		t.Fatalf("submit body: %v", err)
	}
	if ask.Duration != 4 {
		t.Fatalf("the door default 4 seconds must ride H3 verbatim, got %d", ask.Duration)
	}
	// And neither audit surface keeps the pixels: the task's request
	// snapshot and the stored upstream request body — the re-encoded native
	// spelling is where a caller-side-only redactor would let them back in.
	jobID := jobIDOf(t, w)
	var task model.VideoTask
	_ = rig.db.Where("id = ?", jobID).First(&task).Error
	if strings.Contains(task.RequestSnapshot, pixels) {
		t.Fatalf("the snapshot must redact reference pixels")
	}
	var logBody model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-video-e2e").First(&logBody).Error; err != nil {
		t.Fatalf("the submit's log body row must exist: %v", err)
	}
	if strings.Contains(logBody.UpstreamRequestBody, pixels) {
		t.Fatalf("the stored upstream request must redact reference pixels: %.160s", logBody.UpstreamRequestBody)
	}
}

// The large door sizes ride at 2K on H3 but H3-Max has no 2K — they stay at
// its 768P top.
func TestMiniMaxH3MaxMapsLargeSizeToItsTop(t *testing.T) {
	rig := newVideoMiniMaxRig(t, "MiniMax-H3-Max")
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p","seconds":8,"size":"1792x1024"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	var sent struct {
		Model      string `json:"model"`
		Resolution string `json:"resolution"`
		Duration   int    `json:"duration"`
		Ratio      string `json:"ratio"`
	}
	if err := json.Unmarshal(rig.upstream.lastSubmitBody, &sent); err != nil {
		t.Fatalf("body: %v (%s)", err, rig.upstream.lastSubmitBody)
	}
	if sent.Resolution != "768P" || sent.Ratio != "16:9" || sent.Duration != 8 {
		t.Fatalf("h3-max knobs wrong: %s", rig.upstream.lastSubmitBody)
	}
}

// The one model-dependent edge in the door's seconds vocabulary: a 4-second
// ask on H3-Max is refused with the reason, before any upstream is dialled.
// The refusal rides the candidate-gate channel — the same surface the kling
// model gate answers through: the caller sees the exhausted-chain answer,
// and the reason itself lands on the attempt record.
func TestMiniMaxH3MaxRefusesFourSeconds(t *testing.T) {
	rig := newVideoMiniMaxRig(t, "MiniMax-H3-Max")
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p","seconds":4}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the exhausted-chain 502; body = %s", w.Code, w.Body.String())
	}
	if rig.upstream.submitHits != 0 {
		t.Fatalf("the gate must refuse before any upstream submit, hits=%d", rig.upstream.submitHits)
	}
	if spent := rig.keySpent(t); spent != 0 {
		t.Fatalf("a refused submit must bill nothing, spent=%d", spent)
	}
	var log model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-video-e2e").First(&log).Error; err != nil {
		t.Fatalf("the refused submit still writes its log row: %v", err)
	}
	if log.AttemptsDetail == nil || !strings.Contains(*log.AttemptsDetail, "h3-max generates 5 to 15 second clips") {
		t.Fatalf("the duration reason must land on the attempt record, got %v", log.AttemptsDetail)
	}
}

// This vendor's refusals ride real HTTP statuses: the kernel's own
// classification answers the caller (the standing stance — the upstream
// body can echo request fragments — keeps the vendor wording in the audit
// row, not the caller face), and nothing is billed.
func TestMiniMaxUpstreamRefusalSurfacesStatus(t *testing.T) {
	cases := []struct {
		name            string
		status          int
		errType         string
		message         string
		wantCallerWords string // empty = the status digits must appear
	}{
		{
			name:    "insufficient balance answers its own 402",
			status:  http.StatusPaymentRequired,
			errType: "insufficient_balance_error",
			message: "insufficient balance (1008)",
		},
		{
			// A content-safety 422 additionally crosses the kernel's
			// cross-protocol content inspection, which rewords the caller
			// face to its vendor-neutral refusal — the caller never sees
			// the vendor's wording, the audit row still keeps it.
			name:            "sensitive content 422 reworded by the content inspection",
			status:          http.StatusUnprocessableEntity,
			errType:         "unprocessable_entity_error",
			message:         "video description contains sensitive content (1026)",
			wantCallerWords: "content inspection",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVideoMiniMaxRig(t, "MiniMax-H3")
			rig.upstream.minimaxSubmits = []string{
				`{"type":"error","error":{"type":"` + tc.errType + `","message":"` + tc.message + `","http_code":"` + strconv.Itoa(tc.status) + `"},"request_id":"r-1"}`,
			}
			rig.upstream.minimaxSubmitStatus = tc.status
			_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want the upstream's own %d; body = %s", w.Code, tc.status, w.Body.String())
			}
			wantWords := tc.wantCallerWords
			if wantWords == "" {
				wantWords = strconv.Itoa(tc.status)
			}
			if !strings.Contains(w.Body.String(), wantWords) {
				t.Fatalf("the caller answer must carry %q: %s", wantWords, w.Body.String())
			}
			if spent := rig.keySpent(t); spent != 0 {
				t.Fatalf("a refused submit must bill nothing, spent=%d", spent)
			}
			// The vendor's own wording lives on the audit row, not the
			// caller face — the refusal arrived with a real status, and
			// the kernel's standing stance keeps the body in storage.
			var logBody model.RequestLogBody
			if err := rig.db.Where("request_id = ?", "req-video-e2e").First(&logBody).Error; err != nil {
				t.Fatalf("the refused submit's log body row must exist: %v", err)
			}
			if !strings.Contains(logBody.UpstreamResponseBody, tc.message) {
				t.Fatalf("the vendor's message must land on the audit row, got %.200s", logBody.UpstreamResponseBody)
			}
		})
	}
}

// A poll answered 400 "invalid task_id" — the vendor's spelling for a
// task outside its 7-day query window — expires the task on sight,
// unbilled, instead of limping pending until the zombie horizon.
func TestMiniMaxWindowExhaustedPollExpiresUnbilled(t *testing.T) {
	rig := newVideoMiniMaxRig(t, "MiniMax-H3")
	rig.upstream.minimaxQueryErrors["mm-task-1"] = `{"type":"error","error":{"type":"bad_request_error","message":"invalid task_id","http_code":"400"},"request_id":"r-1"}`
	_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("submit %d %s", w.Code, w.Body.String())
	}
	jobID := jobIDOf(t, w)
	w = rig.poll(t, rig.key, jobID)
	if !strings.Contains(w.Body.String(), `"failed"`) || !strings.Contains(w.Body.String(), "task_expired") {
		t.Fatalf("a window-exhausted poll must render failed with task_expired, got %s", w.Body.String())
	}
	if spent := rig.keySpent(t); spent != 0 {
		t.Fatalf("an expired task must bill nothing, spent=%d", spent)
	}
}

func TestMiniMaxFailedAndCancelledTasksRenderUnbilled(t *testing.T) {
	cases := []struct {
		name       string
		poll       string
		wantStatus string
		wantCode   string
	}{
		{
			name:       "failed carries the vendor's error",
			poll:       `{"task":{"id":"mm-task-1","status":"failed","error":{"code":"1026","message":"video description contains sensitive content"},"duration":4,"usage":{}}}`,
			wantStatus: "failed", wantCode: "1026",
		},
		{
			name:       "cancelled maps to failed with the gateway's code",
			poll:       `{"task":{"id":"mm-task-1","status":"cancelled"}}`,
			wantStatus: "failed", wantCode: "task_cancelled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newVideoMiniMaxRig(t, "MiniMax-H3")
			rig.upstream.minimaxTasks["mm-task-1"] = []string{tc.poll}
			_, w := rig.submit(t, `{"model":"video-model","prompt":"p"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("submit %d %s", w.Code, w.Body.String())
			}
			jobID := jobIDOf(t, w)
			w = rig.poll(t, rig.key, jobID)
			if !strings.Contains(w.Body.String(), `"`+tc.wantStatus+`"`) {
				t.Fatalf("rendering wrong: %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantCode) {
				t.Fatalf("error code missing: %s", w.Body.String())
			}
			if spent := rig.keySpent(t); spent != 0 {
				t.Fatalf("a %s task must bill nothing, spent=%d", tc.wantStatus, spent)
			}
		})
	}
}
