package selfupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shrinkStallWindows makes the watchdog act in test time instead of wall
// time, restoring production values afterwards.
func shrinkStallWindows(t *testing.T) {
	t.Helper()
	origInterval, origNoProgress, origProjection := stallSampleInterval, stallNoProgressLimit, stallProjectionAfter
	stallSampleInterval = 20 * time.Millisecond
	stallNoProgressLimit = 400 * time.Millisecond
	stallProjectionAfter = 100 * time.Millisecond
	t.Cleanup(func() {
		stallSampleInterval, stallNoProgressLimit, stallProjectionAfter = origInterval, origNoProgress, origProjection
	})
}

// TestDownloadAbortsWhenNoBytesArrive: a connection that answers the headers
// and then goes silent must be cut by the watchdog as stalled — without it,
// the read would sit inside the whole client timeout before anyone noticed.
func TestDownloadAbortsWhenNoBytesArrive(t *testing.T) {
	shrinkStallWindows(t)
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never send a byte
	}))
	t.Cleanup(silent.Close)

	_, err := downloadVia(context.Background(), &http.Client{Timeout: 5 * time.Second},
		"", silent.URL, false)
	if !errors.Is(err, errDownloadStalled) {
		t.Fatalf("want errDownloadStalled for a silent connection, got %v", err)
	}
}

