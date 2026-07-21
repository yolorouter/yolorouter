// Package handler exposes the analytics endpoints.
// Thin HTTP adapter over AnalyticsService — all composition lives in the
// service, all SQL lives in the repository.
//
// Three routes:
//   - GET /api/admin/analytics/overview  aggregate MetricTotals for filter
//   - GET /api/admin/analytics/report    dimension-grouped aggregates
//   - GET /api/admin/analytics/export    CSV stream of the same report
//
// Filter shape is identical across the three (start/end/api_key_id/model_name/
// provider_id/status); ?dimension selects the report aggregate, ?bucket
// selects the time-bucket granularity for dimension=time only.
package handler

import (
	"errors"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/repository" // for StatusXxx constants only
	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/pkg/csvutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// validAnalyticsDimensions is the wire-level allowlist for ?dimension=.
// Empty defaults to "model" (the most common top-level aggregate); see
// GetAnalyticsReport / ExportAnalyticsCSV.
var validAnalyticsDimensions = map[string]struct{}{
	service.DimensionModel:    {},
	service.DimensionProvider: {},
	service.DimensionCaller:   {},
	service.DimensionTime:     {},
}

// validAnalyticsBuckets is the wire-level allowlist for ?bucket= (only
// meaningful when dimension=time). Empty defaults to "day".
var validAnalyticsBuckets = map[string]struct{}{
	service.BucketDay:  {},
	service.BucketHour: {},
}

// Status-class allowlist is repository.ValidStatusClasses, shared with
// request_log_handler so the two endpoints can't drift.

// GetAnalyticsOverview handles GET /api/admin/analytics/overview — the four
// overview cards (calls / success_rate / cost / unknown-cost-calls) plus
// token totals for the supplied filter window.
func GetAnalyticsOverview(svc *service.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseAnalyticsFilter(c)
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
		data, err := svc.GetOverview(filter, bucket)
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, data)
	}
}

// GetAnalyticsReport handles GET /api/admin/analytics/report. ?dimension=
// picks one of model|provider|caller|time (default model); ?bucket= picks
// day|hour for dimension=time (default day). Other params are the shared
// filter shape.
func GetAnalyticsReport(svc *service.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseAnalyticsFilter(c)
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
		result, err := svc.GetReport(dimension, bucket, filter)
		if err != nil {
			writeAnalyticsServiceError(c, err)
			return
		}
		response.Success(c, result)
	}
}

// ExportAnalyticsCSV handles GET /api/admin/analytics/export — streams the
// report as CSV with a UTF-8 BOM (so Excel auto-detects the encoding and
// renders CJK columns like owner_label / model_name correctly). Filename is
// timestamped so repeated exports don't clobber each other in the browser's
// downloads. Once the BOM is written the response can no longer switch to
// the JSON envelope on a mid-stream error; we surface the failure via
// c.Error + Abort — same convention request_log_handler.go's CSV export
// established.
func ExportAnalyticsCSV(svc *service.AnalyticsService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseAnalyticsFilter(c)
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
		headers, records, err := svc.BuildCSVRecords(dimension, bucket, filter)
		if err != nil {
			writeAnalyticsServiceError(c, err)
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

// parseAnalyticsFilter translates the shared filter query params into a
// service.AnalyticsFilter. Returns false (after writing a 400 envelope) on
// any malformed value; the caller must return immediately on false.
// Reuses applyUintQueryParam / applyTimeQueryParam from request_log_handler
// — the wire contract (RFC3339-only timestamps, plain uint ids) is the same.
func parseAnalyticsFilter(c *gin.Context) (service.AnalyticsFilter, bool) {
	statusClass := c.Query("status")
	if _, ok := repository.ValidStatusClasses[statusClass]; !ok {
		response.ParamError(c, "status must be one of: success, failed, partial, cancelled, rejected")
		return service.AnalyticsFilter{}, false
	}
	filter := service.AnalyticsFilter{
		RequestID:   c.Query("request_id"),
		ModelName:   c.Query("model_name"),
		StatusClass: statusClass,
	}
	if !applyUintQueryParam(c, "api_key_id", func(v uint) { filter.APIKeyID = &v }) {
		return service.AnalyticsFilter{}, false
	}
	if !applyUintQueryParam(c, "provider_id", func(v uint) { filter.ProviderID = &v }) {
		return service.AnalyticsFilter{}, false
	}
	if !applyTimeQueryParam(c, "start", func(v time.Time) { filter.StartTime = &v }) {
		return service.AnalyticsFilter{}, false
	}
	if !applyTimeQueryParam(c, "end", func(v time.Time) { filter.EndTime = &v }) {
		return service.AnalyticsFilter{}, false
	}
	return filter, true
}

// parseDimensionParam returns the dimension query param, defaulting to
// "model" when absent. Returns false (after writing a 400) on an
// unrecognized value.
func parseDimensionParam(c *gin.Context) (string, bool) {
	dimension := c.DefaultQuery("dimension", service.DimensionModel)
	if _, ok := validAnalyticsDimensions[dimension]; !ok {
		response.ParamError(c, "dimension must be one of: model, provider, caller, time")
		return "", false
	}
	return dimension, true
}

// parseBucketParam returns the bucket query param, defaulting to "day" when
// absent. Returns false (after writing a 400) on an unrecognized value.
func parseBucketParam(c *gin.Context) (string, bool) {
	bucket := c.DefaultQuery("bucket", service.BucketDay)
	if _, ok := validAnalyticsBuckets[bucket]; !ok {
		response.ParamError(c, "bucket must be one of: day, hour")
		return "", false
	}
	return bucket, true
}

// writeAnalyticsServiceError maps the service-layer sentinel errors onto
// the project's unified envelope. ErrInvalidBucket / ErrInvalidDimension
// are 400s (bad request shape); anything else is a 500.
//
// dimension/bucket validation mostly happens at the handler (via
// parseDimensionParam / parseBucketParam) so the service sentinels here are
// a defensive second line — easier to keep the mapping than to prove the
// handler is the only entry point.
func writeAnalyticsServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, repository.ErrInvalidBucket),
		errors.Is(err, service.ErrInvalidDimension):
		response.Error(c, errcode.InvalidParam, err.Error())
	default:
		response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
	}
}
