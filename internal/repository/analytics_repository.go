// Package repository additions for M6.1 §6.7 analytics — by-dimension GROUP
// BY aggregations on top of the shared RequestLogFilter (see design doc
// .claude/docs/2026-07-20-m6-analytics-design.md §4.3). Pure data-access
// only: no HTTP shaping, no CSV, no business judgment. The service layer
// (internal/service/analytics_service.go) composes these into the report
// envelope and the CSV export.
//
// Reuse pattern: every AggregateBy* function calls f.applyFilter(db) to
// layer the shared WHERE shape, then adds its own SELECT / GROUP BY.
// success_rate is computed here from the SQL-emitted success_calls and
// ended_calls counters so the SQL stays the single source of truth for the
// success/ended definition (identical to AggregateRequestLogMetrics —
// PRD §6.6.3 / §6.7).
//
// The provider and caller dimensions deliberately avoid a LEFT JOIN to
// providers / api_keys: applyFilter emits UNqualified WHERE conditions
// (created_at, status_code, ...) and a second table in play makes columns
// that exist on both (created_at is on both request_logs and providers)
// ambiguous under Postgres. Names are resolved in a post-fetch batch lookup
// — the same pattern request_log_service.go's fetchRelatedNames established.
package repository

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

// === Dimensions / buckets ================================================
//
// Kept in the repository package (not service) because the repository owns
// the wire-level vocab for what SQL GROUP BY shape to run; the service maps
// its own input onto these.

// Legal ?dimension= values for the analytics report / export endpoints.
const (
	ReportDimensionModel    = "model"
	ReportDimensionProvider = "provider"
	ReportDimensionCaller   = "caller"
	ReportDimensionTime     = "time"
)

// Legal ?bucket= values for dimension=time.
const (
	TimeBucketDay  = "day"
	TimeBucketHour = "hour"
)

// Default / cap lookback for dimension=time when the caller doesn't supply
// start/end. Day bucket caps at 90 days; hour at 30 days to bound row count
// (720 hourly buckets is already a lot for a spreadsheet).
const (
	defaultTimeLookbackDays = 7
	maxDayLookbackDays      = 90
	maxHourLookbackHours    = 24 * 30
)

// ErrInvalidBucket is returned by AggregateByTime for an unrecognized bucket
// value. The handler maps it to a 400 InvalidParam envelope.
var ErrInvalidBucket = errors.New("invalid bucket; must be 'day' or 'hour'")

// === Row types ===========================================================

// ModelReportRow is one row of the dimension=model report. UnknownCostCalls
// counts rows where cost_known=false (price/token missing — PRD §6.7.6: must
// NOT display as zero cost). SuccessRate is computed in Go via finalizeRate.
type ModelReportRow struct {
	ModelName        string  `json:"model_name" gorm:"column:model_name"`
	Calls            int64   `json:"calls" gorm:"column:calls"`
	SuccessCalls     int64   `json:"success_calls" gorm:"column:success_calls"`
	EndedCalls       int64   `json:"ended_calls" gorm:"column:ended_calls"`
	SuccessRate      float64 `json:"success_rate" gorm:"-"`
	InputTokens      int64   `json:"input_tokens" gorm:"column:input_tokens"`
	OutputTokens     int64   `json:"output_tokens" gorm:"column:output_tokens"`
	CostCents        int64   `json:"cost_cents" gorm:"column:cost_cents"`
	UnknownCostCalls int64   `json:"unknown_cost_calls" gorm:"column:unknown_cost_calls"`
}

func (r *ModelReportRow) finalizeRate() {
	r.SuccessRate = successRateOf(r.SuccessCalls, r.EndedCalls)
}

