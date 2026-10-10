package gateway

// The model-capability reasoning egress clear's behavioural contract,
// asserted at the surfaces the acceptance cases pin: the per-landing
// decision as a pure seam (including the both-switches matrix cell), the
// egress body the fake upstream receives on both topologies (cross-protocol
// IR round trip and same-protocol passthrough), and the four landing arms —
// first route cleared, failover route cleared the same, undeclared landing
// untouched, and the gate-off control proving the same request demonstrably
// carries a reasoning injection when nobody clears it (Claude's encoder
// declines thinking under a small max_tokens all by itself, so a "no
// thinking" assertion without the control can pass vacuously).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// clearOn builds the settings snapshot of a site that switched the
// reasoning egress clear on.
func clearOn() settings.CapabilityEnforcementSetting {
	return settings.CapabilityEnforcementSetting{ReasoningEgressClear: true}
}

// capturedUpstream records every body a fake upstream received, per server,
// so the failover arm can assert what EACH landing saw.
type capturedUpstream struct {
	mu     sync.Mutex
	bodies [][]byte
	server *httptest.Server
}

// newClaudeUpstream answers with a Claude Messages non-stream response.
func newClaudeUpstream(t *testing.T) *capturedUpstream {
	t.Helper()
	up := &capturedUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.bodies = append(up.bodies, body)
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet-20241022","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`))
	}))
	t.Cleanup(up.server.Close)
	return up
}

// newOpenAIUpstream answers with an OpenAI Chat non-stream response.
func newOpenAIUpstream(t *testing.T) *capturedUpstream {
	t.Helper()
	up := &capturedUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.bodies = append(up.bodies, body)
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	t.Cleanup(up.server.Close)
	return up
}

// newGeminiUpstream answers with a Gemini non-stream response.
func newGeminiUpstream(t *testing.T) *capturedUpstream {
	t.Helper()
	up := &capturedUpstream{}
	up.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		up.mu.Lock()
		up.bodies = append(up.bodies, body)
		up.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9},"modelVersion":"gemini-2.0-flash-real"}`))
	}))
	t.Cleanup(up.server.Close)
	return up
}

// bodies snapshots what the upstream recorded.
func (u *capturedUpstream) recorded() [][]byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([][]byte(nil), u.bodies...)
}

// decodeBody parses a captured body, refusing to guess at malformed bytes.
func decodeBody(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("egress body is not a JSON object: %v; body = %s", err, body)
	}
	return m
}

// thinkingField reads a Claude egress body's thinking field; present
// distinguishes injected from absent.
func thinkingField(t *testing.T, body []byte) bool {
	t.Helper()
	raw, ok := decodeBody(t, body)["thinking"]
	return ok && string(raw) != "null"
}

// geminiThinkingConfig reports whether a Gemini egress body's
// generationConfig carries a thinkingConfig.
func geminiThinkingConfig(t *testing.T, body []byte) bool {
	t.Helper()
	raw, ok := decodeBody(t, body)["generationConfig"]
	if !ok || string(raw) == "null" {
		return false
	}
	var gc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &gc); err != nil {
		t.Fatalf("generationConfig is not an object: %v; body = %s", err, body)
	}
	cfg, ok := gc["thinkingConfig"]
	return ok && string(cfg) != "null"
}

// reasoningEffortField reports whether an OpenAI Chat egress body carries
// reasoning_effort.
func reasoningEffortField(t *testing.T, body []byte) bool {
	t.Helper()
	raw, ok := decodeBody(t, body)["reasoning_effort"]
	return ok && string(raw) != "null"
}

// openAIReasoningCallerBody is the fixture every IR clear arm sends: a
// reasoning-bearing OpenAI Chat request with a max_tokens large enough that
// the Claude encoder's thinking floor does not silently decline the
// injection (the vacuous-pass trap the control arm exists to defuse).
const openAIReasoningCallerBody = `{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`

