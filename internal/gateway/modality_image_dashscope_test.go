package gateway

// DashScope dialect tests through the real dispatch path: the native
// request reaching the dialect's endpoint, the response decoded into the
// OpenAI shape for the caller, per-image billing counting what the dialect
// actually delivered, a business refusal answered as 422 without failover,
// and a b64_json ask the dialect cannot serve being refused per candidate.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// dashScopeRig points the image rig's provider at a fake upstream and turns
// the dialect detector on for that upstream's base URL, the way a real
// dashscope.aliyuncs.com base would read in production.
type dashScopeRig struct {
	svc      *Service
	db       *gorm.DB
	key      *model.APIKey
	lastPath string
	lastBody []byte
}

func newDashScopeRig(t *testing.T, upstream http.HandlerFunc) *dashScopeRig {
	t.Helper()
	rig := &dashScopeRig{}
	rig.db = testutil.NewSQLiteDB(t)
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	prev := isDashScopeBase
	isDashScopeBase = func(baseURL string) bool { return baseURL == up.URL }
	t.Cleanup(func() { isDashScopeBase = prev })

	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "dashscope-provider", up.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-dashscope-up", "dashscope-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "qwen-image-test", "qwen-image-plus", false, false, 1)
	setImageOutputModalities(t, rig.db, m.ID, `["image"]`)
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Updates(map[string]interface{}{
		"billing_mode":        model.BillingModeImage,
		"image_pricing_tiers": `{"mode":"per_image","default_price":0.02}`,
	}).Error; err != nil {
		t.Fatalf("seed billing: %v", err)
	}
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	return rig
}

const dashScopeTwoImages = `{"request_id":"r1","output":{"choices":[
	{"message":{"content":[{"image":"https://x.test/1.png"}]}},
	{"message":{"content":[{"type":"image","image":"https://x.test/2.png"}]}}
],"finished":true}}`

// The native request reaches the dialect's endpoint on the provider's
// origin, encoded in the dialect's shape, and the caller receives the
// decoded OpenAI shape — billed by the images the dialect actually
// delivered.
func TestDashScopeImageEndToEnd(t *testing.T) {
	var rig *dashScopeRig
	rig = newDashScopeRig(t, func(w http.ResponseWriter, r *http.Request) {
		rig.lastPath = r.URL.Path
		rig.lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dashScopeTwoImages))
	})

	c, w := imageRequest(`{"model":"qwen-image-test","prompt":"a fox","n":2,"size":"1024x1024"}`)
	c.Set("request_id", "req-dashscope-e2e")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rig.lastPath != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Errorf("upstream path = %q, want the native generation endpoint", rig.lastPath)
	}
	var sent struct {
		Model      string `json:"model"`
		Parameters struct {
			N    int    `json:"n"`
			Size string `json:"size"`
		} `json:"parameters"`
		Input struct {
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"input"`
	}
	if err := json.Unmarshal(rig.lastBody, &sent); err != nil {
		t.Fatalf("upstream body did not parse: %v (%s)", err, rig.lastBody)
	}
	if sent.Model != "qwen-image-plus" {
		t.Errorf("upstream model = %q, want the candidate's provider name", sent.Model)
	}
	if sent.Parameters.Size != "1024*1024" {
		t.Errorf("upstream size = %q, want the dialect's separator", sent.Parameters.Size)
	}
	if len(sent.Input.Messages) != 1 || sent.Input.Messages[0].Content[0].Text != "a fox" {
		t.Errorf("upstream prompt = %+v, want the caller's prompt", sent.Input.Messages)
	}

	// The caller received the OpenAI shape, decoded from the dialect's.
	var received struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &received); err != nil {
		t.Fatalf("caller body did not parse: %v (%s)", err, w.Body.String())
	}
	if len(received.Data) != 2 || received.Data[0].URL != "https://x.test/1.png" {
		t.Fatalf("caller data = %+v, want the two decoded image URLs", received.Data)
	}

	// Billed per delivered image at the default price: 2 × 0.02.
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-dashscope-e2e").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if !row.CostKnown || row.CostMicros != 40000 {
		t.Errorf("cost = known:%v %d, want known 40000 (0.02 × 2)", row.CostKnown, row.CostMicros)
	}
}

// A business refusal (HTTP 200, code non-empty) is answered as 422 with the
// dialect's own error rendered into the OpenAI shape — no failover, no bill.
func TestDashScopeBusinessErrorIsAnsweredNotRetried(t *testing.T) {
	rig := newDashScopeRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r2","output":{"choices":[]},"code":"InvalidParameter","message":"prompt contains sensitive content"}`))
	})

	c, w := imageRequest(`{"model":"qwen-image-test","prompt":"a fox"}`)
	c.Set("request_id", "req-dashscope-biz")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	var errOut map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &errOut); err != nil {
		t.Fatalf("error body did not parse: %v", err)
	}
	msg := errOut["error"].(map[string]any)["message"].(string)
	if !strings.Contains(msg, "InvalidParameter") {
		t.Errorf("error message = %q, want the dialect's code and text", msg)
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-dashscope-biz").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a refused request")
	}
}

