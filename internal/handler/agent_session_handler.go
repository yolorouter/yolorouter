// Tool-session view endpoints — thin HTTP adapters over the same
// RequestLogService the request-log views use; all SQL lives in the
// repository, all composition in the service.
//
// Two routes, both admin-only (registered on the protected group, and pinned
// by the member-scope route conformance test like every other admin GET):
//
//   - GET /api/admin/agent-sessions            paginated aggregate list,
//     optional agent_client attribution filter
//   - GET /api/admin/agent-sessions/:sessionId full request timeline of one
//     tool session, chronological
package handler

import (
	"errors"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetAgentSessions handles GET /api/admin/agent-sessions — one aggregate row
// per tool session, most recent activity first, server-side paginated.
// Reuses parseAPIKeyPagination (1-indexed, default 20, max 200) since the
// pagination contract is identical across all paginated admin endpoints.
func GetAgentSessions(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, pageSize := parseAPIKeyPagination(c)
		filter := repository.AgentSessionFilter{Page: page, PageSize: pageSize}
		// GetQuery (not Query) so an absent param stays "filter off" (nil)
		// while a present-but-empty value is a real constraint that matches
		// no session — the same pointer convention the request-log list's
		// agent_client filter uses.
		if v, ok := c.GetQuery("agent_client"); ok {
			filter.AgentClient = &v
		}
		items, total, err := svc.ListAgentSessions(&filter)
		if err != nil {
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.PageSuccess(c, total, page, pageSize, items)
	}
}

// GetAgentSessionDetail handles GET /api/admin/agent-sessions/:sessionId —
// the session's attribution plus every request it contains, chronological.
// An unknown session id answers the domain 404 envelope (the session detail
// is a read of existing rows; there is nothing to create implicitly).
func GetAgentSessionDetail(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("sessionId")
		if sessionID == "" {
			response.ParamError(c, "sessionId is required")
			return
		}
		detail, err := svc.GetAgentSessionDetail(sessionID)
		if err != nil {
			if errors.Is(err, errcode.ErrAgentSessionNotFound) {
				response.NotFound(c, errcode.AgentSessionNotFound, errcode.GetMessage(errcode.AgentSessionNotFound))
				return
			}
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, detail)
	}
}
