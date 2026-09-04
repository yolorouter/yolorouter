package pricecatalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// live is the warm index fetched from the distribution endpoint (a Cloudflare
// Worker serving the daily-refreshed catalog) and swapped in by the background
// refresh loop below. It is nil until the first successful refresh, in which
// case Lookup falls back to the embedded seed.
//
// atomic.Pointer guarantees that a concurrent Lookup observes either the old or
// the new index in full, never a half-swapped mix — see TestConcurrentLookupDuringSwapIsConsistent.
// The catalogIndex carries both halves (token and audio), so a swap can never
// pair a fresh token section with a stale audio one.
var live atomic.Pointer[catalogIndex]

// liveDate holds the updated_at of the currently-warm live index, surfaced via
// UpdatedAt so an admin sees "today's cron", not "the day this binary compiled".
var liveDate atomic.Pointer[string]

// fetchLive is the function the refresh loop calls to pull catalog bytes. It is
// a package-level variable so tests can swap in a deterministic source without
// spinning up an HTTP server; production code never reassigns it after startup.
var fetchLive = func(ctx context.Context, endpoint string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog endpoint returned %s", resp.Status)
	}
	// Bound the read so a misbehaving endpoint cannot exhaust memory by streaming
	// gigabytes into a single refresh.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxLiveBytes))
	if err != nil {
		return nil, fmt.Errorf("read catalog: %w", err)
	}
	return body, nil
}

// maxLiveBytes caps a single live-catalog fetch. The shipped catalog.json is
// ~8 KB; 1 MiB leaves room for growth across every provider while still
// rejecting a clearly broken response.
const maxLiveBytes = 1 << 20

// ApplyLive parses, validates, and atomically swaps in a catalog payload fetched
// from the distribution endpoint. It is the single seam a real fetch exercises
// and the single seam tests drive directly.
//
// On any failure (unparseable body, wrong currency/unit/date, a buildIndex
// rejection) it returns an error and leaves the currently-warm index untouched —
// the "only cover, never delete" rule: a corrupt refresh therefore
// cannot degrade an already-warm instance; the worst case is "stays as it was".
func ApplyLive(raw []byte) error {
	var c Catalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return fmt.Errorf("parse live catalog: %w", err)
	}
	idx, err := buildIndex(&c)
	if err != nil {
		return fmt.Errorf("validate live catalog: %w", err)
	}
	live.Store(&idx)
	d := c.UpdatedAt
	liveDate.Store(&d)
	return nil
}

// clearLive drops any warm index so Lookup falls back to the embedded seed.
// Test-only: production never clears a warm index (there is no path that should).
func clearLive() {
	live.Store(nil)
	liveDate.Store(nil)
}

// StartRefresh launches a background goroutine that fetches the distribution
// endpoint, applies it via ApplyLive, then repeats every interval. It warms
// immediately rather than waiting for the first tick, so a freshly started
// instance serves live prices without a delay.
//
// An empty endpoint is a no-op returning a nil stop: a deployment that opts out
// (or hasn't configured one yet) stays on the embedded seed with zero background
// work. Each fetch error is swallowed (logged by the caller's framework if it
// wires the endpoint), preserving whatever live index is already warm — the
// refresh loop must outlive any single bad response.
//
// The returned stop function cancels the loop and is safe to call any number of
// times, including concurrently with a fetch in flight.
func StartRefresh(ctx context.Context, endpoint string, interval time.Duration) (stop func()) {
	if endpoint == "" {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	// run pulls once, then on every interval tick, until ctx is cancelled. It is
	// a plain function rather than a goroutine literal so the cancellation path is
	// readable: select on ctx.Done() OR the ticker, never a tight loop.
	run := func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// One immediate warm, then tick. The immediate call is what makes a restart
		// pick up today's prices without waiting up to `interval` first.
		refreshOnce(ctx, endpoint)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshOnce(ctx, endpoint)
			}
		}
	}
	go run()

	var stopOnce sync.Once
	return func() {
		stopOnce.Do(cancel)
		<-done
	}
}

// refreshOnce is one fetch+apply attempt. Errors are swallowed here on purpose:
// the loop must keep running across failures, and ApplyLive already preserves
// the warm index on error (only cover, never delete). A transient outage of the
// distribution endpoint thus degrades silently to the embedded seed until the
// next successful fetch — acceptable for a price catalog that changes daily.
func refreshOnce(ctx context.Context, endpoint string) {
	raw, err := fetchLive(ctx, endpoint)
	if err != nil {
		return
	}
	// Strip a leading UTF-8 BOM (EF BB BF) then trim surrounding whitespace. Some
	// endpoints — particularly those fronted by Windows-origin tooling — emit a
	// BOM before the JSON; json.Unmarshal rejects it, so a catalog that loaded
	// fine from a file would silently fail to refresh from the endpoint.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	_ = ApplyLive(raw) // error preserves the warm index; nothing to do here
}
