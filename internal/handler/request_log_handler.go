// Package handler exposes the request-log list + detail endpoints.
// Thin HTTP adapter over RequestLogService — all composition lives
// in the service, all SQL lives in the repository.
//
// Three routes:
//   - GET /api/admin/request-logs           paginated list + filter
//   - GET /api/admin/request-logs/:requestId single-row detail (no body)
//   - GET /api/admin/request-logs/export    CSV stream of the current filter
package handler

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// Status-class allowlist is repository.ValidStatusClasses, shared with the
// analytics handler so a new bucket lands in both endpoints at once.

// GetRequestLogs handles GET /api/admin/request-logs — paginated list with
// filter, JOIN'd username / provider_name, and a derived status_class on
// every row. Reuses parseAPIKeyPagination (1-indexed, default 20, max 200)
// since the pagination contract is identical across all paginated admin
// endpoints.
func GetRequestLogs(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, pageSize := parseAPIKeyPagination(c)
		filter, ok := parseRequestLogFilter(c)
		if !ok {
			return
		}
		filter.Page = page
		filter.PageSize = pageSize
		items, total, err := svc.ListRequestLogs(&filter)
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.PageSuccess(c, total, page, pageSize, items)
	}
}

// GetRequestLogDetail handles GET /api/admin/request-logs/:requestId — a
// single row with attempts_detail parsed into []AttemptRecord. This
// returns metadata only; request/response bodies are served separately.
func GetRequestLogDetail(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.Param("requestId")
		if requestID == "" {
			response.ParamError(c, "requestId is required")
			return
		}
		detail, err := svc.GetRequestLogDetail(requestID)
		if err != nil {
			if errors.Is(err, errcode.ErrRequestLogNotFound) {
				response.NotFound(c, errcode.RequestLogNotFound, errcode.GetMessage(errcode.RequestLogNotFound))
				return
			}
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, detail)
	}
}

// GetRequestLogBodyStream handles GET /api/admin/request-logs/:requestId/body/stream
// — serves the raw sent-SSE bytes captured on disk for a streaming request
// (gateway/stream.go's per-chunk disk append). Deliberately
// separate from GetRequestLogDetail's JSON envelope: a stream body can be
// arbitrarily large (up to the 1GiB backstop) and is plain text, not JSON,
// so it is served as its own endpoint with http.ServeContent (Range/If-*
// support) rather than being embedded inline in the detail DTO.
//
// filepath.Base(path) strips any directory components off the stored
// stream_body_path before joining it under bodiesDir — the path column is
// meant to hold a bare filename (e.g. "req_x.stream"), but treating it
// defensively as untrusted input keeps a corrupted/malicious row from
// escaping bodiesDir via "../" traversal.
func GetRequestLogBodyStream(svc *requestlog.RequestLogService, bodiesDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.Param("requestId")
		path, err := svc.GetStreamBodyPath(requestID)
		if err != nil || path == "" {
			c.Status(http.StatusNotFound)
			return
		}
		abs := filepath.Join(bodiesDir, filepath.Base(path))
		f, err := os.Open(abs)
		if err != nil {
			c.Status(http.StatusNotFound)
			return
		}
		defer func() { _ = f.Close() }()
		c.Header("Content-Type", "text/plain; charset=utf-8")
		http.ServeContent(c.Writer, c.Request, "", time.Time{}, f)
	}
}

// ExportRequestLogsCSV handles GET /api/admin/request-logs/export — streams
// every row matching the current filter as CSV (UTF-8 BOM for Excel CJK
// compatibility). Same query param shape as the list endpoint, minus
// pagination. Content-Disposition is attachment with a timestamped filename
// so repeated exports don't clobber each other in the browser's downloads.
//
// Once the BOM is written the response can no longer switch to the JSON
// envelope on a mid-stream error; we surface the failure via c.Error and
// abort the connection — the client sees a truncated CSV rather than a
// malformed mixed-format response.
func ExportRequestLogsCSV(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseRequestLogFilter(c)
		if !ok {
			return
		}

		// Build rows BEFORE writing any CSV bytes to the wire so a DB /
		// name-resolution failure returns a JSON 500 envelope, not a
		// truncated CSV that the frontend accepts as success. Only after the
		// full row set is built do we
		// commit the HTTP 200 + CSV headers + body.
		items, err := svc.BuildExportRows(&filter)
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		filename := "request-logs-" + time.Now().UTC().Format("20060102-150405") + ".csv"
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		if err := svc.WriteCSVRows(items, c.Writer); err != nil {
			// Headers already committed; can't swap to JSON. Surface via
			// c.Error and abort — the client gets a truncated CSV, but this
			// path is now rare (write-time only; all DB work is pre-built).
			_ = c.Error(err)
			c.Abort()
			return
		}
	}
}

// parseStatusClassParam validates the ?status= query param against the
// shared allowlist (repository.ValidStatusClasses; "" = all buckets is
// valid). Returns false (after writing a 400) on an unrecognized value.
// Shared by the request-log list, request-log export, and analytics
// endpoints so the check and its message cannot drift apart.
func parseStatusClassParam(c *gin.Context) (string, bool) {
	statusClass := c.Query("status")
	if _, ok := repository.ValidStatusClasses[statusClass]; !ok {
		response.ParamError(c, "status must be one of: success, failed, partial, cancelled, rejected")
		return "", false
	}
	return statusClass, true
}

