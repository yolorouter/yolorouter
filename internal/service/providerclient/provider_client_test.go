package providerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
	"github.com/yolorouter/yolorouter/internal/protocols/claude"
)

func newTestClient(handler http.HandlerFunc) (*HTTPProviderClient, *httptest.Server) {
	srv := httptest.NewServer(handler)
	c := NewHTTPProviderClient(false)
	// Swap in a transport that dials the real httptest server directly
	// (bypassing safehttp's loopback denial, which would otherwise block
	// every test here) — production code always uses safehttp.NewTransport();
	// only these unit tests substitute a plain transport to exercise
	// classification logic against a local server. CheckRedirect is kept
	// as NewHTTPProviderClient(false) set it (not reset to the zero value) so
	// every test in this file — not just the redirect-specific one below —
	// exercises the same non-following behavior production code has.
	c.httpClient = &http.Client{Transport: http.DefaultTransport, CheckRedirect: c.httpClient.CheckRedirect}
	return c, srv
}

// TestProviderTestURLMatchesRuntimeDispatchBuilder is a regression test: the
// verification/key-test URL must be built with the exact same
// protocols.JoinUpstreamURL call a real request is dispatched with,
// otherwise a
// bare-host or path-prefixed provider could pass verification against one
// endpoint and receive production traffic at another (e.g. a bare host
// silently NOT getting /v1 inserted at verification time while runtime
// dispatch does insert it).
func TestProviderTestURLMatchesRuntimeDispatchBuilder(t *testing.T) {
	chatEgressPath := chat.RequestEncoder{}.EgressPath("", false)
	cases := []struct {
		name    string
		baseURL string
	}{
		{"bare host", "https://h"},
		{"v1-suffixed host", "https://h/v1"},
		{"trailing slash", "https://h/"},
		{"path-prefixed host", "https://h/openai"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerTestURL(tc.baseURL, protocols.ProtocolOpenAI, "")
			want := protocols.JoinUpstreamURL(tc.baseURL, chatEgressPath, protocols.ProtocolOpenAI)
			if got != want {
				t.Errorf("providerTestURL(%q) = %q, want %q (must match the runtime dispatch URL builder)", tc.baseURL, got, want)
			}
		})
	}

	// Concrete confirmation of the two shapes the fix specifically targets:
	// a /v1-suffixed base must not get a doubled /v1, and a bare host must
	// get /v1 inserted exactly like runtime dispatch does.
	if got := providerTestURL("https://h/v1", protocols.ProtocolOpenAI, ""); got != "https://h/v1/chat/completions" {
		t.Errorf("providerTestURL(%q) = %q, want no doubled /v1", "https://h/v1", got)
	}
	if got := providerTestURL("https://h", protocols.ProtocolOpenAI, ""); got != "https://h/v1/chat/completions" {
		t.Errorf("providerTestURL(%q) = %q, want /v1 inserted to match runtime dispatch", "https://h", got)
	}
}

// TestProviderTestURLMatchesRuntimeDispatchBuilderForClaude is the same
// regression check as TestProviderTestURLMatchesRuntimeDispatchBuilder,
// but for the anthropic protocol: the reverse-delivery path (openai
// ingress -> anthropic upstream) depends on the credential test hitting
// exactly the same /v1/messages URL shape production dispatch would.
func TestProviderTestURLMatchesRuntimeDispatchBuilderForClaude(t *testing.T) {
	claudeEgressPath := claude.RequestEncoder{}.EgressPath("", false)
	cases := []struct {
		name    string
		baseURL string
	}{
		{"bare host", "https://h"},
		{"v1-suffixed host", "https://h/v1"},
		{"trailing slash", "https://h/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providerTestURL(tc.baseURL, protocols.ProtocolClaude, "")
			want := protocols.JoinUpstreamURL(tc.baseURL, claudeEgressPath, protocols.ProtocolClaude)
			if got != want {
				t.Errorf("providerTestURL(%q, anthropic) = %q, want %q", tc.baseURL, got, want)
			}
		})
	}
	if got := providerTestURL("https://h", protocols.ProtocolClaude, ""); got != "https://h/v1/messages" {
		t.Errorf("providerTestURL(%q, anthropic) = %q, want https://h/v1/messages", "https://h", got)
	}
}

func TestTestChatCompletionSuccess(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
}

func TestTestChatCompletionRejects200WithMissingMessageField(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{}]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for choices[0] with no message field, got %v", result.Outcome)
	}
}

