// Package repository provides by-dimension GROUP
// BY analytics aggregations on top of the shared RequestLogFilter. Pure data-access
// only: no HTTP shaping, no CSV, no business judgment. The service layer
// (internal/service/analytics) composes these into the report
// envelope and the CSV export.
//
// Reuse pattern: every AggregateBy* function calls f.applyFilter(db) to
// layer the shared WHERE shape, then adds its own SELECT / GROUP BY.
// success_rate is computed here from the SQL-emitted success_calls and
// ended_calls counters so the SQL stays the single source of truth for the
// success/ended definition (identical to AggregateRequestLogMetrics).
//
// The provider and caller dimensions deliberately avoid a LEFT JOIN to
// providers / api_keys: applyFilter emits UNqualified WHERE conditions
// (created_at, status_code, ...) and a second table in play makes columns
// that exist on both (created_at is on both request_logs and providers)
// ambiguous under Postgres. Names are resolved in a post-fetch batch lookup
// — the same pattern requestlog's fetchRelatedNames established.
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"
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
	ReportDimensionUser     = "user"
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

// ReportCallStats is the call-volume block every report dimension shares:
// raw counters straight from SQL plus the Go-computed success rate. It is
// embedded (flattened) into each dimension's row type so the JSON shape
// stays exactly what it always was, while the fields and finalizeRate
// exist once. UnknownCostCalls-style cost fields live in ReportTokenCost.
type ReportCallStats struct {
	Calls        int64   `json:"calls" gorm:"column:calls"`
	SuccessCalls int64   `json:"success_calls" gorm:"column:success_calls"`
	EndedCalls   int64   `json:"ended_calls" gorm:"column:ended_calls"`
	SuccessRate  float64 `json:"success_rate" gorm:"-"`
}

// finalizeRate computes SuccessRate from the SQL counters. Promoted onto
// every embedding row type — the four hand-written copies this replaces
// were identical.
func (s *ReportCallStats) finalizeRate() {
	s.SuccessRate = successRateOf(s.SuccessCalls, s.EndedCalls)
}

// ReportTokenCost is the token + cost block shared by the model / caller /
// user / time dimensions (provider reports duration instead of tokens and
// keeps its own cost columns). UnknownCostCalls counts rows where
// cost_known=false (price/token missing — must NOT display as zero cost).
type ReportTokenCost struct {
	InputTokens      int64 `json:"input_tokens" gorm:"column:input_tokens"`
	OutputTokens     int64 `json:"output_tokens" gorm:"column:output_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens" gorm:"column:cache_write_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens" gorm:"column:cache_read_tokens"`
	CostMicros       int64 `json:"cost_micros" gorm:"column:cost_micros"`
	UnknownCostCalls int64 `json:"unknown_cost_calls" gorm:"column:unknown_cost_calls"`
}

// ModelReportRow is one row of the dimension=model report.
type ModelReportRow struct {
	ModelName       string `json:"model_name" gorm:"column:model_name"`
	ReportCallStats `gorm:"embedded"`
	ReportTokenCost `gorm:"embedded"`
}

// ProviderReportRow is one row of the dimension=provider report. ProviderID
// nil = the bucket for rows with NULL provider_id (e.g. requests rejected
// before routing picked a provider). ProviderName is resolved post-fetch.
type ProviderReportRow struct {
	ProviderID       *uint  `json:"provider_id" gorm:"column:provider_id"`
	ProviderName     string `json:"provider_name" gorm:"-"`
	ReportCallStats  `gorm:"embedded"`
	AvgDurationMs    float64 `json:"avg_duration_ms" gorm:"column:avg_duration_ms"`
	CostMicros       int64   `json:"cost_micros" gorm:"column:cost_micros"`
	UnknownCostCalls int64   `json:"unknown_cost_calls" gorm:"column:unknown_cost_calls"`
	// Failovers is how many times this provider FAILED a request that a
	// different provider then picked up — charged to the provider that
	// failed, not the one that rescued. Computed post-aggregate from the
	// per-attempt detail, so a provider that always fails away still gets a
	// row (with zero calls): the report exists to surface exactly that one.
	Failovers int64 `json:"failovers" gorm:"-"`
}

// CallerReportRow is one row of the dimension=caller report. APIKeyID nil =
// the bucket for rows with NULL api_key_id (auth failed before the request
// was tied to a key). Username resolved post-fetch.
type CallerReportRow struct {
	APIKeyID        *uint  `json:"api_key_id" gorm:"column:api_key_id"`
	Username        string `json:"username" gorm:"-"`
	KeyPrefix       string `json:"key_prefix" gorm:"-"`
	ReportCallStats `gorm:"embedded"`
	ReportTokenCost `gorm:"embedded"`
}

// UserReportRow is one row of the dimension=user report — the same
// aggregates as the caller report but grouped by owning account, so an
// admin can read usage per person regardless of how many keys each person
// spreads their traffic over. UserID nil = the bucket for rows with NULL
// user_id (auth-rejected requests never tied to an account). Username
// resolved post-fetch.
type UserReportRow struct {
	UserID          *uint  `json:"user_id" gorm:"column:user_id"`
	Username        string `json:"username" gorm:"-"`
	ReportCallStats `gorm:"embedded"`
	ReportTokenCost `gorm:"embedded"`
}

// TimeReportRow is one row of the dimension=time report. Bucket is the local
// formatted label ("2026-07-20" for day, "2026-07-20 14:00" for hour). The
// layout's literal ":00" suffix zeroes minutes — Go's time.Format treats the
// standalone "00" as a literal since it doesn't match any reference-time
// field (the canonical minute pattern is "04").
type TimeReportRow struct {
	Bucket          string `json:"bucket"`
	ReportCallStats `gorm:"embedded"`
	ReportTokenCost `gorm:"embedded"`
}

