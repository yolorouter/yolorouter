package router

import (
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestMemberScopeRouteConformance is the safety net against the silent
// failure mode of ownership scoping: a NEW admin data route whose author
// forgets to consume the forced member scope leaks other accounts' rows
// with no compile error and no failing test — until this one.
//
// Every GET route under /api/admin must be classified below. The sweep at
// the end enumerates the live router; an unclassified route fails the test
// with instructions, forcing the author of any new endpoint to decide —
// visibly, in review — which contract it obeys:
//
//   - adminOnly: a member session gets 403; the data never reaches them.
//   - memberScoped: a member session reaches their own view (usually 200;
//     per-route okStatuses allow domain errors), and the payload must not
//     contain another account's STRING sentinels. Aggregate routes whose
//     bodies are pure numbers (overview, dashboard) are sentinel-blind
//     here — their numeric scoping is asserted by the dedicated tests in
//     member_scope_test.go; this test still pins their reachability.
//   - sessionExempt: session/bootstrap surface with no ownership-bearing
//     data (who am I, which login providers exist).
//
// Scope: GET routes only — every data-returning admin route today is a
// GET; a future member-reachable POST that returns rows would need its
// own entry-point here.
func TestMemberScopeRouteConformance(t *testing.T) {
	f := newMemberScopeFixture(t)

	// Routes a member must be turned away from entirely.
	adminOnly := []string{
		"/api/admin/models",
		"/api/admin/models/:id",
		"/api/admin/models/:id/impact",
		"/api/admin/models/candidates/suggest-price",
		"/api/admin/oauth-providers",
		"/api/admin/providers",
		"/api/admin/providers/:id",
		"/api/admin/providers/:id/candidates",
		"/api/admin/providers/:id/impact",
		"/api/admin/providers/:id/models",
		"/api/admin/request-logs",
		"/api/admin/request-logs/:requestId",
		"/api/admin/request-logs/:requestId/body/stream",
		"/api/admin/request-logs/export",
		"/api/admin/system-settings/custom-system-prompt",
		"/api/admin/system-settings/input-compression",
		"/api/admin/system-settings/vision-fallback",
		"/api/admin/system/version",
		"/api/admin/analytics/compress-stats",
		"/api/admin/analytics/concise-output-projection",
		"/api/admin/users",
	}

	// Routes a member may use, pinned to their own rows. The request path
	// substitutes concrete ids from the fixture; the response must never
	// carry bob's sentinels.
	type scopedProbe struct {
		concrete string
		// okStatuses lists the statuses that count as "the member reached
		// their own view". Most routes answer 200; the plaintext reveal
		// answers a domain 400 here because the fixture's keys predate
		// encrypted storage — the route is still member-reachable and its
		// owner check is covered by TestMemberKeyAccessIsOwnerScoped.
		okStatuses []int
		// badRequestCode pins WHICH domain error legitimizes a 400: the
		// body must carry this business code, so an unrelated rejection
		// (e.g. an owner check misfiring on the member's own key) can't
		// masquerade as the accepted one.
		badRequestCode int
	}
	memberScoped := map[string]scopedProbe{
		"/api/admin/analytics/export":       {concrete: "/api/admin/analytics/export?dimension=caller", okStatuses: []int{http.StatusOK}},
		"/api/admin/analytics/overview":     {concrete: "/api/admin/analytics/overview", okStatuses: []int{http.StatusOK}},
		"/api/admin/analytics/report":       {concrete: "/api/admin/analytics/report?dimension=caller", okStatuses: []int{http.StatusOK}},
		"/api/admin/api-keys":               {concrete: "/api/admin/api-keys?q=&status=&page=1&page_size=50", okStatuses: []int{http.StatusOK}},
		"/api/admin/api-keys/:id":           {concrete: "/api/admin/api-keys/" + uintToString(f.aliceKeyID), okStatuses: []int{http.StatusOK}},
		"/api/admin/api-keys/:id/plaintext": {concrete: "/api/admin/api-keys/" + uintToString(f.aliceKeyID) + "/plaintext", okStatuses: []int{http.StatusOK, http.StatusBadRequest}, badRequestCode: 11016},
		"/api/admin/dashboard":              {concrete: "/api/admin/dashboard", okStatuses: []int{http.StatusOK}},
	}

	// Session/bootstrap surface: reachable pre-role or member-facing by
	// design, carrying no ownership-bearing data rows. Each entry declares
	// the weakest client that must reach it — false means the login
	// bootstrap must answer with NO session at all (a RequireSession
	// slipping in front would lock everyone out of the login page), true
	// means a member session suffices (a RequireAdmin slipping in would
	// break member login) — and the probe below enforces exactly that.
	sessionExempt := map[string]struct{ needsSession bool }{
		"/api/admin/auth/me":              {needsSession: true},
		"/api/admin/auth/oauth/providers": {needsSession: false},
		"/api/admin/auth/state":           {needsSession: false},
		// This deployment's own public address — member-readable by design
		// (rationale on GetSystemEndpoint); carries no account data.
		"/api/admin/system/endpoint": {needsSession: true},
	}

	// bob's sentinel: his username or request id anywhere in a
	// member-scoped payload means the route leaked another account's data.
	// A whole-word regexp instead of quoted substrings so it matches every
	// serialization the routes emit — JSON string values AND bare CSV cells
	// (the export route) — while still not firing on words that merely
	// contain the letters. It also covers "req-bob" (hyphens are word
	// boundaries).
	sentinel := regexp.MustCompile(`\bbob\b`)

	for _, path := range adminOnly {
		concrete := strings.NewReplacer(":id", "1", ":requestId", "req-bob").Replace(path)
		w := f.do(t, http.MethodGet, concrete, "", f.aliceCk)
		if w.Code != http.StatusForbidden {
			t.Errorf("admin-only %s: member session must get 403, got %d", concrete, w.Code)
		}
	}

	for route, probe := range memberScoped {
		w := f.do(t, http.MethodGet, probe.concrete, "", f.aliceCk)
		if !slices.Contains(probe.okStatuses, w.Code) {
			t.Errorf("member-scoped %s: expected one of %v for the member's own view, got %d %s", route, probe.okStatuses, w.Code, w.Body.String())
			continue
		}
		body := w.Body.String()
		if w.Code == http.StatusBadRequest && !strings.Contains(body, `"code":`+strconv.Itoa(probe.badRequestCode)) {
			t.Errorf("member-scoped %s: 400 must carry business code %d, got %s", route, probe.badRequestCode, body)
			continue
		}
		if m := sentinel.FindString(body); m != "" {
			t.Errorf("member-scoped %s leaked another account's data (found %q):\n%s", route, m, body)
		}
	}

	// The sweep: every GET route under /api/admin must appear in exactly
	// one classification above. A new unclassified route fails here, and so
	// does a route claimed by two lists — the contracts are mutually
	// exclusive, so a double claim means one of them is wrong.
	classified := make(map[string]string)
	claim := func(list, path string) {
		if prev, dup := classified[path]; dup {
			t.Errorf("route %s classified twice (%s and %s): a route obeys exactly one contract — remove one entry", path, prev, list)
			return
		}
		classified[path] = list
	}
	for _, p := range adminOnly {
		claim("adminOnly", p)
	}
	for p := range memberScoped {
		claim("memberScoped", p)
	}
	for route, probe := range sessionExempt {
		claim("sessionExempt", route)
		ck := f.aliceCk
		if !probe.needsSession {
			ck = nil
		}
		if w := f.do(t, http.MethodGet, route, "", ck); w.Code != http.StatusOK {
			t.Errorf("session-exempt %s: expected 200 for its weakest client (session=%v), got %d %s", route, probe.needsSession, w.Code, w.Body.String())
		}
	}
	for _, ri := range f.r.Routes() {
		if ri.Method != http.MethodGet || !strings.HasPrefix(ri.Path, "/api/admin") {
			continue
		}
		if _, ok := classified[ri.Path]; !ok {
			t.Errorf("unclassified admin GET route %s: add it to adminOnly, memberScoped (with a leak assertion) or sessionExempt in this test — this is the review gate that keeps a new data route from silently leaking other accounts' rows", ri.Path)
		}
	}
}