func TestTestChatCompletionRejects200WithNullMessage(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":null}]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for an explicit null message, got %v", result.Outcome)
	}
}

// TestTestChatCompletionDoesNotFollowRedirects proves the wiring for the
// no-redirect rule: a server returning a 302 to a second, success-returning
// server must NOT be transparently followed — the redirect response itself
// (302, no valid success body) is what gets classified, never the target's
// content. Without CheckRedirect set, Go's default client would follow it
// and this test would see TestSuccess instead.
func TestTestChatCompletionDoesNotFollowRedirects(t *testing.T) {
	targetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	}))
	defer targetSrv.Close()

	redirectingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, targetSrv.URL+"/chat/completions", http.StatusFound)
	}))
	defer redirectingSrv.Close()

	c, unusedSrv := newTestClient(func(w http.ResponseWriter, r *http.Request) {})
	unusedSrv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, redirectingSrv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome == TestSuccess {
		t.Fatalf("expected the redirect to NOT be followed to the success-returning target, got TestSuccess")
	}
}

func TestTestChatCompletionRejects200WithNonJSONContentType(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html>captive portal</html>`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a 200 with non-JSON content-type, got %v", result.Outcome)
	}
}

func TestTestChatCompletionRejects200MissingChoices(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"unexpected":"shape"}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a 200 missing choices[0].message, got %v", result.Outcome)
	}
}

func TestTestChatCompletion401IsAuthFailed(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	defer srv.Close()

	result, _ := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestAuthFailed {
		t.Fatalf("expected TestAuthFailed, got %v", result.Outcome)
	}
}

func TestTestChatCompletion403ModelScoped(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"you do not have access to model gpt-4o-mini","param":"model"}}`))
	})
	defer srv.Close()

	result, _ := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestPermissionDenied {
		t.Fatalf("expected TestPermissionDenied, got %v", result.Outcome)
	}
	if !result.IsModelScoped {
		t.Fatalf("expected IsModelScoped=true when error.param=\"model\"")
	}
}

func TestTestChatCompletion403Ambiguous(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"forbidden"}}`))
	})
	defer srv.Close()

	result, _ := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestPermissionDenied {
		t.Fatalf("expected TestPermissionDenied, got %v", result.Outcome)
	}
	if result.IsModelScoped {
		t.Fatalf("expected IsModelScoped=false for an ambiguous 403 with no model reference")
	}
}

func TestTestChatCompletion404IsModelNotFound(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"model not found","code":"model_not_found"}}`))
	})
	defer srv.Close()

	result, _ := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestModelNotFound {
		t.Fatalf("expected TestModelNotFound, got %v", result.Outcome)
	}
}

