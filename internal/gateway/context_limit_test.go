package gateway

// The capability context-limit pre-gate's behavioural contract: the
// judgement's boundary arithmetic (pure function), the mounted gate's
// refusal envelope and tolerance arms (handler level, estimator injected
// so the boundaries are exact numbers rather than engineered bodies), and
// the phase contract — the estimate reads the FINAL body, the one the
// ingress rewriters produced, not the bytes the caller sent.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/compress"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/internal/testutil"

	"gorm.io/gorm"
)

// overrideInputEstimator swaps the estimator seam for the duration of one
// test and restores the production reading afterwards.
func overrideInputEstimator(t *testing.T, f func(body []byte) int) {
	t.Helper()
	prev := estimateInputTokens
	estimateInputTokens = f
	t.Cleanup(func() { estimateInputTokens = prev })
}

// blockOn builds the settings snapshot of a site that switched the
// context-limit block on, with the given margin percent.
func blockOn(marginPercent int) settings.CapabilityEnforcementSetting {
	return settings.CapabilityEnforcementSetting{
		ContextLimitBlock:    true,
		ContextMarginPercent: marginPercent,
	}
}

// declareContextWindow sets the model's declared context window the way
// the admin console's capability metadata column does.
func declareContextWindow(t *testing.T, db *gorm.DB, modelID uint, window int) {
	t.Helper()
	if res := db.Exec("UPDATE models SET context_length = ? WHERE id = ?", window, modelID); res.Error != nil {
		t.Fatalf("declare context window: %v", res.Error)
	}
}

// TestContextLimitJudgementBoundary pins the refusal arithmetic on exact
// numbers: the tolerance band (window, window*(1+margin)] passes, only a
// strictly larger estimate is refused, and the margin genuinely moves the
// boundary. Integer math, so every case is exact by construction.
func TestContextLimitJudgementBoundary(t *testing.T) {
	cases := []struct {
		name          string
		estimated     int
		window        int
		marginPercent int
		want          bool
	}{
		{"exactly at the window passes", 1000, 1000, 5, false},
		{"band edge window*1.05 passes", 1050, 1000, 5, false},
		{"one past the band edge refuses", 1051, 1000, 5, true},
		{"far past refuses", 2000, 1000, 5, true},
		{"zero estimate passes", 0, 1000, 5, false},
		{"margin 10 widens past the 5% edge", 1051, 1000, 10, false},
		{"margin 10 band edge passes", 1100, 1000, 10, false},
		{"margin 10 one past refuses", 1101, 1000, 10, true},
		{"margin 0 refuses one past the window", 1001, 1000, 0, true},
		{"margin 0 at the window passes", 1000, 1000, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextLimitExceeded(tc.estimated, tc.window, tc.marginPercent); got != tc.want {
				t.Fatalf("contextLimitExceeded(%d, %d, %d) = %v, want %v",
					tc.estimated, tc.window, tc.marginPercent, got, tc.want)
			}
		})
	}
}

// TestContextLimitBlockBoundaryArms walks the mounted gate through its
// tolerance arms with the estimator pinned to exact numbers: inside the
// window passes, the band passes, past the threshold is refused with the
// 413 envelope naming the model and the window, an undeclared window never
// blocks, a wider margin lets a boundary request through, and the switch
// off leaves the request untouched.
func TestContextLimitBlockBoundaryArms(t *testing.T) {
	window := 1000
	windowPtr := &window
	cases := []struct {
		name          string
		flag          settings.CapabilityEnforcementSetting
		declared      *int // nil = undeclared window
		estimated     int
		wantStatus    int
		wantUpstream  bool
		wantErrType   string
		wantRefused   bool
		wantInMessage []string
	}{
		{
			name: "estimate inside the window passes", flag: blockOn(5),
			declared: windowPtr, estimated: 900, wantStatus: http.StatusOK, wantUpstream: true,
		},
		{
			name: "band edge window*1.05 passes", flag: blockOn(5),
			declared: windowPtr, estimated: 1050, wantStatus: http.StatusOK, wantUpstream: true,
		},
		{
			name: "one past the band edge is refused 413", flag: blockOn(5),
			declared: windowPtr, estimated: 1051, wantStatus: http.StatusRequestEntityTooLarge,
			wantErrType: "context_length_exceeded", wantRefused: true,
			wantInMessage: []string{"gpt-4o", "1000", "1051"},
		},
		{
			name: "undeclared window never blocks", flag: blockOn(5),
			declared: nil, estimated: 100000, wantStatus: http.StatusOK, wantUpstream: true,
		},
		{
			name: "margin 10 lets the 5%-edge estimate through", flag: blockOn(10),
			declared: windowPtr, estimated: 1051, wantStatus: http.StatusOK, wantUpstream: true,
		},
		{
			name: "switch off leaves the request untouched", flag: settings.CapabilityEnforcementSetting{},
			declared: windowPtr, estimated: 100000, wantStatus: http.StatusOK, wantUpstream: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !tc.wantUpstream {
					t.Error("upstream was called for a request the gate refuses")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
			}))
			defer upstream.Close()

			overrideInputEstimator(t, func([]byte) int { return tc.estimated })

			svc := newSvcWithSettings(t, db, stubSettingsProvider{capability: tc.flag})
			p := createProvider(t, db, "p1", upstream.URL)
			createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
			m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
			if tc.declared != nil {
				declareContextWindow(t, db, m.ID, *tc.declared)
			}
			apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

			c, w := newCtx([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
			svc.Handle(c, apiKey)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantRefused {
				var env struct {
					Error struct {
						Message string `json:"message"`
						Type    string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
					t.Fatalf("refusal body is not the OpenAI error envelope: %v; body = %s", err, w.Body.String())
				}
				if env.Error.Type != tc.wantErrType {
					t.Errorf("error.type = %q, want %q", env.Error.Type, tc.wantErrType)
				}
				for _, want := range tc.wantInMessage {
					if !strings.Contains(env.Error.Message, want) {
						t.Errorf("error.message %q does not contain %q", env.Error.Message, want)
					}
				}
			}
		})
	}
}