// === GroupBy functions ===================================================

// successEndedCols is the SELECT fragment shared across the model/provider/
// caller/user dimensions: emits success_calls and ended_calls counters so the
// success_rate denominator/numerator come from one SQL definition of
// "success" (identical to AggregateRequestLogMetrics). "Ended" excludes 499
// caller-cancels. Unqualified column references are safe here
// because these dimensions don't introduce a JOIN — see package doc.
const successEndedCols = `
		COUNT(*) AS calls,
		SUM(CASE WHEN status_code >= 200 AND status_code < 300 AND (fail_reason IS NULL OR fail_reason = '') THEN 1 ELSE 0 END) AS success_calls,
		SUM(CASE WHEN status_code = 499 THEN 0 ELSE 1 END) AS ended_calls`

// tokenCostSumCols is the SELECT fragment behind ReportTokenCost, shared by
// every dimension that reports token volume (model / caller / user). The
// trailing unknown_cost_calls term is parameterized on cost_known = ? so
// the binding translates false per driver (0 on SQLite, FALSE on Postgres).
const tokenCostSumCols = `
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(cache_write_tokens), 0) AS cache_write_tokens,
		COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
		COALESCE(SUM(cost_micros), 0) AS cost_micros,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls`

// groupedQuery describes one GROUP BY aggregation over the filtered
// request_logs set. selectArgs must carry exactly the bind values for
// selectExpr's ? placeholders — passing them explicitly (rather than
// assuming one cost_known bind) keeps a dimension with a different
// placeholder count from silently binding wrong. where / having / limit are
// optional (empty string / zero = absent): the report dimensions group the
// whole filtered set, while the Top-N breakdowns gate rows before the
// aggregate, exclude their unattributed bucket per group, and rank into a
// bounded window.
type groupedQuery[R any] struct {
	selectExpr string
	selectArgs []any
	where      string
	groupCol   string
	having     string
	orderExpr  string
	limit      int
	resolve    func(*gorm.DB, []R) error
	finalize   func(*R)
}

// runGroupedAggregate is the one query pipeline every grouped aggregation
// shares — the report dimensions and the compress Top-N breakdowns alike:
// filtered SELECT, GROUP BY the dimension key, ORDER BY the ranking column,
// optional post-fetch name resolution, then the Go-side rate computation.
// A new dimension is one call to this runner plus its row type — the SQL
// assembly, nil-slice normalization and finalize walk exist only here.
func runGroupedAggregate[R any](ctx context.Context, db *gorm.DB, f *RequestLogFilter, q groupedQuery[R]) ([]R, error) {
	db = db.WithContext(ctx)
	scoped := f.applyFilter(db)
	if q.where != "" {
		scoped = scoped.Where(q.where)
	}
	scoped = scoped.Select(q.selectExpr, q.selectArgs...).Group(q.groupCol)
	if q.having != "" {
		scoped = scoped.Having(q.having)
	}
	scoped = scoped.Order(q.orderExpr)
	if q.limit > 0 {
		scoped = scoped.Limit(q.limit)
	}
	var rows []R
	if err := scoped.Scan(&rows).Error; err != nil {
		return nil, err
	}
	if q.resolve != nil {
		if err := q.resolve(db, rows); err != nil {
			return nil, err
		}
	}
	if q.finalize != nil {
		for i := range rows {
			q.finalize(&rows[i])
		}
	}
	if rows == nil {
		rows = []R{}
	}
	return rows, nil
}

// AggregateByModel groups by model_name (dimension "model"), ordered by
// call volume.
func AggregateByModel(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]ModelReportRow, error) {
	return runGroupedAggregate(ctx, db, f, groupedQuery[ModelReportRow]{
		selectExpr: `
		model_name,`[1:] + successEndedCols + `,` + tokenCostSumCols,
		selectArgs: []any{false},
		groupCol:   "model_name",
		orderExpr:  "calls DESC",
		finalize:   (*ModelReportRow).finalizeRate,
	})
}

// AggregateByProvider groups by provider_id (dimension "provider").
// NULL provider_id forms its own bucket (ProviderName resolved to "" via the
// post-fetch lookup). AvgDurationMs is AVG(duration_ms); rows with no
// successful duration still aggregate at 0 via COALESCE. Providers report
// latency rather than token volume, so this is the one entity dimension
// with its own metric tail instead of tokenCostSumCols.
func AggregateByProvider(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]ProviderReportRow, error) {
	return runGroupedAggregate(ctx, db, f, groupedQuery[ProviderReportRow]{
		selectExpr: `
		provider_id,`[1:] + successEndedCols + `,
		COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
		COALESCE(SUM(cost_micros), 0) AS cost_micros,
		SUM(CASE WHEN cost_known = ? THEN 1 ELSE 0 END) AS unknown_cost_calls
	`,
		selectArgs: []any{false},
		groupCol:   "provider_id",
		orderExpr:  "calls DESC",
		resolve:    resolveProviderNames,
		finalize:   (*ProviderReportRow).finalizeRate,
	})
}