func TestTestChatCompletion429QuotaVsRateLimit(t *testing.T) {
	quotaClient, quotaSrv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"insufficient quota","code":"insufficient_quota"}}`))
	})
	defer quotaSrv.Close()
	result, _ := quotaClient.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, quotaSrv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestQuotaUnavailable {
		t.Fatalf("expected TestQuotaUnavailable, got %v", result.Outcome)
	}

	rateClient, rateSrv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	})
	defer rateSrv.Close()
	result2, _ := rateClient.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, rateSrv.URL, "sk-test", "gpt-4o-mini")
	if result2.Outcome != TestRateLimited {
		t.Fatalf("expected TestRateLimited, got %v", result2.Outcome)
	}
}

func TestTestChatCompletion500IsUpstreamError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	result, _ := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError, got %v", result.Outcome)
	}
}

func TestTestChatCompletionConnectionRefusedIsUnreachable(t *testing.T) {
	c := NewHTTPProviderClient(false)
	c.httpClient = &http.Client{Transport: http.DefaultTransport, Timeout: 2 * time.Second}
	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, "http://127.0.0.1:1", "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion should not return a Go error for connection failures, got: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("expected TestUnreachable, got %v", result.Outcome)
	}
}

// An expired call budget must classify as TestTimeout, never TestUnreachable.
// The two are diagnostically opposite: unreachable means the address never
// accepted a connection at all, while a timeout means it did — the upstream
// just took longer to answer than the budget allowed. Collapsing both into
// "unreachable" tells an operator whose address is perfectly fine to go check
// their URL spelling and network, which is the one thing that is not wrong.
func TestTestChatCompletionExpiredBudgetIsTimeoutNotUnreachable(t *testing.T) {
	release := make(chan struct{})
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		<-release // hold the response open past the caller's budget
	})
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := c.TestChatCompletion(ctx, protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion should not return a Go error for a timeout, got: %v", err)
	}
	if result.Outcome != TestTimeout {
		t.Fatalf("expected TestTimeout, got %v", result.Outcome)
	}
	if result.Detail == "" {
		t.Fatal("expected a timeout to still carry a diagnostic Detail")
	}
}

// hangingRoundTripper never establishes a connection: it blocks until the
// caller's budget expires. This is the shape of a resolution/dial stall — the
// budget runs out with no connection ever made — which must NOT be reported as
// a timeout, because "the address answered too slowly" is a claim we have no
// evidence for.
type hangingRoundTripper struct{}

func (hangingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func TestTestChatCompletionBudgetExpiryWithoutConnectionIsUnreachable(t *testing.T) {
	c := NewHTTPProviderClient(false)
	c.httpClient = &http.Client{Transport: hangingRoundTripper{}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := c.TestChatCompletion(ctx, protocols.ProtocolOpenAI, "https://example.invalid/v1", "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion should not return a Go error, got: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("a budget expiry with no connection established must stay TestUnreachable, got %v", result.Outcome)
	}
}

// A stall AFTER the response header arrives happens inside the body-reading
// handler, not at http.Client.Do. Without an explicit re-check it settles as
// TestUpstreamError — blaming the upstream's reply for what is really our own
// budget running out, and hiding it from the timeout category entirely.
func TestTestChatCompletionBodyStallAfterHeadersIsTimeout(t *testing.T) {
	release := make(chan struct{})
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush() // headers land; the body never finishes
		<-release
	})
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result, err := c.TestChatCompletion(ctx, protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion should not return a Go error, got: %v", err)
	}
	if result.Outcome != TestTimeout {
		t.Fatalf("a body stall past the budget must be TestTimeout, got %v", result.Outcome)
	}
}

// ListModels has its own transport-error path, so the timeout distinction has
// to be made there too — otherwise a stalled catalogue fetch still sends the
// operator off to check a URL that is demonstrably fine.
func TestListModelsStallIsTimeoutNotUnreachable(t *testing.T) {
	release := make(chan struct{})
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	result, err := c.ListModels(ctx, protocols.ProtocolOpenAI, srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("ListModels should not return a Go error, got: %v", err)
	}
	if result.Outcome != TestTimeout {
		t.Fatalf("a stalled catalogue fetch must be TestTimeout, got %v", result.Outcome)
	}
}

func TestListModelsBudgetExpiryWithoutConnectionIsUnreachable(t *testing.T) {
	c := NewHTTPProviderClient(false)
	c.httpClient = &http.Client{Transport: hangingRoundTripper{}}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	result, err := c.ListModels(ctx, protocols.ProtocolOpenAI, "https://example.invalid/v1", "sk-test")
	if err != nil {
		t.Fatalf("ListModels should not return a Go error, got: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("a budget expiry with no connection established must stay TestUnreachable, got %v", result.Outcome)
	}
}

// The dial bound is a phase-level bound nested INSIDE the call budget, so a
// dial that gives up while the call budget is still healthy is a genuine
// "can't reach it" — it must stay TestUnreachable rather than being swept up
// by the new timeout branch.
func TestTestChatCompletionDialFailureStaysUnreachableWithLiveBudget(t *testing.T) {
	c := NewHTTPProviderClient(false)
	c.httpClient = &http.Client{Transport: http.DefaultTransport, Timeout: 2 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), providerClientTimeout)
	defer cancel()

	result, err := c.TestChatCompletion(ctx, protocols.ProtocolOpenAI, "http://127.0.0.1:1", "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion should not return a Go error for connection failures, got: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("expected TestUnreachable while the call budget is still live, got %v", result.Outcome)
	}
}

func TestTestChatCompletionOversizedBodyIsUpstreamError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("a", 70*1024) + `"}}]}`))
	})
	defer srv.Close()
	result, _ := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for oversized body, got %v", result.Outcome)
	}
}

// TestChatCompletion's json.Marshal error branch is not exercised by any
// test here: the request body is always a fixed map[string]interface{} of
// plain strings and an int (model/apiKey/testModel — all caller-supplied
// strings, never structs, channels, funcs, or cyclic values), which
// encoding/json can always marshal successfully. There is no reachable
// input via this function's public signature that makes json.Marshal fail
// here, so the branch is dead code under the current request-building
// logic.

