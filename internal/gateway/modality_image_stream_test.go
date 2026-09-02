package gateway

// End-to-end tests for the streaming half of the image modality: a
// gpt-image generation asked with stream=true is served as named-event SSE,
// forwarded verbatim while the pump reads it for the count that bills and
// the usage the completed event carries; a stream that breaks — by the
// upstream's own error event, or by delivering no image at all — bills
// nothing. A streaming ask for any other model family is refused at the
// door, and a streaming candidate the dialect cannot serve is refused per
// candidate.

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
)

// The SSE frames a gpt-image upstream sends for one whole image: a partial,
// the completed event with usage, and the terminator.
const imageStreamFrames = "event: image_generation.partial_image\n" +
	`data: {"type":"image_generation.partial_image","b64_json":"cGFydGlhbA=="}` + "\n\n" +
	"event: image_generation.completed\n" +
	`data: {"type":"image_generation.completed","usage":{"input_tokens":10,"output_tokens":1020,"total_tokens":1030}}` + "\n\n" +
	"data: [DONE]\n\n"

// newGptImageRig seeds the image rig with a gpt-image model whose candidate
// bills per image, the way a gpt-image-1 mapping would be configured.
func newGptImageRig(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *imageRig {
	t.Helper()
	rig := newImageRigWith(t, answer)
	m := createModelAndCandidate(t, rig.db, rig.provider, "gpt-image-1", "gpt-image-1", false, false, 2)
	setImageOutputModalities(t, rig.db, m.ID, `["image"]`)
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Updates(map[string]interface{}{
		"billing_mode":        model.BillingModeImage,
		"image_pricing_tiers": `{"mode":"per_image","default_price":0.02}`,
	}).Error; err != nil {
		t.Fatalf("seed billing: %v", err)
	}
	// The rig's key reaches the streaming model through one more grant; a
	// second key would collide on the helper's fixed hash.
	if err := rig.db.Create(&model.APIKeyModel{APIKeyID: rig.key.ID, ModelID: m.ID, CreatedAt: time.Now().UTC()}).Error; err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}
	return rig
}

// A whole stream passes through verbatim — same lines, event-stream content
// type — and settles by the images that completed: per-image billing at the
// candidate's price, the usage from the completed event, the row marked as
// a stream, and no capture file, because the policy drops the streamed
// response halves.
func TestImageStreamGenerationEndToEnd(t *testing.T) {
	rig := newGptImageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(imageStreamFrames))
	})

	c, w := imageRequest(`{"model":"gpt-image-1","prompt":"a red fox","stream":true,"size":"1024x1024"}`)
	c.Set("request_id", "req-image-stream-e2e")
	c.Set(BodiesDirContextKey, t.TempDir())
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("caller content type = %q, want text/event-stream", got)
	}
	if w.Body.String() != imageStreamFrames {
		t.Errorf("caller stream differs from the upstream's frames verbatim:\n%s", w.Body.String())
	}
	if rig.lastPath != "/v1/images/generations" {
		t.Errorf("upstream path = %q, want the images endpoint", rig.lastPath)
	}
	// The caller's stream=true reached the upstream inside the forwarded
	// body — the streaming ask is a passthrough field, not a knob the
	// gateway re-encodes.
	if !strings.Contains(string(rig.lastBody), `"stream":true`) {
		t.Errorf("forwarded body lost the stream field: %s", rig.lastBody)
	}

	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-stream-e2e").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if !row.IsStream {
		t.Errorf("is_stream = false, want the row to say the delivery streamed")
	}
	if !row.CostKnown || row.CostMicros != 20000 {
		t.Errorf("cost = known:%v %d, want known 20000 (0.02 × 1 completed image)", row.CostKnown, row.CostMicros)
	}
	if row.ImageCount != 1 || row.ImagePricingSnapshot == "" {
		t.Errorf("settlement = count:%d snapshot:%q, want 1 and a snapshot", row.ImageCount, row.ImagePricingSnapshot)
	}
	// The token sub-counts the completed event carried are the row's: a
	// token-billed image model would settle by them.
	if row.InputTokens != 10 || row.OutputTokens != 1020 {
		t.Errorf("token sub-counts = %d/%d, want the completed event's 10/1020", row.InputTokens, row.OutputTokens)
	}
	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-image-stream-e2e").First(&bodyRow).Error; err != nil {
		t.Fatalf("no body row: %v", err)
	}
	if bodyRow.StreamBodyPath != "" {
		t.Errorf("stream_body_path = %q, want no capture file for a dropped response policy", bodyRow.StreamBodyPath)
	}
}

