// Package handler exposes the analytics endpoints.
// Thin HTTP adapter over AnalyticsService — all composition lives in the
// service, all SQL lives in the repository.
//
// Five routes:
//   - GET /api/admin/analytics/overview        aggregate MetricTotals for filter
//   - GET /api/admin/analytics/report          dimension-grouped aggregates
//   - GET /api/admin/analytics/export          CSV stream of the same report
//   - GET /api/admin/analytics/compress-stats  input-compression roll-up
//   - GET /api/admin/analytics/concise-output-projection  priced output
//     volume + the window's projected savings for the cost-optimization
//     page's concise-output card
//
// Filter shape is identical across the five (start/end/api_key_id/model_name/
// provider_id/status); ?dimension selects the report aggregate, ?bucket
// selects the time-bucket granularity for dimension=time only, ?limit
// selects the per-api-key Top-N row count for compress-stats.
package handler

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/repository" // for the RequestLogFilter type
	"github.com/yolorouter/yolorouter/internal/service/analytics"
	"github.com/yolorouter/yolorouter/pkg/csvutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// validAnalyticsBuckets is the wire-level allowlist for ?bucket= (only
// meaningful when dimension=time). Empty defaults to "day".
var validAnalyticsBuckets = map[string]struct{}{
	analytics.BucketDay:  {},
	analytics.BucketHour: {},
}

// GetAnalyticsOverview handles GET /api/admin/analytics/overview — the four
// overview cards (calls / success_rate / cost / unknown-cost-calls) plus
// token totals for the supplied filter window.
func GetAnalyticsOverview(svc *analytics.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, opts, ok := parseAnalyticsFilter(c)
		if !ok {
			return
		}
		// bucket is parsed even though overview doesn't bucket itself: the
		// service uses it to pick the SAME range cap the report will use, so
		// the overview cards match the time-dimension report's window.
		bucket, ok := parseBucketParam(c)
		if !ok {
			return
		}
		data, err := svc.GetOverview(&filter, opts, bucket, timeNow())
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, data)
	}
}