func TestTestChatCompletionErrorsOnMalformedURL(t *testing.T) {
	c := NewHTTPProviderClient(false)
	// A raw control character in the URL makes net/url.Parse (called inside
	// http.NewRequestWithContext) fail — the one realistic way to force
	// TestChatCompletion's own request-building error branch, as opposed to
	// a network-level failure (which classifies as TestUnreachable instead).
	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, "http://example.com/\x7f", "sk-test", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected a Go error for a malformed request URL, got result=%+v", result)
	}
}

func TestClassifyResponseDefaultStatusModelNotFoundByCode(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	body := []byte(`{"error":{"message":"nope","code":"model_not_found"}}`)
	result := classifyResponse(protocols.ProtocolOpenAI, resp, body, "gpt-4o-mini", 5)
	if result.Outcome != TestModelNotFound {
		t.Fatalf("expected TestModelNotFound, got %v", result.Outcome)
	}
}

func TestClassifyResponseDefaultStatusModelNotFoundByMessage(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	body := []byte(`{"error":{"message":"the requested model does not exist"}}`)
	result := classifyResponse(protocols.ProtocolOpenAI, resp, body, "gpt-4o-mini", 5)
	if result.Outcome != TestModelNotFound {
		t.Fatalf("expected TestModelNotFound, got %v", result.Outcome)
	}
}

func TestClassifyResponseDefaultStatusFallsBackToUpstreamErrorOnUnrelatedError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	body := []byte(`{"error":{"message":"totally unrelated failure"}}`)
	result := classifyResponse(protocols.ProtocolOpenAI, resp, body, "gpt-4o-mini", 5)
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError, got %v", result.Outcome)
	}
}

func TestClassifyResponseDefaultStatusFallsBackToUpstreamErrorOnUnparsableBody(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	result := classifyResponse(protocols.ProtocolOpenAI, resp, []byte("not json at all"), "gpt-4o-mini", 5)
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for an unparsable body, got %v", result.Outcome)
	}
}

func TestIsValidSuccessBodyRejectsBodyWithTopLevelError(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}}
	body := []byte(`{"error":{"message":"boom"}}`)
	if isValidSuccessBody(protocols.ProtocolOpenAI, resp, body) {
		t.Fatalf("expected a 200 body carrying a top-level error object to be rejected")
	}
}

func TestIsValidSuccessBodyRejectsUnparsableBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}}
	if isValidSuccessBody(protocols.ProtocolOpenAI, resp, []byte("not json at all")) {
		t.Fatalf("expected an unparsable body to be rejected")
	}
}

func TestIsModelScopedErrorReturnsFalseOnUnparsableBody(t *testing.T) {
	if isModelScopedError(protocols.ProtocolOpenAI, []byte("not json"), "gpt-4o-mini") {
		t.Fatalf("expected an unparsable body to report false")
	}
}

func TestIsQuotaErrorReturnsFalseOnUnparsableBody(t *testing.T) {
	if isQuotaError(protocols.ProtocolOpenAI, []byte("not json")) {
		t.Fatalf("expected an unparsable body to report false")
	}
}

