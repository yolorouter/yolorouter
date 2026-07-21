package handler

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// newDashboardTestRouter wires up just the dashboard endpoint against a
// fresh migrated SQLite DB, mirroring the pattern in
// provider_handler_test.go / model_handler_test.go. Uses a REAL
// DashboardService (not a fake) — the dashboard's logic is "compose five
// repo calls", which isn't worth faking; the test wants to verify the full
// SQL→service→handler→envelope chain end-to-end.
func newDashboardTestRouter(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	svc := service.NewDashboardService(db)
	r := gin.New()
	r.GET("/api/admin/dashboard", GetDashboard(svc))
	return r, db
}

// todayMetrics mirrors the repository.TodayMetricsDTO JSON shape so the
// handler test can decode it without importing the repository types into
// the assertion. Keeping a local anonymous-struct copy also documents what
// the handler test actually expects the API contract to be.
type todayMetrics struct {
	Calls            int64   `json:"calls"`
	TotalCostMicros  int64   `json:"total_cost_micros"`
	SuccessRate      float64 `json:"success_rate"`
	UnknownCostCalls int64   `json:"unknown_cost_calls"`
}

type trendPoint struct {
	Date       string `json:"date"`
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

type topCaller struct {
	APIKeyID   uint   `json:"api_key_id"`
	OwnerLabel string `json:"owner_label"`
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

type recentFailure struct {
	RequestID  string  `json:"request_id"`
	APIKeyID   *uint   `json:"api_key_id"`
	ModelName  string  `json:"model_name"`
	ProviderID *uint   `json:"provider_id"`
	StatusCode int     `json:"status_code"`
	FailReason *string `json:"fail_reason"`
	IsStream   bool    `json:"is_stream"`
	DurationMs int64   `json:"duration_ms"`
	CreatedAt  string  `json:"created_at"`
}

type upstreamStatus struct {
	AvailableProviders int64 `json:"available_providers"`
	AbnormalKeys       int64 `json:"abnormal_keys"`
	UnavailableModels  int64 `json:"unavailable_models"`
}

type dashboardBody struct {
	Today          todayMetrics    `json:"today"`
	Trend          []trendPoint    `json:"trend"`
	TopCallers     []topCaller     `json:"top_callers"`
	RecentFailures []recentFailure `json:"recent_failures"`
	UpstreamStatus upstreamStatus  `json:"upstream_status"`
}

// insertRequestLog is a thin helper around model.RequestLog construction.
// Default values produce a clean success row at the given timestamp; call
// sites override the fields they care about via the functional options
// pattern (just pass a func to mutate after construction — kept simple here
// since each test only needs one or two variations).
func insertRequestLog(t *testing.T, db *gorm.DB, ts time.Time, mut func(*model.RequestLog)) {
	t.Helper()
	r := model.RequestLog{
		RequestID:    "req-" + ts.Format("20060102150405.000000000"),
		APIKeyID:     nil,
		ModelName:    "gpt-4o-mini",
		IsStream:     false,
		StatusCode:   200,
		InputTokens:  10,
		OutputTokens: 20,
		CostMicros:   100,
		CostKnown:    true,
		Attempts:     1,
		DurationMs:   42,
		CreatedAt:    ts.UTC(),
	}
	if mut != nil {
		mut(&r)
	}
	if err := repository.CreateRequestLog(db, &r); err != nil {
		t.Fatalf("insertRequestLog: %v", err)
	}
}

func TestGetDashboardReturnsZeroEnvelopeOnFreshDB(t *testing.T) {
	r, _ := newDashboardTestRouter(t)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.Success {
		t.Fatalf("expected code %d, got %d", errcode.Success, env.Code)
	}

	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal dashboard body: %v", err)
	}
	if body.Today.Calls != 0 || body.Today.TotalCostMicros != 0 ||
		body.Today.SuccessRate != 0 || body.Today.UnknownCostCalls != 0 {
		t.Fatalf("expected all-zero today section, got %+v", body.Today)
	}
	if len(body.Trend) != service.DashboardTrendDays {
		t.Fatalf("expected %d trend points, got %d", service.DashboardTrendDays, len(body.Trend))
	}
	// Trend must be oldest-first, ending today, contiguous days.
	wantToday := time.Now().In(time.Local).Format("2006-01-02")
	last := body.Trend[len(body.Trend)-1]
	if last.Date != wantToday {
		t.Fatalf("expected last trend point to be today %q, got %q", wantToday, last.Date)
	}
	for i := 1; i < len(body.Trend); i++ {
		prev, _ := time.Parse("2006-01-02", body.Trend[i-1].Date)
		cur, _ := time.Parse("2006-01-02", body.Trend[i].Date)
		if !cur.After(prev) {
			t.Fatalf("trend must be ascending by date, got %s before %s", body.Trend[i-1].Date, body.Trend[i].Date)
		}
		if cur.Sub(prev) != 24*time.Hour {
			t.Fatalf("trend days must be contiguous, gap between %s and %s", body.Trend[i-1].Date, body.Trend[i].Date)
		}
	}
	if len(body.TopCallers) != 0 {
		t.Fatalf("expected no top callers on fresh DB, got %d", len(body.TopCallers))
	}
	if len(body.RecentFailures) != 0 {
		t.Fatalf("expected no recent failures on fresh DB, got %d", len(body.RecentFailures))
	}
	if body.UpstreamStatus != (upstreamStatus{}) {
		t.Fatalf("expected zero upstream status, got %+v", body.UpstreamStatus)
	}
}

func TestGetDashboardTodayMetricsCountRowsInLocalDay(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	loc := time.Local
	start, end := repository.TodayBounds(loc)
	now := time.Now().In(loc)

	// Two clean successes (200, cost_known=true), one failure (500, 0 cost),
	// one unknown-cost success (200, cost_known=false, cost_micros=0), one
	// caller-cancel (499 — counts toward total but NOT success rate).
	insertRequestLog(t, db, now.Add(-5*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 100
		r.CostKnown = true
	})
	insertRequestLog(t, db, now.Add(-4*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 200
		r.CostKnown = true
	})
	insertRequestLog(t, db, now.Add(-3*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 500
		r.CostMicros = 0
		r.CostKnown = true
	})
	insertRequestLog(t, db, now.Add(-2*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 0
		r.CostKnown = false
	})
	insertRequestLog(t, db, now.Add(-1*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 499
		r.CostMicros = 0
		r.CostKnown = true
	})
	// One row just before today's window — must NOT count toward today.
	insertRequestLog(t, db, start.Add(-time.Second), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 9999
		r.CostKnown = true
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 5 rows inside the window: 2 clean succ + 1 fail + 1 unknown-cost succ
	// + 1 cancel. The row before the window doesn't count.
	if body.Today.Calls != 5 {
		t.Fatalf("Calls: want 5, got %d", body.Today.Calls)
	}
	// Known cost sum: 100 + 200 + 0 (unknown) + 0 (fail) + 0 (cancel) = 300.
	if body.Today.TotalCostMicros != 300 {
		t.Fatalf("TotalCostMicros: want 300, got %d", body.Today.TotalCostMicros)
	}
	// Success rate: 3 successes (clean succ + unknown-cost succ) / 4 ended
	// (succ+fail+partial+rejected, cancels excluded) = 0.75.
	if got := body.Today.SuccessRate; got < 0.749 || got > 0.751 {
		t.Fatalf("SuccessRate: want 0.75, got %v", got)
	}
	if body.Today.UnknownCostCalls != 1 {
		t.Fatalf("UnknownCostCalls: want 1, got %d", body.Today.UnknownCostCalls)
	}

	// Sanity: end-exclusive window means a row at exactly `end` would fall
	// into tomorrow; we don't need to test that explicitly since the
	// start.Add(-time.Second) row above already exercises the start side.
	_ = end
}

func TestGetDashboardTrendIncludesTodayRowOnly(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	loc := time.Local
	now := time.Now().In(loc)

	// Two rows today, nothing on prior days.
	insertRequestLog(t, db, now.Add(-1*time.Hour), func(r *model.RequestLog) {
		r.CostMicros = 150
		r.CostKnown = true
	})
	insertRequestLog(t, db, now.Add(-30*time.Minute), func(r *model.RequestLog) {
		r.CostMicros = 50
		r.CostKnown = true
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Trend) != service.DashboardTrendDays {
		t.Fatalf("Trend len: want %d, got %d", service.DashboardTrendDays, len(body.Trend))
	}
	today := body.Trend[len(body.Trend)-1]
	wantToday := now.Format("2006-01-02")
	if today.Date != wantToday {
		t.Fatalf("today.Date: want %q, got %q", wantToday, today.Date)
	}
	if today.Calls != 2 || today.CostMicros != 200 {
		t.Fatalf("today trend: want {calls=2 cost=200}, got %+v", today)
	}
	// Every earlier day must be zero (no rows inserted there).
	for i := 0; i < len(body.Trend)-1; i++ {
		if body.Trend[i].Calls != 0 || body.Trend[i].CostMicros != 0 {
			t.Fatalf("expected zero trend point at index %d (%s), got %+v",
				i, body.Trend[i].Date, body.Trend[i])
		}
	}

	// Insert a row 3 days ago and re-query: that day's point should now be
	// non-zero while the other pre-today days stay zero.
	prev := now.AddDate(0, 0, -3)
	dayStart, _ := repository.DayBoundsAt(loc, prev)
	insertRequestLog(t, db, dayStart.Add(2*time.Hour), func(r *model.RequestLog) {
		r.CostMicros = 77
		r.CostKnown = true
	})
	w2, env2 := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 on re-query, got %d, body: %s", w2.Code, w2.Body.String())
	}
	if err := json.Unmarshal(env2.Data, &body); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	wantDate := prev.Format("2006-01-02")
	found := false
	for _, p := range body.Trend {
		if p.Date == wantDate {
			if p.Calls != 1 || p.CostMicros != 77 {
				t.Fatalf("day-3 trend: want {calls=1 cost=77}, got %+v", p)
			}
			found = true
		} else if p.Date != wantToday {
			// Other pre-today days still zero (the today entry was asserted above).
			if p.Date != wantToday && p.Calls != 0 {
				t.Fatalf("day %s: expected zero, got %+v", p.Date, p)
			}
		}
	}
	if !found {
		t.Fatalf("expected trend to include %q", wantDate)
	}
}

func TestGetDashboardTopCallersRankedByCost(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	loc := time.Local
	now := time.Now().In(loc)

	// Create three api_keys. The dashboard's top-callers list should rank
	// them by cost_micros DESC regardless of how many requests each made —
	// one expensive call beats many cheap ones.
	keys := []struct {
		label    string
		costEach int64
		calls    int
	}{
		{label: "big-spender", costEach: 500, calls: 1},
		{label: "mid-spender", costEach: 100, calls: 3},
		{label: "tiny-spender", costEach: 10, calls: 10},
	}
	keyIDs := make([]uint, len(keys))
	for i, k := range keys {
		ak := model.APIKey{
			// key_hash has a UNIQUE constraint — give each row a distinct
			// hash (we never authenticate via this test, so the value just
			// has to be unique, not a real SHA-256 of anything).
			KeyHash:    "test-hash-" + k.label,
			KeyPrefix:  "sk-xx-",
			OwnerLabel: k.label,
			Status:     model.APIKeyStatusActive,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := db.Create(&ak).Error; err != nil {
			t.Fatalf("create api_key %q: %v", k.label, err)
		}
		keyIDs[i] = ak.ID
		for j := 0; j < k.calls; j++ {
			id := ak.ID
			cost := k.costEach
			insertRequestLog(t, db, now.Add(-time.Duration(j+1)*time.Minute), func(r *model.RequestLog) {
				r.APIKeyID = &id
				r.CostMicros = cost
				r.CostKnown = true
			})
		}
	}

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.TopCallers) != 3 {
		t.Fatalf("TopCallers: want 3 entries, got %d", len(body.TopCallers))
	}
	// Rank: big-spender (500) > mid-spender (300) > tiny-spender (100).
	want := []struct {
		label      string
		costMicros int64
		calls      int64
	}{
		{"big-spender", 500, 1},
		{"mid-spender", 300, 3},
		{"tiny-spender", 100, 10},
	}
	for i, w := range want {
		got := body.TopCallers[i]
		if got.OwnerLabel != w.label || got.CostMicros != w.costMicros || got.Calls != w.calls {
			t.Fatalf("TopCallers[%d]: want {label=%s cost=%d calls=%d}, got %+v",
				i, w.label, w.costMicros, w.calls, got)
		}
	}
}

func TestGetDashboardTopCallersExcludesRowsWithoutAPIKey(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	now := time.Now().In(time.Local)

	// A high-cost row with NULL api_key_id (e.g. failed auth) must NOT
	// surface in the top-callers list — there's no caller identity to show.
	ak := model.APIKey{
		KeyHash: "test-hash-real-caller", KeyPrefix: "sk-xx-",
		OwnerLabel: "real-caller", Status: model.APIKeyStatusActive,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&ak).Error; err != nil {
		t.Fatalf("create api_key: %v", err)
	}
	insertRequestLog(t, db, now.Add(-1*time.Minute), func(r *model.RequestLog) {
		r.CostMicros = 99999
		r.CostKnown = true
		r.APIKeyID = nil
	})
	realID := ak.ID
	insertRequestLog(t, db, now.Add(-30*time.Second), func(r *model.RequestLog) {
		r.CostMicros = 5
		r.CostKnown = true
		r.APIKeyID = &realID
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.TopCallers) != 1 || body.TopCallers[0].OwnerLabel != "real-caller" {
		t.Fatalf("TopCallers: expected only [real-caller], got %+v", body.TopCallers)
	}
}

func TestGetDashboardRecentFailuresMixesAllThreeBuckets(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	now := time.Now().In(time.Local)

	// Insert 4 rows, one in each failure shape (failed, partial, rejected)
	// plus one clean success that must NOT appear. Each timestamp is distinct
	// so the order is deterministic.
	reason := "upstream 500"
	insertRequestLog(t, db, now.Add(-4*time.Minute), func(r *model.RequestLog) {
		r.RequestID = "req-success"
		r.StatusCode = 200
		r.FailReason = nil
	})
	insertRequestLog(t, db, now.Add(-3*time.Minute), func(r *model.RequestLog) {
		r.RequestID = "req-failed"
		r.StatusCode = 500
		r.FailReason = &reason
	})
	insertRequestLog(t, db, now.Add(-2*time.Minute), func(r *model.RequestLog) {
		r.RequestID = "req-partial"
		r.StatusCode = 200
		r.FailReason = &reason // 2xx + fail_reason => partial bucket
	})
	insertRequestLog(t, db, now.Add(-1*time.Minute), func(r *model.RequestLog) {
		r.RequestID = "req-rejected"
		r.StatusCode = 429 // rejected bucket
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.RecentFailures) != 3 {
		t.Fatalf("RecentFailures: want 3 (failed+partial+rejected), got %d (%+v)",
			len(body.RecentFailures), body.RecentFailures)
	}
	// Order is newest-first by created_at.
	want := []string{"req-rejected", "req-partial", "req-failed"}
	for i, w := range want {
		if body.RecentFailures[i].RequestID != w {
			t.Fatalf("RecentFailures[%d].RequestID: want %q, got %q",
				i, w, body.RecentFailures[i].RequestID)
		}
	}
	for _, f := range body.RecentFailures {
		if f.RequestID == "req-success" {
			t.Fatalf("clean success must not appear in recent failures: %+v", f)
		}
	}
}

func TestGetDashboardRecentFailuresExcludesCallerCancel(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	now := time.Now().In(time.Local)

	// 499 is a caller abort, NOT a system failure — it must not surface here.
	insertRequestLog(t, db, now.Add(-1*time.Minute), func(r *model.RequestLog) {
		r.RequestID = "req-cancelled"
		r.StatusCode = 499
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.RecentFailures) != 0 {
		t.Fatalf("RecentFailures: expected empty (499 is not a failure), got %+v", body.RecentFailures)
	}
}

func TestGetDashboardUpstreamStatusCountsCorrectly(t *testing.T) {
	r, db := newDashboardTestRouter(t)

	// 2 enabled providers, 1 disabled.
	p1 := model.Provider{Name: "p1", ProviderType: "openai", BaseURL: "https://a.example.com/v1", ManagementStatus: model.ProviderStatusEnabled}
	p2 := model.Provider{Name: "p2", ProviderType: "openai", BaseURL: "https://b.example.com/v1", ManagementStatus: model.ProviderStatusEnabled}
	p3 := model.Provider{Name: "p3", ProviderType: "openai", BaseURL: "https://c.example.com/v1", ManagementStatus: model.ProviderStatusDisabled}
	if err := db.Create(&p1).Error; err != nil {
		t.Fatalf("create p1: %v", err)
	}
	if err := db.Create(&p2).Error; err != nil {
		t.Fatalf("create p2: %v", err)
	}
	if err := db.Create(&p3).Error; err != nil {
		t.Fatalf("create p3: %v", err)
	}

	// ProviderKey: 1 enabled+passed (normal), 1 enabled+untested (abnormal),
	// 1 disabled+untested (NOT abnormal — already taken offline).
	keys := []model.ProviderKey{
		{ProviderID: p1.ID, Label: "k1", SortOrder: 1, TestModel: "m",
			ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusPassed},
		{ProviderID: p1.ID, Label: "k2", SortOrder: 2, TestModel: "m",
			ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusUntested},
		{ProviderID: p2.ID, Label: "k3", SortOrder: 1, TestModel: "m",
			ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusUntested},
	}
	for i := range keys {
		if err := db.Create(&keys[i]).Error; err != nil {
			t.Fatalf("create provider_key %d: %v", i, err)
		}
	}

	// Model: 1 enabled, 2 disabled.
	m1 := model.Model{Name: "m1", ManagementStatus: model.ModelStatusEnabled}
	m2 := model.Model{Name: "m2", ManagementStatus: model.ModelStatusDisabled}
	m3 := model.Model{Name: "m3", ManagementStatus: model.ModelStatusDisabled}
	if err := db.Create(&m1).Error; err != nil {
		t.Fatalf("create m1: %v", err)
	}
	if err := db.Create(&m2).Error; err != nil {
		t.Fatalf("create m2: %v", err)
	}
	if err := db.Create(&m3).Error; err != nil {
		t.Fatalf("create m3: %v", err)
	}

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.UpstreamStatus.AvailableProviders != 2 {
		t.Fatalf("AvailableProviders: want 2, got %d", body.UpstreamStatus.AvailableProviders)
	}
	if body.UpstreamStatus.AbnormalKeys != 1 {
		t.Fatalf("AbnormalKeys: want 1 (enabled+untested only), got %d", body.UpstreamStatus.AbnormalKeys)
	}
	if body.UpstreamStatus.UnavailableModels != 2 {
		t.Fatalf("UnavailableModels: want 2, got %d", body.UpstreamStatus.UnavailableModels)
	}
}

func TestGetDashboardReturns500WhenDBFails(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	testutil.CloseDB(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.InternalError {
		t.Fatalf("expected code %d, got %d", errcode.InternalError, env.Code)
	}
}
