package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/version"
)

// newTestService wires a VersionService at repo "owner/repo" against a
// httptest server, with shrunken TTLs so cache behaviour is testable in
// milliseconds rather than minutes.
func newTestService(t *testing.T, status int, body any) (*VersionService, *atomic.Int32, func()) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	svc := NewVersionService("owner/repo", "")
	svc.routes = []string{""} // hermetic: no built-in mirror fallback in unit tests
	svc.baseURL = srv.URL
	svc.posTTL = 80 * time.Millisecond
	svc.negTTL = 40 * time.Millisecond
	return svc, &hits, srv.Close
}

func withVersion(t *testing.T, v string) {
	t.Helper()
	orig := version.Version
	t.Cleanup(func() { version.Version = orig })
	version.Version = v
}

// TestCheckRoutesThroughProxy verifies the release lookup is prefixed with the
// configured proxy: the request lands on the proxy host with the real GitHub
// API URL embedded in the path (the prefix shape the mirror worker expects).
func TestCheckRoutesThroughProxy(t *testing.T) {
	withVersion(t, "v0.1.0")
	var gotURI atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI.Store(r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v0.2.0"})
	}))
	defer srv.Close()

	// baseURL stays the real GitHub host; proxy points at the test server, so
	// ProxyURL yields "<test-server>/https://api.github.com/repos/...".
	svc := NewVersionService("owner/repo", srv.URL)
	svc.posTTL = 80 * time.Millisecond
	svc.negTTL = 40 * time.Millisecond

	st := svc.Check(context.Background())
	if st.CheckFailed {
		t.Fatalf("expected success through proxy, got CheckFailed")
	}
	uri, _ := gotURI.Load().(string)
	if !strings.Contains(uri, "https://api.github.com/repos/owner/repo/releases/latest") {
		t.Fatalf("proxied request URI = %q, want it to embed the GitHub API URL", uri)
	}
}

func TestCheckRepoEmptyShortCircuitsWithoutNetwork(t *testing.T) {
	svc := NewVersionService("", "")
	st := svc.Check(context.Background())
	if !st.CheckFailed {
		t.Fatalf("empty repo must report CheckFailed=true, got %+v", st)
	}
	if st.HasUpdate {
		t.Fatalf("empty repo must not report HasUpdate")
	}
}

func TestCheckDetectsNewerRelease(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0", HTMLURL: "https://example/r"})
	defer closeSrv()

	st := svc.Check(context.Background())
	if st.CheckFailed {
		t.Fatalf("expected success, got CheckFailed=true (hits=%d)", hits.Load())
	}
	if st.Latest != "v0.2.0" {
		t.Fatalf("Latest = %q, want v0.2.0", st.Latest)
	}
	if !st.HasUpdate {
		t.Fatalf("HasUpdate should be true for v0.1.0 -> v0.2.0")
	}
	if st.ReleaseURL != "https://example/r" {
		t.Fatalf("ReleaseURL = %q", st.ReleaseURL)
	}
}

func TestCheckNoUpdateWhenAlreadyLatest(t *testing.T) {
	withVersion(t, "v0.2.0")
	svc, _, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0"})
	defer closeSrv()

	st := svc.Check(context.Background())
	if st.HasUpdate {
		t.Fatalf("HasUpdate should be false when current == latest")
	}
}

func TestCheckNoUpdateWhenCurrentIsNewer(t *testing.T) {
	withVersion(t, "v0.3.0")
	svc, _, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0"})
	defer closeSrv()

	st := svc.Check(context.Background())
	if st.HasUpdate {
		t.Fatalf("HasUpdate should be false when current > latest")
	}
}

func TestCheckDevBuildNeverReportsUpdate(t *testing.T) {
	withVersion(t, "dev")
	svc, _, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0"})
	defer closeSrv()

	st := svc.Check(context.Background())
	// "dev" is not comparable: HasUpdate must stay false AND CheckFailed must
	// be true, so the UI shows "check failed" rather than a misleading "up to
	// date" for a build the updater refuses.
	if st.HasUpdate {
		t.Fatalf("HasUpdate must be false for a dev (non-semver) current")
	}
	if !st.CheckFailed {
		t.Fatalf("a dev current must report CheckFailed=true (incomparable), not appear up to date")
	}
}

