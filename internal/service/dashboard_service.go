// Package service additions for M6.1: dashboard composition per PRD §6.6.
// Composes the dashboard envelope from the dashboard_repository aggregation
// queries — strict 3-layer handler → service → repository, same as M1-M4.
// The service owns the business-DTO shape (DashboardData / RecentFailureView);
// the repository owns the SQL aggregation row types (TodayMetricsDTO /
// TrendPoint / TopCaller / UpstreamStatusDTO). See design doc
// .claude/docs/2026-07-20-m6-analytics-design.md §4.2/§4.5.
package service

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// Dashboard card / list sizes — PRD §6.6. PRD doesn't pin a trend window;
// seven days is the smallest window that still shows a week-over-week
// pattern, and is what every reference dashboard (Vercel, Stripe, etc.)
// defaults to.
const (
	DashboardTrendDays         = 7
	DashboardTopCallersLimit   = 5
	DashboardRecentFailuresLim = 5
)

// DashboardService is the stateless composition layer over
// dashboard_repository. M6.1 has no caching, masking, or permission post-
// processing — those concerns will hang off this struct in later milestones
// (e.g. M6.2 may redact owner_label per admin role).
type DashboardService struct {
	db *gorm.DB
}

// NewDashboardService returns a DashboardService bound to db. db is captured
// by reference; callers must not close it before this service stops being
// used (same lifecycle convention as every other service in internal/).
func NewDashboardService(db *gorm.DB) *DashboardService {
	return &DashboardService{db: db}
}

// DashboardData is the GET /api/admin/dashboard response body (PRD §6.6).
// Each section maps 1:1 to a card on the dashboard page.
type DashboardData struct {
	Today          repository.TodayMetricsDTO   `json:"today"`
	Trend          []repository.TrendPoint      `json:"trend"`
	TopCallers     []repository.TopCaller       `json:"top_callers"`
	RecentFailures []RecentFailureView          `json:"recent_failures"`
	UpstreamStatus repository.UpstreamStatusDTO `json:"upstream_status"`
}

// RecentFailureView is the display-safe projection of a RequestLog row in
// the recent-failures list — no plaintext key material (M5 only stores ids
// and prefixes anyway), no attempts_detail JSON blob, no internal id.
// CreatedAt is RFC3339 so the frontend can parse it with native Date without
// guessing the format.
type RecentFailureView struct {
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

// GetDashboard returns the full dashboard envelope. Each section is a
// separate repository call; if any one fails the whole call fails — the
// dashboard can't meaningfully render with a missing section, and returning
// a half-populated envelope would just hide the real error behind zeroes.
//
// All time windowing uses time.Local per PRD §6.6.3 / design doc D4 — the
// dashboard's "today" follows the server's configured timezone.
func (s *DashboardService) GetDashboard() (*DashboardData, error) {
	loc := time.Local

	today, err := repository.GetTodayMetrics(s.db, loc)
	if err != nil {
		return nil, err
	}

	trend, err := repository.GetTrend(s.db, DashboardTrendDays, loc)
	if err != nil {
		return nil, err
	}

	start, end := repository.TodayBounds(loc)
	topCallers, err := repository.GetTopCallers(s.db, start, end, DashboardTopCallersLimit)
	if err != nil {
		return nil, err
	}

	failures, err := repository.GetRecentFailures(s.db, DashboardRecentFailuresLim)
	if err != nil {
		return nil, err
	}

	upstream, err := repository.GetUpstreamStatus(s.db)
	if err != nil {
		return nil, err
	}

	return &DashboardData{
		Today:          *today,
		Trend:          trend,
		TopCallers:     topCallers,
		RecentFailures: toRecentFailureViews(failures),
		UpstreamStatus: upstream,
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
