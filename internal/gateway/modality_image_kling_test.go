package gateway

// Kling image dialect tests through the real dispatch path: the task
// submit reaching the dialect's endpoint, the delivery-side poller driving
// the task to its terminal state inside one synchronous answer, per-image
// billing counting what the task delivered, a failed task answered as 422
// without failover, and the shapes the dialect cannot serve refused per
// candidate.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// klingImageRig points the image rig's provider at a fake Kling upstream
// (submit + task routes) and turns the kling detector on for that base.
type klingImageRig struct {
	svc          *Service
	db           *gorm.DB
	key          *model.APIKey
	upstreamURL  string
	lastPath     string
	lastBody     []byte
	lastQueryURI string
	queryHits    int32
	taskAnswer   atomic.Value // string
}

func newKlingImageRig(t *testing.T, providerModel string) *klingImageRig {
	t.Helper()
	rig := &klingImageRig{}
	rig.db = testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "/v1/images/generations" || r.URL.Path == "/v1/images/omni-image"):
			rig.lastPath = r.URL.Path
			rig.lastBody, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"task_id":"951","task_status":"submitted"}}`))
		case r.Method == http.MethodGet && (strings.HasPrefix(r.URL.Path, "/v1/images/generations/") || strings.HasPrefix(r.URL.Path, "/v1/images/omni-image/")):
			atomic.AddInt32(&rig.queryHits, 1)
			rig.lastQueryURI = r.URL.RequestURI()
			_, _ = w.Write([]byte(rig.taskAnswer.Load().(string)))
		default:
			t.Errorf("unexpected kling route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(up.Close)
	prev := isKlingBase
	isKlingBase = func(baseURL string) bool { return baseURL == up.URL }
	t.Cleanup(func() { isKlingBase = prev })

	rig.upstreamURL = up.URL
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "kling-image-provider", up.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-kling-up", "kling-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "kling-image-test", providerModel, false, false, 1)
	setImageOutputModalities(t, rig.db, m.ID, `["image"]`)
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Updates(map[string]interface{}{
		"billing_mode":        model.BillingModeImage,
		"image_pricing_tiers": `{"mode":"per_image","default_price":0.02}`,
	}).Error; err != nil {
		t.Fatalf("seed billing: %v", err)
	}
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	rig.taskAnswer.Store(`{"code":0,"data":{"task_status":"succeed","final_unit_deduction":"0.04","task_result":{"images":[{"index":0,"url":"https://x.test/1.png"},{"index":1,"url":"https://x.test/2.png"}]}}}`)
	return rig
}

func TestKlingImageEndToEnd(t *testing.T) {
	rig := newKlingImageRig(t, "kling-v3")

	c, w := imageRequest(`{"model":"kling-image-test","prompt":"a fox","n":2,"size":"1024x1024"}`)
	c.Set("request_id", "req-kling-img-e2e")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rig.lastPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want the task submit endpoint", rig.lastPath)
	}
	var sent struct {
		ModelName   string `json:"model_name"`
		Prompt      string `json:"prompt"`
		N           int    `json:"n"`
		Resolution  string `json:"resolution"`
		AspectRatio string `json:"aspect_ratio"`
		Watermark   any    `json:"watermark_info"`
	}
	if err := json.Unmarshal(rig.lastBody, &sent); err != nil {
		t.Fatalf("upstream body did not parse: %v (%s)", err, rig.lastBody)
	}
	if sent.ModelName != "kling-v3" || sent.Prompt != "a fox" || sent.N != 2 ||
		sent.Resolution != "1k" || sent.AspectRatio != "1:1" {
		t.Errorf("upstream shape wrong: %s", rig.lastBody)
	}
	if sent.Watermark != nil {
		t.Errorf("watermark must stay omitted: %s", rig.lastBody)
	}
	if hits := atomic.LoadInt32(&rig.queryHits); hits != 1 {
		t.Errorf("task queries = %d, want the terminal read on the first poll", hits)
	}

	var received struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &received); err != nil {
		t.Fatalf("caller body did not parse: %v (%s)", err, w.Body.String())
	}
	if received.Created == 0 || len(received.Data) != 2 || received.Data[0].URL != "https://x.test/1.png" {
		t.Fatalf("caller data wrong: %s", w.Body.String())
	}

	// Billed per delivered image at the default price: 2 × 0.02.
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-kling-img-e2e").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if !row.CostKnown || row.CostMicros != 40000 {
		t.Errorf("cost = known:%v %d, want known 40000 (0.02 × 2)", row.CostKnown, row.CostMicros)
	}
	if row.ImageCount != 2 {
		t.Errorf("image_count = %d, want 2", row.ImageCount)
	}
}

// A failed task is answered as 422 with the upstream's own reason — no
// failover, no bill: the task already failed, a second submit would only
// buy the same refusal again.
func TestKlingImageFailedTaskIsAnsweredNotRetried(t *testing.T) {
	rig := newKlingImageRig(t, "kling-v3")
	rig.taskAnswer.Store(`{"code":0,"data":{"task_status":"failed","task_status_msg":"content risk control"}}`)

	c, w := imageRequest(`{"model":"kling-image-test","prompt":"a fox"}`)
	c.Set("request_id", "req-kling-img-failed")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "content risk control") {
		t.Fatalf("error must carry the task's own reason: %s", w.Body.String())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-kling-img-failed").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a failed task")
	}
}

// A submit-time business refusal (HTTP 200, code non-empty) is answered as
// 422 in the OpenAI error shape.
func TestKlingImageBusinessRefusalIsAnswered(t *testing.T) {
	rig := newKlingImageRig(t, "kling-v3")
	// A refusal at submit time: a second stub answers every route with the
	// upstream's real refusal shape, and the rig's provider is repointed
	// at it — the refusal lands before any task route is needed.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1102,"message":"Account balance not enough","data":null}`))
	}))
	t.Cleanup(refused.Close)
	prev := isKlingBase
	isKlingBase = func(baseURL string) bool { return baseURL == refused.URL }
	t.Cleanup(func() { isKlingBase = prev })
	if err := rig.db.Model(&model.Provider{}).Where("id = ?", 1).Update("base_url", refused.URL).Error; err != nil {
		t.Fatalf("repoint provider: %v", err)
	}
	// The provider row the poller would load carries the fake base; the
	// refusal happens before any poll, so no task route is needed.

	c, w := imageRequest(`{"model":"kling-image-test","prompt":"a fox"}`)
	c.Set("request_id", "req-kling-img-biz")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "1102") || !strings.Contains(w.Body.String(), "Account balance not enough") {
		t.Fatalf("error must carry code and message: %s", w.Body.String())
	}
}

