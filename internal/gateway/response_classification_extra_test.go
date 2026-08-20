package gateway

import (
	"bytes"
	"context"
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

// ─────────────────── upstream DeadlineExceeded not misclassified ────

// TestAnUpstreamDeadlineIsNotACallerWriteTimeout
// verifies that when the attempt context deadline fires after the first byte
// (a genuine upstream read timeout), the error is classified as
// AttemptServerError + stream_partial and an inline error frame is emitted —
// NOT misclassified as client_write_timeout. context.DeadlineExceeded
// satisfies net.Error.Timeout(); the old broad classifier would have
// misclassified it as a client write failure, skipping the error frame and
// recording 499.
func TestAnUpstreamDeadlineIsNotACallerWriteTimeout(t *testing.T) {
	// Use a short AttemptTimeout so the upstream deadline fires quickly.
	gw := testGatewayConfig()
	gw.AttemptTimeout = 2 * time.Second
	gw.RequestTimeout = 5 * time.Second
	// BodyIdleTimeout must be larger than AttemptTimeout so the idle wrapper
	// does not fire before the attempt context deadline.
	gw.BodyIdleTimeout = 10 * time.Second
	gw.FirstByteTimeout = 10 * time.Second

	db := testutil.NewSQLiteDB(t)

	// Upstream sends one event then stalls forever (never closes, never sends
	// more data). The attempt context deadline will fire while blocked on the
	// upstream read — a genuine upstream timeout, not a client write failure.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// message_start initializes the decoder but produces no client-facing
		// chunk on its own; the text delta after it is what makes the encoder
		// emit, which is what commits the response. Without that second frame
		// the deadline below is met by a stream that never started, and this
		// test would exercise the pre-commit failover branch instead of the
		// mid-stream one it is about.
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-3-5-sonnet\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		// Block until the server is shut down — the attempt ctx deadline fires
		// while the gateway is blocked reading the next upstream chunk.
		<-r.Context().Done()
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, gw)
	p := createAnthropicProvider(t, db, "claude-p-stall", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-stall", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o-stall", "claude-3-5-sonnet-20241022", true, true, 1)
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
		c.Set("request_id", "req-upstream-deadline")
		svc.Handle(c, apiKey)
	})

	srv := &http.Server{Handler: r, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close(); _ = ln.Close() }()

	// Connect and read actively so the first byte goes through; then keep
	// reading slowly (the upstream stalls anyway).
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"gpt-4o-stall","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("connect to gateway: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Wait for the handler to finish — the attempt deadline (2s) should fire.
	waitStart := time.Now()
	for time.Since(waitStart) < 10*time.Second {
		if capturedRC.Load() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	rc := capturedRC.Load()
	if rc == nil {
		t.Fatal("handler did not complete within 10s — attempt deadline should have fired")
	}

	if len(rc.attempts) == 0 {
		t.Fatal("expected at least one attempt record")
	}
	last := rc.attempts[len(rc.attempts)-1]

	// This case is about the mid-stream branch, and the two branches are not
	// told apart by the outcome: attemptOutcomeFor maps every non-client fault
	// on a stream to AttemptServerError, so a stream that died BEFORE committing
	// produces the same label. The reason code is what distinguishes them, and
	// without this assertion the fixture could stop committing the response and
	// nothing here would notice.
	if !strings.HasPrefix(last.FailReason, "stream_partial") {
		t.Errorf("fail reason = %q, want it to start with stream_partial — "+
			"anything else means the stream never committed and this test is "+
			"exercising the pre-commit failover branch instead", last.FailReason)
	}
	// The upstream timeout must NOT be blamed on the caller.
	if strings.Contains(last.FailReason, "client_write_timeout") {
		t.Errorf("upstream DeadlineExceeded was misclassified as client_write_timeout: fail_reason=%q", last.FailReason)
	}
	// It should be AttemptServerError (upstream fault), with stream_partial.
	if last.Outcome != AttemptServerError {
		t.Errorf("attempt outcome = %q, want %q (upstream deadline = server error, not client write)",
			last.Outcome, AttemptServerError)
	}
}

