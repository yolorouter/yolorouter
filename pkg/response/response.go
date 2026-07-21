// Package response provides the unified JSON response envelope used by all
// admin-backend handlers. The proxied /v1/chat/completions traffic itself is
// passed through in the upstream's native shape and does NOT use this
// envelope.
package response

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// Response is the unified response structure.
type Response struct {
	Code      int         `json:"code"`
	Message   string      `json:"message"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp int64       `json:"timestamp"`
}

// PageResponse is the paginated response structure.
type PageResponse struct {
	Total    int64       `json:"total"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	List     interface{} `json:"list"`
}

// Success sends a success response.
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:      errcode.Success,
		Message:   "success",
		Data:      data,
		Timestamp: time.Now().Unix(),
	})
}

// httpStatusForCode maps a business error code to an HTTP status code.
// 10xxx = account/session errors → 401/403 handled by caller via Error(); default 400
// 11xxx = api key errors → 401/429 handled by caller via Error(); default 400
// 12xxx-14xxx = resource/relay errors → 400
// 50xxx = system errors → 500, EXCEPT InvalidParam (50003): despite its
// number falling in the "system error" range, its meaning is a 400 client
// error (a malformed request parameter), not a 500 server fault — special
// case it here so both this function and its only real caller through
// this path, ParamError, get it right in one place. Two call sites
// (internal/handler/auth_handler.go's bindJSON and provider_handler.go's
// parseUintParam) independently discovered and worked around this exact
// bug by calling ErrorStatus(400, ...) directly instead of going through
// Error/ParamError — the root cause belongs here, not repeated as a
// per-caller special case every time a new handler needs it.
func httpStatusForCode(code int) int {
	switch {
	case code == errcode.InvalidParam:
		return http.StatusBadRequest
	case code >= 50000:
		return http.StatusInternalServerError
	case code >= 10000:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// Error sends an error response using the code's default HTTP status.
func Error(c *gin.Context, code int, message string) {
	c.JSON(httpStatusForCode(code), Response{
		Code:      code,
		Message:   message,
		Timestamp: time.Now().Unix(),
	})
}

// ErrorStatus sends an error response with an explicit HTTP status,
// overriding httpStatusForCode. Use for 401/403/429 cases the generic range
// mapping doesn't cover (session invalid, csrf, rate limited, ...).
func ErrorStatus(c *gin.Context, httpStatus int, code int, message string) {
	c.JSON(httpStatus, Response{
		Code:      code,
		Message:   message,
		Timestamp: time.Now().Unix(),
	})
}

// PageSuccess sends a paginated success response.
func PageSuccess(c *gin.Context, total int64, page int, pageSize int, list interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:    errcode.Success,
		Message: "success",
		Data: PageResponse{
			Total:    total,
			Page:     page,
			PageSize: pageSize,
			List:     list,
		},
		Timestamp: time.Now().Unix(),
	})
}

// ParamError sends a parameter error response with a cleaned-up message.
// Gin validation errors like "Key:'CreateProviderRequest.Name' Error:..."
// are rewritten to just the human-readable field constraint.
func ParamError(c *gin.Context, message string) {
	Error(c, errcode.InvalidParam, cleanValidationMessage(message))
}

// cleanValidationMessage rewrites Go struct validation errors into a
// user-friendly form. Non-validation messages are returned as-is.
func cleanValidationMessage(msg string) string {
	if !strings.Contains(msg, "Error:Field validation") {
		return msg
	}

	parts := strings.SplitN(msg, "Error:", 2)
	if len(parts) < 2 {
		return "invalid parameter"
	}
	validationPart := strings.TrimSpace(parts[1])

	fieldStart := strings.Index(validationPart, "'")
	if fieldStart < 0 {
		return "invalid parameter"
	}
	fieldEnd := strings.Index(validationPart[fieldStart+1:], "'")
	if fieldEnd < 0 {
		return "invalid parameter"
	}
	field := validationPart[fieldStart+1 : fieldStart+1+fieldEnd]

	tagMarker := "failed on the '"
	tagIdx := strings.Index(validationPart, tagMarker)
	if tagIdx >= 0 {
		afterTag := validationPart[tagIdx+len(tagMarker):]
		if end := strings.Index(afterTag, "'"); end > 0 {
			tag := afterTag[:end]
			return field + ": " + tag
		}
	}

	return field + ": invalid"
}

// Unauthorized sends a 401 response.
func Unauthorized(c *gin.Context, code int, message string) {
	ErrorStatus(c, http.StatusUnauthorized, code, message)
}

// Forbidden sends a 403 response.
func Forbidden(c *gin.Context, code int, message string) {
	ErrorStatus(c, http.StatusForbidden, code, message)
}

// NotFound sends a 404 response.
func NotFound(c *gin.Context, code int, message string) {
	ErrorStatus(c, http.StatusNotFound, code, message)
}

// TooManyRequests sends a 429 response.
func TooManyRequests(c *gin.Context, code int, message string) {
	ErrorStatus(c, http.StatusTooManyRequests, code, message)
}

// InternalError sends a 500 response.
func InternalError(c *gin.Context, message string) {
	Error(c, errcode.InternalError, message)
}
