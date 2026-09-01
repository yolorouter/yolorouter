package gateway

// Tests for the three declarations a payload makes that the kernel honours:
// the upstream call's content type (the builder of a non-JSON body is the
// only one who can know its type), its progressive flag (forwarding as it
// arrives is a property of the response, not of the caller's stream ask),
// and the log policy (what of a request's bodies may be persisted is the
// modality's to say, because only it knows what its bytes are).
//
// Each test swaps a purpose-built modality into the registry for one ingress
// protocol and drives the real Handle path end to end. The assertions are on
// the wire and on the persisted rows, never on kernel internals, so each one
// goes red exactly when its wiring comes undone.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// wiringModality is a stand-in for a modality this kernel does not carry
// yet: a JSON request routed by model name, answered with the upstream's
// bytes as a whole response. What it exists to carry is the one declaration
// under test — a content type, a progressive flag, or a log policy — through
// the real dispatch path.
type wiringModality struct {
	callContentType string
	progressive     bool
	policy          LogPolicy
	sanitizer       func(BodyKind, string, []byte) string
}

func (m *wiringModality) ID() ModalityID         { return ModalityText }
func (m *wiringModality) Limits() TransferLimits { return TransferLimits{} }

func (m *wiringModality) Admit(_ context.Context, in Ingress) (Payload, *Rejection) {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(in.Body, &req); err != nil || req.Model == "" {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: "invalid_request_error",
			Message: "model is required", FailReason: "wiring_bad_body", Fault: fact.FaultClient,
		}
	}
	return &wiringPayload{mod: m, model: req.Model}, nil
}

// wiringPayload is the per-request half. It embeds the shared fake base only
// for the methods no test here is about; everything a test asserts on is
// implemented on this type, so a test cannot pass by inheriting a stub.
type wiringPayload struct {
	fakePayloadBase
	mod   *wiringModality
	model string
}

func (p *wiringPayload) Routing() RoutingIntent { return RoutingIntent{Model: p.model} }

func (p *wiringPayload) PrepareUpstream(Candidate) (*UpstreamCall, error) {
	return &UpstreamCall{
		Path:        "/v1/wiring",
		Body:        []byte(`{"model":"` + p.model + `"}`),
		ContentType: p.mod.callContentType,
		Progressive: p.mod.progressive,
	}, nil
}

// Deliver reads the whole response, reports it to the capture, and writes it
// to the caller the way a real non-negotiating payload would: commit, write,
// flush. The flush is load-bearing on the progressive path — that is the
// moment bytes become honestly recordable as received.
func (p *wiringPayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fact.Undelivered(resp.StatusCode, fact.VerdictNextCandidate, fact.FaultUpstream, "wiring_body_read", err)
	}
	tools.Capture.Upstream(body)
	if err := tools.Client.Commit(resp.StatusCode); err != nil {
		return fact.Undelivered(resp.StatusCode, fact.VerdictSettled, fact.FaultGateway, "wiring_commit", err)
	}
	if _, err := tools.Client.Write(body); err != nil {
		return fact.Undelivered(resp.StatusCode, fact.VerdictSettled, fact.FaultGateway, "wiring_write", err)
	}
	if err := tools.Client.Flush(); err != nil {
		return fact.Undelivered(resp.StatusCode, fact.VerdictSettled, fact.FaultGateway, "wiring_flush", err)
	}
	return fact.Succeeded(resp.StatusCode)
}

func (p *wiringPayload) LogPolicy() LogPolicy { return p.mod.policy }

func (p *wiringPayload) SanitizeForLog(k BodyKind, contentType string, body []byte) string {
	return p.mod.sanitizer(k, contentType, body)
}

// identitySanitizer returns the bytes as they are — the text modality's own
// answer, used where a test is not about rendering.
func identitySanitizer(_ BodyKind, _ string, body []byte) string { return string(body) }

// storeAllBodies admits every body, uncapped — again the text modality's own
// policy, for tests that are not about what is kept.
func storeAllBodies() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    BodyStoredRaw,
		BodyUpstreamRequest:  BodyStoredRaw,
		BodyUpstreamResponse: BodyStoredRaw,
		BodyClientResponse:   BodyStoredRaw,
	}}
}

