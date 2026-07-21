package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

func newAuthTestRouter(t *testing.T, db *gorm.DB) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	r := gin.New()
	r.Use(middleware.RequestID())

	admin := r.Group("/api/admin")
	admin.GET("/auth/state", GetAuthState(db))
	admin.POST("/auth/setup", PostSetup(db))
	admin.POST("/auth/login", PostLogin(db, middleware.NewSemaphore(8)))
	admin.POST("/auth/logout", middleware.RequireAdminSession(db), PostLogout(db))
	admin.GET("/auth/me", middleware.RequireAdminSession(db), GetMe(db))
	admin.PUT("/auth/password", middleware.RequireAdminSession(db), PutPassword(db))
	return r
}

type envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	Timestamp int64           `json:"timestamp"`
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}, cookie *http.Cookie) (*httptest.ResponseRecorder, envelope) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env envelope
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal response body %q: %v", w.Body.String(), err)
		}
	}
	return w, env
}

func TestGetAuthStateReflectsWhetherSetupIsDone(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/auth/state", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var state struct {
		Initialized bool `json:"initialized"`
	}
	if err := json.Unmarshal(env.Data, &state); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if state.Initialized {
		t.Fatalf("expected initialized=false before setup")
	}
}

func TestSetupLoginLogoutFullFlow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	// Setup
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from setup, got %d, body: %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "session_id" {
		t.Fatalf("expected exactly one session_id cookie, got %+v", cookies)
	}
	sessionCookie := cookies[0]

	// A second setup call must fail now.
	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "someone-else", "password": "password456"}, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 for repeat setup, got %d", w.Code)
	}
	if env.Code != 10007 { // errcode.AccountSetupAlreadyDone
		t.Fatalf("expected code 10007, got %d", env.Code)
	}

	// /auth/me works with the session cookie.
	w, env = doJSON(t, r, http.MethodGet, "/api/admin/auth/me", nil, sessionCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /auth/me, got %d", w.Code)
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(env.Data, &me); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if me.Username != "admin" {
		t.Fatalf("expected username=admin, got %q", me.Username)
	}

	// Logout, then /auth/me must fail.
	w, _ = doJSON(t, r, http.MethodPost, "/api/admin/auth/logout", nil, sessionCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from logout, got %d", w.Code)
	}
	w, _ = doJSON(t, r, http.MethodGet, "/api/admin/auth/me", nil, sessionCookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 from /auth/me after logout, got %d", w.Code)
	}
}

func TestLoginRejectsInvalidUsernameFormatAtBindingLayer(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "ab", "password": "password123"}, nil) // "ab" is 2 chars, min=3
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for too-short username, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestSetupRejectsPasswordWithoutDigit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "onlyletters"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for digit-less password, got %d, body: %s", w.Code, w.Body.String())
	}
	// Guards the round-2 leak (raw Gin/validator text exposing the
	// internal request struct name) staying fixed: a plain status
	// assertion alone can't catch a regression that only changes the
	// message body.
	if strings.Contains(env.Message, "setupRequest") || strings.Contains(env.Message, "Key:") {
		t.Fatalf("expected a cleaned message with no internal struct/field leak, got %q", env.Message)
	}
	if env.Message != "Password: alnum_mixed" {
		t.Fatalf("expected cleaned message %q, got %q", "Password: alnum_mixed", env.Message)
	}
}

// TestSetupRejectsWrongJSONTypeWithoutLeakingStructName guards the other
// bind-failure shape a round-2 fix's regex-style cleaning didn't cover: a
// JSON field of the wrong type reaches ShouldBindJSON as a
// *json.UnmarshalTypeError, not a validator.ValidationErrors, and its
// Error() text embeds the Go struct name directly (round 3's finding).
func TestSetupRejectsWrongJSONTypeWithoutLeakingStructName(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]any{"username": 123, "password": "password123"}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a username sent as a JSON number, got %d, body: %s", w.Code, w.Body.String())
	}
	if strings.Contains(env.Message, "setupRequest") || strings.Contains(env.Message, "Go struct field") {
		t.Fatalf("expected a cleaned message with no internal struct/field leak, got %q", env.Message)
	}
	if env.Message != "username: expected string" {
		t.Fatalf("expected cleaned message %q, got %q", "username: expected string", env.Message)
	}
}

