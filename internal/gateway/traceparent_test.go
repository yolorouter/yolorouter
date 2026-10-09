package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolorouter/yolorouter/internal/capability/requestlog"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	ycrypto "github.com/yolorouter/yolorouter/pkg/crypto"
)

// Every parse test drives the exact chain production runs — http.Header into
// SanitizeHeaders into traceIDFromHeaderSnapshot — rather than calling the
// value parser directly, so the matrices cannot drift from what the masked
// snapshot actually preserves. Each case carries its own distinct ids, so a
// value that leaked in from a neighbouring row cannot pass another row.

// TestTraceparentWithinTheGrammarYieldsTheTraceID is the legal matrix: a
// header the W3C version-1 grammar accepts returns its trace-id verbatim.
func TestTraceparentWithinTheGrammarYieldsTheTraceID(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{
			// The spec's own worked example, sampled flag set.
			name:  "w3c spec example, sampled",
			value: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
			want:  "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			// The unsampled flag value is not a special case: flags is any
			// two lowercase hex digits, and 00 must not read as "absent".
			name:  "flags 00, not sampled",
			value: "00-a1b2c3d4e5f60718293a4b5c6d7e8f90-1122334455667788-00",
			want:  "a1b2c3d4e5f60718293a4b5c6d7e8f90",
		},
		{
			// Bits beyond the sampled one exist already; refusing them would
			// silently untrace callers whose SDK sets a future flag.
			name:  "flags ff, every bit set",
			value: "00-0123456789abcdef0123456789abcdef-0102030405060708-ff",
			want:  "0123456789abcdef0123456789abcdef",
		},
		{
			// Only the all-zero parent-id is rejected (it is the format's
			// unset marker); any other value traces, uniform or not.
			name:  "parent-id any nonzero value",
			value: "00-4bf92f3577b34da6a3ce929d0e0e4736-ffffffffffffffff-01",
			want:  "4bf92f3577b34da6a3ce929d0e0e4736",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := SanitizeHeaders(http.Header{"Traceparent": {tc.value}})
			if got := traceIDFromHeaderSnapshot(snapshot); got != tc.want {
				t.Errorf("traceIDFromHeaderSnapshot(SanitizeHeaders(traceparent %q)) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// TestTraceparentOutsideTheGrammarYieldsNoTraceID is the exhaustive invalid
// matrix: every way a header can be absent, empty, or malformed yields the
// empty string — no partial extraction, no panic, and the verdict comes from
// the same snapshot chain production uses.
func TestTraceparentOutsideTheGrammarYieldsNoTraceID(t *testing.T) {
	const (
		tid    = "4bf92f3577b34da6a3ce929d0e0e4736"
		tidUp  = "4BF92F3577B34DA6A3CE929D0E0E4736"
		pid    = "00f067aa0ba902b7"
		pidUp  = "00F067AA0BA902B7"
		zero32 = "00000000000000000000000000000000"
		zero16 = "0000000000000000"
	)
	cases := []struct {
		name string
		// headers, when non-nil, is masked into a snapshot the way Handle
		// entry does it. A nil headers with nil raw is no snapshot at all.
		headers http.Header
		// raw, when non-nil, is the snapshot verbatim, for shapes the
		// masking step itself can never produce.
		raw []byte
	}{
		{name: "no snapshot at all", headers: nil, raw: nil},
		{
			name: "header absent while others are masked",
			headers: http.Header{
				"Content-Type":  {"application/json"},
				"Authorization": {"Bearer sk-live"},
			},
		},
		{name: "empty value", headers: http.Header{"Traceparent": {""}}},
		{name: "three segments", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid}}},
		{name: "five segments", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid + "-01-extra"}}},
		{name: "version not 00", headers: http.Header{"Traceparent": {"01-" + tid + "-" + pid + "-01"}}},
		{name: "uppercase trace-id", headers: http.Header{"Traceparent": {"00-" + tidUp + "-" + pid + "-01"}}},
		{name: "uppercase parent-id", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pidUp + "-01"}}},
		{name: "uppercase flags", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid + "-FF"}}},
		{name: "non-hex character", headers: http.Header{"Traceparent": {"00-g" + tid[1:] + "-" + pid + "-01"}}},
		{name: "trace-id 31 digits", headers: http.Header{"Traceparent": {"00-" + tid[:31] + "-" + pid + "-01"}}},
		{name: "parent-id 15 digits", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid[:15] + "-01"}}},
		{name: "flags 1 digit", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid + "-1"}}},
		{name: "trace-id all zeros", headers: http.Header{"Traceparent": {"00-" + zero32 + "-" + pid + "-01"}}},
		{name: "parent-id all zeros", headers: http.Header{"Traceparent": {"00-" + tid + "-" + zero16 + "-01"}}},
		{name: "leading whitespace", headers: http.Header{"Traceparent": {" 00-" + tid + "-" + pid + "-01"}}},
		{name: "trailing whitespace", headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid + "-01 "}}},
		{
			// Two pins the legal/invalid split above does not reach:
			//
			// A snapshot key the HTTP layer never canonicalized is not
			// looked up case-insensitively — matching is exact, so anything
			// else is simply absent.
			name: "non-canonical snapshot key",
			raw:  []byte(`{"traceparent":["00-` + tid + `-` + pid + `-01"]}`),
		},
		{
			// A repeated header cannot be reconciled into one trace; neither
			// value is preferred, both are refused.
			name:    "repeated header values",
			headers: http.Header{"Traceparent": {"00-" + tid + "-" + pid + "-01", "00-" + tid + "-" + pid + "-00"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := tc.raw
			if snapshot == nil {
				snapshot = SanitizeHeaders(tc.headers) // nil headers -> nil snapshot
			}
			if got := traceIDFromHeaderSnapshot(snapshot); got != "" {
				t.Errorf("traceIDFromHeaderSnapshot() = %q, want the empty string", got)
			}
		})
	}
}