func TestIsModelNotFoundErrorCoversEveryBranch(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want bool
	}{
		{"unparsable body", []byte("not json"), false},
		{"matches by code", []byte(`{"error":{"message":"nope","code":"model_not_found"}}`), true},
		{"matches by message: not found", []byte(`{"error":{"message":"model foo not found"}}`), true},
		{"matches by message: does not exist", []byte(`{"error":{"message":"model foo does not exist"}}`), true},
		{"mentions model but not missing", []byte(`{"error":{"message":"model foo is deprecated"}}`), false},
		{"unrelated error", []byte(`{"error":{"message":"totally unrelated"}}`), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isModelNotFoundError(protocols.ProtocolOpenAI, c.body); got != c.want {
				t.Fatalf("isModelNotFoundError(%s) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestTestChatCompletionRejectsConcurrencyOverCap(t *testing.T) {
	block := make(chan struct{})
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	// Saturate every slot with in-flight calls that won't return until
	// `block` closes, then confirm one more call over the cap is rejected
	// immediately rather than queueing.
	errCh := make(chan error, providerClientConcurrency)
	for i := 0; i < providerClientConcurrency; i++ {
		go func() {
			_, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
			errCh <- err
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the goroutines above acquire their slots
	_, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected the call over the concurrency cap to be rejected")
	}

	// errCh was collected but never
	// read: the test only ever proved the (providerClientConcurrency+1)th
	// call was rejected, not that all providerClientConcurrency in-flight
	// calls it was supposed to make room for actually succeeded — an
	// off-by-one regression narrowing the real cap would have passed
	// silently. Release the held calls and require every one of them to
	// have succeeded.
	close(block)
	for i := 0; i < providerClientConcurrency; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("expected in-flight call %d (within the concurrency cap) to succeed, got %v", i, err)
		}
	}
}

func TestTestStreamingCompletionAcceptsValidSSEStream(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
}

// TestTestStreamingCompletionAcceptsSpacelessSSEStream: SSE makes the space
// after the colon optional, and an upstream that omits it still streams. A
// probe that dropped those frames would certify a working streaming endpoint
// as broken and clear its streaming capability.
func TestTestStreamingCompletionAcceptsSpacelessSSEStream(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data:{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		flusher.Flush()
		_, _ = fmt.Fprint(w, "data:[DONE]\n\n")
		flusher.Flush()
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess for a spaceless SSE stream, got %v (detail %q)", result.Outcome, result.Detail)
	}
}

func TestTestStreamingCompletionAcceptsStreamWithoutDoneMarker(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	// A content delta proves the stream works; many OpenAI-compatible upstreams
	// (Claude-via-OpenAI bridges, aggregators) omit the [DONE] marker, closing
	// the connection after the last chunk instead.
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess for a stream with content but no [DONE] marker, got %v (detail %q)", result.Outcome, result.Detail)
	}
}

func TestTestStreamingCompletionRejectsStreamWithNoContentDelta(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n") // terminal marker but no content delta
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a stream with no content delta, got %v", result.Outcome)
	}
	if result.Detail == "" {
		t.Fatalf("expected non-empty Detail, got %q", result.Detail)
	}
}

func TestStreamingCompletionPayloadOpenAILimitsTokens(t *testing.T) {
	// The OpenAI streaming probe must cap max_tokens (like the basic probe and
	// the Claude branch) so a large model doesn't generate a full reply just
	// to verify streaming works — keeping the test sub-second.
	p := streamingCompletionPayload(protocols.ProtocolOpenAI, "gpt-4o-mini")
	if p["max_tokens"] != 1 {
		t.Fatalf("openai streaming payload max_tokens = %v, want 1", p["max_tokens"])
	}
	if p["stream"] != true {
		t.Fatalf("openai streaming payload stream = %v, want true", p["stream"])
	}
}

func TestTestStreamingCompletionRejectsRoleOnlyPrelude(t *testing.T) {
	// OpenAI streams open with a role-only delta (a choice carrying role but
	// empty content); that prelude alone must not certify streaming — a
	// broken upstream sending only the prelude then disconnecting should fail.
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a role-only prelude with no content, got %v (detail %q)", result.Outcome, result.Detail)
	}
}

func TestTestStreamingCompletionAcceptsReasoningDelta(t *testing.T) {
	// Reasoning models stream delta.reasoning_content (often with little or no
	// delta.content); that must count as produced content so a working
	// candidate's streaming capability is not cleared.
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess for a reasoning-content stream, got %v (detail %q)", result.Outcome, result.Detail)
	}
}

func TestTestStreamingCompletionRejectsStreamHittingBodyCap(t *testing.T) {
	// A stream exceeding the 64 KiB body cap without terminating must not be
	// certified as streaming-capable — the cap was hit, not a clean close.
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		// Emit well past the 64 KiB cap with no [DONE] / no clean close.
		_, _ = fmt.Fprint(w, strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n", 4000))
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a stream hitting the body cap, got %v (detail %q)", result.Outcome, result.Detail)
	}
}

func TestTestStreamingCompletionClassifiesNonOKStatus(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestAuthFailed {
		t.Fatalf("expected TestAuthFailed for a 401 status, got %v", result.Outcome)
	}
}

func TestTestFunctionCallingAcceptsValidToolCalls(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Beijing\"}"}}]}}]}`)
	})
	defer srv.Close()

	result, err := c.TestFunctionCalling(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestFunctionCalling failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
}

func TestTestFunctionCallingRejectsPlainTextResponse(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"content":"It's sunny."}}]}`)
	})
	defer srv.Close()

	result, err := c.TestFunctionCalling(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestFunctionCalling failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a plain-text response with no tool_calls, got %v", result.Outcome)
	}
}

