package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// snapshotUpstream returns a fake upstream that answers every chat completion
// with the given JSON body.
func snapshotUpstream(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

// TestPriceSnapshotPersistedOnSettlement: a priced settlement writes the four
// unit prices it billed with onto the request_logs row, so the row can be
// re-priced by hand after the candidate's prices change.
func TestPriceSnapshotPersistedOnSettlement(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := snapshotUpstream(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`)
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-upstream-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).
		Updates(map[string]any{
			"input_price": 3.0, "output_price": 6.0,
			"cache_read_price": 0.3, "cache_write_price": 3.75,
		}).Error; err != nil {
		t.Fatalf("set candidate prices: %v", err)
	}
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if !row.CostKnown {
		t.Fatal("cost_known = false, want true (usage and prices were present)")
	}
	assertPrice(t, "settled_input_price", row.SettledInputPrice, 3.0)
	assertPrice(t, "settled_output_price", row.SettledOutputPrice, 6.0)
	assertPrice(t, "settled_cache_write_price", row.SettledCacheWritePrice, 3.75)
	assertPrice(t, "settled_cache_read_price", row.SettledCacheReadPrice, 0.3)
}

// TestPriceSnapshotRecordsEffectiveCachePrices: a candidate without configured
// cache prices bills cache tokens at the input price, and the snapshot records
// THAT — the price actually billed — not the absent configuration. A snapshot
// of nil here would make an old row unre-priceable for exactly the candidates
// where the fallback did the pricing.
func TestPriceSnapshotRecordsEffectiveCachePrices(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := snapshotUpstream(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`)
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-upstream-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	// createModelAndCandidate seeds input 1.0 / output 2.0, no cache prices.
	assertPrice(t, "settled_input_price", row.SettledInputPrice, 1.0)
	assertPrice(t, "settled_output_price", row.SettledOutputPrice, 2.0)
	assertPrice(t, "settled_cache_write_price", row.SettledCacheWritePrice, 1.0)
	assertPrice(t, "settled_cache_read_price", row.SettledCacheReadPrice, 1.0)
}

// TestPriceSnapshotAbsentWhenCostUnknown: a settlement that could not be
// priced (upstream response carried no usage) must leave all four snapshot
// columns NULL — a pseudo-snapshot on an unpriced row would claim the row can
// be re-priced when the cost it would reproduce never existed.
func TestPriceSnapshotAbsentWhenCostUnknown(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := snapshotUpstream(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-upstream-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if row.CostKnown {
		t.Fatal("cost_known = true, want false (upstream sent no usage)")
	}
	for name, got := range map[string]*float64{
		"settled_input_price":       row.SettledInputPrice,
		"settled_output_price":      row.SettledOutputPrice,
		"settled_cache_write_price": row.SettledCacheWritePrice,
		"settled_cache_read_price":  row.SettledCacheReadPrice,
	} {
		if got != nil {
			t.Errorf("%s = %v, want NULL on an unpriced row", name, *got)
		}
	}
}

func assertPrice(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = NULL, want %v", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s = %v, want %v", name, *got, want)
	}
}

// TestPriceSnapshotComesFromTheAttemptThatPriced: on a failover chain, the
// snapshot must carry the prices of the candidate that actually settled the
// cost — the second attempt here — never the first candidate that failed.
func TestPriceSnapshotComesFromTheAttemptThatPriced(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failing.Close()
	upstream := snapshotUpstream(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`)
	defer upstream.Close()

	svc := newSvc(t, db)
	now := time.Now().UTC()
	m := &model.Model{Name: "gpt-4o", ManagementStatus: model.ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	seedSnapshotCandidate := func(name, baseURL string, order int, input, output float64) {
		p := createProvider(t, db, name, baseURL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-"+name, "k1", 1, true)
		if err := db.Create(&model.ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: name + "-model",
			InputPrice: input, OutputPrice: output, MaxOutput: 4096,
			SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
			ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: order,
			VerificationStatus: model.ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %s: %v", name, err)
		}
	}
	seedSnapshotCandidate("p-fails", failing.URL, 1, 100.0, 200.0)
	seedSnapshotCandidate("p-serves", upstream.URL, 2, 3.0, 6.0)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (second candidate serves); body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if row.Attempts < 2 {
		t.Fatalf("attempts = %d, want >= 2 (the first candidate must have been tried)", row.Attempts)
	}
	assertPrice(t, "settled_input_price", row.SettledInputPrice, 3.0)
	assertPrice(t, "settled_output_price", row.SettledOutputPrice, 6.0)
}
