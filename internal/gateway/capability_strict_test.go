package gateway

// The model-capability strict entry refusals' behavioural contract, asserted
// at the two surfaces the acceptance cases pin: the refusal decision as a
// pure table (exact inputs, no wire shape), and the mounted gate at the
// handler level — the 400 envelope the caller receives, the tri-state
// leniency (nil and true never refuse), and the switch-off control arm
// proving the same request is forwarded untouched when the site has not
// switched the rule on. Tools and reasoning are carried side by side so the
// one-answer priority (tools first) is pinned too.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"

	"gorm.io/gorm"
)

// declareModelCapability sets the model's declared tools/reasoning
// tri-state the way the admin console's capability metadata columns do;
// nil leaves the column as-is (undeclared).
func declareModelCapability(t *testing.T, db *gorm.DB, modelID uint, tools, reasoning *bool) {
	t.Helper()
	if tools != nil {
		if res := db.Exec("UPDATE models SET supports_tools = ? WHERE id = ?", *tools, modelID); res.Error != nil {
			t.Fatalf("declare supports_tools: %v", res.Error)
		}
	}
	if reasoning != nil {
		if res := db.Exec("UPDATE models SET supports_reasoning = ? WHERE id = ?", *reasoning, modelID); res.Error != nil {
			t.Fatalf("declare supports_reasoning: %v", res.Error)
		}
	}
}

// strictEnv reads the OpenAI error envelope a refusal writes.
func strictEnv(t *testing.T, body []byte) (message, errType string) {
	t.Helper()
	var env struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("refusal body is not the OpenAI error envelope: %v; body = %s", err, body)
	}
	return env.Error.Message, env.Error.Type
}

// TestCapabilityStrictDecisionTable pins the refusal decision on exact
// inputs: only an explicit false declaration plus the matching carried
// parameter plus the switch on refuses; nil and true never do; tools
// answers before reasoning when both would fire.
func TestCapabilityStrictDecisionTable(t *testing.T) {
	m := &Model{Name: "declared-model", SupportsTools: boolPtr(false), SupportsReasoning: boolPtr(false)}
	cases := []struct {
		name         string
		tools        *bool
		reasoning    *bool
		carriesTools bool
		carriesReas  bool
		ce           settings.CapabilityEnforcementSetting
		wantRefused  bool
		wantErrType  string
	}{
		{"tools: switch on, declared false, carries tools", boolPtr(false), nil, true, false,
			settings.CapabilityEnforcementSetting{ToolsStrict: true}, true, "tools_not_supported"},
		{"tools: switch off is today's behaviour", boolPtr(false), nil, true, false,
			settings.CapabilityEnforcementSetting{}, false, ""},
		{"tools: undeclared never refuses", nil, nil, true, false,
			settings.CapabilityEnforcementSetting{ToolsStrict: true}, false, ""},
		{"tools: declared true is support", boolPtr(true), nil, true, false,
			settings.CapabilityEnforcementSetting{ToolsStrict: true}, false, ""},
		{"tools: declared false but nothing carried", boolPtr(false), nil, false, false,
			settings.CapabilityEnforcementSetting{ToolsStrict: true}, false, ""},
		{"reasoning: switch on, declared false, carries reasoning", nil, boolPtr(false), false, true,
			settings.CapabilityEnforcementSetting{ReasoningStrict: true}, true, "reasoning_not_supported"},
		{"reasoning: switch off is today's behaviour", nil, boolPtr(false), false, true,
			settings.CapabilityEnforcementSetting{}, false, ""},
		{"reasoning: undeclared never refuses", nil, nil, false, true,
			settings.CapabilityEnforcementSetting{ReasoningStrict: true}, false, ""},
		{"reasoning: declared true is support", nil, boolPtr(true), false, true,
			settings.CapabilityEnforcementSetting{ReasoningStrict: true}, false, ""},
		{"reasoning: declared false but nothing carried", nil, boolPtr(false), false, false,
			settings.CapabilityEnforcementSetting{ReasoningStrict: true}, false, ""},
		{"both violated: tools answers first", boolPtr(false), boolPtr(false), true, true,
			settings.CapabilityEnforcementSetting{ToolsStrict: true, ReasoningStrict: true}, true, "tools_not_supported"},
		{"both violated, tools undeclared: reasoning answers", nil, boolPtr(false), true, true,
			settings.CapabilityEnforcementSetting{ToolsStrict: true, ReasoningStrict: true}, true, "reasoning_not_supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mm := &Model{Name: m.Name, SupportsTools: tc.tools, SupportsReasoning: tc.reasoning}
			rej := capabilityStrictRejection(mm, tc.carriesTools, tc.carriesReas, tc.ce)
			if !tc.wantRefused {
				if rej != nil {
					t.Fatalf("refusal = %+v, want none", rej)
				}
				return
			}
			if rej == nil {
				t.Fatal("refusal = nil, want one")
			}
			if rej.Status != http.StatusBadRequest || rej.ErrorType != tc.wantErrType {
				t.Fatalf("refusal = (%d, %s), want (400, %s)", rej.Status, rej.ErrorType, tc.wantErrType)
			}
		})
	}
	if rej := capabilityStrictRejection(nil, true, true, settings.CapabilityEnforcementSetting{ToolsStrict: true}); rej != nil {
		t.Fatalf("nil model must read as nothing declared, got %+v", rej)
	}
}

