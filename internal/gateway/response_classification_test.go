package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// ─────────────────── writeStreamErrorEvent / sendSSEFrame ────

// failingWriteCloser is an http.ResponseWriter whose Write always returns an
// error, simulating a dead client connection. Used to verify
// sendSSEFrame propagates the Write error and does NOT append to the
// stream capture file when the write failed.
type failingWriteCloser struct {
	headers http.Header
	status  int
}

func (w *failingWriteCloser) Header() http.Header {
	if w.headers == nil {
		w.headers = make(http.Header)
	}
	return w.headers
}
func (w *failingWriteCloser) Write(b []byte) (int, error) {
	return 0, errors.New("simulated write failure")
}
func (w *failingWriteCloser) WriteHeader(code int) { w.status = code }

// TestAFrameThatFailedToSendIsNotRecordedAsSent verifies that when the client
// Write fails, sendSSEFrame returns the error AND does NOT append the
// undelivered bytes to the stream capture file — the capture contract is
// "exactly what the client received", and recording bytes that were never
// sent would mislead audit operators.
func TestAFrameThatFailedToSendIsNotRecordedAsSent(t *testing.T) {
	dir := t.TempDir()
	fw := &failingWriteCloser{}
	c, _ := gin.CreateTestContext(fw)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "req-fail-write", ingress: protocols.ProtocolOpenAI}
	// The stream is already underway where this runs, so its capture is already
	// open; the response object built below writes into it rather than opening
	// one of its own.
	openStreamBodyFile(c, rc)
	defer rc.bodies.CloseStream()

	err := sendSSEFrame(committedStreamClient(t, c, rc), []byte("data: test\n\n"))
	if err == nil {
		t.Fatal("expected a Write error from sendSSEFrame, got nil")
	}
	// The capture file must NOT contain the undelivered bytes.
	captured, readErr := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
	if readErr != nil {
		t.Fatalf("read capture file: %v", readErr)
	}
	if len(captured) > 0 {
		t.Errorf("capture file must be empty on write failure, got %q", captured)
	}
}

// TestAFrameThatWentOutIsRecordedAsSent verifies the success path is
// unchanged: a successful Write still appends to the capture file.
func TestAFrameThatWentOutIsRecordedAsSent(t *testing.T) {
	dir := t.TempDir()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "req-ok-write", ingress: protocols.ProtocolOpenAI}
	openStreamBodyFile(c, rc)
	defer rc.bodies.CloseStream()

	data := []byte("data: hello\n\n")
	if err := sendSSEFrame(committedStreamClient(t, c, rc), data); err != nil {
		t.Fatalf("expected nil error on successful write, got %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Errorf("client body = %q, want %q", rec.Body.Bytes(), data)
	}
	captured, _ := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
	if !bytes.Equal(captured, data) {
		t.Errorf("capture file = %q, want %q (must match client bytes)", captured, data)
	}
}

// TestWriteStreamErrorEvent_FailureReturnsError verifies that
// writeStreamErrorEvent returns the Write error when the client is gone,
// instead of silently swallowing it.
func TestWriteStreamErrorEvent_FailureReturnsError(t *testing.T) {
	dir := t.TempDir()
	fw := &failingWriteCloser{}
	c, _ := gin.CreateTestContext(fw)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "req-fail-evt", ingress: protocols.ProtocolOpenAI}
	openStreamBodyFile(c, rc)
	defer rc.bodies.CloseStream()

	err := writeStreamErrorEvent(committedStreamClient(t, c, rc), rc.ingress, rc.requestID)
	if err == nil {
		t.Fatal("expected a Write error from writeStreamErrorEvent, got nil")
	}
	// No bytes should have been captured — the write failed.
	captured, _ := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
	if len(captured) > 0 {
		t.Errorf("capture file must be empty on total write failure, got %q", captured)
	}
}

// ─────────────────── non-2xx error body slow trickle ────────────────

// TestNon2xxErrorBodySlowTrickle503_BoundedByShortBudget verifies that a
// non-2xx error body whose upstream trickles one byte at a short interval is
// bounded by errorBodyTotalBudget, NOT the full attempt_timeout. Without
// the fix, a trickle faster than BodyIdleTimeout would hold io.ReadAll open
// for 20 minutes.
func TestNon2xxErrorBodySlowTrickle503_BoundedByShortBudget(t *testing.T) {
	// Shrink the error-body budgets so the test is sub-second.
	prevTotal := errorBodyTotalBudget
	prevFirstByte := errorBodyFirstByteTimeout
	errorBodyTotalBudget = 300 * time.Millisecond
	errorBodyFirstByteTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		errorBodyTotalBudget = prevTotal
		errorBodyFirstByteTimeout = prevFirstByte
	})

	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		// Trickle one byte every 50ms — faster than the old BodyIdleTimeout
		// (60s) but would hold io.ReadAll open indefinitely without the
		// short total budget. Stop when the client (gateway) closes the
		// connection so the test server can shut down cleanly.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("x"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	start := time.Now()
	svc.Handle(c, apiKey)
	elapsed := time.Since(start)

	// Must complete well within the attempt timeout (20m). The error body
	// budget is 300ms, so a healthy run finishes in well under a second.
	//
	// The ceiling is nonetheless generous, because the regression it guards
	// against is a 20-MINUTE hang: anything in seconds already proves the total
	// budget bounded the read. A tight bound instead measures how loaded the
	// machine is — this assertion tripped at 5.8s on a contended two-core CI
	// runner where merely creating the test database took over three seconds,
	// while the same code takes ~0.3s on an idle host.
	if elapsed > 30*time.Second {
		t.Errorf("slow-trickle 503 took %v, expected < 30s (short total budget)", elapsed)
	}
	// The gateway should fail over and eventually return 502 (all candidates
	// failed) — NOT hang.
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 (all candidates failed after bounded error-body read)", w.Code)
	}
}

