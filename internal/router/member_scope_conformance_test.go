package router

import (
	"fmt"
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
		// Tool-session aggregate view over request_logs: cross-account
		// callers' traffic by construction, same classification as the
		// request-log audit it derives from.
		"/api/admin/agent-sessions",
		"/api/admin/agent-sessions/:sessionId",
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
		// Learned per-key rate-limit evidence describes upstream
		// accounts, which members never see — same dimension as the
		// provider routes above.
		"/api/admin/rate-limits",
		"/api/admin/request-logs",
		"/api/admin/request-logs/:requestId",
		"/api/admin/request-logs/:requestId/body/stream",
		"/api/admin/request-logs/export",
		"/api/admin/system-settings/custom-system-prompt",
		"/api/admin/system-settings/input-compression",
		"/api/admin/system-settings/vision-fallback",
		"/api/admin/system-settings/key-auto-recovery",
		"/api/admin/system-settings/request-log-retention",
		"/api/admin/system/version",
		"/api/admin/analytics/compress-stats",
		"/api/admin/analytics/cache-stats",
		"/api/admin/analytics/concise-output-projection",
		// History model names are the admin filter dropdown's supplement to
		// the (admin-only) models catalog; members derive their own options
		// from their report rows and never see this list.
		"/api/admin/analytics/history-model-names",
		// The video task list is an operator-wide view across every
		// caller's tasks; there is no member-scoped reading of it.
		"/api/admin/video-tasks",
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
		// The Playground pickers: the key list pins to the session's own
		// account inside the service (dedicated tests below hold the exact
		// row set), the model list is the shared routing vocabulary. Both
		// carry no cross-account rows, so the sentinel scan holds vacuously
		// here — the pinning itself is what the dedicated tests pin.
		"/api/admin/playground/keys":   {concrete: "/api/admin/playground/keys", okStatuses: []int{http.StatusOK}},
		"/api/admin/playground/models": {concrete: "/api/admin/playground/models", okStatuses: []int{http.StatusOK}},
		// A finished speech column's retained audio clip. The fixture runs
		// no audio comparison, so the member's own honest answer for any id
		// is 404 — reachable (a session gate in front would answer 401, an
		// admin gate 403) and carrying nothing. The ownership pin (another
		// account's retained clip answers the same nothing) is held by the
		// dedicated audio tests in playground_audio_test.go.
		"/api/admin/playground/audio/:request_id": {concrete: "/api/admin/playground/audio/req-alice-none", okStatuses: []int{http.StatusNotFound}},
		// A finished video column's proxied clip — the same shape of
		// member-reachable miss as the audio route above (the fixture runs
		// no video comparison); the ownership pin is held by the dedicated
		// video tests in playground_video_test.go.
		"/api/admin/playground/video/:request_id": {concrete: "/api/admin/playground/video/req-alice-none", okStatuses: []int{http.StatusNotFound}},
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
		concrete := strings.NewReplacer(":id", "1", ":requestId", "req-bob", ":sessionId", "sess-bob").Replace(path)
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

	// The POST extension of the same review gate. GET dominates the admin
	// surface, but a member-reachable POST spends the member's resources
	// under their identity, which is exactly what must not escape
	// classification. Today's member-reachable POST entries:
	//
	//   /api/admin/playground/run — the console Playground's aggregate
	//   channel. A member reaches their OWN column (their key, their
	//   spend, the run's outcome on the stream); another account's key id
	//   is refused without confirming anything about it.
	//
	//   /api/admin/playground/video/stop-wait — the video column's
	//   stop-waiting switch. It touches no one else's resources: the
	//   service pins the in-flight wait to the account that ran it.
	memberScopedPOST := map[string]func(){
		"/api/admin/playground/run": func() {
			own := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["no-such-model"],"request":{"messages":[{"role":"user","content":"hi"}]}}`, f.aliceKeyID)
			w := f.do(t, http.MethodPost, "/api/admin/playground/run", own, f.aliceCk)
			// Reaching one's own column means the channel opened for the
			// member's own key: 200 + the event stream carrying the run's
			// outcome (an error EVENT for the unknown model is a reached
			// column — the relay answered — not a routing rejection).
			if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Errorf("member-scoped POST /api/admin/playground/run: member must reach their own column (200 event-stream), got %d %s", w.Code, w.Body.String())
				return
			}
			if !strings.Contains(w.Body.String(), "event: error") {
				t.Errorf("member-scoped POST /api/admin/playground/run: expected the column's outcome on the stream, got %s", w.Body.String())
			}
			// Another account's key is refused at the console gate, and
			// the refusal must not echo whose key it was.
			foreign := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["no-such-model"],"request":{"messages":[{"role":"user","content":"hi"}]}}`, f.bobKeyID)
			w = f.do(t, http.MethodPost, "/api/admin/playground/run", foreign, f.aliceCk)
			if w.Code != http.StatusForbidden {
				t.Errorf("member-scoped POST /api/admin/playground/run: foreign key must get 403, got %d %s", w.Code, w.Body.String())
				return
			}
			if m := sentinel.FindString(w.Body.String()); m != "" {
				t.Errorf("member-scoped POST /api/admin/playground/run leaked another account's data (found %q):\n%s", m, w.Body.String())
			}
		},
		"/api/admin/playground/video/stop-wait": func() {
			// The fixture runs no video comparison, so no wait exists: the
			// member's own honest answer is 404 — reachable and confirming
			// nothing. The owner pin (another account's wait answers the
			// same 404) is held by the dedicated video tests.
			w := f.do(t, http.MethodPost, "/api/admin/playground/video/stop-wait", `{"request_id":"req-alice-none"}`, f.aliceCk)
			if w.Code != http.StatusNotFound {
				t.Errorf("member-scoped POST /api/admin/playground/video/stop-wait: expected 404 for an absent wait, got %d %s", w.Code, w.Body.String())
			}
		},
	}
	for route, probe := range memberScopedPOST {
		// The entry must describe a really-registered route — a stale
		// entry would pass forever while guarding nothing.
		registered := false
		for _, ri := range f.r.Routes() {
			if ri.Method == http.MethodPost && ri.Path == route {
				registered = true
				break
			}
		}
		if !registered {
			t.Errorf("member-scoped POST %s: route is not registered — the classification entry is stale", route)
			continue
		}
		probe()
	}
}
