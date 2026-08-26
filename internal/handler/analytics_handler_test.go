// Tests for the analytics endpoints, driven through the real router — the
// production route table with its middleware chain (session auth, member
// scoping, timezone). An external test package so importing the router does
// not cycle through this package's production side. Exercises the full
// HTTP → middleware → service → repository stack against a migrated SQLite
// DB.
package handler_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/router"
	"github.com/yolorouter/yolorouter/internal/service/analytics"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// Local names for the shared testutil helpers, so the test bodies below
// read the same as the rest of the handler tests.
func doJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}, cookie *http.Cookie) (*httptest.ResponseRecorder, testutil.Envelope) {
	t.Helper()
	return testutil.DoJSON(t, r, method, path, body, cookie)
}

func seedRequestLog(t *testing.T, db *gorm.DB, requestID string, ts time.Time, mut func(*model.RequestLog)) {
	t.Helper()
	testutil.SeedRequestLog(t, db, requestID, ts, mut)
}

func seedAPIKey(t *testing.T, db *gorm.DB, owner string) (uint, uint) {
	t.Helper()
	return testutil.SeedAPIKey(t, db, owner)
}

func seedProvider(t *testing.T, db *gorm.DB, name string) uint {
	t.Helper()
	return testutil.SeedProvider(t, db, name)
}

// newAnalyticsFixture builds the real router and an admin session to drive
// it with. Every request in this file goes through the production route
// table and middleware chain — auth, member scoping, timezone — so these
// tests pin that the analytics routes exist on the real table and admit an
// admin session. Which permission GROUP each route sits on is pinned
// separately, by the router package's group-split test, since everything
// here authenticates as an admin.
func newAnalyticsFixture(t *testing.T) (*gin.Engine, *gorm.DB, *http.Cookie) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	r, err := router.New(router.Deps{
		DB:                db,
		ProviderMasterKey: testutil.ProviderMasterKey(),
		BodiesDir:         t.TempDir(),
		Update:            config.UpdateConfig{Enabled: true},
		Gateway:           config.DefaultGatewayConfig(),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	now := time.Now().UTC()
	admin := &model.User{Username: "analytics-admin", Role: model.RoleAdmin,
		Status: model.UserStatusEnabled, IsLocal: true, PasswordHash: "hash",
		CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, admin); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := repository.CreateSession(db, "tok-analytics-admin", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return r, db, &http.Cookie{Name: "session_id", Value: "tok-analytics-admin"}
}

// analyticsStrPtr is a tiny *string helper local to this file (the existing
// seedRequestLog's mutator-callback pattern keeps the test body explicit at
// the cost of needing a closure-captured pointer for fail_reason).
func analyticsStrPtr(s string) *string { return &s }

// === Overview handler ====================================================

func TestGetAnalyticsOverviewAggregatesSeededRows(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	// 2 successes (cost-known), 1 server failure (cost-unknown),
	// 1 caller-cancel (cost-unknown). Verify each metric below.
	seedRequestLog(t, db, "r1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 100
		r.OutputTokens = 50
		r.CacheWriteTokens = 30
		r.CacheReadTokens = 40
		r.CostMicros = 10
		r.CostKnown = true
		r.DurationMs = 500
	})
	seedRequestLog(t, db, "r2", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 200
		r.OutputTokens = 100
		r.CacheReadTokens = 60
		r.CostMicros = 20
		r.CostKnown = true
		r.DurationMs = 600
	})
	seedRequestLog(t, db, "r3", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 500
		r.FailReason = analyticsStrPtr("upstream")
		// Explicitly cost-unknown with zero tokens — seedRequestLog's
		// defaults (InputTokens=10, OutputTokens=20, CostKnown=true) would
		// otherwise skew the aggregate.
		r.InputTokens = 0
		r.OutputTokens = 0
		r.CostMicros = 0
		r.CostKnown = false
		r.DurationMs = 100
	})
	seedRequestLog(t, db, "r4", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 499
		r.InputTokens = 0
		r.OutputTokens = 0
		r.CostMicros = 0
		r.CostKnown = false
		r.DurationMs = 50
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int                   `json:"code"`
		Data analytics.OverviewRow `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data := env.Data
	if data.TotalCalls != 4 {
		t.Fatalf("TotalCalls = %d, want 4", data.TotalCalls)
	}
	if data.SuccessCalls != 2 {
		t.Fatalf("SuccessCalls = %d, want 2", data.SuccessCalls)
	}
	// Ended excludes 499 (caller-cancel). Ended = r1+r2+r3 = 3.
	if data.EndedCalls != 3 {
		t.Fatalf("EndedCalls = %d, want 3", data.EndedCalls)
	}
	wantRate := float64(2) / float64(3)
	if !approxEqual(data.SuccessRate, wantRate, 1e-9) {
		t.Fatalf("SuccessRate = %v, want %v", data.SuccessRate, wantRate)
	}
	if data.InputTokens != 300 || data.OutputTokens != 150 {
		t.Fatalf("tokens = %d/%d, want 300/150", data.InputTokens, data.OutputTokens)
	}
	if data.CostMicros != 30 {
		t.Fatalf("CostMicros = %d, want 30", data.CostMicros)
	}
	if data.UnknownCostCalls != 2 {
		t.Fatalf("UnknownCostCalls = %d, want 2 (r3 + r4)", data.UnknownCostCalls)
	}
	// Cache token sums ride on the overview so clients can derive the
	// token-weighted hit rate for the same filtered window.
	if data.CacheWriteTokens != 30 || data.CacheReadTokens != 100 {
		t.Fatalf("cache tokens = %d/%d, want 30/100", data.CacheWriteTokens, data.CacheReadTokens)
	}
	// Pin the wire-level key names too: decoding into the shared Go struct
	// above stays green even if both json tags were renamed or swapped
	// together, and the frontend reads these exact keys.
	body := w.Body.String()
	if !strings.Contains(body, `"cache_write_tokens":30`) || !strings.Contains(body, `"cache_read_tokens":100`) {
		t.Fatalf("overview wire keys missing or mismatched, body: %s", body)
	}
}

func TestGetAnalyticsOverviewRespectsTimeRange(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	longAgo := now.Add(-30 * 24 * time.Hour)
	seedRequestLog(t, db, "old", longAgo, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 10
		r.OutputTokens = 5
		r.CostMicros = 1
		r.CostKnown = true
	})
	seedRequestLog(t, db, "new", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 20
		r.OutputTokens = 10
		r.CostMicros = 2
		r.CostKnown = true
	})

	// Window covering only `now`.
	start := now.Add(-time.Hour).Format(time.RFC3339)
	end := now.Add(time.Hour).Format(time.RFC3339)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview?start="+start+"&end="+end, nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data analytics.OverviewRow `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.TotalCalls != 1 {
		t.Fatalf("TotalCalls = %d, want 1 (only the recent row)", env.Data.TotalCalls)
	}
}

func TestGetAnalyticsOverviewRejectsBadStartTime(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview?start=not-a-time", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetAnalyticsOverviewRejectsBadStatus(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview?status=bogus", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// === Report handlers =====================================================

func TestGetAnalyticsReportByModelGroupsAndOrdersByCalls(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	mk := func(name string) func(*model.RequestLog) {
		return func(r *model.RequestLog) {
			r.ModelName = name
			r.StatusCode = 200
			r.InputTokens = 10
			r.OutputTokens = 5
			r.CostMicros = 1
			r.CostKnown = true
		}
	}
	seedRequestLog(t, db, "a1", now, mk("gpt-4"))
	seedRequestLog(t, db, "a2", now, mk("gpt-4"))
	seedRequestLog(t, db, "a3", now, mk("gpt-4"))
	seedRequestLog(t, db, "a4", now, mk("gpt-4o"))
	seedRequestLog(t, db, "a5", now, mk("claude"))
	seedRequestLog(t, db, "a6", now, mk("claude"))

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=model", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Dimension string `json:"dimension"`
			Rows      []struct {
				ModelName   string  `json:"model_name"`
				Calls       int64   `json:"calls"`
				SuccessRate float64 `json:"success_rate"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.Dimension != "model" {
		t.Fatalf("dimension = %q, want model", env.Data.Dimension)
	}
	if len(env.Data.Rows) != 3 {
		t.Fatalf("expected 3 model groups, got %d", len(env.Data.Rows))
	}
	// Ordered by calls DESC: gpt-4 (3), claude (2), gpt-4o (1).
	if env.Data.Rows[0].ModelName != "gpt-4" || env.Data.Rows[0].Calls != 3 {
		t.Fatalf("row[0] = %+v, want gpt-4/3", env.Data.Rows[0])
	}
	if env.Data.Rows[1].ModelName != "claude" || env.Data.Rows[1].Calls != 2 {
		t.Fatalf("row[1] = %+v, want claude/2", env.Data.Rows[1])
	}
	if env.Data.Rows[2].ModelName != "gpt-4o" || env.Data.Rows[2].Calls != 1 {
		t.Fatalf("row[2] = %+v, want gpt-4o/1", env.Data.Rows[2])
	}
	// All success, no cancels → rate = 1.0
	if env.Data.Rows[0].SuccessRate != 1.0 {
		t.Fatalf("SuccessRate = %v, want 1.0", env.Data.Rows[0].SuccessRate)
	}
}

func TestGetAnalyticsReportByModelComputesSuccessRateExcluding499(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	// 1 success + 1 server-error (5xx, ended) + 1 caller-cancel (499, NOT ended).
	seedRequestLog(t, db, "s", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.CostKnown = true
		r.CostMicros = 1
	})
	seedRequestLog(t, db, "f", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 500
		r.FailReason = analyticsStrPtr("err")
	})
	seedRequestLog(t, db, "c", now, func(r *model.RequestLog) { r.ModelName = "gpt-4"; r.StatusCode = 499 })

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=model", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				Calls        int64   `json:"calls"`
				SuccessCalls int64   `json:"success_calls"`
				EndedCalls   int64   `json:"ended_calls"`
				SuccessRate  float64 `json:"success_rate"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(env.Data.Rows))
	}
	row := env.Data.Rows[0]
	if row.Calls != 3 || row.SuccessCalls != 1 || row.EndedCalls != 2 {
		t.Fatalf("calls/success/ended = %d/%d/%d, want 3/1/2", row.Calls, row.SuccessCalls, row.EndedCalls)
	}
	want := float64(1) / float64(2)
	if !approxEqual(row.SuccessRate, want, 1e-9) {
		t.Fatalf("SuccessRate = %v, want %v", row.SuccessRate, want)
	}
}

