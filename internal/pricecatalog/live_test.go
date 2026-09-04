package pricecatalog

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// liveJSON marshals a Catalog the way catalog.json (and the daily cron output)
// is shaped, so the live-refresh path exercises the exact bytes a real endpoint
// returns rather than a hand-built index.
func liveJSON(t *testing.T, c *Catalog) []byte {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal live catalog: %v", err)
	}
	return b
}

// A distinct catalog from fixtureCatalog, so a test can tell a live value apart
// from the embedded seed: different host, different figures. If a Lookup returns
// these numbers, the live index won; if it returns fixtureCatalog's numbers, the
// embed fallback wrongly won (or the swap never happened).
func liveCatalog() *Catalog {
	return &Catalog{
		UpdatedAt: "2099-12-31", // far future so it can never equal the embedded seed
		Currency:  expectedCurrency,
		Unit:      expectedUnit,
		Prices: map[string]map[string]Price{
			"api.live.example": {
				"live-model": {Input: 11, Output: 22, CacheRead: ptr(1.5)},
			},
		},
	}
}

func resetLive(t *testing.T) {
	t.Helper()
	clearLive()
	// Restore the default fetcher in case a prior test swapped it. Defer-chains
	// inside subtests make this mandatory: a leaked fake fetcher would make every
	// later StartRefresh test hit the wrong endpoint.
	origFetch := fetchLive
	t.Cleanup(func() {
		clearLive()
		fetchLive = origFetch
	})
}

// ApplyLive is the seam a real fetch exercises: parse + validate + swap. The
// first thing every test below proves is that after a successful ApplyLive the
// live values win, and after a failed ApplyLive the live index is left untouched
// ("only cover, never delete").
func TestApplyLiveMakesLookupReturnLiveValues(t *testing.T) {
	resetLive(t)

	if err := ApplyLive(liveJSON(t, liveCatalog())); err != nil {
		t.Fatalf("ApplyLive failed: %v", err)
	}

	p, ok := Lookup("https://api.live.example", "live-model")
	if !ok {
		t.Fatal("expected Lookup to hit the live index")
	}
	if p.Input != 11 || p.Output != 22 {
		t.Errorf("live index not in effect: got input=%v output=%v, want 11/22", p.Input, p.Output)
	}
}

// A bad payload (wrong unit) must NOT clear whatever live index was already
// there. This is the "only cover, never delete" rule: a single
// corrupt refresh cannot degrade an already-warm instance.
func TestApplyLiveRejectsBadPayloadKeepsPriorLive(t *testing.T) {
	resetLive(t)

	if err := ApplyLive(liveJSON(t, liveCatalog())); err != nil {
		t.Fatalf("seed live index: %v", err)
	}
	prior, _ := Lookup("https://api.live.example", "live-model")

	bad := liveCatalog()
	bad.Unit = "per_thousand_tokens" // violates the CNY/per_million_tokens contract
	if err := ApplyLive(liveJSON(t, bad)); err == nil {
		t.Fatal("ApplyLive accepted a catalog with the wrong unit")
	}

	// The prior live value must still be observable.
	after, ok := Lookup("https://api.live.example", "live-model")
	if !ok {
		t.Fatal("bad payload wiped the live index; only-cover-never-delete violated")
	}
	if after.Input != prior.Input || after.Output != prior.Output {
		t.Errorf("live index changed after a rejected payload: got %v/%v, want %v/%v",
			after.Input, after.Output, prior.Input, prior.Output)
	}
}

// Unparseable bytes are the other failure mode: a truncated or non-JSON response
// from the endpoint. Same rule — reject, keep what's there.
func TestApplyLiveRejectsUnparseableKeepsPriorLive(t *testing.T) {
	resetLive(t)

	if err := ApplyLive(liveJSON(t, liveCatalog())); err != nil {
		t.Fatalf("seed live index: %v", err)
	}

	if err := ApplyLive([]byte("not json at all")); err == nil {
		t.Fatal("ApplyLive accepted garbage")
	}

	if _, ok := Lookup("https://api.live.example", "live-model"); !ok {
		t.Fatal("garbage payload wiped the live index")
	}
}