// GetAnalyticsReport handles GET /api/admin/analytics/report. ?dimension=
// picks a report dimension (default model; the legal set lives in the
// service's dimension vocabulary); ?bucket= picks day|hour for
// dimension=time (default day). Other params are the shared filter shape.
func GetAnalyticsReport(svc *analytics.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, opts, ok := parseAnalyticsFilter(c)
		if !ok {
			return
		}
		dimension, ok := parseDimensionParam(c)
		if !ok {
			return
		}
		bucket, ok := parseBucketParam(c)
		if !ok {
			return
		}
		result, err := svc.GetReport(c.Request.Context(), dimension, bucket, &filter, opts, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

// ExportAnalyticsCSV handles GET /api/admin/analytics/export — streams the
// report as CSV with a UTF-8 BOM (so Excel auto-detects the encoding and
// renders CJK columns like username / model_name correctly). Filename is
// timestamped so repeated exports don't clobber each other in the browser's
// downloads. Once the BOM is written the response can no longer switch to
// the JSON envelope on a mid-stream error; we surface the failure via
// c.Error + Abort — same convention request_log_handler.go's CSV export
// established.
func ExportAnalyticsCSV(svc *analytics.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, opts, ok := parseAnalyticsFilter(c)
		if !ok {
			return
		}
		dimension, ok := parseDimensionParam(c)
		if !ok {
			return
		}
		bucket, ok := parseBucketParam(c)
		if !ok {
			return
		}

		// Build BEFORE committing HTTP 200 / BOM so a build failure (bad
		// dimension/bucket, DB error) returns a JSON envelope, not a truncated
		// CSV reported as success (same pattern as request-log export).
		headers, records, err := svc.BuildCSVRecords(c.Request.Context(), dimension, bucket, &filter, opts, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		filename := "analytics-" + dimension + "-" + time.Now().UTC().Format("20060102-150405") + ".csv"
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		if err := csvutil.WriteCSV(c.Writer, headers, records); err != nil {
			// Headers already committed; can't swap to JSON. Surface via
			// c.Error and abort — write-time failures only (build is done).
			_ = c.Error(err)
			c.Abort()
			return
		}
	}
}

// GetCompressStats handles GET /api/admin/analytics/compress-stats — the
// input-compression roll-up. ?limit= sets the per-api-key Top-N row count
// (default 5, capped at 20). Other params are the shared filter shape.
func GetCompressStats(svc *analytics.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, opts, ok := parseAnalyticsFilter(c)
		if !ok {
			return
		}
		topN, ok := parseTopNParam(c)
		if !ok {
			return
		}
		result, err := svc.GetCompressStats(c.Request.Context(), &filter, opts, topN, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

// GetCacheStats handles GET /api/admin/analytics/cache-stats — the verified
// cache economics roll-up behind the dashboard's cache KPI cards (token
// sums, read saving / write premium, and the unsupported-provider
// disclosure; per-dimension cache figures ride on /analytics/report
// instead). Shares the analytics filter shape; no extra params.
func GetCacheStats(svc *analytics.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, opts, ok := parseAnalyticsFilter(c)
		if !ok {
			return
		}
		result, err := svc.GetCacheStats(c.Request.Context(), &filter, opts, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

// GetConciseOutputProjection handles GET /api/admin/analytics/
// concise-output-projection — the priced output-volume roll-up and the
// window's projected saved cost and saved output tokens behind the
// cost-optimization page's concise-output card. Shares the analytics
// filter shape; no extra params.
func GetConciseOutputProjection(svc *analytics.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, opts, ok := parseAnalyticsFilter(c)
		if !ok {
			return
		}
		result, err := svc.GetConciseOutputProjection(c.Request.Context(), &filter, opts, timeNow())
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

// parseAnalyticsFilter translates the shared filter query params into a
// repository.RequestLogFilter plus the analytics-specific call options.
// Returns false (after writing a 400 envelope) on any malformed value; the
// caller must return immediately on false. Reuses parseStatusClassParam /
// applyUintQueryParam / applyTimeQueryParam from request_log_handler —
// status validation is shared with the request-log endpoints, backed by
// repository.ValidStatusClasses, and the wire contract (RFC3339-only
// timestamps, plain uint ids) is the same.
func parseAnalyticsFilter(c *gin.Context) (repository.RequestLogFilter, analytics.AnalyticsOptions, bool) {
	statusClass, ok := parseStatusClassParam(c)
	if !ok {
		return repository.RequestLogFilter{}, analytics.AnalyticsOptions{}, false
	}
	filter := repository.RequestLogFilter{
		RequestID:   c.Query("request_id"),
		ModelName:   c.Query("model_name"),
		StatusClass: statusClass,
	}
	var opts analytics.AnalyticsOptions
	// Opt-in: only the analytics report page asks for failover counts;
	// the cost pages hit the same endpoint and skip the scan behind it.
	opts.WithFailovers = c.Query("with_failovers") == "1"
	if !applyUintQueryParam(c, "api_key_id", func(v uint) { filter.APIKeyID = &v }) {
		return repository.RequestLogFilter{}, analytics.AnalyticsOptions{}, false
	}
	if !applyUintQueryParam(c, "user_id", func(v uint) { filter.UserID = &v }) {
		return repository.RequestLogFilter{}, analytics.AnalyticsOptions{}, false
	}
	if !applyUintQueryParam(c, "provider_id", func(v uint) { filter.ProviderID = &v }) {
		return repository.RequestLogFilter{}, analytics.AnalyticsOptions{}, false
	}
	if !applyTimeQueryParam(c, "start", func(v time.Time) { filter.StartTime = &v }) {
		return repository.RequestLogFilter{}, analytics.AnalyticsOptions{}, false
	}
	if !applyTimeQueryParam(c, "end", func(v time.Time) { filter.EndTime = &v }) {
		return repository.RequestLogFilter{}, analytics.AnalyticsOptions{}, false
	}
	if loc, ok := c.Get("timezone"); ok {
		opts.Location = loc.(*time.Location)
	}
	// A member's analytics are pinned to their own rows and never carry
	// the provider dimension (upstream identities are operator
	// information): the pinned view overrides any user_id the query
	// smuggled in, and provider-scoped filtering plus the failover scan
	// are stripped rather than trusted. This block must stay LAST, after
	// every query-param assignment — stripping before a param is parsed
	// would silently re-admit it. The pin spans BOTH returns: the
	// UserID/ProviderID half lives on the filter, the WithFailovers half
	// on the options — losing the options half would let a member
	// trigger the full failover scan.
	if scope := middleware.ViewScopeOf(c); scope.Member {
		filter.UserID = scope.Resolve(filter.UserID)
		filter.ProviderID = nil
		opts.WithFailovers = false
	}
	return filter, opts, true
}

// parseTopNParam parses the optional ?limit= query param used by compress-stats
// for the per-api-key Top-N row count. Defaults to analytics.DefaultCompressTopN
// when absent; clamped to analytics.MaxCompressTopN. Returns false (after
// writing a 400) when the value is present but not a positive integer — same
// shape as applyUintQueryParam but with a ceiling.
func parseTopNParam(c *gin.Context) (int, bool) {
	raw := c.Query("limit")
	if raw == "" {
		return analytics.DefaultCompressTopN, true
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		response.ParamError(c, "limit must be a positive integer")
		return 0, false
	}
	n := int(v)
	if n > analytics.MaxCompressTopN {
		n = analytics.MaxCompressTopN
	}
	return n, true
}

// parseDimensionParam returns the dimension query param, defaulting to
// "model" when absent. The allowlist and the 400 text derive from the
// service's single dimension vocabulary (analytics.IsValidDimension /
// analytics.DimensionList) — there is no handler-side copy to drift.
// Returns false (after writing a 400) on an unrecognized value, or (after
// writing a 403) when a member requests the provider dimension —
// per-provider aggregates name the upstream vendors, which is operator
// information members never see.
func parseDimensionParam(c *gin.Context) (string, bool) {
	dimension := c.DefaultQuery("dimension", analytics.DimensionModel)
	if !analytics.IsValidDimension(dimension) {
		response.ParamError(c, "dimension must be one of: "+analytics.DimensionList())
		return "", false
	}
	if dimension == analytics.DimensionProvider && middleware.ViewScopeOf(c).Member {
		middleware.WriteAdminError(c, http.StatusForbidden, errcode.AccountPageForbidden)
		return "", false
	}
	return dimension, true
}

// parseBucketParam returns the bucket query param, defaulting to "day" when
// absent. Returns false (after writing a 400) on an unrecognized value.
func parseBucketParam(c *gin.Context) (string, bool) {
	bucket := c.DefaultQuery("bucket", analytics.BucketDay)
	if _, ok := validAnalyticsBuckets[bucket]; !ok {
		response.ParamError(c, "bucket must be one of: day, hour")
		return "", false
	}
	return bucket, true
}
