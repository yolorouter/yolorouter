// Package service additions: VersionService checks the latest GitHub release
// for the configured repo, with a short positive cache (so a busy admin UI
// doesn't hammer GitHub) and a short negative cache (so a flapping GitHub or
// rate-limit doesn't either). See design doc
// .claude/docs/2026-07-20-version-update-design.md §4.4.
package service

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
	repo    string
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
// how an empty repo is produced from config + the compiled-in default).
func NewVersionService(repo string) *VersionService {
	return &VersionService{
		repo:    repo,
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
// request — design doc §4.4 / Codex review P2.
func (s *VersionService) Check(ctx context.Context) VersionStatus {
	current := version.Version

	// Disabled: don't touch the network or the cache.
	if s.repo == "" {
		return VersionStatus{Current: current, CheckFailed: true}
	}

	// Serve a fresh-enough cached entry without re-fetching. Success and
	// failure entries age out on independent clocks (posTTL vs negTTL) so a
	// transient GitHub blip is retried sooner than a stable "no update".
	if entry := s.readCache(); entry != nil {
		return s.buildStatus(current, entry)
	}

	// singleflight (DoChan): concurrent Check callers for the same repo share
	// one fetch run on a service-owned context.Background (NOT a caller's ctx
	// — a disconnecting first caller would otherwise cancel it for everyone).
	// Each caller waits via select on ctx.Done() so a canceled caller stops
	// waiting without aborting the shared fetch; client.Timeout bounds the
	// fetch, and the closure writes the cache when it lands (Codex review P2).
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
	// the UI shows "check failed" rather than a misleading "up to date"
	// (Codex review P2). git-describe strings ("v1.2.3-dirty",
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
	// the RC. Treat it as incomparable (Codex review P2).
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
func (s *VersionService) fetchLatest(ctx context.Context) *versionCacheEntry {
	url := fmt.Sprintf("%s/repos/%s/releases/latest", s.baseURL, s.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return s.failEntry()
	}
	// User-Agent is required by the GitHub REST API; without it requests are
	// rejected. Accept pins the documented JSON media type.
	req.Header.Set("User-Agent", "yolorouter")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := s.client.Do(req)
	if err != nil {
		return s.failEntry()
	}
	defer func() { _ = resp.Body.Close() }()

	// 404 (no releases published yet — the pre-v0.1.0 public state), 403/429
	// (rate limit), and 5xx all degrade identically: check_failed, not a 500
	// to the admin UI.
	if resp.StatusCode != http.StatusOK {
		return s.failEntry()
	}

	var rel githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return s.failEntry()
	}
	// A tag_name that isn't valid semver can't be compared against current,
	// so treat it as a failed check rather than a misleading "no update".
	if !semver.IsValid(rel.TagName) {
		return s.failEntry()
	}
	// A prerelease latest (v1.3.0-rc1) is incomparable: currentUpdatable
	// refuses to install it and buildStatus reports CheckFailed. Cache it as
	// a FAILURE (negTTL 1min) not a success (posTTL 10min), so a corrected
	// stable release is picked up on the next negTTL cycle rather than being
	// hidden for 10 minutes (Codex review P2).
	if semver.Prerelease(rel.TagName) != "" {
		return s.failEntry()
	}
	return &versionCacheEntry{
		latest:     rel.TagName,
		releaseURL: rel.HTMLURL,
		fetchedAt:  time.Now(),
	}
}

func (s *VersionService) failEntry() *versionCacheEntry {
	return &versionCacheEntry{failed: true, fetchedAt: time.Now()}
}
