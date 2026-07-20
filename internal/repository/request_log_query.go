// Package repository: M6.1 shared request_logs query layer. The dashboard
// (§6.6), analytics (§6.7), and request-log list (§6.8) all filter the same
// request_logs table the same way, so the filter + status-bucketing + basic
// list/count/get + aggregate-totals helpers live here and are reused across
// the three handlers. See design doc
// .claude/docs/2026-07-20-m6-analytics-design.md §4.1.
package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

// RequestLogStatusClass buckets a row into the PRD §6.8.2 status groups the
// UI and analytics group by. Derived from status_code + fail_reason (set by
// M5 finalize): a 2xx with no fail_reason is clean success; a 2xx WITH a
// fail_reason is a partial (stream truncated / no [DONE]); 499 is a caller
// cancel; 401/403/429 are rejections (auth/rate/budget); everything else
// non-2xx is a failure.
const (
	StatusAll       = ""
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusPartial   = "partial"
	StatusCancelled = "cancelled"
	StatusRejected  = "rejected"
)

// ValidStatusClasses is the wire-level allowlist for the ?status= query param
// across every endpoint that exposes the status filter (request-log list,
// analytics report, CSV export). Centralized here so adding or removing a
// bucket updates all endpoints at once — without it each handler kept its own
// copy and the two could silently drift.
var ValidStatusClasses = map[string]struct{}{
	StatusAll:       {},
	StatusSuccess:   {},
	StatusFailed:    {},
	StatusPartial:   {},
	StatusCancelled: {},
	StatusRejected:  {},
}

// RequestLogFilter is the shared query shape for the dashboard, analytics,
// and request-log list endpoints. All fields optional; zero value = no
// constraint on that dimension.
//
// HasTools / KeySwitched / Failover filters are NOT applied in M6.1: they
// need either JSON inspection of attempts_detail or new columns (M6.2). The
// dashboard/analytics endpoints simply don't expose those filters yet.
type RequestLogFilter struct {
	RequestID   string
	APIKeyID    *uint
	ModelName   string
	ProviderID  *uint
	StatusClass string
	IsStream    *bool
	StartTime   *time.Time // inclusive
	EndTime     *time.Time // exclusive
	Page        int
	PageSize    int
}

// applyFilter returns a scoped query with the filter's WHERE conditions
// applied (no pagination / order — callers add those). status-code bucketing
// is layered on via applyStatusClass so the dashboard's success-rate math and
// the request-log list's status filter share ONE definition of "success".
func (f *RequestLogFilter) applyFilter(db *gorm.DB) *gorm.DB {
	q := db.Model(&model.RequestLog{})
	if f.RequestID != "" {
		q = q.Where("request_id = ?", f.RequestID)
	}
	if f.APIKeyID != nil {
		q = q.Where("api_key_id = ?", *f.APIKeyID)
	}
	if f.ModelName != "" {
		q = q.Where("model_name = ?", f.ModelName)
	}
	if f.ProviderID != nil {
		q = q.Where("provider_id = ?", *f.ProviderID)
	}
	if f.IsStream != nil {
		q = q.Where("is_stream = ?", *f.IsStream)
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at < ?", *f.EndTime)
	}
	return applyStatusClass(q, f.StatusClass)
}

// applyStatusClass layers the PRD §6.8.2 status bucket onto a query. Empty /
// unknown class = no constraint.
func applyStatusClass(q *gorm.DB, class string) *gorm.DB {
	switch class {
	case StatusSuccess:
		return q.Where("status_code >= 200 AND status_code < 300 AND (fail_reason IS NULL OR fail_reason = '')")
	case StatusPartial:
		return q.Where("status_code >= 200 AND status_code < 300 AND fail_reason IS NOT NULL AND fail_reason != ''")
	case StatusCancelled:
		return q.Where("status_code = 499")
	case StatusRejected:
		return q.Where("status_code IN (401, 403, 429)")
	case StatusFailed:
		return q.Where("status_code >= 400 AND status_code NOT IN (401, 403, 429, 499)")
	}
	return q
}

// CountRequestLogs returns the total row count matching the filter (ignores
// Page/PageSize).
func CountRequestLogs(db *gorm.DB, f *RequestLogFilter) (int64, error) {
	var n int64
	err := f.applyFilter(db).Count(&n).Error
	return n, err
}

// ListRequestLogs returns one page of rows (newest first) plus the total
// count for the filter, so the caller can render pagination. Page/PageSize
// default to 1/20 and clamp to 1..200.
func ListRequestLogs(db *gorm.DB, f *RequestLogFilter) (rows []model.RequestLog, total int64, err error) {
	total, err = CountRequestLogs(db, f)
	if err != nil {
		return nil, 0, err
	}
	page := f.Page
	if page < 1 {
		page = 1
	}
	pageSize := f.PageSize
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}
	err = f.applyFilter(db).
		Order("created_at DESC, id DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&rows).Error
	return rows, total, err
}

