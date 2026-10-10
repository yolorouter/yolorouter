package gateway

// The model-capability temperature policy's behavioural contract, asserted
// at the surface the acceptance cases pin: the egress body the fake
// upstream receives. The default switch injects the addressed model's
// declared temperature only when the caller sent none; the force switch
// overrides one the caller did send; both stand down without a declaration;
// and the Claude thinking temperature floor outranks both — on the IR path
// the encoder overwrites the policy's value after it runs, and on the
// Claude passthrough path the policy stands down when the caller's body
// carries a thinking configuration.
//
// Two egress topologies carry the arms: the cross-protocol IR round trip
// (an OpenAI Chat caller against an anthropic provider, where the policy
// runs between the ingress decode and the egress encode) and the
// same-protocol byte passthrough (an OpenAI or native Gemini caller against
// a provider of its own protocol, where the same decision is applied to
// the forwarded bytes by field surgery).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"

	"gorm.io/gorm"
)

// declareDefaultTemperature sets the model's declared default temperature
// the way the admin console's capability metadata column does.
func declareDefaultTemperature(t *testing.T, db *gorm.DB, modelID uint, v float64) {
	t.Helper()
	if res := db.Exec("UPDATE models SET temperature = ? WHERE id = ?", v, modelID); res.Error != nil {
		t.Fatalf("declare default temperature: %v", res.Error)
	}
}

// temperatureOf reads the top-level temperature field of a captured egress
// body. Present distinguishes "the field is absent" from "the field is
// null": an absent field is what an untouched request looks like, and a
// null one is what nothing here should ever write.
func temperatureOf(t *testing.T, body []byte) (value float64, present bool) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("egress body is not a JSON object: %v; body = %s", err, body)
	}
	raw, ok := m["temperature"]
	if !ok || string(raw) == "null" {
		return 0, false
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("egress temperature is not a number: %v; body = %s", err, body)
	}
	return value, true
}

// TestTemperatureDecisionTable pins the policy's decision arithmetic on
// exact inputs, independent of any wire shape: without a declaration
// neither switch does anything, force wins over anything the caller sent,
// the default only fills an absent value, and both off leaves the request
// alone.
func TestTemperatureDecisionTable(t *testing.T) {
	declared, sent := 0.5, 0.9
	cases := []struct {
		name     string
		declared *float64
		sent     *float64
		ce       settings.CapabilityEnforcementSetting
		wantSet  bool
		want     float64
	}{
		{"no declaration, both switches on, still inert", nil, nil,
			settings.CapabilityEnforcementSetting{TemperatureDefault: true, TemperatureForce: true}, false, 0},
		{"default on and nothing sent injects", &declared, nil,
			settings.CapabilityEnforcementSetting{TemperatureDefault: true}, true, 0.5},
		{"default on and something sent leaves it", &declared, &sent,
			settings.CapabilityEnforcementSetting{TemperatureDefault: true}, false, 0},
		{"default off injects nothing", &declared, nil,
			settings.CapabilityEnforcementSetting{}, false, 0},
		{"force off does not override", &declared, &sent,
			settings.CapabilityEnforcementSetting{TemperatureDefault: true}, false, 0},
		{"force on overrides an explicit value", &declared, &sent,
			settings.CapabilityEnforcementSetting{TemperatureForce: true}, true, 0.5},
		{"force on also fills an absent value", &declared, nil,
			settings.CapabilityEnforcementSetting{TemperatureForce: true}, true, 0.5},
		{"force on with nothing declared is inert", nil, &sent,
			settings.CapabilityEnforcementSetting{TemperatureForce: true}, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := temperatureDecision(tc.declared, tc.sent, tc.ce)
			if ok != tc.wantSet {
				t.Fatalf("temperatureDecision set = %v, want %v", ok, tc.wantSet)
			}
			if ok && *got != tc.want {
				t.Fatalf("temperatureDecision value = %v, want %v", *got, tc.want)
			}
		})
	}
}