// parseRequestLogFilter translates the shared filter query params into a
// repository.RequestLogFilter: status/source validation, the direct string
// literal, and the optional typed params (applyRequestLogFilterParams).
// Pagination is deliberately NOT parsed here — the list endpoint fills
// Page/PageSize itself afterwards, the CSV export exports unpaginated — so
// the two endpoints share every filter byte and differ only in pagination.
// Returns false (after writing a 400 envelope) on any malformed value.
func parseRequestLogFilter(c *gin.Context) (repository.RequestLogFilter, bool) {
	statusClass, ok := parseStatusClassParam(c)
	if !ok {
		return repository.RequestLogFilter{}, false
	}
	source := c.Query("source")
	if source != "" && source != model.RequestLogSourceVisionFallback && source != model.RequestLogSourceCallerFilter {
		response.ParamError(c, "source must be one of: caller, vision_fallback")
		return repository.RequestLogFilter{}, false
	}
	filter := repository.RequestLogFilter{
		RequestID:   c.Query("request_id"),
		ModelName:   c.Query("model_name"),
		KeyPrefix:   c.Query("key_prefix"),
		Source:      source,
		RequestPath: c.Query("request_path"),
		StatusClass: statusClass,
	}
	if !applyRequestLogFilterParams(c, &filter) {
		return repository.RequestLogFilter{}, false
	}
	return filter, true
}

// applyRequestLogFilterParams parses the optional api_key_id / provider_id
// / is_stream / start / end query params into filter. Returns false (after
// writing a 400 envelope) when any value is malformed; the caller must
// `return` immediately on false. The callback-based apply* helpers keep
// "absent", "valid", and "wrote-400" distinguishable without the caller
// inspecting c.Writer.Written().
func applyRequestLogFilterParams(c *gin.Context, filter *repository.RequestLogFilter) bool {
	return applyUintQueryParam(c, "api_key_id", func(v uint) { filter.APIKeyID = &v }) &&
		applyUintQueryParam(c, "user_id", func(v uint) { filter.UserID = &v }) &&
		applyUintQueryParam(c, "provider_id", func(v uint) { filter.ProviderID = &v }) &&
		applyBoolQueryParam(c, "is_stream", func(v bool) { filter.IsStream = &v }) &&
		applyBoolQueryParam(c, "cost_known", func(v bool) { filter.CostKnown = &v }) &&
		applyTimeQueryParam(c, "start", func(v time.Time) { filter.StartTime = &v }) &&
		applyTimeQueryParam(c, "end", func(v time.Time) { filter.EndTime = &v })
}

// applyUintQueryParam parses an optional uint query param. Returns true
// when the caller should continue; on a malformed value it writes a 400
// envelope and returns false. Absent param = no-op (setter not called).
func applyUintQueryParam(c *gin.Context, key string, setter func(uint)) bool {
	if c.Query(key) == "" {
		return true
	}
	v, ok := parseUintQueryValue(c, key)
	if !ok {
		return false
	}
	setter(v)
	return true
}

// requireUintQuery is applyUintQueryParam's mandatory sibling, for a look-up
// whose subject arrives in the query string rather than the path (see
// GetCandidateSuggestPrice). Absent is reported separately from unusable, so a
// client is never told a value it did supply is missing. Everything it cannot
// use — non-numeric, negative, zero — gets one message stating the whole
// contract, rather than the optional helper's "non-negative" wording, which
// would invite a retry with the zero this function also rejects.
func requireUintQuery(c *gin.Context, key string) (uint, bool) {
	raw := c.Query(key)
	if raw == "" {
		response.ParamError(c, key+" is required")
		return 0, false
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 {
		response.ParamError(c, key+" must be a positive integer")
		return 0, false
	}
	return uint(v), true
}

// parseUintQueryValue parses one present-but-possibly-malformed uint query
// value, writing the 400 envelope itself so both callers word it identically.
func parseUintQueryValue(c *gin.Context, key string) (uint, bool) {
	v, err := strconv.ParseUint(c.Query(key), 10, 64)
	if err != nil {
		response.ParamError(c, key+" must be a non-negative integer")
		return 0, false
	}
	return uint(v), true
}

// applyBoolQueryParam parses an optional bool query param accepting the
// canonical wire forms "true" / "false". Any other value writes a 400 and
// returns false; absent param = no-op.
func applyBoolQueryParam(c *gin.Context, key string, setter func(bool)) bool {
	raw := c.Query(key)
	if raw == "" {
		return true
	}
	switch raw {
	case "true":
		setter(true)
		return true
	case "false":
		setter(false)
		return true
	default:
		response.ParamError(c, key+" must be true or false")
		return false
	}
}

// applyTimeQueryParam parses an optional RFC3339 timestamp query param.
// Other formats are rejected with a 400 — RFC3339 is the only format the
// frontend emits and accepting e.g. Unix seconds would just paper over a
// frontend bug.
func applyTimeQueryParam(c *gin.Context, key string, setter func(time.Time)) bool {
	raw := c.Query(key)
	if raw == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		response.ParamError(c, key+" must be an RFC3339 timestamp")
		return false
	}
	setter(t)
	return true
}