// TestCheckPrereleaseCurrentNeverReportsUpdate guards the
// downgrade-via-git-describe regression: a current built
// from "v1.2.3-dirty" / "v1.2.3-4-gabc" / "v1.2.3-rc1" is a semver
// prerelease, ranked BELOW its release. Without the Prerelease guard,
// Compare(latest, current) would report the tag as newer and the admin UI
// would advertise an "update" that actually downgrades the newer dirty build
// to the older tag.
func TestCheckPrereleaseCurrentNeverReportsUpdate(t *testing.T) {
	for _, cur := range []string{"v1.2.3-dirty", "v1.2.3-4-gabc", "v1.2.3-rc1"} {
		t.Run(cur, func(t *testing.T) {
			withVersion(t, cur)
			svc, _, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v1.2.3"})
			defer closeSrv()

			st := svc.Check(context.Background())
			if st.HasUpdate {
				t.Fatalf("prerelease current %q must not report HasUpdate (would downgrade to v1.2.3), got %+v", cur, st)
			}
			if !st.CheckFailed {
				t.Fatalf("prerelease current %q must report CheckFailed=true (incomparable), not appear up to date", cur)
			}
		})
	}
}

// TestCheckPrereleaseLatestNeverReportsUpdate: a prerelease latest (e.g.
// v1.3.0-rc1 published as /releases/latest) is incomparable — installing it
// would strand the user on an RC currentUpdatable refuses to advance from.
// Must report CheckFailed (not HasUpdate), matching the `update` CLI's
// rejection of prerelease latests.
func TestCheckPrereleaseLatestNeverReportsUpdate(t *testing.T) {
	withVersion(t, "v1.2.0")
	for _, latest := range []string{"v1.3.0-rc1", "v1.3.0-beta"} {
		t.Run(latest, func(t *testing.T) {
			svc, _, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: latest})
			defer closeSrv()
			st := svc.Check(context.Background())
			if st.HasUpdate {
				t.Fatalf("prerelease latest %q must not report HasUpdate (would strand on RC), got %+v", latest, st)
			}
			if !st.CheckFailed {
				t.Fatalf("prerelease latest %q must report CheckFailed=true (incomparable)", latest)
			}
		})
	}
}

// TestCheckUnusableReleaseStopsTheWalk pins that an answer GitHub actually
// delivered ends the walk. A prerelease or unparseable tag is what every
// route will report — they all read the same release — so walking on buys
// nothing and spends a second request against the quota the fallback exists
// to work around. Contrast TestCheckRouteFailureWalksOn below: a route that
// never delivered an answer IS worth retrying elsewhere.
func TestCheckUnusableReleaseStopsTheWalk(t *testing.T) {
	withVersion(t, "v1.2.0")
	for _, latest := range []string{"v1.3.0-rc1", "not-semver"} {
		t.Run(latest, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(githubRelease{TagName: latest})
			}))
			defer srv.Close()
			svc := NewVersionService("owner/repo", "")
			// Two routes, both pointed at the same server: whatever the walk
			// does, the second request would be answered identically.
			svc.routes = []string{"", ""}
			svc.baseURL = srv.URL

			if st := svc.Check(context.Background()); !st.CheckFailed {
				t.Fatalf("latest %q must report CheckFailed", latest)
			}
			if got := hits.Load(); got != 1 {
				t.Errorf("server saw %d requests, want 1 — the release was delivered and unusable, so the second route can only fetch the same answer", got)
			}
		})
	}
}