// TestTemperatureDefaultAppliedOnIRPath walks the mounted policy through
// its arms on the cross-protocol IR path (OpenAI Chat caller against an
// anthropic provider), asserting the egress body the upstream receives:
// an absent temperature becomes the declared default, an explicit one is
// left byte-alone, no declaration means no injection, and the switches off
// is today's behaviour.
func TestTemperatureDefaultAppliedOnIRPath(t *testing.T) {
	cases := []struct {
		name             string
		ce               settings.CapabilityEnforcementSetting
		declared         *float64 // nil = model declares nothing
		body             string
		wantPresent      bool
		wantValue        float64
		wantThinking     bool
		wantUpstreamTemp float64
	}{
		{
			name:        "absent temperature becomes the declared default",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			declared:    floatPtr(0.5),
			body:        `{"model":"gpt-4o","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.5,
		},
		{
			name:        "explicit temperature keeps the caller's value",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			declared:    floatPtr(0.5),
			body:        `{"model":"gpt-4o","max_tokens":2048,"temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.9,
		},
		{
			name:        "no declaration injects nothing",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			declared:    nil,
			body:        `{"model":"gpt-4o","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: false,
		},
		{
			name:        "switch off is today's behaviour",
			ce:          settings.CapabilityEnforcementSetting{},
			declared:    floatPtr(0.5),
			body:        `{"model":"gpt-4o","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: false,
		},
		{
			name:        "force off leaves an explicit value alone",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			declared:    floatPtr(0.5),
			body:        `{"model":"gpt-4o","max_tokens":2048,"temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.9,
		},
		{
			name:        "force on overrides the explicit value with the declaration",
			ce:          settings.CapabilityEnforcementSetting{TemperatureForce: true},
			declared:    floatPtr(0.5),
			body:        `{"model":"gpt-4o","max_tokens":2048,"temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.5,
		},
		{
			name:        "force on with nothing declared leaves the explicit value",
			ce:          settings.CapabilityEnforcementSetting{TemperatureForce: true},
			declared:    nil,
			body:        `{"model":"gpt-4o","max_tokens":2048,"temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.9,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
			}))
			defer upstream.Close()

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: tc.ce})
			p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
			if tc.declared != nil {
				declareDefaultTemperature(t, db, m.ID, *tc.declared)
			}
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtx([]byte(tc.body))
			svc.Handle(c, apiKey)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			value, present := temperatureOf(t, upstreamBody)
			if present != tc.wantPresent {
				t.Fatalf("egress temperature present = %v, want %v; body = %s", present, tc.wantPresent, upstreamBody)
			}
			if present && value != tc.wantValue {
				t.Fatalf("egress temperature = %v, want %v; body = %s", value, tc.wantValue, upstreamBody)
			}
		})
	}
}

// TestTemperatureAppliedOnPassthroughPath carries the same decision onto
// the same-protocol passthrough body — the topology the e2e stub drives —
// where no IR exists and the policy works by field surgery on the
// forwarded bytes: an absent temperature gains the declared default, an
// explicit one survives with the switch off, and force replaces it.
func TestTemperatureAppliedOnPassthroughPath(t *testing.T) {
	cases := []struct {
		name        string
		ce          settings.CapabilityEnforcementSetting
		body        string
		wantPresent bool
		wantValue   float64
	}{
		{
			name:        "absent temperature gains the declared default",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			body:        `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.5,
		},
		{
			name:        "explicit temperature survives the default switch",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			body:        `{"model":"gpt-4o","temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.9,
		},
		{
			name:        "force overrides the explicit value",
			ce:          settings.CapabilityEnforcementSetting{TemperatureForce: true},
			body:        `{"model":"gpt-4o","temperature":0.9,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.5,
		},
		{
			name:        "switches off forward the bytes untouched",
			ce:          settings.CapabilityEnforcementSetting{},
			body:        `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: false,
		},
		{
			name:        "an explicit null reads as absent and gains the default",
			ce:          settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			body:        `{"model":"gpt-4o","temperature":null,"messages":[{"role":"user","content":"hi"}]}`,
			wantPresent: true, wantValue: 0.5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
			}))
			defer upstream.Close()

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: tc.ce})
			p := createProvider(t, db, "p1", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			declareDefaultTemperature(t, db, m.ID, 0.5)
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtx([]byte(tc.body))
			svc.Handle(c, apiKey)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			value, present := temperatureOf(t, upstreamBody)
			if present != tc.wantPresent {
				t.Fatalf("egress temperature present = %v, want %v; body = %s", present, tc.wantPresent, upstreamBody)
			}
			if present && value != tc.wantValue {
				t.Fatalf("egress temperature = %v, want %v; body = %s", value, tc.wantValue, upstreamBody)
			}
		})
	}
}

// TestTemperatureThinkingInteractionKeepsProtocolFloor pins the priority
// the acceptance cases demand: with Claude thinking enabled, the encoder's
// existing temperature=1.0 floor outranks the declared default — with
// either switch on — and on the Claude passthrough path, where no encoder
// runs, the policy stands down entirely rather than injecting a value the
// thinking constraint forbids.
//
// The floor erases the policy's own value from the egress body (1.0
// whether the policy injected 0.5, forced 0.5, or never ran), so each
// priority arm is paired with a control arm on the SAME topology minus
// thinking: the control proves the policy demonstrably injects there, so
// the priority arm's 1.0 means the floor won, not that nothing ran.
func TestTemperatureThinkingInteractionKeepsProtocolFloor(t *testing.T) {
	// The IR path: an OpenAI Chat caller sends reasoning_effort (thinking
	// on arrival at the Claude encoder) against an anthropic provider with
	// a declared default of 0.5. The encoder runs after the policy, so
	// whatever the policy decided, the egress temperature is 1.0.
	irArm := func(t *testing.T, ce settings.CapabilityEnforcementSetting, thinking bool) (float64, bool, map[string]json.RawMessage) {
		t.Helper()
		db := testutil.NewSQLiteDB(t)
		var upstreamBody []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
		}))
		defer upstream.Close()

		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: ce})
		p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
		declareDefaultTemperature(t, db, m.ID, 0.5)
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		body := `{"model":"gpt-4o","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`
		if thinking {
			body = `{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`
		}
		c, w := newCtx([]byte(body))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var sent map[string]json.RawMessage
		if err := json.Unmarshal(upstreamBody, &sent); err != nil {
			t.Fatalf("egress body is not JSON: %v; body = %s", err, upstreamBody)
		}
		value, present := temperatureOf(t, upstreamBody)
		return value, present, sent
	}

	t.Run("control: same topology without thinking injects the default", func(t *testing.T) {
		value, present, _ := irArm(t, settings.CapabilityEnforcementSetting{TemperatureDefault: true}, false)
		if !present || value != 0.5 {
			t.Fatalf("egress temperature = (%v, present=%v), want 0.5 — without this control the priority arms below prove nothing", value, present)
		}
	})
	t.Run("default switch loses to the thinking floor", func(t *testing.T) {
		value, present, sent := irArm(t, settings.CapabilityEnforcementSetting{TemperatureDefault: true}, true)
		if !present || value != 1.0 {
			t.Fatalf("egress temperature = (%v, present=%v), want 1.0 — the thinking floor must outrank the declared default; body = %s", value, present, rawOf(sent))
		}
		if _, thinking := sent["thinking"]; !thinking {
			t.Fatalf("control arm failed: egress body carries no thinking config, so the 1.0 assertion proves nothing about priority; body = %s", rawOf(sent))
		}
	})
	t.Run("force switch also loses to the thinking floor", func(t *testing.T) {
		value, present, sent := irArm(t, settings.CapabilityEnforcementSetting{TemperatureForce: true}, true)
		if !present || value != 1.0 {
			t.Fatalf("egress temperature = (%v, present=%v), want 1.0 — the thinking floor must outrank force; body = %s", value, present, rawOf(sent))
		}
		if _, thinking := sent["thinking"]; !thinking {
			t.Fatalf("control arm failed: egress body carries no thinking config; body = %s", rawOf(sent))
		}
	})

	// The passthrough path: a Claude caller's own thinking configuration
	// declares the constraint, no encoder will enforce it after the policy,
	// so the policy stands down — the caller's sampling state travels as
	// the caller sent it.
	claudePassthrough := func(t *testing.T, ce settings.CapabilityEnforcementSetting, body string) map[string]json.RawMessage {
		t.Helper()
		db := testutil.NewSQLiteDB(t)
		var upstreamBody []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
		}))
		defer upstream.Close()

		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: ce})
		p := createAnthropicProvider(t, db, "claude-provider", upstream.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-20241022", true, true, 1)
		declareDefaultTemperature(t, db, m.ID, 0.5)
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtxPath("/v1/messages", []byte(body))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		var sent map[string]json.RawMessage
		if err := json.Unmarshal(upstreamBody, &sent); err != nil {
			t.Fatalf("egress body is not JSON: %v; body = %s", err, upstreamBody)
		}
		return sent
	}

	t.Run("control: claude passthrough without thinking injects the default", func(t *testing.T) {
		sent := claudePassthrough(t,
			settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			`{"model":"claude-3-5-sonnet","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`)
		value, present := temperatureOf(t, rawOf(sent))
		if !present || value != 0.5 {
			t.Fatalf("egress temperature = (%v, present=%v), want 0.5 — the claude passthrough topology must inject before the stand-down arm can mean anything; body = %s", value, present, rawOf(sent))
		}
	})
	t.Run("claude passthrough with thinking stands down", func(t *testing.T) {
		sent := claudePassthrough(t,
			settings.CapabilityEnforcementSetting{TemperatureDefault: true, TemperatureForce: true},
			`{"model":"claude-3-5-sonnet","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`)
		if _, thinking := sent["thinking"]; !thinking {
			t.Fatalf("caller's thinking config did not survive passthrough; body = %s", rawOf(sent))
		}
		if value, present := temperatureOf(t, rawOf(sent)); present {
			t.Fatalf("policy injected temperature %v into a thinking passthrough body — the constraint outranks both switches; body = %s", value, rawOf(sent))
		}
	})
}

// TestTemperatureGeminiPassthroughNestsUnderGenerationConfig covers the
// one protocol whose temperature is not a top-level field: on a native
// Gemini passthrough the policy creates or amends generationConfig, and
// leaves the body byte-alone when it declines.
func TestTemperatureGeminiPassthroughNestsUnderGenerationConfig(t *testing.T) {
	cases := []struct {
		name     string
		ce       settings.CapabilityEnforcementSetting
		body     string
		wantTemp *float64 // nil = expect no temperature inside generationConfig
	}{
		{
			name:     "no generationConfig gains one with the declared default",
			ce:       settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			wantTemp: floatPtr(0.5),
		},
		{
			name:     "explicit generationConfig temperature survives the default switch",
			ce:       settings.CapabilityEnforcementSetting{TemperatureDefault: true},
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.9}}`,
			wantTemp: floatPtr(0.9),
		},
		{
			name:     "force replaces the nested value",
			ce:       settings.CapabilityEnforcementSetting{TemperatureForce: true},
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.9}}`,
			wantTemp: floatPtr(0.5),
		},
		{
			name:     "switches off leave the nested state alone",
			ce:       settings.CapabilityEnforcementSetting{},
			body:     `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{}}`,
			wantTemp: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			var upstreamBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9},"modelVersion":"gemini-2.0-flash-real"}`))
			}))
			defer upstream.Close()

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: tc.ce})
			p := createGeminiProvider(t, db, "gemini-provider", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-gemini-upstream", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gemini-2.0-flash-real", true, true, 1)
			declareDefaultTemperature(t, db, m.ID, 0.5)
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:generateContent", []byte(tc.body))
			svc.Handle(c, apiKey)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(upstreamBody, &sent); err != nil {
				t.Fatalf("egress body is not JSON: %v; body = %s", err, upstreamBody)
			}
			var gc map[string]json.RawMessage
			if raw, ok := sent["generationConfig"]; ok && string(raw) != "null" {
				if err := json.Unmarshal(raw, &gc); err != nil {
					t.Fatalf("generationConfig is not an object: %v; body = %s", err, upstreamBody)
				}
			}
			if tc.wantTemp == nil {
				if raw, ok := gc["temperature"]; ok && string(raw) != "null" {
					t.Fatalf("generationConfig.temperature = %s, want absent", raw)
				}
				return
			}
			if gc == nil || gc["temperature"] == nil {
				t.Fatalf("generationConfig.temperature missing; body = %s", upstreamBody)
			}
			value, present := temperatureOf(t, []byte(`{"temperature":`+string(gc["temperature"])+`}`))
			if !present || value != *tc.wantTemp {
				t.Fatalf("generationConfig.temperature = %v (present=%v), want %v; body = %s", value, present, *tc.wantTemp, upstreamBody)
			}
		})
	}
}

// TestTemperatureGeminiNonObjectGenerationConfigDeclines pins the decline
// branch geminiGenerationTemperature carries: a generationConfig that exists
// but is not a JSON object makes the policy hand the bytes back untouched,
// under either switch — the caller put something structurally odd where the
// temperature would go, and declining leaves a body a lenient upstream might
// still accept intact.
//
// Reachability, for the record: this shape cannot arrive through Handle.
// Admission runs the full structural decode before any candidate exists
// (Admit → validateIngressBody → the gemini codec's DecodeRequest), and the
// codec's generationConfig is an object field, so a string one is refused
// with 400 at the door — peekGeminiIngress's own lenient wire (contents and
// tools only) is not the last word on admission. The branch is therefore
// defensive, the same family as the literal-null arm below and the
// rewriteJSONStringField precedent it follows, and is pinned at the function
// seam the mount calls. The control arm on the same input minus the
// malformation proves the policy demonstrably injects there, so the decline
// arms' untouched bytes mean the branch declined, not that nothing ran.
// Delete the branch and the caller's string is replaced by an object
// carrying temperature — these arms go red.
func TestTemperatureGeminiNonObjectGenerationConfigDeclines(t *testing.T) {
	seam := func(t *testing.T, ce settings.CapabilityEnforcementSetting, generationConfig string) []byte {
		t.Helper()
		body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]` + generationConfig + `}`)
		out, err := applyPassthroughTemperature(body, protocols.ProtocolGemini, floatPtr(0.5), ce)
		if err != nil {
			t.Fatalf("applyPassthroughTemperature errored (%v) — the decline hands the bytes back, it does not fail the request", err)
		}
		return out
	}

	t.Run("control: same seam with a decodable generationConfig injects", func(t *testing.T) {
		out := seam(t, settings.CapabilityEnforcementSetting{TemperatureDefault: true}, `,"generationConfig":{}`)
		var sent map[string]json.RawMessage
		if err := json.Unmarshal(out, &sent); err != nil {
			t.Fatalf("egress body is not JSON: %v; body = %s", err, out)
		}
		raw, ok := sent["generationConfig"]
		if !ok {
			t.Fatalf("egress body carries no generationConfig; body = %s", out)
		}
		var gc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &gc); err != nil {
			t.Fatalf("control arm's generationConfig is not an object: %v; body = %s", err, out)
		}
		temp, ok := gc["temperature"]
		if !ok || string(temp) == "null" {
			t.Fatalf("control arm: generationConfig.temperature not injected — without this control the decline arms below prove nothing; body = %s", out)
		}
		var v float64
		if err := json.Unmarshal(temp, &v); err != nil || v != 0.5 {
			t.Fatalf("control arm: generationConfig.temperature = %s, want 0.5; body = %s", temp, out)
		}
	})

	declineArm := func(t *testing.T, ce settings.CapabilityEnforcementSetting) {
		t.Helper()
		const in = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":"opaque"}`
		out := seam(t, ce, `,"generationConfig":"opaque"`)
		if string(out) != in {
			t.Fatalf("egress body = %s, want the caller's bytes %s untouched — a non-object generationConfig makes the policy decline, not rewrite", out, in)
		}
	}
	t.Run("default switch declines on a non-object generationConfig", func(t *testing.T) {
		declineArm(t, settings.CapabilityEnforcementSetting{TemperatureDefault: true})
	})
	t.Run("force switch declines on a non-object generationConfig", func(t *testing.T) {
		declineArm(t, settings.CapabilityEnforcementSetting{TemperatureForce: true})
	})
}