func TestTestFunctionCallingClassifiesNonOKStatus(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()

	result, err := c.TestFunctionCalling(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestFunctionCalling failed: %v", err)
	}
	if result.Outcome != TestRateLimited {
		t.Fatalf("expected TestRateLimited for a 429 status, got %v", result.Outcome)
	}
}

func TestTestStreamingCompletionReturnsUnreachableOnNetworkError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close() // closed before the call — connection refused

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("expected TestUnreachable for a connection failure, got %v", result.Outcome)
	}
}

func TestTestFunctionCallingReturnsUnreachableOnNetworkError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()

	result, err := c.TestFunctionCalling(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestFunctionCalling failed: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("expected TestUnreachable for a connection failure, got %v", result.Outcome)
	}
}

func TestIsValidToolCallsBodyRejectsEmptyFunctionName(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{}"}}]}}]}`)
	if isValidToolCallsBody(body) {
		t.Fatalf("expected false for a tool call with an empty function name")
	}
}

func TestIsValidToolCallsBodyRejectsUnparseableArguments(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"not json"}}]}}]}`)
	if isValidToolCallsBody(body) {
		t.Fatalf("expected false for a tool call with unparseable JSON arguments")
	}
}

func TestIsValidToolCallsBodyRejectsEmptyToolCalls(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"tool_calls":[]}}]}`)
	if isValidToolCallsBody(body) {
		t.Fatalf("expected false for an empty tool_calls array")
	}
}

func TestIsValidToolCallsBodyRejectsMalformedJSON(t *testing.T) {
	if isValidToolCallsBody([]byte(`not json`)) {
		t.Fatalf("expected false for malformed JSON")
	}
}

// --- Anthropic (Claude) protocol-aware credential test coverage ---

// TestTestChatCompletionClaudeHitsMessagesEndpointWithAnthropicAuth proves
// the protocol-aware request shape: an anthropic-typed provider's
// credential test must hit /v1/messages with x-api-key +
// anthropic-version, and must NOT carry an Authorization header — using
// OpenAI's Bearer-token header against an Anthropic endpoint would fail
// authentication even with a correct key.
func TestTestChatCompletionClaudeHitsMessagesEndpointWithAnthropicAuth(t *testing.T) {
	var gotPath, gotAPIKeyHeader, gotVersionHeader, gotAuthHeader string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		gotVersionHeader = r.Header.Get("anthropic-version")
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
	if !strings.HasSuffix(gotPath, "/messages") {
		t.Fatalf("expected request path to end with /messages, got %q", gotPath)
	}
	if gotAPIKeyHeader != "sk-ant-test" {
		t.Fatalf("expected x-api-key header %q, got %q", "sk-ant-test", gotAPIKeyHeader)
	}
	if gotVersionHeader == "" {
		t.Fatalf("expected a non-empty anthropic-version header")
	}
	if gotAuthHeader != "" {
		t.Fatalf("expected NO Authorization header for an anthropic request, got %q", gotAuthHeader)
	}
}

func TestTestChatCompletionClaude401IsAuthFailed(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-bad", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestAuthFailed {
		t.Fatalf("expected TestAuthFailed, got %v", result.Outcome)
	}
}

func TestTestChatCompletionClaude429WithQuotaMessageIsQuotaUnavailable(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"you have exceeded your quota"}}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestQuotaUnavailable {
		t.Fatalf("expected TestQuotaUnavailable for a 429 whose error message mentions quota, got %v", result.Outcome)
	}
}

func TestTestChatCompletionClaude429WithoutQuotaMessageIsRateLimited(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"too many requests, slow down"}}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestRateLimited {
		t.Fatalf("expected TestRateLimited for a plain rate_limit_error, got %v", result.Outcome)
	}
}

func TestTestChatCompletionClaudeRejectsErrorTypeBodyOn200(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a 200 carrying type:\"error\", got %v", result.Outcome)
	}
}

func TestTestChatCompletionClaudeRejectsEmptyContentOn200(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a message body with empty content, got %v", result.Outcome)
	}
}

