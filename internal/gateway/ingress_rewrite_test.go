package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/decision"
	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// scriptedIngress is a rewriter a test can aim: it appends its own mark to the
// body, or returns whatever the script says instead.
type scriptedIngress struct {
	name string
	// on is called with the body it was handed and returns what to answer.
	on func(body []byte) ([]byte, error)
	// declines makes Applies answer false, so the kernel should never call on.
	declines bool
}

func (r scriptedIngress) Name() string { return r.name }

func (r scriptedIngress) Applies(struct{}) bool { return !r.declines }

func (r scriptedIngress) RewriteIngress(_ context.Context, _ struct{}, body []byte, _ fact.Sink) ([]byte, error) {
	return r.on(body)
}

func noView(*Exchange) struct{} { return struct{}{} }

// upstreamEchoingBody stands up a server that records the body it was sent, so
// a test can prove what actually left the gateway rather than what the kernel
// believed it was sending.
func upstreamEchoingBody(t *testing.T, sent *[]byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(r.Body)
		*sent = buf.Bytes()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// wireOneCandidate builds the smallest routable setup: one provider, one key,
// one model.
func wireOneCandidate(t *testing.T, svc *Service, db *gorm.DB, upstreamURL string) *model.APIKey {
	t.Helper()
	p := createProvider(t, db, "p1", upstreamURL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	return createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})
}

// Stage order is the whole reason the stage exists. Registration order is
// deliberately the reverse of stage order here, so a runner that simply
// appended would produce the wrong text and this would catch it.
func TestIngressRewritersRunInStageOrder(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sent []byte
	upstream := upstreamEchoingBody(t, &sent)
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	// Each rewriter writes its name immediately before the marker and leaves
	// the marker in place, so the names accumulate left to right in the order
	// they ran and the next rewriter still has something to match on.
	mark := func(name string, at IngressStage) {
		RegisterIngressRewriter(svc, scriptedIngress{name: name, on: func(body []byte) ([]byte, error) {
			return bytes.Replace(body, []byte("@@"), []byte(name+"@@"), 1), nil
		}}, at, noView)
	}
	mark("second", 90)
	mark("first", 80)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"@@"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(sent, []byte("firstsecond@@")) {
		t.Fatalf("upstream saw %s, want the stage-80 rewriter to have run before the stage-90 one", sent)
	}
}

// Two rewriters on the same stage have no defined order between them, so this
// is a wiring mistake that must surface at assembly rather than resolve to
// whichever registration happened to come first.
func TestIngressStageCollisionPanicsAtRegistration(t *testing.T) {
	svc := &Service{}
	pass := scriptedIngress{name: "pass", on: func(b []byte) ([]byte, error) { return b, nil }}
	RegisterIngressRewriter(svc, pass, StageCompress, noView)

	defer func() {
		if recover() == nil {
			t.Fatal("registering a second rewriter on the same stage was accepted")
		}
	}()
	RegisterIngressRewriter(svc, scriptedIngress{name: "other", on: pass.on}, StageCompress, noView)
}

// Returning nil and returning the input unchanged are the same answer: "I have
// nothing to do". Neither may lose the body.
func TestIngressAbstentionKeepsTheBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   func([]byte) ([]byte, error)
	}{
		{"nil", func([]byte) ([]byte, error) { return nil, nil }},
		{"unchanged", func(b []byte) ([]byte, error) { return b, nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			var sent []byte
			upstream := upstreamEchoingBody(t, &sent)
			svc := newSvc(t, db)
			apiKey := wireOneCandidate(t, svc, db, upstream.URL)
			RegisterIngressRewriter(svc, scriptedIngress{name: "abstainer", on: tc.on}, 90, noView)

			body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
			c, w := newCtx(body)
			svc.Handle(c, apiKey)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			if !bytes.Contains(sent, []byte(`"hi"`)) {
				t.Fatalf("upstream saw %s, want the caller's own content", sent)
			}
		})
	}
}

// An error means the body must not be sent. No candidate has been chosen yet,
// so there is nothing to fail over to: the request ends, and — the part worth
// pinning — nothing reaches any upstream.
func TestIngressRefusalEndsTheRequestBeforeAnyUpstream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	dialed := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dialed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)
	RegisterIngressRewriter(svc, scriptedIngress{name: "refuser", on: func([]byte) ([]byte, error) {
		return nil, errors.New("this body must not be sent")
	}}, 90, noView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if dialed {
		t.Error("an upstream was contacted after a rewriter refused the body")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 — the table's terminal status for a refused ingress body", w.Code)
	}
}

// The first rewriter is shown the very bytes the audit row keeps, so the shape
// forbids editing them in place. Nothing can enforce that at run time without
// paying the copy the shape exists to avoid — what CAN be pinned is that the
// kernel hands over the live body rather than quietly reintroducing a copy,
// because a copy here is the regression this contract was chosen over.
func TestIngressRewriterIsHandedTheLiveBodyNotACopy(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sent []byte
	upstream := upstreamEchoingBody(t, &sent)
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	var shown []byte
	RegisterIngressRewriter(svc, scriptedIngress{name: "observer", on: func(b []byte) ([]byte, error) {
		shown = b
		return nil, nil
	}}, 90, noView)

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx(body)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if shown == nil {
		t.Fatal("the rewriter was never called")
	}
	if &shown[0] != &captured.RequestBody()[0] {
		t.Error("the rewriter was handed a copy: on a twenty-megabyte body that copy is the cost this contract was chosen over")
	}
}