// UpdatedAt must reflect the live catalog when one is warm, because that date is
// what an admin sees next to a suggested price — it must say "today's cron", not
// "the day this binary was compiled".
func TestUpdatedAtReflectsLiveWhenWarm(t *testing.T) {
	resetLive(t)

	if got := UpdatedAt(); got == "2099-12-31" {
		t.Fatalf("precondition failed: embed seed already reports the live fixture date")
	}

	if err := ApplyLive(liveJSON(t, liveCatalog())); err != nil {
		t.Fatalf("ApplyLive: %v", err)
	}
	if got := UpdatedAt(); got != "2099-12-31" {
		t.Errorf("UpdatedAt = %q, want the live date 2099-12-31", got)
	}
}

// Lookup must use the live index when warm and fall back to the embedded seed
// when nothing has been applied. The fallback is what keeps a freshly deployed
// instance (no endpoint configured, or the worker still unreachable) serving
// sane prices instead of nothing.
func TestLookupFallsBackToEmbedWhenLiveEmpty(t *testing.T) {
	resetLive(t) // live is empty here

	// The embedded seed's hosts are not in liveCatalog, so a Lookup for an
	// embedded host must still succeed via the fallback. load() already proved
	// the seed has hosts; pick the first one.
	idx, err := load()
	if err != nil {
		t.Fatalf("load embedded seed: %v", err)
	}
	var seedHost, seedModel string
	var seedPrice Price
	for h, ms := range idx.tokens {
		for m, p := range ms {
			seedHost, seedModel, seedPrice = h, m, p
			break
		}
		break
	}
	if seedHost == "" {
		t.Fatal("embedded seed has no entries to test fallback against")
	}

	got, ok := Lookup("https://"+seedHost, seedModel)
	if !ok {
		t.Fatalf("Lookup missed %s/%s with empty live — embed fallback broken", seedHost, seedModel)
	}
	if got.Input != seedPrice.Input || got.Output != seedPrice.Output {
		t.Errorf("fallback returned wrong figures: got %+v, want %+v", got, seedPrice)
	}
}

// The live index and the embed are different datasets, and Lookup unions them:
// live wins where it has an entry, embed fills in where it doesn't. This lets the
// cron refresh a subset of providers (phase 1 ships only 5 of 9 parsers) without
// "losing" the un-refreshed ones — they keep their embedded price as a fallback.
// A model present in both must return the live figures (refreshed overrides
// stale), and a model present in only one must still hit.
func TestLiveUnionsWithEmbed(t *testing.T) {
	resetLive(t)

	// Pick an embed-only host/model before warming.
	idx, _ := load()
	var embedHost, embedModel string
	for h, ms := range idx.tokens {
		for m := range ms {
			embedHost, embedModel = h, m
			break
		}
		break
	}
	if embedHost == "" {
		t.Fatal("embedded seed has no entries to test union against")
	}

	if err := ApplyLive(liveJSON(t, liveCatalog())); err != nil {
		t.Fatalf("ApplyLive: %v", err)
	}

	// Union: embed-only host still hits via fallback.
	if _, ok := Lookup("https://"+embedHost, embedModel); !ok {
		t.Error("embed-only host missed after warm; union fallback broken")
	}
	// Union: live-only host hits.
	if _, ok := Lookup("https://api.live.example", "live-model"); !ok {
		t.Error("live-only model missed after warm")
	}
}

// StartRefresh must apply a successful fetch immediately (not wait for the first
// interval to elapse) so a freshly started instance warms without a delay.
func TestStartRefreshWarmsImmediately(t *testing.T) {
	resetLive(t)
	fetchLive = func(_ context.Context, _ string) ([]byte, error) {
		return liveJSON(t, liveCatalog()), nil
	}

	stop := StartRefresh(context.Background(), "https://example.test/catalog.json", time.Hour)
	defer stop()

	// The refresh is async; poll briefly rather than a fixed sleep so the test is
	// fast on a fast machine and only fails on a genuinely broken warm-on-start.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := Lookup("https://api.live.example", "live-model"); ok {
			return // warmed
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("StartRefresh did not warm the live index immediately")
}

