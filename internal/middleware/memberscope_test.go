package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
)

// memberScopeProbe runs one request through MemberScope with the given
// context pre-population (simulating what RequireSession would have set)
// and reports the response code plus the ViewScope the handler saw.
func memberScopeProbe(t *testing.T, populate func(*gin.Context)) (int, ViewScope, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var scope ViewScope
	handlerRan := false
	r.GET("/probe", func(c *gin.Context) {
		if populate != nil {
			populate(c)
		}
		c.Next()
	}, MemberScope(), func(c *gin.Context) {
		handlerRan = true
		scope = ViewScopeOf(c)
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/probe", nil))
	return w.Code, scope, handlerRan
}

// TestMemberScopeFailsClosedWithoutSession: wired without RequireSession
// there is no user id to scope to — the request must be rejected, not let
// through unscoped.
func TestMemberScopeFailsClosedWithoutSession(t *testing.T) {
	code, _, handlerRan := memberScopeProbe(t, nil)
	if code != http.StatusForbidden {
		t.Fatalf("MemberScope without a session must answer 403, got %d", code)
	}
	if handlerRan {
		t.Fatalf("MemberScope without a session must abort the chain, but the handler ran")
	}
}

// TestMemberScopePinsMemberAndPassesAdmin: a member session gets the
// forced scope with their own id; an admin session passes unscoped.
func TestMemberScopePinsMemberAndPassesAdmin(t *testing.T) {
	code, scope, _ := memberScopeProbe(t, func(c *gin.Context) {
		c.Set(UserIDKey, uint(42))
		c.Set(UserRoleKey, model.RoleMember)
	})
	if code != http.StatusOK || !scope.Member || scope.Owner == nil || *scope.Owner != 42 {
		t.Fatalf("member session: expected 200 with member scope pinned to 42, got %d / %+v", code, scope)
	}

	code, scope, _ = memberScopeProbe(t, func(c *gin.Context) {
		c.Set(UserIDKey, uint(1))
		c.Set(UserRoleKey, model.RoleAdmin)
	})
	if code != http.StatusOK || scope.Member || scope.Owner != nil {
		t.Fatalf("admin session: expected 200 with the zero (admin) scope, got %d / %+v", code, scope)
	}
}

// TestViewScopeResolvePinsMemberAndPassesAdminFilter pins the one merge
// every ownership-scoped endpoint runs through. A member's own id wins
// over whatever the request's filter parameter says — the pin, not a
// default — while an admin's filter parameter passes through untouched,
// absent or present. Getting this backwards hands one account another
// account's rows, so both directions are pinned from both ends.
func TestViewScopeResolvePinsMemberAndPassesAdminFilter(t *testing.T) {
	own := uint(7)
	query := uint(9)

	member := ViewScope{Member: true, Owner: &own}
	if got := member.Resolve(nil); got == nil || *got != 7 {
		t.Fatalf("member with no query filter: Resolve = %v, want own id 7", got)
	}
	if got := member.Resolve(&query); got == nil || *got != 7 {
		t.Fatalf("member with a smuggled ?user_id: Resolve = %v, want the pin 7 — the query must never win", got)
	}

	admin := ViewScope{}
	if got := admin.Resolve(nil); got != nil {
		t.Fatalf("admin with no filter: Resolve = %v, want nil (no constraint)", got)
	}
	if got := admin.Resolve(&query); got == nil || *got != 9 {
		t.Fatalf("admin with a filter: Resolve = %v, want the filter 9 passed through", got)
	}
}