func TestGetAnalyticsReportByProviderResolvesNamesViaPostFetch(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	// Seed a real Provider so resolveProviderNames can find it.
	prov := &model.Provider{Name: "openai-main", ProviderType: "openai", BaseURL: "https://api.example.com/v1", ManagementStatus: model.ProviderStatusEnabled}
	if err := db.Create(prov).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	now := time.Now().UTC()
	seedRequestLog(t, db, "p1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.ProviderID = &prov.ID
		r.StatusCode = 200
		r.InputTokens = 10
		r.OutputTokens = 5
		r.CostMicros = 1
		r.CostKnown = true
		r.DurationMs = 100
	})
	seedRequestLog(t, db, "p2", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200 // NULL-provider bucket
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=provider", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				ProviderID    *uint   `json:"provider_id"`
				ProviderName  string  `json:"provider_name"`
				Calls         int64   `json:"calls"`
				AvgDurationMs float64 `json:"avg_duration_ms"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (provider bucket + NULL bucket)", len(env.Data.Rows))
	}
	var named *struct {
		ProviderID    *uint   `json:"provider_id"`
		ProviderName  string  `json:"provider_name"`
		Calls         int64   `json:"calls"`
		AvgDurationMs float64 `json:"avg_duration_ms"`
	}
	for i := range env.Data.Rows {
		if env.Data.Rows[i].ProviderID != nil {
			named = &env.Data.Rows[i]
		}
	}
	if named == nil {
		t.Fatalf("no non-NULL provider bucket in result %+v", env.Data.Rows)
	}
	if named.ProviderName != "openai-main" {
		t.Fatalf("ProviderName = %q, want openai-main", named.ProviderName)
	}
	if named.Calls != 1 {
		t.Fatalf("Calls = %d, want 1", named.Calls)
	}
	// avg(duration_ms=100) over one row → 100.
	if named.AvgDurationMs != 100 {
		t.Fatalf("AvgDurationMs = %v, want 100", named.AvgDurationMs)
	}
}

func TestGetAnalyticsReportByCallerResolvesOwnerUsernames(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	key, _ := seedAPIKey(t, db, "alice")
	now := time.Now().UTC()
	seedRequestLog(t, db, "k1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.APIKeyID = &key
		r.StatusCode = 200
		r.InputTokens = 30
		r.OutputTokens = 15
		r.CostMicros = 3
		r.CostKnown = true
	})
	seedRequestLog(t, db, "k2", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200 // NULL-api_key bucket
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=caller", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				APIKeyID *uint  `json:"api_key_id"`
				Username string `json:"username"`
				Calls    int64  `json:"calls"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(env.Data.Rows))
	}
	var named *struct {
		APIKeyID *uint  `json:"api_key_id"`
		Username string `json:"username"`
		Calls    int64  `json:"calls"`
	}
	for i := range env.Data.Rows {
		if env.Data.Rows[i].APIKeyID != nil {
			named = &env.Data.Rows[i]
		}
	}
	if named == nil {
		t.Fatalf("no non-NULL api_key bucket in %+v", env.Data.Rows)
	}
	if named.Username != "alice" {
		t.Fatalf("Username = %q, want alice", named.Username)
	}
}

func TestGetAnalyticsReportDefaultsDimensionToModel(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	seedRequestLog(t, db, "d1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.CostKnown = true
		r.CostMicros = 1
	})

	// No ?dimension= on the URL at all.
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Dimension string `json:"dimension"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.Dimension != analytics.DimensionModel {
		t.Fatalf("default dimension = %q, want %q", env.Data.Dimension, analytics.DimensionModel)
	}
}

func TestGetAnalyticsReportRejectsUnknownDimension(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=banana", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetAnalyticsReportRejectsUnknownBucket(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=time&bucket=century", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// === Time dimension ======================================================

func TestGetAnalyticsReportByTimeDayBucketFillsGaps(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	// Seed a row 3 days ago and another 1 day ago; the day in between has
	// zero data and must still appear with zeros so the trend line is
	// continuous. Use UTC instants that map unambiguously to local days in
	// most timezones (mid-afternoon UTC lands in the same local calendar
	// day for offsets in [-11h, +10h], which covers CI runners).
	now := time.Now().UTC()
	day3 := now.Add(-3 * 24 * time.Hour)
	day1 := now.Add(-1 * 24 * time.Hour)
	seedRequestLog(t, db, "g1", day3, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.CostKnown = true
		r.CostMicros = 5
	})
	seedRequestLog(t, db, "g2", day1, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.CostKnown = true
		r.CostMicros = 10
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=time&bucket=day", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				Bucket string `json:"bucket"`
				Calls  int64  `json:"calls"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Buckets are ordered chronologically; format "YYYY-MM-DD".
	if len(env.Data.Rows) < 3 {
		t.Fatalf("expected at least 3 day buckets (gap-fill), got %d", len(env.Data.Rows))
	}
	// Find a zero-bucket in the middle — the day strictly between the two
	// seeded days. Exact date depends on time.Local, but a contiguous walk
	// must include at least one bucket with zero calls in the middle.
	sawZeroBetween := false
	for _, row := range env.Data.Rows[1 : len(env.Data.Rows)-1] {
		if row.Calls == 0 {
			sawZeroBetween = true
			break
		}
	}
	if !sawZeroBetween {
		t.Fatalf("no zero-call bucket between seeded days — gap-fill failed; rows: %+v", env.Data.Rows)
	}
	// Total calls across all buckets = 2.
	var total int64
	for _, row := range env.Data.Rows {
		total += row.Calls
	}
	if total != 2 {
		t.Fatalf("total calls across buckets = %d, want 2", total)
	}
}

// TestAggregateByTimeWalksDayBucketsInUTC exercises the repository directly
// with a fixed *time.Location so the bucket labels and walk length are
// deterministic regardless of the test machine's TZ.
func TestAggregateByTimeWalksDayBucketsInUTC(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	loc := time.UTC

	// Build an explicit [start, end) window: 3 UTC days starting from a
	// fixed base. Seed rows on day 0 and day 2; day 1 stays empty.
	base := time.Date(2026, 7, 14, 0, 0, 0, 0, loc)
	day0 := base
	day2 := base.AddDate(0, 0, 2)
	end := base.AddDate(0, 0, 3)

	seedRequestLog(t, db, "d0-a", day0.Add(6*time.Hour), func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 10
		r.OutputTokens = 5
		r.CostMicros = 1
		r.CostKnown = true
	})
	seedRequestLog(t, db, "d0-b", day0.Add(7*time.Hour), func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 10
		r.OutputTokens = 5
		r.CostMicros = 1
		r.CostKnown = true
	})
	seedRequestLog(t, db, "d2-a", day2.Add(8*time.Hour), func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 500
		r.FailReason = analyticsStrPtr("err")
	})

	startUTC := day0
	endUTC := end
	f := &repository.RequestLogFilter{StartTime: &startUTC, EndTime: &endUTC}

	// now is deliberately far from the explicit filter window — if it ever
	// leaked into the windowing, the bucket assertions below would fail.
	rows, err := repository.AggregateByTime(t.Context(), db, f, loc, repository.TimeBucketDay, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	wantBuckets := []string{"2026-07-16", "2026-07-15", "2026-07-14"}
	for i, want := range wantBuckets {
		if rows[i].Bucket != want {
			t.Fatalf("rows[%d].Bucket = %q, want %q", i, rows[i].Bucket, want)
		}
	}
	if rows[0].Calls != 1 || rows[0].SuccessCalls != 0 {
		t.Fatalf("day2 (newest) = %+v, want Calls=1 SuccessCalls=0", rows[0])
	}
	if rows[1].Calls != 0 {
		t.Fatalf("day1 gap-fill Calls = %d, want 0", rows[1].Calls)
	}
	if rows[2].Calls != 2 || rows[2].SuccessCalls != 2 {
		t.Fatalf("day0 (oldest) = %+v, want Calls=2 SuccessCalls=2", rows[2])
	}
}

func TestAggregateByTimeRejectsInvalidBucket(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	_, err := repository.AggregateByTime(t.Context(), db, &repository.RequestLogFilter{}, time.UTC, "century", time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if !errors.Is(err, repository.ErrInvalidBucket) {
		t.Fatalf("expected ErrInvalidBucket, got %v", err)
	}
}

// TestAggregateByTimeHourBucketsCrossUTCDay pins the hour-bucket walk in a
// non-UTC zone whose local buckets straddle a UTC midnight: labels carry the
// local wall clock plus the UTC offset suffix, empty buckets gap-fill with
// zeros, the order is newest-first, and no call is lost or double-counted
// across the UTC day boundary.
func TestAggregateByTimeHourBucketsCrossUTCDay(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	loc := time.FixedZone("UTC+8", 8*3600)

	// Local window 2026-07-14 23:00 → 2026-07-15 02:00 (+08): three hour
	// buckets, the last two on the next local day, all inside UTC July 14.
	start := time.Date(2026, 7, 14, 23, 0, 0, 0, loc)
	end := start.Add(3 * time.Hour)

	seedRequestLog(t, db, "h0", start.Add(10*time.Minute).UTC(), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.InputTokens = 7
		r.CostKnown = true
	})
	seedRequestLog(t, db, "h2-a", start.Add(2*time.Hour+40*time.Minute).UTC(), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})
	seedRequestLog(t, db, "h2-b", start.Add(2*time.Hour+50*time.Minute).UTC(), func(r *model.RequestLog) {
		r.StatusCode = 500
		r.FailReason = analyticsStrPtr("err")
		r.CostKnown = true
	})

	startUTC := start.UTC()
	endUTC := end.UTC()
	f := &repository.RequestLogFilter{StartTime: &startUTC, EndTime: &endUTC}
	rows, err := repository.AggregateByTime(t.Context(), db, f, loc, repository.TimeBucketHour, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}
	wantBuckets := []string{"2026-07-15 01:00 +08:00", "2026-07-15 00:00 +08:00", "2026-07-14 23:00 +08:00"}
	for i, want := range wantBuckets {
		if rows[i].Bucket != want {
			t.Fatalf("rows[%d].Bucket = %q, want %q", i, rows[i].Bucket, want)
		}
	}
	if rows[0].Calls != 2 || rows[0].SuccessCalls != 1 {
		t.Fatalf("newest bucket = %+v, want Calls=2 SuccessCalls=1", rows[0])
	}
	if rows[1].Calls != 0 {
		t.Fatalf("middle bucket gap-fill Calls = %d, want 0", rows[1].Calls)
	}
	if rows[2].Calls != 1 || rows[2].InputTokens != 7 {
		t.Fatalf("oldest bucket = %+v, want Calls=1 InputTokens=7", rows[2])
	}
	if total := rows[0].Calls + rows[1].Calls + rows[2].Calls; total != 3 {
		t.Fatalf("total calls = %d, want 3 (no loss across the UTC day boundary)", total)
	}
}

