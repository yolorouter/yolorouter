// Package dashboard composes the admin dashboard envelope.
// Composes the dashboard envelope from the dashboard_repository aggregation
// queries — strict 3-layer handler → service → repository.
// The service owns the business-DTO shape (DashboardData / RecentFailureView);
// the repository owns the SQL aggregation row types (TodayMetricsDTO /
// TrendPoint / TopCaller / UpstreamStatusDTO).
package dashboard

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// Dashboard card / list sizes. There's no pinned trend window;
// seven days is the smallest window that still shows a week-over-week
// pattern, and is what every reference dashboard (Vercel, Stripe, etc.)
// defaults to.
const (
	DashboardTrendDays         = 7
	DashboardTopCallersLimit   = 5
	DashboardRecentFailuresLim = 5
	// maxDashboardRangeDays caps a user-selected custom range so a year-spanning
	// query doesn't produce a multi-thousand-row trend + gap-fill. Analytics
	// has its own 90-day cap; the dashboard is more generous (a year) since it's
	// the high-level overview.
	maxDashboardRangeDays = 365
)

// DashboardService is the stateless composition layer over
// dashboard_repository. It has no caching, masking, or permission post-
// processing — those concerns will hang off this struct in later milestones
// (e.g. it may redact caller identity per admin role).
type DashboardService struct {
	db *gorm.DB
}

// NewDashboardService returns a DashboardService bound to db. db is captured
// by reference; callers must not close it before this service stops being
// used (same lifecycle convention as every other service in internal/).
func NewDashboardService(db *gorm.DB) *DashboardService {
	return &DashboardService{db: db}
}

// DashboardData is the GET /api/admin/dashboard response body.
// Each section maps 1:1 to a card on the dashboard page.
type DashboardData struct {
	Today          repository.TodayMetricsDTO   `json:"today"`
	Trend          []repository.TrendPoint      `json:"trend"`
	TopCallers     []repository.TopCaller       `json:"top_callers"`
	RecentFailures []RecentFailureView          `json:"recent_failures"`
	UpstreamStatus repository.UpstreamStatusDTO `json:"upstream_status"`
	Setup          repository.SetupStatusDTO    `json:"setup"`
}

// RecentFailureView is the display-safe projection of a RequestLog row in
// the recent-failures list — no plaintext key material (only ids
// and prefixes are stored anyway), no attempts_detail JSON blob, no internal id.
// CreatedAt is RFC3339 so the frontend can parse it with native Date without
// guessing the format.
type RecentFailureView struct {
	RequestID string `json:"request_id"`
	APIKeyID  *uint  `json:"api_key_id"`
	ModelName string `json:"model_name"`
	// omitempty so a member response carries no provider key at all —
	// "member payloads contain no provider fields" is a key-absence
	// requirement, not a null-value one. Admin rows with a NULL provider
	// (requests rejected before routing) also drop the key; the frontend
	// never reads it from this list.
	ProviderID *uint   `json:"provider_id,omitempty"`
	StatusCode int     `json:"status_code"`
	FailReason *string `json:"fail_reason"`
	IsStream   bool    `json:"is_stream"`
	DurationMs int64   `json:"duration_ms"`
	CreatedAt  string  `json:"created_at"`
}

