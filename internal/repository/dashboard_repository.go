// Package repository additions for M6.1: dashboard aggregates per PRD §6.6.
// Pure data-access — no business judgment, no HTTP shaping. See design doc
// .claude/docs/2026-07-20-m6-analytics-design.md §4.2.
//
// Every function here is a small wrapper over the shared RequestLogFilter
// query layer (request_log_query.go) plus a handful of straight COUNT/SUM
// queries against the M2/M3/M4 tables. The dashboard handler composes them
// into one GET /api/admin/dashboard response.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// TodayMetricsDTO is the four today-card values the dashboard renders at the
// top (PRD §6.6.2). SuccessRate is in [0, 1] — the frontend formats it as a
// percentage. TotalCostMicros sums cost_micros, which M5 finalize leaves at 0
// whenever cost_known=false, so this sum equals the known-cost total without
// a dialect-specific CASE on the boolean column.
type TodayMetricsDTO struct {
	Calls            int64   `json:"calls"`
	TotalCostMicros  int64   `json:"total_cost_micros"`
	SuccessRate      float64 `json:"success_rate"`
	UnknownCostCalls int64   `json:"unknown_cost_calls"`
}

// GetTodayMetrics returns calls / total known cost / success rate / unknown-
// cost count for the calendar day containing now in loc (PRD §6.6.3: "today"
// is by the system's current timezone). Delegates the bucketing math to
// AggregateRequestLogMetrics so the dashboard's success-rate definition
// stays identical to the analytics page's.
func GetTodayMetrics(db *gorm.DB, loc *time.Location) (*TodayMetricsDTO, error) {
	start, end := TodayBounds(loc)
	m, err := AggregateRequestLogMetrics(db, &RequestLogFilter{StartTime: &start, EndTime: &end})
	if err != nil {
		return nil, err
	}
	return &TodayMetricsDTO{
		Calls:            m.TotalCalls,
		TotalCostMicros:  m.KnownCostMicros,
		SuccessRate:      m.SuccessRate(),
		UnknownCostCalls: m.UnknownCostCalls,
	}, nil
}

// TrendPoint is one day's totals in the N-day trend chart.
type TrendPoint struct {
	Date       string `json:"date"` // "2006-01-02", localized
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

// GetTrend returns per-day totals for the `days` calendar days ending today
// (today inclusive, oldest first). Walks backwards via DayBoundsAt so each
// day's [start, end) window is computed in loc and compared against the
// UTC-stored created_at — one round trip per day, no dialect-specific
// date-trunc needed. days<=0 snaps to 1.
//
// The returned slice is ordered oldest-first so the frontend can render it
// left-to-right without re-sorting.
func GetTrend(db *gorm.DB, days int, loc *time.Location) ([]TrendPoint, error) {
	if days < 1 {
		days = 1
	}
	now := time.Now().In(loc)
	points := make([]TrendPoint, 0, days)
	for i := days - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i)
		start, end := DayBoundsAt(loc, day)
		var r struct {
			Calls      int64
			CostMicros int64
		}
		err := db.Model(&model.RequestLog{}).
			Where("created_at >= ? AND created_at < ?", start, end).
			Select("COUNT(*) AS calls, COALESCE(SUM(cost_micros), 0) AS cost_micros").
			Scan(&r).Error
		if err != nil {
			return nil, err
		}
		points = append(points, TrendPoint{
			Date:       day.Format("2006-01-02"),
			Calls:      r.Calls,
			CostMicros: r.CostMicros,
		})
	}
	return points, nil
}