// withWiringModality registers the wiring modality for the OpenAI ingress
// protocol for the duration of one test. The registry is package state, so
// the swap is restored by cleanup — a leaked swap would hand every later
// test in the package this modality's answers.
func withWiringModality(t *testing.T, m *wiringModality) {
	t.Helper()
	prev := modalities[protocols.ProtocolOpenAI]
	modalities[protocols.ProtocolOpenAI] = m
	t.Cleanup(func() { modalities[protocols.ProtocolOpenAI] = prev })
}

// newWiringRig stands up the dispatch fixture: a database, a fake upstream
// serving whatever the test's handler says, a provider pointed at it, a
// routable model, and a caller key allowed to reach that model.
func newWiringRig(t *testing.T, upstream http.HandlerFunc) (*Service, *model.APIKey) {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	svc := newSvc(t, db)
	p := createProvider(t, db, "wiring-provider", up.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-wiring-up", "wiring-key", 1, true)
	mdl := createModelAndCandidate(t, db, p, "wiring-model", "wiring-real", false, false, 1)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{mdl.ID})
	return svc, key
}

// wiringRequest builds a caller context the way the router middleware would:
// a request id the audit rows can be found by, and a bodies directory the
// stream capture can land in.
func wiringRequest(t *testing.T, requestID, bodiesDir string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := newCtx([]byte(`{"model":"wiring-model"}`))
	c.Set("request_id", requestID)
	c.Set(BodiesDirContextKey, bodiesDir)
	return c, w
}

