package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/capability/requestlog"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// probeProvider is a SettingsProvider whose every method either returns a
// fixed triple or reports that it was reached. The resolve function is what
// decides WHETHER a global read happens at all — the per-key overrides are
// supposed to short-circuit it — so the probe's job is to make "the global
// read was skipped" observable, which no value assertion can.
type probeProvider struct {
	compress func() (bool, error)
	prompt   func() (settings.CustomSystemPromptSetting, error)
	vision   func() (settings.VisionFallbackSetting, error)
}

func (p *probeProvider) CustomSystemPrompt(context.Context) (settings.CustomSystemPromptSetting, int64, error) {
	v, err := p.prompt()
	return v, 1, err
}

func (p *probeProvider) GetInputCompression(context.Context) (bool, int64, error) {
	v, err := p.compress()
	return v, 1, err
}

func (p *probeProvider) GetVisionFallback(context.Context) (settings.VisionFallbackSetting, int64, error) {
	v, err := p.vision()
	return v, 1, err
}

// quietProbe never errors and never fails; the default fixture for rows that
// only care about one setting.
func quietProbe() *probeProvider {
	return &probeProvider{
		compress: func() (bool, error) { return false, nil },
		prompt:   func() (settings.CustomSystemPromptSetting, error) { return settings.CustomSystemPromptSetting{}, nil },
		vision:   func() (settings.VisionFallbackSetting, error) { return settings.VisionFallbackSetting{}, nil },
	}
}

// TestResolveSettingsOverrideShortCircuitsTheGlobalRead pins the reason the
// override branch exists: a stalled settings read can never block an
// override key, because the global read never runs. A value assertion
// cannot see this — "override wins" and "global read then override wins"
// produce the same value — so the probe fails the test the moment its
// global method is reached with an override key.
func TestResolveSettingsOverrideShortCircuitsTheGlobalRead(t *testing.T) {
	probed := quietProbe()
	probed.compress = func() (bool, error) {
		t.Fatal("GetInputCompression ran under a compression override key — the override must short-circuit the read")
		return false, nil
	}
	probed.prompt = func() (settings.CustomSystemPromptSetting, error) {
		t.Fatal("CustomSystemPrompt ran under a prompt override key — the override must short-circuit the read")
		return settings.CustomSystemPromptSetting{}, nil
	}

	key := &model.APIKey{
		CompressEnabledOverride:           true,
		CompressEnabled:                   true,
		CustomSystemPromptEnabledOverride: true,
		CustomSystemPromptEnabled:         true,
		CustomSystemPrompt:                "override prompt",
	}
	got := resolveRequestSettings(context.Background(), probed, key, "req-1")
	if !got.CompressEnabled || !got.CustomSystemPromptEnabled || got.CustomSystemPrompt != "override prompt" {
		t.Fatalf("override values not applied: %+v", got)
	}
}

// TestResolveSettingsKeepsLastKnownGoodOnReadError pins the fail-open
// posture for all three settings: the provider delivers its snapshot
// alongside the error, and the resolve keeps it. Flipping any of the three
// to "error means disabled" turns this red.
func TestResolveSettingsKeepsLastKnownGoodOnReadError(t *testing.T) {
	probed := quietProbe()
	probed.compress = func() (bool, error) { return true, errors.New("db down") }
	probed.prompt = func() (settings.CustomSystemPromptSetting, error) {
		return settings.CustomSystemPromptSetting{Enabled: true, Text: "cached"}, errors.New("db down")
	}
	probed.vision = func() (settings.VisionFallbackSetting, error) {
		return settings.VisionFallbackSetting{Model: "m1", Prompt: "p1"}, errors.New("db down")
	}

	got := resolveRequestSettings(context.Background(), probed, &model.APIKey{}, "req-1")
	if !got.CompressEnabled {
		t.Error("compress: last-known-good (true) dropped alongside the error")
	}
	if !got.CustomSystemPromptEnabled || got.CustomSystemPrompt != "cached" {
		t.Error("prompt: last-known-good dropped alongside the error")
	}
	if got.VisionFallbackModel != "m1" || got.VisionFallbackPrompt != "p1" {
		t.Error("vision fallback: last-known-good dropped alongside the error")
	}
}

