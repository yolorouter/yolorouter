package gateway

// Door-level tests for the video modality: the create call's parse and
// the refusals no candidate could have changed. Everything past the door
// (delivery, settlement) belongs to the provider-adaptor ticket; here the
// contract is what the caller is told before any upstream exists.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
)

func admitVideo(t *testing.T, contentType string, body []byte) (Payload, *Rejection) {
	t.Helper()
	return videoModality{}.Admit(t.Context(), Ingress{
		Protocol:    "videos",
		Path:        videos.CreatePath,
		ContentType: contentType,
		Body:        body,
	})
}

func videoJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestVideoAdmitJSONParsesAndDefaults(t *testing.T) {
	payload, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
		"model": "sora-2", "prompt": "a calico cat at a piano",
	}))
	if rej != nil {
		t.Fatalf("valid create refused: %+v", rej)
	}
	routing := payload.Routing()
	if routing.Model != "sora-2" || routing.Stream {
		t.Fatalf("routing not read from the parsed request: %+v", routing)
	}
}

func TestVideoAdmitMultipartParses(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "sora-2")
	_ = mw.WriteField("prompt", "piano cat")
	_ = mw.WriteField("seconds", "8")
	fw, _ := mw.CreateFormFile("input_reference", "cat.png")
	_, _ = fw.Write([]byte("png-bytes"))
	_ = mw.Close()

	payload, rej := admitVideo(t, mw.FormDataContentType(), buf.Bytes())
	if rej != nil {
		t.Fatalf("valid multipart create refused: %+v", rej)
	}
	if payload.Routing().Model != "sora-2" {
		t.Fatalf("model not routed: %+v", payload.Routing())
	}
}

func TestVideoAdmitDoorRefusals(t *testing.T) {
	cases := []struct {
		name, failReason string
		contentType      string
		body             []byte
	}{
		{
			name:        "missing model",
			failReason:  "empty_model",
			contentType: "application/json",
			body:        videoJSON(t, map[string]any{"prompt": "p"}),
		},
		{
			name:        "missing prompt",
			failReason:  "empty_prompt",
			contentType: "application/json",
			body:        videoJSON(t, map[string]any{"model": "m"}),
		},
		{
			name:        "seconds outside the vocabulary",
			failReason:  "invalid_seconds",
			contentType: "application/json",
			body:        videoJSON(t, map[string]any{"model": "m", "prompt": "p", "seconds": 5}),
		},
		{
			name:        "size outside the vocabulary",
			failReason:  "invalid_size",
			contentType: "application/json",
			body:        videoJSON(t, map[string]any{"model": "m", "prompt": "p", "size": "1080x1920"}),
		},
		{
			name:        "file_id reference",
			failReason:  "input_reference_file_id_unsupported",
			contentType: "application/json",
			body:        videoJSON(t, map[string]any{"model": "m", "prompt": "p", "input_reference": map[string]any{"file_id": "file-123"}}),
		},
		{
			name:        "unparseable body",
			failReason:  "parse:",
			contentType: "application/json",
			body:        []byte("{not json"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rej := admitVideo(t, tc.contentType, tc.body)
			if rej == nil {
				t.Fatal("expected a door refusal")
			}
			if rej.Status != http.StatusBadRequest {
				t.Fatalf("door refusals are 400s, got %d", rej.Status)
			}
			if !strings.HasPrefix(rej.FailReason, tc.failReason) {
				t.Fatalf("fail reason %q does not start with %q", rej.FailReason, tc.failReason)
			}
		})
	}
}

func TestVideoAdmitAcceptsVocabularyEdges(t *testing.T) {
	// Every legal seconds/size spelling, plus both reference shapes,
	// must clear the door: the door judges the vocabulary, it does not
	// narrow it.
	for _, seconds := range []int{4, 8, 12} {
		_, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
			"model": "m", "prompt": "p", "seconds": seconds,
		}))
		if rej != nil {
			t.Fatalf("seconds %d refused: %+v", seconds, rej)
		}
	}
	for _, size := range []string{"720x1280", "1280x720", "1024x1792", "1792x1024"} {
		_, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
			"model": "m", "prompt": "p", "size": size,
		}))
		if rej != nil {
			t.Fatalf("size %s refused: %+v", size, rej)
		}
	}
	_, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
		"model": "m", "prompt": "p", "input_reference": map[string]any{"image_url": "https://example.test/cat.png"},
	}))
	if rej != nil {
		t.Fatalf("image_url reference refused: %+v", rej)
	}
}