// TestTestChatCompletionClaudePayloadCarriesMaxTokens proves the
// anthropic-shaped request body includes max_tokens — Claude's Messages API
// rejects a request without it, unlike OpenAI where it's optional.
func TestTestChatCompletionClaudePayloadCarriesMaxTokens(t *testing.T) {
	var gotBody map[string]interface{}
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]}`))
	})
	defer srv.Close()

	if _, err := c.TestChatCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet"); err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Fatalf("expected the anthropic request body to carry max_tokens, got %+v", gotBody)
	}
	if gotBody["model"] != "claude-3-5-sonnet" {
		t.Fatalf("expected model %q in the request body, got %+v", "claude-3-5-sonnet", gotBody["model"])
	}
}

// TestTestChatCompletionOpenAINonRegression re-confirms, after threading
// proto through every call site, that an openai-typed provider's
// credential test still hits /chat/completions with a Bearer token — the
// exact request shape production traffic for an openai provider uses.
func TestTestChatCompletionOpenAINonRegression(t *testing.T) {
	var gotPath, gotAuthHeader string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Fatalf("expected request path to end with /chat/completions, got %q", gotPath)
	}
	if gotAuthHeader != "Bearer sk-test" {
		t.Fatalf("expected Authorization: Bearer sk-test, got %q", gotAuthHeader)
	}

	// 401 still classifies exactly as before proto-awareness was added.
	c401, srv401 := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	defer srv401.Close()
	result401, err := c401.TestChatCompletion(context.Background(), protocols.ProtocolOpenAI, srv401.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result401.Outcome != TestAuthFailed {
		t.Fatalf("expected TestAuthFailed, got %v", result401.Outcome)
	}
}

// --- Gemini/Responses 2xx must never falsely certify (Finding 2) ---

// TestTestChatCompletionGeminiSuccessBodyIsVerificationUnsupported is the
// direct regression test for Finding 2: gemini has no real success-body
// validator yet, so a 200 with a plausible-looking body (even an empty
// object) must NOT classify as TestSuccess — that would falsely certify a
// credential/destination pair that was never actually verified.
func TestTestChatCompletionGeminiSuccessBodyIsVerificationUnsupported(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolGemini, srv.URL, "sk-test", "gemini-1.5-flash")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestVerificationUnsupported {
		t.Fatalf("expected TestVerificationUnsupported for a gemini 200, got %v", result.Outcome)
	}
}

// TestTestChatCompletionResponsesSuccessBodyIsVerificationUnsupported mirrors
// the gemini case above for the responses protocol, with an unrelated but
// still-parseable 200 JSON body.
func TestTestChatCompletionResponsesSuccessBodyIsVerificationUnsupported(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_123","object":"response"}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolResponses, srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestVerificationUnsupported {
		t.Fatalf("expected TestVerificationUnsupported for a responses 200, got %v", result.Outcome)
	}
}

// TestTestChatCompletionGemini401StillAuthFailed proves real error statuses
// stay meaningful for gemini/responses even though a 2xx can no longer
// certify success — only the success-body path is affected by Finding 2's
// fix.
func TestTestChatCompletionGemini401StillAuthFailed(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), protocols.ProtocolGemini, srv.URL, "sk-test", "gemini-1.5-flash")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestAuthFailed {
		t.Fatalf("expected TestAuthFailed for a gemini 401, got %v", result.Outcome)
	}
}

func TestClassifyResponseGeminiAndResponses200NeverTestSuccess(t *testing.T) {
	for _, proto := range []protocols.ProtocolID{protocols.ProtocolGemini, protocols.ProtocolResponses} {
		resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}}
		result := classifyResponse(proto, resp, []byte(`{}`), "some-model", 5)
		if result.Outcome != TestVerificationUnsupported {
			t.Fatalf("classifyResponse(%s, 200) = %v, want TestVerificationUnsupported", proto, result.Outcome)
		}
	}
}

func TestTestStreamingCompletionClaudeSuccessOnParseableBody(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess for a non-empty 200 stream body (anthropic body classification is deferred), got %v", result.Outcome)
	}
}

func TestTestFunctionCallingClaudeSuccessOnParseableBody(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"location":"Beijing"}}]}`)
	})
	defer srv.Close()

	result, err := c.TestFunctionCalling(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-ant-test", "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("TestFunctionCalling failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess for a non-error 200 body (anthropic tool_use body classification is deferred), got %v", result.Outcome)
	}
}