// GetDashboard returns the dashboard envelope. When rangeStart/rangeEnd are
// nil the dashboard defaults to "today" for the KPI cards + the fixed
// DashboardTrendDays trend. When supplied, the KPI cards and trend cover the
// selected [start, end) window instead. Top callers always follow the same
// window. Recent failures, upstream status, and setup status are independent
// of the time range (they reflect current state, not a historical window).
// If any one section fails the whole call fails — the dashboard can't
// meaningfully render with a missing section.
//
// now is the caller's clock reading; "today" and the trend window are both
// derived from it, so a request that straddles local midnight still sees one
// consistent day, and tests can pin the boundary exactly.
//
// userID narrows every request_logs-backed section (KPI cards, trend, top
// callers, recent failures) to one account's rows; nil = all accounts. The
// upstream/setup status sections stay global — they describe the
// deployment, not anyone's traffic.
//
// memberScoped marks a member session (not merely "a user filter is set"):
// an admin filtering by ?user_id keeps the deployment sections and the
// failures' provider identities, while a member session gets both
// stripped — that distinction cannot be derived from userID alone.
func (s *DashboardService) GetDashboard(loc *time.Location, rangeStart, rangeEnd *time.Time, userID *uint, memberScoped bool, now time.Time) (*DashboardData, error) {
	if loc == nil {
		loc = time.Local
	}

	// Resolve the KPI window once; top callers + metrics use it directly.
	// The trend window differs in the default case (7-day trend vs today KPI).
	var kpiStart, kpiEnd time.Time
	var trend []repository.TrendPoint
	var err error

	if rangeStart != nil && rangeEnd != nil {
		kpiStart, kpiEnd = *rangeStart, *rangeEnd
		// Cap the range so a year-spanning custom selection doesn't produce
		// a multi-thousand-row trend + gap-fill.
		if kpiEnd.Sub(kpiStart) > time.Duration(maxDashboardRangeDays)*24*time.Hour {
			kpiStart = kpiEnd.In(loc).AddDate(0, 0, -maxDashboardRangeDays)
		}
		trend, err = repository.GetTrendRange(s.db, kpiStart, kpiEnd, loc, userID)
	} else {
		kpiStart, kpiEnd = repository.DayBoundsAt(loc, now)
		// Trend window: DashboardTrendDays calendar days ending today,
		// today inclusive. Day arithmetic happens in loc so the window
		// lands on local midnights across DST transitions.
		trendStart, _ := repository.DayBoundsAt(loc, now.In(loc).AddDate(0, 0, -(DashboardTrendDays-1)))
		trend, err = repository.GetTrendRange(s.db, trendStart, kpiEnd, loc, userID)
	}
	if err != nil {
		return nil, err
	}

	metrics, err := repository.GetRangeMetrics(s.db, kpiStart, kpiEnd, userID)
	if err != nil {
		return nil, err
	}
	topCallers, err := repository.GetTopCallers(s.db, kpiStart, kpiEnd, DashboardTopCallersLimit, userID)
	if err != nil {
		return nil, err
	}

	failures, err := repository.GetRecentFailures(s.db, DashboardRecentFailuresLim, userID)
	if err != nil {
		return nil, err
	}

	// The upstream-health and setup-funnel cards describe the deployment
	// (which providers exist, how far onboarding got) — operator
	// information a member's dashboard must not carry. Member sessions get
	// zero-valued sections instead of the counts, and their recent
	// failures drop the provider identity too: which upstream served a
	// request is the same operator information in row form.
	if memberScoped {
		views := toRecentFailureViews(failures)
		for i := range views {
			views[i].ProviderID = nil
		}
		return &DashboardData{
			Today:          *metrics,
			Trend:          trend,
			TopCallers:     topCallers,
			RecentFailures: views,
		}, nil
	}

	upstream, err := repository.GetUpstreamStatus(s.db)
	if err != nil {
		return nil, err
	}

	setup, err := repository.GetSetupStatus(s.db)
	if err != nil {
		return nil, err
	}

	return &DashboardData{
		Today:          *metrics,
		Trend:          trend,
		TopCallers:     topCallers,
		RecentFailures: toRecentFailureViews(failures),
		UpstreamStatus: upstream,
		Setup:          setup,
	}, nil
}

// toRecentFailureViews projects each RequestLog row into its display-safe
// DTO shape. Takes pointers-to-slice-elements to avoid struct copies in the
// loop (rows[i] is already a value, &rows[i] aliases it — fine since we
// don't mutate).
func toRecentFailureViews(rows []model.RequestLog) []RecentFailureView {
	out := make([]RecentFailureView, 0, len(rows))
	for i := range rows {
		r := &rows[i]
		out = append(out, RecentFailureView{
			RequestID:  r.RequestID,
			APIKeyID:   r.APIKeyID,
			ModelName:  r.ModelName,
			ProviderID: r.ProviderID,
			StatusCode: r.StatusCode,
			FailReason: r.FailReason,
			IsStream:   r.IsStream,
			DurationMs: r.DurationMs,
			CreatedAt:  r.CreatedAt.Format(time.RFC3339),
		})
	}
	return out
}