// ─────────────────── CAS survives attempt ctx expiry ────────────────

// TestUnauthorized401_CASSurvivesAttemptContextExpiry verifies that the 401
// CAS (MarkProviderKeyVerificationFailedIfCurrent) uses a context detached
// from the attempt deadline. A 401 arriving near the attempt deadline would
// cause the CAS's UPDATE to be cancelled if it shared the attempt context,
// leaving the dead key marked as valid.
func TestUnauthorized401_CASSurvivesAttemptContextExpiry(t *testing.T) {
	// Short AttemptTimeout so the ctx is nearly expired by the time the 401
	// response arrives.
	gw := testGatewayConfig()
	gw.AttemptTimeout = 500 * time.Millisecond
	gw.RequestTimeout = 5 * time.Second
	gw.HeaderTimeout = 10 * time.Second
	gw.FirstByteTimeout = 10 * time.Second
	gw.BodyIdleTimeout = 10 * time.Second

	db := testutil.NewSQLiteDB(t)

	// Upstream waits just shy of the attempt deadline, then returns 401.
	// By the time the CAS runs, the attempt ctx is about to expire (or has
	// expired). The detached CAS context must still complete the UPDATE.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep almost as long as the attempt budget so the CAS runs against
		// a nearly-dead context. 450ms < 500ms attempt timeout.
		time.Sleep(450 * time.Millisecond)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer upstream.Close()

	svc := newSvcWithGateway(t, db, gw)
	p := createProvider(t, db, "p-cas-expiry", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-dead-cas", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o-cas-expiry", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, _ := newCtx([]byte(`{"model":"gpt-4o-cas-expiry","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	// The key's verification_status MUST be persisted as Failed despite the
	// attempt context being nearly or already expired when the CAS ran.
	keys, err := repository.ListProviderKeysByProvider(db, p.ID)
	if err != nil {
		t.Fatalf("list provider keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].VerificationStatus != model.VerificationStatusFailed {
		t.Errorf("key verification_status = %d, want %d (CAS must survive attempt ctx expiry via detached context)",
			keys[0].VerificationStatus, model.VerificationStatusFailed)
	}
}

// TestUnauthorized401_CASSurvivesClientCancel verifies that a client cancel
// arriving between the 401 response headers and the CAS does not abort the
// UPDATE — the detached context (WithoutCancel) is immune to client-side
// cancellation.
func TestUnauthorized401_CASSurvivesClientCancel(t *testing.T) {
	// Shrink the error-body budgets so the test is sub-second.
	prevTotal := errorBodyTotalBudget
	prevFirstByte := errorBodyFirstByteTimeout
	errorBodyTotalBudget = 500 * time.Millisecond
	errorBodyFirstByteTimeout = 500 * time.Millisecond
	t.Cleanup(func() {
		errorBodyTotalBudget = prevTotal
		errorBodyFirstByteTimeout = prevFirstByte
	})

	gw := testGatewayConfig()
	gw.AttemptTimeout = 10 * time.Second
	gw.RequestTimeout = 30 * time.Second
	gw.HeaderTimeout = 10 * time.Second
	gw.FirstByteTimeout = 10 * time.Second
	gw.BodyIdleTimeout = 10 * time.Second

	db := testutil.NewSQLiteDB(t)

	// Upstream returns 401 quickly, but with a slow body so the gateway is
	// still reading when the client cancels.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		flusher, _ := w.(http.Flusher)
		if flusher != nil {
			flusher.Flush()
		}
		// Trickle the body so the client has time to cancel mid-read.
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

	svc := newSvcWithGateway(t, db, gw)
	p := createProvider(t, db, "p-cas-cancel", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-dead-cancel", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o-cas-cancel", "gpt-4o-real", true, true, 1)
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
		c.Set("request_id", "req-cancel-cas")
		svc.Handle(c, apiKey)
	})

	srv := &http.Server{Handler: r, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close(); _ = ln.Close() }()

	// Send a request, then cancel it almost immediately — before the CAS
	// has time to run on the shared attempt context, but the detached
	// context keeps the CAS alive.
	client := &http.Client{Timeout: 30 * time.Second}
	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	req, _ := http.NewRequestWithContext(reqCtx, http.MethodPost,
		"http://"+ln.Addr().String()+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-cas-cancel","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err == nil {
		// Cancel after a short delay — the CAS is already running on the
		// detached context and must survive.
		go func() {
			time.Sleep(100 * time.Millisecond)
			reqCancel()
		}()
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}

	// Wait for the handler to finish.
	waitStart := time.Now()
	for time.Since(waitStart) < 15*time.Second {
		if capturedRC.Load() != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Even though the client cancelled, the CAS must have persisted.
	keys, err := repository.ListProviderKeysByProvider(db, p.ID)
	if err != nil {
		t.Fatalf("list provider keys: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].VerificationStatus != model.VerificationStatusFailed {
		t.Errorf("key verification_status = %d, want %d (CAS must survive client cancel via detached context)",
			keys[0].VerificationStatus, model.VerificationStatusFailed)
	}
}

// ─────────────────── FlushError timing in sendSSEFrame ────────

// flushErrorWriter is an http.ResponseWriter whose Write succeeds but whose
// FlushError returns an error. This simulates the net/http behavior where a
// small Write is buffered and returns nil, but the real socket error surfaces
// only when Flush pushes bytes onto the wire. http.NewResponseController
// discovers the FlushError method via its unwrap chain through gin's
// responseWriter.
type flushErrorWriter struct {
	headers http.Header
	status  int
	flushed bool
}

func (w *flushErrorWriter) Header() http.Header {
	if w.headers == nil {
		w.headers = make(http.Header)
	}
	return w.headers
}
func (w *flushErrorWriter) Write(b []byte) (int, error) {
	return len(b), nil // succeed — bytes are buffered, not yet on the wire
}
func (w *flushErrorWriter) WriteHeader(code int) { w.status = code }

// Flush satisfies http.Flusher (no-op; the real error path is FlushError).
func (w *flushErrorWriter) Flush() { w.flushed = true }

// FlushError returns an error — discovered by http.NewResponseController.
func (w *flushErrorWriter) FlushError() error {
	return errors.New("simulated flush failure")
}

// TestAFrameLostAtTheFlushIsNotRecordedAsSent verifies that when Write
// succeeds but Flush fails (buffered bytes never reached the client),
// sendSSEFrame returns the flush error AND does NOT append the
// undelivered bytes to the stream capture file.
func TestAFrameLostAtTheFlushIsNotRecordedAsSent(t *testing.T) {
	dir := t.TempDir()
	fw := &flushErrorWriter{}
	c, _ := gin.CreateTestContext(fw)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "req-flush-err", ingress: protocols.ProtocolOpenAI}
	openStreamBodyFile(c, rc)
	defer rc.bodies.CloseStream()

	err := sendSSEFrame(committedStreamClient(t, c, rc), []byte("data: test\n\n"))
	if err == nil {
		t.Fatal("expected a Flush error from sendSSEFrame, got nil")
	}
	// The capture file must NOT contain the undelivered bytes.
	captured, readErr := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
	if readErr != nil {
		t.Fatalf("read capture file: %v", readErr)
	}
	if len(captured) > 0 {
		t.Errorf("capture file must be empty on flush failure, got %q", captured)
	}
}

// TestAFrameIsRecordedOnlyOnceItsFlushSucceeds verifies the success path is
// unchanged: when both Write and Flush succeed, the bytes are still appended
// to the capture file.
func TestAFrameIsRecordedOnlyOnceItsFlushSucceeds(t *testing.T) {
	dir := t.TempDir()
	// httptest.ResponseRecorder implements http.Flusher; NewResponseController
	// discovers it and returns nil from Flush().
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(BodiesDirContextKey, dir)
	rc := &Exchange{requestID: "req-flush-ok", ingress: protocols.ProtocolOpenAI}
	openStreamBodyFile(c, rc)
	defer rc.bodies.CloseStream()

	data := []byte("data: hello\n\n")
	if err := sendSSEFrame(committedStreamClient(t, c, rc), data); err != nil {
		t.Fatalf("expected nil error on successful write+flush, got %v", err)
	}
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Errorf("client body = %q, want %q", rec.Body.Bytes(), data)
	}
	captured, _ := os.ReadFile(filepath.Join(dir, rc.requestID+".stream"))
	if !bytes.Equal(captured, data) {
		t.Errorf("capture file = %q, want %q", captured, data)
	}
}

// ─────────────────── sentinel unit test ─────────────

// TestIsClientWriteError_SentinelOnlyForTimeout verifies that the classifier
// uses the dedicated ErrClientWrite sentinel for timeout classification, and
// does NOT match bare net.Error.Timeout() or context.DeadlineExceeded (which
// could originate from the upstream attempt context).
func TestIsClientWriteError_SentinelOnlyForTimeout(t *testing.T) {
	// context.DeadlineExceeded satisfies net.Error (Timeout()=true,
	// Temporary()=false) but must NOT be classified as a client write error.
	deadlineErr := context.DeadlineExceeded

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"context.DeadlineExceeded bare", deadlineErr, false},
		{"wrapped context.DeadlineExceeded", fmt.Errorf("upstream read: %w", deadlineErr), false},
		{"net.OpError write timeout (bare)", &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}, false},
		{"sentinel (ErrClientWrite)", protocols.ErrClientWrite, true},
		{"sentinel wrapped with cause", fmt.Errorf("%w: %v", protocols.ErrClientWrite, deadlineErr), true},
		{"double-wrapped sentinel", fmt.Errorf("stream write failed: %w", fmt.Errorf("%w: write tcp", protocols.ErrClientWrite)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isClientWriteError(tt.err); got != tt.want {
				t.Errorf("isClientWriteError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// ─────────────────── IR mid-stream client-disconnect precheck ──────

// TestACallerDisconnectOnTheIRPathIsNotBlamedOnTheProvider covers the
// dormant cross-protocol IR path: unlike the passthrough stream pumps (which
// detect a caller disconnect via ctx.Done()/errClientDisconnected before the
// failure is ever classified), protocols.IRStreamRelay's scanner loop returns
// a caller disconnect as a plain
// "upstream stream read error: context canceled" — neither wrapped in
// protocols.ErrClientWrite nor the gateway's own errClientDisconnected
// sentinel. Before this fix that fell through to the generic mid-stream
// server-failure branch: an inline SSE error frame was written to an
// already-dead connection and the row was misrecorded as stream_partial,
// wrongly blaming the provider for the caller's own disconnect.
func TestACallerDisconnectOnTheIRPathIsNotBlamedOnTheProvider(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	p := createProvider(t, db, "p-mid-disconnect", "http://upstream.invalid")
	m := createModelAndCandidate(t, db, p, "gpt-4o-mid-disconnect", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var cand model.ModelCandidate
	if err := db.Where("model_id = ?", m.ID).First(&cand).Error; err != nil {
		t.Fatalf("load seeded candidate: %v", err)
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller's own connection is already gone
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)

	rc := &Exchange{requestID: "req-ir-mid-stream-disconnect", originalModel: "gpt-4o-mid-disconnect",
		apiKeyID: apiKey.ID, ingress: protocols.ProtocolOpenAI, isStream: true}
	rc.markFirstByteSent()

	// The exact unwrapped shape the streaming scanner loop returns for a caller
	// disconnect: `fmt.Errorf("upstream stream read error: %w", err)`, which is
	// neither wrapped in protocols.ErrClientWrite nor in the gateway's own
	// disconnect sentinel.
	err := fmt.Errorf("upstream stream read error: %w", context.Canceled)
	resp := &http.Response{StatusCode: http.StatusOK}

	// A caller who is already gone, on a response they were served before they
	// left.
	client := &stubClient{committed: true, status: http.StatusOK, gone: true}
	payload := streamingPayload(t, protocols.ProtocolOpenAI, streamCallerBody)
	d := payload.settleStream(DeliveryTools{Client: client, RequestID: rc.requestID}, resp, nil, err)
	rc.attempt.BeginCandidate(&cand)
	rc.attempt.BindProvider(p)
	rc.attempt.BindKey(&model.ProviderKey{})
	result := svc.recordAndSettle(c, rc, admitted{payload: payload}, d,
		resp.StatusCode, time.Now())

	if result != attemptSuccess {
		t.Errorf("result = %v, want attemptSuccess (already committed, cannot fail over)", result)
	}
	if rc.statusCode != 499 {
		t.Errorf("rc.statusCode = %d, want 499 (a caller disconnect must not be billed as an upstream server fault)", rc.statusCode)
	}
	if len(rc.attempts) != 1 {
		t.Fatalf("Attempts = %+v, want exactly one entry", rc.attempts)
	}
	if rc.attempts[0].Outcome != AttemptConnError {
		t.Errorf("attempt outcome = %q, want %q", rc.attempts[0].Outcome, AttemptConnError)
	}
	if !strings.HasPrefix(rc.attempts[0].FailReason, "client_disconnected") {
		t.Errorf("fail_reason = %q, want it to start with %q", rc.attempts[0].FailReason, "client_disconnected")
	}
	// The dead connection must never receive an inline SSE error frame: writing
	// one is what the precheck exists to prevent.
	if client.writes != 0 {
		t.Errorf("expected nothing written to a disconnected client, got %q", client.pending.String())
	}
}

// TestALiveCallerStillGetsTheStreamClosedOffOnAProviderFailure is the negative
// control: a genuine mid-stream upstream failure on a LIVE connection must
// still take the stream_partial branch (inline error frame + AttemptServerError),
// not be swept into the new disconnect precheck.
func TestALiveCallerStillGetsTheStreamClosedOffOnAProviderFailure(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	p := createProvider(t, db, "p-mid-live", "http://upstream.invalid")
	m := createModelAndCandidate(t, db, p, "gpt-4o-mid-live", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	var cand model.ModelCandidate
	if err := db.Where("model_id = ?", m.ID).First(&cand).Error; err != nil {
		t.Fatalf("load seeded candidate: %v", err)
	}

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil) // live context, never canceled
	c.Set(BodiesDirContextKey, t.TempDir())

	rc := &Exchange{requestID: "req-ir-mid-stream-live-error", originalModel: "gpt-4o-mid-live",
		apiKeyID: apiKey.ID, ingress: protocols.ProtocolOpenAI, isStream: true}
	rc.markFirstByteSent()

	err := errors.New("upstream stream read error: connection reset by peer")
	resp := &http.Response{StatusCode: http.StatusOK}

	// The caller is still there, so the stream can be closed off properly.
	client := &stubClient{committed: true, status: http.StatusOK}
	payload := streamingPayload(t, protocols.ProtocolOpenAI, streamCallerBody)
	d := payload.settleStream(DeliveryTools{Client: client, RequestID: rc.requestID}, resp, nil, err)
	rc.attempt.BeginCandidate(&cand)
	rc.attempt.BindProvider(p)
	rc.attempt.BindKey(&model.ProviderKey{})
	result := svc.recordAndSettle(c, rc, admitted{payload: payload}, d,
		resp.StatusCode, time.Now())

	if result != attemptSuccess {
		t.Errorf("result = %v, want attemptSuccess", result)
	}
	if rc.statusCode != http.StatusOK {
		t.Errorf("rc.statusCode = %d, want 200 (stream_partial keeps the already-committed 200)", rc.statusCode)
	}
	if len(rc.attempts) != 1 || rc.attempts[0].Outcome != AttemptServerError {
		t.Errorf("Attempts = %+v, want exactly one AttemptServerError entry", rc.attempts)
	}
	if !strings.HasPrefix(rc.attempts[0].FailReason, "stream_partial") {
		t.Errorf("fail_reason = %q, want it to start with %q", rc.attempts[0].FailReason, "stream_partial")
	}
}
