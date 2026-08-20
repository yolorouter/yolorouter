// Package handler exposes the dashboard endpoint.
// Thin HTTP adapter over DashboardService — all composition lives in the
// service, all SQL lives in the repository.
package handler

import (
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/service/dashboard"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetDashboard handles GET /api/admin/dashboard. The dashboard is read-only
// apart from an optional ?timezone= query param carrying the admin's IANA
// timezone (e.g. "Asia/Shanghai") from the browser's
// Intl.DateTimeFormat().resolvedOptions().timeZone. Used for day-bucket
// windowing so the dashboard's "today" matches the admin's wall clock;
// missing or invalid values fall back to the server's local zone. Every
// other windowing / limit constant is pinned and lives in the service, so
// this handler only translates the service error into the project's response
// envelope.
func GetDashboard(svc *dashboard.DashboardService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var loc *time.Location
		if v, ok := c.Get("timezone"); ok {
			loc = v.(*time.Location)
		}
		// Optional start/end let the dashboard follow a user-selected time
		// range instead of the default "today + 7-day trend".
		var rangeStart, rangeEnd *time.Time
		if !applyTimeQueryParam(c, "start", func(v time.Time) { rangeStart = &v }) {
			return
		}
		if !applyTimeQueryParam(c, "end", func(v time.Time) { rangeEnd = &v }) {
			return
		}
		// Require both-or-neither: a single bound is silently ignored by the
		// service; reject explicitly so the caller knows.
		if (rangeStart == nil) != (rangeEnd == nil) {
			response.ParamError(c, "start and end must be provided together")
			return
		}
		// Reject inverted or zero-length ranges — GetTrendRange would compute
		// a negative day count.
		if rangeStart != nil && rangeEnd != nil && !rangeStart.Before(*rangeEnd) {
			response.ParamError(c, "start must be before end")
			return
		}
		// Optional per-account scope for the traffic-backed sections. A
		// member session is pinned to itself regardless of the query; only
		// a member session (not an admin filtering by ?user_id) loses the
		// deployment-status sections.
		var userID *uint
		if !applyUintQueryParam(c, "user_id", func(v uint) { userID = &v }) {
			return
		}
		scope := middleware.ViewScopeOf(c)
		data, err := svc.GetDashboard(loc, rangeStart, rangeEnd, scope.Resolve(userID), scope.Member, timeNow())
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, data)
	}
}