// The shapes the dialect cannot serve are refused per candidate: b64_json
// (URLs only) and a model off the endpoint list.
func TestKlingImagePerCandidateRefusals(t *testing.T) {
	rig := newKlingImageRig(t, "kling-v3")
	rig.taskAnswer.Store(`{"code":0,"data":{"task_status":"submitted"}}`) // never terminal; refusal must precede dispatch

	c, w := imageRequest(`{"model":"kling-image-test","prompt":"a fox","response_format":"b64_json"}`)
	c.Set("request_id", "req-kling-img-b64")
	rig.svc.Handle(c, rig.key)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("b64_json ask: status = %d, want 502 from the refused chain", w.Code)
	}
	if hits := atomic.LoadInt32(&rig.queryHits); hits != 0 {
		t.Errorf("a refused candidate must not be dispatched, task queries = %d", hits)
	}

	// An edits ask is refused per candidate — the dialect serves
	// generations only. The multipart shape only has to parse; the
	// refusal happens at Supports, before any dispatch.
	var editBuf bytes.Buffer
	editMW := multipart.NewWriter(&editBuf)
	_ = editMW.WriteField("model", "kling-image-test")
	_ = editMW.WriteField("prompt", "animate this")
	fw, _ := editMW.CreateFormFile("image", "cat.png")
	_, _ = fw.Write([]byte("png-bytes"))
	_ = editMW.Close()
	editCtx, editW := newCtxPath("/v1/images/edits", editBuf.Bytes())
	editCtx.Request.Header.Set("Content-Type", editMW.FormDataContentType())
	editCtx.Set("request_id", "req-kling-img-edits")
	rig.svc.Handle(editCtx, rig.key)
	if editW.Code != http.StatusBadGateway {
		t.Fatalf("edits ask: status = %d, want 502 from the refused chain; body %s", editW.Code, editW.Body.String())
	}
	if hits := atomic.LoadInt32(&rig.queryHits); hits != 0 {
		t.Errorf("an edits ask must not be dispatched, task queries = %d", hits)
	}

	// A streaming ask under an ordinary name never reaches a candidate —
	// the door's own gpt-image-* gate refuses it as 400.
	c, w = imageRequest(`{"model":"kling-image-test","prompt":"a fox","stream":true}`)
	c.Set("request_id", "req-kling-img-stream")
	rig.svc.Handle(c, rig.key)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("stream ask: status = %d, want the door's 400", w.Code)
	}
	if hits := atomic.LoadInt32(&rig.queryHits); hits != 0 {
		t.Errorf("a stream ask must not be dispatched, task queries = %d", hits)
	}

	// The dialect's own streaming gate is the second line: a gpt-image*
	// name clears the door and must then be refused per kling candidate.
	streamPayload, srej := NewImageModality().Admit(context.Background(), Ingress{
		Path: "/v1/images/generations", ContentType: "application/json",
		Body: []byte(`{"model":"gpt-image-1","prompt":"a fox","stream":true}`),
	})
	if srej != nil {
		t.Fatalf("a gpt-image* stream ask clears the door: %+v", srej)
	}
	if v := streamPayload.Supports(Candidate{ProviderModelName: "kling-v3", BaseURL: rig.upstreamURL, EgressProtocol: protocols.ProtocolOpenAI}); v.OK || v.Reason != klingImageNoStreamReason {
		t.Fatalf("a gpt-image* stream ask on a kling base must hit the dialect's own gate, got %+v", v)
	}

	// A model off the endpoint list is refused the same way — the submit
	// body's model_name must never carry a name the endpoint would refuse.
	var cand model.ModelCandidate
	if err := rig.db.First(&cand, "model_id = ?", 1).Error; err != nil {
		t.Fatalf("load candidate: %v", err)
	}
	if err := rig.db.Model(&model.ModelCandidate{}).Where("id = ?", cand.ID).Update("provider_model_name", "kling-v1").Error; err != nil {
		t.Fatalf("retarget candidate: %v", err)
	}
	c, w = imageRequest(`{"model":"kling-image-test","prompt":"a fox"}`)
	c.Set("request_id", "req-kling-img-whitelist")
	rig.svc.Handle(c, rig.key)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("off-list model: status = %d, want 502 from the refused chain", w.Code)
	}
	if hits := atomic.LoadInt32(&rig.queryHits); hits != 0 {
		t.Errorf("an off-list model must not be dispatched, task queries = %d", hits)
	}
}

