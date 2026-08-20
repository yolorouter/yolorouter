package gateway

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/config"
	"github.com/yolorouter/yolorouter/internal/gateway/circuit"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// circuitGatewayConfig is the default gateway config with the breaker sized
// for a test, so every test names its own thresholds and open window.
func circuitGatewayConfig(failures, successes int, openTimeout time.Duration) config.GatewayConfig {
	g := config.DefaultGatewayConfig()
	g.CircuitFailureThreshold = failures
	g.CircuitSuccessThreshold = successes
	g.CircuitOpenTimeout = openTimeout
	return g
}

// Provider faults open the breaker: after the threshold of 5xx responses the
// next request never reaches the upstream — the candidate is skipped on the
// health record alone.
func TestProviderFaultsOpenTheBreaker(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(3, 2, time.Hour))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	for i := 0; i < 3; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 3 {
		t.Fatalf("upstream calls before open = %d, want 3", before)
	}

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Fatalf("open breaker still dispatched: %d calls, want %d", after, before)
	}
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no dispatchable candidate)", w.Code)
	}
}

// A 2xx whose delivery fails on the provider's side — here a body cut short
// of its declared length, surfacing as a read failure — is a provider fault
// like any 5xx: it counts against the breaker, and enough of them take the
// provider out of rotation. (A body that merely fails OUR passthrough
// rewrite is attributed to the gateway and deliberately books nothing.)
func TestFailedDeliveriesOpenTheBreaker(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real"`))
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, func() config.GatewayConfig {
		g := circuitGatewayConfig(3, 2, time.Hour)
		// A roomy attempt budget so the walk is stopped by the breaker, not
		// by the per-request allowance.
		g.MaxUpstreamAttempts = 100
		return g
	}())
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	for i := 0; i < 3; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 3 {
		t.Fatalf("upstream calls before open = %d, want 3", before)
	}

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Fatalf("open breaker still dispatched after delivery failures: %d calls, want %d", after, before)
	}
}

// A rate limit is load, not ill health: the soft penalty is half-weight, so
// a throttling provider is tolerated to twice the failure threshold — but a
// provider that throttles persistently is still taken out of rotation.
func TestRateLimitsOpenTheBreakerOnlyAtDoubleThreshold(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(2, 2, time.Hour))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Twice the threshold of 2 = 4 rate-limited dispatches to open; the
	// first three must all still be admitted (a hard-failure breaker would
	// have opened after two).
	for i := 0; i < 4; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 4 {
		t.Fatalf("upstream calls = %d, want 4 (half-weight tolerance)", before)
	}

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Fatalf("persistent throttling never opened the breaker: %d calls", after)
	}
}

// A fault booked by one key in a rotation can be the fault that opens the
// breaker; the rotation must then stop instead of dispatching the remaining
// keys to a provider the record just declared down.
func TestTripMidRotationStopsTheKeyLoop(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer upstream.Close()

	// Threshold 1: two soft faults (half-weight) trip the breaker, which
	// happens exactly on the second key of the rotation.
	svc := newSvcWithGateway(t, db, circuitGatewayConfig(1, 2, time.Hour))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-2", "k2", 2, true)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-3", "k3", 3, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (the trip must stop the rotation before key 3)", got)
	}
}

// A stream cut short after commitment cannot fail over — the bytes are on
// the wire — so the soft penalty on the health record is the only protection
// the NEXT request has. Persistent truncation opens the breaker at the
// double threshold.
func TestPersistentStreamTruncationOpensTheBreaker(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-4o-real\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Abort the connection mid-stream: a genuine truncation, not the
		// routine missing-terminator ending (which books nothing).
		panic(http.ErrAbortHandler)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(2, 2, time.Hour))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	for i := 0; i < 4; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	mu.Lock()
	before := calls
	mu.Unlock()
	if before != 4 {
		t.Fatalf("upstream calls = %d, want 4 (half-weight tolerance)", before)
	}

	c, _ := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	mu.Lock()
	after := calls
	mu.Unlock()
	if after != before {
		t.Fatalf("persistent truncation never opened the breaker: %d calls", after)
	}
}

