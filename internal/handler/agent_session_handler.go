// Tool-session view endpoints — thin HTTP adapters over the same
// RequestLogService the request-log views use; all SQL lives in the
// repository, all composition in the service.
//
// Four routes, all admin-only (registered on the protected group, and pinned
// by the member-scope route conformance test like every other admin GET):
//
//   - GET /api/admin/agent-sessions            paginated aggregate list,
//     optional agent_client attribution filter
//   - GET /api/admin/agent-sessions/:sessionId full request timeline of one
//     tool session, chronological
//   - GET /api/admin/agent-sessions/:sessionId/transcript
//     the session's deduplicated replay transcript (aggregate + system
//     timeline metadata + one cursor-paginated page of blocks), assembled by
//     the service over the pure replay pass
//   - GET /api/admin/agent-sessions/:sessionId/transcript/system
//     the system timeline's raw values (the on-expand fetch for the
//     collapsed system group)
package handler

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/requestlog"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetAgentSessions handles GET /api/admin/agent-sessions — one aggregate row
// per tool session, most recent activity first, server-side paginated.
// Reuses parseAPIKeyPagination (1-indexed, default 20, max 200) — the shared
// page/page_size contract of the offset-paginated list endpoints. Note this
// is no longer "every" paginated endpoint's contract: the transcript route
// below deliberately deviates with cursor pagination (see
// GetAgentSessionTranscript for the declared deviation).
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

// GetAgentSessionTranscript handles GET
// /api/admin/agent-sessions/:sessionId/transcript — the session replay as a
// cursor-paginated payload: the session aggregate and system timeline
// METADATA arrive whole (the header bar's numbers are session-wide, the
// system raw text is fetched from /transcript/system on expand), while the
// blocks arrive one page at a time.
//
// DEVIATION FROM THE UNIFIED PAGINATION CONTRACT: this endpoint uses
// cursor + byte-budget pagination, NOT the page/page_size offset contract
// parseAPIKeyPagination serves. Offset semantics cannot express "continue
// toward older from where I stopped" or "start at this anchor block" — both
// of which the replay page needs.
//
// Query parameters:
//
//   - limit: block-count cap. Default 20, max 200; anything non-numeric,
//     non-positive, or over 200 silently falls back to 20 (the same
//     lenient-fallback precedent the page/page_size params set).
//   - before: block cursor (request_id) — the blocks strictly OLDER than the
//     cursor (exclusive). The upscroll continuation.
//   - after: block cursor (request_id) — the blocks from the cursor ONWARD
//     (inclusive). The #req-<id> anchor entry and the toward-newer
//     continuation share it.
//   - No cursor: the latest blocks (the first screen lands on the newest
//     turn).
//   - A cursor that names no block of the session answers an empty page
//     with both has_more flags false (a stale deep link must not explode).
//   - before and after together: 400 ParamError — one call pages one way.
//
// An unknown session id answers the domain 404 envelope, the same convention
// the detail endpoint holds.
func GetAgentSessionTranscript(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("sessionId")
		if sessionID == "" {
			response.ParamError(c, "sessionId is required")
			return
		}
		before := c.Query("before")
		after := c.Query("after")
		if before != "" && after != "" {
			response.ParamError(c, "before and after cannot be combined")
			return
		}
		// A non-numeric limit stays 0; the slicer normalizes 0 (and any
		// out-of-range value) to the default — the boundary rule lives in
		// one place.
		limit := 0
		if v, err := strconv.Atoi(c.Query("limit")); err == nil {
			limit = v
		}
		transcript, err := svc.GetAgentSessionTranscript(sessionID, requestlog.AgentSessionTranscriptPage{
			Limit:  limit,
			Before: before,
			After:  after,
		})
		if err != nil {
			if errors.Is(err, errcode.ErrAgentSessionNotFound) {
				response.NotFound(c, errcode.AgentSessionNotFound, errcode.GetMessage(errcode.AgentSessionNotFound))
				return
			}
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, transcript)
	}
}

// GetAgentSessionTranscriptSystem handles GET
// /api/admin/agent-sessions/:sessionId/transcript/system — the full
// deduplicated system-prompt timeline with its raw values, fetched when the
// collapsed system group is expanded. Index-aligned with the transcript's
// system metadata list (both come from the same assembly). An unknown
// session id answers the domain 404 envelope, like the transcript route.
func GetAgentSessionTranscriptSystem(svc *requestlog.RequestLogService) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Param("sessionId")
		if sessionID == "" {
			response.ParamError(c, "sessionId is required")
			return
		}
		system, err := svc.GetAgentSessionTranscriptSystem(sessionID)
		if err != nil {
			if errors.Is(err, errcode.ErrAgentSessionNotFound) {
				response.NotFound(c, errcode.AgentSessionNotFound, errcode.GetMessage(errcode.AgentSessionNotFound))
				return
			}
			response.Error(c, errcode.InternalError, errcode.GetMessage(errcode.InternalError))
			return
		}
		response.Success(c, system)
	}
}
