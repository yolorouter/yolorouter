package gateway

// End-to-end tests for the image modality: a caller's OpenAI Images API
// request, routed through the real Handle path to a fake upstream, and the
// refusals the endpoint owes a caller before any upstream is walked. The
// assertions are on the wire and on the persisted rows, so each one goes red
// exactly when the behaviour it describes comes undone.

import (
	"encoding/base64"
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

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// setOutputModalities points a seeded model's declaration at a
// modality list, the way the admin API's update would.
func setOutputModalities(t *testing.T, db *gorm.DB, modelID uint, list string) {
	t.Helper()
	if err := db.Model(&Model{}).Where("id = ?", modelID).Update("output_modalities", list).Error; err != nil {
		t.Fatalf("set output modalities: %v", err)
	}
}

// imageRig is the dispatch fixture for image requests: a database, a fake
// upstream that records what it was asked, a provider pointed at it, an
// image-capable model with one candidate, and a caller key allowed to reach
// that model.
type imageRig struct {
	svc      *Service
	db       *gorm.DB
	key      *APIKey
	modelID  uint
	provider *Provider
	hits     atomic.Int64
	// lastPath / lastAuth / lastBody record what the upstream saw, written
	// from the handler goroutine and read after Handle returns.
	lastPath string
	lastAuth string
	lastBody []byte
}

const imageUpstreamBody = `{"created":1700000000,"data":[{"url":"https://example.test/img.png"}],"usage":{"input_tokens":10,"output_tokens":1020,"total_tokens":1030}}`

func newImageRig(t *testing.T) *imageRig {
	t.Helper()
	return newImageRigWith(t, nil)
}

// newImageRigWith builds the fixture with a caller-chosen upstream answer;
// nil means the default OpenAI-shaped images JSON. The recording wrapper
// runs for every answer, so a test's handler only writes its response.
func newImageRigWith(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *imageRig {
	t.Helper()
	rig := &imageRig{}
	rig.db = testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		rig.lastPath = r.URL.Path
		rig.lastAuth = r.Header.Get("Authorization")
		rig.lastBody, _ = io.ReadAll(r.Body)
		if answer != nil {
			answer(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageUpstreamBody))
	}))
	t.Cleanup(up.Close)
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "image-provider", up.URL)
	rig.provider = p
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "image-model", "image-model-real", false, false, 1)
	setOutputModalities(t, rig.db, m.ID, `["image"]`)
	rig.modelID = m.ID
	rig.key = createAPIKey(t, rig.db, APIKeyStatusActive, []uint{m.ID})
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

// A streaming ask for a model outside the streaming family is refused at
// the door, before any upstream is walked: the caller learns which models
// stream rather than hanging on a stream that never frames.
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
	if row.FailReason == nil || *row.FailReason != "image_streaming_model_unsupported" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("fail_reason = %q, want image_streaming_model_unsupported", got)
	}
}