// TopCaller is one row in the "top callers by cost" list (PRD §6.6.4).
type TopCaller struct {
	APIKeyID   uint   `json:"api_key_id"`
	OwnerLabel string `json:"owner_label"`
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

// GetTopCallers returns the top `limit` API keys by known cost incurred
// within [start, end), joined to api_keys for owner_label display. Rows with
// NULL api_key_id are excluded — they represent requests that failed auth
// before being tied to a key and carry no meaningful "caller" identity.
// Ties on cost break by api_key_id ascending for a stable order.
//
// GROUP BY both rl.api_key_id AND ak.owner_label to satisfy Postgres's
// "non-aggregated SELECT column must appear in GROUP BY" rule; because each
// api_key_id maps to exactly one owner_label through the INNER JOIN, this
// produces the same groups as grouping by api_key_id alone.
func GetTopCallers(db *gorm.DB, start, end time.Time, limit int) ([]TopCaller, error) {
	if limit < 1 {
		limit = 1
	}
	var rows []TopCaller
	err := db.Table("request_logs AS rl").
		Select("rl.api_key_id AS api_key_id, COALESCE(ak.owner_label, '') AS owner_label, "+
			"COUNT(*) AS calls, COALESCE(SUM(rl.cost_micros), 0) AS cost_micros").
		Joins("INNER JOIN api_keys ak ON ak.id = rl.api_key_id").
		Where("rl.created_at >= ? AND rl.created_at < ?", start, end).
		Where("rl.api_key_id IS NOT NULL").
		Group("rl.api_key_id, ak.owner_label").
		Order("cost_micros DESC, rl.api_key_id ASC").
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []TopCaller{}
	}
	return rows, nil
}

// GetRecentFailures returns the most recent `limit` rows that fall into any
// of the failed / partial / rejected status buckets (PRD §6.8.2). Caller-
// cancel (499) is deliberately excluded — it's a client-side abort, not a
// system-side failure the admin needs to investigate.
//
// One OR'd query instead of three ListRequestLogs calls (StatusFailed /
// StatusPartial / StatusRejected) to keep this at a single round trip: the
// dashboard renders five rows, and the merged sort key (created_at DESC,
// id DESC) is identical to ListRequestLogs's.
//
// No time window — "recent" means most-recent-overall, not most-recent-
// today. A failure from 11pm yesterday is still useful on this morning's
// dashboard.
func GetRecentFailures(db *gorm.DB, limit int) ([]model.RequestLog, error) {
	if limit < 1 {
		limit = 1
	}
	var rows []model.RequestLog
	err := db.Model(&model.RequestLog{}).
		Where(`(status_code >= 200 AND status_code < 300 AND fail_reason IS NOT NULL AND fail_reason != '')
			OR status_code IN (401, 403, 429)
			OR (status_code >= 400 AND status_code NOT IN (401, 403, 429, 499))`).
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// UpstreamStatusDTO reports provider/key/model availability counts for the
// dashboard's upstream-health card (PRD §6.6.5). Each count is a straight
// COUNT against the corresponding M2/M3 table; no request_logs involvement.
type UpstreamStatusDTO struct {
	AvailableProviders int64 `json:"available_providers"`
	AbnormalKeys       int64 `json:"abnormal_keys"`
	UnavailableModels  int64 `json:"unavailable_models"`
}

// GetUpstreamStatus counts:
//   - AvailableProviders: providers with management_status=Enabled (M2)
//   - AbnormalKeys: provider_keys with management_status=Enabled but
//     verification_status != Passed — i.e. keys that are supposed to be
//     serving traffic but haven't passed (or have lost) verification
//   - UnavailableModels: models with management_status != Enabled — i.e.
//     models the admin has switched off, regardless of candidate state
//
// "Abnormal" keys are scoped to management_status=Enabled because a disabled
// key that's also unverified isn't actually abnormal — it's been taken
// offline already, which is the right state for an unverified credential.
func GetUpstreamStatus(db *gorm.DB) (UpstreamStatusDTO, error) {
	var s UpstreamStatusDTO
	if err := db.Model(&model.Provider{}).
		Where("management_status = ?", model.ProviderStatusEnabled).
		Count(&s.AvailableProviders).Error; err != nil {
		return s, err
	}
	if err := db.Model(&model.ProviderKey{}).
		Where("management_status = ? AND verification_status != ?",
			model.ProviderKeyStatusEnabled, model.VerificationStatusPassed).
		Count(&s.AbnormalKeys).Error; err != nil {
		return s, err
	}
	if err := db.Model(&model.Model{}).
		Where("management_status != ?", model.ModelStatusEnabled).
		Count(&s.UnavailableModels).Error; err != nil {
		return s, err
	}
	return s, nil
}