// ProviderReportRow is one row of the dimension=provider report. ProviderID
// nil = the bucket for rows with NULL provider_id (e.g. requests rejected
// before routing picked a provider). ProviderName is resolved post-fetch.
type ProviderReportRow struct {
	ProviderID       *uint   `json:"provider_id" gorm:"column:provider_id"`
	ProviderName     string  `json:"provider_name" gorm:"-"`
	Calls            int64   `json:"calls" gorm:"column:calls"`
	SuccessCalls     int64   `json:"success_calls" gorm:"column:success_calls"`
	EndedCalls       int64   `json:"ended_calls" gorm:"column:ended_calls"`
	SuccessRate      float64 `json:"success_rate" gorm:"-"`
	AvgDurationMs    float64 `json:"avg_duration_ms" gorm:"column:avg_duration_ms"`
	CostCents        int64   `json:"cost_cents" gorm:"column:cost_cents"`
	UnknownCostCalls int64   `json:"unknown_cost_calls" gorm:"column:unknown_cost_calls"`
}

func (r *ProviderReportRow) finalizeRate() {
	r.SuccessRate = successRateOf(r.SuccessCalls, r.EndedCalls)
}

// CallerReportRow is one row of the dimension=caller report. APIKeyID nil =
// the bucket for rows with NULL api_key_id (auth failed before the request
// was tied to a key). OwnerLabel resolved post-fetch.
type CallerReportRow struct {
	APIKeyID         *uint   `json:"api_key_id" gorm:"column:api_key_id"`
	OwnerLabel       string  `json:"owner_label" gorm:"-"`
	Calls            int64   `json:"calls" gorm:"column:calls"`
	SuccessCalls     int64   `json:"success_calls" gorm:"column:success_calls"`
	EndedCalls       int64   `json:"ended_calls" gorm:"column:ended_calls"`
	SuccessRate      float64 `json:"success_rate" gorm:"-"`
	InputTokens      int64   `json:"input_tokens" gorm:"column:input_tokens"`
	OutputTokens     int64   `json:"output_tokens" gorm:"column:output_tokens"`
	CostCents        int64   `json:"cost_cents" gorm:"column:cost_cents"`
	UnknownCostCalls int64   `json:"unknown_cost_calls" gorm:"column:unknown_cost_calls"`
}

func (r *CallerReportRow) finalizeRate() {
	r.SuccessRate = successRateOf(r.SuccessCalls, r.EndedCalls)
}