// A model that declares only text is not in the image pool: the images
// endpoint refuses it by name, before any candidate is walked.
func TestImagesEndpointRefusesTextOnlyModel(t *testing.T) {
	rig := newImageRig(t)
	setOutputModalities(t, rig.db, rig.modelID, `["text"]`)

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
	setOutputModalities(t, db, m.ID, `["image"]`)
	key := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

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

// A small upload redacts as surely as a large one: the request-side data-URI
// redactor has no length floor, because a caller's image can honestly be a
// few hundred bytes and a data URI in a request this gateway encoded is an
// image payload by construction.
func TestImageRequestRedactionHasNoLengthFloor(t *testing.T) {
	tiny := base64.StdEncoding.EncodeToString([]byte("\x89PNG small"))
	body := []byte(`{"model":"m","input":{"messages":[{"role":"user","content":[{"image":"data:image/png;base64,` + tiny + `"},{"text":"p"}]}]}}`)
	out := redactImageRequest(body)
	if strings.Contains(out, tiny) || strings.Contains(out, "data:image/") {
		t.Errorf("small data URI survived the request redactor: %s", out)
	}
	if !strings.Contains(out, "[base64 image omitted:") {
		t.Errorf("redaction note missing: %s", out)
	}
}

// estimatePV builds a priced basis around a serialized tier table, the way
// the relay's pricing view carries it.
func estimatePV(tiers string) PricingView {
	return PricingView{ImageTiers: rows.ParseImagePricingTiers(tiers)}
}

// The image estimate prices the ask: the table resolved against the
// request's own axes, floored at one image, with the table's default
// pricing an unmatched pair.
func TestImageEstimateCostMatrix(t *testing.T) {
	// Ordered the way a real table is: specific tiers ahead of the plain
	// one, because resolution is first-match-wins and a plainer tier first
	// would shadow the specific one. The ordering is part of the contract,
	// not an accident here.
	const table = `{"mode":"per_image","tiers":[
		{"quality":"high","size":"1024x1024","price":0.20},
		{"size":"1024x1024","price":0.11},
		{"size":"","price":0.05}],
	"default_price":0.02}`
	leg := func(name string, got CostEstimate, wantMicros int64, wantKnown bool) {
		t.Helper()
		want := CostEstimate{Known: wantKnown, Micros: wantMicros, Unit: fact.UnitImage}
		if got != want {
			t.Errorf("%s: got %+v, want %+v", name, got, want)
		}
	}

	p := &imagePayload{req: &images.Request{Model: "m", Size: "1024x1024"}}
	leg("explicit tier, n floored to one",
		p.EstimateCost(estimatePV(table)), 110_000, true)

	p = &imagePayload{req: &images.Request{Model: "m", Size: "1024x1024", N: 3}}
	leg("explicit tier × n",
		p.EstimateCost(estimatePV(table)), 330_000, true)

	p = &imagePayload{req: &images.Request{Model: "m", Quality: "high", Size: "1024x1024"}}
	leg("specific tier ahead of the plain one",
		p.EstimateCost(estimatePV(table)), 200_000, true)

	p = &imagePayload{req: &images.Request{Model: "m", Size: "2048x2048"}}
	leg("empty-size tier is the wildcard",
		p.EstimateCost(estimatePV(table)), 50_000, true)

	// "*" is a literal size, not a wildcard — only an empty axis is.
	const starTable = `{"mode":"per_image","tiers":[{"size":"*","price":0.05}],"default_price":0.02}`
	p = &imagePayload{req: &images.Request{Model: "m", Size: "2048x2048"}}
	leg("star tier is literal → default prices",
		p.EstimateCost(estimatePV(starTable)), 20_000, true)

	// No wildcard, no default: unpriced, not free.
	const noDefault = `{"mode":"per_image","tiers":[{"size":"1024x1024","price":0.11}]}`
	p = &imagePayload{req: &images.Request{Model: "m", Size: "2048x2048"}}
	leg("no match, no default → unknown",
		p.EstimateCost(estimatePV(noDefault)), 0, false)

	// No table at all (token-priced image model, or tiers that failed to
	// parse): the token rates are not this modality's vocabulary.
	p = &imagePayload{req: &images.Request{Model: "m"}}
	leg("no table → unknown",
		p.EstimateCost(PricingView{InputPricePerMillion: 3}), 0, false)

	// The edits half carries its own axes and prices on the same terms.
	p = &imagePayload{edit: &images.EditRequest{Model: "m", N: 2, Size: "1024x1024"}}
	leg("edit axes × n",
		p.EstimateCost(estimatePV(noDefault)), 220_000, true)

	// Tiers a parser cannot read are no table: unknown, not free.
	p = &imagePayload{req: &images.Request{Model: "m", Size: "1024x1024"}}
	leg("malformed table → unknown",
		p.EstimateCost(estimatePV(`{"tiers":[`)), 0, false)
}