// TestResolveSettingsColdCacheYieldsZeroOnReadError separates the two
// fail-open shapes: on a cold cache the provider has no snapshot, so the
// zero value IS what "keep what the provider returned" means. Without this
// row the test above could be misread as "errors always keep something".
func TestResolveSettingsColdCacheYieldsZeroOnReadError(t *testing.T) {
	probed := quietProbe()
	probed.compress = func() (bool, error) { return false, errors.New("db down") }
	probed.prompt = func() (settings.CustomSystemPromptSetting, error) {
		return settings.CustomSystemPromptSetting{}, errors.New("db down")
	}
	probed.vision = func() (settings.VisionFallbackSetting, error) {
		return settings.VisionFallbackSetting{}, errors.New("db down")
	}

	got := resolveRequestSettings(context.Background(), probed, &model.APIKey{}, "req-1")
	if got.CompressEnabled || got.CustomSystemPromptEnabled || got.CustomSystemPrompt != "" ||
		got.VisionFallbackModel != "" || got.VisionFallbackPrompt != "" {
		t.Fatalf("cold cache with error should yield the zero settings, got %+v", got)
	}
}

// TestSettingsReadErrorDoesNotBlockTheRequest pins the other half of the
// cold-cache row end to end: a provider that errors on every read (with
// nothing cached to fall back on) must not fail the request — it dispatches
// and the caller gets the upstream's answer.
func TestSettingsReadErrorDoesNotBlockTheRequest(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	failing := &probeProvider{
		compress: func() (bool, error) { return false, errors.New("db down") },
		prompt: func() (settings.CustomSystemPromptSetting, error) {
			return settings.CustomSystemPromptSetting{}, errors.New("db down")
		},
		vision: func() (settings.VisionFallbackSetting, error) {
			return settings.VisionFallbackSetting{}, errors.New("db down")
		},
	}
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewService(db, masterKey, false, failing, testGatewayConfig())
	svc.client.httpClient.Transport = &http.Transport{}

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a settings read error must never block the request; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hi there") {
		t.Fatalf("upstream answer not delivered; body = %s", w.Body.String())
	}
}

// TestResolveSettingsNilProviderKeepsOverridesAndZeroesTheRest pins the nil
// guard: a service built without a settings provider still serves override
// keys and never panics; only the global branches go quiet.
func TestResolveSettingsNilProviderKeepsOverridesAndZeroesTheRest(t *testing.T) {
	key := &model.APIKey{
		CompressEnabledOverride:           true,
		CompressEnabled:                   true,
		CustomSystemPromptEnabledOverride: true,
		CustomSystemPromptEnabled:         true,
		CustomSystemPrompt:                "override prompt",
	}
	got := resolveRequestSettings(context.Background(), nil, key, "req-1")
	if !got.CompressEnabled || !got.CustomSystemPromptEnabled || got.CustomSystemPrompt != "override prompt" {
		t.Fatalf("overrides must apply with a nil provider, got %+v", got)
	}
	if got.VisionFallbackModel != "" || got.VisionFallbackPrompt != "" {
		t.Errorf("vision fallback has no override layer; a nil provider must leave it zero, got %+v", got)
	}

	zero := resolveRequestSettings(context.Background(), nil, &model.APIKey{}, "req-1")
	if zero != (requestSettings{}) {
		t.Fatalf("nil provider with no overrides should yield the zero settings, got %+v", zero)
	}
}

