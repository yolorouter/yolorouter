// Package repository provides the shared request_logs query layer. The dashboard,
// analytics, and request-log list all filter the same
// request_logs table the same way, so the filter + status-bucketing + basic
// list/count/get + aggregate-totals helpers live here and are reused across
// the three handlers.
package repository

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// RequestLogStatusClass buckets a row into the status groups the
// UI and analytics group by. Derived from status_code + fail_reason (set by
// finalize): a 2xx with no fail_reason is clean success; a 2xx WITH a
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
// HasTools / KeySwitched / Failover filters are NOT applied yet: they
// need either JSON inspection of attempts_detail or new columns. The
// dashboard/analytics endpoints simply don't expose those filters yet.
type RequestLogFilter struct {
	RequestID string
	APIKeyID  *uint
	// UserID narrows to rows owned by one account (the key owner,
	// denormalized onto request_logs at write time). Unauthenticated audit
	// rows carry NULL and therefore never match a user filter.
	UserID      *uint
	ModelName   string
	ProviderID  *uint
	StatusClass string
	IsStream    *bool
	// CostKnown filters on whether the row could be priced. Deep-linked from
	// the dashboard's unknown-cost figure, whose count this filter mirrors.
	CostKnown *bool
	// KeyPrefix narrows to rows whose api key starts with this prefix — the
	// searchable identity a key HAS (the plaintext is never stored), matched
	// via a subquery on api_keys so this table needs no join or new column.
	KeyPrefix string
	// Source narrows on who initiated the request: "" = no constraint,
	// model.RequestLogSourceVisionFallback = describe sub-calls only,
	// model.RequestLogSourceCallerFilter = normal requests only (see that
	// constant for why the sentinel exists).
	Source string
	// RequestPath narrows on the caller-side request path. Matched exactly,
	// except a value ending in "/" selects the whole subtree — needed for the
	// Gemini-compatible ingress, whose paths embed the model name
	// (/v1beta/models/{model}:{action}) and so cannot be listed exactly.
	// Exact-by-default keeps a fixed path like /v1/messages from also
	// pulling in sibling paths under it.
	RequestPath string
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
	if f.UserID != nil {
		q = q.Where("user_id = ?", *f.UserID)
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
	if f.CostKnown != nil {
		q = q.Where("cost_known = ?", *f.CostKnown)
	}
	if f.KeyPrefix != "" {
		// LOWER on both sides, like every other LIKE in this package: search
		// must not depend on the driver, and prefixes mix case freely.
		q = q.Where("api_key_id IN (SELECT id FROM api_keys WHERE LOWER(key_prefix) LIKE LOWER(?) ESCAPE '\\')", likePrefixPattern(f.KeyPrefix))
	}
	switch f.Source {
	case "":
	case model.RequestLogSourceCallerFilter:
		q = q.Where("source = ''")
	default:
		q = q.Where("source = ?", f.Source)
	}
	if f.RequestPath != "" {
		if strings.HasSuffix(f.RequestPath, "/") {
			q = q.Where("LOWER(request_path) LIKE LOWER(?) ESCAPE '\\'", likePrefixPattern(f.RequestPath))
		} else {
			q = q.Where("LOWER(request_path) = LOWER(?)", f.RequestPath)
		}
	}
	if f.StartTime != nil {
		q = q.Where("created_at >= ?", *f.StartTime)
	}
	if f.EndTime != nil {
		q = q.Where("created_at < ?", *f.EndTime)
	}
	return applyStatusClass(q, f.StatusClass)
}

// applyStatusClass layers the status bucket onto a query. Empty /
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

// GetRequestLogByRequestID returns the single row for a request id, so a
// single request can be located precisely by its request identifier. Returns
// gorm.ErrRecordNotFound when absent.
func GetRequestLogByRequestID(db *gorm.DB, requestID string) (*model.RequestLog, error) {
	var row model.RequestLog
	if err := db.Where("request_id = ?", requestID).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// GetRequestLogBodyByRequestID returns the 1:1 body row joined by request_id.
// Returns (nil, nil) when absent (pre-migration rows or capture
// failure) — the service treats nil as empty bodies, never an error. Mirrors
// reference FindRelayLogBodyByRequestID (relay_log_dao.go:18-28).
func GetRequestLogBodyByRequestID(db *gorm.DB, requestID string) (*model.RequestLogBody, error) {
	var row model.RequestLogBody
	err := db.Where("request_id = ?", requestID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// GetStreamBodyPathByRequestID reads ONLY the stream_body_path column for
// requestID, instead of loading the whole body row (which carries the large
// request/response TEXT columns) just to read one short filename — the
// stream-file serving endpoint needs nothing else.
// Returns ("", nil) when there is no body row for the request.
func GetStreamBodyPathByRequestID(db *gorm.DB, requestID string) (string, error) {
	var paths []string
	err := db.Model(&model.RequestLogBody{}).
		Where("request_id = ?", requestID).
		Limit(1).
		Pluck("stream_body_path", &paths).Error
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		return "", nil
	}
	return paths[0], nil
}

// DayBoundsAt returns the [start, end) UTC timestamps for the calendar day
// containing t in the given location. created_at is stored UTC, so callers
// compare created_at >= start AND created_at < end; end is exclusive.
//
// This is the single home for day-window arithmetic. Callers that mean
// "today" pass their own clock reading — the repository layer never reads
// the wall clock for a query window, so day-boundary behaviour stays
// deterministic under test with a pinned time.
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
// bucket (caller cancels count toward total calls but NOT the
// success rate). KnownCostMicros sums cost_micros, which finalize leaves at
// 0 whenever cost_known=false, so the sum equals the known-cost total
// without a dialect-specific CASE on the boolean column.
type MetricTotals struct {
	TotalCalls       int64
	SuccessCalls     int64
	EndedCalls       int64
	UnknownCostCalls int64
	InputTokens      int64
	OutputTokens     int64
	CacheWriteTokens int64
	CacheReadTokens  int64
	KnownCostMicros  int64
	// Cache economics, both non-negative. Net cache saving is
	// CacheReadSavedMicros − CacheWriteExtraMicros, left for the reader.
	CacheReadSavedMicros  int64
	CacheWriteExtraMicros int64
}

// successRateOf returns success/ended, or 0 when no request has ended. It is
// the single definition of the success-rate formula ("ended"
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
		TotalCalls            int64
		SuccessCalls          int64
		EndedCalls            int64
		UnknownCostCalls      int64
		InputTokens           int64
		OutputTokens          int64
		CacheWriteTokens      int64
		CacheReadTokens       int64
		KnownCostMicros       int64
		CacheReadSavedMicros  int64
		CacheWriteExtraMicros int64
	}
	err := f.applyFilter(db).Select(`
		COUNT(*) AS total_calls,
		SUM(CASE WHEN status_code >= 200 AND status_code < 300 AND (fail_reason IS NULL OR fail_reason = '') THEN 1 ELSE 0 END) AS success_calls,
		SUM(CASE WHEN status_code = 499 THEN 0 ELSE 1 END) AS ended_calls,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
		COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
		COALESCE(SUM(cost_micros), 0) AS known_cost_micros,
		COALESCE(SUM(cache_read_saved_micros), 0) AS cache_read_saved_micros,
		COALESCE(SUM(cache_write_extra_micros), 0) AS cache_write_extra_micros
	`, false).Scan(&r).Error
	if err != nil {
		return nil, err
	}
	return &MetricTotals{
		TotalCalls:            r.TotalCalls,
		SuccessCalls:          r.SuccessCalls,
		EndedCalls:            r.EndedCalls,
		UnknownCostCalls:      r.UnknownCostCalls,
		InputTokens:           r.InputTokens,
		OutputTokens:          r.OutputTokens,
		CacheWriteTokens:      r.CacheWriteTokens,
		CacheReadTokens:       r.CacheReadTokens,
		KnownCostMicros:       r.KnownCostMicros,
		CacheReadSavedMicros:  r.CacheReadSavedMicros,
		CacheWriteExtraMicros: r.CacheWriteExtraMicros,
	}, nil
}

// CountRequestLogsForModelSince counts requests that asked for one model name
// after a cutoff. Impact previews use it as the live-traffic signal for
// disabling or renaming a model: allowlists reference models by id and follow
// a rename, but callers ask by name, and this is how many recent calls would
// have missed.
func CountRequestLogsForModelSince(db *gorm.DB, modelName string, since time.Time) (int64, error) {
	var cnt int64
	err := db.Model(&model.RequestLog{}).
		Where("model_name = ? AND created_at >= ?", modelName, since).
		Count(&cnt).Error
	return cnt, err
}

// ListDistinctModelNamesSince returns every model name carried by a request
// log at or after the cutoff, deduplicated and ordered ascending. The
// analytics filter dropdown merges this history supplement into its options
// so a deleted model stays selectable for as long as the window reaches —
// log rows keep the name, and by-name filtering keeps working on it. The
// lower edge is inclusive (created_at >= since), the same convention
// CountRequestLogsForModelSince uses for "after a cutoff".
//
// SQLite satisfies the DISTINCT + ORDER BY from idx_request_logs_model_name
// (an ordered list of every indexed name) instead of building a temp b-tree;
// verified against the migrated schema with EXPLAIN QUERY PLAN.
func ListDistinctModelNamesSince(db *gorm.DB, since time.Time) ([]string, error) {
	var names []string
	err := db.Model(&model.RequestLog{}).
		Where("created_at >= ?", since).
		Distinct().Order("model_name ASC").Pluck("model_name", &names).Error
	if err != nil {
		return nil, err
	}
	return names, nil
}