// TestSetupRejectsPasswordLongerThanBcryptLimit guards against
// bcrypt.GenerateFromPassword's ErrPasswordTooLong surfacing as a confusing
// generic 500 DatabaseError — a password over 72 bytes must be rejected at
// the binding layer with a clear 400 instead.
func TestSetupRejectsPasswordLongerThanBcryptLimit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	tooLong := strings.Repeat("a1", 37) // 74 ASCII bytes, > 72
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": tooLong}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a password over bcrypt's 72-byte limit, got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestSetupAcceptsPasswordAtExactlyBcryptLimit guards the boundary itself:
// exactly 72 bytes must still be accepted (only strictly-over is rejected).
func TestSetupAcceptsPasswordAtExactlyBcryptLimit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	exactly72 := strings.Repeat("a1", 36) // 72 ASCII bytes, satisfies min=10/alnum_mixed too
	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": exactly72}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for a password at exactly 72 bytes, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestLoginLockedResponseCarriesLockedUntil(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)

	var w *httptest.ResponseRecorder
	var env envelope
	for i := 0; i < 5; i++ {
		w, env = doJSON(t, r, http.MethodPost, "/api/admin/auth/login",
			map[string]string{"username": "admin", "password": "wrong-password"}, nil)
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 on the 5th failure, got %d, body: %s", w.Code, w.Body.String())
	}
	// WriteAdminErrorWithData's Code/Message are only exercised indirectly
	// through this test — assert them here too, not just the data payload,
	// so a regression that sent the wrong error code or an empty message
	// alongside a correct locked_until can't pass silently.
	if env.Code != errcode.AccountLoginLocked {
		t.Fatalf("expected code %d, got %d", errcode.AccountLoginLocked, env.Code)
	}
	if env.Message != errcode.ErrorMessages[errcode.AccountLoginLocked] {
		t.Fatalf("expected message %q, got %q", errcode.ErrorMessages[errcode.AccountLoginLocked], env.Message)
	}
	var data struct {
		LockedUntil int64 `json:"locked_until"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if data.LockedUntil == 0 {
		t.Fatalf("expected a non-zero locked_until in the response body")
	}
}

// TestSetupRejectsEmptyBodyWithoutLeakingParserText and
// TestSetupRejectsMalformedJSONWithoutLeakingParserText guard bindJSON's
// io.EOF/io.ErrUnexpectedEOF/*json.SyntaxError branch the same way
// TestSetupRejectsWrongJSONTypeWithoutLeakingStructName already guards the
// *json.UnmarshalTypeError branch — without them, a later reordering of
// bindJSON's errors.As checks (or a Go/gin-gonic upgrade that changes how
// ShouldBindJSON wraps these errors) could silently reintroduce the raw
// parser text ("EOF", "invalid character ...") in the response body with
// no test catching it.
func TestSetupRejectsEmptyBodyWithoutLeakingParserText(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup", nil, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty body, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Message != "invalid request body" {
		t.Fatalf("expected cleaned message %q, got %q", "invalid request body", env.Message)
	}
}

func TestSetupRejectsMalformedJSONWithoutLeakingParserText(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/setup", strings.NewReader(`{"username":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response body %q: %v", w.Body.String(), err)
	}
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed JSON, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Message != "invalid request body" {
		t.Fatalf("expected cleaned message %q, got %q", "invalid request body", env.Message)
	}
}

func TestChangePasswordClearsCookieAndInvalidatesSession(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	sessionCookie := w.Result().Cookies()[0]

	w, _ = doJSON(t, r, http.MethodPut, "/api/admin/auth/password", map[string]string{
		"current_password": "password123",
		"new_password":     "newpassword456",
	}, sessionCookie)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from change-password, got %d, body: %s", w.Code, w.Body.String())
	}

	w, _ = doJSON(t, r, http.MethodGet, "/api/admin/auth/me", nil, sessionCookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with the old session after password change, got %d", w.Code)
	}

	// New password logs in fine.
	w, _ = doJSON(t, r, http.MethodPost, "/api/admin/auth/login",
		map[string]string{"username": "admin", "password": "newpassword456"}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 logging in with the new password, got %d", w.Code)
	}
}

func TestAuthRoutesRequireLoginExceptStateSetupLogin(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/admin/auth/logout"},
		{http.MethodGet, "/api/admin/auth/me"},
		{http.MethodPut, "/api/admin/auth/password"},
	} {
		w, _ := doJSON(t, r, tc.method, tc.path, nil, nil)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: expected 401 without a session, got %d", tc.method, tc.path, w.Code)
		}
	}
}

