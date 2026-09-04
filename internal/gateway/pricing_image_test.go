package gateway

// Per-image settlement tests. The rig is the image e2e rig with the
// candidate's billing mode and tier table pointed where each case needs;
// the assertions are on the persisted row (cost, snapshot) and the key's
// budget — the places a bill actually lands.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// billingRig is an image rig whose candidate bills per image through a
// caller-chosen tier table, and whose upstream answers with a
// caller-chosen data array.
type billingRig struct {
	svc     *Service
	db      *gorm.DB
	key     *model.APIKey
	modelID uint
}

// newBillingRig sets up a per-image-billed model. tiersJSON may be "" to
// leave the table unset; dataJSON replaces the upstream's response body.
func newBillingRig(t *testing.T, tiersJSON, dataJSON string) *billingRig {
	t.Helper()
	rig := &billingRig{}
	rig.db = testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dataJSON))
	}))
	t.Cleanup(up.Close)
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "image-provider", up.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "image-model", "image-model-real", false, false, 1)
	setOutputModalities(t, rig.db, m.ID, `["image"]`)
	rig.modelID = m.ID
	updates := map[string]interface{}{"billing_mode": model.BillingModeImage}
	if tiersJSON != "" {
		updates["image_pricing_tiers"] = tiersJSON
	}
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Updates(updates).Error; err != nil {
		t.Fatalf("set billing mode: %v", err)
	}
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	return rig
}

func (r *billingRig) generate(t *testing.T, requestID, quality, size string, n int) (int, model.RequestLog) {
	t.Helper()
	body := `{"model":"image-model","prompt":"a fox","quality":"` + quality + `","size":"` + size + `"`
	if n > 0 {
		body += `,"n":` + string(rune('0'+n))
	}
	body += `}`
	c, w := imageRequest(body)
	c.Set("request_id", requestID)
	r.svc.Handle(c, r.key)
	var row model.RequestLog
	if err := r.db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		t.Fatalf("no request log row for %s: %v", requestID, err)
	}
	return w.Code, row
}

func (r *billingRig) budget(t *testing.T) int64 {
	t.Helper()
	var spent struct {
		BudgetSpentMicros int64
	}
	if err := r.db.Table("api_keys").Select("budget_spent_micros").Where("id = ?", r.key.ID).Scan(&spent).Error; err != nil {
		t.Fatalf("read key budget: %v", err)
	}
	return spent.BudgetSpentMicros
}

const twoImageResponse = `{"created":1700000000,"data":[{"url":"https://x.test/1.png"},{"url":"https://x.test/2.png"}]}`

// A tier that matches the request's quality and size prices each delivered
// image: asked for four, delivered two, billed two.
func TestImageBillsPerDeliveredImageThroughMatchingTier(t *testing.T) {
	tiers := `{"mode":"per_image","tiers":[{"quality":"high","size":"1024x1024","price":0.04}],"default_price":0.01}`
	rig := newBillingRig(t, tiers, twoImageResponse)

	code, row := rig.generate(t, "req-img-tier", "high", "1024x1024", 4)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !row.CostKnown {
		t.Fatal("cost_known = false, want the matched tier to bill")
	}
	if row.CostMicros != 80000 { // 0.04 × 2 images, in micros
		t.Errorf("cost_micros = %d, want 80000 (0.04 × 2 delivered)", row.CostMicros)
	}
	if got := rig.budget(t); got != 80000 {
		t.Errorf("budget_spent = %d, want 80000", got)
	}

	var snap struct {
		BillingMode    string  `json:"billing_mode"`
		RequestQuality string  `json:"request_quality"`
		RequestSize    string  `json:"request_size"`
		RequestN       int     `json:"request_n"`
		ActualN        int     `json:"actual_n"`
		UnitPrice      float64 `json:"unit_price"`
		PriceSource    string  `json:"price_source"`
	}
	if err := json.Unmarshal([]byte(row.ImagePricingSnapshot), &snap); err != nil {
		t.Fatalf("snapshot did not parse: %v (%s)", err, row.ImagePricingSnapshot)
	}
	if snap.BillingMode != "image" || snap.RequestQuality != "high" || snap.RequestSize != "1024x1024" {
		t.Errorf("snapshot axes = %+v, want high/1024x1024 image-mode", snap)
	}
	if snap.RequestN != 4 || snap.ActualN != 2 {
		t.Errorf("snapshot counts = asked %d delivered %d, want 4 and 2", snap.RequestN, snap.ActualN)
	}
	if snap.UnitPrice != 0.04 || snap.PriceSource != "tier" {
		t.Errorf("snapshot price = %v via %q, want 0.04 via tier", snap.UnitPrice, snap.PriceSource)
	}
}

