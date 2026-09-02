package gateway

// End-to-end tests for the edits half of the image modality: a caller's
// multipart upload, routed through the real Handle path to a fake
// OpenAI-compatible upstream with only the model field rewritten, the door
// refusals the endpoint owes a caller before any upstream is walked, and
// the audit row that keeps the upload's shape without its pixels.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// editUploadPNG is the reference image the edit tests upload: a real PNG
// rendered on first use, large enough that its base64 crosses the audit
// redaction's length floor (a 1x1 chip would slip under it and prove
// nothing about the redactor). The exact bytes make "the pixels reached the
// upstream" and "the pixels stayed out of the audit row" one comparison
// away.
var editUploadPNG = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			// Both axes vary, so the PNG filters have nothing to collapse
			// and the encoded body stays well past the floor.
			img.SetRGBA(x, y, color.RGBA{R: uint8(x / 2), G: uint8(y / 2), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("edit test image: " + err.Error())
	}
	return buf.Bytes()
}()

// editRig is the edits fixture: like the generations rig, plus the
// upstream's content type, a swappable upstream answer, and a multipart
// builder for the caller side.
type editRig struct {
	svc      *Service
	db       *gorm.DB
	key      *model.APIKey
	modelID  uint
	hits     atomic.Int64
	baseURL  string
	lastPath string
	lastCT   string
	lastBody []byte
}

// newEditRig builds the fixture with the default OpenAI-shaped answer.
func newEditRig(t *testing.T) *editRig {
	t.Helper()
	return newEditRigWith(t, nil)
}

// newEditRigWith builds the fixture with a caller-chosen upstream answer;
// nil means the default OpenAI-shaped images JSON.
func newEditRigWith(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *editRig {
	t.Helper()
	rig := &editRig{}
	rig.db = testutil.NewSQLiteDB(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rig.hits.Add(1)
		rig.lastPath = r.URL.Path
		rig.lastCT = r.Header.Get("Content-Type")
		rig.lastBody, _ = io.ReadAll(r.Body)
		if answer != nil {
			answer(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(imageUpstreamBody))
	}))
	t.Cleanup(up.Close)
	rig.baseURL = up.URL
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "image-provider", up.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-image-up", "image-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "image-model", "image-model-real", false, false, 1)
	setImageOutputModalities(t, rig.db, m.ID, `["image"]`)
	rig.modelID = m.ID
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	return rig
}

// buildEditUpload builds the caller's multipart body with the reference
// image and the fields the test names.
func buildEditUpload(t *testing.T) (contentType string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for field, value := range map[string]string{
		"model":  "image-model",
		"prompt": "make the fox wear a hat",
		"n":      "1",
		"size":   "1024x1024",
	} {
		if err := w.WriteField(field, value); err != nil {
			t.Fatalf("write field %q: %v", field, err)
		}
	}
	part, err := w.CreateFormFile("image", "fox.png")
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := part.Write(editUploadPNG); err != nil {
		t.Fatalf("write image part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

// editRequest builds a caller context for the edits endpoint with the
// caller's own multipart content type.
func editRequest(contentType string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	c, w := newCtxPath("/v1/images/edits", body)
	c.Request.Header.Set("Content-Type", contentType)
	return c, w
}

// A whole edits request passes through the gateway to an OpenAI-compatible
// upstream and back: the endpoint path is the edits API, the body arrives
// as multipart with the candidate's provider model and the caller's pixels,
// and the caller receives the upstream's bytes verbatim. The audit row
// keeps the upload's rendered shape — not its pixels — and the settlement
// is the images one, on the same response shape the generations half bills.
func TestImageEditPassthroughEndToEnd(t *testing.T) {
	rig := newEditRig(t)
	ct, body := buildEditUpload(t)

	c, w := editRequest(ct, body)
	c.Set("request_id", "req-image-edit-e2e")
	c.Set(BodiesDirContextKey, t.TempDir())
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want exactly 1", rig.hits.Load())
	}
	if rig.lastPath != "/v1/images/edits" {
		t.Errorf("upstream path = %q, want the edits endpoint", rig.lastPath)
	}

	// The forwarded body must parse with the content type it was sent with:
	// the rewrite minted a fresh boundary, and the two travel together.
	mediaType, params, err := mime.ParseMediaType(rig.lastCT)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("upstream content type = %q, want multipart", rig.lastCT)
	}
	form, err := multipart.NewReader(bytes.NewReader(rig.lastBody), params["boundary"]).ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("forwarded body does not parse: %v", err)
	}
	if got := form.Value["model"]; len(got) != 1 || got[0] != "image-model-real" {
		t.Errorf("forwarded model = %v, want the candidate's provider name", got)
	}
	if got := form.Value["prompt"]; len(got) != 1 || got[0] != "make the fox wear a hat" {
		t.Errorf("forwarded prompt = %v, want the caller's untouched", got)
	}
	files := form.File["image"]
	if len(files) != 1 || files[0].Filename != "fox.png" {
		t.Fatalf("forwarded files = %+v, want the reference image", files)
	}
	f, err := files[0].Open()
	if err != nil {
		t.Fatalf("open forwarded image: %v", err)
	}
	got, _ := io.ReadAll(f)
	_ = f.Close()
	if !bytes.Equal(got, editUploadPNG) {
		t.Errorf("forwarded image bytes differ from the upload")
	}
	if w.Body.String() != imageUpstreamBody {
		t.Errorf("caller body differs from the upstream's bytes")
	}

	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-edit-e2e").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.StatusCode != http.StatusOK {
		t.Errorf("log status_code = %d, want 200", row.StatusCode)
	}
	// The seeded candidate bills in the token default, so this pins the
	// token-mode settlement on the edits half: the delivery's token
	// sub-counts, priced at the candidate's token prices.
	if !row.CostKnown || row.CostMicros != 2050 {
		t.Errorf("cost = %d known=%v, want the images token-mode settlement 2050", row.CostMicros, row.CostKnown)
	}
	if row.ImagePricingSnapshot != "" {
		t.Errorf("image_pricing_snapshot = %q on a token-mode settlement, want empty", row.ImagePricingSnapshot)
	}

	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-image-edit-e2e").First(&bodyRow).Error; err != nil {
		t.Fatalf("no body row: %v", err)
	}
	if !strings.Contains(bodyRow.RequestBody, "[BINARY:") || !strings.Contains(bodyRow.RequestBody, "make the fox wear a hat") {
		t.Errorf("audit request body lost the upload's rendered shape:\n%s", bodyRow.RequestBody)
	}
	if strings.Contains(bodyRow.RequestBody, string(editUploadPNG)) {
		t.Errorf("audit request body kept the pixel bytes")
	}
	if bodyRow.ResponseBody != imageUpstreamBody {
		t.Errorf("body row response differs from what the caller received")
	}
}

