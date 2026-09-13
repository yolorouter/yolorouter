package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestIsClientDisconnected pins isClientDisconnected's classification
// contract directly: it must report true only when the CALLER's own request
// context (c.Request.Context()) is context.Canceled, and false for a
// deadline-derived context (context.DeadlineExceeded) or a live context —
// the exact distinction this rests on (a client disconnect must
// settle as 499, while the gateway's own request-budget expiry must not be
// misclassified as one).
func TestIsClientDisconnected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtxWith := func(reqCtx context.Context) *gin.Context {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(reqCtx)
		c.Request = req
		return c
	}

	t.Run("canceled request context is a disconnect", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if !isClientDisconnected(newCtxWith(ctx)) {
			t.Error("expected a canceled request context to be classified as client-disconnected")
		}
	})

	t.Run("deadline-exceeded request context is NOT a disconnect", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 0)
		defer cancel()
		<-ctx.Done() // force DeadlineExceeded, not Canceled
		if isClientDisconnected(newCtxWith(ctx)) {
			t.Error("a DeadlineExceeded request context must NOT be classified as client-disconnected (that's the gateway's own budget, not a client hangup)")
		}
	})

	t.Run("live request context is not a disconnect", func(t *testing.T) {
		if isClientDisconnected(newCtxWith(context.Background())) {
			t.Error("a live (non-canceled) request context must not be classified as client-disconnected")
		}
	})
}

// cancelOnReadBody wraps a request body reader and cancels the associated
// context the first time Read is called, AFTER the underlying bytes are
// copied — so io.ReadAll (Handle's body read) still returns a nil error (the
// existing body-read disconnect check only fires on a non-nil ReadAll error,
// so it is never triggered here), while every context derived from the
// request afterward (rc.requestCtx, and any db.WithContext(rc.requestCtx)
// query issued once Handle proceeds past the body read) observes an
// already-canceled context — modeling a client that hangs up the instant
// after its request finished uploading.
type cancelOnReadBody struct {
	r      *bytes.Reader
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelOnReadBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if !c.fired {
		c.fired = true
		c.cancel()
	}
	return n, err
}

func (c *cancelOnReadBody) Close() error { return nil }

// newCtxDisconnectAfterBodyRead builds a gin context around
// cancelOnReadBody, so a full Handle() call reads the caller's request body
// successfully but every DB query issued afterward sees an already-canceled
// request context — a controlled, non-racy way to reach a canceled context
// exactly at a query point without depending on real timing.
func newCtxDisconnectAfterBodyRead(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &cancelOnReadBody{r: bytes.NewReader(body), cancel: cancel})
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, w
}

// TestHandleFindModelDBCanceledContextReturns499 covers the first
// s.db.WithContext(requestCtx) query in Handle (repository.FindModelByName):
// when the caller has disconnected, database/sql sees the already-canceled
// context and returns context.Canceled from the query BEFORE it ever
// reaches the driver. Classified naively, this lands in the generic
// "internal error" branch and was logged as a 500 db_model fault; it must
// instead settle as 499 client_disconnected, matching the disconnect
// handling already used around the body read and the upstream send.
func TestHandleFindModelDBCanceledContextReturns499(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, nil)

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtxDisconnectAfterBodyRead([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if captured.statusCode != 499 {
		t.Errorf("rc.statusCode = %d, want 499", captured.statusCode)
	}
	if w.Body.Len() != 0 {
		t.Errorf("expected nothing written to a disconnected client, got: %s", w.Body.String())
	}

	row, err := repository.GetRequestLogByRequestID(db, captured.requestID)
	if err != nil {
		t.Fatalf("GetRequestLogByRequestID: %v", err)
	}
	if row == nil {
		t.Fatal("expected a request_logs row")
	}
	if row.StatusCode != 499 {
		t.Errorf("persisted StatusCode = %d, want 499", row.StatusCode)
	}
	if row.FailReason == nil || *row.FailReason != "client_disconnected" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("persisted FailReason = %q, want %q", got, "client_disconnected")
	}
}

// TestHandleFindModelDBRealErrorStays500 is the negative-control pin for the
// case above: a genuine DB fault (not a canceled context) at the exact
// same FindModelByName query point must still settle as a 500 db_model
// failure, not be swept up into the new disconnect branch. Closing the
// underlying sqlite connection produces a real driver error
// ("sql: database is closed"), which is neither context.Canceled nor
// gorm.ErrRecordNotFound.
func TestHandleFindModelDBRealErrorStays500(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, nil)

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close sqlite connection: %v", err)
	}

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if captured.statusCode != http.StatusInternalServerError {
		t.Errorf("rc.statusCode = %d, want %d (a real DB fault must not be misclassified as a disconnect)", captured.statusCode, http.StatusInternalServerError)
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("w.Code = %d, want %d", w.Code, http.StatusInternalServerError)
	}
}

// TestRelayCandidatesProviderKeyLoadCanceledContextReturns499 covers the
// candidate-loop provider-key-load query (repository.ListProviderKeysByProvider,
// via s.db.WithContext(rc.requestCtx) in relayCandidates): unguarded,
// a client disconnect here was indistinguishable from a genuine load
// failure, so the candidate loop kept walking the (now-pointless) remaining
// candidates and eventually settled as a 502 all_candidates_failed instead
// of the correct 499. relayCandidates is called directly (unexported, same
// package) with an already-canceled RequestCtx/request context, so the
// query fails deterministically without needing real timing.
func TestRelayCandidatesProviderKeyLoadCanceledContextReturns499(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := newSvc(t, db)
	p := createProvider(t, db, "p1", "http://upstream.invalid")
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	var cand ModelCandidate
	if err := db.Where("model_id = ?", m.ID).First(&cand).Error; err != nil {
		t.Fatalf("load seeded candidate: %v", err)
	}
	cand.Provider = p

	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone before relayCandidates ever runs
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)

	rc := &Exchange{
		requestID:       "test-req-id",
		requestCtx:      ctx,
		requestDeadline: time.Now().Add(time.Hour),
		originalModel:   "gpt-4o",
		apiKeyID:        apiKey.ID,
	}

	// The chain now carries the admitted payload; a caller who left before it
	// starts is the same either way, but the loop cannot run without one.
	payload, rej := NewTextModality().Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/chat/completions",
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid body: %+v", rej)
	}
	svc.relayCandidates(c, rc, admitted{payload: newOrderedPayload(payload, rc.requestID)}, []ModelCandidate{cand}, time.Now())
	// relayCandidates settles the exchange; Handle is what records it, so a
	// test that calls the inner function has to do the same.
	svc.recordTerminal(rc)

	if rc.statusCode != 499 {
		t.Errorf("rc.statusCode = %d, want 499", rc.statusCode)
	}
	if len(rc.attempts) != 1 || rc.attempts[0].Outcome != AttemptConnError {
		t.Errorf("rc.attempts = %+v, want exactly one AttemptConnError entry", rc.attempts)
	}

	row, err := repository.GetRequestLogByRequestID(db, rc.requestID)
	if err != nil {
		t.Fatalf("GetRequestLogByRequestID: %v", err)
	}
	if row == nil {
		t.Fatal("expected a request_logs row")
	}
	if row.FailReason == nil || *row.FailReason != "client_disconnected" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("persisted FailReason = %q, want %q", got, "client_disconnected")
	}
}