func TestListModelsOpenAISuccess(t *testing.T) {
	var gotPath string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"deepseek-chat"},{"id":"deepseek-reasoner"}]}`))
	})
	defer srv.Close()

	result, err := c.ListModels(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v (detail %q)", result.Outcome, result.Detail)
	}
	if len(result.Models) != 2 || result.Models[0] != "deepseek-chat" || result.Models[1] != "deepseek-reasoner" {
		t.Fatalf("expected [deepseek-chat deepseek-reasoner], got %v", result.Models)
	}
	// A base URL without a version segment must resolve to /v1/models.
	if gotPath != "/v1/models" {
		t.Fatalf("expected GET /v1/models, got %q", gotPath)
	}
}

func TestListModelsGeminiStripsModelsPrefix(t *testing.T) {
	var gotPath string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-3.5-flash"},{"name":"models/gemini-3.5-pro"}]}`))
	})
	defer srv.Close()

	result, err := c.ListModels(context.Background(), protocols.ProtocolGemini, srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Models) != 2 || result.Models[0] != "gemini-3.5-flash" {
		t.Fatalf("expected the models/ prefix stripped, got %v", result.Models)
	}
	// Gemini catalogues are served under /v1beta.
	if gotPath != "/v1beta/models" {
		t.Fatalf("expected GET /v1beta/models, got %q", gotPath)
	}
}

func TestListModels401ReturnsAuthFailedWithDetail(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	})
	defer srv.Close()

	result, _ := c.ListModels(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test")
	if result.Outcome != TestAuthFailed {
		t.Fatalf("expected TestAuthFailed, got %v", result.Outcome)
	}
	if !strings.Contains(result.Detail, "HTTP 401") || !strings.Contains(result.Detail, "invalid api key") {
		t.Fatalf("expected detail to carry status and upstream message, got %q", result.Detail)
	}
	if len(result.Models) != 0 {
		t.Fatalf("expected no models on failure, got %v", result.Models)
	}
}

func TestListModelsUnreachableOnNetworkError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close() // close before the call so the dial fails

	result, _ := c.ListModels(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test")
	if result.Outcome != TestUnreachable {
		t.Fatalf("expected TestUnreachable, got %v", result.Outcome)
	}
}

func TestListModels404ReturnsEmptyCatalogue(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"not found"}}`))
	})
	defer srv.Close()

	// A 404 on the catalogue endpoint means "no /models here", not "model not
	// found": report an empty but successful catalogue so the UI falls back to
	// manual entry instead of showing a misleading model-not-found error.
	result, _ := c.ListModels(context.Background(), protocols.ProtocolOpenAI, srv.URL, "sk-test")
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess (empty catalogue) for a 404, got %v (detail %q)", result.Outcome, result.Detail)
	}
	if len(result.Models) != 0 {
		t.Fatalf("expected no models for a 404 catalogue, got %v", result.Models)
	}
}

func TestListModelsFollowsDataPagination(t *testing.T) {
	var pages []string
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("after_id") == "m2" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m3"}],"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}],"has_more":true,"last_id":"m2"}`))
	})
	defer srv.Close()

	result, err := c.ListModels(context.Background(), protocols.ProtocolClaude, srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Models) != 3 || result.Models[0] != "m1" || result.Models[2] != "m3" {
		t.Fatalf("expected the paginated pages merged to [m1 m2 m3], got %v", result.Models)
	}
	if len(pages) != 2 || pages[0] != "" || pages[1] != "after_id=m2" {
		t.Fatalf("expected page 1 unqualified then page 2 with after_id=m2, got %v", pages)
	}
}

func TestListModelsFollowsGeminiPagination(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Query().Get("pageToken") == "tok" {
			_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-a"}],"nextPageToken":"tok"}`))
	})
	defer srv.Close()

	result, _ := c.ListModels(context.Background(), protocols.ProtocolGemini, srv.URL, "sk-test")
	if len(result.Models) != 2 || result.Models[0] != "gemini-a" || result.Models[1] != "gemini-b" {
		t.Fatalf("expected [gemini-a gemini-b] across two pages, got %v", result.Models)
	}
}

// TestTruncateRuneSafeBacksOffPartialRune pins the rune-boundary backoff:
// a cut that lands inside a multi-byte rune must retreat to the previous
// boundary so upstream error details never end in a broken sequence.
func TestTruncateRuneSafeBacksOffPartialRune(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"under limit unchanged", "héllo", 100, "héllo"},
		{"cut mid-rune backs off", "aé", 2, "a"}, // é is 2 bytes; cutting at 2 splits it
		{"cut on boundary keeps rune", "aé", 3, "aé"},
		{"multibyte CJK backs off", "日本", 4, "日"}, // each rune is 3 bytes
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateRuneSafe(c.in, c.maxBytes)
			if got != c.want {
				t.Fatalf("truncateRuneSafe(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result %q is not valid UTF-8", got)
			}
		})
	}
}
