// Package audit is the admin console's audit-trail core: the automatic
// capture middleware mounted at the /api/admin group root, the action
// naming dictionary every console write endpoint is enumerated in, the
// fallback derivation that keeps unlisted (including future) endpoints
// covered, and the asynchronous worker that persists records without ever
// delaying a request.
//
// The layering is deliberate: this package sits above the repository and
// below the router. It must not import the router (the router wires it,
// not the other way around), and nothing here interprets business
// semantics of the endpoints it audits — naming and capture only. The
// explicit-event layer and its merge hook build on the context seat
// reserved here (see detailContextKey).
package audit

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// adminRoutePrefix is the group every audited route lives under. Kept as
// one constant so the dictionary below and the middleware agree on what
// "a console write route" is.
const adminRoutePrefix = "/api/admin"

// actionByRoute names every console write endpoint that exists today.
// Keyed by "<METHOD> <gin pattern>" — the pattern (with its :params), not
// a concrete path, so one entry covers every id. The dictionary exists so
// actions read as domain language (provider_key.status) instead of route
// mechanics; it is a convenience, never a gate: an endpoint missing here
// still gets audited via the fallback derivation below, which is how a
// brand-new route cannot ship an audit hole just because nobody updated
// this map.
var actionByRoute = map[string]string{
	// Public auth group (no session yet): first-run setup, local login,
	// and the OAuth state issuance the login page drives.
	"POST /api/admin/auth/setup":       "auth.setup",
	"POST /api/admin/auth/login":       ActionLogin,
	"POST /api/admin/auth/oauth/state": "auth.oauth_state",

	// Session-only group: any signed-in account may end its own session
	// or change its own password.
	"POST /api/admin/auth/logout":  "auth.logout",
	"PUT /api/admin/auth/password": "auth.password_change",

	// Accounts (admin-managed).
	"POST /api/admin/users":              "user.create",
	"POST /api/admin/users/:id/password": "user.password_reset",
	"PATCH /api/admin/users/:id/profile": "user.update",
	"PATCH /api/admin/users/:id/status":  "user.status",
	"PATCH /api/admin/users/:id/role":    "user.role",

	// Caller API keys (ownership-scoped group).
	"POST /api/admin/api-keys":             "api_key.create",
	"PATCH /api/admin/api-keys/:id":        "api_key.update",
	"PATCH /api/admin/api-keys/:id/revoke": "api_key.revoke",

	// Providers and their upstream keys.
	"POST /api/admin/providers":                         "provider.create",
	"POST /api/admin/providers/test-key":                "provider.test_key",
	"POST /api/admin/providers/list-models":             "provider.list_models",
	"PATCH /api/admin/providers/:id":                    "provider.update",
	"PATCH /api/admin/providers/:id/status":             "provider.status",
	"DELETE /api/admin/providers/:id":                   "provider.delete",
	"POST /api/admin/providers/:id/keys":                "provider_key.create",
	"PATCH /api/admin/providers/:id/keys/:keyId":        "provider_key.update",
	"PATCH /api/admin/providers/:id/keys/:keyId/order":  "provider_key.order",
	"PATCH /api/admin/providers/:id/keys/:keyId/status": "provider_key.status",
	"DELETE /api/admin/providers/:id/keys/:keyId":       "provider_key.delete",
	"POST /api/admin/providers/:id/keys/:keyId/test":    "provider_key.test",
	"POST /api/admin/providers/:id/keys/test-all":       "provider_key.test_all",

	// Learned upstream rate-limit evidence.
	"DELETE /api/admin/rate-limits/:id": "rate_limit.delete",

	// Models and import candidates.
	"POST /api/admin/models":                                     "model.create",
	"POST /api/admin/models/batch":                               "model.batch_create",
	"PATCH /api/admin/models/:id":                                "model.update",
	"PATCH /api/admin/models/:id/status":                         "model.status",
	"DELETE /api/admin/models/:id":                               "model.delete",
	"POST /api/admin/models/:id/candidates":                      "model_candidate.create",
	"POST /api/admin/models/:id/candidates/test-and-create":      "model_candidate.test_and_create",
	"PATCH /api/admin/models/:id/candidates/:candidateId":        "model_candidate.update",
	"PATCH /api/admin/models/:id/candidates/:candidateId/order":  "model_candidate.order",
	"PATCH /api/admin/models/:id/candidates/:candidateId/status": "model_candidate.status",
	"POST /api/admin/models/:id/candidates/:candidateId/test":    "model_candidate.test",
	"DELETE /api/admin/models/:id/candidates/:candidateId":       "model_candidate.delete",

	// Provider-scoped model-domain operations (bulk import and pricing
	// suggestions; subjects are the provider's upstream catalog).
	"POST /api/admin/providers/:id/models/import":         "provider.import_models",
	"POST /api/admin/providers/:id/models/suggest-prices": "provider.suggest_prices",

	// OAuth identity providers (client_secret is the redaction-sensitive
	// field on create/update).
	"POST /api/admin/oauth-providers":          "oauth_provider.create",
	"PATCH /api/admin/oauth-providers/:id":     "oauth_provider.update",
	"DELETE /api/admin/oauth-providers/:id":    "oauth_provider.delete",
	"POST /api/admin/oauth-providers/discover": "oauth_provider.discover",

	// Typed system settings: one PUT per named setting.
	"PUT /api/admin/system-settings/custom-system-prompt":  "system_settings.update",
	"PUT /api/admin/system-settings/input-compression":     "system_settings.update",
	"PUT /api/admin/system-settings/vision-fallback":       "system_settings.update",
	"PUT /api/admin/system-settings/key-auto-recovery":     "system_settings.update",
	"PUT /api/admin/system-settings/request-log-retention": "system_settings.update",
	"PUT /api/admin/system-settings/admin-audit-retention": "system_settings.update",

	// One-click self-update.
	"POST /api/admin/system/update": "system.update",
}

