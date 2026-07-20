// Package handler additions for M6.1: dashboard endpoint per PRD §6.6.
// Thin HTTP adapter over DashboardService — all composition lives in the
// service, all SQL lives in the repository. See design doc
// .claude/docs/2026-07-20-m6-analytics-design.md §4.5.
package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/response"
)

// GetDashboard handles GET /api/admin/dashboard. The dashboard is read-only
// and parameterless in M6.1 — every windowing / limit constant is pinned by
// PRD §6.6 and lives in the service, so this handler only translates the
// service error into the project's response envelope.
func GetDashboard(svc *service.DashboardService) gin.HandlerFunc {
	return func(c *gin.Context) {
		data, err := svc.GetDashboard()
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, data)
	}
}