// A stream that merely omits the terminator after a complete answer is a
// documented provider vernacular, classified routine: it must book NOTHING —
// soft-penalising it would open the breaker on a provider that served every
// request.
func TestRoutineNoTerminatorStreamsBookNothing(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-4o-real\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		// The final usage frame marks a finished stream; only [DONE] is
		// omitted — the routine ending several providers ship.
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-4o-real\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(2, 2, time.Hour))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Far past the double threshold: every request must still dispatch.
	for i := 0; i < 6; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 6 {
		t.Fatalf("upstream calls = %d, want 6 (routine endings must not open the breaker)", got)
	}
}

// Routine endings are successes to the health record too: a provider whose
// every stream omits the terminator must still be able to CLOSE a half-open
// breaker after an outage — merely not penalising it would strand the
// provider half-open forever, serving one probe per interval.
func TestRoutineStreamsCloseAHalfOpenBreaker(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-4o-real\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"model\":\"gpt-4o-real\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(2, 2, time.Minute))
	var (
		clockMu sync.Mutex
		fakeNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	)
	svc.breaker = circuit.NewWithClock(circuit.Config{
		FailureThreshold: 2, SuccessThreshold: 2, OpenTimeout: time.Minute,
	}, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fakeNow
	})
	advance := func(d time.Duration) {
		clockMu.Lock()
		fakeNow = fakeNow.Add(d)
		clockMu.Unlock()
	}
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	send := func() {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	send() // 502
	send() // 502 — breaker opens
	advance(time.Minute)
	send() // probe 1: routine stream, must count as success
	advance(30 * time.Second)
	send() // probe 2: closes the breaker
	// Closed again: consecutive requests dispatch without waiting a probe
	// interval.
	send()
	send()
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 6 {
		t.Fatalf("upstream calls = %d, want 6 (routine probes must close the breaker)", got)
	}
}

// A successful, complete delivery resets the failure streak: the threshold
// counts consecutive faults, not faults over a lifetime.
func TestSuccessResetsTheFailureStreak(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		// Requests 1-2 fail, 3 succeeds, 4-5 fail, 6 must still be dispatched.
		if n == 3 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(3, 2, time.Hour))
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	for i := 0; i < 6; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 6 {
		t.Fatalf("upstream calls = %d, want 6 (the success must break the streak)", got)
	}
}

// After the open window a probe goes through, and a healthy answer starts the
// recovery: the caller of the probe request gets the real response, not a
// circuit-open rejection.
func TestOpenWindowElapsesIntoAServedProbe(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var (
		mu    sync.Mutex
		calls int
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, circuitGatewayConfig(3, 2, time.Minute))
	// The breaker gets an injected clock so the open window is crossed by
	// moving time, not by sleeping — a loaded runner cannot turn the
	// while-open assertions flaky.
	var (
		clockMu sync.Mutex
		fakeNow = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	)
	svc.breaker = circuit.NewWithClock(circuit.Config{
		FailureThreshold: 3, SuccessThreshold: 2, OpenTimeout: time.Minute,
	}, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return fakeNow
	})
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	for i := 0; i < 3; i++ {
		c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
	}
	// Open: an immediate request is refused without a dispatch.
	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status while open = %d, want 502", w.Code)
	}
	mu.Lock()
	during := calls
	mu.Unlock()
	if during != 3 {
		t.Fatalf("upstream calls while open = %d, want 3", during)
	}

	clockMu.Lock()
	fakeNow = fakeNow.Add(time.Minute)
	clockMu.Unlock()
	c, w = newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusOK {
		t.Fatalf("probe status = %d, want 200 served through the half-open breaker; body = %s", w.Code, w.Body.String())
	}
}
