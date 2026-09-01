package gateway

// End-to-end tests for the image modality: a caller's OpenAI Images API
// request, routed through the real Handle path to a fake upstream, and the
// refusals the endpoint owes a caller before any upstream is walked. The
// assertions are on the wire and on the persisted rows, so each one goes red
// exactly when the behaviour it describes comes undone.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// setImageOutputModalities points a seeded model's declaration at a
// modality list, the way the admin API's update would.
func setImageOutputModalities(t *testing.T, db *gorm.DB, modelID uint, list string) {
	t.Helper()
	if err := db.Model(&model.Model{}).Where("id = ?", modelID).Update("output_modalities", list).Error; err != nil {
		t.Fatalf("set output modalities: %v", err)
	}
}

// imageRig is the dispatch fixture for image requests: a database, a fake
// upstream that records what it was asked, a provider pointed at it, an
// image-capable model with one candidate, and a caller key allowed to reach
// that model.
type imageRig struct {
	svc     *Service
	db      *gorm.DB
	key     *model.APIKey
	modelID uint
	hits    atomic.Int64
	// lastPath / lastAuth / lastBody record what the upstream saw, written
	// from the handler goroutine and read after Handle returns.
	lastPath string
	lastAuth string
	lastBody []byte
}

const imageUpstreamBody = `{"created":1700000000,"data":[{"url":"https://example.test/img.png"}],"usage":{"input_tokens":10,"output_tokens":1020,"total_tokens":1030}}`

func newImageRig(t *testing.T) *imageRig {
	t.Helper()
	rig := &imageRig{}
	rig.db = testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		rig.lastPath = r.URL.Path
		rig.lastAuth = r.Header.Get("Authorization")
		rig.lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageUpstreamBody))
	}))
	t.Cleanup(up.Close)
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "image-provider", up.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "image-model", "image-model-real", false, false, 1)
	setImageOutputModalities(t, rig.db, m.ID, `["image"]`)
	rig.modelID = m.ID
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	return rig
}

// imageRequest builds a caller context for the images endpoint the way the
// router middleware would.
func imageRequest(body string) (*gin.Context, *httptest.ResponseRecorder) {
	return newCtxPath("/v1/images/generations", []byte(body))
}

// A whole image request passes through the gateway to an OpenAI-compatible
// upstream and back: the endpoint path is the images API, the credential is
// the provider key, the model field is the candidate's provider name, and
// the caller receives the upstream's bytes verbatim. The seeded candidate
// bills in the token default, so this also pins the token-mode image
// settlement: the delivery's token sub-counts, priced at the candidate's
// token prices, and no per-image snapshot.
func TestImageGenerationPassthroughEndToEnd(t *testing.T) {
	rig := newImageRig(t)
	bodiesDir := t.TempDir()

	c, w := imageRequest(`{"model":"image-model","prompt":"a red fox","n":1,"quality":"high","size":"1024x1024"}`)
	c.Set("request_id", "req-image-e2e")
	c.Set(BodiesDirContextKey, bodiesDir)
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1", rig.hits.Load())
	}
	if rig.lastPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want the images endpoint", rig.lastPath)
	}
	if rig.lastAuth != "Bearer sk-image-up" {
		t.Errorf("upstream authorization = %q, want the provider key", rig.lastAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal(rig.lastBody, &sent); err != nil {
		t.Fatalf("upstream body did not parse: %v (%s)", err, rig.lastBody)
	}
	if sent["model"] != "image-model-real" {
		t.Errorf("upstream model = %v, want the candidate's provider name", sent["model"])
	}
	if sent["prompt"] != "a red fox" {
		t.Errorf("upstream prompt = %v, want the caller's prompt untouched", sent["prompt"])
	}
	if w.Body.String() != imageUpstreamBody {
		t.Errorf("caller body differs from the upstream's bytes")
	}

	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-e2e").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.StatusCode != http.StatusOK {
		t.Errorf("log status_code = %d, want 200", row.StatusCode)
	}
	// The candidate seeds with token prices and the default token billing
	// mode, so the delivery settles by its token sub-counts: 10 input at 1.0
	// plus 1020 output at 2.0, per million, in micros.
	if !row.CostKnown {
		t.Fatalf("cost_known = false, want the token-mode settlement to bill")
	}
	if row.CostMicros != 2050 {
		t.Errorf("cost_micros = %d, want 2050 (10*1.0 + 1020*2.0 per million)", row.CostMicros)
	}
	if row.ImagePricingSnapshot != "" {
		t.Errorf("image_pricing_snapshot = %q on a token-mode settlement, want empty", row.ImagePricingSnapshot)
	}
	var spent struct {
		BudgetSpentMicros int64
	}
	if err := rig.db.Table("api_keys").Select("budget_spent_micros").Where("id = ?", rig.key.ID).Scan(&spent).Error; err != nil {
		t.Fatalf("read key budget: %v", err)
	}
	if spent.BudgetSpentMicros != 2050 {
		t.Errorf("budget_spent_micros = %d, want 2050", spent.BudgetSpentMicros)
	}
	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-image-e2e").First(&bodyRow).Error; err != nil {
		t.Fatalf("no body row: %v", err)
	}
	if bodyRow.ResponseBody != imageUpstreamBody {
		t.Errorf("body row response differs from what the caller received")
	}
}

