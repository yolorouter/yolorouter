package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/middleware"
	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
	"github.com/yolorouter/yolorouter-ce/pkg/response"
)

type setupRequest struct {
	Username string `json:"username" binding:"required,min=3,max=32,alnum_dash"`
	Password string `json:"password" binding:"required,min=10,alnum_mixed,bcrypt_len"`
}

type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required,min=10,alnum_mixed,bcrypt_len"`
}

// writeSessionCookie centralizes every cookie attribute (design doc §6) —
// Setup/Login/Logout/ChangePassword must never diverge on
// Path/HttpOnly/SameSite by accident. Pass a positive maxAge to set the
// cookie, or -1 to clear it (the standard net/http convention for cookie
// deletion).
func writeSessionCookie(c *gin.Context, sessionID string, maxAge int) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(middleware.SessionCookieName, sessionID, maxAge, "/", "", false, true)
}

// bindJSON decodes the request body into req, writing the right error
// envelope and returning false on failure — callers must return
// immediately when it returns false. The /api/admin group is capped at
// 1MiB (middleware.BodySizeLimit, wired in internal/router/router.go); a
// body over that limit surfaces here as *http.MaxBytesError from
// ShouldBindJSON's underlying reader, which must map to the project's
// existing 413/RequestEntityTooLarge envelope — not the generic 400 every
// other bind failure (missing field, wrong type, failed validator tag)
// gets. That generic 400 is a deliberate, tested contract (see e.g.
// TestSetupRejectsPasswordLongerThanBcryptLimit) — pkg/response.ParamError
// isn't used here even though every reference-project handler calls it,
// because ParamError's status comes from httpStatusForCode(InvalidParam),
// which maps to 500 in this copied package (InvalidParam=50003 falls in
// the "system error" 50xxx bucket) and would turn every validation
// failure into a misleading 500.
//
// Bind failures come in four shapes that all need their raw text kept
// away from the client: a failed validator tag (err.Error() reads "Key:
// 'setupRequest.Password' Error:Field validation for ..."), handled by
// cleanBindValidationError; a JSON type mismatch (e.g. a string field
// sent as a number), reported as a *json.UnmarshalTypeError whose
// Error() text embeds the Go struct name directly ("cannot unmarshal
// number into Go struct field setupRequest.username of type string"); a
// malformed or empty body (io.EOF for an empty body,
// io.ErrUnexpectedEOF for a truncated one, *json.SyntaxError for
// anything else unparseable), whose raw text ("EOF", "unexpected end of
// JSON input", "invalid character '}' looking for beginning of object
// key string") is just as much an internal parser detail as the other
// shapes; and a stalled body read past cmd/yolorouter-ce/serve.go's
// http.Server.ReadTimeout, which surfaces as a net.Error with
// Timeout()==true (e.g. "read tcp 127.0.0.1:8080->...: i/o timeout") —
// that text can include local/remote socket details and must be
// sanitized exactly like the others.
func bindJSON(c *gin.Context, req interface{}) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			middleware.WriteAdminError(c, http.StatusRequestEntityTooLarge, errcode.RequestEntityTooLarge)
			return false
		}
		var unmarshalTypeErr *json.UnmarshalTypeError
		if errors.As(err, &unmarshalTypeErr) {
			response.ErrorStatus(c, http.StatusBadRequest, errcode.InvalidParam, cleanUnmarshalTypeError(unmarshalTypeErr))
			return false
		}
		var syntaxErr *json.SyntaxError
		var netErr net.Error
		isNetTimeout := errors.As(err, &netErr) && netErr.Timeout()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &syntaxErr) || isNetTimeout {
			response.ErrorStatus(c, http.StatusBadRequest, errcode.InvalidParam, "invalid request body")
			return false
		}
		response.ErrorStatus(c, http.StatusBadRequest, errcode.InvalidParam, cleanBindValidationError(err.Error()))
		return false
	}
	return true
}

// GetAuthState reports whether first-run setup has already been completed
// — the frontend router guard uses this to decide between the setup page
// and the login page.
func GetAuthState(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		initialized, err := service.CheckState(db)
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, gin.H{"initialized": initialized})
	}
}

// PostSetup creates the first admin and signs them in.
func PostSetup(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req setupRequest
		if !bindJSON(c, &req) {
			return
		}

		admin, sessionID, err := service.Setup(db, req.Username, req.Password, time.Now().UTC())
		if errors.Is(err, errcode.ErrAccountSetupAlreadyDone) {
			middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountSetupAlreadyDone)
			return
		}
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}

		writeSessionCookie(c, sessionID, int(service.SessionTTL.Seconds()))
		response.Success(c, gin.H{"username": admin.Username})
	}
}

// PostLogin verifies username+password and, on success, issues a session.
// limiter caps the number of in-flight bcrypt comparisons (see
// middleware.Semaphore's doc comment) — acquired only around the
// service.Login call, after bindJSON has already fully read the request
// body, so a slow/stalled request body can't hold a slot hostage.
func PostLogin(db *gorm.DB, limiter *middleware.Semaphore) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req loginRequest
		if !bindJSON(c, &req) {
			return
		}

		if !limiter.TryAcquire() {
			middleware.WriteAdminError(c, http.StatusTooManyRequests, errcode.ServiceUnavailable)
			return
		}
		defer limiter.Release()

		admin, sessionID, err := service.Login(db, req.Username, req.Password, time.Now().UTC())
		var lockedErr *service.LockedError
		switch {
		case errors.As(err, &lockedErr):
			// AccountLoginLocked is the one error response that carries an
			// extra "locked_until" field (design doc §5).
			middleware.WriteAdminErrorWithData(c, http.StatusForbidden, errcode.AccountLoginLocked,
				gin.H{"locked_until": lockedErr.LockedUntil.Unix()})
			return
		case errors.Is(err, errcode.ErrAccountInvalidCredentials):
			middleware.WriteAdminError(c, http.StatusUnauthorized, errcode.AccountInvalidCredentials)
			return
		case err != nil:
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}

		writeSessionCookie(c, sessionID, int(service.SessionTTL.Seconds()))
		response.Success(c, gin.H{"username": admin.Username})
	}
}

// PostLogout deletes the caller's session and clears the cookie.
func PostLogout(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, _ := c.Cookie(middleware.SessionCookieName)
		if token != "" {
			if err := service.Logout(db, token); err != nil {
				middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
				return
			}
		}
		writeSessionCookie(c, "", -1)
		response.Success(c, nil)
	}
}

// GetMe returns the currently logged-in admin's username.
func GetMe(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		adminID := c.MustGet(middleware.AdminIDKey).(uint)
		admin, err := service.Me(db, adminID)
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		response.Success(c, gin.H{"username": admin.Username})
	}
}

// PutPassword changes the caller's own password and forces re-login.
func PutPassword(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req changePasswordRequest
		if !bindJSON(c, &req) {
			return
		}

		adminID := c.MustGet(middleware.AdminIDKey).(uint)
		err := service.ChangePassword(db, adminID, req.CurrentPassword, req.NewPassword, time.Now().UTC())
		if errors.Is(err, errcode.ErrAccountInvalidCredentials) {
			middleware.WriteAdminError(c, http.StatusUnauthorized, errcode.AccountInvalidCredentials)
			return
		}
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}

		writeSessionCookie(c, "", -1)
		response.Success(c, nil)
	}
}