// TestReasoningClearDueTable pins the per-landing decision at its seam:
// only the clear switch plus an explicit false declaration strips; nil and
// true never do; and the strict entry flag does not gate the clear — the
// matrix cell where both switches are on and the landing is declared false
// strips, because the two rules answer different questions.
func TestReasoningClearDueTable(t *testing.T) {
	cases := []struct {
		name    string
		cand    Candidate
		ce      settings.CapabilityEnforcementSetting
		wantDue bool
	}{
		{"clear on, landing declared false", Candidate{DeclaredReasoning: boolPtr(false)}, clearOn(), true},
		{"clear off is today's behaviour", Candidate{DeclaredReasoning: boolPtr(false)}, settings.CapabilityEnforcementSetting{}, false},
		{"clear on, landing undeclared", Candidate{}, clearOn(), false},
		{"clear on, landing declared true", Candidate{DeclaredReasoning: boolPtr(true)}, clearOn(), false},
		{"both switches on, landing declared false: the clear still strips", Candidate{DeclaredReasoning: boolPtr(false)},
			settings.CapabilityEnforcementSetting{ReasoningStrict: true, ReasoningEgressClear: true}, true},
		{"strict alone never strips", Candidate{DeclaredReasoning: boolPtr(false)},
			settings.CapabilityEnforcementSetting{ReasoningStrict: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasoningClearDue(tc.cand, tc.ce); got != tc.wantDue {
				t.Fatalf("reasoningClearDue = %v, want %v", got, tc.wantDue)
			}
		})
	}
}

// TestReasoningClearFirstRoute is the clear's home arm: a reasoning request
// whose addressed model is declared not to reason, switched on, lands on its
// first (and only) candidate — the egress body the upstream receives carries
// no thinking configuration, and the request is served, not refused.
func TestReasoningClearFirstRoute(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	up := newClaudeUpstream(t)

	svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: clearOn()})
	p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	declareModelCapability(t, db, m.ID, nil, boolPtr(false))
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(openAIReasoningCallerBody))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the clear forwards, it does not refuse; body = %s", w.Code, w.Body.String())
	}
	recorded := up.recorded()
	if len(recorded) != 1 {
		t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
	}
	if thinkingField(t, recorded[0]) {
		t.Fatalf("egress body carries thinking config on a landing declared not to reason; body = %s", recorded[0])
	}
	// The strip is scoped: the request itself still arrives whole.
	sent := decodeBody(t, recorded[0])
	if _, ok := sent["messages"]; !ok {
		t.Fatalf("cleared body lost its messages; body = %s", recorded[0])
	}
	if model, ok := sent["model"]; !ok || !strings.Contains(string(model), "claude") {
		t.Fatalf("cleared body lost its provider model rewrite; body = %s", recorded[0])
	}
}