// A streaming ask is refused at the door, before any upstream is walked:
// the caller learns the endpoint does not stream rather than hanging on a
// stream that never frames.
func TestImageStreamingIsRefusedBeforeUpstream(t *testing.T) {
	rig := newImageRig(t)

	c, w := imageRequest(`{"model":"image-model","prompt":"a red fox","stream":true}`)
	c.Set("request_id", "req-image-stream")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-stream").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.FailReason == nil || *row.FailReason != "image_streaming_unsupported" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("fail_reason = %q, want image_streaming_unsupported", got)
	}
}

// A model that declares only text is not in the image pool: the images
// endpoint refuses it by name, before any candidate is walked.
func TestImagesEndpointRefusesTextOnlyModel(t *testing.T) {
	rig := newImageRig(t)
	setImageOutputModalities(t, rig.db, rig.modelID, `["text"]`)

	c, w := imageRequest(`{"model":"image-model","prompt":"a red fox"}`)
	c.Set("request_id", "req-image-textmodel")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-textmodel").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.FailReason == nil || *row.FailReason != "model_modality_mismatch" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("fail_reason = %q, want model_modality_mismatch", got)
	}
}

// The isolation runs both ways: a model that declares only image is not in
// the chat pool either, so an image model cannot be driven through the chat
// endpoint where its responses would be nonsense.
func TestChatEndpointRefusesImageOnlyModel(t *testing.T) {
	rig := newImageRig(t)

	c, w := newCtx([]byte(`{"model":"image-model","messages":[{"role":"user","content":"hi"}]}`))
	c.Set("request_id", "req-chat-imagemodel")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-chat-imagemodel").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.FailReason == nil || *row.FailReason != "model_modality_mismatch" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("fail_reason = %q, want model_modality_mismatch", got)
	}
}

// A request without a model or without a prompt is refused at admission —
// both are fields no candidate could supply.
func TestImageAdmitRequiresModelAndPrompt(t *testing.T) {
	rig := newImageRig(t)
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"no model", `{"prompt":"a red fox"}`, "empty_model"},
		{"no prompt", `{"model":"image-model"}`, "empty_prompt"},
		{"not json", `not-json`, "parse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := imageRequest(tc.body)
			rig.svc.Handle(c, rig.key)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if rig.hits.Load() != 0 {
				t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
			}
		})
	}
}