// TestContextLimitEstimatorReadsTheRewrittenBody pins the phase contract:
// the estimate is taken on the body AFTER the ingress rewriters ran. A
// request whose original form breaks the threshold but whose compressed
// form does not is let through — while the same request with compression
// switched off is refused, so the arm cannot pass vacuously.
func TestContextLimitEstimatorReadsTheRewrittenBody(t *testing.T) {
	// The fixture: a chat request whose user message is go-test output the
	// compress engine shrinks by more than half.
	content := buildOutputContent()
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":` + jsonStr(content) + `}]}`)

	// Learn the rewritten length through the same engine the rewriter uses,
	// then declare a window whose 5%-widened threshold sits between the two
	// estimates: original/4 breaks it, rewritten/4 does not.
	compacted, res := compress.ByProtocol(t.Context(), protocols.ProtocolOpenAI, body, compress.DefaultOptions())
	if res.Skipped {
		t.Fatalf("fixture does not compress (skip reason %q) — the phase arm needs a body that shrinks", res.SkipReason)
	}
	origTokens := len(body) / 4
	compTokens := len(compacted) / 4
	if origTokens <= compTokens {
		t.Fatalf("fixture does not shrink enough: orig %d tokens, rewritten %d", origTokens, compTokens)
	}
	// A window below the midpoint of the two estimates keeps its widened
	// threshold under the original estimate; being above the rewritten
	// estimate keeps the rewritten form inside the window outright.
	window := (origTokens + compTokens) / 2
	if int64(window)*105/100 >= int64(origTokens) || window < compTokens {
		t.Fatalf("window %d does not separate the estimates (orig %d, rewritten %d)", window, origTokens, compTokens)
	}

	run := func(t *testing.T, compressOn bool) int {
		t.Helper()
		db := testutil.NewSQLiteDB(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"gpt-4o-real","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
		}))
		defer upstream.Close()

		// The production estimator reading stays in place: the arm's whole
		// point is that the DEFAULT chars/4 reading of the REWRITTEN body
		// decides. The override only observes which bytes the gate held.
		var sawLen int
		overrideInputEstimator(t, func(b []byte) int {
			sawLen = len(b)
			return defaultEstimateInputTokens(b)
		})

		svc := newSvcWithSettings(t, db, stubSettingsProvider{
			compressEnabled: compressOn,
			capability:      blockOn(5),
		})
		p := createProvider(t, db, "p1", upstream.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-1", "k1", 1, true)
		m := createModelAndCandidate(t, db, p, "gpt-4o", "gpt-4o-real", true, true, 1)
		declareContextWindow(t, db, m.ID, window)
		apiKey := createAPIKey(t, db, APIKeyStatusActive, []uint{m.ID})

		c, w := newCtx(body)
		svc.Handle(c, apiKey)

		if compressOn && sawLen == 0 {
			t.Fatal("estimator was never handed a body")
		}
		if compressOn && sawLen >= len(body) {
			t.Fatalf("estimator saw %d bytes, not the rewritten body (%d original) — the gate reads the wrong phase", sawLen, len(body))
		}
		return w.Code
	}

	t.Run("compressed under the threshold passes", func(t *testing.T) {
		if code := run(t, true); code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the estimate must read the rewritten body", code)
		}
	})
	t.Run("same request uncompressed is refused", func(t *testing.T) {
		if code := run(t, false); code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413 — the control arm must show the original form breaks the threshold", code)
		}
	})
}
