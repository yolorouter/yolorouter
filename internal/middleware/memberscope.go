package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// ViewScopeKey is the gin.Context key MemberScope sets for non-admin
// sessions: the ownership view every scoped query MUST respect, regardless
// of what the request's own parameters say. Handlers read it via
// ViewScopeOf.
const ViewScopeKey = "view_scope"

// ViewScope is whose eyes a request sees through. The zero value is an
// admin session: no pin, no hiding — an admin may filter by any account
// and sees operator information. A member session is Member=true with
// Owner set: every ownership-scoped query resolves to Owner no matter
// what the query says, and operator information (upstream identities,
// deployment-wide sections) is hidden. The distinction between "a member
// session" and "an admin filtering by user" cannot be derived from a user
// id alone — the dashboard keeps the deployment sections for one and
// strips them for the other — which is why both facts travel together.
type ViewScope struct {
	Member bool
	Owner  *uint
}

// Resolve returns the account id an ownership-scoped query should narrow
// to: a member's own id, regardless of what the request's own filter
// parameter says (the pin — a smuggled ?user_id never wins); an admin's
// optional filter parameter otherwise. One home for the merge that every
// scoped endpoint used to hand-roll.
func (s ViewScope) Resolve(queryUserID *uint) *uint {
	if s.Member {
		return s.Owner
	}
	return queryUserID
}

// ViewScopeOf returns the view MemberScope pinned for this request, or the
// zero value (an unrestricted admin session) when it did not. An entry of
// the wrong type under the key panics rather than degrading to the zero
// value — this accessor guards an ownership boundary, and a corrupted
// entry must fail loudly instead of silently granting admin reach.
func ViewScopeOf(c *gin.Context) ViewScope {
	v, ok := c.Get(ViewScopeKey)
	if !ok {
		return ViewScope{}
	}
	return v.(ViewScope)
}

// MemberScope marks non-admin sessions with their forced ownership
// scope. It must run after RequireSession (it reads the resolved role).
// Admins pass through unmarked — their requests keep the optional
// filter-by-anyone semantics. This is the enforcement half of the member
// data-visibility rule: hiding pages in the frontend is cosmetic, this
// is what actually pins a member's queries to their own rows.
func MemberScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := c.Get(UserRoleKey)
		if !ok || role != model.RoleAdmin {
			// Fail closed exactly like RequireAdmin: a route wired with
			// MemberScope but no RequireSession has no user id to scope to
			// and must not proceed unscoped.
			userID, hasUser := c.Get(UserIDKey)
			if !hasUser {
				WriteAdminError(c, http.StatusForbidden, errcode.AccountPageForbidden)
				return
			}
			id := userID.(uint)
			c.Set(ViewScopeKey, ViewScope{Member: true, Owner: &id})
		}
		c.Next()
	}
}
