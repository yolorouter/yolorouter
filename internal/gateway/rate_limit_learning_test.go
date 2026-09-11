package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// benchRemainder is the test-side view of how long a key's standing bench
// still runs, measured on the pool's own clock (tests inject a fake one).
func benchRemainder(p *keyPool, keyID uint) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.states[keyID]
	if !ok || !s.benched() {
		return 0
	}
	return s.benchUntil.Sub(p.now())
}

// keyIDForLabel resolves the row id of a provider key by label, for
// asserting pool state built during a relay.
func keyIDForLabel(t *testing.T, db *gorm.DB, providerID uint, label string) uint {
	t.Helper()
	var key model.ProviderKey
	if err := db.Where("provider_id = ? AND label = ?", providerID, label).First(&key).Error; err != nil {
		t.Fatalf("load key %q: %v", label, err)
	}
	return key.ID
}

// A 429 whose headers state the limits leaves a learned row per meter,
// with the numbers the header carried.
func TestRelayLearnsObservedRateLimitsFromHeaders(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-Ratelimit-Limit-Requests", "60")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		w.Header().Set("X-Ratelimit-Reset-Requests", "6ms")
		w.Header().Set("X-Ratelimit-Limit-Tokens", "100000")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	rows, err := repository.ListObservedRateLimits(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("learned rows = %d, want 2 (requests + tokens)", len(rows))
	}
	byMeter := map[string]repository.ObservedRateLimitView{}
	for _, row := range rows {
		byMeter[row.Meter] = row
	}
	req := byMeter["requests"]
	if req.LimitValue == nil || *req.LimitValue != 60 {
		t.Fatalf("requests limit = %v, want 60", req.LimitValue)
	}
	if req.LastRemaining == nil || *req.LastRemaining != 0 {
		t.Fatalf("requests remaining = %v, want 0 (a real zero, not a NULL)", req.LastRemaining)
	}
	if req.KeyLabel != "k1" || req.ProviderName != "p1" {
		t.Fatalf("joined names = %q/%q, want k1/p1", req.KeyLabel, req.ProviderName)
	}
	if tok := byMeter["tokens"]; tok.LimitValue == nil || *tok.LimitValue != 100000 {
		t.Fatalf("tokens limit = %v, want 100000", tok.LimitValue)
	}
}

// A reset header that names a long window benches the key for that
// window, not the ten-minute Retry-After ceiling.
func TestRelayBenchesLongWindowFromResetHeader(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("X-Ratelimit-Reset-Requests", "1h")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	keyID := keyIDForLabel(t, db, p.ID, "k1")
	if rem := benchRemainder(svc.keyPool, keyID); rem < 50*time.Minute {
		t.Fatalf("bench remainder = %v, want ~1h from the reset header", rem)
	}
}

// Gemini's body dialect: the PerDay window lands as a learned row (the
// requests meter, window only) and a structured retryDelay beyond the
// Retry-After ceiling lengthens the header-stage bench.
func TestRelayLearnsGeminiWindowAndLengthensBench(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"Quota exceeded","status":"RESOURCE_EXHAUSTED","details":[`+
			`{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaMetric":"x/GenerateRequestsPerDayPerProject"}]},`+
			`{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"36000s"}]}}`)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	rows, err := repository.ListObservedRateLimits(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Meter != "requests" {
		t.Fatalf("learned rows = %+v, want one requests row", rows)
	}
	if rows[0].WindowSeconds == nil || *rows[0].WindowSeconds != 86400 {
		t.Fatalf("window = %v, want 86400", rows[0].WindowSeconds)
	}

	keyID := keyIDForLabel(t, db, p.ID, "k1")
	if rem := benchRemainder(svc.keyPool, keyID); rem < 9*time.Hour {
		t.Fatalf("bench remainder = %v, want ~10h from the structured retryDelay", rem)
	}
}

// The quota-exhausted path owns the key: its 429 still teaches the
// limits, but the key is not left benched — it left rotation for retest.
func TestRelayQuotaExhaustedTeachesWithoutBench(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-Ratelimit-Limit-Requests", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, quotaExhausted429Body)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-a", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", false, false, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Code)
	}

	rows, err := repository.ListObservedRateLimits(db)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].LimitValue == nil || *rows[0].LimitValue != 60 {
		t.Fatalf("learned rows = %+v, want the requests limit 60", rows)
	}
	keyID := keyIDForLabel(t, db, p.ID, "k1")
	if rem := benchRemainder(svc.keyPool, keyID); rem != 0 {
		t.Fatalf("bench remainder = %v, want 0 (invalidated key must not stay benched)", rem)
	}
}

// Unit-level: lengthenKeyBench only extends the verdict that booked the
// bench, only forward, and only while a bench stands.
func TestLengthenKeyBenchRules(t *testing.T) {
	base := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return base }
	p := newKeyPool(now)
	dispatched := base.Add(-time.Second)

	p.coolKey(1, 1, dispatched, 10*time.Minute)
	p.lengthenKeyBench(1, 1, dispatched, time.Hour)
	if rem := benchRemainder(p, 1); rem < 50*time.Minute {
		t.Fatalf("after lengthen = %v, want ~1h", rem)
	}

	// Shorter is refused: more evidence never shortens a bench.
	p.lengthenKeyBench(1, 1, dispatched, 5*time.Minute)
	if rem := benchRemainder(p, 1); rem < 50*time.Minute {
		t.Fatalf("shorter lengthen changed bench to %v", rem)
	}

	// An older verdict's straggler evidence is refused, like coolKey.
	p.coolKey(2, 1, dispatched, time.Minute)
	p.lengthenKeyBench(2, 1, dispatched.Add(-time.Minute), time.Hour)
	if rem := benchRemainder(p, 2); rem > time.Minute {
		t.Fatalf("older verdict lengthened to %v, want refused", rem)
	}

	// Without a standing bench there is nothing to lengthen — the quota
	// path's dropKey must be final.
	p.dropKey(3, 1, base)
	p.lengthenKeyBench(3, 1, base.Add(-time.Second), time.Hour)
	if rem := benchRemainder(p, 3); rem != 0 {
		t.Fatalf("lengthen revived a dropped key's bench to %v", rem)
	}
}
