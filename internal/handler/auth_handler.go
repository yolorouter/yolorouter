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

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
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

// writeSessionCookie centralizes every cookie attribute —
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
// still isn't used here, for a narrower reason than before: ParamError's
// cleanValidationMessage only understands Go validator-tag failure text,
// not the JSON-type-mismatch/malformed-body shapes handled below, so this
// function's own cleanBindValidationError remains the right tool. (An
// earlier version of this comment cited a since-fixed bug where
// httpStatusForCode(InvalidParam) mapped to 500 instead of 400 — that's
// now special-cased in pkg/response.httpStatusForCode itself, so ParamError
// is no longer unsafe to call, just not the right fit for every shape below.)
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
// shapes; and a stalled body read past cmd/yolorouter/serve.go's
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
		initialized, err := auth.CheckState(db)
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

		user, sessionID, err := auth.Setup(db, req.Username, req.Password, time.Now().UTC())
		if errors.Is(err, errcode.ErrAccountSetupAlreadyDone) {
			middleware.WriteAdminError(c, http.StatusConflict, errcode.AccountSetupAlreadyDone)
			return
		}
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}

		writeSessionCookie(c, sessionID, int(auth.SessionTTL.Seconds()))
		writeMeResponse(c, user)
	}
}

// PostLogin verifies username+password and, on success, issues a session.
// limiter caps the number of in-flight bcrypt comparisons (see
// middleware.Semaphore's doc comment) — acquired only around the
// auth.Login call, after bindJSON has already fully read the request
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

		user, sessionID, err := auth.Login(db, req.Username, req.Password, time.Now().UTC())
		var lockedErr *auth.LockedError
		switch {
		case errors.As(err, &lockedErr):
			// AccountLoginLocked is the one error response that carries an
			// extra "locked_until" field.
			middleware.WriteAdminErrorWithData(c, http.StatusForbidden, errcode.AccountLoginLocked,
				gin.H{"locked_until": lockedErr.LockedUntil.Unix()})
			return
		case errors.Is(err, errcode.ErrAccountInvalidCredentials):
			middleware.WriteAdminError(c, http.StatusUnauthorized, errcode.AccountInvalidCredentials)
			return
		case errors.Is(err, errcode.ErrAccountDisabled):
			middleware.WriteAdminError(c, http.StatusForbidden, errcode.AccountDisabled)
			return
		case err != nil:
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}

		writeSessionCookie(c, sessionID, int(auth.SessionTTL.Seconds()))
		writeMeResponse(c, user)
	}
}

// PostLogout deletes the caller's session and clears the cookie.
func PostLogout(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, _ := c.Cookie(middleware.SessionCookieName)
		if token != "" {
			if err := auth.Logout(db, token); err != nil {
				middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
				return
			}
		}
		writeSessionCookie(c, "", -1)
		response.Success(c, nil)
	}
}

// GetMe returns the currently logged-in user's username and role. The
// browser sends its own IANA timezone via the ?timezone= param on
// dashboard/analytics, so the server no longer needs to expose its local
// offset here.
func GetMe(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.MustGet(middleware.UserIDKey).(uint)
		user, err := auth.Me(db, userID)
		if err != nil {
			middleware.WriteAdminError(c, http.StatusInternalServerError, errcode.DatabaseError)
			return
		}
		writeMeResponse(c, user)
	}
}

// writeMeResponse emits the shared me-shape used by PostSetup, PostLogin, and
// GetMe. Centralizing it keeps the three "you are now logged in" responses
// identical. Role drives which navigation the frontend renders; is_local
// drives whether the change-password entry appears at all — externally-
// provisioned accounts have no password to change.
func writeMeResponse(c *gin.Context, user *model.User) {
	response.Success(c, gin.H{
		"username": user.Username,
		"role":     user.Role,
		"is_local": user.IsLocal,
	})
}

// PutPassword changes the caller's own password and forces re-login.
func PutPassword(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req changePasswordRequest
		if !bindJSON(c, &req) {
			return
		}

		userID := c.MustGet(middleware.UserIDKey).(uint)
		err := auth.ChangePassword(db, userID, req.CurrentPassword, req.NewPassword, time.Now().UTC())
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