// TestAggregateByTimeKeepsBucketsAnchoredAtStart pins that buckets stay
// anchored at the range start rather than snapping to clock boundaries: a
// window starting at 14:23 produces [14:23, 15:23) and [15:23, 16:23), and
// two rows that share the 15:00 clock hour land in DIFFERENT buckets because
// the anchor splits that hour. An implementation that grouped by wall-clock
// hour would fold them together.
func TestAggregateByTimeKeepsBucketsAnchoredAtStart(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	start := time.Date(2026, 7, 14, 14, 23, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)

	seedRequestLog(t, db, "first-bucket", time.Date(2026, 7, 14, 15, 0, 0, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})
	seedRequestLog(t, db, "second-bucket", time.Date(2026, 7, 14, 15, 30, 0, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})

	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}
	rows, err := repository.AggregateByTime(t.Context(), db, f, time.UTC, repository.TimeBucketHour, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Bucket != "2026-07-14 15:23 +00:00" || rows[1].Bucket != "2026-07-14 14:23 +00:00" {
		t.Fatalf("buckets = %q / %q, want start-anchored 15:23 / 14:23", rows[0].Bucket, rows[1].Bucket)
	}
	if rows[1].Calls != 1 || rows[0].Calls != 1 {
		t.Fatalf("calls = %d / %d, want 1 in each bucket (15:00 belongs to the 14:23 bucket, 15:30 to the 15:23 one)", rows[1].Calls, rows[0].Calls)
	}
}

// TestAggregateByTimeSubMinuteStartClampsToFirstBucket pins the sub-minute
// anchor edge: a range starting mid-minute (14:23:30) floors its first
// rows' minute group to 14:23:00, which sorts BEFORE the first bucket
// boundary — those rows are still inside [start, end) and must clamp into
// the first bucket instead of being dropped or panicking.
func TestAggregateByTimeSubMinuteStartClampsToFirstBucket(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	start := time.Date(2026, 7, 14, 14, 23, 30, 0, time.UTC)
	end := start.Add(time.Hour)

	seedRequestLog(t, db, "sub-minute", time.Date(2026, 7, 14, 14, 23, 45, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})
	seedRequestLog(t, db, "mid-bucket", time.Date(2026, 7, 14, 14, 50, 0, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})

	f := &repository.RequestLogFilter{StartTime: &start, EndTime: &end}
	rows, err := repository.AggregateByTime(t.Context(), db, f, time.UTC, repository.TimeBucketHour, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].Calls != 2 {
		t.Fatalf("Calls = %d, want 2 (the 14:23:45 row must clamp into the first bucket, not vanish)", rows[0].Calls)
	}
}

// TestAggregateByTimeDayBucketAcrossDSTTransition pins the day-bucket walk
// across a spring-forward transition: the transition day is 23 hours long,
// so the next day's boundary sits at local midnight, NOT 24 clock hours
// later. A row in the first half hour after that midnight belongs to the
// post-transition day — a walk advancing by a fixed 24h would misfile it
// into the transition day.
func TestAggregateByTimeDayBucketAcrossDSTTransition(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// America/New_York springs forward on 2026-03-08 (02:00 EST -> 03:00
	// EDT), making that local day 23 hours long.
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	start := time.Date(2026, 3, 8, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 2)

	// 03:30 UTC on March 9 = 23:30 EDT on March 8 — late in the transition
	// day. 04:30 UTC = 00:30 EDT on March 9 — just past the 23-hour
	// boundary, inside the drift window where a fixed-24h walk misfiles.
	seedRequestLog(t, db, "transition-day", time.Date(2026, 3, 9, 3, 30, 0, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})
	seedRequestLog(t, db, "day-after", time.Date(2026, 3, 9, 4, 30, 0, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})

	startUTC := start.UTC()
	endUTC := end.UTC()
	f := &repository.RequestLogFilter{StartTime: &startUTC, EndTime: &endUTC}
	rows, err := repository.AggregateByTime(t.Context(), db, f, loc, repository.TimeBucketDay, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2 (the 23-hour day and the day after)", len(rows))
	}
	if rows[0].Bucket != "2026-03-09" || rows[1].Bucket != "2026-03-08" {
		t.Fatalf("buckets = %q / %q, want 2026-03-09 / 2026-03-08", rows[0].Bucket, rows[1].Bucket)
	}
	if rows[1].Calls != 1 || rows[0].Calls != 1 {
		t.Fatalf("calls = %d / %d, want 1 in each day (00:30 EDT belongs to March 9, not the 23-hour March 8)", rows[1].Calls, rows[0].Calls)
	}
}

// TestAggregateByTimeDayBucketNonUTCOffset pins day-bucket assignment in a
// non-UTC zone: a row late in the UTC evening belongs to the NEXT local
// calendar day.
func TestAggregateByTimeDayBucketNonUTCOffset(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	loc := time.FixedZone("UTC+8", 8*3600)
	start := time.Date(2026, 7, 14, 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, 2)

	// 20:00 UTC on July 14 is 04:00 on July 15 in +08:00.
	seedRequestLog(t, db, "utc-evening", time.Date(2026, 7, 14, 20, 0, 0, 0, time.UTC), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostKnown = true
	})

	startUTC := start.UTC()
	endUTC := end.UTC()
	f := &repository.RequestLogFilter{StartTime: &startUTC, EndTime: &endUTC}
	rows, err := repository.AggregateByTime(t.Context(), db, f, loc, repository.TimeBucketDay, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("AggregateByTime: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Bucket != "2026-07-15" || rows[0].Calls != 1 {
		t.Fatalf("rows[0] = %+v, want the 2026-07-15 local day carrying the call", rows[0])
	}
	if rows[1].Bucket != "2026-07-14" || rows[1].Calls != 0 {
		t.Fatalf("rows[1] = %+v, want an empty 2026-07-14 local day", rows[1])
	}
}

// === CSV export ==========================================================

