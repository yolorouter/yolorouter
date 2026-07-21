package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	svc := NewVersionService("owner/repo")
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

func TestCheckRepoEmptyShortCircuitsWithoutNetwork(t *testing.T) {
	svc := NewVersionService("")
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
	svc := NewVersionService("owner/repo")
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
