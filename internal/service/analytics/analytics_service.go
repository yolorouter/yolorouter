// Package analytics composes the admin analytics reports.
// Strict 3-layer handler → service → repository, same shape as the
// sibling DashboardService / RequestLogService. This service owns:
//
//   - the query conditions, passed straight through as
//     repository.RequestLogFilter — the ONE shared query shape for the
//     dashboard, analytics, and request-log list (no service-level mirror
//     to keep in sync; a mirror's field copy once dropped 6 of 15 fields)
//   - the analytics-specific call options (AnalyticsOptions: bucketing
//     timezone + failover scan opt-in) — not query conditions, so they
//     travel beside the filter instead of on it
//   - the dimension / bucket vocabulary on the wire
//   - the overview row DTO that wraps MetricTotals + success_rate
//   - the CSV stream assembly (BOM + headers + records)
//
// All SQL lives in internal/repository/analytics_repository.go.
package analytics

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
)

// === Wire-level vocab ====================================================
//
// Mirrored here (not imported from repository) so the handler layer stays
// repository-free — same convention request_log_handler.go uses for its
// validStatusClasses map.

const (
	// DimensionModel groups by request_logs.model_name.
	DimensionModel = repository.ReportDimensionModel
	// DimensionProvider groups by request_logs.provider_id.
	DimensionProvider = repository.ReportDimensionProvider
	// DimensionCaller groups by request_logs.api_key_id.
	DimensionCaller = repository.ReportDimensionCaller
	// DimensionUser groups by request_logs.user_id (the owning account).
	DimensionUser = repository.ReportDimensionUser
	// DimensionTime groups by local-day (or local-hour) time bucket.
	DimensionTime = repository.ReportDimensionTime
)

const (
	// BucketDay emits one row per calendar day in the configured location.
	BucketDay = repository.TimeBucketDay
	// BucketHour emits one row per clock hour in the configured location.
	BucketHour = repository.TimeBucketHour
)

// validDimensions is the single ordered vocabulary of legal ?dimension=
// values. The handler's allowlist and every error text derive from it, so
// the values a caller may send and the values the errors quote cannot drift
// apart — maintained by hand, they did (the service text once omitted user
// while the handler's listed it).
var validDimensions = []string{
	DimensionModel,
	DimensionProvider,
	DimensionCaller,
	DimensionUser,
	DimensionTime,
}

// DimensionList renders validDimensions as the comma-separated list the
// error texts quote — the order is part of the wire contract. Exported for
// the handler's 400 text; the service's own ErrInvalidDimension derives
// from the same list.
func DimensionList() string {
	return strings.Join(validDimensions, ", ")
}

// IsValidDimension reports whether dim is one of the legal ?dimension=
// values. The handler validates against this instead of keeping its own
// allowlist copy.
func IsValidDimension(dim string) bool {
	for _, d := range validDimensions {
		if d == dim {
			return true
		}
	}
	return false
}

// ErrInvalidDimension is returned by GetReport / BuildCSVRecords when
// dimension isn't one of the valid values. The handler maps it to 400.
var ErrInvalidDimension = fmt.Errorf("invalid dimension; must be one of: %s", DimensionList())

// === Filter / DTO types ==================================================

// AnalyticsOptions carries the analytics-specific call options that are NOT
// query conditions (the repository filter's applyFilter never reads them):
// the timezone buckets are computed in, and the opt-in failover scan. They
// travel beside the shared repository.RequestLogFilter instead of living on
// it, because the request-log list and the dashboard never use them.
type AnalyticsOptions struct {
	// Location is the client-supplied IANA timezone used for day-bucket
	// grouping so analytics "today" matches the admin's wall clock. nil
	// falls back to the server's local zone (location()).
	Location *time.Location
	// WithFailovers asks the provider dimension to also compute per-provider
	// failover counts. Opt-in because the count walks every multi-attempt
	// row in the window: the analytics report wants it, the cost breakdowns
	// consuming the same endpoint do not, and they should not pay for it.
	WithFailovers bool
}

// location returns the effective timezone for day-bucket aggregation,
// falling back to time.Local when the caller didn't supply one. Centralized
// here so every repository call site picks up the same zone without each
// having to repeat the nil-check.
func (o AnalyticsOptions) location() *time.Location {
	if o.Location != nil {
		return o.Location
	}
	return time.Local
}

