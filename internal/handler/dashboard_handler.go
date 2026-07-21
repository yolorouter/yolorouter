// Package handler exposes the dashboard endpoint.
// Thin HTTP adapter over DashboardService — all composition lives in the
// service, all SQL lives in the repository.
package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetDashboard handles GET /api/admin/dashboard. The dashboard is read-only
// and parameterless — every windowing / limit constant is pinned
// and lives in the service, so this handler only translates the
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