// TestUnauthorized401_CASPersistsKeyFailureBeforeBodyRead verifies that a
// 401 response triggers the CAS persisting verification_status=Failed BEFORE
// the slow error body read. Without this ordering, a slow-trickle upstream
// could exhaust the attempt context during the body read, causing the CAS to
// fail on an expired context and leaving the dead key marked as valid.
func TestUnauthorized401_CASPersistsKeyFailureBeforeBodyRead(t *testing.T) {
	// Shrink the error-body budgets so the test is sub-second.
	prevTotal := errorBodyTotalBudget
	prevFirstByte := errorBodyFirstByteTimeout
	errorBodyTotalBudget = 300 * time.Millisecond
	errorBodyFirstByteTimeout = 300 * time.Millisecond
	t.Cleanup(func() {
		errorBodyTotalBudget = prevTotal
		errorBodyFirstByteTimeout = prevFirstByte
	})

	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		// Trickle the error body slowly — would exhaust the context if the
		// CAS ran AFTER the read. Stop when the gateway closes the connection.
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte("x"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-dead", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, _ := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	// The key's verification_status MUST be persisted as Failed even though
	// the slow error body read would have exhausted the attempt context.
	keys, err := repository.ListProviderKeysByProvider(db, p.ID)
	if err != nil {
		t.Fatalf("list provider keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].VerificationStatus != model.VerificationStatusFailed {
		t.Errorf("key verification_status = %d, want %d (CAS must persist before error body read exhausts ctx)",
			keys[0].VerificationStatus, model.VerificationStatusFailed)
	}
}

// ─────────────────── IR downstream write timeout classification ─────

// TestIsClientWriteError_ClassifiesDeadlineAndDisconnect is a table-driven
// unit test for the classifier that distinguishes downstream (client-side)
// write failures from upstream server faults.
func TestIsClientWriteError_ClassifiesDeadlineAndDisconnect(t *testing.T) {
	// Simulate a net.Error timeout (the shape SetWriteDeadline produces).
	// A bare timeout error is NOT classified as a client write error — it
	// could originate from the upstream attempt context
	// (context.DeadlineExceeded also satisfies net.Error.Timeout()).
	deadlineErr := &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// A bare timeout is ambiguous (could be upstream) — must NOT match.
		{"bare deadline exceeded (net.OpError)", deadlineErr, false},
		{"bare os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, false},
		// The sentinel identifies a genuine downstream write failure.
		{"sentinel wrapped", fmt.Errorf("%w: write tcp: %w", protocols.ErrClientWrite, deadlineErr), true},
		{"sentinel outer wrap", fmt.Errorf("stream write failed: %w", protocols.ErrClientWrite), true},
		{"plain error", errors.New("some upstream error"), false},
		// A bare io.ErrClosedPipe is ambiguous: an upstream RST while
		// reading the response body can surface the exact same error, so it
		// must NOT classify as a client write failure unless a write site
		// wrapped it in protocols.ErrClientWrite.
		{"bare closed pipe (read-side, not a write failure)", io.ErrClosedPipe, false},
		{"closed pipe wrapped at a write site", fmt.Errorf("%w: write to client: %w", protocols.ErrClientWrite, io.ErrClosedPipe), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isClientWriteError(tt.err)
			if got != tt.want {
				t.Errorf("isClientWriteError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestASlowCallerOnACrossProtocolStreamIsBlamedForTheirOwnTimeout drives
// the cross-protocol IR streaming path with a slow client writer that fills
// its buffer and then blocks until the sliding write deadline fires. The
// resulting write error must be classified as a client-side timeout
// (AttemptConnError + "client write timeout"), NOT AttemptServerError, and
// no second error frame may be emitted.
func TestASlowCallerOnACrossProtocolStreamIsBlamedForTheirOwnTimeout(t *testing.T) {
	// Shrink streamWriteWindow so the test is fast.
	prevWindow := protocols.StreamWriteWindow()
	protocols.SetStreamWriteWindow(150 * time.Millisecond)
	t.Cleanup(func() { protocols.SetStreamWriteWindow(prevWindow) })

	db := testutil.NewSQLiteDB(t)

	// Upstream speaks Claude SSE — sends events continuously until the
	// client (gateway) closes the connection. The large per-event payload
	// ensures the client's TCP + kernel buffers fill, which is what triggers
	// the sliding write deadline on the gateway side.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// message_start is required for the Claude decoder to initialize.
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Send large content deltas in a tight loop until the gateway
		// closes the connection (write deadline fires on the gateway's
		// side, IRStreamRelay returns, resp.Body.Close propagates here).
		pad := strings.Repeat("x", 8192)
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"%s\"}}\n\n", pad)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-p", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Use a real http.Server so SetWriteDeadline is honored through
	// http.NewResponseController.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var capturedRC atomic.Pointer[Exchange]
	prevHook := testHookHandleDone
	testHookHandleDone = func(rc *Exchange) {
		capturedRC.Store(rc)
	}
	t.Cleanup(func() { testHookHandleDone = prevHook })

	r.POST("/v1/chat/completions", func(c *gin.Context) {
		// Fake auth — the real middleware isn't needed for this test.
		c.Set("request_id", "req-slow-client")
		svc.Handle(c, apiKey)
	})

	srv := &http.Server{Handler: r, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close(); _ = ln.Close() }()

	// Connect but do NOT read the body — the handler's Writes will
	// eventually block in the TCP buffer, and the sliding write deadline
	// will cut it.
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post("http://"+ln.Addr().String()+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Wait for the handler to finish (the sliding deadline should fire and
	// end the attempt within a few streamWriteWindow intervals).
	waitStart := time.Now()
	for time.Since(waitStart) < 10*time.Second {
		if capturedRC.Load() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rc := capturedRC.Load()
	if rc == nil {
		t.Fatal("handler did not complete within 10s — sliding deadline should have fired")
	}

	// Verify the attempt was classified as a client write timeout, not an
	// upstream server error.
	if len(rc.attempts) == 0 {
		t.Fatal("expected at least one attempt record")
	}
	last := rc.attempts[len(rc.attempts)-1]
	if last.Outcome != AttemptConnError {
		t.Errorf("attempt outcome = %q, want %q (client write timeout must not be classified as upstream server fault)",
			last.Outcome, AttemptConnError)
	}
	if !strings.Contains(last.FailReason, "client_write_timeout") {
		t.Errorf("fail_reason = %q, want it to name the client write timeout", last.FailReason)
	}
}

// TestABrokenProviderOnACrossProtocolStreamIsStillFiledAsPartial is the
// complement: an upstream read error (not a client write error) after the
// first byte must still be classified as AttemptServerError + stream_partial,
// proving the classifier does not over-classify.
func TestABrokenProviderOnACrossProtocolStreamIsStillFiledAsPartial(t *testing.T) {
	prevWindow := protocols.StreamWriteWindow()
	protocols.SetStreamWriteWindow(150 * time.Millisecond)
	t.Cleanup(func() { protocols.SetStreamWriteWindow(prevWindow) })

	db := testutil.NewSQLiteDB(t)

	// Upstream sends one event then abruptly closes mid-stream (clean
	// truncation without message_stop — a genuine upstream fault).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Abort: hijack and close the underlying connection to simulate an
		// upstream that drops the stream after the first byte.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	p := createAnthropicProvider(t, db, "claude-p2", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude2", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o-2", "claude-3-5-sonnet-20241022", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	gin.SetMode(gin.TestMode)
	r := gin.New()
	var capturedRC atomic.Pointer[Exchange]
	prevHook := testHookHandleDone
	testHookHandleDone = func(rc *Exchange) {
		capturedRC.Store(rc)
	}
	t.Cleanup(func() { testHookHandleDone = prevHook })

	r.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set("request_id", "req-upstream-fault")
		svc.Handle(c, apiKey)
	})

	srv := &http.Server{Handler: r, ReadTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close(); _ = ln.Close() }()

	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"gpt-4o-2","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	waitStart := time.Now()
	for time.Since(waitStart) < 10*time.Second {
		if capturedRC.Load() != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rc := capturedRC.Load()
	if rc == nil {
		t.Fatal("handler did not complete within 10s")
	}

	// An upstream read error must NOT be classified as client_write_timeout.
	// It should be AttemptServerError (or similar upstream-fault outcome).
	if len(rc.attempts) == 0 {
		t.Fatal("expected at least one attempt record")
	}
	last := rc.attempts[len(rc.attempts)-1]
	if strings.Contains(last.FailReason, "client_write_timeout") {
		t.Errorf("upstream read error was misclassified as client_write_timeout: fail_reason=%q", last.FailReason)
	}
}

// committedStreamClient builds the response object a mid-stream write goes
// through, already committed — which is the only state these frames are ever
// written in, since a stream that has not committed has no error frame to add
// to.
func committedStreamClient(t *testing.T, c *gin.Context, rc *Exchange) ClientResponse {
	t.Helper()
	w := realStreamClient(&Service{}, c, rc)
	if err := w.Commit(http.StatusOK); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return w
}