// A b64_json ask is refused for the dialect candidate (it serves URLs only);
// with no other candidate the chain ends failed rather than silently
// downgrading the caller's delivery format.
func TestDashScopeRefusesB64JSONPerCandidate(t *testing.T) {
	rig := newDashScopeRig(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the dialect candidate must be refused before dispatch")
	})

	c, w := imageRequest(`{"model":"qwen-image-test","prompt":"a fox","response_format":"b64_json"}`)
	c.Set("request_id", "req-dashscope-b64")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from the refused chain", w.Code)
	}
}

// The separator capability converts the size on the OpenAI-compatible path
// for the model families that spell it with stars, and leaves every other
// request alone. Driven through the same egress pipeline the assembly
// registers it into.
func TestImageSizeSeparatorCapability(t *testing.T) {
	// The dashscope native path converts its own size above; this pins the
	// OpenAI-compatible path, where the capability is the only converter.
	t.Run("converts for star-spelled family", func(t *testing.T) {
		var gotBody []byte
		db := testutil.NewSQLiteDB(t)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(imageUpstreamBody))
		}))
		t.Cleanup(up.Close)
		svc := newSvc(t, db)
		p := createProvider(t, db, "image-provider", up.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
		m := createModelAndCandidate(t, db, p, "image-model", "wan2.2-image", false, false, 1)
		setImageOutputModalities(t, db, m.ID, `["image"]`)
		key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

		c, w := imageRequest(`{"model":"image-model","prompt":"a fox","size":"1024x1024"}`)
		c.Set("request_id", "req-sizeaxis-conv")
		svc.Handle(c, key)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(string(gotBody), `"1024*1024"`) {
			t.Errorf("upstream size not converted: %s", gotBody)
		}
	})

	t.Run("leaves other models alone", func(t *testing.T) {
		var gotBody []byte
		db := testutil.NewSQLiteDB(t)
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(imageUpstreamBody))
		}))
		t.Cleanup(up.Close)
		svc := newSvc(t, db)
		p := createProvider(t, db, "image-provider", up.URL)
		createProviderKey(t, db, svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
		m := createModelAndCandidate(t, db, p, "image-model", "gpt-image-2", false, false, 1)
		setImageOutputModalities(t, db, m.ID, `["image"]`)
		key := createAPIKey(t, db, model.APIKeyStatusActive, []uint{m.ID})

		c, w := imageRequest(`{"model":"image-model","prompt":"a fox","size":"1024x1024"}`)
		c.Set("request_id", "req-sizeaxis-keep")
		svc.Handle(c, key)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", w.Code, w.Body.String())
		}
		if !strings.Contains(string(gotBody), `"1024x1024"`) {
			t.Errorf("upstream size was rewritten for a model that does not need it: %s", gotBody)
		}
	})
}