// An upstream that says so itself — an error event after partial frames —
// settles as an upstream fault with nothing billed: the caller already hold
// the error event verbatim, and the status is spent by the stream's first
// frame.
func TestImageStreamErrorEventBillsNothing(t *testing.T) {
	rig := newGptImageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_generation.partial_image\n" +
			`data: {"type":"image_generation.partial_image","b64_json":"cGFydGlhbA=="}` + "\n\n" +
			"event: error\n" +
			`data: {"type":"error","message":"stream disconnected"}` + "\n\n"))
	})

	c, w := imageRequest(`{"model":"gpt-image-1","prompt":"a red fox","stream":true}`)
	c.Set("request_id", "req-image-stream-error")
	rig.svc.Handle(c, rig.key)

	if !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("caller body = %s, want the upstream's error event verbatim", w.Body.String())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-stream-error").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a stream the upstream broke")
	}
	if row.FailReason == nil || *row.FailReason != "image_stream_error_event" {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("fail_reason = %q, want image_stream_error_event", got)
	}
}

// A stream that ends cleanly but never completes an image is not a
// delivery: nothing bills, and the row says why.
func TestImageStreamNoImagesBillsNothing(t *testing.T) {
	rig := newGptImageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_generation.partial_image\n" +
			`data: {"type":"image_generation.partial_image","b64_json":"cGFydGlhbA=="}` + "\n\n"))
	})

	c, _ := imageRequest(`{"model":"gpt-image-1","prompt":"a red fox","stream":true}`)
	c.Set("request_id", "req-image-stream-none")
	rig.svc.Handle(c, rig.key)

	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-stream-none").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a stream that completed no image")
	}
	if row.FailReason == nil || *row.FailReason != "image_stream_no_images" {
		t.Errorf("fail_reason = %v, want image_stream_no_images", row.FailReason)
	}
}

// A candidate the dialect cannot stream to — a DashScope base under a
// gpt-image name — is refused per candidate; with no other candidate the
// chain ends failed rather than forwarding an SSE ask to a JSON dialect.
func TestImageStreamDashScopeCandidateRefused(t *testing.T) {
	rig := newGptImageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the dialect candidate must be refused before dispatch")
	})
	prev := isDashScopeBase
	isDashScopeBase = func(baseURL string) bool { return true }
	t.Cleanup(func() { isDashScopeBase = prev })

	c, w := imageRequest(`{"model":"gpt-image-1","prompt":"a red fox","stream":true}`)
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 from the refused chain; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
	}
}

// The door repeats the family check the per-candidate verdict makes, against
// the name the caller actually typed: a streaming ask for anything outside
// the gpt-image family is a 400 before any upstream is walked.
func TestImageStreamNonGptImageModelRefusedAtDoor(t *testing.T) {
	rig := newGptImageRig(t, nil)

	// The door check runs on the caller's own name, so a non-family alias
	// is refused before the candidate mapping could disagree with it.
	c, w := imageRequest(`{"model":"other-image-model","prompt":"a red fox","stream":true}`)
	c.Set("request_id", "req-image-stream-door")
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if rig.hits.Load() != 0 {
		t.Errorf("upstream hits = %d, want 0", rig.hits.Load())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-stream-door").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.FailReason == nil || *row.FailReason != "image_streaming_model_unsupported" {
		t.Errorf("fail_reason = %v, want image_streaming_model_unsupported", row.FailReason)
	}
}