func TestVideoPayloadSupportsVerdicts(t *testing.T) {
	payload, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{"model": "m", "prompt": "p"}))
	if rej != nil {
		t.Fatalf("admit refused: %+v", rej)
	}
	// An OpenAI-dialect candidate is refused: this build wires the wan
	// task dialect only.
	if v := payload.Supports(Candidate{ProviderModelName: "sora-2", BaseURL: "https://api.openai.com/v1"}); v.OK {
		t.Fatalf("openai-dialect candidate must be refused, got %+v", v)
	}
	// A DashScope candidate of a family the dialect does not encode is
	// refused per candidate.
	prev := isDashScopeBase
	isDashScopeBase = func(string) bool { return true }
	t.Cleanup(func() { isDashScopeBase = prev })
	if v := payload.Supports(Candidate{ProviderModelName: "qwen-video-9", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"}); v.OK {
		t.Fatalf("non-wan family must be refused, got %+v", v)
	}
	// A wan family on a DashScope base is the one shape served.
	for _, name := range []string{"wan2.7-t2v", "wan2.7-i2v", "wan3.0-video", "wan2.6-t2v", "wan2.1-t2v-plus"} {
		if v := payload.Supports(Candidate{ProviderModelName: name, BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1"}); !v.OK {
			t.Fatalf("wan family %q must be supported, got %+v", name, v)
		}
	}
}

func TestVideoSanitizeForLogRedactsReferencePixels(t *testing.T) {
	payload, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
		"model": "m", "prompt": "piano cat",
		"input_reference": map[string]any{
			"image_url": "data:image/png;base64," + strings.Repeat("A", 40),
		},
	}))
	if rej != nil {
		t.Fatalf("admit refused: %+v", rej)
	}
	rendered := payload.SanitizeForLog(BodyClientRequest, "application/json", videoJSON(t, map[string]any{
		"model": "m", "prompt": "piano cat",
		"input_reference": map[string]any{
			"image_url": "data:image/png;base64," + strings.Repeat("A", 40),
		},
	}))
	if strings.Contains(rendered, strings.Repeat("A", 40)) {
		t.Fatalf("reference pixels must not survive rendering: %s", rendered)
	}
	if !strings.Contains(rendered, "piano cat") {
		t.Fatalf("the prompt is diagnostics and must survive: %s", rendered)
	}
	if !strings.Contains(rendered, "[base64 image omitted:") {
		t.Fatalf("omission note missing: %s", rendered)
	}
}

func TestVideoSanitizeForLogRendersMultipartShape(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "m")
	_ = mw.WriteField("prompt", "piano cat")
	fw, _ := mw.CreateFormFile("input_reference", "cat.png")
	_, _ = fw.Write([]byte("png-bytes-pixel-payload"))
	_ = mw.Close()

	payload, rej := admitVideo(t, mw.FormDataContentType(), buf.Bytes())
	if rej != nil {
		t.Fatalf("multipart with only a prompt must still admit: %+v", rej)
	}
	rendered := payload.SanitizeForLog(BodyClientRequest, mw.FormDataContentType(), buf.Bytes())
	if strings.Contains(rendered, "png-bytes-pixel-payload") {
		t.Fatalf("uploaded pixels must not survive rendering: %s", rendered)
	}
	if !strings.Contains(rendered, "piano cat") || !strings.Contains(rendered, "[BINARY:") {
		t.Fatalf("shape must survive rendering: %s", rendered)
	}
}