// dropAdminsTable forces every subsequent admins-table query to fail with a
// generic (non-gorm.ErrRecordNotFound) error, without touching the
// admin_sessions table — used to reach a handler's "DB error other than not
// found" branch for calls made through a route whose own
// middleware.RequireAdminSession(db) check must still succeed (it only
// queries admin_sessions). admin_sessions.admin_id is a foreign key into
// admins, so the drop must disable FK enforcement first or SQLite refuses
// it outright ("FOREIGN KEY constraint failed").
func dropAdminsTable(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if err := db.Exec("DROP TABLE admins").Error; err != nil {
		t.Fatalf("drop admins table: %v", err)
	}
}

func TestGetAuthStateReturns500WhenCountAdminsFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)
	dropAdminsTable(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/auth/state", nil, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.DatabaseError {
		t.Fatalf("expected code %d, got %d", errcode.DatabaseError, env.Code)
	}
}

func TestPostSetupReturns500WhenCountAdminsFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)
	dropAdminsTable(t, db)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.DatabaseError {
		t.Fatalf("expected code %d, got %d", errcode.DatabaseError, env.Code)
	}
}

func TestPostLoginRejectsMalformedJSON(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/login", strings.NewReader(`{"username":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", w.Code, w.Body.String())
	}
}

// TestPostLoginReturns429WhenLimiterExhausted builds its own minimal router
// (rather than newAuthTestRouter's shared capacity-8 semaphore) with a
// zero-capacity semaphore, so the very first login attempt already finds no
// slot available.
func TestPostLoginReturns429WhenLimiterExhausted(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	r := gin.New()
	admin := r.Group("/api/admin")
	admin.POST("/auth/setup", PostSetup(db))
	admin.POST("/auth/login", PostLogin(db, middleware.NewSemaphore(0)))

	doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/login",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.ServiceUnavailable {
		t.Fatalf("expected code %d, got %d", errcode.ServiceUnavailable, env.Code)
	}
}

func TestPostLoginReturns500OnGenericDBError(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	dropAdminsTable(t, db)

	w, env := doJSON(t, r, http.MethodPost, "/api/admin/auth/login",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.DatabaseError {
		t.Fatalf("expected code %d, got %d", errcode.DatabaseError, env.Code)
	}
}

func TestGetMeReturns500WhenAdminLookupFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	sessionCookie := w.Result().Cookies()[0]
	dropAdminsTable(t, db)

	w, env := doJSON(t, r, http.MethodGet, "/api/admin/auth/me", nil, sessionCookie)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.DatabaseError {
		t.Fatalf("expected code %d, got %d", errcode.DatabaseError, env.Code)
	}
}

func TestPutPasswordRejectsMalformedJSON(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	sessionCookie := w.Result().Cookies()[0]

	req := httptest.NewRequest(http.MethodPut, "/api/admin/auth/password", strings.NewReader(`{"current_password":`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(sessionCookie)
	wRec := httptest.NewRecorder()
	r.ServeHTTP(wRec, req)
	if wRec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d, body: %s", wRec.Code, wRec.Body.String())
	}
}

func TestPutPasswordReturns401WhenCurrentPasswordWrong(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	sessionCookie := w.Result().Cookies()[0]

	w, env := doJSON(t, r, http.MethodPut, "/api/admin/auth/password", map[string]string{
		"current_password": "wrong-password1",
		"new_password":     "newpassword456",
	}, sessionCookie)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.AccountInvalidCredentials {
		t.Fatalf("expected code %d, got %d", errcode.AccountInvalidCredentials, env.Code)
	}
}

func TestPutPasswordReturns500WhenAdminLookupFails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	r := newAuthTestRouter(t, db)

	w, _ := doJSON(t, r, http.MethodPost, "/api/admin/auth/setup",
		map[string]string{"username": "admin", "password": "password123"}, nil)
	sessionCookie := w.Result().Cookies()[0]
	dropAdminsTable(t, db)

	w, env := doJSON(t, r, http.MethodPut, "/api/admin/auth/password", map[string]string{
		"current_password": "password123",
		"new_password":     "newpassword456",
	}, sessionCookie)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body: %s", w.Code, w.Body.String())
	}
	if env.Code != errcode.DatabaseError {
		t.Fatalf("expected code %d, got %d", errcode.DatabaseError, env.Code)
	}
}