// TestCheckRouteFailureWalksOn is the other half of the rule: a route that
// fails to deliver leaves the answer unknown, so the next route is tried.
func TestCheckRouteFailureWalksOn(t *testing.T) {
	withVersion(t, "v1.2.0")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 403 is the shared-quota rejection the direct fallback exists for.
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	svc := NewVersionService("owner/repo", "")
	svc.routes = []string{"", ""}
	svc.baseURL = srv.URL

	if st := svc.Check(context.Background()); !st.CheckFailed {
		t.Fatal("every route rejected the lookup, want CheckFailed")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("server saw %d requests, want 2 — a route that never answered must not end the walk", got)
	}
}

func TestCheckCachesSuccessfulResult(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0"})
	defer closeSrv()

	svc.Check(context.Background())
	svc.Check(context.Background())
	svc.Check(context.Background())
	if got := hits.Load(); got != 1 {
		t.Fatalf("cached success should be reused: expected 1 GitHub hit, got %d", got)
	}
}

func TestCheckPositiveCacheExpiryRefetches(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0"})
	defer closeSrv()

	svc.Check(context.Background())
	time.Sleep(svc.posTTL + 20*time.Millisecond)
	svc.Check(context.Background())
	if got := hits.Load(); got != 2 {
		t.Fatalf("after posTTL expiry a refetch should happen: expected 2 hits, got %d", got)
	}
}

func TestCheckNegativeCachesFailure(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeSrv := newTestService(t, http.StatusInternalServerError, nil)
	defer closeSrv()

	svc.Check(context.Background())
	svc.Check(context.Background())
	if got := hits.Load(); got != 1 {
		t.Fatalf("a failed result should be negatively cached: expected 1 hit, got %d", got)
	}
	if st := svc.Check(context.Background()); !st.CheckFailed {
		t.Fatalf("negatively-cached failure must still report CheckFailed=true")
	}
}

func TestCheckNegativeCacheExpiryRefetches(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeSrv := newTestService(t, http.StatusInternalServerError, nil)
	defer closeSrv()

	svc.Check(context.Background())
	time.Sleep(svc.negTTL + 20*time.Millisecond)
	svc.Check(context.Background())
	if got := hits.Load(); got != 2 {
		t.Fatalf("after negTTL expiry a failed result should refetch: expected 2 hits, got %d", got)
	}
}

func TestCheckDegradesOnHTTPError(t *testing.T) {
	withVersion(t, "v0.1.0")
	// 404 = no releases published yet (the pre-v0.1.0 public state); 403/429
	// = rate limit; 500 = outage. All must degrade to CheckFailed, not panic
	// or surface as a 500 to the admin UI.
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status=%d", status), func(t *testing.T) {
			svc, _, closeSrv := newTestService(t, status, nil)
			defer closeSrv()
			st := svc.Check(context.Background())
			if !st.CheckFailed {
				t.Fatalf("status %d must degrade to CheckFailed=true, got %+v", status, st)
			}
		})
	}
}

func TestCheckDegradesOnBadJSON(t *testing.T) {
	withVersion(t, "v0.1.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()
	svc := NewVersionService("owner/repo", "")
	svc.routes = []string{""} // hermetic: no built-in mirror fallback in unit tests
	svc.baseURL = srv.URL

	if st := svc.Check(context.Background()); !st.CheckFailed {
		t.Fatalf("malformed JSON must degrade to CheckFailed=true, got %+v", st)
	}
}

func TestCheckDegradesOnNonSemverTag(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, _, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "latest"})
	defer closeSrv()

	if st := svc.Check(context.Background()); !st.CheckFailed {
		t.Fatalf("a non-semver tag_name must degrade to CheckFailed=true, got %+v", st)
	}
}