// The edits half of the streaming family: image_edit.* events pass through
// verbatim, the completed event settles the bill, and the audit row keeps
// the upload's rendered shape — not its pixels — even on a streamed ask.
func TestImageStreamEditEndToEnd(t *testing.T) {
	frames := "event: image_edit.partial_image\n" +
		`data: {"type":"image_edit.partial_image","b64_json":"cGFydGlhbA=="}` + "\n\n" +
		"event: image_edit.completed\n" +
		`data: {"type":"image_edit.completed","usage":{"input_tokens":12,"output_tokens":2048,"total_tokens":2060}}` + "\n\n" +
		"data: [DONE]\n\n"
	rig := newGptImageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(frames))
	})

	ct, body := buildEditUpload(t)
	streamCT, streamBody := rebuildEditUpload(t, ct, body, "model", map[string]string{"model": "gpt-image-1", "stream": "true"})
	c, w := editRequest(streamCT, streamBody)
	c.Set("request_id", "req-image-edit-stream-e2e")
	c.Set(BodiesDirContextKey, t.TempDir())
	rig.svc.Handle(c, rig.key)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("caller content type = %q, want text/event-stream", got)
	}
	if w.Body.String() != frames {
		t.Errorf("caller stream differs from the upstream's frames verbatim:\n%s", w.Body.String())
	}
	if rig.lastPath != "/v1/images/edits" {
		t.Errorf("upstream path = %q, want the edits endpoint", rig.lastPath)
	}

	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-edit-stream-e2e").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if !row.IsStream {
		t.Errorf("is_stream = false, want the row to say the delivery streamed")
	}
	if !row.CostKnown || row.CostMicros != 20000 {
		t.Errorf("cost = known:%v %d, want known 20000 (0.02 × 1 completed image)", row.CostKnown, row.CostMicros)
	}
	if row.InputTokens != 12 || row.OutputTokens != 2048 {
		t.Errorf("token sub-counts = %d/%d, want the completed event's 12/2048", row.InputTokens, row.OutputTokens)
	}
	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", "req-image-edit-stream-e2e").First(&bodyRow).Error; err != nil {
		t.Fatalf("no body row: %v", err)
	}
	if strings.Contains(bodyRow.RequestBody, string(editUploadPNG)) || !strings.Contains(bodyRow.RequestBody, "make the fox wear a hat") {
		t.Errorf("audit request body lost the rendered shape:\n%s", bodyRow.RequestBody)
	}
	if bodyRow.StreamBodyPath != "" {
		t.Errorf("stream_body_path = %q, want no capture file for a dropped response policy", bodyRow.StreamBodyPath)
	}
}

// A stream whose read breaks mid-body — the upstream hard-cuts the
// connection after committing frames — settles as an upstream fault with
// nothing billed, even though a completed event had already arrived: the
// caller cannot be charged per image for a stream whose end they never saw.
func TestImageStreamBrokenReadBillsNothing(t *testing.T) {
	rig := newGptImageRig(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = w.Write([]byte("event: image_generation.completed\n" +
			`data: {"type":"image_generation.completed","usage":{"input_tokens":10,"output_tokens":1020,"total_tokens":1030}}` + "\n\n"))
		flusher.Flush()
		// Hard-cut with an unread send buffer: a lingering close sends RST,
		// so the reader sees a transport error rather than a clean EOF that
		// would end the stream as if the upstream had finished.
		hj := w.(http.Hijacker)
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetLinger(0)
		}
		_ = conn.Close()
	})

	c, _ := imageRequest(`{"model":"gpt-image-1","prompt":"a red fox","stream":true}`)
	c.Set("request_id", "req-image-stream-broken")
	rig.svc.Handle(c, rig.key)

	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-stream-broken").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on a stream whose read broke after one completed event")
	}
	if row.FailReason == nil || !strings.HasPrefix(*row.FailReason, "image_stream_read") {
		got := "<nil>"
		if row.FailReason != nil {
			got = *row.FailReason
		}
		t.Errorf("fail_reason = %q, want the broken-read account", got)
	}
}

// The edits half reports the same trio the generations half is pinned on;
// its error-event case is here — the settle path is shared, the event
// names are not.
func TestImageStreamEditErrorEventBillsNothing(t *testing.T) {
	rig := newGptImageRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: image_edit.partial_image\n" +
			`data: {"type":"image_edit.partial_image","b64_json":"cGFydGlhbA=="}` + "\n\n" +
			"event: error\n" +
			`data: {"type":"error","message":"stream disconnected"}` + "\n\n"))
	})

	ct, body := buildEditUpload(t)
	streamCT, streamBody := rebuildEditUpload(t, ct, body, "model", map[string]string{"model": "gpt-image-1", "stream": "true"})
	c, w := editRequest(streamCT, streamBody)
	c.Set("request_id", "req-image-edit-stream-error")
	rig.svc.Handle(c, rig.key)

	if !strings.Contains(w.Body.String(), `"type":"error"`) {
		t.Fatalf("caller body = %s, want the upstream's error event verbatim", w.Body.String())
	}
	var row model.RequestLog
	if err := rig.db.Where("request_id = ?", "req-image-edit-stream-error").First(&row).Error; err != nil {
		t.Fatalf("no request log row: %v", err)
	}
	if row.CostKnown {
		t.Errorf("cost_known = true on an edits stream the upstream broke")
	}
	if row.FailReason == nil || *row.FailReason != "image_stream_error_event" {
		t.Errorf("fail_reason = %v, want image_stream_error_event", row.FailReason)
	}
}
