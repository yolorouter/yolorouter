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
	"github.com/yolorouter/yolorouter/internal/service/dashboard"
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
	svc := dashboard.NewDashboardService(db)
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
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
}

type trendPoint struct {
	Date       string `json:"date"`
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

type topCaller struct {
	APIKeyID   uint   `json:"api_key_id"`
	Username   string `json:"username"`
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

type setupStatus struct {
	Providers     int64 `json:"providers"`
	EnabledModels int64 `json:"enabled_models"`
	APIKeys       int64 `json:"api_keys"`
}

type dashboardBody struct {
	Today          todayMetrics    `json:"today"`
	Trend          []trendPoint    `json:"trend"`
	TopCallers     []topCaller     `json:"top_callers"`
	RecentFailures []recentFailure `json:"recent_failures"`
	UpstreamStatus upstreamStatus  `json:"upstream_status"`
	Setup          setupStatus     `json:"setup"`
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

// pinDashboardClock pins the handler clock to a fixed instant for the
// duration of one test. The dashboard tests seed rows a few minutes before
// "now" and assert they land in today's window; with the real clock those
// rows fall into yesterday when the test runs just after local midnight,
// so every day-window assertion here runs against a pinned instant instead.
func pinDashboardClock(t *testing.T, fixed time.Time) {
	t.Helper()
	prev := timeNow
	timeNow = func() time.Time { return fixed.UTC() }
	t.Cleanup(func() { timeNow = prev })
}

// dashboardTestNow is a mid-day local instant: far from both midnights, so
// the relative seed offsets the tests use (minutes) can never cross a day
// boundary. The date itself is arbitrary — each test gets a fresh DB.
var dashboardTestNow = time.Date(2026, 5, 15, 12, 30, 0, 0, time.Local)

func TestGetDashboardReturnsZeroEnvelopeOnFreshDB(t *testing.T) {
	r, _ := newDashboardTestRouter(t)
	pinDashboardClock(t, dashboardTestNow)

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
	if len(body.Trend) != dashboard.DashboardTrendDays {
		t.Fatalf("expected %d trend points, got %d", dashboard.DashboardTrendDays, len(body.Trend))
	}
	// Trend must be oldest-first, ending today, contiguous days.
	wantToday := dashboardTestNow.Format("2006-01-02")
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
	if body.Setup != (setupStatus{}) {
		t.Fatalf("expected zero setup status, got %+v", body.Setup)
	}
}

func TestGetDashboardTodayMetricsCountRowsInLocalDay(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	loc := time.Local
	pinDashboardClock(t, dashboardTestNow)
	start, end := repository.DayBoundsAt(loc, dashboardTestNow)
	now := dashboardTestNow

	// Two clean successes (200, cost_known=true), one failure (500, 0 cost),
	// one unknown-cost success (200, cost_known=false, cost_micros=0), one
	// caller-cancel (499 — counts toward total but NOT success rate).
	//
	// Token counts differ per row and per bucket (input/output/cache-write/
	// cache-read use distinct magnitudes) so a transposed column mapping shows
	// up as a wrong sum rather than passing on symmetric values. Unlike cost,
	// tokens are summed across every row regardless of status or cost_known.
	insertRequestLog(t, db, now.Add(-5*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 100
		r.CostKnown = true
		r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens = 10, 1, 100, 1000
	})
	insertRequestLog(t, db, now.Add(-4*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 200
		r.CostKnown = true
		r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens = 20, 2, 200, 2000
	})
	insertRequestLog(t, db, now.Add(-3*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 500
		r.CostMicros = 0
		r.CostKnown = true
		r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens = 30, 3, 300, 3000
	})
	insertRequestLog(t, db, now.Add(-2*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 0
		r.CostKnown = false
		r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens = 40, 4, 400, 4000
	})
	insertRequestLog(t, db, now.Add(-1*time.Minute), func(r *model.RequestLog) {
		r.StatusCode = 499
		r.CostMicros = 0
		r.CostKnown = true
		r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens = 50, 5, 500, 5000
	})
	// One row just before today's window — must NOT count toward today.
	insertRequestLog(t, db, start.Add(-time.Second), func(r *model.RequestLog) {
		r.StatusCode = 200
		r.CostMicros = 9999
		r.CostKnown = true
		r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens = 99999, 99999, 99999, 99999
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
	// Token sums cover all 5 in-window rows and exclude the pre-window row,
	// whose 99999s would swamp any of these totals if the window leaked.
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"InputTokens", body.Today.InputTokens, 150},
		{"OutputTokens", body.Today.OutputTokens, 15},
		{"CacheWriteTokens", body.Today.CacheWriteTokens, 1500},
		{"CacheReadTokens", body.Today.CacheReadTokens, 15000},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: want %d, got %d", tc.name, tc.want, tc.got)
		}
	}

	// Sanity: end-exclusive window means a row at exactly `end` would fall
	// into tomorrow; we don't need to test that explicitly since the
	// start.Add(-time.Second) row above already exercises the start side.
	_ = end
}

func TestGetDashboardTrendIncludesTodayRowOnly(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	loc := time.Local
	pinDashboardClock(t, dashboardTestNow)
	now := dashboardTestNow

	// Two rows today, nothing on prior days.
	insertRequestLog(t, db, now.Add(-10*time.Minute), func(r *model.RequestLog) {
		r.CostMicros = 150
		r.CostKnown = true
	})
	insertRequestLog(t, db, now.Add(-5*time.Minute), func(r *model.RequestLog) {
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
	if len(body.Trend) != dashboard.DashboardTrendDays {
		t.Fatalf("Trend len: want %d, got %d", dashboard.DashboardTrendDays, len(body.Trend))
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
	pinDashboardClock(t, dashboardTestNow)
	now := dashboardTestNow

	// Create three accounts, one api_key each. The dashboard's top-callers
	// list should rank them by cost_micros DESC regardless of how many
	// requests each made — one expensive call beats many cheap ones.
	keys := []struct {
		owner    string
		costEach int64
		calls    int
	}{
		{owner: "big-spender", costEach: 500, calls: 1},
		{owner: "mid-spender", costEach: 100, calls: 3},
		{owner: "tiny-spender", costEach: 10, calls: 10},
	}
	for _, k := range keys {
		keyID, userID := seedAPIKey(t, db, k.owner)
		for j := 0; j < k.calls; j++ {
			id := keyID
			owner := userID
			cost := k.costEach
			insertRequestLog(t, db, now.Add(-time.Duration(j+1)*time.Minute), func(r *model.RequestLog) {
				r.APIKeyID = &id
				r.UserID = &owner
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
		username   string
		costMicros int64
		calls      int64
	}{
		{"big-spender", 500, 1},
		{"mid-spender", 300, 3},
		{"tiny-spender", 100, 10},
	}
	for i, w := range want {
		got := body.TopCallers[i]
		if got.Username != w.username || got.CostMicros != w.costMicros || got.Calls != w.calls {
			t.Fatalf("TopCallers[%d]: want {username=%s cost=%d calls=%d}, got %+v",
				i, w.username, w.costMicros, w.calls, got)
		}
	}
}

func TestGetDashboardTopCallersExcludesRowsWithoutAPIKey(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	pinDashboardClock(t, dashboardTestNow)
	now := dashboardTestNow

	// A high-cost row with NULL api_key_id (e.g. failed auth) must NOT
	// surface in the top-callers list — there's no caller identity to show.
	realID, ownerID := seedAPIKey(t, db, "real-caller")
	insertRequestLog(t, db, now.Add(-1*time.Minute), func(r *model.RequestLog) {
		r.CostMicros = 99999
		r.CostKnown = true
		r.APIKeyID = nil
	})
	insertRequestLog(t, db, now.Add(-30*time.Second), func(r *model.RequestLog) {
		r.CostMicros = 5
		r.CostKnown = true
		r.APIKeyID = &realID
		r.UserID = &ownerID
	})

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.TopCallers) != 1 || body.TopCallers[0].Username != "real-caller" {
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

func TestGetDashboardSetupStatusCountsFunnelEntities(t *testing.T) {
	r, db := newDashboardTestRouter(t)

	// Providers count every row regardless of management_status: 1 enabled +
	// 1 disabled = 2, so a disabled-only deployment still reports the provider
	// step as done.
	pEnabled := model.Provider{Name: "pa", ProviderType: "openai", BaseURL: "https://a.example.com/v1", ManagementStatus: model.ProviderStatusEnabled}
	pDisabled := model.Provider{Name: "pb", ProviderType: "openai", BaseURL: "https://b.example.com/v1", ManagementStatus: model.ProviderStatusDisabled}
	if err := db.Create(&pEnabled).Error; err != nil {
		t.Fatalf("create pEnabled: %v", err)
	}
	if err := db.Create(&pDisabled).Error; err != nil {
		t.Fatalf("create pDisabled: %v", err)
	}

	// EnabledModels counts management_status=Enabled only: 1 of 2.
	mEnabled := model.Model{Name: "me", ManagementStatus: model.ModelStatusEnabled}
	mDisabled := model.Model{Name: "md", ManagementStatus: model.ModelStatusDisabled}
	if err := db.Create(&mEnabled).Error; err != nil {
		t.Fatalf("create mEnabled: %v", err)
	}
	if err := db.Create(&mDisabled).Error; err != nil {
		t.Fatalf("create mDisabled: %v", err)
	}

	// APIKeys counts active (non-revoked) only: 1 of 2.
	kActive := model.APIKey{KeyHash: "h-active", KeyPrefix: "pk-a", Status: model.APIKeyStatusActive}
	kRevoked := model.APIKey{KeyHash: "h-revoked", KeyPrefix: "pk-r", Status: model.APIKeyStatusRevoked}
	if err := db.Create(&kActive).Error; err != nil {
		t.Fatalf("create kActive: %v", err)
	}
	if err := db.Create(&kRevoked).Error; err != nil {
		t.Fatalf("create kRevoked: %v", err)
	}

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/dashboard", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", w.Code, w.Body.String())
	}
	var body dashboardBody
	if err := json.Unmarshal(env.Data, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Setup.Providers != 2 {
		t.Fatalf("Setup.Providers: want 2 (enabled+disabled), got %d", body.Setup.Providers)
	}
	if body.Setup.EnabledModels != 1 {
		t.Fatalf("Setup.EnabledModels: want 1 (enabled only), got %d", body.Setup.EnabledModels)
	}
	if body.Setup.APIKeys != 1 {
		t.Fatalf("Setup.APIKeys: want 1 (active only), got %d", body.Setup.APIKeys)
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

// TestGetDashboardTodayWindowJustAfterLocalMidnight pins the clock 38 seconds
// past local midnight: a row from five minutes ago belongs to YESTERDAY and
// must not count, while a row from ten seconds ago is today's only call. This
// is the exact configuration under which a window derived from a wall-clock
// read inside the query layer went wrong — the handler's pinned clock must be
// the single time source for the day boundary.
func TestGetDashboardTodayWindowJustAfterLocalMidnight(t *testing.T) {
	r, db := newDashboardTestRouter(t)
	justPastMidnight := time.Date(2026, 5, 15, 0, 0, 38, 0, time.Local)
	pinDashboardClock(t, justPastMidnight)

	// 23:55:38 yesterday — outside today's window.
	insertRequestLog(t, db, justPastMidnight.Add(-5*time.Minute), func(r *model.RequestLog) {
		r.CostMicros = 9999
		r.CostKnown = true
	})
	// 00:00:28 today — the only row inside the window.
	insertRequestLog(t, db, justPastMidnight.Add(-10*time.Second), func(r *model.RequestLog) {
		r.CostMicros = 55
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
	if body.Today.Calls != 1 {
		t.Fatalf("Calls: want 1 (yesterday's row must not leak in), got %d", body.Today.Calls)
	}
	if body.Today.TotalCostMicros != 55 {
		t.Fatalf("TotalCostMicros: want 55, got %d", body.Today.TotalCostMicros)
	}
	// The trend's last point is today as seen by the pinned clock, holding
	// the trend window to the same time source as the KPI window.
	if got := body.Trend[len(body.Trend)-1].Date; got != justPastMidnight.Format("2006-01-02") {
		t.Fatalf("trend last date: want %q, got %q", justPastMidnight.Format("2006-01-02"), got)
	}
}