// A poll that dies mid-flight after an accepted task settles as an upstream
// error for the caller — never a failover, which would submit a second
// billable task for a request whose first is already rendering.
func TestKlingImagePollFailureDoesNotFailOver(t *testing.T) {
	rig := newKlingImageRig(t, "kling-v3")
	rig.taskAnswer.Store("0") // not a JSON object with data → the poll's parse fails → settled
	c, w := imageRequest(`{"model":"kling-image-test","prompt":"a fox"}`)
	c.Set("request_id", "req-kling-img-pollerr")
	rig.svc.Handle(c, rig.key)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("poll failure: status = %d, want 502 settled, body = %s", w.Code, w.Body.String())
	}
}

// The omni pair rides its own endpoint with the caller's kling-native
// fields beside the mapped ones, and a series delivery bills per image.
func TestKlingOmniImageEndToEnd(t *testing.T) {
	rig := newKlingImageRig(t, "kling-v3-omni")
	rig.taskAnswer.Store(`{"code":0,"data":{"task_status":"succeed","final_unit_deduction":"0.04",` +
		`"task_result":{"result_type":"series","images":[],"series_images":[{"index":0,"url":"https://x.test/s1"},{"index":1,"url":"https://x.test/s2"}]}}}`)

	c, w := imageRequest(`{"model":"kling-image-test","prompt":"merge <<<image_1>>>",` +
		`"image_list":[{"image":"https://a/1.png"}],"result_type":"series","series_amount":2,"size":"2k"}`)
	c.Set("request_id", "req-kling-omni-e2e")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rig.lastPath != "/v1/images/omni-image" {
		t.Fatalf("submit path = %q, want the omni endpoint", rig.lastPath)
	}
	if !strings.HasPrefix(rig.lastQueryURI, "/v1/images/omni-image/951") {
		t.Fatalf("poll route = %q, want the omni task route", rig.lastQueryURI)
	}
	var sent struct {
		ModelName    string          `json:"model_name"`
		Prompt       string          `json:"prompt"`
		Resolution   string          `json:"resolution"`
		ImageList    json.RawMessage `json:"image_list"`
		ResultType   string          `json:"result_type"`
		SeriesAmount json.RawMessage `json:"series_amount"`
	}
	if err := json.Unmarshal(rig.lastBody, &sent); err != nil {
		t.Fatalf("upstream body did not parse: %v (%s)", err, rig.lastBody)
	}
	if sent.ModelName != "kling-v3-omni" || sent.Resolution != "2k" || sent.ResultType != "series" {
		t.Fatalf("knobs wrong: %s", rig.lastBody)
	}
	if !strings.Contains(string(sent.ImageList), "https://a/1.png") {
		t.Fatalf("image_list must reach the upstream: %s", sent.ImageList)
	}
	if !strings.Contains(string(sent.SeriesAmount), "2") {
		t.Fatalf("series_amount must reach the upstream: %s", sent.SeriesAmount)
	}

	var received struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &received); err != nil {
		t.Fatalf("caller body did not parse: %v", err)
	}
	if len(received.Data) != 2 || received.Data[1].URL != "https://x.test/s2" {
		t.Fatalf("series images must join the delivered surface: %s", w.Body.String())
	}
	// A series of two bills as two delivered images.
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-kling-omni-e2e").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if !row.CostKnown || row.CostMicros != 40000 || row.ImageCount != 2 {
		t.Fatalf("cost = known:%v %d count=%d, want 2 × 0.02", row.CostKnown, row.CostMicros, row.ImageCount)
	}
}