// GetRequestLogByRequestID returns the single row for a request id (PRD
// §6.8.7: "可通过请求标识精确找到单次请求"). Returns gorm.ErrRecordNotFound
// when absent.
func GetRequestLogByRequestID(db *gorm.DB, requestID string) (*model.RequestLog, error) {
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// TodayBounds returns the [start, end) UTC timestamps covering the current
// calendar day in the given location (PRD §6.6.3: "today" is by the system's
// current timezone). created_at is stored UTC, so callers compare
// created_at >= start AND created_at < end. end is exclusive.
func TodayBounds(loc *time.Location) (start, end time.Time) {
	now := time.Now().In(loc)
	startLocal := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	return startLocal.UTC(), startLocal.AddDate(0, 0, 1).UTC()
}

// DayBoundsAt returns the [start, end) UTC timestamps for the calendar day
// containing t in the given location. Used by the trend / time-bucket queries
// that walk back N days from today.
func DayBoundsAt(loc *time.Location, t time.Time) (start, end time.Time) {
	local := t.In(loc)
	startLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return startLocal.UTC(), startLocal.AddDate(0, 0, 1).UTC()
}

// RequestLogCursor pins a position in the request_logs ordering for keyset
// pagination. CSV export uses it as a natural snapshot: once the first page
// is read, the cursor is fixed, and rows inserted AFTER that point
// (created_at larger than the cursor) never satisfy the WHERE predicate on
// later pages — no offset drift, no duplicate/skipped rows.
type RequestLogCursor struct {
	CreatedAt time.Time
	ID        uint
}

// ListRequestLogsKeyset returns up to limit rows ordered newest-first, with
// (created_at, id) strictly less than cursor (nil cursor = start from newest).
// The filter's range still applies. Replaces the export path's old offset
// pagination, which drifted when the gateway inserted rows mid-export. The
// row-value comparison `(created_at, id) < (?, ?)` is supported by SQLite,
// Postgres, and MySQL.
func ListRequestLogsKeyset(db *gorm.DB, f *RequestLogFilter, cursor *RequestLogCursor, limit int) ([]model.RequestLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	q := f.applyFilter(db)
	if cursor != nil {
		q = q.Where("(created_at, id) < (?, ?)", cursor.CreatedAt, cursor.ID)
	}
	var rows []model.RequestLog
	err := q.Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// MetricTotals is the aggregate summary the dashboard's today-cards and the
// analytics overview row both render. SuccessRate() returns success/ended in
// [0,1] (0 when ended=0); the dashboard formats it as a percentage.
//
// "Ended" = success + failed + partial + rejected — every request that
// reached a real outcome — and explicitly EXCLUDES the 499 caller-cancel
// bucket (PRD §6.6.3: caller cancels count toward total calls but NOT the
// success rate). KnownCostCents sums cost_cents, which M5 finalize leaves at
// 0 whenever cost_known=false, so the sum equals the known-cost total
// without a dialect-specific CASE on the boolean column.
type MetricTotals struct {
	TotalCalls       int64
	SuccessCalls     int64
	EndedCalls       int64
	UnknownCostCalls int64
	InputTokens      int64
	OutputTokens     int64
	KnownCostCents   int64
}

// successRateOf returns success/ended, or 0 when no request has ended. It is
// the single definition of the success-rate formula (PRD §6.6.3: "ended"
// excludes the 499 caller-cancel bucket), shared by MetricTotals.SuccessRate
// and every analytics report row's finalizeRate so the call sites can't drift.
func successRateOf(successCalls, endedCalls int64) float64 {
	if endedCalls == 0 {
		return 0
	}
	return float64(successCalls) / float64(endedCalls)
}

// SuccessRate returns success/ended, or 0 when no request has ended yet.
func (m *MetricTotals) SuccessRate() float64 {
	return successRateOf(m.SuccessCalls, m.EndedCalls)
}

// AggregateRequestLogMetrics computes the MetricTotals for one filter set in
// a single SELECT. The CASE on cost_known uses a ? placeholder so GORM binds
// the bool per driver (SQLite 0/1, Postgres TRUE/FALSE) — the same mechanism
// as the standalone `cost_known = ?` queries elsewhere, so no dialect
// special-casing leaks into business code (earlier this was two queries, the
// second a separate COUNT for unknown-cost rows).
func AggregateRequestLogMetrics(db *gorm.DB, f *RequestLogFilter) (*MetricTotals, error) {
	var r struct {
		TotalCalls       int64
		SuccessCalls     int64
		EndedCalls       int64
		UnknownCostCalls int64
		InputTokens      int64
		OutputTokens     int64
		KnownCostCents   int64
	}
	err := f.applyFilter(db).Select(`
		COUNT(*) AS total_calls,
		SUM(CASE WHEN status_code >= 200 AND status_code < 300 AND (fail_reason IS NULL OR fail_reason = '') THEN 1 ELSE 0 END) AS success_calls,
		SUM(CASE WHEN status_code = 499 THEN 0 ELSE 1 END) AS ended_calls,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cost_cents), 0) AS known_cost_cents
	`, false).Scan(&r).Error
	if err != nil {
		return nil, err
	}
	return &MetricTotals{
		TotalCalls:       r.TotalCalls,
		SuccessCalls:     r.SuccessCalls,
		EndedCalls:       r.EndedCalls,
		UnknownCostCalls: r.UnknownCostCalls,
		InputTokens:      r.InputTokens,
		OutputTokens:     r.OutputTokens,
		KnownCostCents:   r.KnownCostCents,
	}, nil
}