// AttachProviderFailovers decorates a provider aggregate with each
// provider's failover count, synthesizing rows for providers that only
// appear as failed-away-from. Split from AggregateByProvider because the
// scan behind it walks every multi-attempt row in the window: the analytics
// report wants that, the cost breakdowns reuse the same aggregate and never
// render failovers, and making them pay for the scan — or see zero-only
// synthesized rows their tables cannot explain — served nobody.
func AttachProviderFailovers(ctx context.Context, db *gorm.DB, f *RequestLogFilter, rows []ProviderReportRow) ([]ProviderReportRow, error) {
	db = db.WithContext(ctx)
	failovers, err := collectProviderFailovers(db, f)
	if err != nil {
		return nil, err
	}
	seen := make(map[uint]bool, len(rows))
	for i := range rows {
		if rows[i].ProviderID != nil {
			rows[i].Failovers = failovers[*rows[i].ProviderID]
			seen[*rows[i].ProviderID] = true
		}
	}
	// A provider every request failed away from serves nothing, so the
	// grouped aggregate has no row for it — yet it is the row an operator
	// most needs to see. Give it one, carrying only the failover count. Under
	// a provider filter, only the filtered provider may gain such a row:
	// synthesizing others would show providers the filter asked to exclude.
	appended := false
	for id, n := range failovers {
		if !seen[id] {
			if f.ProviderID != nil && *f.ProviderID != id {
				continue
			}
			rows = append(rows, ProviderReportRow{ProviderID: &id, Failovers: n})
			appended = true
		}
	}
	if appended {
		if err := resolveProviderNames(db, rows); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// attemptForFailover is the slice of the per-attempt JSON this count reads:
// which provider tried. The gateway's own attempt record carries the full
// shape, but this package sits below the gateway and must not import it, so
// the one field is declared locally.
type attemptForFailover struct {
	ProviderID uint `json:"provider_id"`
}

// collectProviderFailovers charges each provider-to-provider switch to the
// provider that was switched AWAY from. Key rotation within one provider is
// not a failover and is not counted; attempts that never reached a provider
// (id 0) are skipped, so a switch is judged against the next provider that
// actually tried.
//
// The count deliberately ignores the filter's final-provider and status
// dimensions: a failover is a per-attempt event, and those two describe how
// the request ENDED. Filtering the source rows by them mis-charges every
// switch (a report filtered to provider A would count only requests that
// finished on A — none of which A failed away from) — time, model and key
// scoping still apply.
//
// Only rows with more than one attempt can contain a switch, and those are
// a small fraction of traffic, so the per-attempt JSON is decoded in Go —
// one code path for every SQL driver — rather than unnested with
// driver-specific JSON SQL. The load is unbounded by design: a LIMIT would
// silently undercount, which is worse than the tens of megabytes a
// worst-case 90-day retry-heavy window costs.
func collectProviderFailovers(db *gorm.DB, f *RequestLogFilter) (map[uint]int64, error) {
	scope := *f
	scope.ProviderID = nil
	scope.StatusClass = ""
	var details []string
	if err := scope.applyFilter(db).
		Where("attempts > 1 AND attempts_detail IS NOT NULL").
		Pluck("attempts_detail", &details).Error; err != nil {
		return nil, err
	}
	counts := make(map[uint]int64)
	for _, raw := range details {
		var attempts []attemptForFailover
		// A row this build cannot parse contributes nothing — the count is
		// an aggregate, and failing the whole report over one malformed row
		// would trade a metric for an outage.
		if err := json.Unmarshal([]byte(raw), &attempts); err != nil {
			continue
		}
		last := uint(0)
		for _, a := range attempts {
			if a.ProviderID == 0 {
				continue
			}
			if last != 0 && a.ProviderID != last {
				counts[last]++
			}
			last = a.ProviderID
		}
	}
	return counts, nil
}

// resolveProviderNames populates ProviderName via a single batched SELECT
// against providers. Rows whose ProviderID is nil (NULL provider_id bucket)
// or whose provider has been hard-deleted surface as "" — the frontend
// renders those as the "unknown" / "unrouted" bucket.
func resolveProviderNames(db *gorm.DB, rows []ProviderReportRow) error {
	return backfillByPtrID(db, rows,
		func(r *ProviderReportRow) *uint { return r.ProviderID },
		FindProviderNamesByIDs,
		func(r *ProviderReportRow, name string) { r.ProviderName = name })
}

// AggregateByCaller groups by api_key_id (dimension "caller"). NULL
// api_key_id forms its own bucket (Username resolved to "" — typically
// auth-rejected traffic). Ordered by spend, not volume: the row to look at
// first is whoever costs the most, and a cheap chatty key outranking an
// expensive quiet one would bury it; the table offers call-count sorting
// client-side.
func AggregateByCaller(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]CallerReportRow, error) {
	return runGroupedAggregate(ctx, db, f, groupedQuery[CallerReportRow]{
		selectExpr: `
		api_key_id,`[1:] + successEndedCols + `,` + tokenCostSumCols,
		selectArgs: []any{false},
		groupCol:   "api_key_id",
		orderExpr:  "cost_micros DESC",
		resolve:    resolveOwnerUsernames,
		finalize:   (*CallerReportRow).finalizeRate,
	})
}

// AggregateByUser groups the same aggregates by owning account
// (request_logs.user_id). Rows with NULL user_id form their own bucket
// (auth-rejected traffic never tied to an account). Ordered by spend for
// the same reason as the caller report.
func AggregateByUser(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]UserReportRow, error) {
	return runGroupedAggregate(ctx, db, f, groupedQuery[UserReportRow]{
		selectExpr: `
		user_id,`[1:] + successEndedCols + `,` + tokenCostSumCols,
		selectArgs: []any{false},
		groupCol:   "user_id",
		orderExpr:  "cost_micros DESC",
		resolve:    resolveAccountUsernames,
		finalize:   (*UserReportRow).finalizeRate,
	})
}

// resolveAccountUsernames populates Username on user-report rows via the
// shared batch lookup. Missing user rows simply stay "".
func resolveAccountUsernames(db *gorm.DB, rows []UserReportRow) error {
	return backfillByPtrID(db, rows,
		func(r *UserReportRow) *uint { return r.UserID },
		FindUsernamesByIDs,
		func(r *UserReportRow, name string) { r.Username = name })
}