// TestResolveSettingsCombinationGrid walks override × global for the two
// settings that have a per-key layer, and asserts the third (vision
// fallback) ignores the key entirely — it has no override layer by design.
func TestResolveSettingsCombinationGrid(t *testing.T) {
	cases := []struct {
		name          string
		keyOverride   bool
		keyValue      bool
		globalValue   bool
		wantCompress  bool
		wantFromLayer string // "key" or "global"
	}{
		{"override on, key true, global false", true, true, false, true, "key"},
		{"override on, key false, global true", true, false, true, false, "key"},
		{"override off, global true", false, false, true, true, "global"},
		{"override off, global false", false, false, false, false, "global"},
	}
	for _, tc := range cases {
		t.Run("compress: "+tc.name, func(t *testing.T) {
			probed := quietProbe()
			probed.compress = func() (bool, error) { return tc.globalValue, nil }
			key := &model.APIKey{CompressEnabledOverride: tc.keyOverride, CompressEnabled: tc.keyValue}
			got := resolveRequestSettings(context.Background(), probed, key, "req-1")
			if got.CompressEnabled != tc.wantCompress {
				t.Fatalf("CompressEnabled = %v, want %v (winner: %s)", got.CompressEnabled, tc.wantCompress, tc.wantFromLayer)
			}
		})
	}

	promptCases := []struct {
		name        string
		keyOverride bool
		keyEnabled  bool
		keyText     string
		globalOn    bool
		wantOn      bool
		wantText    string
	}{
		{"override on wins over global", true, true, "from-key", true, true, "from-key"},
		{"override on, key disabled, global on stays out", true, false, "from-key", true, false, "from-key"},
		{"override off inherits global", false, true, "from-key", true, true, "from-global"},
		{"override off, global off", false, true, "from-key", false, false, "from-global"},
	}
	for _, tc := range promptCases {
		t.Run("prompt: "+tc.name, func(t *testing.T) {
			probed := quietProbe()
			probed.prompt = func() (settings.CustomSystemPromptSetting, error) {
				return settings.CustomSystemPromptSetting{Enabled: tc.globalOn, Text: "from-global"}, nil
			}
			key := &model.APIKey{
				CustomSystemPromptEnabledOverride: tc.keyOverride,
				CustomSystemPromptEnabled:         tc.keyEnabled,
				CustomSystemPrompt:                tc.keyText,
			}
			got := resolveRequestSettings(context.Background(), probed, key, "req-1")
			if got.CustomSystemPromptEnabled != tc.wantOn || got.CustomSystemPrompt != tc.wantText {
				t.Fatalf("prompt = (%v, %q), want (%v, %q)",
					got.CustomSystemPromptEnabled, got.CustomSystemPrompt, tc.wantOn, tc.wantText)
			}
		})
	}

	t.Run("vision fallback ignores the key entirely", func(t *testing.T) {
		probed := quietProbe()
		probed.vision = func() (settings.VisionFallbackSetting, error) {
			return settings.VisionFallbackSetting{Model: "global-model", Prompt: "global-prompt"}, nil
		}
		// Every override flag the key could carry is set; none may matter.
		key := &model.APIKey{
			CompressEnabledOverride:           true,
			CustomSystemPromptEnabledOverride: true,
			CustomSystemPromptEnabled:         true,
		}
		got := resolveRequestSettings(context.Background(), probed, key, "req-1")
		if got.VisionFallbackModel != "global-model" || got.VisionFallbackPrompt != "global-prompt" {
			t.Fatalf("vision fallback must come from the global setting only, got %+v", got)
		}
	})
}

// countingProvider wraps another provider and counts calls per method. The
// early-resolution contract (the resolve runs once at entry, before the body
// is validated) is about WHETHER the read happens on a request that later
// gets rejected — only a counter can see that.
type countingProvider struct {
	inner         SettingsProvider
	promptCalls   int
	compressCalls int
}

func (c *countingProvider) CustomSystemPrompt(ctx context.Context) (settings.CustomSystemPromptSetting, int64, error) {
	c.promptCalls++
	return c.inner.CustomSystemPrompt(ctx)
}

func (c *countingProvider) GetInputCompression(ctx context.Context) (bool, int64, error) {
	c.compressCalls++
	return c.inner.GetInputCompression(ctx)
}

func (c *countingProvider) GetVisionFallback(ctx context.Context) (settings.VisionFallbackSetting, int64, error) {
	return c.inner.GetVisionFallback(ctx)
}