// TestReasoningClearFailoverRoute is the scenario the rule exists for: the
// first candidate fails over and the walk lands on a second provider — the
// retry path must clear exactly as the first route did. Both landings are
// asserted, because the clear runs per candidate at PrepareUpstream time,
// not once for the request.
func TestReasoningClearFailoverRoute(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	first := &capturedUpstream{}
	first.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		first.mu.Lock()
		first.bodies = append(first.bodies, body)
		first.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"upstream fell over"}}`))
	}))
	t.Cleanup(first.server.Close)
	second := newClaudeUpstream(t)

	svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: clearOn()})
	p1 := createAnthropicProvider(t, db, "claude-p1", first.server.URL)
	createProviderKey(t, db, svc.secrets, p1.ID, "sk-1", "k1", 1, true)
	p2 := createAnthropicProvider(t, db, "claude-p2", second.server.URL)
	createProviderKey(t, db, svc.secrets, p2.ID, "sk-2", "k2", 1, true)

	now := time.Now().UTC()
	m := &Model{Name: "gpt-4o", ManagementStatus: ModelStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model: %v", err)
	}
	for i, p := range []*Provider{p1, p2} {
		if err := db.Create(&ModelCandidate{
			ModelID: m.ID, ProviderID: p.ID, ProviderModelName: "claude-3-5-sonnet-20241022",
			InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
			SupportsStreaming: boolPtr(true), SupportsFunctionCalling: boolPtr(true),
			ManagementStatus: ModelCandidateStatusEnabled, SortOrder: i + 1,
			VerificationStatus: ModelVerificationStatusPassed,
			CreatedAt:          now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatalf("seed candidate %d: %v", i, err)
		}
	}
	declareModelCapability(t, db, m.ID, nil, boolPtr(false))
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(openAIReasoningCallerBody))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after failover; body = %s", w.Code, w.Body.String())
	}
	firstRecorded := first.recorded()
	if len(firstRecorded) != 1 {
		t.Fatalf("first upstream recorded %d requests, want the one dispatch that failed", len(firstRecorded))
	}
	if thinkingField(t, firstRecorded[0]) {
		t.Fatalf("first (failing) landing's egress body carries thinking config — the clear must run on every landing; body = %s", firstRecorded[0])
	}
	recorded := second.recorded()
	if len(recorded) != 1 {
		t.Fatalf("second upstream recorded %d requests, want exactly the failover dispatch", len(recorded))
	}
	if thinkingField(t, recorded[0]) {
		t.Fatalf("failover landing's egress body carries thinking config; body = %s", recorded[0])
	}
}

// TestReasoningClearUndeclaredLandingKeepsTodayBehaviour is the tri-state
// arm: with the clear switched on but the landing's declaration absent,
// the injection the encoders perform today still happens — an admin who
// never declared the model non-reasoning must not have the gateway editing
// reasoning requests on their behalf.
func TestReasoningClearUndeclaredLandingKeepsTodayBehaviour(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	up := newClaudeUpstream(t)

	svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: clearOn()})
	p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	// supports_reasoning stays NULL.
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(openAIReasoningCallerBody))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	recorded := up.recorded()
	if len(recorded) != 1 {
		t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
	}
	if !thinkingField(t, recorded[0]) {
		t.Fatalf("undeclared landing lost today's thinking injection; body = %s", recorded[0])
	}
}

// TestReasoningClearControlGateOffInjects is the control arm that keeps the
// clear arms honest: the SAME request against the SAME declared-false
// model with the switch off must show the thinking injection present in the
// egress body. Claude's encoder already declines thinking under a small
// max_tokens, so without this arm a "no thinking" assertion can pass while
// nothing was ever cleared — here max_tokens is 2048 and the injection is
// demonstrably there until the switch removes it.
func TestReasoningClearControlGateOffInjects(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	up := newClaudeUpstream(t)

	svc := newSvcWithSettings(t, db, stubSettingsProvider{})
	p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
	declareModelCapability(t, db, m.ID, nil, boolPtr(false))
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(openAIReasoningCallerBody))
	svc.Handle(c, apiKey)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	recorded := up.recorded()
	if len(recorded) != 1 {
		t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
	}
	if !thinkingField(t, recorded[0]) {
		t.Fatalf("control arm failed: egress body carries no thinking injection, so the clear arms' absence assertions prove nothing; body = %s", recorded[0])
	}
}

// TestReasoningClearThreeProtocolsIR asserts the clear on the cross-protocol
// IR path for every egress dialect: Claude's thinking, Gemini's
// thinkingConfig, and OpenAI's reasoning_effort. Each cleared arm is paired
// with a switch-off control on the same topology proving the field was
// demonstrably there.
func TestReasoningClearThreeProtocolsIR(t *testing.T) {
	t.Run("claude egress: thinking cleared, control injects", func(t *testing.T) {
		run := func(clear bool) bool {
			t.Helper()
			db := testutil.NewSQLiteDB(t)
			up := newClaudeUpstream(t)
			ce := settings.CapabilityEnforcementSetting{}
			if clear {
				ce = clearOn()
			}
			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: ce})
			p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
			declareModelCapability(t, db, m.ID, nil, boolPtr(false))
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})
			c, w := newCtx([]byte(openAIReasoningCallerBody))
			svc.Handle(c, apiKey)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			recorded := up.recorded()
			if len(recorded) != 1 {
				t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
			}
			return thinkingField(t, recorded[0])
		}
		if run(false) != true {
			t.Fatal("control: the claude egress must carry thinking when the clear is off, or the cleared arm proves nothing")
		}
		if got := run(true); got {
			t.Fatal("cleared: the claude egress still carries thinking after the clear")
		}
	})

	t.Run("gemini egress: thinkingConfig cleared, control injects", func(t *testing.T) {
		run := func(clear bool) bool {
			t.Helper()
			db := testutil.NewSQLiteDB(t)
			up := newGeminiUpstream(t)
			ce := settings.CapabilityEnforcementSetting{}
			if clear {
				ce = clearOn()
			}
			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: ce})
			p := createGeminiProvider(t, db, "gemini-provider", up.server.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-gemini-upstream", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gemini-2.0-flash-real", true, true, 1)
			declareModelCapability(t, db, m.ID, nil, boolPtr(false))
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})
			c, w := newCtx([]byte(openAIReasoningCallerBody))
			svc.Handle(c, apiKey)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			recorded := up.recorded()
			if len(recorded) != 1 {
				t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
			}
			return geminiThinkingConfig(t, recorded[0])
		}
		if run(false) != true {
			t.Fatal("control: the gemini egress must carry thinkingConfig when the clear is off, or the cleared arm proves nothing")
		}
		if got := run(true); got {
			t.Fatal("cleared: the gemini egress still carries thinkingConfig after the clear")
		}
	})

	t.Run("openai egress: reasoning_effort cleared, control emits it", func(t *testing.T) {
		const claudeCallerBody = `{"model":"gpt-4o","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`
		run := func(clear bool) bool {
			t.Helper()
			db := testutil.NewSQLiteDB(t)
			up := newOpenAIUpstream(t)
			ce := settings.CapabilityEnforcementSetting{}
			if clear {
				ce = clearOn()
			}
			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: ce})
			p := createProvider(t, db, "openai-provider", up.server.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-openai-upstream", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			declareModelCapability(t, db, m.ID, nil, boolPtr(false))
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})
			c, w := newCtxPath("/v1/messages", []byte(claudeCallerBody))
			svc.Handle(c, apiKey)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			recorded := up.recorded()
			if len(recorded) != 1 {
				t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
			}
			return reasoningEffortField(t, recorded[0])
		}
		if run(false) != true {
			t.Fatal("control: the openai egress must carry reasoning_effort when the clear is off, or the cleared arm proves nothing")
		}
		if got := run(true); got {
			t.Fatal("cleared: the openai egress still carries reasoning_effort after the clear")
		}
	})
}