// The content type a payload stated for the body it built is what the
// upstream must receive — not the egress codec's assumption about what
// protocol-shaped bodies look like. Without the override, a JSON protocol's
// encoder answers "application/json" for a body that is not JSON.
func TestUpstreamCallContentTypeReachesTheWire(t *testing.T) {
	var gotContentType string
	svc, key := newWiringRig(t, func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	withWiringModality(t, &wiringModality{
		callContentType: "image/svg+xml",
		policy:          storeAllBodies(),
		sanitizer:       identitySanitizer,
	})

	c, w := wiringRequest(t, "req-wiring-ct", t.TempDir())
	svc.Handle(c, key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if gotContentType != "image/svg+xml" {
		t.Errorf("upstream saw Content-Type %q, want the payload's %q", gotContentType, "image/svg+xml")
	}
}

// A payload's progressive declaration builds streaming delivery tools even
// when the caller asked for no stream — the observable difference for a
// whole-body write is the stream capture file, which only a progressive
// delivery opens.
func TestUpstreamCallProgressiveBuildsStreamingTools(t *testing.T) {
	svc, key := newWiringRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	withWiringModality(t, &wiringModality{
		callContentType: "application/json",
		progressive:     true,
		policy:          storeAllBodies(),
		sanitizer:       identitySanitizer,
	})

	bodiesDir := t.TempDir()
	c, w := wiringRequest(t, "req-wiring-progressive", bodiesDir)
	svc.Handle(c, key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(bodiesDir, "req-wiring-progressive.stream")); err != nil {
		t.Errorf("no stream capture for a progressive delivery: %v", err)
	}
}

// A policy that keeps nothing persists nothing: every body column of the
// audit row is empty, and the stream capture file a progressive delivery
// would have opened is not on disk — the file is the caller-facing body in
// file form, and the policy keeps none of that body.
func TestLogPolicyDropsEveryBodyItDoesNotAdmit(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	svc := newSvc(t, db)
	p := createProvider(t, db, "wiring-provider", up.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-wiring-up", "wiring-key", 1, true)
	mdl := createModelAndCandidate(t, db, p, "wiring-model", "wiring-real", false, false, 1)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{mdl.ID})

	// The zero policy keeps nothing — deliberately the safe default a
	// modality that forgot to answer gets.
	withWiringModality(t, &wiringModality{
		callContentType: "application/json",
		progressive:     true,
		sanitizer:       identitySanitizer,
	})

	bodiesDir := t.TempDir()
	c, w := wiringRequest(t, "req-wiring-drop", bodiesDir)
	svc.Handle(c, key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLogBody
	if err := db.Where("request_id = ?", "req-wiring-drop").First(&row).Error; err != nil {
		t.Fatalf("no body row for the request: %v", err)
	}
	for name, val := range map[string]string{
		"request_body":           row.RequestBody,
		"upstream_request_body":  row.UpstreamRequestBody,
		"response_body":          row.ResponseBody,
		"upstream_response_body": row.UpstreamResponseBody,
		"stream_body_path":       row.StreamBodyPath,
	} {
		if val != "" {
			t.Errorf("%s = %q, want dropped by the policy", name, val)
		}
	}
	if _, err := os.Stat(filepath.Join(bodiesDir, "req-wiring-drop.stream")); !os.IsNotExist(err) {
		t.Errorf("stream capture file exists despite the policy keeping no client response (stat err: %v)", err)
	}
}

// The stream capture file is opened (or not) by policy at the single choke
// point every opener goes through. A policy that keeps no client response
// must mean the bytes never reach disk — not that they are written and
// deleted afterwards, which leaves a window where a body the modality
// forbade persisting exists as a file.
func TestStreamCaptureOpenedOnlyForPolicyThatKeepsIt(t *testing.T) {
	cases := []struct {
		name   string
		policy LogPolicy
		want   bool
	}{
		{"kept", LogPolicy{Store: map[BodyKind]BodyStorage{BodyClientResponse: BodyStoredRaw}}, true},
		{"not kept", LogPolicy{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			dir := t.TempDir()
			c.Set(BodiesDirContextKey, dir)

			rc := &Exchange{requestID: "req-stream-policy", payloadLog: &payloadLogView{policy: tc.policy}}
			openStreamBodyFile(c, rc)
			defer rc.bodies.CloseStream()

			_, statErr := os.Stat(filepath.Join(dir, "req-stream-policy.stream"))
			if got := statErr == nil; got != tc.want {
				t.Errorf("capture file exists = %v, want %v (stat err: %v)", got, tc.want, statErr)
			}
		})
	}
}

// What a policy admits is rendered through the payload's own sanitizer and
// capped to its byte limit — the persisted row shows what the modality said
// may be kept, in the form it said to keep it in, and no more of it than it
// allowed.
func TestLogPolicyRendersAndCapsWhatItAdmits(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(up.Close)
	svc := newSvc(t, db)
	p := createProvider(t, db, "wiring-provider", up.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-wiring-up", "wiring-key", 1, true)
	mdl := createModelAndCandidate(t, db, p, "wiring-model", "wiring-real", false, false, 1)
	key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{mdl.ID})

	// Everything stored rendered, capped at 16 bytes — the form a modality
	// whose bodies must not be stored as they arrived asks for.
	policy := LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    BodyStoredRendered,
		BodyUpstreamRequest:  BodyStoredRendered,
		BodyUpstreamResponse: BodyStoredRendered,
		BodyClientResponse:   BodyStoredRendered,
	}, MaxBytes: 16}
	withWiringModality(t, &wiringModality{
		callContentType: "application/json",
		policy:          policy,
		sanitizer: func(k BodyKind, _ string, body []byte) string {
			return fmt.Sprintf("<%s %d bytes>", k, len(body))
		},
	})

	c, w := wiringRequest(t, "req-wiring-render", t.TempDir())
	svc.Handle(c, key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var row model.RequestLogBody
	if err := db.Where("request_id = ?", "req-wiring-render").First(&row).Error; err != nil {
		t.Fatalf("no body row for the request: %v", err)
	}
	for name, val := range map[string]string{
		"request_body":           row.RequestBody,
		"upstream_request_body":  row.UpstreamRequestBody,
		"upstream_response_body": row.UpstreamResponseBody,
	} {
		if val == "" {
			t.Errorf("%s is empty, want the payload's rendering", name)
			continue
		}
		if len(val) > 16 {
			t.Errorf("%s = %q (%d bytes), want capped at the policy's 16", name, val, len(val))
		}
	}
	// The exact rendering this body gets — asserted against its first 16
	// bytes, because the rendering is 27 bytes long and the cap is what this
	// test exists to see working.
	want := fmt.Sprintf("<%s %d bytes>", BodyUpstreamResponse, len(`{"ok":true}`))
	if len(want) <= 16 {
		t.Fatalf("test setup: rendering %q must outgrow the 16-byte cap to exercise it", want)
	}
	if row.UpstreamResponseBody != want[:16] {
		t.Errorf("upstream_response_body = %q, want the first 16 bytes of %q", row.UpstreamResponseBody, want)
	}
}
