// Package version: VersionService checks the latest GitHub release
// for the configured repo, with a short positive cache (so a busy admin UI
// doesn't hammer GitHub) and a short negative cache (so a flapping GitHub or
// rate-limit doesn't either).
package version

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/mod/semver"
	"golang.org/x/sync/singleflight"

	"github.com/yolorouter/yolorouter/internal/version"
)

// VersionStatus is the fully-resolved answer to "is there a newer release?".
// Every field is populated even on failure — Check never returns an error,
// because a failed update check is an expected runtime condition (pre-public
// repo, GitHub outage, rate limit), not something the admin handler should
// surface as a 500.
type VersionStatus struct {
	Current     string
	Latest      string
	HasUpdate   bool
	ReleaseURL  string
	CheckFailed bool
}

// VersionService resolves the latest GitHub release for one repo and caches
// the result. It is safe for concurrent use: a singleflight collapses
// simultaneous refreshes into one GitHub call, and a mutex guards the cache.
type VersionService struct {
	repo string
	// routes is the ordered list of proxy prefixes the lookup tries ("" =
	// direct): the same fallback walk the updater uses, so the About page's
	// "update available" badge appears in exactly the deployments where the
	// fallback-backed update it gates would succeed. An explicitly
	// configured proxy is the sole route.
	routes  []string
	baseURL string // "https://api.github.com" in production; tests inject a httptest URL
	client  *http.Client

	// posTTL caches a successful result; negTTL caches a failure (so a down
	// GitHub or rate-limit doesn't trigger a refresh on every page mount).
	// Both are fields rather than constants so tests can shrink them.
	posTTL time.Duration
	negTTL time.Duration

	g     singleflight.Group
	mu    sync.Mutex
	cache *versionCacheEntry
}

type versionCacheEntry struct {
	latest     string
	releaseURL string
	failed     bool
	fetchedAt  time.Time
}

// NewVersionService builds a service for the given resolved "owner/repo".
// An empty repo disables the service: Check short-circuits to CheckFailed
// without ever touching the network (see ResolveRepo in internal/version for
// how an empty repo is produced from config + the compiled-in default). A
// non-empty proxy routes the release lookup through a mirror prefix (for
// deployments where GitHub is slow or blocked); empty means direct GitHub.
func NewVersionService(repo, proxy string) *VersionService {
	return &VersionService{
		repo:    repo,
		routes:  version.UpdateRoutes(proxy),
		baseURL: "https://api.github.com",
		client:  &http.Client{Timeout: 10 * time.Second},
		posTTL:  10 * time.Minute,
		negTTL:  1 * time.Minute,
	}
}

// Check returns the latest-release status for the configured repo. The ctx
// only gates how long THIS caller waits for the shared fetch (via DoChan +
// select on ctx.Done): the fetch itself runs on a service-owned
// context.Background so a disconnecting caller can't cancel it for everyone.
// client.Timeout bounds the fetch so a stalled GitHub can never hang an admin
// request.
func (s *VersionService) Check(ctx context.Context) VersionStatus {
	return s.check(ctx, false)
}

// CheckFresh is Check minus the cache read: an operator who explicitly
// clicks "check for updates" must get GitHub's current answer, not a result
// cached up to posTTL ago (a release published inside that window would
// otherwise read as "up to date"). The fresh result still lands in the
// cache, so background checks benefit from it.
func (s *VersionService) CheckFresh(ctx context.Context) VersionStatus {
	return s.check(ctx, true)
}

func (s *VersionService) check(ctx context.Context, force bool) VersionStatus {
	current := version.Version

	// Disabled: don't touch the network or the cache.
	if s.repo == "" {
		return VersionStatus{Current: current, CheckFailed: true}
	}

	// Serve a fresh-enough cached entry without re-fetching. Success and
	// failure entries age out on independent clocks (posTTL vs negTTL) so a
	// transient GitHub blip is retried sooner than a stable "no update".
	if !force {
		if entry := s.readCache(); entry != nil {
			return s.buildStatus(current, entry)
		}
	}

	// singleflight (DoChan): concurrent Check callers for the same repo share
	// one fetch run on a service-owned context.Background (NOT a caller's ctx
	// — a disconnecting first caller would otherwise cancel it for everyone).
	// Each caller waits via select on ctx.Done() so a canceled caller stops
	// waiting without aborting the shared fetch; client.Timeout bounds the
	// fetch, and the closure writes the cache when it lands.
	ch := s.g.DoChan(s.repo, func() (any, error) {
		entry := s.fetchLatest(context.Background())
		s.mu.Lock()
		s.cache = entry
		s.mu.Unlock()
		return entry, nil
	})
	select {
	case result := <-ch:
		if result.Val == nil {
			return VersionStatus{Current: current, CheckFailed: true}
		}
		return s.buildStatus(current, result.Val.(*versionCacheEntry))
	case <-ctx.Done():
		return VersionStatus{Current: current, CheckFailed: true}
	}
}

// readCache returns a non-expired cache entry, or nil if absent/expired. The
// TTL depends on whether the cached result was a failure (negTTL) or a
// success (posTTL).
func (s *VersionService) readCache() *versionCacheEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cache == nil {
		return nil
	}
	ttl := s.posTTL
	if s.cache.failed {
		ttl = s.negTTL
	}
	if time.Since(s.cache.fetchedAt) >= ttl {
		return nil
	}
	// Return a copy so callers can't mutate the cached entry.
	out := *s.cache
	return &out
}