// The images protocol resolves to the image modality, and the image
// modality declares the media budget. Both are registration facts: the
// first is what routes the request at all, the second is what caps it.
func TestImagesProtocolRegisteredAndBudgetDeclared(t *testing.T) {
	m, ok := modalityFor(protocols.ProtocolImages)
	if !ok {
		t.Fatal("no modality registered for the images protocol")
	}
	if m.ID() != ModalityImage {
		t.Fatalf("modality id = %q, want %q", m.ID(), ModalityImage)
	}
	if got := m.Limits().TotalBudget; got != imageRequestBudget {
		t.Fatalf("declared total budget = %v, want %v", got, imageRequestBudget)
	}
	if got := IngressProtocol("/v1/images/generations"); got != protocols.ProtocolImages {
		t.Fatalf("IngressProtocol = %q, want images", got)
	}
}

// The declared media budget caps the request for real: an admitted image
// request's deadline is narrowed to the budget, well under the kernel's own
// 30-minute request timeout. Asserted on the exchange the handle hook
// captures, so the wiring — not the declaration alone — is what's pinned.
func TestImageRequestDeadlineNarrowsToMediaBudget(t *testing.T) {
	rig := newImageRig(t)
	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	t.Cleanup(func() { testHookHandleDone = nil })

	before := time.Now()
	c, w := imageRequest(`{"model":"image-model","prompt":"a red fox"}`)
	c.Set("request_id", "req-image-budget")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("test hook never ran")
	}
	deadline := captured.requestDeadline
	if deadline.After(before.Add(imageRequestBudget + 5*time.Second)) {
		t.Errorf("request deadline %v is not narrowed: it outruns the %v media budget", deadline.Sub(before), imageRequestBudget)
	}
}

// A b64_json answer is audited with the image payload redacted: the debug
// row keeps every field and the fact of the image, but not megabytes of
// base64 — the audit table diagnoses protocol bugs, it is not an image
// store. The caller, meanwhile, still receives the full payload verbatim.
func TestImageB64ResponseIsAuditedRedacted(t *testing.T) {
	b64 := strings.Repeat("QUJD", 2048) // 8192 chars of base64
	b64Body := `{"created":1700000000,"data":[{"b64_json":"` + b64 + `"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
	db := testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(b64Body))
	}))
	t.Cleanup(up.Close)
	svc := newSvc(t, db)
	p := createProvider(t, db, "image-provider", up.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
	m := createModelAndCandidate(t, db, p, "image-model", "image-model-real", false, false, 1)
	setImageOutputModalities(t, db, m.ID, `["image"]`)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := imageRequest(`{"model":"image-model","prompt":"a fox","response_format":"b64_json"}`)
	c.Set("request_id", "req-image-b64")
	svc.Handle(c, key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), b64) {
		t.Fatal("the caller did not receive the full base64 payload")
	}
	var row model.RequestLogBody
	if err := db.Where("request_id = ?", "req-image-b64").First(&row).Error; err != nil {
		t.Fatalf("no body row: %v", err)
	}
	wantNote := "[base64 image omitted: " + strconv.Itoa(len(b64)) + " chars]"
	for name, stored := range map[string]string{
		"response_body":          row.ResponseBody,
		"upstream_response_body": row.UpstreamResponseBody,
	} {
		if strings.Contains(stored, b64) {
			t.Errorf("%s stored the full base64 payload (%d bytes)", name, len(stored))
		}
		// The redacted row must still parse: it is a diagnostic an operator
		// diffs and pretty-prints, and a body no JSON parser accepts hides
		// the very bug it was kept to explain.
		var asJSON map[string]any
		if err := json.Unmarshal([]byte(stored), &asJSON); err != nil {
			t.Fatalf("%s is not valid JSON after redaction: %v (%.160s)", name, err, stored)
		}
		data, _ := asJSON["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("%s data array lost entries: %.160s", name, stored)
		}
		if got := data[0].(map[string]any)["b64_json"]; got != wantNote {
			t.Errorf("%s redaction note = %v, want %q (key preserved, exact length)", name, got, wantNote)
		}
	}
	// The request bodies stay raw: they are the caller's own small JSON.
	if !strings.Contains(row.RequestBody, "a fox") {
		t.Errorf("request_body was rendered instead of kept raw: %.120s", row.RequestBody)
	}
}