// The door refusals of the edits half: a body that is not the multipart it
// must be, a missing model or prompt, an upload with no reference image,
// and a streaming ask — each refused before any upstream is walked.
func TestImageEditAdmitRefusals(t *testing.T) {
	rig := newEditRig(t)
	ct, body := buildEditUpload(t)

	noModelCT, noModel := rebuildEditUpload(t, ct, body, "model", nil)
	noPromptCT, noPrompt := rebuildEditUpload(t, ct, body, "prompt", nil)
	noImageCT, noImage := rebuildEditUpload(t, ct, body, "\x00no-field\x00", nil, withoutFiles)
	streamCT, streamBody := rebuildEditUpload(t, ct, body, "stream", map[string]string{"stream": "true"})
	for _, tc := range []struct {
		name   string
		ct     string
		body   []byte
		reason string
	}{
		{"json body", "application/json", []byte(`{"model":"image-model","prompt":"x"}`), "parse"},
		{"no model", noModelCT, noModel, "empty_model"},
		{"no prompt", noPromptCT, noPrompt, "empty_prompt"},
		{"no image", noImageCT, noImage, "empty_image"},
		{"stream ask", streamCT, streamBody, "image_streaming_model_unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := editRequest(tc.ct, tc.body)
			c.Set("request_id", "req-image-edit-refusal-"+tc.name)
			rig.svc.Handle(c, rig.key)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if rig.hits.Load() != 0 {
				t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
			}
			// The recorded reason is what the refusal actually was — a body
			// that fails for an unintended reason (a stale boundary, say)
			// reads as "parse" here and turns the test red.
			var row model.RequestLog
			if err := rig.db.Where("request_id = ?", "req-image-edit-refusal-"+tc.name).First(&row).Error; err != nil {
				t.Fatalf("no request log row: %v", err)
			}
			if row.FailReason == nil || !strings.HasPrefix(*row.FailReason, tc.reason) {
				t.Errorf("fail reason = %v, want prefix %q", row.FailReason, tc.reason)
			}
		})
	}
}