// TimeReportRow is one row of the dimension=time report. Bucket is the local
// formatted label ("2026-07-20" for day, "2026-07-20 14:00" for hour). The
// layout's literal ":00" suffix zeroes minutes — Go's time.Format treats the
// standalone "00" as a literal since it doesn't match any reference-time
// field (the canonical minute pattern is "04").
//
// Unlike the model/provider/caller rows, TimeReportRow has no finalizeRate
// method: AggregateByTime delegates each bucket's totals to
// AggregateRequestLogMetrics and copies m.SuccessRate() directly, so the
// rate is always populated at construction time.
type TimeReportRow struct {
	Bucket           string  `json:"bucket"`
	Calls            int64   `json:"calls"`
	SuccessCalls     int64   `json:"success_calls"`
	EndedCalls       int64   `json:"ended_calls"`
	SuccessRate      float64 `json:"success_rate"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CostCents        int64   `json:"cost_cents"`
	UnknownCostCalls int64   `json:"unknown_cost_calls"`
}

// === GroupBy functions ===================================================

// successEndedCols is the SELECT fragment shared across the model/provider/
// caller dimensions: emits success_calls and ended_calls counters so the
// success_rate denominator/numerator come from one SQL definition of
// "success" (identical to AggregateRequestLogMetrics). "Ended" excludes 499
// caller-cancels per PRD §6.6.3. Unqualified column references are safe here
// because these dimensions don't introduce a JOIN — see package doc.
const successEndedCols = `
		COUNT(*) AS calls,
		SUM(CASE WHEN status_code >= 200 AND status_code < 300 AND (fail_reason IS NULL OR fail_reason = '') THEN 1 ELSE 0 END) AS success_calls,
		SUM(CASE WHEN status_code = 499 THEN 0 ELSE 1 END) AS ended_calls`

// AggregateByModel groups by model_name (PRD §6.7 dimension "model").
// Rows ordered by calls DESC. UnknownCostCalls is parameterized on
// cost_known = ? so the binding translates false → 0 (SQLite) / FALSE
// (Postgres) per driver, letting the boolean check live inside the
// aggregating SELECT instead of requiring a second per-group query.
func AggregateByModel(db *gorm.DB, f *RequestLogFilter) ([]ModelReportRow, error) {
	var rows []ModelReportRow
	err := f.applyFilter(db).Select(`
		model_name,`[1:]+successEndedCols+`,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cost_cents), 0) AS cost_cents,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls
	`, false).Group("model_name").Order("calls DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].finalizeRate()
	}
	if rows == nil {
		rows = []ModelReportRow{}
	}
	return rows, nil
}

// AggregateByProvider groups by provider_id (PRD §6.7 dimension "provider").
// NULL provider_id forms its own bucket (ProviderName resolved to "" via the
// post-fetch lookup). AvgDurationMs is AVG(duration_ms); rows with no
// successful duration still aggregate at 0 via COALESCE.
func AggregateByProvider(db *gorm.DB, f *RequestLogFilter) ([]ProviderReportRow, error) {
	var rows []ProviderReportRow
	err := f.applyFilter(db).Select(`
		provider_id,`[1:]+successEndedCols+`,
		COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
		COALESCE(SUM(cost_cents), 0) AS cost_cents,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls
	`, false).Group("provider_id").Order("calls DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if err := resolveProviderNames(db, rows); err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].finalizeRate()
	}
	if rows == nil {
		rows = []ProviderReportRow{}
	}
	return rows, nil
}

// resolveProviderNames populates ProviderName via a single batched SELECT
// against providers. Rows whose ProviderID is nil (NULL provider_id bucket)
// or whose provider has been hard-deleted surface as "" — the frontend
// renders those as the "unknown" / "unrouted" bucket.
func resolveProviderNames(db *gorm.DB, rows []ProviderReportRow) error {
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		if r.ProviderID != nil {
			ids = append(ids, *r.ProviderID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var providers []model.Provider
	if err := db.Select("id", "name").Where("id IN ?", ids).Find(&providers).Error; err != nil {
		return err
	}
	names := make(map[uint]string, len(providers))
	for _, p := range providers {
		names[p.ID] = p.Name
	}
	for i := range rows {
		if rows[i].ProviderID != nil {
			rows[i].ProviderName = names[*rows[i].ProviderID]
		}
	}
	return nil
}

// AggregateByCaller groups by api_key_id (PRD §6.7 dimension "caller"). NULL
// api_key_id forms its own bucket (OwnerLabel resolved to "" — typically
// requests that failed auth before being associated with a key).
func AggregateByCaller(db *gorm.DB, f *RequestLogFilter) ([]CallerReportRow, error) {
	var rows []CallerReportRow
	err := f.applyFilter(db).Select(`
		api_key_id,`[1:]+successEndedCols+`,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cost_cents), 0) AS cost_cents,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls
	`, false).Group("api_key_id").Order("calls DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if err := resolveOwnerLabels(db, rows); err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].finalizeRate()
	}
	if rows == nil {
		rows = []CallerReportRow{}
	}
	return rows, nil
}

// resolveOwnerLabels populates OwnerLabel via a single batched SELECT
// against api_keys. Same nil / hard-deleted semantics as
// resolveProviderNames.
func resolveOwnerLabels(db *gorm.DB, rows []CallerReportRow) error {
	ids := make([]uint, 0, len(rows))
	for _, r := range rows {
		if r.APIKeyID != nil {
			ids = append(ids, *r.APIKeyID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	var keys []model.APIKey
	if err := db.Select("id", "owner_label").Where("id IN ?", ids).Find(&keys).Error; err != nil {
		return err
	}
	labels := make(map[uint]string, len(keys))
	for _, k := range keys {
		labels[k.ID] = k.OwnerLabel
	}
	for i := range rows {
		if rows[i].APIKeyID != nil {
			rows[i].OwnerLabel = labels[*rows[i].APIKeyID]
		}
	}
	return nil
}

// AggregateByTime walks the [start, end) range one bucket at a time and
// delegates each bucket's totals to AggregateRequestLogMetrics — that keeps
// the success/ended/unknown-cost definition identical to the overview card,
// and avoids dialect-specific date-trunc (SQLite strftime vs Postgres
// to_char). Buckets with no data are still emitted (with zeros) so the
// frontend can draw a continuous trend without patching gaps client-side.
//
// Range defaults to [today end, today end - 7 days) when the filter doesn't
// carry start/end. day bucket caps at 90 days, hour at 30 days.
func AggregateByTime(db *gorm.DB, f *RequestLogFilter, loc *time.Location, bucket string) ([]TimeReportRow, error) {
	layout, advance, err := timeBucketConfig(bucket)
	if err != nil {
		return nil, err
	}
	end, start := ResolveTimeRange(f, loc, bucket)

	cursor := start.In(loc)
	endLocal := end.In(loc)
	result := make([]TimeReportRow, 0)
	for cursor.Before(endLocal) {
		next := advance(cursor)
		ff := *f
		bucketStartUTC := cursor.UTC()
		bucketEndUTC := next.UTC()
		ff.StartTime = &bucketStartUTC
		ff.EndTime = &bucketEndUTC
		m, err := AggregateRequestLogMetrics(db, &ff)
		if err != nil {
			return nil, err
		}
		row := TimeReportRow{
			Bucket:           cursor.Format(layout),
			Calls:            m.TotalCalls,
			SuccessCalls:     m.SuccessCalls,
			EndedCalls:       m.EndedCalls,
			SuccessRate:      m.SuccessRate(),
			InputTokens:      m.InputTokens,
			OutputTokens:     m.OutputTokens,
			CostCents:        m.KnownCostCents,
			UnknownCostCalls: m.UnknownCostCalls,
		}
		result = append(result, row)
		cursor = next
	}
	// Newest-first: someone scanning the report wants today at the top, not
	// a week ago. The walk above is ascending only because gap-fill needs
	// in-order bucket emission; reversing the output matches the display
	// order used everywhere else (created_at DESC). CSV export shares this
	// path so the downloaded file stays consistent with the on-screen table.
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return result, nil
}

// timeBucketConfig maps bucket to (time format layout, advance function).
// Empty bucket defaults to "day" (the common case in the PRD example).
// advance uses AddDate(0,0,1) for day so DST transitions don't shift the
// local-midnight boundary; hour uses Add(1h) which is DST-safe by definition
// (DST jumps are 1h maximum, so the next hour-bucket start is still
// well-defined even if the wall-clock skips).
func timeBucketConfig(bucket string) (layout string, advance func(time.Time) time.Time, err error) {
	switch bucket {
	case "", TimeBucketDay:
		return "2006-01-02", func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }, nil
	case TimeBucketHour:
		return "2006-01-02 15:00", func(t time.Time) time.Time { return t.Add(time.Hour) }, nil
	}
	return "", nil, ErrInvalidBucket
}

// resolveTimeRange returns the [end, start) UTC timestamps to query. If the
// filter already has StartTime/EndTime they're used as-is; otherwise the
// range defaults to [today end (local), today end - 7 days). The range is
// then capped per bucket to bound query count.
// ResolveTimeRange returns the [end, start) UTC timestamps to query, applying
// the bucket lookback cap. Exported so the analytics service can clamp the
// filter ONCE before dispatching to overview + every dimension, keeping the
// overview cards and the time-dimension report on the exact same window
// (otherwise the time report silently truncates an oversized custom range
// while the overview aggregates the full range).
func ResolveTimeRange(f *RequestLogFilter, loc *time.Location, bucket string) (end, start time.Time) {
	if f.EndTime != nil {
		end = *f.EndTime
	} else {
		_, e := TodayBounds(loc)
		end = e
	}
	if f.StartTime != nil {
		start = *f.StartTime
	} else {
		start = end.AddDate(0, 0, -defaultTimeLookbackDays)
	}
	if bucket == TimeBucketHour {
		if maxDur := time.Duration(maxHourLookbackHours) * time.Hour; end.Sub(start) > maxDur {
			start = end.Add(-maxDur)
		}
	} else {
		if maxDur := time.Duration(maxDayLookbackDays) * 24 * time.Hour; end.Sub(start) > maxDur {
			start = end.AddDate(0, 0, -maxDayLookbackDays)
		}
	}
	return end, start
}
