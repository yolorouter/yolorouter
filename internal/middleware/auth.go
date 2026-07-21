package middleware

import (
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// AdminIDKey is the gin.Context key RequireAdminSession sets on success —
// handlers read it via c.MustGet(AdminIDKey).(uint).
const AdminIDKey = "admin_id"

// SessionCookieName is the single cookie name used for the whole admin
// session lifecycle (set on login/setup, cleared on logout/password
// change) — see design doc §6. Exported so internal/handler (which writes
// and clears the cookie) and this package (which reads it) share one
// symbol instead of each hardcoding the literal.
const SessionCookieName = "session_id"

// RequireAdminSession gates a route behind a valid, unexpired session
// cookie. On any failure it writes the unified 401 envelope via
// WriteAdminError and aborts — it never lets a request without a resolved
// admin identity reach the next handler.
func RequireAdminSession(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(SessionCookieName)
		if err != nil || token == "" {
			WriteAdminError(c, http.StatusUnauthorized, errcode.AccountSessionInvalid)
			return
		}

		session, err := repository.FindValidSessionByID(db, token, time.Now().UTC())
		if errors.Is(err, gorm.ErrRecordNotFound) {
			WriteAdminError(c, http.StatusUnauthorized, errcode.AccountSessionInvalid)
			return
		}
		if err != nil {
			WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}

		c.Set(AdminIDKey, session.AdminID)
		c.Next()
	}
}