// TestToolsStrictEntryArms walks the mounted tools gate through its arms at
// the handler level: the refusal is a 400 tools_not_supported naming the
// model, the upstream is never contacted, and the two pass arms (nil
// declaration, switch off) forward the same tools payload untouched — the
// control proving the refusal arms are the switch's doing, not the body's.
func TestToolsStrictEntryArms(t *testing.T) {
	cases := []struct {
		name        string
		ce          settings.CapabilityEnforcementSetting
		declared    *bool // nil = undeclared
		body        string
		wantStatus  int
		wantErrType string
		wantUp      bool // upstream should be reached
		wantTools   bool // egress body carries the tools array
	}{
		{
			name:        "switch on, declared false, tools carried: refused",
			ce:          settings.CapabilityEnforcementSetting{ToolsStrict: true},
			declared:    boolPtr(false),
			body:        `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
			wantStatus:  http.StatusBadRequest,
			wantErrType: "tools_not_supported",
		},
		{
			name:       "undeclared passes the tools through",
			ce:         settings.CapabilityEnforcementSetting{ToolsStrict: true},
			declared:   nil,
			body:       `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
			wantStatus: http.StatusOK, wantUp: true, wantTools: true,
		},
		{
			name:       "switch off forwards the same payload (control)",
			ce:         settings.CapabilityEnforcementSetting{},
			declared:   boolPtr(false),
			body:       `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`,
			wantStatus: http.StatusOK, wantUp: true, wantTools: true,
		},
		{
			name:       "an empty tools array carries nothing to refuse",
			ce:         settings.CapabilityEnforcementSetting{ToolsStrict: true},
			declared:   boolPtr(false),
			body:       `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"tools":[]}`,
			wantStatus: http.StatusOK, wantUp: true, wantTools: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			var upstreamBody []byte
			var upstreamCalls int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&upstreamCalls, 1)
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
			}))
			defer upstream.Close()

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: tc.ce})
			p := createProvider(t, db, "p1", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			declareModelCapability(t, db, m.ID, tc.declared, nil)
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtx([]byte(tc.body))
			svc.Handle(c, apiKey)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantErrType != "" {
				message, errType := strictEnv(t, w.Body.Bytes())
				if errType != tc.wantErrType {
					t.Errorf("error.type = %q, want %q", errType, tc.wantErrType)
				}
				if !strings.Contains(message, `"gpt-4o"`) {
					t.Errorf("error.message %q does not name the model", message)
				}
				if got := atomic.LoadInt32(&upstreamCalls); got != 0 {
					t.Errorf("upstream was contacted %d times for a request the gate refuses", got)
				}
				return
			}
			if !tc.wantUp {
				t.Fatal("arm expected no upstream call but was not a refusal")
			}
			var sent map[string]json.RawMessage
			if err := json.Unmarshal(upstreamBody, &sent); err != nil {
				t.Fatalf("egress body is not JSON: %v; body = %s", err, upstreamBody)
			}
			rawTools, has := sent["tools"]
			var toolCount int
			if has && string(rawTools) != "null" {
				var arr []json.RawMessage
				if err := json.Unmarshal(rawTools, &arr); err != nil {
					t.Fatalf("egress tools is not an array: %v; body = %s", err, upstreamBody)
				}
				toolCount = len(arr)
			}
			if hasTools := toolCount > 0; hasTools != tc.wantTools {
				t.Fatalf("egress body carries %d tools, wantTools = %v; body = %s", toolCount, tc.wantTools, upstreamBody)
			}
		})
	}
}