// TestSettingsResolveRunsBeforeTheBodyIsValidated: the settings read runs
// once at entry, before body validation — so a request whose body never
// passes validation (rejected with 400 before any candidate runs) still
// performs the read, custom system prompt included. The read is one cached
// lookup on the reject path, the resolved fields reach no log, and the
// rejection the caller sees is unaffected.
func TestSettingsResolveRunsBeforeTheBodyIsValidated(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a body that fails validation")
	}))
	defer upstream.Close()

	stub := stubSettingsProvider{}
	counting := &countingProvider{inner: stub}
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewService(db, masterKey, false, counting, testGatewayConfig())
	svc.client.httpClient.Transport = &http.Transport{}
	// A bare Service records nothing; the recorder is wired here because the
	// settled reason below is asserted from the persisted row, as production
	// would see it.
	RegisterRecorder(svc, requestlog.New(db), func(e *Exchange) requestlog.View { return e })

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	// Passes the cheap meta check (non-empty messages, positive max_tokens),
	// fails the full decoder (content is an object, not a string or block
	// array) — a 400 that never reaches a candidate.
	body := []byte(`{"model":"claude-3-5-sonnet","max_tokens":1024,"messages":[{"role":"user","content":{"foo":"bar"}}]}`)
	c, w := newCtxPath("/v1/messages", body)
	svc.Handle(c, apiKey)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want the validation 400 unchanged by the early read; body = %s", w.Code, w.Body.String())
	}
	if counting.promptCalls != 1 {
		t.Errorf("custom system prompt reads = %d, want exactly 1 — the resolve runs once at entry, before validation", counting.promptCalls)
	}
	if counting.compressCalls != 1 {
		t.Errorf("compression reads = %d, want exactly 1 on a reject path", counting.compressCalls)
	}
	// The persisted row must carry the validator's verdict: the early read
	// resolves settings only and takes no part in the rejection.
	var reqLog model.RequestLog
	if err := db.First(&reqLog).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if reqLog.FailReason == nil {
		t.Error("log fail_reason is NULL, want the invalid_request verdict unchanged by the early read")
	} else if !strings.HasPrefix(*reqLog.FailReason, "invalid_request:") {
		t.Errorf("log fail_reason = %q, want the invalid_request verdict unchanged by the early read", *reqLog.FailReason)
	}
}

// TestSettingsResolveRunsBeforeAnIngressRewriterRefuses is the second half of
// the early-read contract: a body an ingress rewriter declares unsendable
// (rejected before any candidate, with the rewriter's own verdict surfaced)
// also performs the settings read. Same reasoning as the validation-reject
// twin above; both reject paths sit after the one resolve call.
func TestSettingsResolveRunsBeforeAnIngressRewriterRefuses(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be called for a body an ingress rewriter refused")
	}))
	defer upstream.Close()

	stub := stubSettingsProvider{}
	counting := &countingProvider{inner: stub}
	masterKey := bytes.Repeat([]byte{0x42}, 32)
	svc := NewService(db, masterKey, false, counting, testGatewayConfig())
	svc.client.httpClient.Transport = &http.Transport{}
	// Wired for the same reason as the validation twin: the settled reason is
	// asserted from the persisted row.
	RegisterRecorder(svc, requestlog.New(db), func(e *Exchange) requestlog.View { return e })

	// A rewriter that refuses the body outright, the way a payload
	// inspection would: the error maps to KindIngressRewriteFailed and the
	// caller sees the rewriter's verdict, never a candidate.
	RegisterIngressRewriter(svc, scriptedIngress{name: "refuser", on: func(body []byte) ([]byte, error) {
		return nil, errors.New("body unsendable")
	}}, StageCompress, noView)

	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.masterKey, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	apiKey := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the rewriter's refusal surfaced as its fixed 500; body = %s", w.Code, w.Body.String())
	}
	if counting.promptCalls != 1 {
		t.Errorf("custom system prompt reads = %d, want exactly 1 on the rewrite-refuse path", counting.promptCalls)
	}
	// Pin the settled reason too: a refusal that keeps its 500 but files the
	// row under a different reason would still be a caller-visible audit
	// regression, and only the exact string catches it.
	var reqLog model.RequestLog
	if err := db.First(&reqLog).Error; err != nil {
		t.Fatalf("no request_log row: %v", err)
	}
	if reqLog.FailReason == nil {
		t.Errorf("log fail_reason is NULL, want %q", "ingress_rewrite_failed")
	} else if *reqLog.FailReason != "ingress_rewrite_failed" {
		t.Errorf("log fail_reason = %q, want %q", *reqLog.FailReason, "ingress_rewrite_failed")
	}
}
