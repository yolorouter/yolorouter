package middleware

import (
	"fmt"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
)

// Recovery recovers from any panic in a later handler, logs it (including
// the call stack at the point of the panic, via debug.Stack() — pkg/logger
// is a verbatim copy of the reference project's implementation and doesn't
// enable zap.AddStacktrace, so without capturing it explicitly here, the
// log would only ever show the panic value itself, not where it happened),
// and — if the response hasn't already been written — writes the unified
// 500 error envelope instead of letting gin's default recovery close the
// connection.
//
// If a panic happens after the handler already started writing the
// response (e.g. a future SSE/streaming handler mid-flush), writing a JSON
// error body on top of already-sent bytes would produce a malformed
// response — worse, to an unsuspecting client it can look like a complete,
// successful response that just happens to be truncated, rather than a
// visible failure (design doc §9). Re-panicking with http.ErrAbortHandler
// is the sentinel net/http's own per-connection recover specifically
// checks for: it aborts the connection (without re-logging — we already
// captured the stack above) instead of silently completing the response as
// if nothing went wrong.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				requestID, _ := c.Get(RequestIDKey)
				logger.Error("panic recovered",
					zap.Any("panic", rec),
					zap.String("stack", string(debug.Stack())),
					zap.String("request_id", fmt.Sprint(requestID)),
					zap.String("path", c.Request.URL.Path),
				)
				if !c.Writer.Written() {
					WriteNamespacedError(c, c.Request.URL.Path, http.StatusInternalServerError, errcode.InternalError)
					return
				}
				panic(http.ErrAbortHandler)
			}
		}()
		c.Next()
	}
}
