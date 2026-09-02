package videos

// Unit pins for the DashScope half of the video dialect: family
// classification, size mapping, and the response parsers. The
// submit/poll round trips are held by the gateway's end-to-end battery;
// these are the vocabulary tables underneath them.

import (
	"strings"
	"testing"
)

func TestDashScopeModelFamily(t *testing.T) {
	cases := map[string]DashScopeFamily{
		"wan2.7-t2v":         DashScopeFamilyMedia,
		"wan2.7-i2v":         DashScopeFamilyMedia,
		"wan3.0-video":       DashScopeFamilyMedia,
		"wan3.0-video-prime": DashScopeFamilyMedia,
		"wan2.6-t2v":         DashScopeFamilyLegacy,
		"wan2.5-t2v-preview": DashScopeFamilyLegacy,
		"wan2.2-t2v-plus":    DashScopeFamilyLegacy,
		"wanx2.1-i2v-turbo":  DashScopeFamilyLegacy,
		// A family the dialect has not read shape documentation for is
		// none of its business, however wan-adjacent the name looks.
		"happyhorse-1.1-t2v": DashScopeFamilyNone,
		"happyhorse-1.0-t2v": DashScopeFamilyNone,
		"qwen-video":         DashScopeFamilyNone,
		"sora-2":             DashScopeFamilyNone,
		"":                   DashScopeFamilyNone,
	}
	for name, want := range cases {
		if got := DashScopeModelFamily(name); got != want {
			t.Errorf("DashScopeModelFamily(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMapDashScopeSize(t *testing.T) {
	cases := map[string]struct{ res, ratio string }{
		"720x1280":  {"720P", "9:16"},
		"1280x720":  {"720P", "16:9"},
		"1024x1792": {"1080P", "9:16"},
		"1792x1024": {"1080P", "16:9"},
	}
	for size, want := range cases {
		res, ratio, ok := MapDashScopeSize(size)
		if !ok || res != want.res || ratio != want.ratio {
			t.Errorf("MapDashScopeSize(%q) = %q,%q,%v; want %q,%q", size, res, ratio, ok, want.res, want.ratio)
		}
	}
	for _, size := range []string{"", "1080x1920", "720p"} {
		if _, _, ok := MapDashScopeSize(size); ok {
			t.Errorf("MapDashScopeSize(%q) must not map", size)
		}
	}
}

func TestEncodeDashScopeSubmitFamilyShapes(t *testing.T) {
	// The media family, text-only: input.prompt and no media array — the
	// flat shape the image-to-video and wan3.0 references document — with
	// the ratio stated because no reference image decides it.
	body, err := EncodeDashScopeSubmit(DashScopeSubmitRequest{
		Model: "wan2.7-t2v", Prompt: "p", Resolution: "720P", Ratio: "16:9", Duration: 4,
	})
	if err != nil {
		t.Fatalf("encode media: %v", err)
	}
	for _, wrong := range []string{`"messages"`, `"img_url"`, `"media"`} {
		if strings.Contains(string(body), wrong) {
			t.Fatalf("media family text-only shape wrong (%s): %s", wrong, body)
		}
	}
	if !strings.Contains(string(body), `"prompt":"p"`) || !strings.Contains(string(body), `"ratio":"16:9"`) {
		t.Fatalf("media family must carry input.prompt and the ratio: %s", body)
	}
	// The media family with a reference: a typed input.media[] entry, and
	// NO ratio — the reference image decides the aspect.
	body, err = EncodeDashScopeSubmit(DashScopeSubmitRequest{
		Model: "wan2.7-i2v", Prompt: "p", Resolution: "720P", Ratio: "16:9", Duration: 4,
		RefURL: "https://example.test/first.png",
	})
	if err != nil {
		t.Fatalf("encode media with ref: %v", err)
	}
	if !strings.Contains(string(body), `"media":[{"type":"first_frame","url":"https://example.test/first.png"}]`) {
		t.Fatalf("media family reference shape wrong: %s", body)
	}
	if strings.Contains(string(body), `"ratio"`) {
		t.Fatalf("an image-referenced submit must not state a ratio: %s", body)
	}
	// The legacy family: the flat prompt, img_url only with a reference.
	body, err = EncodeDashScopeSubmit(DashScopeSubmitRequest{
		Model: "wan2.6-i2v", Prompt: "p", Resolution: "720P", Duration: 8,
		RefURL: "data:image/png;base64,QUJD",
	})
	if err != nil {
		t.Fatalf("encode legacy: %v", err)
	}
	if !strings.Contains(string(body), `"img_url"`) || strings.Contains(string(body), `"messages"`) || strings.Contains(string(body), `"ratio"`) {
		t.Fatalf("legacy family shape wrong (img_url in, messages and ratio out): %s", body)
	}
	// A model outside the families refuses to encode — a candidate like
	// that was already refused by the verdict; this is the same rule on
	// the encode side.
	if _, err := EncodeDashScopeSubmit(DashScopeSubmitRequest{Model: "qwen-video", Prompt: "p"}); err == nil {
		t.Fatal("a non-wan model must refuse to encode")
	}
}

func TestEncodeDashScopeSubmitSniffsFileContentType(t *testing.T) {
	body, err := EncodeDashScopeSubmit(DashScopeSubmitRequest{
		Model: "wan2.6-i2v", Prompt: "p", Resolution: "720P", Duration: 4,
		RefData: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(body), "data:image/png;base64,") {
		t.Fatalf("an unlabelled PNG upload must sniff to image/png: %s", body)
	}
}

func TestParseDashScopeSubmitResponse(t *testing.T) {
	id, biz, err := ParseDashScopeSubmitResponse([]byte(`{"output":{"task_id":"t-1","task_status":"PENDING"},"request_id":"r"}`))
	if err != nil || biz != nil || id != "t-1" {
		t.Fatalf("plain acceptance: id=%q biz=%v err=%v", id, biz, err)
	}
	_, biz, err = ParseDashScopeSubmitResponse([]byte(`{"code":"InvalidApiKey","message":"bad"}`))
	if err != nil || biz == nil || biz.Code != "InvalidApiKey" {
		t.Fatalf("business refusal: biz=%v err=%v", biz, err)
	}
	if _, _, err := ParseDashScopeSubmitResponse([]byte(`{"output":{}}`)); err == nil {
		t.Fatal("a task-less acceptance must be a decode error, not a job with an empty id")
	}
}

func TestParseDashScopeTaskStatusVocabulary(t *testing.T) {
	cases := map[string]struct{ status, code string }{
		`{"output":{"task_status":"PENDING"}}`:                         {"pending", ""},
		`{"output":{"task_status":"RUNNING"}}`:                         {"processing", ""},
		`{"output":{"task_status":"SUCCEEDED","video_url":"u"}}`:       {"completed", ""},
		`{"output":{"task_status":"FAILED","code":"X","message":"m"}}`: {"failed", "X"},
		`{"output":{"task_status":"FAILED"}}`:                          {"failed", "upstream_failed"},
		`{"output":{"task_status":"CANCELED"}}`:                        {"cancelled", "task_cancelled"},
		`{"output":{"task_status":"UNKNOWN"}}`:                         {"expired", "task_expired"},
	}
	for body, want := range cases {
		obs, _, err := ParseDashScopeTaskResponse([]byte(body))
		if err != nil {
			t.Fatalf("%s: err=%v", body, err)
		}
		if obs.Status != want.status || obs.ErrorCode != want.code {
			t.Fatalf("%s: got %q/%q, want %q/%q", body, obs.Status, obs.ErrorCode, want.status, want.code)
		}
	}
	obs, _, err := ParseDashScopeTaskResponse([]byte(`{"output":{"task_status":"SUCCEEDED","video_url":"https://v"},"usage":{"duration":8}}`))
	if err != nil || obs.VideoURL != "https://v" || obs.UsageSecs != 8 {
		t.Fatalf("completion must carry url and usage: %+v err=%v", obs, err)
	}
	if _, _, err := ParseDashScopeTaskResponse([]byte(`{"output":{"task_status":"WEIRD"}}`)); err == nil {
		t.Fatal("an undocumented status word must be a decode error, never a guess")
	}
}

func TestOrigin(t *testing.T) {
	if got := Origin("https://dashscope.aliyuncs.com/compatible-mode/v1"); got != "https://dashscope.aliyuncs.com" {
		t.Fatalf("origin = %q", got)
	}
}
