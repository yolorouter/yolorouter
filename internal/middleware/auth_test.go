package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/internal/testutil"
)

func TestRequireAdminSessionRejectsMissingCookie(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	r := gin.New()
	r.Use(RequestID())
	r.GET("/protected", RequireAdminSession(db), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no cookie, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestRequireAdminSessionRejectsUnknownSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	r := gin.New()
	r.Use(RequestID())
	r.GET("/protected", RequireAdminSession(db), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "no-such-token"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown session, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestRequireAdminSessionAcceptsValidSessionAndSetsAdminID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin := &model.Admin{Username: "alice", PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}
	if err := repository.CreateSession(db, "valid-tok", admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	r := gin.New()
	r.Use(RequestID())
	var gotAdminID uint
	r.GET("/protected", RequireAdminSession(db), func(c *gin.Context) {
		gotAdminID = c.MustGet(AdminIDKey).(uint)
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "valid-tok"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid session, got %d, body: %s", w.Code, w.Body.String())
	}
	if gotAdminID != admin.ID {
		t.Fatalf("expected admin id %d in context, got %d", admin.ID, gotAdminID)
	}
}

func TestRequireAdminSessionRejectsExpiredSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	admin := &model.Admin{Username: "bob", PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAdmin(db, admin); err != nil {
		t.Fatalf("CreateAdmin failed: %v", err)
	}
	if err := repository.CreateSession(db, "expired-tok", admin.ID, now.Add(-time.Minute), now.Add(-time.Hour)); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	r := gin.New()
	r.Use(RequestID())
	r.GET("/protected", RequireAdminSession(db), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "expired-tok"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for expired session, got %d, body: %s", w.Code, w.Body.String())
	}
}
