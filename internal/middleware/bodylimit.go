package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// BodySizeLimit wraps the request body with http.MaxBytesReader. Attach it
// per route group (1MiB for admin JSON, 20MiB for the gateway body).
func BodySizeLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}
