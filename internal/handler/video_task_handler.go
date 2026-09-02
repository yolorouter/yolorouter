// The video task admin list: a read-only window over the task domain.
// The row is the billing evidence and the lifecycle record; this endpoint
// only ever read it. Caller-facing task reads (ownership, lazy refresh)
// belong to the /v1/videos resource routes, not here.
package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetVideoTasks handles GET /api/admin/video-tasks — paginated, newest
// first, filterable by api key, model, and status. Reuses
// parseAPIKeyPagination because the pagination contract is identical
// across all paginated admin endpoints.
func GetVideoTasks(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		filter := repository.ListVideoTasksFilter{Status: c.Query("status")}
		if v, err := strconv.ParseUint(c.Query("api_key_id"), 10, 64); err == nil && v > 0 {
			filter.APIKeyID = uint(v)
		}
		if v, err := strconv.ParseUint(c.Query("model_id"), 10, 64); err == nil && v > 0 {
			filter.ModelID = uint(v)
		}
		filter.Page, filter.PageSize = parseAPIKeyPagination(c)
		tasks, total, err := repository.ListVideoTasks(db.WithContext(c.Request.Context()), filter)
		if err != nil {
			response.Error(c, errcode.InternalError, "list video tasks failed")
			return
		}
		response.Success(c, gin.H{"items": tasks, "total": total, "page": filter.Page, "page_size": filter.PageSize})
	}
}