// TestCheckSingleflightCollapsesConcurrentCalls fires many concurrent Check
// calls during one cache window and asserts GitHub is hit exactly once:
// singleflight must serialize them into a single fetch, not fan out.
func TestCheckSingleflightCollapsesConcurrentCalls(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeSrv := newTestService(t, http.StatusOK, githubRelease{TagName: "v0.2.0"})
	defer closeSrv()

	const n = 16
	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for range n {
		go func() {
			defer wg.Done()
			<-start
			_ = svc.Check(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Fatalf("concurrent Checks must collapse to 1 GitHub hit via singleflight, got %d", got)
	}
}

// TestCheckFreshBypassesPositiveCache pins the manual-check contract: an
// operator's explicit "check now" must reach GitHub even while a fresh
// positive cache entry exists, and its result must refresh that cache.
func TestCheckFreshBypassesPositiveCache(t *testing.T) {
	withVersion(t, "v0.1.0")
	svc, hits, closeFn := newTestService(t, http.StatusOK, map[string]any{
		"tag_name": "v0.2.0", "html_url": "https://example/rel",
	})
	defer closeFn()

	if st := svc.Check(context.Background()); !st.HasUpdate {
		t.Fatalf("first check should see the newer release")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("first check should fetch once, got %d", got)
	}

	// Within posTTL a plain Check serves the cache…
	if st := svc.Check(context.Background()); !st.HasUpdate {
		t.Fatalf("cached check should still report the update")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("cached check must not refetch, got %d hits", got)
	}

	// …but CheckFresh must refetch despite the fresh cache.
	if st := svc.CheckFresh(context.Background()); !st.HasUpdate {
		t.Fatalf("fresh check should report the update")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("CheckFresh must bypass the cache and refetch, got %d hits", got)
	}

	// The forced fetch refreshed the cache for later plain Checks.
	if st := svc.Check(context.Background()); !st.HasUpdate {
		t.Fatalf("post-fresh cached check should still report the update")
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("the fresh result must land in the cache (got %d hits)", got)
	}
}

// TestCheckFallsBackToMirror: the About page's badge gates the update
// button, so its lookup must walk the same direct-then-mirror routes the
// updater walks — a blocked direct path with a working mirror must still
// report the release instead of check_failed.
func TestCheckFallsBackToMirror(t *testing.T) {
	withVersion(t, "v0.1.0")
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.0","html_url":"h"}`))
	}))
	defer mirror.Close()

	svc := NewVersionService("owner/repo", "")
	svc.baseURL = "http://127.0.0.1:1" // direct route: refused instantly
	svc.routes = []string{"", mirror.URL}

	st := svc.Check(context.Background())
	if st.CheckFailed || !st.HasUpdate || st.Latest != "v0.2.0" {
		t.Fatalf("mirror fallback must surface the release, got %+v", st)
	}
}

// TestCheckWalkSharesOneTimeoutBudget: the client restarts its timeout per
// request, so a direct-plus-mirror walk would double the documented bound
// without a walk-level deadline. Two hanging routes must together fail
// within roughly ONE client timeout.
func TestCheckWalkSharesOneTimeoutBudget(t *testing.T) {
	withVersion(t, "v0.1.0")
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	svc := NewVersionService("owner/repo", "")
	svc.baseURL = hang.URL
	svc.routes = []string{"", hang.URL}
	svc.client = &http.Client{Timeout: 400 * time.Millisecond}

	start := time.Now()
	st := svc.Check(context.Background())
	if !st.CheckFailed {
		t.Fatalf("two hanging routes must degrade to CheckFailed, got %+v", st)
	}
	if took := time.Since(start); took > 700*time.Millisecond {
		t.Fatalf("the walk took %v — the second route got a fresh timeout instead of the walk's remainder", took)
	}
}

// TestCheckHangingDirectStillReachesTheMirror: a direct request that hangs
// until the walk deadline must not consume the mirror's share — the About
// page must still learn about the release from the mirror.
func TestCheckHangingDirectStillReachesTheMirror(t *testing.T) {
	withVersion(t, "v0.1.0")
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.0","html_url":"h"}`))
	}))
	defer mirror.Close()

	svc := NewVersionService("owner/repo", "")
	svc.baseURL = hang.URL
	svc.routes = []string{"", mirror.URL}
	svc.client = &http.Client{Timeout: 800 * time.Millisecond}

	st := svc.Check(context.Background())
	if st.CheckFailed || !st.HasUpdate || st.Latest != "v0.2.0" {
		t.Fatalf("the mirror must still get a live share of the walk budget, got %+v", st)
	}
}