// fireTracingProbeExchange runs one exchange through the kernel exactly the
// way the reject-path settings tests do: a body that fails validation, so no
// upstream or candidate is involved, and the recorder reads the finished
// exchange the way production assembly wires it (RegisterRecorder with the
// exchange itself as the requestlog View). The traceparent header is set on
// the request when the caller was tracing and left unset when it was not —
// those are the only two inputs that differ between the two arms below.
func fireTracingProbeExchange(t *testing.T, traceparent string) *model.RequestLog {
	t.Helper()

	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a body that fails validation")
	}))
	defer upstream.Close()

	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewService(testStoreFrom(db), stubVideoTasks{}, ycrypto.NewSecretBox(masterKey), false, stubSettingsProvider{}, testGatewayConfig(), ProxyResolvers{})
	svc.client.httpClient.Transport = &http.Transport{}
	RegisterRecorder(svc, requestlog.New(db), func(e *Exchange) requestlog.View { return e })

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	// Passes the cheap meta check (non-empty messages, positive max_tokens),
	// fails the full decoder (content is an object, not a string or block
	// array) — a 400 that never reaches a candidate, so the row it leaves is
	// about capture, not about relaying.
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":{"foo":"bar"}}]}`)
	c, w := newCtxPath("/v1/messages", body)
	if traceparent != "" {
		c.Request.Header.Set("traceparent", traceparent)
	}
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the validation 400 unchanged by header tracing; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLog
	if err := db.First(&row).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	return &row
}

// TestTheAuditRowCarriesTheCallerTraceID proves the pass-through the two
// parse matrices stop short of: the value the View reports lands in the
// persisted row's column — present when the caller sent a valid traceparent,
// SQL NULL when it sent none. NULL, not the empty string, is the contract:
// the column is a join key, and only the caller's own tracing writes it.
func TestTheAuditRowCarriesTheCallerTraceID(t *testing.T) {
	t.Run("view provides a trace-id", func(t *testing.T) {
		row := fireTracingProbeExchange(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
		if row.W3CTraceID == nil {
			t.Fatal("request_log.w3c_trace_id is NULL, want the trace-id the caller sent")
		}
		if want := "4bf92f3577b34da6a3ce929d0e0e4736"; *row.W3CTraceID != want {
			t.Errorf("request_log.w3c_trace_id = %q, want %q", *row.W3CTraceID, want)
		}
	})
	t.Run("view provides none", func(t *testing.T) {
		row := fireTracingProbeExchange(t, "")
		if row.W3CTraceID != nil {
			t.Errorf("request_log.w3c_trace_id = %q, want NULL: no traceparent was sent", *row.W3CTraceID)
		}
	})
}