// resolveOwnerUsernames populates Username via a single batched query
// joining api_keys to users. Same nil / hard-deleted semantics as
// resolveProviderNames.
func resolveOwnerUsernames(db *gorm.DB, rows []CallerReportRow) error {
	return backfillByPtrID(db, rows,
		func(r *CallerReportRow) *uint { return r.APIKeyID },
		FindOwnerIdentitiesByKeyIDs,
		func(r *CallerReportRow, o OwnerIdentity) { r.Username, r.KeyPrefix = o.Username, o.KeyPrefix })
}

// AggregateByTime aggregates the [start, end) range into time buckets with
// ONE grouped query rather than one query per bucket — per-bucket querying
// would cost up to 90 round-trips for a 90-day day-bucket window and 720
// for a 30-day hour window. Bucket boundaries are computed in Go, walking
// the range with the bucket's advance function, so the boundary semantics
// are untouched: day buckets follow the local calendar (a DST-transition
// day is 23 or 25 hours long), and all buckets stay anchored at the range
// start rather than snapping to clock boundaries.
//
// The SQL side groups at minute granularity on driver-neutral epoch
// arithmetic (no timezone SQL, one expression shape per dialect); Go then
// assigns each minute group to its bucket by binary search over the
// boundary list. Every real boundary is a whole minute — timezone offsets
// are whole minutes, so local midnight is too — which makes the assignment
// exact; only a caller-supplied sub-minute start could blur a boundary, by
// less than one minute of rows. The SELECT reuses successEndedCols +
// tokenCostSumCols, so the success/ended/unknown-cost definition stays
// identical to the overview card by construction.
//
// Buckets with no data are still emitted (with zeros) so the frontend can
// draw a continuous trend without patching gaps client-side. Range defaults
// to [today end, today end - 7 days) when the filter doesn't carry
// start/end. day bucket caps at 90 days, hour at 30 days.
func AggregateByTime(ctx context.Context, db *gorm.DB, f *RequestLogFilter, loc *time.Location, bucket string, now time.Time) ([]TimeReportRow, error) {
	layout, advance, err := timeBucketConfig(bucket)
	if err != nil {
		return nil, err
	}
	end, start := ResolveTimeRange(f, loc, bucket, now)

	// Bucket boundary walk — ascending start times, one per emitted row.
	bounds := make([]time.Time, 0)
	endLocal := end.In(loc)
	for cursor := start.In(loc); cursor.Before(endLocal); cursor = advance(cursor) {
		bounds = append(bounds, cursor)
	}
	if len(bounds) == 0 {
		// Empty or inverted range: nothing to emit and nothing to query.
		return []TimeReportRow{}, nil
	}

	// One grouped query over the whole window. The filter copy pins the
	// resolved [start, end) so the SQL sees exactly the walked range even
	// when the caller's filter carried no explicit times.
	ff := *f
	startUTC := start.UTC()
	endUTC := end.UTC()
	ff.StartTime = &startUTC
	ff.EndTime = &endUTC
	var groups []struct {
		MinuteIndex     int64 `gorm:"column:minute_index"`
		ReportCallStats `gorm:"embedded"`
		ReportTokenCost `gorm:"embedded"`
	}
	if err := ff.applyFilter(db.WithContext(ctx)).
		Select(minuteIndexExpr(db)+` AS minute_index,`+successEndedCols+`,`+tokenCostSumCols, false).
		Group("minute_index").
		Scan(&groups).Error; err != nil {
		return nil, err
	}

	// Assign each minute group to the last bucket starting at or before it,
	// summing the counters. A sub-minute range start floors the first
	// minute's group to just before bounds[0]; its rows are still >= start
	// (the SQL guarantees that), so the group clamps to the first bucket.
	result := make([]TimeReportRow, len(bounds))
	for _, g := range groups {
		minuteStart := time.Unix(g.MinuteIndex*60, 0).UTC()
		i := sort.Search(len(bounds), func(i int) bool { return bounds[i].After(minuteStart) }) - 1
		if i < 0 {
			i = 0
		}
		r := &result[i]
		r.Calls += g.Calls
		r.SuccessCalls += g.SuccessCalls
		r.EndedCalls += g.EndedCalls
		r.InputTokens += g.InputTokens
		r.OutputTokens += g.OutputTokens
		r.CacheWriteTokens += g.CacheWriteTokens
		r.CacheReadTokens += g.CacheReadTokens
		r.CostMicros += g.CostMicros
		r.UnknownCostCalls += g.UnknownCostCalls
	}
	for i, b := range bounds {
		// For hourly buckets, append the UTC offset to the label so fall-back
		// DST hours (e.g., two 01:00 entries) are distinguishable. Day buckets
		// never repeat so they stay as YYYY-MM-DD.
		if bucket == TimeBucketHour {
			result[i].Bucket = b.Format("2006-01-02 15:04 -07:00")
		} else {
			result[i].Bucket = b.Format(layout)
		}
		result[i].finalizeRate()
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

// minuteIndexExpr builds the dialect's epoch-minute grouping key:
// floor(unix epoch of created_at / 60). Integer epoch arithmetic is the
// whole point — no timezone or date-truncation SQL, so both dialects
// compute the identical key and all calendar/DST logic stays in Go.
func minuteIndexExpr(db *gorm.DB) string {
	if db.Dialector.Name() == "postgres" { //nolint:staticcheck // QF1008 false-positive
		return "(floor(extract(epoch FROM created_at) / 60))::bigint"
	}
	return "(CAST(strftime('%s', created_at) AS INTEGER) / 60)"
}

// timeBucketConfig maps bucket to (time format layout, advance function).
// Empty bucket defaults to "day" (the common case).
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

// === Input-compression stats =============================================
//
// Aggregates over the four compress_* columns added by migration 00016.
// Filter shape is the shared RequestLogFilter, identical to the model /
// provider / caller / time dimensions above — every WHERE condition comes
// from applyFilter, so a compress-stats query sees the SAME row set the
// overview cards see for a given filter.
//
// The service layer composes these row types into one response DTO; each
// function is a single SELECT/GROUP BY so the composition stays cheap.

// CompressTotals is the platform-wide roll-up of the compress_* columns.
// CompressedCalls counts rows where compressors_applied != ” (compression
// actually ran and modified the body); TotalCalls echoes the filtered row
// count so the caller can compute compressed_rate = CompressedCalls /
// TotalCalls without a second query. TokensSaved / CostSavedMicros are
// COALESCE'd SUMs (0 when no row contributed).
type CompressTotals struct {
	TotalCalls           int64 `json:"total_calls" gorm:"column:total_calls"`
	CompressedCalls      int64 `json:"compressed_calls" gorm:"column:compressed_calls"`
	TokensSaved          int64 `json:"tokens_saved" gorm:"column:tokens_saved"`
	CostSavedMicros      int64 `json:"cost_saved_micros" gorm:"column:cost_saved_micros"`
	TotalEstimatedTokens int64 `json:"total_estimated_tokens" gorm:"column:total_estimated_tokens"`
}

// AggregateCompressTotals returns the platform-wide totals for the filter.
// total_estimated_tokens is the total input volume compression acts on, used
// as the denominator for tokens_saved as a percentage. input_tokens stores the
// NET (non-cached) count, but compression processes the full prompt including
// its cached portion, so cache read/write tokens are added back here — without
// them a cache-heavy request would understate the denominator and inflate the
// compression rate (a fully-cached prompt could report ~100%).
func AggregateCompressTotals(ctx context.Context, db *gorm.DB, f *RequestLogFilter) (*CompressTotals, error) {
	var row CompressTotals
	err := f.applyFilter(db.WithContext(ctx)).Select(`
		COUNT(*) AS total_calls,
		SUM(CASE WHEN compressors_applied != '' THEN 1 ELSE 0 END) AS compressed_calls,
		COALESCE(SUM(compress_estimated_tokens_saved), 0) AS tokens_saved,
		COALESCE(SUM(compress_estimated_cost_saved_micros), 0) AS cost_saved_micros,
		COALESCE(SUM(input_tokens + cache_read_tokens + cache_write_tokens), 0) AS total_estimated_tokens
	`[1:]).Scan(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// CompressSkipReasonRow is one row of the skip-reason breakdown. SkipReason
// is ” for requests that actually ran compression (the OK bucket); other
// values are the short stable codes the compress stage emits when it bypasses
// a request (e.g. 'no_live_zone', 'timeout'). Ordered by count DESC so the
// common case surfaces first. Rows that never entered the compress gate are
// excluded entirely — see AggregateCompressSkipReasons.
type CompressSkipReasonRow struct {
	SkipReason string `json:"skip_reason" gorm:"column:skip_reason"`
	Calls      int64  `json:"calls" gorm:"column:calls"`
}

// AggregateCompressSkipReasons groups the rows that entered the compress gate
// by compress_skip_reason. Only rows where the compress stage actually ran
// participate (compressors_applied != ” OR compress_skip_reason != ”); rows
// where both are empty — switch off, non-chat, rejected pre-relay — never
// entered the gate and are excluded so the ” (OK) bucket stays meaningful.
// NULL is coalesced to ” (migration 00016 makes the column NOT NULL DEFAULT
// ”, so NULL only appears on pre-migration rows the filter happens to
// include).
func AggregateCompressSkipReasons(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]CompressSkipReasonRow, error) {
	var rows []CompressSkipReasonRow
	// Gate on rows that actually entered the compress stage: either the
	// engine modified the body (compressors_applied != '') or it set a skip
	// reason. Rows where both are empty never entered the gate (switch off,
	// non-chat, rejected pre-relay) and would otherwise pollute the '' (OK)
	// bucket. After the gate, '' means "entered + succeeded", mirroring the
	// compressors_applied-based definition used by CompressTotals.
	err := f.applyFilter(db.WithContext(ctx)).Select(`
		COALESCE(NULLIF(compress_skip_reason, ''), '') AS skip_reason,
		COUNT(*) AS calls
	`[1:]).Where("compressors_applied != '' OR compress_skip_reason != ''").
		Group("compress_skip_reason").Order("calls DESC").Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []CompressSkipReasonRow{}
	}
	return rows, nil
}

// CompressTopAPIKeyRow is one row of the per-API-key Top N. TokensSaved is
// the sort key (DESC); Username is resolved post-fetch via the same
// batched-SELECT pattern the caller dimension uses. Rows with NULL
// api_key_id are excluded by HAVING api_key_id IS NOT NULL — they represent
// requests that failed auth before being tied to a key and would otherwise
// surface as an "(unknown)" bucket at the top of the list.
type CompressTopAPIKeyRow struct {
	APIKeyID    *uint  `json:"api_key_id" gorm:"column:api_key_id"`
	Username    string `json:"username" gorm:"-"`
	KeyPrefix   string `json:"key_prefix" gorm:"-"`
	Calls       int64  `json:"calls" gorm:"column:calls"`
	TokensSaved int64  `json:"tokens_saved" gorm:"column:tokens_saved"`
}

// AggregateCompressTopAPIKeys returns the top `limit` api_key_id values by
// SUM(compress_estimated_tokens_saved). limit is clamped to [1, 20] (the
// service-layer cap) before this function is reached; the floor below only
// guards against a stray 0 producing an empty result.
//
// The NULL api_key_id bucket (requests that failed auth before being tied
// to a key) is excluded at the SQL layer via HAVING so it can't consume a
// LIMIT slot — a Go post-filter would let the NULL row steal one of the
// real-key slots when it sorts within the window.
func AggregateCompressTopAPIKeys(ctx context.Context, db *gorm.DB, f *RequestLogFilter, limit int) ([]CompressTopAPIKeyRow, error) {
	if limit < 1 {
		limit = 1
	}
	return runGroupedAggregate(ctx, db, f, groupedQuery[CompressTopAPIKeyRow]{
		selectExpr: `
		api_key_id,
		COUNT(*) AS calls,
		COALESCE(SUM(compress_estimated_tokens_saved), 0) AS tokens_saved
	`[1:],
		groupCol:  "api_key_id",
		having:    "api_key_id IS NOT NULL",
		orderExpr: "tokens_saved DESC, api_key_id ASC",
		limit:     limit,
		resolve:   resolveCompressOwnerUsernames,
	})
}

// resolveCompressOwnerUsernames populates Username via a single batched
// SELECT — same nil / hard-deleted semantics as resolveOwnerUsernames.
func resolveCompressOwnerUsernames(db *gorm.DB, rows []CompressTopAPIKeyRow) error {
	return backfillByPtrID(db, rows,
		func(r *CompressTopAPIKeyRow) *uint { return r.APIKeyID },
		FindOwnerIdentitiesByKeyIDs,
		func(r *CompressTopAPIKeyRow, o OwnerIdentity) { r.Username, r.KeyPrefix = o.Username, o.KeyPrefix })
}

// compressSavingsSumCols is the SELECT tail shared by the model and provider
// compress Top-N breakdowns — the two differ only in their grouping key.
// compressed_calls counts rows that actually ran compression; under the
// compressors_applied != ” row gate both breakdowns apply, it equals
// total_calls, but the two columns are kept separate so the wire shape
// doesn't encode the gate.
const compressSavingsSumCols = `
		COALESCE(SUM(compress_estimated_tokens_saved), 0) AS tokens_saved,
		COALESCE(SUM(compress_estimated_cost_saved_micros), 0) AS cost_saved_micros,
		SUM(CASE WHEN compressors_applied != '' THEN 1 ELSE 0 END) AS compressed_calls,
		COUNT(*) AS total_calls`

// CompressTopModelRow is one row of the per-model Top N. Same shape as
// CompressTopAPIKeyRow but keyed by model_name (NOT NULL, so no nil bucket).
// CompressedCalls counts only rows where compressors_applied != ” (the row
// actually ran compression); TotalCalls is every row in the group so the
// caller can compute a compressed_rate per model.
type CompressTopModelRow struct {
	ModelName       string `json:"model_name" gorm:"column:model_name"`
	TokensSaved     int64  `json:"tokens_saved" gorm:"column:tokens_saved"`
	CostSavedMicros int64  `json:"cost_saved_micros" gorm:"column:cost_saved_micros"`
	CompressedCalls int64  `json:"compressed_calls" gorm:"column:compressed_calls"`
	TotalCalls      int64  `json:"total_calls" gorm:"column:total_calls"`
}

// AggregateCompressTopModels returns the top `limit` model_name values by
// SUM(compress_estimated_tokens_saved). Only rows that actually compressed
// (compressors_applied != ”) are included — otherwise every model that ever
// routed a request surfaces, drowning the signal in zero-saved rows. Empty
// model_name is excluded via HAVING so legacy rows without a model name don't
// consume a LIMIT slot (same rationale as the NULL api_key_id bucket).
func AggregateCompressTopModels(ctx context.Context, db *gorm.DB, f *RequestLogFilter, limit int) ([]CompressTopModelRow, error) {
	if limit < 1 {
		limit = 1
	}
	return runGroupedAggregate(ctx, db, f, groupedQuery[CompressTopModelRow]{
		selectExpr: "model_name," + compressSavingsSumCols,
		where:      "compressors_applied != ''",
		groupCol:   "model_name",
		having:     "model_name != ''",
		orderExpr:  "tokens_saved DESC, model_name ASC",
		limit:      limit,
	})
}

// CompressTopProviderRow is one row of the per-provider Top N. ProviderID is
// *uint to mirror ProviderReportRow (NULL provider_id = unrouted requests);
// ProviderName is resolved post-fetch via resolveCompressProviderNames.
type CompressTopProviderRow struct {
	ProviderID      *uint  `json:"provider_id" gorm:"column:provider_id"`
	ProviderName    string `json:"provider_name" gorm:"-"`
	TokensSaved     int64  `json:"tokens_saved" gorm:"column:tokens_saved"`
	CostSavedMicros int64  `json:"cost_saved_micros" gorm:"column:cost_saved_micros"`
	CompressedCalls int64  `json:"compressed_calls" gorm:"column:compressed_calls"`
	TotalCalls      int64  `json:"total_calls" gorm:"column:total_calls"`
}

// AggregateCompressTopProviders returns the top `limit` provider_id values by
// SUM(compress_estimated_tokens_saved). Same compressors_applied != ” gate
// as AggregateCompressTopModels. The NULL provider_id bucket (unrouted
// requests) is excluded at the SQL layer via HAVING so it can't steal a
// LIMIT slot — identical rationale to AggregateCompressTopAPIKeys.
func AggregateCompressTopProviders(ctx context.Context, db *gorm.DB, f *RequestLogFilter, limit int) ([]CompressTopProviderRow, error) {
	if limit < 1 {
		limit = 1
	}
	return runGroupedAggregate(ctx, db, f, groupedQuery[CompressTopProviderRow]{
		selectExpr: "provider_id," + compressSavingsSumCols,
		where:      "compressors_applied != ''",
		groupCol:   "provider_id",
		having:     "provider_id IS NOT NULL",
		orderExpr:  "tokens_saved DESC, provider_id ASC",
		limit:      limit,
		resolve:    resolveCompressProviderNames,
	})
}

// resolveCompressProviderNames populates ProviderName via a single batched
// SELECT against providers — same nil / hard-deleted semantics as
// resolveProviderNames.
func resolveCompressProviderNames(db *gorm.DB, rows []CompressTopProviderRow) error {
	return backfillByPtrID(db, rows,
		func(r *CompressTopProviderRow) *uint { return r.ProviderID },
		FindProviderNamesByIDs,
		func(r *CompressTopProviderRow, name string) { r.ProviderName = name })
}

// CompressDailySeriesRow is one day of the daily token-saved trend. Bucket is
// the local formatted label ("2026-07-20"); TokensSaved / CostSavedMicros
// are the day's COALESCE'd SUM. CompressedCalls counts rows that actually ran
// compression that day (compressors_applied != ”).
//
// The series is gap-filled so a day with zero compressed requests still
// appears in the series with zeros — keeps the frontend trend line continuous
// without client-side patching.
type CompressDailySeriesRow struct {
	Bucket          string `json:"bucket"`
	TokensSaved     int64  `json:"tokens_saved"`
	CostSavedMicros int64  `json:"cost_saved_micros"`
	CompressedCalls int64  `json:"compressed_calls"`
}

// dayBucketExpr builds a SQL day-truncation expression. On Postgres it
// uses AT TIME ZONE with the IANA zone name (DST-aware — each row is assigned
// to the correct calendar day even across a DST transition). SQLite has no
// native named-zone support, so it falls back to the fixed-offset approach
// (a 1-hour misattribution on DST transition days, acceptable for a trend
// chart). The zone name comes from time.LoadLocation and is validated against
// the tz database — safe to interpolate (only [A-Za-z0-9_/-]).
func dayBucketExpr(db *gorm.DB, loc *time.Location, offsetSec int) string {
	zoneName := loc.String()
	switch db.Dialector.Name() { //nolint:staticcheck // QF1008 false-positive
	case "postgres":
		if zoneName != "Local" {
			return fmt.Sprintf(
				"to_char((created_at AT TIME ZONE '%s')::date, 'YYYY-MM-DD')",
				zoneName,
			)
		}
		return fmt.Sprintf(
			"to_char(((created_at AT TIME ZONE 'UTC') + INTERVAL '%d seconds')::date, 'YYYY-MM-DD')",
			offsetSec,
		)
	default: // sqlite
		return fmt.Sprintf(
			"strftime('%%Y-%%m-%%d', created_at, '%+d seconds')",
			offsetSec,
		)
	}
}

// AggregateCompressDailySeries runs ONE GROUP BY query over the [start, end)
// window instead of the former day-by-day loop (which fired one SUM per day,
// 90 queries for a 90-day window). The date-truncation expression is
// dialect-specific (date arithmetic / strftime) and produces a "YYYY-MM-DD"
// label aligned to the server's local timezone. Only rows that actually ran
// compression contribute (WHERE compressors_applied != ”).
//
// Gap-fill: SQL skips days with zero compressed rows, so the result is merged
// against a Go-side walk of [start, end) — missing days surface as zero rows,
// keeping the chart x-axis continuous. Output is sorted ascending (oldest
// first) for a left-to-right trend.
func AggregateCompressDailySeries(ctx context.Context, db *gorm.DB, f *RequestLogFilter, loc *time.Location, now time.Time) ([]CompressDailySeriesRow, error) {
	layout := "2006-01-02"
	end, start := ResolveTimeRange(f, loc, TimeBucketDay, now)

	byDay := make(map[string]CompressDailySeriesRow)

	if db.Dialector.Name() == "postgres" { //nolint:staticcheck // QF1008 false-positive
		// Postgres: AT TIME ZONE with the IANA zone name is fully DST-aware.
		_, offsetSec := start.In(loc).Zone()
		dayExpr := dayBucketExpr(db, loc, offsetSec)
		var raw []struct {
			Day             string `gorm:"column:day"`
			TokensSaved     int64  `gorm:"column:tokens_saved"`
			CostSavedMicros int64  `gorm:"column:cost_saved_micros"`
			CompressedCalls int64  `gorm:"column:compressed_calls"`
		}
		err := f.applyFilter(db.WithContext(ctx)).
			Where("compressors_applied != ''").
			Select(dayExpr + ` AS day,
				COALESCE(SUM(compress_estimated_tokens_saved), 0) AS tokens_saved,
				COALESCE(SUM(compress_estimated_cost_saved_micros), 0) AS cost_saved_micros,
				COUNT(*) AS compressed_calls
			`).
			Group(dayExpr).
			Scan(&raw).Error
		if err != nil {
			return nil, err
		}
		for _, r := range raw {
			byDay[r.Day] = CompressDailySeriesRow{
				Bucket:          r.Day,
				TokensSaved:     r.TokensSaved,
				CostSavedMicros: r.CostSavedMicros,
				CompressedCalls: r.CompressedCalls,
			}
		}
	} else {
		// SQLite: no native named-zone support. Group by UTC hour in SQL
		// (efficient), then remap each UTC hour to the correct local
		// calendar day in Go using time.In(loc) — DST-aware per row.
		var raw []struct {
			UTCHour         string `gorm:"column:utc_hour"`
			TokensSaved     int64  `gorm:"column:tokens_saved"`
			CostSavedMicros int64  `gorm:"column:cost_saved_micros"`
			CompressedCalls int64  `gorm:"column:compressed_calls"`
		}
		err := f.applyFilter(db.WithContext(ctx)).
			Where("compressors_applied != ''").
			Select(`strftime('%Y-%m-%dT%H:00:00', created_at) AS utc_hour,
				COALESCE(SUM(compress_estimated_tokens_saved), 0) AS tokens_saved,
				COALESCE(SUM(compress_estimated_cost_saved_micros), 0) AS cost_saved_micros,
				COUNT(*) AS compressed_calls
			`).
			Group("utc_hour").
			Scan(&raw).Error
		if err != nil {
			return nil, err
		}
		for _, r := range raw {
			t, err := time.ParseInLocation("2006-01-02T15:04:05", r.UTCHour, time.UTC)
			if err != nil {
				continue
			}
			localDay := t.In(loc).Format(layout)
			row := byDay[localDay]
			row.Bucket = localDay
			row.TokensSaved += r.TokensSaved
			row.CostSavedMicros += r.CostSavedMicros
			row.CompressedCalls += r.CompressedCalls
			byDay[localDay] = row
		}
	}

	// Gap-fill [start, end) one local day at a time so the chart's x-axis is
	// continuous. The cursor is normalized to local-midnight of start's day so
	// a custom range that begins mid-day (e.g. start = 14:00) still produces a
	// bucket for that partial day AND for the tail day — otherwise the loop
	// advances by 24h from the exact start instant and the last calendar day
	// that intersects [start, end) is silently dropped. The walk is ascending
	// (oldest-first), matching the natural left-to-right order of a trend chart.
	result := make([]CompressDailySeriesRow, 0)
	startLocal := start.In(loc)
	cursor := time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, loc)
	endLocal := end.In(loc)
	for cursor.Before(endLocal) {
		label := cursor.Format(layout)
		row, ok := byDay[label]
		if !ok {
			row = CompressDailySeriesRow{Bucket: label}
		}
		result = append(result, row)
		cursor = cursor.AddDate(0, 0, 1)
	}
	return result, nil
}

// knownCompressorNames is the fixed set of compressor names that can appear
// in compressors_applied, matching the compressors wired in compress.go's
// compressorsFor: build output -> [gotest, log], git diff -> [diff], search
// results -> [grep]. No name is a substring of another, so substring LIKE
// matching is unambiguous.
var knownCompressorNames = []string{"gotest", "log", "diff", "grep"}

// compressorHitsSelect is the fixed SUM(CASE WHEN ...) per known compressor.
// One scan of the filtered set produces all four counts at once.
var compressorHitsSelect = `
	SUM(CASE WHEN compressors_applied LIKE '%gotest%' THEN 1 ELSE 0 END) AS gotest_hits,
	SUM(CASE WHEN compressors_applied LIKE '%log%' THEN 1 ELSE 0 END) AS log_hits,
	SUM(CASE WHEN compressors_applied LIKE '%diff%' THEN 1 ELSE 0 END) AS diff_hits,
	SUM(CASE WHEN compressors_applied LIKE '%grep%' THEN 1 ELSE 0 END) AS grep_hits`[1:]

// AggregateCompressorHits counts, for each known compressor name, how many
// filtered ROWS used that compressor. This is a single SQL aggregation over
// the fixed compressor name set: SUM(CASE WHEN compressors_applied LIKE '%<name>%'
// THEN 1 ELSE 0 END) per name. Only rows with non-empty compressors_applied
// participate.
//
// Semantics note: this counts ROWS-that-used-the-compressor, not total
// invocations. A row whose compressors_applied lists "log,log" counts ONCE
// for log (the row used the compressor; how many times it fired within that
// row is not surfaced). This is more meaningful for "how many requests used
// this compressor" than the former app-side split which counted every
// occurrence.
//
// Zero-hit entries are retained so the UI can show the user which compressors
// exist even when they haven't fired in the filtered window.
func AggregateCompressorHits(ctx context.Context, db *gorm.DB, f *RequestLogFilter) ([]CompressorHitRow, error) {
	var agg struct {
		GotestHits int64 `gorm:"column:gotest_hits"`
		LogHits    int64 `gorm:"column:log_hits"`
		DiffHits   int64 `gorm:"column:diff_hits"`
		GrepHits   int64 `gorm:"column:grep_hits"`
	}
	err := f.applyFilter(db.WithContext(ctx)).
		Where("compressors_applied != ''").
		Select(compressorHitsSelect).
		Scan(&agg).Error
	if err != nil {
		return nil, err
	}

	hits := map[string]int64{
		"gotest": agg.GotestHits,
		"log":    agg.LogHits,
		"diff":   agg.DiffHits,
		"grep":   agg.GrepHits,
	}
	out := make([]CompressorHitRow, 0, len(knownCompressorNames))
	for _, name := range knownCompressorNames {
		out = append(out, CompressorHitRow{Name: name, Hits: hits[name]})
	}
	// Sort by hits DESC, name ASC for deterministic output (the CSV export
	// would otherwise be diff-unstable across runs).
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits != out[j].Hits {
			return out[i].Hits > out[j].Hits
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// CompressorHitRow is one compressor name + the count of rows that used it
// across the filtered set.
type CompressorHitRow struct {
	Name string `json:"name"`
	Hits int64  `json:"hits"`
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
func ResolveTimeRange(f *RequestLogFilter, loc *time.Location, bucket string, now time.Time) (end, start time.Time) {
	if f.EndTime != nil {
		end = *f.EndTime
	} else {
		_, e := DayBoundsAt(loc, now)
		end = e
	}
	if f.StartTime != nil {
		start = *f.StartTime
	} else {
		// Calendar arithmetic in the client's timezone so "7 days ago" lands
		// on local midnight, not UTC midnight (which shifts across DST).
		start = end.In(loc).AddDate(0, 0, -defaultTimeLookbackDays)
	}
	if bucket == TimeBucketHour {
		if maxDur := time.Duration(maxHourLookbackHours) * time.Hour; end.Sub(start) > maxDur {
			start = end.Add(-maxDur)
		}
	} else {
		if maxDur := time.Duration(maxDayLookbackDays) * 24 * time.Hour; end.Sub(start) > maxDur {
			start = end.In(loc).AddDate(0, 0, -maxDayLookbackDays)
		}
	}
	return end, start
}
