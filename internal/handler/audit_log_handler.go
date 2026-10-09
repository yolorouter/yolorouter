// Package handler exposes the admin audit-log query endpoints: the
// console's audit page backing store. Thin HTTP adapter over the
// repository query layer — no service in between because there is nothing
// to compose: the row is already self-contained (admin_username is a
// capture-time snapshot, so no live join can ever change what a historical
// row says), the filter's intersection rule lives in the repository, and
// the audit domain needs none of the cross-entity name resolution the
// request-log list keeps a service for.
//
// Two routes, both reads, both deliberately outside the audit capture
// layer's method filter — reading the audit trail must never write to it:
//
//   - GET /api/admin/audit-logs         paginated list + filter
//   - GET /api/admin/audit-logs/export  CSV stream of the current filter
package handler

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/csvutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetAuditLogs handles GET /api/admin/audit-logs — paginated list with the
// console's five filter dimensions (admin / action / resource / time /
// status), each optional, composed as an intersection. Rows serialize as
// the model itself (json tags live there), admin_username included as the
// capture-time snapshot — renaming or deleting the account later never
// changes what history reads. Reuses parseAPIKeyPagination (1-indexed,
// default 20, max 200) since the pagination contract is identical across
// all paginated admin endpoints.
func GetAuditLogs(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, pageSize := parseAPIKeyPagination(c)
		filter, ok := parseAuditLogFilter(c)
		if !ok {
			return
		}
		filter.Page = page
		filter.PageSize = pageSize
		rows, total, err := repository.ListAdminAuditLogsByFilter(db.WithContext(c.Request.Context()), &filter)
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.PageSuccess(c, total, page, pageSize, rows)
	}
}

// ExportAuditLogsCSV handles GET /api/admin/audit-logs/export — streams
// every row matching the current filter as CSV (UTF-8 BOM for Excel CJK
// compatibility, per-cell formula-injection guard, both from
// csvutil.WriteCSV). Same query param shape as the list endpoint, minus
// pagination. Content-Disposition is attachment with a timestamped
// filename so repeated exports don't clobber each other in the browser's
// downloads.
//
// Rows are pulled and built BEFORE any CSV byte goes to the wire (page-
// walked by keyset so concurrent captures can't drift the result set), so
// a DB failure returns a JSON 500 envelope, not a truncated CSV the
// frontend accepts as success. Once the BOM is written the response can no
// longer switch to the JSON envelope on a mid-stream error; that path
// surfaces via c.Error and an aborted connection — the client sees a
// truncated CSV rather than a malformed mixed-format response.
func ExportAuditLogsCSV(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter, ok := parseAuditLogFilter(c)
		if !ok {
			return
		}
		items, err := buildAuditExportRows(db.WithContext(c.Request.Context()), &filter)
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		filename := "admin-audit-logs-" + time.Now().UTC().Format("20060102-150405") + ".csv"
		c.Header("Content-Type", "text/csv; charset=utf-8")
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		if err := csvutil.WriteCSV(c.Writer, auditCSVHeader, auditCSVRecords(items)); err != nil {
			// Headers already committed; can't swap to JSON. Surface via
			// c.Error and abort — the client gets a truncated CSV, but this
			// path is write-time only; all DB work is pre-built.
			_ = c.Error(err)
			c.Abort()
			return
		}
	}
}

// buildAuditExportRows pulls every row matching filter, page-walked by
// keyset (same walk shape the request-log export uses) so the row set is a
// stable snapshot even while the audit capture worker keeps flushing.
func buildAuditExportRows(db *gorm.DB, filter *repository.AdminAuditLogFilter) ([]model.AdminAuditLog, error) {
	const pageSize = 200
	var cursor *repository.AdminAuditCursor
	items := make([]model.AdminAuditLog, 0)
	for {
		rows, err := repository.ListAdminAuditLogsKeyset(db, filter, cursor, pageSize)
		if err != nil {
			return nil, err
		}
		items = append(items, rows...)
		if len(rows) < pageSize {
			return items, nil // last page
		}
		last := rows[len(rows)-1]
		cursor = &repository.AdminAuditCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// auditCSVHeader is the export's one header row. Column order mirrors the
// model's field order so a reader diffing the struct against the CSV walks
// one direction.
var auditCSVHeader = []string{
	"id", "created_at", "admin_user_id", "admin_username", "action",
	"resource_type", "resource_id", "detail", "status", "error_brief",
	"source_ip", "user_agent", "request_id",
}

// auditCSVRecords renders rows as CSV string cells. Every cell goes
// through csvutil.WriteCSV's per-row sanitize, so a snapshot username that
// happens to start with a formula marker cannot execute in a spreadsheet.
// Timestamps are RFC3339 for round-trip parsing.
func auditCSVRecords(rows []model.AdminAuditLog) [][]string {
	records := make([][]string, 0, len(rows))
	for _, r := range rows {
		adminID := ""
		if r.AdminUserID != nil {
			adminID = strconv.FormatUint(uint64(*r.AdminUserID), 10)
		}
		records = append(records, []string{
			strconv.FormatUint(uint64(r.ID), 10),
			r.CreatedAt.UTC().Format(time.RFC3339),
			adminID,
			r.AdminUsername,
			r.Action,
			r.ResourceType,
			r.ResourceID,
			r.Detail,
			r.Status,
			r.ErrorBrief,
			r.SourceIP,
			r.UserAgent,
			r.RequestID,
		})
	}
	return records
}

// parseAuditLogFilter translates the audit endpoints' query params into a
// repository.AdminAuditLogFilter: status validated against the shared
// allowlist, admin_user_id / start / end through the shared typed-param
// helpers, and the string dimensions taken as exact literals. Pagination
// is deliberately NOT parsed here — the list endpoint fills Page/PageSize
// itself afterwards, the CSV export exports unpaginated — so the two
// endpoints share every filter byte and differ only in pagination.
// Returns false (after writing a 400 envelope) on any malformed value.
func parseAuditLogFilter(c *gin.Context) (repository.AdminAuditLogFilter, bool) {
	status := c.Query("status")
	if _, ok := repository.ValidAuditStatuses[status]; !ok {
		response.ParamError(c, "status must be one of: success, denied, error")
		return repository.AdminAuditLogFilter{}, false
	}
	filter := repository.AdminAuditLogFilter{
		Action:       c.Query("action"),
		ResourceType: c.Query("resource_type"),
		ResourceID:   c.Query("resource_id"),
		Status:       status,
	}
	if !applyUintQueryParam(c, "admin_user_id", func(v uint) { filter.AdminUserID = &v }) ||
		!applyTimeQueryParam(c, "start", func(v time.Time) { filter.StartTime = &v }) ||
		!applyTimeQueryParam(c, "end", func(v time.Time) { filter.EndTime = &v }) {
		return repository.AdminAuditLogFilter{}, false
	}
	return filter, true
}
