package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(handler http.HandlerFunc) (*HTTPProviderClient, *httptest.Server) {
	srv := httptest.NewServer(handler)
	c := NewHTTPProviderClient()
	// Swap in a transport that dials the real httptest server directly
	// (bypassing safehttp's loopback denial, which would otherwise block
	// every test here) — production code always uses safehttp.NewTransport();
	// only these unit tests substitute a plain transport to exercise
	// classification logic against a local server. CheckRedirect is kept
	// as NewHTTPProviderClient() set it (not reset to the zero value) so
	// every test in this file — not just the redirect-specific one below —
	// exercises the same non-following behavior production code has.
	c.httpClient = &http.Client{Transport: http.DefaultTransport, CheckRedirect: c.httpClient.CheckRedirect}
	return c, srv
}

func TestTestChatCompletionSuccess(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`))
	})
	defer srv.Close()

	result, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for an explicit null message, got %v", result.Outcome)
	}
}

// TestTestChatCompletionDoesNotFollowRedirects proves the wiring for design
// doc §5 item 5: a server returning a 302 to a second, success-returning
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

	result, err := c.TestChatCompletion(context.Background(), redirectingSrv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, _ := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, _ := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, _ := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, _ := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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
	result, _ := quotaClient.TestChatCompletion(context.Background(), quotaSrv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestQuotaUnavailable {
		t.Fatalf("expected TestQuotaUnavailable, got %v", result.Outcome)
	}

	rateClient, rateSrv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded"}}`))
	})
	defer rateSrv.Close()
	result2, _ := rateClient.TestChatCompletion(context.Background(), rateSrv.URL, "sk-test", "gpt-4o-mini")
	if result2.Outcome != TestRateLimited {
		t.Fatalf("expected TestRateLimited, got %v", result2.Outcome)
	}
}

func TestTestChatCompletion500IsUpstreamError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()
	result, _ := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError, got %v", result.Outcome)
	}
}

func TestTestChatCompletionConnectionRefusedIsUnreachable(t *testing.T) {
	c := NewHTTPProviderClient()
	c.httpClient = &http.Client{Transport: http.DefaultTransport, Timeout: 2 * time.Second}
	result, err := c.TestChatCompletion(context.Background(), "http://127.0.0.1:1", "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestChatCompletion should not return a Go error for connection failures, got: %v", err)
	}
	if result.Outcome != TestUnreachable {
		t.Fatalf("expected TestUnreachable, got %v", result.Outcome)
	}
}

func TestTestChatCompletionOversizedBodyIsUpstreamError(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("a", 70*1024) + `"}}]}`))
	})
	defer srv.Close()
	result, _ := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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
	c := NewHTTPProviderClient()
	// A raw control character in the URL makes net/url.Parse (called inside
	// http.NewRequestWithContext) fail — the one realistic way to force
	// TestChatCompletion's own request-building error branch, as opposed to
	// a network-level failure (which classifies as TestUnreachable instead).
	result, err := c.TestChatCompletion(context.Background(), "http://example.com/\x7f", "sk-test", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected a Go error for a malformed request URL, got result=%+v", result)
	}
}

func TestClassifyResponseDefaultStatusModelNotFoundByCode(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	body := []byte(`{"error":{"message":"nope","code":"model_not_found"}}`)
	result := classifyResponse(resp, body, "gpt-4o-mini", 5)
	if result.Outcome != TestModelNotFound {
		t.Fatalf("expected TestModelNotFound, got %v", result.Outcome)
	}
}

func TestClassifyResponseDefaultStatusModelNotFoundByMessage(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	body := []byte(`{"error":{"message":"the requested model does not exist"}}`)
	result := classifyResponse(resp, body, "gpt-4o-mini", 5)
	if result.Outcome != TestModelNotFound {
		t.Fatalf("expected TestModelNotFound, got %v", result.Outcome)
	}
}

func TestClassifyResponseDefaultStatusFallsBackToUpstreamErrorOnUnrelatedError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	body := []byte(`{"error":{"message":"totally unrelated failure"}}`)
	result := classifyResponse(resp, body, "gpt-4o-mini", 5)
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError, got %v", result.Outcome)
	}
}

func TestClassifyResponseDefaultStatusFallsBackToUpstreamErrorOnUnparsableBody(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	result := classifyResponse(resp, []byte("not json at all"), "gpt-4o-mini", 5)
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for an unparsable body, got %v", result.Outcome)
	}
}

func TestIsValidSuccessBodyRejectsBodyWithTopLevelError(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}}
	body := []byte(`{"error":{"message":"boom"}}`)
	if isValidSuccessBody(resp, body) {
		t.Fatalf("expected a 200 body carrying a top-level error object to be rejected")
	}
}

func TestIsValidSuccessBodyRejectsUnparsableBody(t *testing.T) {
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}}
	if isValidSuccessBody(resp, []byte("not json at all")) {
		t.Fatalf("expected an unparsable body to be rejected")
	}
}

func TestIsModelScopedErrorReturnsFalseOnUnparsableBody(t *testing.T) {
	if isModelScopedError([]byte("not json"), "gpt-4o-mini") {
		t.Fatalf("expected an unparsable body to report false")
	}
}

func TestIsQuotaErrorReturnsFalseOnUnparsableBody(t *testing.T) {
	if isQuotaError([]byte("not json")) {
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
			if got := isModelNotFoundError(c.body); got != c.want {
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
			_, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
			errCh <- err
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the goroutines above acquire their slots
	_, err := c.TestChatCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
	if err == nil {
		t.Fatalf("expected the call over the concurrency cap to be rejected")
	}

	// A max-effort code-review round found errCh was collected but never
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

	result, err := c.TestStreamingCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestSuccess {
		t.Fatalf("expected TestSuccess, got %v", result.Outcome)
	}
}

func TestTestStreamingCompletionRejectsStreamMissingDoneMarker(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("TestStreamingCompletion failed: %v", err)
	}
	if result.Outcome != TestUpstreamError {
		t.Fatalf("expected TestUpstreamError for a stream with no [DONE] marker, got %v", result.Outcome)
	}
}

func TestTestStreamingCompletionClassifiesNonOKStatus(t *testing.T) {
	c, srv := newTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()

	result, err := c.TestStreamingCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestFunctionCalling(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestFunctionCalling(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestFunctionCalling(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestStreamingCompletion(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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

	result, err := c.TestFunctionCalling(context.Background(), srv.URL, "sk-test", "gpt-4o-mini")
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