// TestReasoningStrictEntryArms carries the same mounted-gate arms over to
// the reasoning switch, on both wire spellings an OpenAI Chat caller can
// use (reasoning_effort and the reasoning object).
func TestReasoningStrictEntryArms(t *testing.T) {
	cases := []struct {
		name        string
		ce          settings.CapabilityEnforcementSetting
		declared    *bool
		body        string
		wantStatus  int
		wantErrType string
	}{
		{
			name:        "switch on, declared false, reasoning_effort: refused",
			ce:          settings.CapabilityEnforcementSetting{ReasoningStrict: true},
			declared:    boolPtr(false),
			body:        `{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:  http.StatusBadRequest,
			wantErrType: "reasoning_not_supported",
		},
		{
			name:        "switch on, declared false, reasoning object: refused",
			ce:          settings.CapabilityEnforcementSetting{ReasoningStrict: true},
			declared:    boolPtr(false),
			body:        `{"model":"gpt-4o","max_tokens":2048,"reasoning":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus:  http.StatusBadRequest,
			wantErrType: "reasoning_not_supported",
		},
		{
			name:       "undeclared passes",
			ce:         settings.CapabilityEnforcementSetting{ReasoningStrict: true},
			declared:   nil,
			body:       `{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "declared true is support",
			ce:         settings.CapabilityEnforcementSetting{ReasoningStrict: true},
			declared:   boolPtr(true),
			body:       `{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "switch off is today's behaviour (control)",
			ce:         settings.CapabilityEnforcementSetting{},
			declared:   boolPtr(false),
			body:       `{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
			}))
			defer upstream.Close()

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: tc.ce})
			p := createProvider(t, db, "p1", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			declareModelCapability(t, db, m.ID, nil, tc.declared)
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtx([]byte(tc.body))
			svc.Handle(c, apiKey)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantErrType != "" {
				message, errType := strictEnv(t, w.Body.Bytes())
				if errType != tc.wantErrType {
					t.Errorf("error.type = %q, want %q", errType, tc.wantErrType)
				}
				if !strings.Contains(message, `"gpt-4o"`) {
					t.Errorf("error.message %q does not name the model", message)
				}
			}
		})
	}
}

// TestReasoningStrictNativeIngressArms proves the entry peek speaks each
// ingress protocol's own reasoning dialect: a Claude caller's thinking
// object, a Gemini caller's nested thinkingConfig, and a Responses
// caller's reasoning object all read as carried, and the refusal comes
// back in the caller's own error envelope shape.
func TestReasoningStrictNativeIngressArms(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		body       string
		wantStatus int
	}{
		{
			name:       "claude thinking object is carried",
			path:       "/v1/messages",
			body:       `{"model":"gpt-4o","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "claude without thinking passes",
			path:       "/v1/messages",
			body:       `{"model":"gpt-4o","max_tokens":2048,"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "gemini nested thinkingConfig is carried",
			path:       "/v1beta/models/gpt-4o:generateContent",
			body:       `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":1024}}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "gemini without thinkingConfig passes",
			path:       "/v1beta/models/gpt-4o:generateContent",
			body:       `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":0.7}}`,
			wantStatus: http.StatusOK,
		},
		{
			name:       "responses reasoning object is carried",
			path:       "/v1/responses",
			body:       `{"model":"gpt-4o","input":"hi","reasoning":{"effort":"high"}}`,
			wantStatus: http.StatusBadRequest,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				// A generic 2xx JSON body; the pass arms only assert the
				// status — the refusal arms never reach the upstream.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","object":"response","created_at":1,"status":"completed","model":"gpt-4o"}`))
			}))
			defer upstream.Close()

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: settings.CapabilityEnforcementSetting{ReasoningStrict: true}})
			p := createResponsesProvider(t, db, "p1", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			declareModelCapability(t, db, m.ID, nil, boolPtr(false))
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtxPath(tc.path, []byte(tc.body))
			svc.Handle(c, apiKey)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			// Each native dialect shapes the refusal its own way — Claude maps
			// the errType through its own vocabulary and Gemini classifies by
			// status alone — so what every envelope carries verbatim is the
			// message naming the declaration. The OpenAI envelope's errType
			// fidelity is asserted by the arms above.
			if tc.wantStatus == http.StatusBadRequest && !strings.Contains(w.Body.String(), "is declared not to support reasoning") {
				t.Fatalf("refusal body does not carry the declaration-naming message: %s", w.Body.String())
			}
		})
	}
}

// TestStrictLeavesNonReasoningTopologyUncovered pins the boundary between
// the two rules: with only the strict switch on and the addressed model
// declared false, the 400 comes at the door; with only the clear switch on,
// the same request is NOT refused (the clear is not a gate) — its landing
// behaviour is reasoning_clear_test.go's contract. This arm exists so a
// future regression that makes the clear refuse is caught as a change to
// this boundary, not absorbed silently.
func TestStrictLeavesNonReasoningTopologyUncovered(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	}))
	defer upstream.Close()

	svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: settings.CapabilityEnforcementSetting{ReasoningEgressClear: true}})
	p := createProvider(t, db, "p1", upstream.URL)
	createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
	m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
	declareModelCapability(t, db, m.ID, nil, boolPtr(false))
	apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

	c, w := newCtx([]byte(`{"model":"gpt-4o","max_tokens":2048,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`))
	svc.Handle(c, apiKey)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the clear switch alone must not refuse at the door; body = %s", w.Code, w.Body.String())
	}
}