// A request no tier matches prices from the default; a table with neither
// match nor default is unpriced (cost unknown), never free.
func TestImageUnmatchedRequestPricesFromDefaultOrNotAtAll(t *testing.T) {
	t.Run("default prices", func(t *testing.T) {
		rig := newBillingRig(t, `{"mode":"per_image","default_price":0.02}`, twoImageResponse)
		_, row := rig.generate(t, "req-img-default", "low", "512x512", 0)
		if !row.CostKnown || row.CostMicros != 40000 {
			t.Fatalf("cost = known:%v %d, want known 40000 (0.02 × 2)", row.CostKnown, row.CostMicros)
		}
	})

	t.Run("no match and no default is unknown, not free", func(t *testing.T) {
		rig := newBillingRig(t, `{"mode":"per_image","tiers":[{"quality":"high","price":0.04}]}`, twoImageResponse)
		_, row := rig.generate(t, "req-img-unpriced", "medium", "", 0)
		if row.CostKnown {
			t.Fatalf("cost_known = true with no matching tier and no default: unpriced must not read as free")
		}
		if got := rig.budget(t); got != 0 {
			t.Errorf("budget_spent = %d, want 0", got)
		}
	})
}

// A zero-price tier is a free image, which is a legal declaration and a
// KNOWN zero — different from unpriced.
func TestImageZeroPriceTierBillsKnownZero(t *testing.T) {
	rig := newBillingRig(t, `{"mode":"per_image","tiers":[{"quality":"free","price":0}]}`, twoImageResponse)
	_, row := rig.generate(t, "req-img-free", "free", "", 0)
	if !row.CostKnown || row.CostMicros != 0 {
		t.Fatalf("cost = known:%v %d, want known 0 (free is not unknown)", row.CostKnown, row.CostMicros)
	}
}

// A failing upstream bills nothing: the chain exhausts, the row records the
// failure, the key's budget is untouched.
func TestImageFailedUpstreamBillsNothing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"no render for you"}}`))
	}))
	t.Cleanup(up.Close)
	svc := newSvc(t, db)
	p := createProvider(t, db, "image-provider", up.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
	m := createModelAndCandidate(t, db, p, "image-model", "image-model-real", false, false, 1)
	setOutputModalities(t, db, m.ID, `["image"]`)
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Updates(map[string]interface{}{
		"billing_mode":        model.BillingModeImage,
		"image_pricing_tiers": `{"mode":"per_image","default_price":0.04}`,
	}).Error; err != nil {
		t.Fatalf("seed billing: %v", err)
	}
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := imageRequest(`{"model":"image-model","prompt":"a fox"}`)
	c.Set("request_id", "req-img-fail")
	svc.Handle(c, key)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from an exhausted chain", w.Code)
	}
	var row model.RequestLog
	if err := db.Where("request_id = ?", "req-img-fail").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a failed request")
	}
	var spent struct {
		BudgetSpentMicros int64
	}
	if err := db.Table("api_keys").Select("budget_spent_micros").Where("id = ?", key.ID).Scan(&spent).Error; err != nil {
		t.Fatalf("read budget: %v", err)
	}
	if spent.BudgetSpentMicros != 0 {
		t.Errorf("budget_spent = %d, want 0", spent.BudgetSpentMicros)
	}
}

// An OK answer whose data array is empty is not a delivery: the chain hands
// the failure on and the request ends failed, unbilled.
func TestImageEmptyDataIsNotADelivery(t *testing.T) {
	rig := newBillingRig(t, `{"mode":"per_image","default_price":0.04}`, `{"created":1700000000,"data":[]}`)

	code, row := rig.generate(t, "req-img-empty", "", "", 0)
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: an empty answer exhausted the chain", code)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true for an empty answer")
	}
	if got := rig.budget(t); got != 0 {
		t.Errorf("budget_spent = %d, want 0", got)
	}
}