// TestDownloadAbortsATrickleThatCannotFinish reproduces the field failure:
// bytes keep arriving (so the no-progress guard never fires) but the rate
// against the announced Content-Length can never finish within budget. The
// projection must condemn it early instead of letting it burn the whole
// timeout.
func TestDownloadAbortsATrickleThatCannotFinish(t *testing.T) {
	shrinkStallWindows(t)
	trickle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000000")
		w.WriteHeader(http.StatusOK)
		for { // a few bytes per sample interval: progress, but hopeless
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(trickle.Close)

	start := time.Now()
	_, err := downloadVia(context.Background(), &http.Client{Timeout: 2 * time.Second},
		"", trickle.URL, false)
	if !errors.Is(err, errTransferTooSlow) {
		t.Fatalf("want errTransferTooSlow for a hopeless trickle, got %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the projection took %v to condemn the trickle — it should abort well before the %v budget", took, 2*time.Second)
	}
}

// TestDownloadFallsBackToMirrorAndPromotesIt: when the direct route stalls,
// the same download must succeed through the mirror route, and the mirror
// must be promoted so the run's NEXT download goes straight there instead of
// re-paying the direct probe.
func TestDownloadFallsBackToMirrorAndPromotesIt(t *testing.T) {
	shrinkStallWindows(t)
	var directHits, mirrorHits atomic.Int32
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		directHits.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(dead.Close)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mirrorHits.Add(1)
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(mirror.Close)

	// Route "" fetches the raw URL as-is (the dead server); the mirror route
	// prefixes it, which the mirror handler ignores.
	rs := &routeSet{routes: []string{"", mirror.URL}}
	client := &http.Client{Timeout: 5 * time.Second}

	data, err := rs.download(context.Background(), client, dead.URL)
	if err != nil {
		t.Fatalf("download with mirror fallback: %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("mirror payload mismatch: %q", data)
	}
	if directHits.Load() != 1 || mirrorHits.Load() != 1 {
		t.Fatalf("first download: want 1 direct + 1 mirror hit, got %d/%d", directHits.Load(), mirrorHits.Load())
	}

	// Second download of the same run: the promoted mirror answers first,
	// the dead direct route is never touched again.
	if _, err := rs.download(context.Background(), client, dead.URL); err != nil {
		t.Fatalf("second download: %v", err)
	}
	if directHits.Load() != 1 || mirrorHits.Load() != 2 {
		t.Fatalf("after promotion: want the dead route untouched (1 hit) and the mirror at 2, got %d/%d",
			directHits.Load(), mirrorHits.Load())
	}
}

// TestExplicitProxyFallsBackToDirect: an explicit proxy goes first, but when
// it fails the walk retries direct GitHub — the recovery that matters when a
// shared mirror is rate-limited while the deployment's own path to GitHub is
// healthy. Healthy proxies are unaffected: the direct route only runs after
// the proxy attempt has already failed.
func TestExplicitProxyFallsBackToDirect(t *testing.T) {
	var proxyHits atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(broken.Close)

	rs := newRouteSet(broken.URL)
	if len(rs.routes) != 2 || rs.routes[0] != broken.URL || rs.routes[1] != "" {
		t.Fatalf("explicit proxy must be first with direct behind it, got %v", rs.routes)
	}

	// The asset URL itself is a working origin, so the "" (direct) route
	// behind the failing proxy must serve the download.
	const payload = "asset-bytes"
	asset := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(asset.Close)

	data, err := rs.download(context.Background(), &http.Client{Timeout: 2 * time.Second}, asset.URL)
	if err != nil {
		t.Fatalf("download must fall back to direct after proxy failure: %v", err)
	}
	if string(data) != payload {
		t.Fatalf("fell back but got wrong bytes: %q", string(data))
	}
	if proxyHits.Load() != 1 {
		t.Fatalf("want exactly one attempt against the explicit proxy, got %d", proxyHits.Load())
	}
	// The successful direct route is promoted to the front so later
	// downloads of the same run skip the broken proxy.
	if rs.routes[0] != "" {
		t.Fatalf("successful direct route must be promoted to front, got %v", rs.routes)
	}
}

// TestFetchLatestFallsBackToMirror: the release lookup walks the same route
// set — an unreachable first route must not fail the lookup when the mirror
// can answer.
func TestFetchLatestFallsBackToMirror(t *testing.T) {
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"h","assets":[]}`))
	}))
	t.Cleanup(mirror.Close)

	// First route: a closed port that refuses instantly.
	rs := &routeSet{routes: []string{"http://127.0.0.1:1", mirror.URL}}
	rel, err := rs.fetchLatest(context.Background(), &http.Client{Timeout: 2 * time.Second}, "owner/repo")
	if err != nil {
		t.Fatalf("lookup with mirror fallback: %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Fatalf("mirror release mismatch: %+v", rel)
	}
}

// TestAllRoutesFailingNamesEveryRoute: when nothing works the operator gets
// the whole picture — each route's own failure and the config knob that
// overrides routing — instead of only the last symptom.
func TestAllRoutesFailingNamesEveryRoute(t *testing.T) {
	rs := &routeSet{routes: []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}}
	_, err := rs.fetchLatest(context.Background(), &http.Client{Timeout: time.Second}, "owner/repo")
	if err == nil {
		t.Fatal("two dead routes must fail the lookup")
	}
	for _, want := range []string{"127.0.0.1:1", "127.0.0.1:2", "update.github_proxy"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("all-routes error must mention %q, got: %v", want, err)
		}
	}
}

// TestHealthyDownloadIsNotCondemned: the watchdog must never cut a transfer
// that is actually going to make it — a payload delivered promptly passes
// untouched even with the test's tiny windows.
func TestHealthyDownloadIsNotCondemned(t *testing.T) {
	shrinkStallWindows(t)
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("y", 1<<16)))
	}))
	t.Cleanup(ok.Close)

	data, err := downloadVia(context.Background(), &http.Client{Timeout: 5 * time.Second},
		"", ok.URL, false)
	if err != nil {
		t.Fatalf("healthy download: %v", err)
	}
	if len(data) != 1<<16 {
		t.Fatalf("payload length %d, want %d", len(data), 1<<16)
	}
}

// TestDownloadAbortsWhenHeadersNeverArrive: a route that accepts the TCP
// connection but never sends a response line is neither a connect failure
// nor a body stall — the header watchdog must condemn it within the same
// silence limit instead of sitting out the client's whole timeout.
func TestDownloadAbortsWhenHeadersNeverArrive(t *testing.T) {
	shrinkStallWindows(t)
	headerless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // accept, then never write the response line
	}))
	t.Cleanup(headerless.Close)

	start := time.Now()
	_, err := downloadVia(context.Background(), &http.Client{Timeout: 10 * time.Second},
		"", headerless.URL, false)
	if !errors.Is(err, errDownloadStalled) {
		t.Fatalf("want errDownloadStalled for a headerless connection, got %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the header watchdog took %v — it must fire on the silence limit, not the %v client timeout", took, 10*time.Second)
	}
}

// TestStalledIdentitySurvivesRouteAggregation: errDownloadStalled must stay
// errors.Is-able through the route walk's combined error, so a caller (or a
// future retry policy) can tell a stall from a hard failure.
func TestStalledIdentitySurvivesRouteAggregation(t *testing.T) {
	shrinkStallWindows(t)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(dead.Close)

	rs := &routeSet{routes: []string{"", ""}} // both routes hit the dead server
	_, err := rs.download(context.Background(), &http.Client{Timeout: 5 * time.Second}, dead.URL)
	if !errors.Is(err, errDownloadStalled) {
		t.Fatalf("stall identity lost through route aggregation: %v", err)
	}
}

// TestCancelledContextStopsTheRouteWalk: a caller that has given up must get
// its cancellation back, not a probe of the next route it never asked to
// wait for.
func TestCancelledContextStopsTheRouteWalk(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var secondRouteHits atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondRouteHits.Add(1)
		_, _ = w.Write([]byte("late"))
	}))
	t.Cleanup(second.Close)

	rs := &routeSet{routes: []string{"http://127.0.0.1:1", second.URL}}
	cancel() // the caller is gone before the first route even fails
	_, err := rs.download(ctx, &http.Client{Timeout: time.Second}, "https://example.invalid/asset")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the caller's own cancellation back, got %v", err)
	}
	if secondRouteHits.Load() != 0 {
		t.Fatalf("the walk probed the next route %d times after the caller cancelled", secondRouteHits.Load())
	}
}

// TestRouteErrorsRedactProxyCredentials: a configured proxy URL may carry
// Basic Auth credentials, and route errors are logged and shown in the
// console — the failing route's label must never echo the password back.
func TestRouteErrorsRedactProxyCredentials(t *testing.T) {
	rs := newRouteSet("http://alice:supersecret@127.0.0.1:1")
	_, err := rs.download(context.Background(), &http.Client{Timeout: time.Second},
		"https://example.invalid/asset")
	if err == nil {
		t.Fatal("a dead explicit proxy must surface an error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("route error leaks the proxy password: %v", err)
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("route error should still name the proxy host: %v", err)
	}
}

// TestFinalRouteRidesOutABurstySlowStart: the rate projection exists only to
// reach the NEXT route sooner. On the last route there is nothing to switch
// to, so a bursty-but-viable transfer — a slow warm-up, then full speed —
// must be allowed to finish instead of being condemned on its early average.
func TestFinalRouteRidesOutABurstySlowStart(t *testing.T) {
	shrinkStallWindows(t)
	payload := strings.Repeat("z", 1<<20)
	bursty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		// Warm-up: a trickle long enough that the early average projects
		// far past any budget.
		for i := 0; i < 12; i++ {
			_, _ = w.Write([]byte{payload[i]})
			w.(http.Flusher).Flush()
			time.Sleep(25 * time.Millisecond)
		}
		// Then the burst: the rest arrives at once.
		_, _ = w.Write([]byte(payload[12:]))
	}))
	t.Cleanup(bursty.Close)

	data, err := downloadVia(context.Background(), &http.Client{Timeout: 5 * time.Second},
		"", bursty.URL, true)
	if err != nil {
		t.Fatalf("a viable bursty transfer on the final route must complete, got %v", err)
	}
	if len(data) != len(payload) {
		t.Fatalf("payload length %d, want %d", len(data), len(payload))
	}
}

// TestTransferDoomed pins the projection arithmetic, including that time
// already spent on connection setup and headers counts AGAINST the budget
// left for the body — the client's timeout clock started before the first
// body byte.
func TestTransferDoomed(t *testing.T) {
	cases := []struct {
		name        string
		bodyElapsed time.Duration
		headerDelay time.Duration
		budget      time.Duration
		n, total    int64
		want        bool
	}{
		{"hopeless trickle", 15 * time.Second, 0, 5 * time.Minute, 15 * 1024, 36 << 20, true},
		{"healthy transfer", 15 * time.Second, 0, 5 * time.Minute, 18 << 20, 36 << 20, false},
		{"header delay tips the balance", 15 * time.Second, 29 * time.Second, 5 * time.Minute, 966_367, 18_000_000, true},
		{"same rate, small header delay", 15 * time.Second, 10 * time.Second, 5 * time.Minute, 966_367, 18_000_000, false},
		{"unknown total is never doomed", time.Minute, 0, 5 * time.Minute, 1, -1, false},
		{"no clock is never doomed", time.Minute, 0, 0, 1, 36 << 20, false},
		{"headers ate the whole budget", time.Second, 6 * time.Minute, 5 * time.Minute, 1024, 2048, true},
	}
	for _, tc := range cases {
		if got := transferDoomed(tc.bodyElapsed, tc.headerDelay, tc.budget, tc.n, tc.total); got != tc.want {
			t.Errorf("%s: transferDoomed=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWalkBudgetSpansAllRoutes: the client restarts its timeout per request,
// so without a walk-level deadline a direct-plus-mirror walk would double
// the documented budget. Two dead routes must together fail within roughly
// ONE client timeout, not two.
func TestWalkBudgetSpansAllRoutes(t *testing.T) {
	shrinkStallWindows(t)
	headerless := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(headerless.Close)

	rs := &routeSet{routes: []string{"", headerless.URL}}
	start := time.Now()
	_, err := rs.download(context.Background(), &http.Client{Timeout: 400 * time.Millisecond}, headerless.URL)
	if err == nil {
		t.Fatal("two dead routes must fail the download")
	}
	if took := time.Since(start); took > 650*time.Millisecond {
		t.Fatalf("the walk took %v — the second route got a fresh budget instead of the walk's remainder", took)
	}
}

// TestProjectionKilledRouteGetsASecondChance: the projection is a switching
// heuristic, not proof of death. When the switch target fails too, the
// projection-killed route must be retried once with the projection off —
// otherwise a viable-but-bursty direct route plus a broken mirror would fail
// an update either route order could have completed.
func TestProjectionKilledRouteGetsASecondChance(t *testing.T) {
	shrinkStallWindows(t)
	payload := strings.Repeat("q", 1<<16)
	var directRequests atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if directRequests.Add(1) == 1 {
			// First attempt: a warm-up trickle the projection condemns.
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusOK)
			for i := 0; ; i++ {
				if _, err := w.Write([]byte{payload[i%len(payload)]}); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		// The retry: full speed.
		_, _ = w.Write([]byte(payload))
	}))
	t.Cleanup(direct.Close)
	brokenMirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mirror down", http.StatusBadGateway)
	}))
	t.Cleanup(brokenMirror.Close)

	rs := &routeSet{routes: []string{"", brokenMirror.URL}}
	data, err := rs.download(context.Background(), &http.Client{Timeout: 5 * time.Second}, direct.URL)
	if err != nil {
		t.Fatalf("the projection-killed route must get a projection-free retry, got %v", err)
	}
	if len(data) != len(payload) {
		t.Fatalf("payload length %d, want %d", len(data), len(payload))
	}
	if directRequests.Load() != 2 {
		t.Fatalf("want the direct route hit twice (kill + retry), got %d", directRequests.Load())
	}
}

// TestHangingFirstRouteLeavesBudgetForTheMirror: a direct route that hangs
// until the walk deadline must not starve the mirror — the very scenario the
// fallback exists for. The non-final attempt is capped below the walk
// budget, so the mirror still runs inside a live context and succeeds.
func TestHangingFirstRouteLeavesBudgetForTheMirror(t *testing.T) {
	hangProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hangProxy.Close)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9","html_url":"h","assets":[]}`))
	}))
	t.Cleanup(mirror.Close)

	rs := &routeSet{routes: []string{hangProxy.URL, mirror.URL}}
	rel, err := rs.fetchLatest(context.Background(), &http.Client{Timeout: 800 * time.Millisecond}, "owner/repo")
	if err != nil {
		t.Fatalf("the mirror must still get a live share of the walk budget, got %v", err)
	}
	if rel.TagName != "v9.9.9" {
		t.Fatalf("mirror release mismatch: %+v", rel)
	}
}