// rebuildEditUpload rebuilds a parsed upload minus one scalar field (drop),
// plus any extra fields (extra), and optionally without its file parts. It
// returns the rebuilt body with its own content type — the new writer mints
// a fresh boundary, and sending the body under the old type would fail the
// parse for a reason the test never meant.
func rebuildEditUpload(t *testing.T, contentType string, body []byte, drop string, extra map[string]string, opts ...func(*rebuildOptions)) (string, []byte) {
	t.Helper()
	var ro rebuildOptions
	for _, o := range opts {
		o(&ro)
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		t.Fatalf("parse content type: %v (%q)", err, contentType)
	}
	form, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).ReadForm(32 << 20)
	if err != nil {
		t.Fatalf("read form: %v", err)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for name, values := range form.Value {
		if name == drop {
			continue
		}
		for _, v := range values {
			if err := w.WriteField(name, v); err != nil {
				t.Fatalf("write field: %v", err)
			}
		}
	}
	for name, v := range extra {
		if err := w.WriteField(name, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	if !ro.withoutFiles {
		part, err := w.CreateFormFile("image", "fox.png")
		if err != nil {
			t.Fatalf("create image part: %v", err)
		}
		if _, err := part.Write(editUploadPNG); err != nil {
			t.Fatalf("write image part: %v", err)
		}
		if ro.withMask {
			mask, err := w.CreateFormFile("mask", "mask.png")
			if err != nil {
				t.Fatalf("create mask part: %v", err)
			}
			if _, err := mask.Write(editUploadPNG); err != nil {
				t.Fatalf("write mask part: %v", err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

type rebuildOptions struct {
	withoutFiles bool
	withMask     bool
}

func withoutFiles(ro *rebuildOptions) { ro.withoutFiles = true }
func withMask(ro *rebuildOptions)     { ro.withMask = true }

// The edits route resolves to the images protocol — the registration fact
// that routes a multipart upload to the image modality at all.
func TestImageEditRouteResolvesToImagesProtocol(t *testing.T) {
	if got := IngressProtocol("/v1/images/edits"); got != protocols.ProtocolImages {
		t.Fatalf("IngressProtocol = %q, want images", got)
	}
}

// A DashScope candidate is refused per candidate when the caller's ask has
// no mapping in the native edit dialect — a b64_json delivery format (the
// dialect answers with URLs) or a mask upload (no field to carry it). With
// no other candidate the chain ends failed rather than silently dropping
// the field or downgrading the delivery format.
func TestImageEditDashScopeRefusesUnmappableAsksPerCandidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]string
		files bool
		mask  bool
	}{
		{"b64_json ask", map[string]string{"response_format": "b64_json"}, true, false},
		{"mask upload", nil, true, true},
		{"gpt-image-only knob", map[string]string{"input_fidelity": "high"}, true, false},
		{"another knob", map[string]string{"background": "transparent"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newEditRig(t)
			prev := isDashScopeBase
			isDashScopeBase = func(baseURL string) bool { return true }
			t.Cleanup(func() { isDashScopeBase = prev })

			ct, body := buildEditUpload(t)
			var opts []func(*rebuildOptions)
			if tc.mask {
				opts = append(opts, withMask)
			}
			ct, body = rebuildEditUpload(t, ct, body, "\x00no-field\x00", tc.extra, opts...)
			c, w := editRequest(ct, body)
			c.Set("request_id", "req-image-edit-unmappable")
			rig.svc.Handle(c, rig.key)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 from the refused chain; body = %s", w.Code, w.Body.String())
			}
			if rig.hits.Load() != 0 {
				t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
			}
		})
	}
}

// The edits half of the native dialect, end to end: the uploaded reference
// images leave as base64 data URI content items beside the instruction
// text, the dialect's answer is decoded into the OpenAI shape, and the
// settlement bills by the images the dialect actually delivered.
func TestImageEditDashScopeEndToEnd(t *testing.T) {
	rig := newEditRigWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(dashScopeTwoImages))
	})
	prev := isDashScopeBase
	isDashScopeBase = func(baseURL string) bool { return baseURL == rig.baseURL }
	t.Cleanup(func() { isDashScopeBase = prev })
	// The dialect candidate bills per image at a default price, the way a
	// qwen-image-edit mapping would be configured.
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", rig.modelID).Updates(map[string]interface{}{
		"billing_mode":        model.BillingModeImage,
		"image_pricing_tiers": `{"mode":"per_image","default_price":0.02}`,
	}).Error; err != nil {
		t.Fatalf("seed billing: %v", err)
	}

	ct, body := buildEditUpload(t)
	c, w := editRequest(ct, body)
	c.Set("request_id", "req-image-edit-dashscope-e2e")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if rig.lastPath != "/api/v1/services/aigc/multimodal-generation/generation" {
		t.Errorf("upstream path = %q, want the native multimodal-generation endpoint", rig.lastPath)
	}
	var sent struct {
		Model string `json:"model"`
		Input struct {
			Messages []struct {
				Content []struct {
					Text  string `json:"text"`
					Image string `json:"image"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"input"`
		Parameters struct {
			N    int    `json:"n"`
			Size string `json:"size"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(rig.lastBody, &sent); err != nil {
		t.Fatalf("upstream body did not parse: %v (%s)", err, rig.lastBody)
	}
	if sent.Model != "image-model-real" {
		t.Errorf("upstream model = %q, want the candidate's provider name", sent.Model)
	}
	if sent.Parameters.Size != "1024*1024" || sent.Parameters.N != 1 {
		t.Errorf("parameters = %+v, want the converted size and asked count", sent.Parameters)
	}
	items := sent.Input.Messages[0].Content
	if len(items) != 2 {
		t.Fatalf("content items = %d, want the image then the prompt", len(items))
	}
	// CreateFormFile labels the part octet-stream; the sniff upgrades it to
	// the real PNG type, which is what a curl -F upload relies on.
	if want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(editUploadPNG); items[0].Image != want {
		t.Errorf("image item = %q, want the sniffed png data URI", items[0].Image)
	}
	if items[1].Text != "make the fox wear a hat" {
		t.Errorf("prompt item = %q", items[1].Text)
	}

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

	// Billed per delivered image at the default price: 2 × 0.02, with the
	// snapshot recording the axes the caller sent.
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-edit-dashscope-e2e").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if !row.CostKnown || row.CostMicros != 40000 {
		t.Errorf("cost = known:%v %d, want known 40000 (0.02 × 2)", row.CostKnown, row.CostMicros)
	}
	if row.ImageCount != 2 || row.ImagePricingSnapshot == "" {
		t.Errorf("image settlement = count:%d snapshot:%q, want 2 and a snapshot", row.ImageCount, row.ImagePricingSnapshot)
	}
	if !strings.Contains(row.ImagePricingSnapshot, `"request_size":"1024x1024"`) {
		t.Errorf("snapshot lost the caller's original size string: %s", row.ImagePricingSnapshot)
	}

	// The native upstream request is JSON carrying the upload as a data URI;
	// the audit row keeps the request's shape with the payload redacted,
	// not the pixels.
	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-image-edit-dashscope-e2e").First(&bodyRow).Error; err != nil {
		t.Fatalf("no body row: %v", err)
	}
	if !strings.Contains(bodyRow.UpstreamRequestBody, "[base64 image omitted:") || strings.Contains(bodyRow.UpstreamRequestBody, "data:image/") {
		t.Errorf("native upstream request not redacted in the audit row: %s", bodyRow.UpstreamRequestBody)
	}
}

// A business refusal arrives with HTTP 200 and a code; it is answered as
// 422 with the refusal rendered in the OpenAI error shape, not failed over
// — the same body would be refused by any other retry — and bills nothing.
func TestImageEditDashScopeBusinessErrorIsAnsweredNotRetried(t *testing.T) {
	rig := newEditRigWith(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"request_id":"r1","code":"InvalidParameter","message":"bad image"}`))
	})
	prev := isDashScopeBase
	isDashScopeBase = func(baseURL string) bool { return baseURL == rig.baseURL }
	t.Cleanup(func() { isDashScopeBase = prev })

	ct, body := buildEditUpload(t)
	c, w := editRequest(ct, body)
	c.Set("request_id", "req-image-edit-biz")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "bad image") {
		t.Errorf("refusal body lost the upstream's message: %s", w.Body.String())
	}
	if rig.hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want exactly 1 (no failover on a business refusal)", rig.hits.Load())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-edit-biz").First(&row).Error; err != nil {
		t.Fatalf("no row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a refused request")
	}
}