// Outcome-level auth actions. Unlike every name in the dictionary these
// describe how a login attempt ENDED, not which route served it, so they
// can never be route keys: the login endpoint is one route whose failures
// the security audit needs to tell apart. The authentication flow's
// explicit-event layer swaps the route-derived "auth.login" for one of
// these at request exit (see ExplicitEvent), with the status pinned to
// denied — a refused attempt is a denial whatever envelope shape carries
// the refusal.
const (
	// ActionLogin names the login route's successful outcome — the one
	// auth outcome the route shape alone expresses, which is why it is
	// also the dictionary's name for the route.
	ActionLogin = "auth.login"
	// ActionLoginDenied marks credentials the account rejected: unknown
	// username, wrong password, or a non-local username — deliberately
	// indistinguishable, exactly like the response itself.
	ActionLoginDenied = "auth.login_denied"
	// ActionLoginLocked marks an attempt against an account the lockout
	// state machine had already frozen.
	ActionLoginLocked = "auth.login_locked"
	// ActionAPIKeyReveal names the audited sensitive read: the scoped GET
	// that returns a caller API key's full plaintext. Reads never reach
	// the automatic layer's method filter; the reveal endpoint parks this
	// event so the one enumerated sensitive read leaves its trace — an
	// admin's unscoped reach into any key is exactly the access the audit
	// trail exists to remember.
	ActionAPIKeyReveal = "api_key.reveal"
)

// actionKey builds the dictionary key for a matched route.
func actionKey(method, pattern string) string {
	return method + " " + pattern
}

// KnownAction reports whether the dictionary explicitly names this
// method+pattern. Exported for the route-sweep test: every write route on
// today's surface is enumerated, and the sweep holds the map to that
// standard (a route relying on the fallback is a dictionary gap, not an
// audit hole — the test makes the gap visible instead of silent).
func KnownAction(method, pattern string) bool {
	_, ok := actionByRoute[actionKey(method, pattern)]
	return ok
}

// fallbackVerb maps an HTTP method to the generic verb used when a route
// is not in the dictionary. PUT and PATCH both read as "update" — the
// distinction is a wire habit, not a business fact, and the audit trail
// should not invent a difference the endpoints themselves do not carry.
// GET maps to "read" so a read that raised an explicit event without an
// action of its own degrades to an honest "<resource>.read" instead of a
// nonsensical write name; reads only ever reach a row through that
// explicit-event door, never from the route shape alone. The empty
// return stays as the derivation's total-guard.
func fallbackVerb(method string) string {
	switch method {
	case http.MethodPost:
		return "create"
	case http.MethodPut, http.MethodPatch:
		return "update"
	case http.MethodDelete:
		return "delete"
	case http.MethodGet:
		return "read"
	default:
		return ""
	}
}