// TestRouteNameNeverEchoesAnUnparseableProxy: when the configured proxy is
// malformed or hostless, credentials could hide anywhere in the string — the
// error label must hide the whole value, not fall back to echoing it.
func TestRouteNameNeverEchoesAnUnparseableProxy(t *testing.T) {
	for _, proxy := range []string{
		"alice:supersecret@example.com",  // no scheme: parses hostless
		"http://[::1:supersecret@broken", // does not parse at all
	} {
		got := routeName(proxy)
		if strings.Contains(got, "supersecret") {
			t.Errorf("routeName(%q) leaks the credential: %q", proxy, got)
		}
	}
}

// TestUnknownLengthTrickleStillLeavesMirrorBudget: a direct download with no
// Content-Length that keeps trickling defeats both the silence guard and the
// projection — only the non-final attempt cap stops it from consuming the
// whole walk budget. The mirror must still get its share and complete.
func TestUnknownLengthTrickleStillLeavesMirrorBudget(t *testing.T) {
	shrinkStallWindows(t)
	trickle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // chunked: no Content-Length
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(trickle.Close)
	mirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(mirror.Close)

	rs := &routeSet{routes: []string{"", mirror.URL}}
	data, err := rs.download(context.Background(), &http.Client{Timeout: 800 * time.Millisecond}, trickle.URL)
	if err != nil {
		t.Fatalf("the mirror must still get a live share of the walk budget, got %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("mirror payload mismatch: %q", data)
	}
}

// TestNon200ErrorRedactsProxyCredentials: routeName redacts the LABEL, but a
// status-code failure also embeds the request URL — built with the proxy
// prefix — inside the wrapped error. An authenticated proxy answering an
// error status must not leak its password through that inner URL.
func TestNon200ErrorRedactsProxyCredentials(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	t.Cleanup(broken.Close)
	u, err := url.Parse(broken.URL)
	if err != nil {
		t.Fatal(err)
	}
	authed := "http://alice:supersecret@" + u.Host

	rs := newRouteSet(authed)
	// Redaction is the whole subject here, so the walk is pinned to the
	// proxy: the direct route newRouteSet puts behind it would send the
	// lookup below to the real api.github.com, making a unit test depend on
	// the network and on what owner/repo happens to return.
	rs.routes = []string{authed}
	if _, err := rs.download(context.Background(), &http.Client{Timeout: 2 * time.Second},
		"https://example.invalid/asset"); err == nil {
		t.Fatal("a 502 from the explicit proxy must surface an error")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("status-code error leaks the proxy password: %v", err)
	}
	if _, err := rs.fetchLatest(context.Background(), &http.Client{Timeout: 2 * time.Second},
		"owner/repo"); err == nil {
		t.Fatal("a 502 from the explicit proxy must fail the lookup")
	} else if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("lookup status error leaks the proxy password: %v", err)
	}
}