// OverviewRow is the GET /analytics/overview body. Mirrors
// repository.MetricTotals plus a pre-computed SuccessRate so the client
// doesn't have to do the divide-on-zero dance.
type OverviewRow struct {
	TotalCalls       int64   `json:"total_calls"`
	SuccessCalls     int64   `json:"success_calls"`
	EndedCalls       int64   `json:"ended_calls"`
	SuccessRate      float64 `json:"success_rate"`
	UnknownCostCalls int64   `json:"unknown_cost_calls"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CostMicros       int64   `json:"cost_micros"`
	// Cache economics for the filtered window. Net saving is
	// CacheReadSavedMicros − CacheWriteExtraMicros; both are sent so the client
	// can show the read saving and the write premium as separate line items.
	CacheReadSavedMicros  int64 `json:"cache_read_saved_micros"`
	CacheWriteExtraMicros int64 `json:"cache_write_extra_micros"`
}

// ReportResult is the GET /analytics/report body. Dimension echoes the
// request so a single client code path can render any dimension; Rows is
// the dimension-specific row slice (typed by the handler as
// []repository.*ReportRow — kept as interface{} here to avoid a service →
// handler generic-type handshake, see GetReport).
type ReportResult struct {
	Dimension string      `json:"dimension"`
	Rows      interface{} `json:"rows"`
}

// === Input-compression stats DTO =========================================

// CompressStatsResult is the GET /analytics/compress-stats body. Each slice
// is normalized to empty (never nil) so the JSON never emits null — the
// frontend can call .map / .length unconditionally.
//
// TopAPIKeyLimit echoes the effective limit used for TopAPIKeys so the UI
// can render "showing top N" deterministically. Totals carries the
// platform-wide roll-up; SkipReasonBreakdown / CompressorHits / DailySeries
// are the dimension-specific breakouts the UI renders side-by-side.
type CompressStatsResult struct {
	Totals              repository.CompressTotals           `json:"totals"`
	SkipReasonBreakdown []repository.CompressSkipReasonRow  `json:"skip_reason_breakdown"`
	TopAPIKeys          []repository.CompressTopAPIKeyRow   `json:"top_api_keys"`
	TopModels           []repository.CompressTopModelRow    `json:"top_models"`
	TopProviders        []repository.CompressTopProviderRow `json:"top_providers"`
	CompressorHits      []repository.CompressorHitRow       `json:"compressor_hits"`
	DailySeries         []repository.CompressDailySeriesRow `json:"daily_series"`
}

// === Service =============================================================

// AnalyticsService is the stateless composition layer over
// analytics_repository. It has no caching, masking, or permission post-
// processing — those concerns will hang off this struct in later milestones
// (cost-masking per admin role is a likely add).
type AnalyticsService struct {
	db *gorm.DB
}

// NewAnalyticsService returns an AnalyticsService bound to db. db is
// captured by reference; callers must not close it before this service
// stops being used (same lifecycle convention as every other service in
// internal/).
func NewAnalyticsService(db *gorm.DB) *AnalyticsService {
	return &AnalyticsService{db: db}
}

// GetOverview returns the aggregate MetricTotals for the filter.
// Reuses AggregateRequestLogMetrics so the overview's success-rate
// definition stays identical to the dashboard's today-card.
func (s *AnalyticsService) GetOverview(filter *repository.RequestLogFilter, opts AnalyticsOptions, bucket string, now time.Time) (*OverviewRow, error) {
	// Clamp the window on the same bucket cap the report uses, so the overview
	// cards and the time-dimension report aggregate the exact same range.
	resolveEffectiveRange(filter, opts, bucket, now)
	m, err := repository.AggregateRequestLogMetrics(s.db, filter)
	if err != nil {
		return nil, err
	}
	return &OverviewRow{
		TotalCalls:            m.TotalCalls,
		SuccessCalls:          m.SuccessCalls,
		EndedCalls:            m.EndedCalls,
		SuccessRate:           m.SuccessRate(),
		UnknownCostCalls:      m.UnknownCostCalls,
		InputTokens:           m.InputTokens,
		OutputTokens:          m.OutputTokens,
		CostMicros:            m.KnownCostMicros,
		CacheReadSavedMicros:  m.CacheReadSavedMicros,
		CacheWriteExtraMicros: m.CacheWriteExtraMicros,
	}, nil
}

// GetReport returns the dimension-grouped aggregates. dimension selects one
// of the report dimensions (the legal set is validDimensions above); bucket
// is only consulted for dimension=time (empty bucket defaults to "day").
// Returns ErrInvalidDimension for an unrecognized dimension and
// repository.ErrInvalidBucket for an unrecognized bucket; the handler maps
// both to 400.
func (s *AnalyticsService) GetReport(ctx context.Context, dimension, bucket string, filter *repository.RequestLogFilter, opts AnalyticsOptions, now time.Time) (*ReportResult, error) {
	rows, err := s.runReport(ctx, dimension, bucket, filter, opts, now)
	if err != nil {
		return nil, err
	}
	return &ReportResult{Dimension: dimension, Rows: rows}, nil
}

// BuildCSVRecords runs the report and renders the CSV headers + records.
// The handler commits the HTTP 200 + Content-Type / Content-Disposition +
// BOM bytes only after this succeeds (csvutil.WriteCSV then streams), so a
// build failure returns a JSON envelope, not a truncated CSV reported as
// success — same pattern as request_log's BuildExportRows + WriteCSVRows.
// Buffering the whole thing is fine: analytics exports are bounded by the
// lookback cap (90d day / 30d hour).
func (s *AnalyticsService) BuildCSVRecords(ctx context.Context, dimension, bucket string, filter *repository.RequestLogFilter, opts AnalyticsOptions, now time.Time) ([]string, [][]string, error) {
	// The CSV is the analytics report in file form; its provider sheet has a
	// failovers column, so the data is always computed here regardless of
	// what the querying page asked for on screen. opts is a value copy, so
	// forcing this flag never leaks back to the caller.
	if dimension == DimensionProvider {
		opts.WithFailovers = true
	}
	rows, err := s.runReport(ctx, dimension, bucket, filter, opts, now)
	if err != nil {
		return nil, nil, err
	}
	return buildCSV(dimension, rows)
}

// runReport dispatches to the right repository function and returns the
// typed row slice (caller-facing type chosen by dimension). The switch is
// the single point that maps dimension → row type.
func (s *AnalyticsService) runReport(ctx context.Context, dimension, bucket string, filter *repository.RequestLogFilter, opts AnalyticsOptions, now time.Time) (interface{}, error) {
	// Resolve the window once for every dimension (day cap by default; the
	// time dimension uses the caller's bucket cap) so overview, model/
	// provider/caller, and time reports all see the same [start, end).
	resolveEffectiveRange(filter, opts, bucketForDimension(dimension, bucket), now)
	switch dimension {
	case DimensionModel:
		return repository.AggregateByModel(ctx, s.db, filter)
	case DimensionProvider:
		rows, err := repository.AggregateByProvider(ctx, s.db, filter)
		if err != nil {
			return nil, err
		}
		if opts.WithFailovers {
			return repository.AttachProviderFailovers(ctx, s.db, filter, rows)
		}
		return rows, nil
	case DimensionCaller:
		return repository.AggregateByCaller(ctx, s.db, filter)
	case DimensionUser:
		return repository.AggregateByUser(ctx, s.db, filter)
	case DimensionTime:
		return repository.AggregateByTime(ctx, s.db, filter, opts.location(), bucket, now)
	}
	return nil, ErrInvalidDimension
}

// resolveEffectiveRange clamps the filter's window to the bucket lookback cap
// and writes the resolved [start, end) back into filter IN PLACE — it mutates
// the caller's struct; there is no copy. Every dimension and the overview go
// through this so they aggregate the SAME window — otherwise the time report
// would silently truncate an oversized custom range while the overview /
// model / provider / caller dimensions kept the full range. bucket="" uses
// the day-bucket cap.
func resolveEffectiveRange(filter *repository.RequestLogFilter, opts AnalyticsOptions, bucket string, now time.Time) {
	end, start := repository.ResolveTimeRange(filter, opts.location(), bucket, now)
	filter.StartTime = &start
	filter.EndTime = &end
}

// bucketForDimension returns the bucket to use for range resolution: the time
// dimension honors the caller's bucket; every other dimension uses the day
// default (they don't bucket, so the day cap is the relevant upper bound).
func bucketForDimension(dimension, bucket string) string {
	if dimension == DimensionTime {
		return bucket
	}
	return BucketDay
}

// === CSV assembly ========================================================

// tokenReportCSVColumns / tokenReportCSVCells are the shared CSV tail for
// every dimension that reports ReportCallStats + ReportTokenCost (model /
// caller / user / time): the per-dimension branches only prepend their own
// key columns. Provider keeps its own tail (duration instead of tokens).
func tokenReportCSVColumns() []string {
	return []string{"calls", "success_rate", "input_tokens", "output_tokens", "cache_write_tokens", "cache_read_tokens", "cost_micros", "unknown_cost_calls"}
}

func tokenReportCSVCells(cs repository.ReportCallStats, tc repository.ReportTokenCost) []string {
	return []string{
		strconv.FormatInt(cs.Calls, 10),
		formatRate(cs.SuccessRate),
		strconv.FormatInt(tc.InputTokens, 10),
		strconv.FormatInt(tc.OutputTokens, 10),
		strconv.FormatInt(tc.CacheWriteTokens, 10),
		strconv.FormatInt(tc.CacheReadTokens, 10),
		strconv.FormatInt(tc.CostMicros, 10),
		strconv.FormatInt(tc.UnknownCostCalls, 10),
	}
}

// buildCSV returns the headers and record rows for a dimension's typed row
// slice. The type assertions encode the row type → column order mapping per
// dimension; a future column change only needs to update this function.
func buildCSV(dimension string, rows interface{}) ([]string, [][]string, error) {
	switch dimension {
	case DimensionModel:
		typed, ok := rows.([]repository.ModelReportRow)
		if !ok {
			return nil, nil, errCSVTypeMismatch("ModelReportRow")
		}
		headers := append([]string{"model_name"}, tokenReportCSVColumns()...)
		records := make([][]string, len(typed))
		for i, r := range typed {
			records[i] = append([]string{r.ModelName}, tokenReportCSVCells(r.ReportCallStats, r.ReportTokenCost)...)
		}
		return headers, records, nil
	case DimensionProvider:
		typed, ok := rows.([]repository.ProviderReportRow)
		if !ok {
			return nil, nil, errCSVTypeMismatch("ProviderReportRow")
		}
		headers := []string{"provider_id", "provider_name", "calls", "success_rate", "failovers", "avg_duration_ms", "cost_micros", "unknown_cost_calls"}
		records := make([][]string, len(typed))
		for i, r := range typed {
			records[i] = []string{
				formatUintPtr(r.ProviderID),
				r.ProviderName,
				strconv.FormatInt(r.Calls, 10),
				formatRate(r.SuccessRate),
				strconv.FormatInt(r.Failovers, 10),
				strconv.FormatFloat(r.AvgDurationMs, 'f', 2, 64),
				strconv.FormatInt(r.CostMicros, 10),
				strconv.FormatInt(r.UnknownCostCalls, 10),
			}
		}
		return headers, records, nil
	case DimensionCaller:
		typed, ok := rows.([]repository.CallerReportRow)
		if !ok {
			return nil, nil, errCSVTypeMismatch("CallerReportRow")
		}
		headers := append([]string{"api_key_id", "username", "key_prefix"}, tokenReportCSVColumns()...)
		records := make([][]string, len(typed))
		for i, r := range typed {
			records[i] = append([]string{formatUintPtr(r.APIKeyID), r.Username, r.KeyPrefix}, tokenReportCSVCells(r.ReportCallStats, r.ReportTokenCost)...)
		}
		return headers, records, nil
	case DimensionUser:
		typed, ok := rows.([]repository.UserReportRow)
		if !ok {
			return nil, nil, errCSVTypeMismatch("UserReportRow")
		}
		headers := append([]string{"user_id", "username"}, tokenReportCSVColumns()...)
		records := make([][]string, len(typed))
		for i, r := range typed {
			records[i] = append([]string{formatUintPtr(r.UserID), r.Username}, tokenReportCSVCells(r.ReportCallStats, r.ReportTokenCost)...)
		}
		return headers, records, nil
	case DimensionTime:
		typed, ok := rows.([]repository.TimeReportRow)
		if !ok {
			return nil, nil, errCSVTypeMismatch("TimeReportRow")
		}
		headers := append([]string{"bucket"}, tokenReportCSVColumns()...)
		records := make([][]string, len(typed))
		for i, r := range typed {
			records[i] = append([]string{r.Bucket}, tokenReportCSVCells(r.ReportCallStats, r.ReportTokenCost)...)
		}
		return headers, records, nil
	}
	// Unreachable: runReport already validated dimension before the rows
	// reach buildCSV. Returning an error (not panicking) so a future
	// dimension added to runReport without a buildCSV branch fails loudly
	// instead of crashing the process.
	return nil, nil, ErrInvalidDimension
}

// errCSVTypeMismatch is the defensive fallback when runReport's switch and
// buildCSV's switch disagree on the row type for a dimension — should be
// unreachable but a regression here would otherwise panic on the type
// assertion and take down the request.
func errCSVTypeMismatch(expected string) error {
	return errors.New("analytics: internal type mismatch, expected " + expected)
}

// formatRate renders a float success_rate as a fixed-point string with 4
// decimal digits (e.g. 0.6667). Spreadsheet apps handle floats either way,
// but fixed-point keeps the CSV diff-stable across exports.
func formatRate(r float64) string {
	return strconv.FormatFloat(r, 'f', 4, 64)
}

// formatUintPtr renders *uint as decimal or "" for nil — matches the JSON
// encoding of the row fields (provider_id / api_key_id with null for the
// NULL-FK bucket).
func formatUintPtr(v *uint) string {
	if v == nil {
		return ""
	}
	return strconv.FormatUint(uint64(*v), 10)
}

// === Input-compression stats ============================================

// DefaultCompressTopN is the per-api-key Top-N row count when the caller
// doesn't supply one. Mirrors the dashboard's "top 5 callers" default
// (rendering 5 rows fits the admin UI's card height).
const DefaultCompressTopN = 5

// MaxCompressTopN caps the per-api-key Top-N row count. A higher value
// wastes screen real estate without adding signal — the dashboard's other
// lists also cap at 20.
const MaxCompressTopN = 20

// GetCompressStats aggregates the compress_* columns into one response DTO.
// Clamps the filter window on the day-bucket cap so the daily series and
// totals aggregate the same range — same reason ResolveTimeRange is applied
// in runReport. topN is clamped to [1, MaxCompressTopN]; DefaultCompressTopN
// is used when the caller passes zero.
//
// ctx is threaded through to every repository query so a client disconnect
// cancels in-flight SQL. Every slice in the result is non-nil (empty [] on
// the wire, never JSON null) so the frontend can call .map / .length without
// null-checking.
func (s *AnalyticsService) GetCompressStats(ctx context.Context, filter *repository.RequestLogFilter, opts AnalyticsOptions, topN int, now time.Time) (*CompressStatsResult, error) {
	if topN <= 0 {
		topN = DefaultCompressTopN
	}
	if topN > MaxCompressTopN {
		topN = MaxCompressTopN
	}
	// Clamp on the day-bucket cap so the daily series walk and the totals
	// share the exact same [start, end) window.
	resolveEffectiveRange(filter, opts, BucketDay, now)

	totals, err := repository.AggregateCompressTotals(ctx, s.db, filter)
	if err != nil {
		return nil, err
	}
	skipReasons, err := repository.AggregateCompressSkipReasons(ctx, s.db, filter)
	if err != nil {
		return nil, err
	}
	topKeys, err := repository.AggregateCompressTopAPIKeys(ctx, s.db, filter, topN)
	if err != nil {
		return nil, err
	}
	topModels, err := repository.AggregateCompressTopModels(ctx, s.db, filter, topN)
	if err != nil {
		return nil, err
	}
	topProviders, err := repository.AggregateCompressTopProviders(ctx, s.db, filter, topN)
	if err != nil {
		return nil, err
	}
	compressorHits, err := repository.AggregateCompressorHits(ctx, s.db, filter)
	if err != nil {
		return nil, err
	}
	daily, err := repository.AggregateCompressDailySeries(ctx, s.db, filter, opts.location(), now)
	if err != nil {
		return nil, err
	}
	return &CompressStatsResult{
		Totals:              *totals,
		SkipReasonBreakdown: skipReasons,
		TopAPIKeys:          topKeys,
		TopModels:           topModels,
		TopProviders:        topProviders,
		CompressorHits:      compressorHits,
		DailySeries:         daily,
	}, nil
}