func (s *VersionService) buildStatus(current string, entry *versionCacheEntry) VersionStatus {
	st := VersionStatus{
		Current:     current,
		Latest:      entry.latest,
		ReleaseURL:  entry.releaseURL,
		CheckFailed: entry.failed,
	}
	// Only an exact-tag current (valid semver, no prerelease) is comparable.
	// A dev build or a git-describe/RC prerelease is not — it must NOT be
	// reported as "up to date", because the updater (currentUpdatable) refuses
	// such builds and no comparison occurred. Surface it as check_failed so
	// the UI shows "check failed" rather than a misleading "up to date".
	// git-describe strings ("v1.2.3-dirty",
	// "v1.2.3-4-gabc") and RC tags ("v1.2.3-rc1") are semver prereleases
	// ranked below their release, so comparing one against the tag would
	// falsely report "has update" and let `update` downgrade a newer dirty
	// build to the older tag.
	if entry.failed {
		return st
	}
	currentComparable := semver.IsValid(current) && semver.Prerelease(current) == ""
	// latest must also be an exact-tag stable release: a prerelease latest
	// (e.g. v1.3.0-rc1 published as /releases/latest) would install a build
	// currentUpdatable then refuses to advance from, stranding the user on
	// the RC. Treat it as incomparable.
	latestComparable := semver.IsValid(entry.latest) && semver.Prerelease(entry.latest) == ""
	if !currentComparable || !latestComparable {
		st.CheckFailed = true
		return st
	}
	st.HasUpdate = semver.Compare(entry.latest, current) > 0
	return st
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// fetchLatest always returns a non-nil entry: any failure (network, non-200,
// bad JSON, non-semver tag) becomes a failed entry, so Check never has to
// distinguish "couldn't fetch" from "fetched". The entry's fetchedAt starts
// the positive-or-negative cache clock.
//
// The lookup walks s.routes in order and settles on the first route that
// yields a usable release. EVERY failure moves to the next route, content
// failures included — a transparent middlebox can answer a direct request
// with a 200 HTML page that fails to decode, and the mirror is exactly the
// route that gets past it.
//
// The whole walk shares ONE client.Timeout budget: the client restarts its
// clock per request, so without this deadline a direct-plus-mirror walk
// would quietly double the bound the Check documentation promises — later
// routes get only what the earlier ones left.
func (s *VersionService) fetchLatest(ctx context.Context) *versionCacheEntry {
	if s.client.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.client.Timeout)
		defer cancel()
	}
	for i, proxy := range s.routes {
		// A non-final attempt leaves the routes behind it a share of the
		// walk budget — a hanging request must not starve the fallback the
		// walk exists to reach.
		attemptCtx, cancel := version.RouteAttemptContext(ctx, i == len(s.routes)-1)
		entry, routeFailed := s.fetchLatestVia(attemptCtx, proxy)
		cancel()
		if entry != nil {
			return entry
		}
		// The route answered and the answer is simply not installable —
		// every other route reaches the same GitHub release and returns the
		// same thing. Walking on would spend a second request, and a second
		// slice of the very quota this walk exists to conserve, to be told
		// the same thing twice.
		if !routeFailed {
			break
		}
	}
	return s.failEntry()
}

// fetchLatestVia is one route's lookup attempt. A nil entry with routeFailed
// true means this route did not deliver an answer and the next one is worth
// trying; nil with routeFailed false means the route delivered GitHub's
// answer and that answer is not installable, which no other route can
// change.
func (s *VersionService) fetchLatestVia(ctx context.Context, proxy string) (entry *versionCacheEntry, routeFailed bool) {
	url := version.ProxyURL(proxy, fmt.Sprintf("%s/repos/%s/releases/latest", s.baseURL, s.repo))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, true
	}
	// User-Agent is required by the GitHub REST API; without it requests are
	// rejected. Accept pins the documented JSON media type.
	req.Header.Set("User-Agent", "yolorouter")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, true
	}
	defer func() { _ = resp.Body.Close() }()

	// 404 (no releases published yet — the pre-v0.1.0 public state), 403/429
	// (rate limit), and 5xx all degrade identically: check_failed (after the
	// remaining routes also fail), not a 500 to the admin UI.
	if resp.StatusCode != http.StatusOK {
		return nil, true
	}

	var rel githubRelease
	// A body that will not decode is a broken route, not a broken release: a
	// misconfigured proxy answering with its own HTML lands here, and the
	// next route may well return the real JSON.
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, true
	}
	// A tag_name that isn't valid semver can't be compared against current,
	// so treat it as a failed check rather than a misleading "no update".
	if !semver.IsValid(rel.TagName) {
		return nil, false
	}
	// A prerelease latest (v1.3.0-rc1) is incomparable: currentUpdatable
	// refuses to install it and buildStatus reports CheckFailed. Cache it as
	// a FAILURE (negTTL 1min) not a success (posTTL 10min), so a corrected
	// stable release is picked up on the next negTTL cycle rather than being
	// hidden for 10 minutes.
	if semver.Prerelease(rel.TagName) != "" {
		return nil, false
	}
	return &versionCacheEntry{
		latest:     rel.TagName,
		releaseURL: rel.HTMLURL,
		fetchedAt:  time.Now(),
	}, false
}

func (s *VersionService) failEntry() *versionCacheEntry {
	return &versionCacheEntry{failed: true, fetchedAt: time.Now()}
}