// TestTooSlowIdentitySurvivesTheWholeWalk: a walk that ends with only a
// projection kill to show for it must still be errors.Is-identifiable as
// too-slow through the aggregation, second pass included.
func TestTooSlowIdentitySurvivesTheWholeWalk(t *testing.T) {
	shrinkStallWindows(t)
	trickle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000000")
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}))
	t.Cleanup(trickle.Close)
	brokenMirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mirror down", http.StatusBadGateway)
	}))
	t.Cleanup(brokenMirror.Close)

	rs := &routeSet{routes: []string{"", brokenMirror.URL}}
	_, err := rs.download(context.Background(), &http.Client{Timeout: time.Second}, trickle.URL)
	if err == nil {
		t.Fatal("a hopeless trickle plus a broken mirror must fail the walk")
	}
	if !errors.Is(err, errTransferTooSlow) {
		t.Fatalf("too-slow identity lost through the walk aggregation: %v", err)
	}
}

// TestSecondPassRetriesShareTheBudgetToo: with several projection-killed
// routes, a retry that has retries still pending behind it must not spend
// the whole remaining walk budget — the later retry must still get its
// share and complete.
func TestSecondPassRetriesShareTheBudgetToo(t *testing.T) {
	shrinkStallWindows(t)
	tricklePayload := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "10000000")
		w.WriteHeader(http.StatusOK)
		for {
			if _, err := w.Write([]byte("x")); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	routeA := httptest.NewServer(http.HandlerFunc(tricklePayload))
	t.Cleanup(routeA.Close)
	var routeBRequests atomic.Int32
	routeB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if routeBRequests.Add(1) == 1 {
			tricklePayload(w, r)
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
	t.Cleanup(routeB.Close)
	deadMirror := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(deadMirror.Close)

	// Route "" downloads rawURL (route A); the other two are proxy prefixes.
	rs := &routeSet{routes: []string{"", routeB.URL, deadMirror.URL}}
	data, err := rs.download(context.Background(), &http.Client{Timeout: 1200 * time.Millisecond}, routeA.URL)
	if err != nil {
		t.Fatalf("the second retry must inherit a live share of the walk budget, got %v", err)
	}
	if string(data) != "payload" {
		t.Fatalf("payload mismatch: %q", data)
	}
	if routeBRequests.Load() != 2 {
		t.Fatalf("want route B hit twice (projection kill + capped-walk retry), got %d", routeBRequests.Load())
	}
}