// TestVideoAdmitNilContextSafety pins the SA1012 lesson from the images
// round: Admit must tolerate a context it never uses, and callers that
// pass a live one must see the same answers.
func TestVideoAdmitNilContextSafety(t *testing.T) {
	_, rej := videoModality{}.Admit(context.Background(), Ingress{
		ContentType: "application/json",
		Body:        videoJSON(t, map[string]any{"model": "m", "prompt": "p"}),
	})
	if rej != nil {
		t.Fatalf("valid create refused: %+v", rej)
	}
}

// stubVideoStore scripts PrecheckBudget's answer so the door's budget
// hook can be exercised without a whole task service.
type stubVideoStore struct {
	precheckErr error
}

func (s *stubVideoStore) Create(context.Context, *model.VideoTask, time.Time) error {
	return nil
}

func (s *stubVideoStore) PrecheckBudget(context.Context, uint, string, string, int) error {
	return s.precheckErr
}

func withVideoStore(t *testing.T, store videoTaskStore) {
	t.Helper()
	prev := videoTasks
	videoTasks = store
	t.Cleanup(func() { videoTasks = prev })
}

func TestVideoAdmitBudgetPrecheckRefuses(t *testing.T) {
	withVideoStore(t, &stubVideoStore{precheckErr: &videotask.BudgetExceededError{
		Limit: 1_000_000, Spent: 0, InFlight: 0, Ask: 2_800_000,
	}})
	_, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
		"model": "sora-2", "prompt": "a calico cat at a piano",
	}))
	if rej == nil {
		t.Fatalf("a certain budget overflow must be refused at the door")
	}
	if rej.Status != http.StatusTooManyRequests || rej.ErrorType != errTypeInsufficientQuota {
		t.Fatalf("the refusal must be a quota-shaped 429, got %+v", rej)
	}
	if rej.FailReason != "video_budget_exceeded" {
		t.Fatalf("the refusal must keep the budget fail reason, got %q", rej.FailReason)
	}
	if !strings.Contains(rej.Message, "limit 1000000") || !strings.Contains(rej.Message, "this task 2800000") {
		t.Fatalf("the refusal must carry the arithmetic, got %q", rej.Message)
	}
}

func TestVideoAdmitBudgetPrecheckStaysSilentWhenUnsure(t *testing.T) {
	// A precheck that cannot say — any error that is not a budget
	// refusal — must not refuse the call: the exact gate still runs in
	// Create, and turning an unsure read into a refusal would trade
	// certain orphan renders for possible ones.
	withVideoStore(t, &stubVideoStore{precheckErr: errors.New("transient read")})
	payload, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{
		"model": "sora-2", "prompt": "a calico cat at a piano",
	}))
	if rej != nil {
		t.Fatalf("an unsure precheck must not refuse, got %+v", rej)
	}
	if payload == nil {
		t.Fatalf("the payload must be admitted")
	}
}

func TestVideoPayloadKlingWhitelistVerdicts(t *testing.T) {
	payload, rej := admitVideo(t, "application/json", videoJSON(t, map[string]any{"model": "m", "prompt": "p"}))
	if rej != nil {
		t.Fatalf("admit refused: %+v", rej)
	}
	prev := isKlingBase
	isKlingBase = func(string) bool { return true }
	t.Cleanup(func() { isKlingBase = prev })
	// The model name rides in the submit path, so a name without an
	// endpoint would dial a route that does not exist — refused per
	// candidate, the same shape the wan family gate takes.
	v := payload.Supports(Candidate{ProviderModelName: "kling-2.6", BaseURL: "https://api-beijing.klingai.com"})
	if v.OK || v.Reason != klingModelUnsupported {
		t.Fatalf("a model off the endpoint list must be refused with its own reason, got %+v", v)
	}
	for _, name := range []string{"kling-3.0", "kling-3.0-turbo"} {
		if v := payload.Supports(Candidate{ProviderModelName: name, BaseURL: "https://api-beijing.klingai.com"}); !v.OK {
			t.Fatalf("endpoint-listed model %q must be supported, got %+v", name, v)
		}
	}
}
