package handler

import (
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// GetRateLimits lists the learned 429 evidence rows with provider/key
// names attached. Admin-only by route registration: the rows describe
// upstream accounts, which members never see.
func GetRateLimits(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		rows, err := repository.ListObservedRateLimits(db)
		if err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, gin.H{"list": rows})
	}
}

// DeleteRateLimit removes one learned row — the reset gesture. The next
// 429 against that key learns fresh limits, so an admin resets when the
// upstream's real ceiling has moved beyond what only-down will ever
// record. Idempotent by design: deleting an already-deleted row succeeds,
// because "this row is gone" is exactly the state the caller asked for
// (and a dedicated not-found error code is not worth widening the errcode
// registry for a reset button).
func DeleteRateLimit(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := parseUintParam(c, "id")
		if !ok {
			return
		}
		if err := repository.DeleteObservedRateLimit(db, id); err != nil {
			writeServiceError(c, err)
			return
		}
		response.Success(c, nil)
	}
}