// fallbackResource derives the resource half of a fallback action from
// the route pattern: the first path segment under /api/admin, hyphens
// normalized to underscores so the derived name lands in the same
// snake_case family as the dictionary ("api-keys" -> "api_keys"). Empty
// when the pattern has no segment there, which cannot happen for a
// matched admin route but keeps the function total.
func fallbackResource(pattern string) string {
	rest := strings.TrimPrefix(pattern, adminRoutePrefix)
	segments := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segments) == 0 || segments[0] == "" {
		return ""
	}
	return strings.ReplaceAll(segments[0], "-", "_")
}

// DeriveFallbackAction composes the fallback action name
// ("<resource>.<verb>") for a route the dictionary does not name.
// Future endpoints get audited with this shape on day one, before anyone
// gives them a dictionary entry; when both halves cannot be derived the
// result is "admin.write" — an honest "unnamed console write" rather than
// an empty action that filters and CSV exports would trip over.
func DeriveFallbackAction(method, pattern string) string {
	resource := fallbackResource(pattern)
	verb := fallbackVerb(method)
	if resource == "" || verb == "" {
		return "admin.write"
	}
	return resource + "." + verb
}

// ActionResolution is what the middleware needs from a matched route: the
// action name, its resource half (the action's namespace — the part
// before the last dot, for both dictionary and fallback names), and the
// resource id the path carried.
type ActionResolution struct {
	Action       string
	ResourceType string
	ResourceID   string
	// FromDictionary distinguishes a named endpoint from a fallback
	// derivation. The record itself does not carry this flag (the action
	// string is the product); it exists so tests and future tooling can
	// tell a deliberate name from a derived one.
	FromDictionary bool
}

// ResolveAction names a matched console write route. Dictionary first;
// anything else falls through to the derivation above. ResourceID comes
// from the route's own parameters — the rightmost parameter is the most
// specific subject (/providers/:id/keys/:keyId is about the key), and
// parameterless setting routes identify their subject by the final path
// segment ("custom-system-prompt"). Everything else without a parameter
// (auth.login, system.update) has no path-carried subject and leaves the
// id empty.
func ResolveAction(c *gin.Context, method, pattern string) ActionResolution {
	if action, ok := actionByRoute[actionKey(method, pattern)]; ok {
		return ActionResolution{
			Action:         action,
			ResourceType:   actionNamespace(action),
			ResourceID:     resourceIDFromPath(c, pattern),
			FromDictionary: true,
		}
	}
	action := DeriveFallbackAction(method, pattern)
	return ActionResolution{
		Action:         action,
		ResourceType:   actionNamespace(action),
		ResourceID:     resourceIDFromPath(c, pattern),
		FromDictionary: false,
	}
}

// actionNamespace returns everything before the last dot. Guarded because
// an action without a dot would otherwise panic a slice expression; the
// fallback's "admin.write" guarantees a dot even on the degraded path.
func actionNamespace(action string) string {
	if i := strings.LastIndex(action, "."); i > 0 {
		return action[:i]
	}
	return action
}

// resourceIDFromPath extracts the audited subject's id from the matched
// route. The rightmost path parameter wins: nested resources are about
// their leaf (/models/:id/candidates/:candidateId audits the candidate).
// Routes with no parameter that carry a literal final segment under the
// typed-settings prefix use that segment as the subject's name — each
// settings PUT is one named setting and the segment is its stable slug.
func resourceIDFromPath(c *gin.Context, pattern string) string {
	segments := strings.Split(strings.Trim(pattern, "/"), "/")
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if strings.HasPrefix(seg, ":") {
			name := strings.TrimPrefix(seg, ":")
			if v := c.Param(name); v != "" {
				return v
			}
		}
	}
	if strings.HasPrefix(pattern, adminRoutePrefix+"/system-settings/") && len(segments) > 0 {
		return segments[len(segments)-1]
	}
	return ""
}
