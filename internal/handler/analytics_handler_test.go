// Package handler tests for M6.1 §6.7 analytics endpoints. Exercises the
// full HTTP → service → repository stack against a migrated SQLite DB;
// repository-only tests for the time-bucket walk live alongside (they'd
// require an awkward HTTP shim to drive a fixed *time.Location otherwise).
package handler

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// newAnalyticsTestRouter wires up a Gin engine with the three analytics
// routes mounted under /api/admin/analytics — the same paths
// internal/router/router.go would use, duplicated here so this test file
// doesn't have to touch router.go (out of scope per the task boundary).
func newAnalyticsTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	svc := service.NewAnalyticsService(db)

	r := gin.New()
	admin := r.Group("/api/admin")
	admin.GET("/analytics/overview", GetAnalyticsOverview(svc))
	admin.GET("/analytics/report", GetAnalyticsReport(svc))
	admin.GET("/analytics/export", ExportAnalyticsCSV(svc))
	return r, db
}

// analyticsStrPtr is a tiny *string helper local to this file (the existing
// seedRequestLog's mutator-callback pattern keeps the test body explicit at
// the cost of needing a closure-captured pointer for fail_reason).
func analyticsStrPtr(s string) *string { return &s }

// === Overview handler ====================================================

func TestGetAnalyticsOverviewAggregatesSeededRows(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
	now := time.Now().UTC()
	// 2 successes (cost-known), 1 server failure (cost-unknown),
	// 1 caller-cancel (cost-unknown). Verify each metric below.
	seedRequestLog(t, db, "r1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 100
		r.OutputTokens = 50
		r.CostMicros = 10
		r.CostKnown = true
		r.DurationMs = 500
	})
	seedRequestLog(t, db, "r2", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.InputTokens = 200
		r.OutputTokens = 100
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

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int                 `json:"code"`
		Data service.OverviewRow `json:"data"`
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
}

func TestGetAnalyticsOverviewRespectsTimeRange(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
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
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview?start="+start+"&end="+end, nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data service.OverviewRow `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.TotalCalls != 1 {
		t.Fatalf("TotalCalls = %d, want 1 (only the recent row)", env.Data.TotalCalls)
	}
}

func TestGetAnalyticsOverviewRejectsBadStartTime(t *testing.T) {
	r, _ := newAnalyticsTestRouter(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview?start=not-a-time", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetAnalyticsOverviewRejectsBadStatus(t *testing.T) {
	r, _ := newAnalyticsTestRouter(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/overview?status=bogus", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// === Report handlers =====================================================

func TestGetAnalyticsReportByModelGroupsAndOrdersByCalls(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
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

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=model", nil, nil)
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
	r, db := newAnalyticsTestRouter(t)
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

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=model", nil, nil)
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
	r, db := newAnalyticsTestRouter(t)
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

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=provider", nil, nil)
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

func TestGetAnalyticsReportByCallerResolvesOwnerLabels(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
	key := &model.APIKey{OwnerLabel: "alice", KeyHash: "x", KeyPrefix: "sk-", Status: model.APIKeyStatusActive}
	if err := db.Create(key).Error; err != nil {
		t.Fatalf("seed api_key: %v", err)
	}
	now := time.Now().UTC()
	seedRequestLog(t, db, "k1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.APIKeyID = &key.ID
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

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=caller", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Rows []struct {
				APIKeyID   *uint  `json:"api_key_id"`
				OwnerLabel string `json:"owner_label"`
				Calls      int64  `json:"calls"`
			} `json:"rows"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(env.Data.Rows))
	}
	var labeled *struct {
		APIKeyID   *uint  `json:"api_key_id"`
		OwnerLabel string `json:"owner_label"`
		Calls      int64  `json:"calls"`
	}
	for i := range env.Data.Rows {
		if env.Data.Rows[i].APIKeyID != nil {
			labeled = &env.Data.Rows[i]
		}
	}
	if labeled == nil {
		t.Fatalf("no non-NULL api_key bucket in %+v", env.Data.Rows)
	}
	if labeled.OwnerLabel != "alice" {
		t.Fatalf("OwnerLabel = %q, want alice", labeled.OwnerLabel)
	}
}

func TestGetAnalyticsReportDefaultsDimensionToModel(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
	now := time.Now().UTC()
	seedRequestLog(t, db, "d1", now, func(r *model.RequestLog) {
		r.ModelName = "gpt-4"
		r.StatusCode = 200
		r.CostKnown = true
		r.CostMicros = 1
	})

	// No ?dimension= on the URL at all.
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report", nil, nil)
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
	if env.Data.Dimension != service.DimensionModel {
		t.Fatalf("default dimension = %q, want %q", env.Data.Dimension, service.DimensionModel)
	}
}

func TestGetAnalyticsReportRejectsUnknownDimension(t *testing.T) {
	r, _ := newAnalyticsTestRouter(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=banana", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetAnalyticsReportRejectsUnknownBucket(t *testing.T) {
	r, _ := newAnalyticsTestRouter(t)
	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=time&bucket=century", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// === Time dimension ======================================================

func TestGetAnalyticsReportByTimeDayBucketFillsGaps(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
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

	w, _ := doJSON(t, r, http.MethodGet, "/api/admin/analytics/report?dimension=time&bucket=day", nil, nil)
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

	rows, err := repository.AggregateByTime(db, f, loc, repository.TimeBucketDay)
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
	_, err := repository.AggregateByTime(db, &repository.RequestLogFilter{}, time.UTC, "century")
	if !errors.Is(err, repository.ErrInvalidBucket) {
		t.Fatalf("expected ErrInvalidBucket, got %v", err)
	}
}

// === CSV export ==========================================================

func TestExportAnalyticsCSVWritesBOMAndHeadersAndRows(t *testing.T) {
	r, db := newAnalyticsTestRouter(t)
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
	wantHeader := []string{"model_name", "calls", "success_rate", "input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens", "cost_micros", "unknown_cost_calls"}
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
	r, _ := newAnalyticsTestRouter(t)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/analytics/export?dimension=banana", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
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