// TestReasoningClearThreeProtocolsPassthrough asserts the same strip on the
// same-protocol byte passthrough, where the caller's own reasoning field
// rides verbatim: OpenAI's two spellings, Claude's thinking, and Gemini's
// nested thinkingConfig each leave a body that no longer names reasoning.
func TestReasoningClearThreeProtocolsPassthrough(t *testing.T) {
	t.Run("openai passthrough: both reasoning spellings removed", func(t *testing.T) {
		db := testutil.NewSQLiteDB(t)
		up := newOpenAIUpstream(t)
		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: clearOn()})
		p := createProvider(t, db, "openai-provider", up.server.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
		declareModelCapability(t, db, m.ID, nil, boolPtr(false))
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtx([]byte(`{"model":"gpt-4o","reasoning_effort":"high","reasoning":{"budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		recorded := up.recorded()
		if len(recorded) != 1 {
			t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
		}
		sent := decodeBody(t, recorded[0])
		for _, key := range []string{"reasoning_effort", "reasoning"} {
			if raw, ok := sent[key]; ok && string(raw) != "null" {
				t.Fatalf("passthrough body still carries %s after the clear; body = %s", key, recorded[0])
			}
		}
		if _, ok := sent["messages"]; !ok {
			t.Fatalf("cleared passthrough lost its messages; body = %s", recorded[0])
		}
	})

	t.Run("claude passthrough: thinking removed", func(t *testing.T) {
		db := testutil.NewSQLiteDB(t)
		up := newClaudeUpstream(t)
		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: clearOn()})
		p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "claude-3-5-sonnet", "claude-3-5-sonnet-20241022", true, true, 1)
		declareModelCapability(t, db, m.ID, nil, boolPtr(false))
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtxPath("/v1/messages", []byte(`{"model":"claude-3-5-sonnet","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":"hi"}]}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		recorded := up.recorded()
		if len(recorded) != 1 {
			t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
		}
		if thinkingField(t, recorded[0]) {
			t.Fatalf("claude passthrough body still carries thinking after the clear; body = %s", recorded[0])
		}
	})

	t.Run("gemini passthrough: thinkingConfig removed, siblings kept", func(t *testing.T) {
		db := testutil.NewSQLiteDB(t)
		up := newGeminiUpstream(t)
		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: clearOn()})
		p := createGeminiProvider(t, db, "gemini-provider", up.server.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-gemini-upstream", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gemini-2.0-flash", "gemini-2.0-flash-real", true, true, 1)
		declareModelCapability(t, db, m.ID, nil, boolPtr(false))
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtxPath("/v1beta/models/gemini-2.0-flash:generateContent",
			[]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1024},"temperature":0.7}}`))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		recorded := up.recorded()
		if len(recorded) != 1 {
			t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
		}
		sent := decodeBody(t, recorded[0])
		raw, ok := sent["generationConfig"]
		if !ok {
			t.Fatalf("cleared gemini passthrough lost generationConfig entirely; body = %s", recorded[0])
		}
		var gc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &gc); err != nil {
			t.Fatalf("generationConfig is not an object: %v; body = %s", err, recorded[0])
		}
		if cfg, ok := gc["thinkingConfig"]; ok && string(cfg) != "null" {
			t.Fatalf("gemini passthrough still carries thinkingConfig after the clear; body = %s", recorded[0])
		}
		if temp, ok := gc["temperature"]; !ok || string(temp) == "null" {
			t.Fatalf("clear dropped a sibling generationConfig field it had no business touching; body = %s", recorded[0])
		}
	})
}

// TestStrictPlusClearMatrix pins the both-switches result matrix. Cell one
// is handler-realizable and asserted end to end: the entry refusal wins.
// Cell two — entry misses, landing declared false — is pinned at the seam
// (TestReasoningClearDueTable's both-switches arm) plus the handler arm
// here showing an entry miss forwards; the exact "entry missed on a
// false-declared landing" combination cannot be built through Handle today
// because every landing offer carries the addressed model's own row value
// (a per-candidate declaration is out of scope for this batch), so the
// seam is where the matrix's second cell is proven to compose.
func TestStrictPlusClearMatrix(t *testing.T) {
	t.Run("entry hit: both switches on, addressed model declared false, 400 wins", func(t *testing.T) {
		db := testutil.NewSQLiteDB(t)
		up := newClaudeUpstream(t)
		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: settings.CapabilityEnforcementSetting{
			ReasoningStrict:      true,
			ReasoningEgressClear: true,
		}})
		p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
		declareModelCapability(t, db, m.ID, nil, boolPtr(false))
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtx([]byte(openAIReasoningCallerBody))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 — the entry refusal must win over the clear; body = %s", w.Code, w.Body.String())
		}
		if got := len(up.recorded()); got != 0 {
			t.Fatalf("upstream recorded %d requests, want 0 — a 400 must precede any dispatch", got)
		}
	})

	t.Run("entry miss: undeclared addressed model forwards under both switches", func(t *testing.T) {
		db := testutil.NewSQLiteDB(t)
		up := newClaudeUpstream(t)
		svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: settings.CapabilityEnforcementSetting{
			ReasoningStrict:      true,
			ReasoningEgressClear: true,
		}})
		p := createAnthropicProvider(t, db, "claude-provider", up.server.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-claude-upstream", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gpt-4o", "claude-3-5-sonnet-20241022", true, true, 1)
		// supports_reasoning stays NULL: the entry question has no
		// declaration to refuse on, and the landing carries the same
		// undeclaration, so nothing is cleared either.
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtx([]byte(openAIReasoningCallerBody))
		svc.Handle(c, apiKey)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — an entry miss must forward; body = %s", w.Code, w.Body.String())
		}
		recorded := up.recorded()
		if len(recorded) != 1 {
			t.Fatalf("upstream recorded %d requests, want 1", len(recorded))
		}
		if !thinkingField(t, recorded[0]) {
			t.Fatalf("undeclared landing must keep today's injection under both switches; body = %s", recorded[0])
		}
	})
}

// TestStripPassthroughReasoningSeams pins the passthrough strip's decline
// and shape branches at the function seam: nothing to strip leaves the
// bytes untouched (no re-marshal, so formatting survives), a literal null
// body and an unknown protocol decline, an explicitly nulled field reads
// as absent, and a Gemini generationConfig the clear cannot parse declines
// rather than rewriting.
func TestStripPassthroughReasoningSeams(t *testing.T) {
	const untouched = `{"model":"gpt-4o-real","messages":[{"role":"user","content":"hi"}],"temperature":0.9}`

	t.Run("nothing to strip is byte-identical", func(t *testing.T) {
		for _, egress := range []protocols.ProtocolID{protocols.ProtocolOpenAI, protocols.ProtocolClaude, protocols.ProtocolGemini, protocols.ProtocolResponses} {
			out, err := stripPassthroughReasoning([]byte(untouched), egress)
			if err != nil {
				t.Fatalf("%s: %v", egress, err)
			}
			if string(out) != untouched {
				t.Fatalf("%s: body = %s, want byte-identical %s", egress, out, untouched)
			}
		}
	})

	t.Run("literal null body declines", func(t *testing.T) {
		out, err := stripPassthroughReasoning([]byte("null"), protocols.ProtocolOpenAI)
		if err != nil {
			t.Fatalf("errored (%v) — the decline hands the bytes back", err)
		}
		if string(out) != "null" {
			t.Fatalf("body = %s, want the literal null untouched", out)
		}
	})

	t.Run("explicitly nulled fields read as absent", func(t *testing.T) {
		const in = `{"model":"m","reasoning_effort":null,"reasoning":null,"messages":[]}`
		out, err := stripPassthroughReasoning([]byte(in), protocols.ProtocolOpenAI)
		if err != nil {
			t.Fatalf("errored (%v)", err)
		}
		if string(out) != in {
			t.Fatalf("body = %s, want the nulled fields left untouched: %s", out, in)
		}
	})

	t.Run("gemini non-object generationConfig declines", func(t *testing.T) {
		const in = `{"contents":[],"generationConfig":"opaque"}`
		out, err := stripPassthroughReasoning([]byte(in), protocols.ProtocolGemini)
		if err != nil {
			t.Fatalf("errored (%v) — the decline hands the bytes back", err)
		}
		if string(out) != in {
			t.Fatalf("body = %s, want the caller's bytes untouched: %s", out, in)
		}
	})

	t.Run("gemini empty generationConfig has nothing to strip", func(t *testing.T) {
		const in = `{"contents":[],"generationConfig":{}}`
		out, err := stripPassthroughReasoning([]byte(in), protocols.ProtocolGemini)
		if err != nil {
			t.Fatalf("errored (%v)", err)
		}
		if string(out) != in {
			t.Fatalf("body = %s, want byte-identical %s", out, in)
		}
	})

	t.Run("openai strip removes both spellings", func(t *testing.T) {
		out, err := stripPassthroughReasoning([]byte(`{"model":"m","reasoning_effort":"high","reasoning":{"budget_tokens":1},"messages":[]}`), protocols.ProtocolOpenAI)
		if err != nil {
			t.Fatalf("errored (%v)", err)
		}
		sent := decodeBody(t, out)
		for _, key := range []string{"reasoning_effort", "reasoning"} {
			if _, ok := sent[key]; ok {
				t.Fatalf("%s survived the strip; body = %s", key, out)
			}
		}
	})

	t.Run("responses strip removes reasoning", func(t *testing.T) {
		out, err := stripPassthroughReasoning([]byte(`{"model":"m","reasoning":{"effort":"high"},"input":"hi"}`), protocols.ProtocolResponses)
		if err != nil {
			t.Fatalf("errored (%v)", err)
		}
		if _, ok := decodeBody(t, out)["reasoning"]; ok {
			t.Fatalf("reasoning survived the strip; body = %s", out)
		}
	})
}