// A rewrite that succeeds has to be visible twice over: the upstream receives
// the new bytes, and the audit row keeps both versions. Recording only one of
// them makes a rewrite unprovable after the fact.
func TestIngressRewriteReachesUpstreamAndIsCapturedSeparately(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sent []byte
	upstream := upstreamEchoingBody(t, &sent)
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)
	RegisterIngressRewriter(svc, scriptedIngress{name: "shouter", on: func(body []byte) ([]byte, error) {
		return bytes.Replace(body, []byte(`"hi"`), []byte(`"HI"`), 1), nil
	}}, 90, noView)

	var captured *Exchange
	testHookHandleDone = func(rc *Exchange) { captured = rc }
	defer func() { testHookHandleDone = nil }()

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(sent, []byte(`"HI"`)) {
		t.Fatalf("upstream saw %s, want the rewritten content", sent)
	}
	if captured == nil {
		t.Fatal("testHookHandleDone was never invoked")
	}
	if !bytes.Contains(captured.RequestBody(), []byte(`"hi"`)) {
		t.Errorf("the verbatim capture lost what the caller sent: %s", captured.RequestBody())
	}
	if !bytes.Contains(captured.CompressedRequestBody(), []byte(`"HI"`)) {
		t.Errorf("the rewritten body was not captured: %s", captured.CompressedRequestBody())
	}
}

// A refusal that carried no fixed status must not degrade to the admission
// fallback. Nothing in the table produces such a verdict today, but the two
// call sites share one reader, and a rate limit is the one answer this path
// must never give: it tells the caller to wait for a condition that waiting
// cannot change.
func TestIngressRefusalWithoutAFixedStatusIsAServerFaultNotARateLimit(t *testing.T) {
	status, errType := decision.PreDispatchRejectionResponse(decision.Resolved{},
		http.StatusInternalServerError, errTypeServer)

	if status == http.StatusTooManyRequests {
		t.Fatal("an ingress refusal degraded to 429, telling the caller to retry a gateway fault")
	}
	if status != http.StatusInternalServerError || errType != errTypeServer {
		t.Fatalf("status/errType = %d/%q, want 500/%q", status, errType, errTypeServer)
	}
}

// A rewriter that declines through Applies must not be handed the body at all.
// Production registers every capability regardless of whether the deployment
// switched it on, so this is the ordinary path, and on it the kernel must not
// copy a body that can run to megabytes for a rewriter that will not look at it.
func TestIngressRewriterThatDeclinesIsNeverHandedTheBody(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sent []byte
	upstream := upstreamEchoingBody(t, &sent)
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	called := false
	RegisterIngressRewriter(svc, scriptedIngress{name: "decliner", declines: true, on: func(body []byte) ([]byte, error) {
		called = true
		return body, nil
	}}, 90, noView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("a rewriter that declined through Applies was still handed the body")
	}
}

// The refusal verdict ends the request, so its detail is what the caller reads.
// A rewriter's own error text is written for an operator: it can carry a parser
// message, a fragment of the body, or an internal identifier, and none of that
// belongs in a response.
func TestIngressRefusalDoesNotLeakTheRewritersErrorToTheCaller(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := upstreamEchoingBody(t, new([]byte))
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	const secret = "panic at offset 42 near \"sk-live-INTERNAL\""
	RegisterIngressRewriter(svc, scriptedIngress{name: "leaker", on: func([]byte) ([]byte, error) {
		return nil, errors.New(secret)
	}}, 90, noView)

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "sk-live-INTERNAL") || strings.Contains(w.Body.String(), "offset 42") {
		t.Fatalf("the rewriter's error text reached the caller: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "leaker") {
		t.Errorf("the response names an internal capability: %s", w.Body.String())
	}
}

// The clamp is only worth anything if the clamped body is what actually leaves.
// Its own package tests drive it directly with a stub view; this one proves the
// wiring — that production's assembly reaches it, that it is handed the
// candidate's real limit, and that the number the upstream reads is the clamped
// one.
func TestTheOutputCeilingReachingTheUpstreamIsTheCandidatesLimit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	var sent []byte
	upstream := upstreamEchoingBody(t, &sent)
	svc := newSvc(t, db)
	apiKey := wireOneCandidate(t, svc, db, upstream.URL)

	// The fixture's candidate allows 4096; ask for well over it.
	c, w := newCtx([]byte(`{"model":"gpt-4o","max_tokens":9000,"messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var doc struct {
		MaxTokens int `json:"max_tokens"`
	}
	if err := json.Unmarshal(sent, &doc); err != nil {
		t.Fatalf("upstream body is not JSON: %v (%s)", err, sent)
	}
	if doc.MaxTokens != 4096 {
		t.Fatalf("upstream was asked for %d output tokens, want the candidate's 4096: the request was forwarded over the provider's limit", doc.MaxTokens)
	}
}
