// Package middleware provides shared gin middleware: request identification,
// access logging, panic recovery, and the unified /api/* error envelope.
package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const RequestIDKey = "request_id"

// RequestID must be the first middleware registered — every later
// middleware (access log, recovery, error envelopes) reads the value it
// sets. v0.1 always generates its own ID and does not trust a
// client-supplied X-Request-Id header.
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := uuid.NewString()
		c.Set(RequestIDKey, id)
		c.Writer.Header().Set("X-Request-Id", id)
		c.Next()
	}
}