// A payload carrying a leading UTF-8 BOM (EF BB BF) — which some endpoints emit
// in front of the JSON — must still load. json.Unmarshal rejects the BOM, so
// refreshOnce strips it; without that, a catalog that embeds fine would silently
// fail to refresh from such an endpoint. This test is the contract for the
// "strips a BOM" comment in refreshOnce.
func TestStartRefreshStripsLeadingBOM(t *testing.T) {
	resetLive(t)
	good := liveJSON(t, liveCatalog())
	// Prepend the UTF-8 BOM. refreshOnce must strip it before ApplyLive.
	bom := []byte{0xEF, 0xBB, 0xBF}
	fetchLive = func(_ context.Context, _ string) ([]byte, error) {
		return append(bom, good...), nil
	}

	stop := StartRefresh(context.Background(), "https://example.test/catalog.json", time.Hour)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := Lookup("https://api.live.example", "live-model"); ok {
			return // BOM was stripped, payload loaded
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("BOM-prefixed payload did not warm the live index; BOM stripping broken")
}

// A fetch that keeps failing must never wipe an already-warm index, and must
// never panic — the refresh loop outlives any single bad response.
func TestStartRefreshSurvivesFetchErrors(t *testing.T) {
	resetLive(t)
	calls := atomic.Int64{}
	// First call seeds a good index; every subsequent call errors.
	good := liveJSON(t, liveCatalog())
	fetchLive = func(_ context.Context, _ string) ([]byte, error) {
		if calls.Add(1) == 1 {
			return good, nil
		}
		return nil, errors.New("network down")
	}

	stop := StartRefresh(context.Background(), "https://example.test/catalog.json", 5*time.Millisecond)
	defer stop()

	// Let at least one failing tick land.
	time.Sleep(40 * time.Millisecond)

	p, ok := Lookup("https://api.live.example", "live-model")
	if !ok {
		t.Fatal("failing fetch wiped the warm live index; only-cover-never-delete violated in refresh loop")
	}
	if p.Input != 11 {
		t.Errorf("live index corrupted by failing fetch: input=%v want 11", p.Input)
	}
}

// stop must cancel the refresh loop and be safe to call more than once (a
// graceful-shutdown path may race with t.Cleanup). A hung loop would hang the
// test suite.
func TestStopIsIdempotentAndHalts(t *testing.T) {
	resetLive(t)
	fetchLive = func(_ context.Context, _ string) ([]byte, error) {
		return liveJSON(t, liveCatalog()), nil
	}
	stop := StartRefresh(context.Background(), "https://example.test/catalog.json", 5*time.Millisecond)
	stop()
	stop() // must not panic

	// Give the loop a window in which it must NOT still be fetching.
	before := atomic.Int64{}
	fetchLive = func(_ context.Context, _ string) ([]byte, error) {
		before.Add(1)
		return nil, errors.New("should not be called after stop")
	}
	time.Sleep(30 * time.Millisecond)
	if before.Load() != 0 {
		t.Errorf("refresh loop still fetching after stop: %d calls", before.Load())
	}
}

// Concurrent Lookup during a swap must never observe a torn index. atomic.Pointer
// makes the pointer read/writer atomic, but this test pins the contract end-to-end:
// many readers racing a writer all see either the old or the new index wholesale,
// never a mix.
func TestConcurrentLookupDuringSwapIsConsistent(t *testing.T) {
	resetLive(t)

	catalogA := &Catalog{
		UpdatedAt: "2099-01-01", Currency: expectedCurrency, Unit: expectedUnit,
		Prices: map[string]map[string]Price{"api.race": {"m": {Input: 1, Output: 1}}},
	}
	catalogB := &Catalog{
		UpdatedAt: "2099-02-02", Currency: expectedCurrency, Unit: expectedUnit,
		Prices: map[string]map[string]Price{"api.race": {"m": {Input: 2, Output: 2}}},
	}
	if err := ApplyLive(liveJSON(t, catalogA)); err != nil {
		t.Fatalf("seed A: %v", err)
	}

	var stop atomic.Bool
	var wg sync.WaitGroup
	var inconsistent atomic.Int64

	// Swapper: flip A<->B repeatedly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		a := true
		for !stop.Load() {
			c := catalogB
			if a {
				c = catalogA
			}
			_ = ApplyLive(liveJSON(t, c))
			a = !a
		}
	}()

	// Readers: every Lookup must see a self-consistent index (A's pair or B's
	// pair), never input from one and output from the other.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				p, ok := Lookup("https://api.race", "m")
				if !ok {
					inconsistent.Add(1)
					continue
				}
				if p.Input != p.Output { // both A and B have input==output
					inconsistent.Add(1)
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	if inconsistent.Load() > 0 {
		t.Fatalf("observed %d inconsistent reads during concurrent swap", inconsistent.Load())
	}
}

// An empty endpoint string means "do not refresh"; StartRefresh must be a no-op
// rather than fetching an empty URL, so a deployment that opts out (or hasn't
// configured one yet) stays on the embedded seed with zero background work.
func TestStartRefreshNoOpOnEmptyEndpoint(t *testing.T) {
	resetLive(t)
	called := atomic.Bool{}
	fetchLive = func(_ context.Context, _ string) ([]byte, error) {
		called.Store(true)
		return nil, errors.New("should not fetch")
	}

	stop := StartRefresh(context.Background(), "", time.Millisecond)
	if stop != nil {
		stop()
	}
	time.Sleep(20 * time.Millisecond)
	if called.Load() {
		t.Error("empty endpoint triggered a fetch; should be a no-op")
	}
}

// If the live payload carries the same host as the embed but different figures,
// the live figures win. This is the day-to-day case: cron refreshed a price that
// the embed also carries at a stale value.
func TestLiveOverridesEmbeddedHostFigures(t *testing.T) {
	resetLive(t)
	idx, _ := load()
	var host, model string
	for h, ms := range idx.tokens {
		for m := range ms {
			host, model = h, m
			break
		}
		break
	}
	override := &Catalog{
		UpdatedAt: "2099-12-31", Currency: expectedCurrency, Unit: expectedUnit,
		Prices: map[string]map[string]Price{
			host: {model: {Input: 777, Output: 888}},
		},
	}
	if err := ApplyLive(liveJSON(t, override)); err != nil {
		t.Fatalf("ApplyLive: %v", err)
	}
	p, ok := Lookup("https://"+host, model)
	if !ok {
		t.Fatal("override host missed")
	}
	if p.Input != 777 || p.Output != 888 {
		t.Errorf("embed won over live: got %v/%v, want 777/888", p.Input, p.Output)
	}
}

// The audio half follows the same live-wins, embed-fills union as tokens: a
// refreshed character price overrides the seed, and an un-refreshed one keeps
// it.
func TestLiveUnionsWithEmbedForAudio(t *testing.T) {
	resetLive(t)

	seedPrice, ok := LookupAudio("https://api.minimax.cn", "speech-2.8-turbo")
	if !ok || seedPrice != 200 {
		t.Fatalf("embed fallback for audio broken: %v, %v", seedPrice, ok)
	}

	override := &Catalog{
		UpdatedAt: "2099-12-31", Currency: expectedCurrency, Unit: expectedUnit,
		Prices: map[string]map[string]Price{},
		Audio: &AudioCatalog{
			Unit:   expectedAudioUnit,
			Prices: map[string]map[string]float64{"api.minimax.cn": {"speech-2.8-turbo": 123}},
		},
	}
	raw, err := json.Marshal(override)
	if err != nil {
		t.Fatalf("marshal override: %v", err)
	}
	if err := ApplyLive(raw); err != nil {
		t.Fatalf("ApplyLive override: %v", err)
	}
	if p, ok := LookupAudio("https://api.minimax.cn/v1", "speech-2.8-turbo"); !ok || p != 123 {
		t.Errorf("live audio figures did not win: %v, %v", p, ok)
	}
}