func TestExportAnalyticsCSVWritesBOMAndHeadersAndRows(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	prov := &model.Provider{Name: "openai-main", ProviderType: "openai", BaseURL: "https://api.example.com/v1", ManagementStatus: model.ProviderStatusEnabled}
	if err := db.Create(prov).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	now := time.Now().UTC()
	seedRequestLog(t, db, "c1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.ProviderID = &prov.ID
		r.StatusCode = 200
		r.InputTokens = 10
		r.OutputTokens = 5
		r.CostMicros = 1
		r.CostKnown = true
	})

	req := httptest.NewRequest(http.MethodGet, "/api/admin/analytics/export?dimension=model", nil)
	req.AddCookie(ck)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("Content-Type = %q, want text/csv*", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment;") || !strings.Contains(cd, ".csv") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	body := w.Body.Bytes()
	// UTF-8 BOM
	if len(body) < 3 || body[0] != 0xEF || body[1] != 0xBB || body[2] != 0xBF {
		t.Fatalf("missing UTF-8 BOM; first bytes: % x", body[:minInt(3, len(body))])
	}
	reader := csv.NewReader(bytes.NewReader(body[3:]))
	records, err := reader.ReadAll()
	if err != nil {
		t.Fatalf("csv read: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("expected header + at least 1 row, got %d records", len(records))
	}
	wantHeader := []string{"model_name", "calls", "success_rate", "input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens", "cost_micros", "unknown_cost_calls", "cache_read_saved_micros", "cache_write_extra_micros"}
	if len(records[0]) != len(wantHeader) {
		t.Fatalf("header len = %d, want %d (%v)", len(records[0]), len(wantHeader), records[0])
	}
	for i, h := range wantHeader {
		if records[0][i] != h {
			t.Fatalf("header[%d] = %q, want %q", i, records[0][i], h)
		}
	}
	// Find the gpt-4 row.
	var found bool
	for _, rec := range records[1:] {
		if rec[0] == "gpt-4" {
			found = true
			if rec[1] != "1" {
				t.Fatalf("gpt-4 calls = %q, want 1", rec[1])
			}
			break
		}
	}
	if !found {
		t.Fatalf("gpt-4 row missing from CSV; records: %+v", records[1:])
	}
}

func TestExportAnalyticsCSVRejectsUnknownDimension(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/analytics/export?dimension=banana", nil)
	req.AddCookie(ck)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// === Compress stats ======================================================

// compressStatsEnvelop mirrors the response envelope for GetCompressStats —
// kept local so the test doesn't need to import the service-level Compress*
// types. Field names match the JSON tags.
type compressStatsEnvelop struct {
	Totals struct {
		TotalCalls           int64 `json:"total_calls"`
		CompressedCalls      int64 `json:"compressed_calls"`
		TokensSaved          int64 `json:"tokens_saved"`
		CostSavedMicros      int64 `json:"cost_saved_micros"`
		TotalEstimatedTokens int64 `json:"total_estimated_tokens"`
	} `json:"totals"`
	SkipReasonBreakdown []struct {
		SkipReason string `json:"skip_reason"`
		Calls      int64  `json:"calls"`
	} `json:"skip_reason_breakdown"`
	TopAPIKeys []struct {
		APIKeyID    *uint  `json:"api_key_id"`
		Username    string `json:"username"`
		Calls       int64  `json:"calls"`
		TokensSaved int64  `json:"tokens_saved"`
	} `json:"top_api_keys"`
	TopModels []struct {
		ModelName       string `json:"model_name"`
		TokensSaved     int64  `json:"tokens_saved"`
		CostSavedMicros int64  `json:"cost_saved_micros"`
		CompressedCalls int64  `json:"compressed_calls"`
		TotalCalls      int64  `json:"total_calls"`
	} `json:"top_models"`
	TopProviders []struct {
		ProviderID      *uint  `json:"provider_id"`
		ProviderName    string `json:"provider_name"`
		TokensSaved     int64  `json:"tokens_saved"`
		CostSavedMicros int64  `json:"cost_saved_micros"`
		CompressedCalls int64  `json:"compressed_calls"`
		TotalCalls      int64  `json:"total_calls"`
	} `json:"top_providers"`
	CompressorHits []struct {
		Name string `json:"name"`
		Hits int64  `json:"hits"`
	} `json:"compressor_hits"`
	DailySeries []struct {
		Bucket          string `json:"bucket"`
		TokensSaved     int64  `json:"tokens_saved"`
		CostSavedMicros int64  `json:"cost_saved_micros"`
		CompressedCalls int64  `json:"compressed_calls"`
	} `json:"daily_series"`
}

// doCompressStats is a tiny helper: hits the endpoint, unmarshals the
// envelope, fails the test on any non-200 or unmarshal error.
func doCompressStats(t *testing.T, r *gin.Engine, ck *http.Cookie, query string) compressStatsEnvelop {
	t.Helper()
	path := "/api/admin/analytics/compress-stats"
	if query != "" {
		path += "?" + query
	}
	w, _ := doJSON(t, r, http.MethodGet, path, nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int                  `json:"code"`
		Data compressStatsEnvelop `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return env.Data
}

// TestGetCompressStatsAggregatesSeededRows exercises the full roll-up:
// totals, skip-reason breakdown, Top-N api keys, compressor-hit counting
// (the "diff,gotest,diff" row now counts once per compressor name — the
// new semantics count ROWS-that-used-the-compressor, not total invocations),
// and the daily series (now a single GROUP BY query with gap-fill).
func TestGetCompressStatsAggregatesSeededRows(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)

	// Seed two api_keys with distinct owners so the Top-N bucket resolves
	// each key's username.
	key1, _ := seedAPIKey(t, db, "alice")
	key2, _ := seedAPIKey(t, db, "bob")
	// Seed two providers so TopProviders resolves provider_name.
	prov1 := seedProvider(t, db, "openai-main")
	prov2 := seedProvider(t, db, "anthropic-main")

	now := time.Now().UTC()
	day3 := now.Add(-3 * 24 * time.Hour)

	// Row 1 (alice, gpt-4o-mini, openai, compressed, diff+gotest+diff): 100 tokens, 10 cost.
	seedRequestLog(t, db, "c1", now, func(r *model.RequestLog) {
		r.APIKeyID = &key1
		r.ModelName = "gpt-4o-mini"
		r.ProviderID = &prov1
		r.InputTokens = 500
		r.CompressEstimatedTokensSaved = 100
		r.CompressEstimatedCostSavedMicros = 10
		r.CompressSkipReason = ""
		r.CompressorsApplied = "diff,gotest,diff"
	})
	// Row 2 (bob, claude-3-haiku, anthropic, compressed, diff): 50 tokens, 5 cost.
	seedRequestLog(t, db, "c2", now, func(r *model.RequestLog) {
		r.APIKeyID = &key2
		r.ModelName = "claude-3-haiku"
		r.ProviderID = &prov2
		r.InputTokens = 200
		r.CompressEstimatedTokensSaved = 50
		r.CompressEstimatedCostSavedMicros = 5
		r.CompressSkipReason = ""
		r.CompressorsApplied = "diff"
	})
	// Row 3 (alice, gpt-4o-mini, openai, skipped too_small, 0 tokens, no compressors).
	seedRequestLog(t, db, "c3", now, func(r *model.RequestLog) {
		r.APIKeyID = &key1
		r.ModelName = "gpt-4o-mini"
		r.ProviderID = &prov1
		r.InputTokens = 5
		r.CompressSkipReason = "too_small"
		r.CompressorsApplied = ""
	})
	// Row 4 (alice, gpt-4o-mini, openai, 3 days ago, compressed, gotest): 30 tokens, 3 cost.
	seedRequestLog(t, db, "c4", day3, func(r *model.RequestLog) {
		r.APIKeyID = &key1
		r.ModelName = "gpt-4o-mini"
		r.ProviderID = &prov1
		r.InputTokens = 100
		r.CompressEstimatedTokensSaved = 30
		r.CompressEstimatedCostSavedMicros = 3
		r.CompressSkipReason = ""
		r.CompressorsApplied = "gotest"
	})
	// Row 5 (bob, claude-3-haiku, anthropic, compression never attempted).
	// BOTH compress_skip_reason='' AND compressors_applied='' — this is the
	// regression case that proves compressed_calls is gated on
	// compressors_applied, not on compress_skip_reason. Counting this row as
	// compressed would be the overcount bug.
	seedRequestLog(t, db, "c5", now, func(r *model.RequestLog) {
		r.APIKeyID = &key2
		r.ModelName = "claude-3-haiku"
		r.ProviderID = &prov2
		r.InputTokens = 50
	})

	data := doCompressStats(t, r, ck, "")

	// Totals: 5 total calls, 3 compressed (rows 1, 2, 4 — NOT row 5 which
	// never attempted), tokens 100+50+30=180, cost 10+5+3=18, total
	// input_tokens 500+200+5+100+50=855.
	if data.Totals.TotalCalls != 5 {
		t.Fatalf("TotalCalls = %d, want 5", data.Totals.TotalCalls)
	}
	if data.Totals.CompressedCalls != 3 {
		t.Fatalf("CompressedCalls = %d, want 3 (row 5 must NOT count)", data.Totals.CompressedCalls)
	}
	if data.Totals.TokensSaved != 180 {
		t.Fatalf("TokensSaved = %d, want 180", data.Totals.TokensSaved)
	}
	if data.Totals.CostSavedMicros != 18 {
		t.Fatalf("CostSavedMicros = %d, want 18", data.Totals.CostSavedMicros)
	}
	if data.Totals.TotalEstimatedTokens != 855 {
		t.Fatalf("TotalEstimatedTokens = %d, want 855", data.Totals.TotalEstimatedTokens)
	}

	// Skip-reason breakdown is gated on rows that ENTERED the compress stage
	// (compressors_applied != '' OR compress_skip_reason != ''). Row 5 (both
	// empty — switch off / never attempted) must NOT appear here; the ''
	// bucket is "entered + succeeded" only (rows 1, 2, 4), and 'too_small' is
	// row 3 which entered but skipped.
	if len(data.SkipReasonBreakdown) != 2 {
		t.Fatalf("SkipReasonBreakdown len = %d, want 2", len(data.SkipReasonBreakdown))
	}
	if data.SkipReasonBreakdown[0].SkipReason != "" || data.SkipReasonBreakdown[0].Calls != 3 {
		t.Fatalf("SkipReasonBreakdown[0] = %+v, want ''/3 (row 5 must NOT appear)", data.SkipReasonBreakdown[0])
	}
	if data.SkipReasonBreakdown[1].SkipReason != "too_small" || data.SkipReasonBreakdown[1].Calls != 1 {
		t.Fatalf("SkipReasonBreakdown[1] = %+v, want too_small/1", data.SkipReasonBreakdown[1])
	}

	// Top-N api keys by tokens_saved DESC: alice (130 = 100+30), bob (50).
	if len(data.TopAPIKeys) != 2 {
		t.Fatalf("TopAPIKeys len = %d, want 2", len(data.TopAPIKeys))
	}
	if data.TopAPIKeys[0].Username != "alice" || data.TopAPIKeys[0].TokensSaved != 130 {
		t.Fatalf("TopAPIKeys[0] = %+v, want alice/130", data.TopAPIKeys[0])
	}
	if data.TopAPIKeys[1].Username != "bob" || data.TopAPIKeys[1].TokensSaved != 50 {
		t.Fatalf("TopAPIKeys[1] = %+v, want bob/50", data.TopAPIKeys[1])
	}
	// Calls per key: alice has 3 rows (c1, c3, c4); bob has 2 (c2, c5).
	if data.TopAPIKeys[0].Calls != 3 {
		t.Fatalf("TopAPIKeys[0].Calls = %d, want 3", data.TopAPIKeys[0].Calls)
	}
	if data.TopAPIKeys[1].Calls != 2 {
		t.Fatalf("TopAPIKeys[1].Calls = %d, want 2", data.TopAPIKeys[1].Calls)
	}

	// Top-N models by tokens_saved DESC. Only compressed rows (c1, c2, c4)
	// participate — c3 (skip) and c5 (never attempted) are excluded by the
	// compressors_applied != '' gate.
	// gpt-4o-mini: 100+30=130 tokens, 10+3=13 cost, 2 compressed, 2 total.
	// claude-3-haiku: 50 tokens, 5 cost, 1 compressed, 1 total.
	if len(data.TopModels) != 2 {
		t.Fatalf("TopModels len = %d, want 2", len(data.TopModels))
	}
	if data.TopModels[0].ModelName != "gpt-4o-mini" || data.TopModels[0].TokensSaved != 130 {
		t.Fatalf("TopModels[0] = %+v, want gpt-4o-mini/130", data.TopModels[0])
	}
	if data.TopModels[0].CostSavedMicros != 13 || data.TopModels[0].CompressedCalls != 2 || data.TopModels[0].TotalCalls != 2 {
		t.Fatalf("TopModels[0] = %+v, want cost=13/compressed=2/total=2", data.TopModels[0])
	}
	if data.TopModels[1].ModelName != "claude-3-haiku" || data.TopModels[1].TokensSaved != 50 {
		t.Fatalf("TopModels[1] = %+v, want claude-3-haiku/50", data.TopModels[1])
	}
	if data.TopModels[1].CostSavedMicros != 5 || data.TopModels[1].CompressedCalls != 1 || data.TopModels[1].TotalCalls != 1 {
		t.Fatalf("TopModels[1] = %+v, want cost=5/compressed=1/total=1", data.TopModels[1])
	}

	// Top-N providers by tokens_saved DESC. Same compressed-only gate.
	// openai-main: 100+30=130 tokens, 10+3=13 cost, 2 compressed, 2 total.
	// anthropic-main: 50 tokens, 5 cost, 1 compressed, 1 total.
	if len(data.TopProviders) != 2 {
		t.Fatalf("TopProviders len = %d, want 2", len(data.TopProviders))
	}
	if data.TopProviders[0].ProviderName != "openai-main" || data.TopProviders[0].TokensSaved != 130 {
		t.Fatalf("TopProviders[0] = %+v, want openai-main/130", data.TopProviders[0])
	}
	if data.TopProviders[0].CostSavedMicros != 13 || data.TopProviders[0].CompressedCalls != 2 || data.TopProviders[0].TotalCalls != 2 {
		t.Fatalf("TopProviders[0] = %+v, want cost=13/compressed=2/total=2", data.TopProviders[0])
	}
	if data.TopProviders[1].ProviderName != "anthropic-main" || data.TopProviders[1].TokensSaved != 50 {
		t.Fatalf("TopProviders[1] = %+v, want anthropic-main/50", data.TopProviders[1])
	}
	if data.TopProviders[1].CostSavedMicros != 5 || data.TopProviders[1].CompressedCalls != 1 || data.TopProviders[1].TotalCalls != 1 {
		t.Fatalf("TopProviders[1] = %+v, want cost=5/compressed=1/total=1", data.TopProviders[1])
	}

	// Compressor hits: counts ROWS that used each compressor (not total
	// invocations). c1 "diff,gotest,diff" counts once for diff + once for
	// gotest; c2 "diff" counts once for diff; c4 "gotest" counts once for
	// gotest. So diff=2 (c1+c2), gotest=2 (c1+c4), log=0, grep=0.
	// Zero-hit entries are retained (all four known compressors appear).
	// Ordered by hits DESC, name ASC: diff(2), gotest(2), grep(0), log(0).
	if len(data.CompressorHits) != 8 {
		t.Fatalf("CompressorHits len = %d, want 8 (all known compressors)", len(data.CompressorHits))
	}
	if data.CompressorHits[0].Name != "diff" || data.CompressorHits[0].Hits != 2 {
		t.Fatalf("CompressorHits[0] = %+v, want diff/2 (rows c1+c2)", data.CompressorHits[0])
	}
	if data.CompressorHits[1].Name != "gotest" || data.CompressorHits[1].Hits != 2 {
		t.Fatalf("CompressorHits[1] = %+v, want gotest/2 (rows c1+c4)", data.CompressorHits[1])
	}
	if data.CompressorHits[2].Name != "grep" || data.CompressorHits[2].Hits != 0 {
		t.Fatalf("CompressorHits[2] = %+v, want grep/0", data.CompressorHits[2])
	}
	if data.CompressorHits[3].Name != "log" || data.CompressorHits[3].Hits != 0 {
		t.Fatalf("CompressorHits[3] = %+v, want log/0", data.CompressorHits[3])
	}

	// Daily series: at least 4 days (gap-filled between day3 and now).
	// Total tokens across the series = 180; total compressed_calls = 3.
	if len(data.DailySeries) < 4 {
		t.Fatalf("DailySeries len = %d, want >= 4 (gap-fill)", len(data.DailySeries))
	}
	var seriesTokens int64
	var seriesCalls int64
	for _, row := range data.DailySeries {
		seriesTokens += row.TokensSaved
		seriesCalls += row.CompressedCalls
	}
	if seriesTokens != 180 {
		t.Fatalf("series TokensSaved sum = %d, want 180", seriesTokens)
	}
	if seriesCalls != 3 {
		t.Fatalf("series CompressedCalls sum = %d, want 3", seriesCalls)
	}
	// Newest-first ordering (today at index 0).
	if data.DailySeries[0].Bucket == "" {
		t.Fatalf("DailySeries[0].Bucket empty; rows: %+v", data.DailySeries)
	}
}

// TestGetCompressStatsEmptyReturnsEmptyArrays verifies that a filter that
// matches zero rows produces empty arrays (not JSON null) for every slice
// field — the contract the frontend relies on for .map / .length.
func TestGetCompressStatsEmptyReturnsEmptyArrays(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	// No rows seeded; the default 7-day window still matches nothing.
	data := doCompressStats(t, r, ck, "")

	if data.SkipReasonBreakdown == nil {
		t.Fatalf("SkipReasonBreakdown = nil, want empty slice")
	}
	if len(data.SkipReasonBreakdown) != 0 {
		t.Fatalf("SkipReasonBreakdown len = %d, want 0", len(data.SkipReasonBreakdown))
	}
	if data.TopAPIKeys == nil {
		t.Fatalf("TopAPIKeys = nil, want empty slice")
	}
	if len(data.TopAPIKeys) != 0 {
		t.Fatalf("TopAPIKeys len = %d, want 0", len(data.TopAPIKeys))
	}
	if data.TopModels == nil {
		t.Fatalf("TopModels = nil, want empty slice")
	}
	if len(data.TopModels) != 0 {
		t.Fatalf("TopModels len = %d, want 0", len(data.TopModels))
	}
	if data.TopProviders == nil {
		t.Fatalf("TopProviders = nil, want empty slice")
	}
	if len(data.TopProviders) != 0 {
		t.Fatalf("TopProviders len = %d, want 0", len(data.TopProviders))
	}
	if data.CompressorHits == nil {
		t.Fatalf("CompressorHits = nil, want slice")
	}
	// CompressorHits now always returns the four known compressors (zero-hit
	// entries retained) so the UI can show which compressors exist even when
	// they haven't fired.
	if len(data.CompressorHits) != 8 {
		t.Fatalf("CompressorHits len = %d, want 8 (all known, zero-hit)", len(data.CompressorHits))
	}
	for _, ch := range data.CompressorHits {
		if ch.Hits != 0 {
			t.Fatalf("CompressorHits = %+v, want all zero", data.CompressorHits)
		}
	}
	if data.DailySeries == nil {
		t.Fatalf("DailySeries = nil, want empty slice")
	}
	// DailySeries is still gap-filled (7 days of zeros by default), so it
	// isn't empty — but it must be a non-nil slice.
	if len(data.DailySeries) == 0 {
		t.Fatalf("DailySeries len = 0, want gap-filled days")
	}
	// Totals is zero-valued but not nil (struct value, never nil).
	if data.Totals.TotalCalls != 0 || data.Totals.TokensSaved != 0 {
		t.Fatalf("Totals = %+v, want all zero", data.Totals)
	}
}

// TestGetCompressStatsRespectsAPIKeyFilter verifies the shared filter shape
// propagates to every sub-query (totals, skip-reasons, Top-N, compressors,
// daily). An api_key_id filter that matches one key should yield that key's
// rows only.
func TestGetCompressStatsRespectsAPIKeyFilter(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	key1, _ := seedAPIKey(t, db, "alice")
	key2, _ := seedAPIKey(t, db, "bob")
	now := time.Now().UTC()
	seedRequestLog(t, db, "c1", now, func(r *model.RequestLog) {
		r.APIKeyID = &key1
		r.CompressEstimatedTokensSaved = 100
		r.CompressorsApplied = "diff"
	})
	seedRequestLog(t, db, "c2", now, func(r *model.RequestLog) {
		r.APIKeyID = &key2
		r.CompressEstimatedTokensSaved = 50
		r.CompressorsApplied = "gotest"
	})

	// Filter to alice's key only.
	data := doCompressStats(t, r, ck, "api_key_id="+strconv.FormatUint(uint64(key1), 10))
	if data.Totals.TotalCalls != 1 {
		t.Fatalf("TotalCalls = %d, want 1 (filtered)", data.Totals.TotalCalls)
	}
	if data.Totals.TokensSaved != 100 {
		t.Fatalf("TokensSaved = %d, want 100", data.Totals.TokensSaved)
	}
	if len(data.TopAPIKeys) != 1 || data.TopAPIKeys[0].Username != "alice" {
		t.Fatalf("TopAPIKeys = %+v, want alice only", data.TopAPIKeys)
	}
	// CompressorHits returns all four known compressors; only diff has a
	// non-zero hit count (1 row, alice's c1 which used "diff").
	if len(data.CompressorHits) != 8 {
		t.Fatalf("CompressorHits len = %d, want 8 (all known)", len(data.CompressorHits))
	}
	if data.CompressorHits[0].Name != "diff" || data.CompressorHits[0].Hits != 1 {
		t.Fatalf("CompressorHits[0] = %+v, want diff/1", data.CompressorHits[0])
	}
}

// TestGetCompressStatsLimitParamClampsToMax verifies the limit query param
// both clamps and defaults correctly. We pass limit=1000 and expect the
// service to clamp to MaxCompressTopN (20); the row count can't exceed the
// seeded key count either way, so we verify the request is accepted and
// returns at most MaxCompressTopN rows.
func TestGetCompressStatsLimitParamClampsToMax(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	// No rows seeded — clamp doesn't change row count, but a 1000 must not
	// 400 (the parser accepts and clamps).
	data := doCompressStats(t, r, ck, "limit=1000")
	if len(data.TopAPIKeys) > analytics.MaxCompressTopN {
		t.Fatalf("TopAPIKeys len = %d, want <= %d (clamped)", len(data.TopAPIKeys), analytics.MaxCompressTopN)
	}
}

// TestGetCompressStatsRejectsBadLimit verifies a non-numeric / zero limit
// produces a 400, not a silent default — same posture as the existing
// filter-param validators.
func TestGetCompressStatsRejectsBadLimit(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/compress-stats?limit=banana", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for limit=banana, got %d", w.Code)
	}
	w, _ = doJSON(t, r, http.MethodGet, "/api/admin/analytics/compress-stats?limit=0", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for limit=0, got %d", w.Code)
	}
}

// TestGetCompressStatsRejectsBadStartTime verifies the shared filter parser
// still runs before compress-stats does its work — a bad start timestamp
// fails the same way it does for /analytics/overview.
func TestGetCompressStatsRejectsBadStartTime(t *testing.T) {
	r, _, ck := newAnalyticsFixture(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/compress-stats?start=not-a-time", nil, ck)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestGetCompressStatsTopAPIKeysDropsNullBucketWithinLimit seeds a NULL
// api_key_id row (auth-failed requests) whose tokens_saved would sort it
// inside the Top-N window, then asserts the NULL bucket is excluded at the
// SQL layer (HAVING api_key_id IS NOT NULL) rather than post-fetch in Go.
//
// Before the HAVING fix the NULL row consumed a LIMIT slot and was then
// dropped by a Go loop, so the caller got back limit-1 real keys instead of
// the requested limit. With 5 real keys + 1 NULL bucket and limit=5, the
// response must contain all 5 real keys (not 4).
func TestGetCompressStatsTopAPIKeysDropsNullBucketWithinLimit(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)

	// Five real keys, each with a distinct tokens_saved so the order is
	// deterministic. Their values are all BELOW the NULL bucket's 1000 so
	// the NULL row would land at position 0 inside the LIMIT=5 window.
	for i := 0; i < 5; i++ {
		key, _ := seedAPIKey(t, db, "owner"+strconv.Itoa(i))
		seedRequestLog(t, db, "real"+strconv.Itoa(i), time.Now().UTC(), func(r *model.RequestLog) {
			r.APIKeyID = &key
			r.CompressEstimatedTokensSaved = 100 - i // 100, 99, 98, 97, 96
		})
	}

	// NULL api_key_id row — sorts first (1000 > 100) and would steal a slot.
	seedRequestLog(t, db, "null-bucket", time.Now().UTC(), func(r *model.RequestLog) {
		r.APIKeyID = nil
		r.CompressEstimatedTokensSaved = 1000
	})

	data := doCompressStats(t, r, ck, "limit=5")
	if len(data.TopAPIKeys) != 5 {
		t.Fatalf("TopAPIKeys len = %d, want 5 (NULL bucket must not consume a LIMIT slot)", len(data.TopAPIKeys))
	}
	for _, row := range data.TopAPIKeys {
		if row.APIKeyID == nil {
			t.Fatalf("NULL api_key_id row leaked into TopAPIKeys: %+v", row)
		}
		if row.Username == "" {
			t.Fatalf("Username empty for api_key_id=%v (owner resolution broken)", row.APIKeyID)
		}
	}
}

// TestGetCompressStatsTopProvidersDropsNullBucketWithinLimit mirrors the
// TopAPIKeys NULL-bucket test: seeds a NULL provider_id row whose
// tokens_saved would sort it first, then asserts the HAVING clause drops it
// at the SQL layer so all 3 real providers fit within LIMIT=3.
func TestGetCompressStatsTopProvidersDropsNullBucketWithinLimit(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)

	// Three real providers with descending tokens_saved so the order is
	// deterministic. All values are BELOW the NULL bucket's 1000 so the NULL
	// row would land at position 0 inside the LIMIT=3 window.
	for i := 0; i < 3; i++ {
		pid := seedProvider(t, db, "p"+strconv.Itoa(i))
		seedRequestLog(t, db, "prov"+strconv.Itoa(i), time.Now().UTC(), func(r *model.RequestLog) {
			r.ProviderID = &pid
			r.ModelName = "m" + strconv.Itoa(i)
			r.CompressEstimatedTokensSaved = 100 - i // 100, 99, 98
			r.CompressorsApplied = "diff"
		})
	}

	// NULL provider_id row — sorts first (1000 > 100) and would steal a slot.
	seedRequestLog(t, db, "null-prov", time.Now().UTC(), func(r *model.RequestLog) {
		r.ProviderID = nil
		r.CompressEstimatedTokensSaved = 1000
		r.CompressorsApplied = "diff"
	})

	data := doCompressStats(t, r, ck, "limit=3")
	if len(data.TopProviders) != 3 {
		t.Fatalf("TopProviders len = %d, want 3 (NULL bucket must not consume a LIMIT slot)", len(data.TopProviders))
	}
	for _, row := range data.TopProviders {
		if row.ProviderID == nil {
			t.Fatalf("NULL provider_id row leaked into TopProviders: %+v", row)
		}
		if row.ProviderName == "" {
			t.Fatalf("ProviderName empty for provider_id=%v (name resolution broken)", row.ProviderID)
		}
	}
}

// TestGetCompressStatsDailySeriesAscendingWithGapFill seeds compressed rows
// on two non-adjacent days inside a narrow window, then verifies the daily
// series:
//   - is gap-filled (every day in the window appears, even with zero rows),
//   - is sorted ascending (oldest-first) for a left-to-right trend chart,
//   - only counts rows where compressors_applied != ”.
func TestGetCompressStatsDailySeriesAscendingWithGapFill(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)

	now := time.Now().UTC()
	// Two rows: today and 2 days ago. The day in between has zero compressed
	// rows and must still appear as a zero gap-fill row.
	seedRequestLog(t, db, "c1", now, func(r *model.RequestLog) {
		r.CompressEstimatedTokensSaved = 100
		r.CompressorsApplied = "diff"
	})
	seedRequestLog(t, db, "c2", now.Add(-2*24*time.Hour), func(r *model.RequestLog) {
		r.CompressEstimatedTokensSaved = 50
		r.CompressorsApplied = "gotest"
	})

	// Use a 4-day window so we know there are exactly 4 buckets.
	start := now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	end := now.Add(1 * 24 * time.Hour).Format(time.RFC3339)
	data := doCompressStats(t, r, ck, "start="+start+"&end="+end)

	if len(data.DailySeries) < 4 {
		t.Fatalf("DailySeries len = %d, want >= 4 (gap-fill)", len(data.DailySeries))
	}
	// Ascending: every bucket label must be <= the next (oldest-first).
	for i := 1; i < len(data.DailySeries); i++ {
		if data.DailySeries[i].Bucket < data.DailySeries[i-1].Bucket {
			t.Fatalf("DailySeries not ascending at index %d: %q > %q",
				i, data.DailySeries[i-1].Bucket, data.DailySeries[i].Bucket)
		}
	}
	// At least one zero-fill row exists between the two seeded days.
	var zeroRows int
	for _, row := range data.DailySeries {
		if row.CompressedCalls == 0 && row.TokensSaved == 0 {
			zeroRows++
		}
	}
	if zeroRows == 0 {
		t.Fatalf("no zero-fill rows found; DailySeries = %+v", data.DailySeries)
	}
}

// TestGetCompressStatsCompressorHitsCountsRowsNotInvocations verifies the
// SQL-side compressor-hit counter counts ROWS-that-used-the-compressor, not
// total invocations. A row listing "log,log,diff" counts once for log and
// once for diff (not twice for log).
func TestGetCompressStatsCompressorHitsCountsRowsNotInvocations(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)

	now := time.Now().UTC()
	// One row that lists log twice + diff once. Under the old app-side
	// split, log would get 2 hits. Under the new SQL-side LIKE counting,
	// log gets 1 (the row used log, regardless of how many times).
	seedRequestLog(t, db, "c1", now, func(r *model.RequestLog) {
		r.CompressorsApplied = "log,log,diff"
	})

	data := doCompressStats(t, r, ck, "")

	// All four known compressors appear; log=1 (not 2), diff=1, gotest=0, grep=0.
	hitsByName := make(map[string]int64, len(data.CompressorHits))
	for _, ch := range data.CompressorHits {
		hitsByName[ch.Name] = ch.Hits
	}
	if hitsByName["log"] != 1 {
		t.Fatalf("log hits = %d, want 1 (row-count, not invocation-count)", hitsByName["log"])
	}
	if hitsByName["diff"] != 1 {
		t.Fatalf("diff hits = %d, want 1", hitsByName["diff"])
	}
	if hitsByName["gotest"] != 0 {
		t.Fatalf("gotest hits = %d, want 0", hitsByName["gotest"])
	}
	if hitsByName["grep"] != 0 {
		t.Fatalf("grep hits = %d, want 0", hitsByName["grep"])
	}
}

// === Filter pin (per-entity scope) =======================================

// TestAnalyticsReportFilterPin asserts that pinning one dimension in the
// shared analytics filter scopes EVERY aggregate (overview + time + model +
// provider + caller) down to that entity. The per-entity cost detail pages
// rely on this contract without any new backend code: parseAnalyticsFilter
// builds one repository.RequestLogFilter and hands the same filter to every
// aggregation path, so a pin on api_key_id / model_name / provider_id must
// shrink all five aggregates consistently. If any path stops respecting the
// shared filter this test fails before the frontend can rely on the premise.
func TestAnalyticsReportFilterPin(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)

	// Seed two keys + two providers and capture the assigned IDs so the
	// filter query strings use the real values (SQLite picks IDs; we don't
	// hard-code 1/2). Two rows with disjoint (key, model, provider) triples
	// and distinct cost so a pin that leaks the other entity is detectable.
	key1, _ := seedAPIKey(t, db, "alice")
	key2, _ := seedAPIKey(t, db, "bob")
	prov1 := seedProvider(t, db, "openai-main")
	prov2 := seedProvider(t, db, "anthropic-main")

	now := time.Now().UTC()
	seedRequestLog(t, db, "pin-a", now, func(r *model.RequestLog) {
		r.APIKeyID = &key1
		r.ModelName = "modelA"
		r.ProviderID = &prov1
		r.CostMicros = 1000
		r.CostKnown = true
	})
	seedRequestLog(t, db, "pin-b", now, func(r *model.RequestLog) {
		r.APIKeyID = &key2
		r.ModelName = "modelB"
		r.ProviderID = &prov2
		r.CostMicros = 2000
		r.CostKnown = true
	})

	// --- pin api_key_id=key1 → model report must contain modelA only ---
	w, _ := doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=model&api_key_id="+strconv.FormatUint(uint64(key1), 10), nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var modelEnv struct {
		Data struct {
			Rows []struct {
				ModelName string `json:"model_name"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &modelEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(modelEnv.Data.Rows) != 1 || modelEnv.Data.Rows[0].ModelName != "modelA" {
		t.Fatalf("pin api_key_id=key1 model rows = %+v, want only modelA", modelEnv.Data.Rows)
	}

	// --- pin model_name=modelB → caller report must contain key2 only ---
	w, _ = doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=caller&model_name=modelB", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var callerEnv struct {
		Data struct {
			Rows []struct {
				APIKeyID *uint `json:"api_key_id"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &callerEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(callerEnv.Data.Rows) != 1 ||
		callerEnv.Data.Rows[0].APIKeyID == nil ||
		*callerEnv.Data.Rows[0].APIKeyID != key2 {
		t.Fatalf("pin model_name=modelB caller rows = %+v, want only key2", callerEnv.Data.Rows)
	}

	// --- pin provider_id=prov2 → model report must contain modelB only ---
	w, _ = doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=model&provider_id="+strconv.FormatUint(uint64(prov2), 10), nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var modelEnv2 struct {
		Data struct {
			Rows []struct {
				ModelName string `json:"model_name"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &modelEnv2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(modelEnv2.Data.Rows) != 1 || modelEnv2.Data.Rows[0].ModelName != "modelB" {
		t.Fatalf("pin provider_id=prov2 model rows = %+v, want only modelB", modelEnv2.Data.Rows)
	}

	// --- pin provider_id=prov2 → provider report must contain prov2 only ---
	w, _ = doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=provider&provider_id="+strconv.FormatUint(uint64(prov2), 10), nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var provEnv struct {
		Data struct {
			Rows []struct {
				ProviderID *uint `json:"provider_id"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &provEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(provEnv.Data.Rows) != 1 ||
		provEnv.Data.Rows[0].ProviderID == nil ||
		*provEnv.Data.Rows[0].ProviderID != prov2 {
		t.Fatalf("pin provider_id=prov2 provider rows = %+v, want only prov2", provEnv.Data.Rows)
	}

	// --- pin api_key_id=key1 → time report must total exactly 1 call ---
	// Gap-fill may produce many day buckets, but the SUM of calls across all
	// buckets must equal the count of matching rows (1) — that's the
	// per-entity scope contract for the time dimension.
	w, _ = doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=time&bucket=day&api_key_id="+strconv.FormatUint(uint64(key1), 10), nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var timeEnv struct {
		Data struct {
			Rows []struct {
				Bucket string `json:"bucket"`
				Calls  int64  `json:"calls"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &timeEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var timeCalls int64
	for _, row := range timeEnv.Data.Rows {
		timeCalls += row.Calls
	}
	if timeCalls != 1 {
		t.Fatalf("pin api_key_id=key1 time-dimension total calls = %d, want 1", timeCalls)
	}

	// --- pin model_name=modelA → overview cost must equal modelA's cost only ---
	w, _ = doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/overview?model_name=modelA", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var ovEnv struct {
		Data struct {
			TotalCalls int64 `json:"total_calls"`
			CostMicros int64 `json:"cost_micros"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ovEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ovEnv.Data.TotalCalls != 1 || ovEnv.Data.CostMicros != 1000 {
		t.Fatalf("pin model_name=modelA overview = %+v, want calls=1 cost=1000", ovEnv.Data)
	}

	// --- pin provider_id=prov2 → overview cost must equal modelB's cost only ---
	w, _ = doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/overview?provider_id="+strconv.FormatUint(uint64(prov2), 10), nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var ovEnv2 struct {
		Data struct {
			TotalCalls int64 `json:"total_calls"`
			CostMicros int64 `json:"cost_micros"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ovEnv2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ovEnv2.Data.TotalCalls != 1 || ovEnv2.Data.CostMicros != 2000 {
		t.Fatalf("pin provider_id=prov2 overview = %+v, want calls=1 cost=2000", ovEnv2.Data)
	}
}

// === Helpers =============================================================

// approxEqual compares two floats with an absolute tolerance — fine for the
// success-rate math in these tests (denominators are small, precision is
// not the question under test).
func approxEqual(a, b, tol float64) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// The caller ranking leads with spend, not volume: a quiet key that costs
// more sorts above a chatty cheap one. Call-count ordering is a client-side
// toggle on the table, not a server contract.
func TestCallerReportRanksBySpendNotVolume(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	cheap, _ := seedAPIKey(t, db, "chatty-cheap")
	pricey, _ := seedAPIKey(t, db, "pricey-quiet")
	for i := 0; i < 3; i++ {
		seedRequestLog(t, db, fmt.Sprintf("cheap-%d", i), now, func(rl *model.RequestLog) {
			rl.APIKeyID = &cheap
			rl.StatusCode = 200
			rl.CostMicros = 1
			rl.CostKnown = true
		})
	}
	seedRequestLog(t, db, "pricey-1", now, func(rl *model.RequestLog) {
		rl.APIKeyID = &pricey
		rl.StatusCode = 200
		rl.CostMicros = 1000
		rl.CostKnown = true
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=caller", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				Username string `json:"username"`
				Calls    int64  `json:"calls"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Rows) < 2 {
		t.Fatalf("rows = %d, want at least 2", len(env.Data.Rows))
	}
	if env.Data.Rows[0].Username != "pricey-quiet" {
		t.Fatalf("first row = %q (calls=%d), want pricey-quiet on top despite fewer calls",
			env.Data.Rows[0].Username, env.Data.Rows[0].Calls)
	}
}

// Failovers are charged to the provider that was switched AWAY from, never
// the one that rescued the request; key rotation within one provider is not
// a failover; and a provider every request failed away from still gets a
// report row — it served nothing, which is exactly why it must be visible.
func TestProviderReportChargesFailoversToTheProviderThatFailed(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	flaky := &model.Provider{Name: "flaky", BaseURL: "http://f", DestinationVersion: 1}
	rescuer := &model.Provider{Name: "rescuer", BaseURL: "http://r", DestinationVersion: 1}
	for _, p := range []*model.Provider{flaky, rescuer} {
		if err := db.Create(p).Error; err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	detail := fmt.Sprintf(`[{"provider_id":%d,"outcome":"server_error"},{"provider_id":%d,"outcome":"success"}]`, flaky.ID, rescuer.ID)
	seedRequestLog(t, db, "failover-1", now, func(rl *model.RequestLog) {
		rl.ProviderID = &rescuer.ID
		rl.StatusCode = 200
		rl.Attempts = 2
		rl.AttemptsDetail = &detail
	})
	// Key rotation on one provider: two attempts, same provider — no failover.
	rotation := fmt.Sprintf(`[{"provider_id":%d,"outcome":"auth_failed"},{"provider_id":%d,"outcome":"success"}]`, rescuer.ID, rescuer.ID)
	seedRequestLog(t, db, "rotation-1", now, func(rl *model.RequestLog) {
		rl.ProviderID = &rescuer.ID
		rl.StatusCode = 200
		rl.Attempts = 2
		rl.AttemptsDetail = &rotation
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=provider&with_failovers=1", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				ProviderName string `json:"provider_name"`
				Calls        int64  `json:"calls"`
				Failovers    int64  `json:"failovers"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byName := map[string]struct {
		Calls     int64
		Failovers int64
	}{}
	for _, row := range env.Data.Rows {
		byName[row.ProviderName] = struct {
			Calls     int64
			Failovers int64
		}{row.Calls, row.Failovers}
	}
	f, ok := byName["flaky"]
	if !ok {
		t.Fatalf("flaky served nothing but must still have a row; rows: %+v", env.Data.Rows)
	}
	if f.Calls != 0 || f.Failovers != 1 {
		t.Fatalf("flaky = %+v, want calls 0 / failovers 1", f)
	}
	rsc := byName["rescuer"]
	if rsc.Calls != 2 || rsc.Failovers != 0 {
		t.Fatalf("rescuer = %+v, want calls 2 / failovers 0 (rescuing and key rotation are not its fault)", rsc)
	}
}

// A failover is a per-attempt event; the provider filter describes where a
// request ENDED. Filtering the report to one provider must neither hide
// that provider's own failovers (they live in rows that ended elsewhere)
// nor synthesize rows for providers the filter excluded.
func TestProviderReportFailoversSurviveTheProviderFilter(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	flaky := &model.Provider{Name: "flaky", BaseURL: "http://f", DestinationVersion: 1}
	rescuer := &model.Provider{Name: "rescuer", BaseURL: "http://r", DestinationVersion: 1}
	for _, p := range []*model.Provider{flaky, rescuer} {
		if err := db.Create(p).Error; err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	detail := fmt.Sprintf(`[{"provider_id":%d,"outcome":"server_error"},{"provider_id":%d,"outcome":"success"}]`, flaky.ID, rescuer.ID)
	seedRequestLog(t, db, "filtered-failover", now, func(rl *model.RequestLog) {
		rl.ProviderID = &rescuer.ID
		rl.StatusCode = 200
		rl.Attempts = 2
		rl.AttemptsDetail = &detail
	})

	parse := func(w *httptest.ResponseRecorder) map[string][2]int64 {
		t.Helper()
		var env struct {
			Data struct {
				Rows []struct {
					ProviderName string `json:"provider_name"`
					Calls        int64  `json:"calls"`
					Failovers    int64  `json:"failovers"`
				} `json:"rows"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out := map[string][2]int64{}
		for _, row := range env.Data.Rows {
			out[row.ProviderName] = [2]int64{row.Calls, row.Failovers}
		}
		return out
	}

	// Filtered to flaky: its own failover must show, on a synthesized row.
	w, _ := doJSON(t, r, http.MethodGet,
		fmt.Sprintf("/api/admin/analytics/report?dimension=provider&with_failovers=1&provider_id=%d", flaky.ID), nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	rows := parse(w)
	if got := rows["flaky"]; got != [2]int64{0, 1} {
		t.Fatalf("flaky under its own filter = %v, want calls 0 / failovers 1", got)
	}
	if _, ok := rows["rescuer"]; ok {
		t.Fatal("the filter excluded rescuer; it must not be synthesized into the report")
	}

	// Filtered to rescuer: no phantom flaky row, and rescuing is not charged.
	w, _ = doJSON(t, r, http.MethodGet,
		fmt.Sprintf("/api/admin/analytics/report?dimension=provider&with_failovers=1&provider_id=%d", rescuer.ID), nil, ck)
	rows = parse(w)
	if got := rows["rescuer"]; got != [2]int64{1, 0} {
		t.Fatalf("rescuer under its own filter = %v, want calls 1 / failovers 0", got)
	}
	if _, ok := rows["flaky"]; ok {
		t.Fatal("the filter excluded flaky; it must not be synthesized into the report")
	}
}

// A failover is counted whatever the request's final status class: a chain
// that ultimately failed still charged the provider that failed first, even
// when the report itself is filtered to a class that excludes that request.
func TestProviderReportFailoversIgnoreTheStatusFilter(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	flaky := &model.Provider{Name: "flaky", BaseURL: "http://f", DestinationVersion: 1}
	alsoBad := &model.Provider{Name: "also-bad", BaseURL: "http://b", DestinationVersion: 1}
	for _, p := range []*model.Provider{flaky, alsoBad} {
		if err := db.Create(p).Error; err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	// Both attempts failed: the request's final class is "failed", which a
	// status=success report excludes — the switch must still be counted.
	detail := fmt.Sprintf(`[{"provider_id":%d,"outcome":"server_error"},{"provider_id":%d,"outcome":"server_error"}]`, flaky.ID, alsoBad.ID)
	seedRequestLog(t, db, "all-failed", now, func(rl *model.RequestLog) {
		rl.ProviderID = &alsoBad.ID
		rl.StatusCode = 502
		rl.Attempts = 2
		rl.AttemptsDetail = &detail
	})

	w, _ := doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=provider&with_failovers=1&status=success", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				ProviderName string `json:"provider_name"`
				Failovers    int64  `json:"failovers"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, row := range env.Data.Rows {
		if row.ProviderName == "flaky" && row.Failovers == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("flaky's failover vanished under status=success; rows: %+v", env.Data.Rows)
	}
}

// One malformed attempts_detail row must cost only its own contribution,
// never the report: the endpoint stays 200 and the valid rows still count.
func TestProviderReportSurvivesMalformedAttemptDetail(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	flaky := &model.Provider{Name: "flaky", BaseURL: "http://f", DestinationVersion: 1}
	rescuer := &model.Provider{Name: "rescuer", BaseURL: "http://r", DestinationVersion: 1}
	for _, p := range []*model.Provider{flaky, rescuer} {
		if err := db.Create(p).Error; err != nil {
			t.Fatalf("seed provider: %v", err)
		}
	}
	valid := fmt.Sprintf(`[{"provider_id":%d,"outcome":"server_error"},{"provider_id":%d,"outcome":"success"}]`, flaky.ID, rescuer.ID)
	seedRequestLog(t, db, "valid-switch", now, func(rl *model.RequestLog) {
		rl.ProviderID = &rescuer.ID
		rl.StatusCode = 200
		rl.Attempts = 2
		rl.AttemptsDetail = &valid
	})
	broken := `{"this is": "not an attempt array"`
	seedRequestLog(t, db, "corrupted", now, func(rl *model.RequestLog) {
		rl.ProviderID = &rescuer.ID
		rl.StatusCode = 200
		rl.Attempts = 2
		rl.AttemptsDetail = &broken
	})

	w, _ := doJSON(t, r, http.MethodGet,
		"/api/admin/analytics/report?dimension=provider&with_failovers=1", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 despite the malformed row, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				ProviderName string `json:"provider_name"`
				Failovers    int64  `json:"failovers"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, row := range env.Data.Rows {
		if row.ProviderName == "flaky" && row.Failovers == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the valid switch must still count beside the malformed row; rows: %+v", env.Data.Rows)
	}
}

// TestGetAnalyticsReportByUserGroupsAcrossKeys: dimension=user must merge
// every key an account owns into one row (usage per person, not per
// credential), resolve the account's username, and omit keyless
// auth-rejected traffic entirely — this report answers "who spent what",
// and traffic that never reached an account has no account to answer for.
// The u3 seed below is that traffic: it must not produce a row.
func TestGetAnalyticsReportByUserGroupsAcrossKeys(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	key1, userID := seedAPIKey(t, db, "carol")
	// A second key owned by the SAME account — its traffic must fold into
	// carol's single row.
	now := time.Now().UTC()
	key2 := model.APIKey{KeyHash: "test-hash-carol-2", KeyPrefix: "sk-x2-", UserID: userID,
		Status: model.APIKeyStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&key2).Error; err != nil {
		t.Fatalf("seed second key: %v", err)
	}
	seedRequestLog(t, db, "u1", now, func(l *model.RequestLog) {
		l.APIKeyID = &key1
		l.UserID = &userID
		l.StatusCode = 200
		l.InputTokens = 10
		l.CostMicros = 5
		l.CostKnown = true
	})
	seedRequestLog(t, db, "u2", now, func(l *model.RequestLog) {
		l.APIKeyID = &key2.ID
		l.UserID = &userID
		l.StatusCode = 200
		l.InputTokens = 7
		l.CostMicros = 2
		l.CostKnown = true
	})
	seedRequestLog(t, db, "u3", now, func(l *model.RequestLog) {
		l.StatusCode = 401 // keyless, accountless reject
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=user", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				UserID      *uint  `json:"user_id"`
				Username    string `json:"username"`
				Calls       int64  `json:"calls"`
				InputTokens int64  `json:"input_tokens"`
				CostMicros  int64  `json:"cost_micros"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Rows) != 1 {
		t.Fatalf("expected carol alone (u3 is accountless), got %d rows: %s", len(env.Data.Rows), w.Body.String())
	}
	var carol bool
	for _, row := range env.Data.Rows {
		if row.UserID == nil {
			t.Fatalf("accountless traffic must not produce a row, got %+v", row)
		}
		if *row.UserID == userID {
			carol = true
			if row.Username != "carol" || row.Calls != 2 || row.InputTokens != 17 || row.CostMicros != 7 {
				t.Fatalf("carol row must merge both keys' traffic, got %+v", row)
			}
		}
	}
	if !carol {
		t.Fatalf("missing carol's row: %s", w.Body.String())
	}
}

// === Concise-output projection handler ===================================

// TestGetConciseOutputProjectionReturnsPricedVolumeAndTotals drives the
// endpoint through the real route table and decodes the JSON envelope by
// wire field name, so a rename or a mis-wired struct field fails here rather
// than silently reaching the console as an em-dash.
//
// Every number in the fixture is deliberately distinct: unit price is not 1,
// and the window mixes priced with heavy unpriced traffic, so total tokens,
// priced tokens and spend can never coincide. Swapping any two of them —
// output_tokens for priced_output_tokens in particular, which is what the
// coverage ratio divides — turns this red.
func TestGetConciseOutputProjectionReturnsPricedVolumeAndTotals(t *testing.T) {
	r, db, ck := newAnalyticsFixture(t)
	now := time.Now().UTC()
	providerID := seedProvider(t, db, "concise-provider")
	m := model.Model{Name: "concise-model", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover}
	if err := db.Create(&m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	cand := model.ModelCandidate{ModelID: m.ID, ProviderID: providerID,
		ProviderModelName: "upstream/concise-model", OutputPrice: 4.0}
	if err := db.Create(&cand).Error; err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	// 250K priced output tokens at 4 CNY/M = 1,000,000 micros of spend.
	seedRequestLog(t, db, "cp1", now, func(l *model.RequestLog) {
		l.ModelName = "concise-model"
		l.ProviderID = &providerID
		l.StatusCode = 200
		l.OutputTokens = 250_000
	})
	// Same model, but never routed — unpriced, and far heavier, so a
	// request-share coverage would read 50% where the token share is 20%.
	seedRequestLog(t, db, "cu1", now, func(l *model.RequestLog) {
		l.ModelName = "concise-model"
		l.ProviderID = nil
		l.StatusCode = 200
		l.OutputTokens = 1_000_000
	})

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/concise-output-projection", nil, ck)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	// Decoded by wire name rather than into the service struct, so a changed
	// json tag is caught too.
	var env struct {
		Code int `json:"code"`
		Data struct {
			OutputSpendMicros  int64 `json:"output_spend_micros"`
			OutputRows         int64 `json:"output_rows"`
			OutputTokens       int64 `json:"output_tokens"`
			PricedRows         int64 `json:"priced_rows"`
			PricedOutputTokens int64 `json:"priced_output_tokens"`
			SavedCostMicros    int64 `json:"projected_saved_cost_micros"`
			SavedOutputTokens  int64 `json:"projected_saved_output_tokens"`
			// Deprecated wire field pre-upgrade tabs still read; a pointer so
			// its absent-vs-zero contract stays observable here.
			LegacyPerMillion *int64  `json:"projected_savings_per_million_tokens_micros"`
			Coefficient      float64 `json:"coefficient"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := env.Data
	if d.OutputRows != 2 || d.PricedRows != 1 {
		t.Errorf("rows = %d total / %d priced, want 2/1", d.OutputRows, d.PricedRows)
	}
	if d.OutputTokens != 1_250_000 {
		t.Errorf("output_tokens = %d, want 1250000 (priced + unpriced)", d.OutputTokens)
	}
	if d.PricedOutputTokens != 250_000 {
		t.Errorf("priced_output_tokens = %d, want 250000", d.PricedOutputTokens)
	}
	if d.OutputSpendMicros != 1_000_000 {
		t.Errorf("output_spend_micros = %d, want 1000000 (250K x 4 CNY/M)", d.OutputSpendMicros)
	}
	// Window spend (1,000,000 micros) and priced tokens (250K), each scaled
	// by the coefficient.
	wantCost := int64(math.Round(1_000_000 * analytics.ConciseOutputCoefficient))
	if d.SavedCostMicros != wantCost {
		t.Errorf("projected_saved_cost_micros = %d, want %d", d.SavedCostMicros, wantCost)
	}
	wantTokens := int64(math.Round(250_000 * analytics.ConciseOutputCoefficient))
	if d.SavedOutputTokens != wantTokens {
		t.Errorf("projected_saved_output_tokens = %d, want %d", d.SavedOutputTokens, wantTokens)
	}
	// The deprecated per-million rate must still reach pre-upgrade tabs:
	// 4 CNY per million output tokens x the coefficient.
	wantLegacy := int64(math.Round(4 * analytics.ConciseOutputCoefficient * 1e6))
	if d.LegacyPerMillion == nil {
		t.Fatalf("projected_savings_per_million_tokens_micros absent, want %d", wantLegacy)
	}
	if *d.LegacyPerMillion != wantLegacy {
		t.Errorf("projected_savings_per_million_tokens_micros = %d, want %d", *d.LegacyPerMillion, wantLegacy)
	}
	if d.Coefficient != analytics.ConciseOutputCoefficient {
		t.Errorf("coefficient = %v, want %v", d.Coefficient, analytics.ConciseOutputCoefficient)
	}
}
