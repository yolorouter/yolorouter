package audit

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestDictionaryShape guards the dictionary's invariants: every key names
// a write method on a console route pattern, and every action is a
// two-part namespace.verb name. The sweep test in internal/router holds
// the dictionary against the live route surface; this test holds the
// dictionary's own shape so a rushed entry cannot degrade it.
func TestDictionaryShape(t *testing.T) {
	if len(actionByRoute) == 0 {
		t.Fatal("action dictionary is empty")
	}
	for key, action := range actionByRoute {
		method, pattern, ok := splitActionKey(key)
		if !ok {
			t.Errorf("dictionary key %q is not '<METHOD> <pattern>'", key)
			continue
		}
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			t.Errorf("dictionary key %q: %s is not a write method — the automatic layer never captures it", key, method)
		}
		if len(pattern) <= len(adminRoutePrefix) || pattern[:len(adminRoutePrefix)] != adminRoutePrefix {
			t.Errorf("dictionary key %q: pattern must live under %s", key, adminRoutePrefix)
		}
		if dotCount(action) != 1 {
			t.Errorf("action %q (key %q) must be exactly namespace.verb", action, key)
		}
	}
}

// TestDeriveFallbackAction pins the fallback naming: a route the
// dictionary does not list still gets an action, derived from the route's
// own shape with hyphens normalized to the dictionary's snake_case
// family. The degraded path (nothing derivable) yields an honest
// admin.write rather than an empty action.
func TestDeriveFallbackAction(t *testing.T) {
	cases := []struct {
		method, pattern string
		want            string
	}{
		{http.MethodPost, "/api/admin/widgets", "widgets.create"},
		{http.MethodPut, "/api/admin/widgets", "widgets.update"},
		{http.MethodPatch, "/api/admin/widgets/:id", "widgets.update"},
		{http.MethodDelete, "/api/admin/widgets/:id", "widgets.delete"},
		// Hyphenated first segments normalize, matching the dictionary's
		// own naming ("api-keys" routes are named api_key.*).
		{http.MethodPost, "/api/admin/export-targets", "export_targets.create"},
		// Nested routes still key off the first segment: the fallback is
		// deliberately coarse — naming precision is the dictionary's job.
		{http.MethodPost, "/api/admin/widgets/:id/parts", "widgets.create"},
		// The upstream-key reveal's fallback trap, pinned: without its
		// dictionary entry a denied or throttled reveal attempt would be
		// recorded as "providers.create" (first segment under /api/admin is
		// "providers", POST maps to create) — one character away from the
		// genuine provider.create that names provider-row creation. The
		// value is asserted so a derivation change that re-points this
		// trap is a visible test failure, not a silent audit rename.
		{http.MethodPost, "/api/admin/providers/:id/keys/:keyId/reveal", "providers.create"},
		// A read that reaches the derivation (an explicit event on a GET
		// with no action of its own) degrades honestly to ".read", never
		// to a write verb.
		{http.MethodGet, "/api/admin/widgets", "widgets.read"},
		// Degraded path: nothing derivable still yields a non-empty action.
		{http.MethodPost, "/api/admin", "admin.write"},
	}
	for _, tc := range cases {
		if got := DeriveFallbackAction(tc.method, tc.pattern); got != tc.want {
			t.Errorf("DeriveFallbackAction(%s, %s) = %q, want %q", tc.method, tc.pattern, got, tc.want)
		}
	}
}

// TestResolveActionResourceIDs pins which path parameter (or literal
// segment) becomes the audited resource id: the rightmost parameter is
// the most specific subject, parameterless settings routes name their
// setting by the final segment, and routes with no subject carry an empty
// id.
func TestResolveActionResourceIDs(t *testing.T) {
	cases := []struct {
		name, method, pattern string
		params                gin.Params
		wantAction, wantRID   string
		wantDictionary        bool
	}{
		{
			name:   "nested provider key audit names the key, not the provider",
			method: http.MethodPatch, pattern: "/api/admin/providers/:id/keys/:keyId",
			params:     gin.Params{{Key: "id", Value: "3"}, {Key: "keyId", Value: "9"}},
			wantAction: "provider_key.update", wantRID: "9", wantDictionary: true,
		},
		{
			name:   "the reveal route resolves through the dictionary, not the providers.create fallback",
			method: http.MethodPost, pattern: "/api/admin/providers/:id/keys/:keyId/reveal",
			params:     gin.Params{{Key: "id", Value: "3"}, {Key: "keyId", Value: "9"}},
			wantAction: ActionProviderKeyReveal, wantRID: "9", wantDictionary: true,
		},
		{
			name:   "candidate routes name the candidate",
			method: http.MethodDelete, pattern: "/api/admin/models/:id/candidates/:candidateId",
			params:     gin.Params{{Key: "id", Value: "2"}, {Key: "candidateId", Value: "5"}},
			wantAction: "model_candidate.delete", wantRID: "5", wantDictionary: true,
		},
		{
			name:   "parameterless settings PUT names its setting by the final segment",
			method: http.MethodPut, pattern: "/api/admin/system-settings/request-log-retention",
			params:     nil,
			wantAction: "system_settings.update", wantRID: "request-log-retention", wantDictionary: true,
		},
		{
			name:   "routes with no path subject carry an empty id",
			method: http.MethodPost, pattern: "/api/admin/auth/login",
			params:     nil,
			wantAction: "auth.login", wantRID: "", wantDictionary: true,
		},
		{
			name:   "fallback derivation carries the first-segment parameter when present",
			method: http.MethodDelete, pattern: "/api/admin/widgets/:id",
			params:     gin.Params{{Key: "id", Value: "42"}},
			wantAction: "widgets.delete", wantRID: "42", wantDictionary: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(tc.method, tc.pattern, nil)
			c.Params = tc.params
			res := ResolveAction(c, tc.method, tc.pattern)
			if res.Action != tc.wantAction {
				t.Errorf("action = %q, want %q", res.Action, tc.wantAction)
			}
			if res.ResourceID != tc.wantRID {
				t.Errorf("resource id = %q, want %q", res.ResourceID, tc.wantRID)
			}
			if res.FromDictionary != tc.wantDictionary {
				t.Errorf("FromDictionary = %v, want %v", res.FromDictionary, tc.wantDictionary)
			}
			// The resource type is the action's namespace for dictionary
			// and fallback names alike.
			if want := actionNamespace(res.Action); res.ResourceType != want {
				t.Errorf("resource type = %q, want %q", res.ResourceType, want)
			}
		})
	}
}

// splitActionKey splits "<METHOD> <pattern>" at its single space.
func splitActionKey(key string) (method, pattern string, ok bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == ' ' {
			return key[:i], key[i+1:], true
		}
	}
	return "", "", false
}

func dotCount(s string) int {
	n := 0
	for _, r := range s {
		if r == '.' {
			n++
		}
	}
	return n
}
