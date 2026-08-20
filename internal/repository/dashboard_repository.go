// Package repository provides dashboard aggregates.
// Pure data-access — no business judgment, no HTTP shaping.
//
// Every function here is a small wrapper over the shared RequestLogFilter
// query layer (request_log_query.go) plus a handful of straight COUNT/SUM
// queries against the provider/model/api-key tables. The dashboard handler composes them
// into one GET /api/admin/dashboard response.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// TodayMetricsDTO is the KPI card values the dashboard renders at the
// top. SuccessRate is in [0, 1] — the frontend formats it as a
// percentage. TotalCostMicros sums cost_micros, which finalize leaves at 0
// whenever cost_known=false, so this sum equals the known-cost total without
// a dialect-specific CASE on the boolean column.
//
// NOTE: this DTO — like TrendPoint, TopCaller, UpstreamStatusDTO, and
// SetupStatusDTO below — is embedded verbatim in the GET /api/admin/dashboard
// response body (dashboard.DashboardData). Its json tags ARE the public wire
// contract: renaming a field here renames a field the frontend reads. If a
// dashboard section ever needs to diverge from its SQL row shape, give it a
// handler-side view type at that point instead of editing these tags.
// The four token sums are mutually exclusive buckets: input_tokens is the net
// prompt (cache reads/writes excluded), so input + output + cache_write +
// cache_read is the true total without double counting.
type TodayMetricsDTO struct {
	Calls            int64   `json:"calls"`
	TotalCostMicros  int64   `json:"total_cost_micros"`
	SuccessRate      float64 `json:"success_rate"`
	UnknownCostCalls int64   `json:"unknown_cost_calls"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
}

// GetRangeMetrics returns calls / total known cost / success rate / unknown-
// cost count for an arbitrary [start, end) window. The dashboard uses it for
// both the default "today" window (via TodayBounds) and user-selected ranges.
// userID narrows to one account's rows; nil = all accounts.
func GetRangeMetrics(db *gorm.DB, start, end time.Time, userID *uint) (*TodayMetricsDTO, error) {
	m, err := AggregateRequestLogMetrics(db, &RequestLogFilter{StartTime: &start, EndTime: &end, UserID: userID})
	if err != nil {
		return nil, err
	}
	return &TodayMetricsDTO{
		Calls:            m.TotalCalls,
		TotalCostMicros:  m.KnownCostMicros,
		SuccessRate:      m.SuccessRate(),
		UnknownCostCalls: m.UnknownCostCalls,
		InputTokens:      m.InputTokens,
		OutputTokens:     m.OutputTokens,
		CacheWriteTokens: m.CacheWriteTokens,
		CacheReadTokens:  m.CacheReadTokens,
	}, nil
}

// TrendPoint is one day's totals in the N-day trend chart.
type TrendPoint struct {
	Date       string `json:"date"` // "2006-01-02", localized
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

// GetTrendRange returns per-day totals for an arbitrary [rangeStart, rangeEnd)
// window in a single GROUP BY query, then gap-fills missing days with zeros
// so the frontend chart x-axis is continuous. Uses dayBucketExpr for
// DST-aware day truncation on Postgres and fixed-offset on SQLite.
func GetTrendRange(db *gorm.DB, rangeStart, rangeEnd time.Time, loc *time.Location, userID *uint) ([]TrendPoint, error) {
	_, offsetSec := rangeStart.In(loc).Zone()
	bucketExpr := dayBucketExpr(db, loc, offsetSec)

	var raw []struct {
		Day        string `gorm:"column:day"`
		Calls      int64  `gorm:"column:calls"`
		CostMicros int64  `gorm:"column:cost_micros"`
	}
	q := db.Model(&model.RequestLog{}).
		Where("created_at >= ? AND created_at < ?", rangeStart, rangeEnd)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	err := q.
		Select(bucketExpr + ` AS day,
			COUNT(*) AS calls,
			COALESCE(SUM(cost_micros), 0) AS cost_micros
		`).
		Group(bucketExpr).
		Order("day ASC").
		Scan(&raw).Error
	if err != nil {
		return nil, err
	}

	byDay := make(map[string]TrendPoint, len(raw))
	for _, r := range raw {
		byDay[r.Day] = TrendPoint{Date: r.Day, Calls: r.Calls, CostMicros: r.CostMicros}
	}

	// Gap-fill: walk [rangeStart, rangeEnd) one local day at a time so the
	// chart's x-axis is continuous. Missing days (zero traffic) get zeros.
	layout := "2006-01-02"
	// Pre-allocate for the number of calendar days in the window.
	capacity := int(rangeEnd.Sub(rangeStart)/(24*time.Hour)) + 1
	result := make([]TrendPoint, 0, capacity)
	startLocal := rangeStart.In(loc)
	cursor := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, loc)
	endLocal := rangeEnd.In(loc)
	for cursor.Before(endLocal) {
		label := cursor.Format(layout)
		if point, ok := byDay[label]; ok {
			result = append(result, point)
		} else {
			result = append(result, TrendPoint{Date: label})
		}
		cursor = cursor.AddDate(0, 0, 1)
	}
	return result, nil
}

// TopCaller is one row in the "top callers by cost" list.
type TopCaller struct {
	APIKeyID uint   `json:"api_key_id"`
	Username string `json:"username"`
	// KeyPrefix disambiguates rows when one account owns several keys —
	// the username alone would render identical labels for distinct keys.
	KeyPrefix  string `json:"key_prefix"`
	Calls      int64  `json:"calls"`
	CostMicros int64  `json:"cost_micros"`
}

// GetTopCallers returns the top `limit` API keys by known cost incurred
// within [start, end), joined to users (via the denormalized
// request_logs.user_id) for the owning account's username. Rows with
// NULL api_key_id are excluded — they represent requests that failed auth
// before being tied to a key and carry no meaningful "caller" identity.
// Ties on cost break by api_key_id ascending for a stable order.
//
// GROUP BY rl.api_key_id AND the joined display columns to satisfy
// Postgres's "non-aggregated SELECT column must appear in GROUP BY" rule;
// because each api_key_id maps to exactly one owner through rl.user_id and
// one key row, this produces the same groups as api_key_id alone.
func GetTopCallers(db *gorm.DB, start, end time.Time, limit int, userID *uint) ([]TopCaller, error) {
	if limit < 1 {
		limit = 1
	}
	q := db.Table("request_logs AS rl").
		Select("rl.api_key_id AS api_key_id, COALESCE(u.username, '') AS username, "+
			"COALESCE(ak.key_prefix, '') AS key_prefix, "+
			"COUNT(*) AS calls, COALESCE(SUM(rl.cost_micros), 0) AS cost_micros").
		Joins("LEFT JOIN users u ON u.id = rl.user_id").
		Joins("LEFT JOIN api_keys ak ON ak.id = rl.api_key_id").
		Where("rl.created_at >= ? AND rl.created_at < ?", start, end).
		Where("rl.api_key_id IS NOT NULL")
	if userID != nil {
		q = q.Where("rl.user_id = ?", *userID)
	}
	var rows []TopCaller
	err := q.
		Group("rl.api_key_id, u.username, ak.key_prefix").
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
// of the failed / partial / rejected status buckets. Caller-
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
func GetRecentFailures(db *gorm.DB, limit int, userID *uint) ([]model.RequestLog, error) {
	if limit < 1 {
		limit = 1
	}
	q := db.Model(&model.RequestLog{}).
		Where(`(status_code >= 200 AND status_code < 300 AND fail_reason IS NOT NULL AND fail_reason != '')
			OR status_code IN (401, 403, 429)
			OR (status_code >= 400 AND status_code NOT IN (401, 403, 429, 499))`)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	var rows []model.RequestLog
	err := q.
		Order("created_at DESC, id DESC").
		Limit(limit).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// UpstreamStatusDTO reports provider/key/model availability counts for the
// dashboard's upstream-health card. Each count is a straight
// COUNT against the corresponding provider/model table; no request_logs involvement.
type UpstreamStatusDTO struct {
	AvailableProviders int64 `json:"available_providers"`
	AbnormalKeys       int64 `json:"abnormal_keys"`
	UnavailableModels  int64 `json:"unavailable_models"`
}

// SetupStatusDTO reports how far the operator has progressed through the
// one-time onboarding funnel (add provider -> enable a model -> create an API
// key). The dashboard uses it to show the single next setup action before any
// traffic has been recorded. Each field is a raw existence count, deliberately
// independent of the routability-health signals in UpstreamStatusDTO: the
// funnel answers "what should I set up next", while the upstream card answers
// "is what I set up healthy".
type SetupStatusDTO struct {
	Providers     int64 `json:"providers"`      // total provider rows, any status
	EnabledModels int64 `json:"enabled_models"` // models with management_status=Enabled
	APIKeys       int64 `json:"api_keys"`       // active (non-revoked) API keys
	TotalRequests int64 `json:"total_requests"` // lifetime request rows — range-independent "has any traffic ever existed" signal for the waiting banner
}

// GetSetupStatus counts the onboarding-funnel entities:
//   - Providers: every provider row regardless of management_status, so a
//     provider that was added then disabled still counts as "provider step
//     done" (the next action is fixing it, surfaced by the upstream card, not
//     re-adding one)
//   - EnabledModels: models with management_status=Enabled — an API key can
//     only meaningfully allowlist a model that is switched on
//   - APIKeys: non-revoked keys — an existing key means the "create a key"
//     step is complete even while waiting for the first request
//   - TotalRequests: lifetime count of request_logs rows, intentionally NOT
//     scoped to any time window. The dashboard's "waiting for first request"
//     banner must reflect whether traffic has ever existed, not whether the
//     currently selected range happens to be quiet — a range-filtered signal
//     would falsely surface the banner on a fully active system whenever a
//     quiet period is selected.
func GetSetupStatus(db *gorm.DB) (SetupStatusDTO, error) {
	var s SetupStatusDTO
	if err := db.Model(&model.Provider{}).Count(&s.Providers).Error; err != nil {
		return s, err
	}
	if err := db.Model(&model.Model{}).
		Where("management_status = ?", model.ModelStatusEnabled).
		Count(&s.EnabledModels).Error; err != nil {
		return s, err
	}
	if err := db.Model(&model.APIKey{}).
		Where("status = ?", model.APIKeyStatusActive).
		Count(&s.APIKeys).Error; err != nil {
		return s, err
	}
	if err := db.Model(&model.RequestLog{}).Count(&s.TotalRequests).Error; err != nil {
		return s, err
	}
	return s, nil
}

// GetUpstreamStatus counts:
//   - AvailableProviders: providers with management_status=Enabled
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