// TestApplyPassthroughTemperatureLiteralNullBodyDeclines covers the
// defensive arm admission makes unreachable: a JSON literal "null" parses
// into a nil map with no error, and writing a field onto a nil map panics —
// the anti-panic shape rewriteJSONStringField already set (openai.go). No
// ingress peek admits such a body to the passthrough path (each validates
// required object fields first), so the arm is pinned at the function seam,
// in the maximal-touch configuration — both switches on and a declaration
// present — where the early exit is the only thing keeping the bytes
// untouched. Delete the arm and the nil-map write panics; this goes red.
func TestApplyPassthroughTemperatureLiteralNullBodyDeclines(t *testing.T) {
	ce := settings.CapabilityEnforcementSetting{TemperatureDefault: true, TemperatureForce: true}
	for _, egress := range []protocols.ProtocolID{protocols.ProtocolOpenAI, protocols.ProtocolClaude, protocols.ProtocolGemini} {
		t.Run(fmt.Sprintf("%s egress hands a literal null body back untouched", egress), func(t *testing.T) {
			out, err := applyPassthroughTemperature([]byte("null"), egress, floatPtr(0.5), ce)
			if err != nil {
				t.Fatalf("applyPassthroughTemperature errored (%v) on a literal null body — the defensive arm hands the bytes back, it does not fail the request", err)
			}
			if string(out) != "null" {
				t.Fatalf("egress body = %s, want the literal null body byte-for-byte untouched", out)
			}
		})
	}
}

// floatPtr is the local float pointer helper; the package's boolPtr is the
// shape this follows.
func floatPtr(v float64) *float64 { return &v }

// rawOf renders a decoded body map back to raw bytes for failure messages.
func rawOf(m map[string]json.RawMessage) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("<undecodable>")
	}
	return b
}
