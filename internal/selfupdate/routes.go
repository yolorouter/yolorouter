// Route fallback for update traffic. Without a configured proxy: try GitHub
// directly, and when it is unreachable or the download stalls, retry through
// the built-in mirror — so a deployment behind a slow or blocked GitHub
// upgrades without anyone having to discover and hand-edit
// update.github_proxy first. With one: the configured proxy leads and direct
// GitHub trails it as a no-cost fallback.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/yolorouter/yolorouter/internal/version"
)

// routeName renders a proxy prefix for error messages. The rendered form is
// redacted: a proxy URL may carry Basic Auth credentials, and this string
// ends up in errors that the update endpoint logs and returns to the console
// — the transport already redacts ITS copy of the URL, so echoing the raw
// configured value here would undo that.
func routeName(proxy string) string {
	if proxy == "" {
		return "direct"
	}
	if u, err := url.Parse(proxy); err == nil && u.Host != "" {
		return u.Redacted()
	}
	// Unparseable or hostless: credentials could hide anywhere in the
	// string, so nothing of the configured value is safe to echo.
	return "configured proxy (unparseable, value hidden)"
}

// redactURL renders a request URL for error messages with any userinfo
// masked — the proxy prefix a URL was built with may carry credentials, and
// an error that echoes the raw URL would undo routeName's redaction from the
// inside of the %w wrapping.
func redactURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Redacted()
	}
	return "(unparseable url hidden)"
}

// routeSet is the ordered list of proxy prefixes one update run tries, with
// the route that last worked promoted to the front so the later downloads of
// the same run do not re-pay a failed route's probe time.
//
// The order comes from version.UpdateRoutes, which is where the reasoning
// for each shape lives.
type routeSet struct {
	routes []string
}

func newRouteSet(explicitProxy string) *routeSet {
	return &routeSet{routes: version.UpdateRoutes(explicitProxy)}
}

// promote moves routes[i] to the front.
func (rs *routeSet) promote(i int) {
	if i == 0 {
		return
	}
	winner := rs.routes[i]
	copy(rs.routes[1:i+1], rs.routes[:i])
	rs.routes[0] = winner
}

// routeFailures accumulates one walk's per-route failures. The combined
// error names each route's own failure so the operator sees the whole
// picture (and the config knob that overrides routing) instead of only the
// last route's symptom, and every underlying error is wrapped (%w), so a
// sentinel like errDownloadStalled — or the caller's own context.Canceled —
// stays errors.Is-able through the aggregation.
type routeFailures struct {
	format strings.Builder
	args   []any
	n      int
}

func (f *routeFailures) add(proxy string, err error) {
	if f.n > 0 {
		f.format.WriteString("; ")
	}
	f.format.WriteString("%s: %w")
	f.args = append(f.args, routeName(proxy), err)
	f.n++
}

func (f *routeFailures) err(what string) error {
	if f.n == 1 {
		return fmt.Errorf("%s failed — "+f.format.String(), append([]any{what}, f.args...)...)
	}
	return fmt.Errorf("%s failed on every route — "+f.format.String()+
		"; set update.github_proxy to a reachable mirror to override",
		append([]any{what}, f.args...)...)
}

// walkBudget bounds a WHOLE route walk by the client's own timeout. The
// client restarts its clock for every request, so without this a
// direct-plus-mirror walk would quietly double the documented budget — and
// outlive whatever wait the caller sized against it. Attempts after the
// first inherit only what the walk has left, through the context deadline.
func walkBudget(ctx context.Context, client *http.Client) (context.Context, context.CancelFunc) {
	if client.Timeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, client.Timeout)
}

// tryEach runs attempt against every route in order and returns on the first
// success, promoting that route for the rest of the run. Non-final attempts
// are capped below the walk deadline, so one that hangs cannot starve the
// fallback the walk exists for; the bool handed to attempt says whether this
// is the last route, with everything that remains to spend.
//
// No between-routes cancellation check is needed: every attempt carries the
// caller's context into its HTTP request, so once the caller gives up the
// remaining attempts refuse instantly at the transport without contacting
// anything, and the cancellation surfaces through the failure wrapping.
func (rs *routeSet) tryEach(ctx context.Context, what string, attempt func(ctx context.Context, proxy string, lastRoute bool) error) error {
	var failures routeFailures
	for i, proxy := range rs.routes {
		lastRoute := i == len(rs.routes)-1
		attemptCtx, cancel := version.RouteAttemptContext(ctx, lastRoute)
		err := attempt(attemptCtx, proxy, lastRoute)
		cancel()
		if err == nil {
			rs.promote(i)
			return nil
		}
		failures.add(proxy, err)
	}
	return failures.err(what)
}

// fetchLatest is FetchLatestRelease with route fallback, bounded as one walk
// by the client's timeout.
func (rs *routeSet) fetchLatest(ctx context.Context, client *http.Client, repo string) (*Release, error) {
	ctx, cancel := walkBudget(ctx, client)
	defer cancel()
	var rel *Release
	err := rs.tryEach(ctx, "release lookup", func(ctx context.Context, proxy string, _ bool) error {
		got, err := fetchLatestReleaseVia(ctx, client, repo, proxy)
		if err != nil {
			return err
		}
		rel = got
		return nil
	})
	return rel, err
}

// download fetches rawURL with route fallback and stall detection, bounded
// as one walk by the client's timeout.
//
// The walk runs in two passes. The first pass keeps the rate projection on
// for every route but the last, so a route that provably cannot finish in
// time is abandoned early in favor of the next one. The projection is a
// switching heuristic, not proof of death — an early average can undersell a
// transfer that speeds up later — so when every alternative has failed too,
// the second pass re-tries the projection-killed routes with the projection
// off, spending whatever remains of the walk budget on the chance the
// heuristic was wrong. Routes that died of certain causes (no headers, no
// bytes) are not retried.
func (rs *routeSet) download(ctx context.Context, client *http.Client, rawURL string) ([]byte, error) {
	ctx, cancel := walkBudget(ctx, client)
	defer cancel()
	what := "download " + rawURL
	var failures routeFailures
	var projectionKilled []int
	for i, proxy := range rs.routes {
		lastRoute := i == len(rs.routes)-1
		attemptCtx, cancel := version.RouteAttemptContext(ctx, lastRoute)
		data, err := downloadVia(attemptCtx, client, proxy, rawURL, lastRoute)
		cancel()
		if err == nil {
			rs.promote(i)
			return data, nil
		}
		if errors.Is(err, errTransferTooSlow) {
			projectionKilled = append(projectionKilled, i)
		}
		failures.add(proxy, err)
	}
	for retry, i := range projectionKilled {
		if ctx.Err() != nil {
			break
		}
		// Retries share the same budget discipline as the first pass: a
		// retry with retries still pending behind it must leave them a
		// share, only the very last one may spend everything that remains.
		retryCtx, cancel := version.RouteAttemptContext(ctx, retry == len(projectionKilled)-1)
		data, err := downloadVia(retryCtx, client, rs.routes[i], rawURL, true)
		cancel()
		if err == nil {
			rs.promote(i)
			return data, nil
		}
		failures.add(rs.routes[i], fmt.Errorf("second try without the rate projection: %w", err))
	}
	return nil, failures.err(what)
}