// The rewritten multipart is cached per target provider model: two attempts
// at the same model re-encode once. Observed on the payload the admission
// built, the same seam PrepareUpstream serves in production.
func TestImageEditReencodeCachedPerModel(t *testing.T) {
	ct, body := buildEditUpload(t)
	payload, rej := (imageModality{}).Admit(context.TODO(), Ingress{
		Protocol: protocols.ProtocolImages, Path: "/v1/images/edits", ContentType: ct, Body: body,
	})
	if rej != nil {
		t.Fatalf("admit rejected: %+v", rej)
	}
	p := payload.(*imagePayload)

	openAI := func(model string) Candidate {
		return Candidate{EgressProtocol: protocols.ProtocolOpenAI, BaseURL: "https://api.openai.example", ProviderModelName: model}
	}
	first, err := p.PrepareUpstream(openAI("provider-a"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	again, err := p.PrepareUpstream(openAI("provider-a"))
	if err != nil {
		t.Fatalf("prepare again: %v", err)
	}
	if len(p.rewritten) != 1 {
		t.Errorf("cache holds %d entries after two attempts at one model, want 1", len(p.rewritten))
	}
	if !bytes.Equal(first.Body, again.Body) || first.ContentType != again.ContentType {
		t.Errorf("cached attempt differs from the first encode")
	}
	if first.Path != "/v1/images/edits" {
		t.Errorf("call path = %q, want the edits endpoint", first.Path)
	}

	if _, err := p.PrepareUpstream(openAI("provider-b")); err != nil {
		t.Fatalf("prepare other model: %v", err)
	}
	if len(p.rewritten) != 2 {
		t.Errorf("cache holds %d entries after a second model, want 2", len(p.rewritten))
	}
}
