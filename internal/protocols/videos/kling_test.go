package videos

// Unit pins for the Kling half of the video dialect: base detection, the
// model whitelist, the shared size table through Kling's spelling, both
// submit shapes, and the response parsers. The submit/poll round trips
// are held by the gateway's end-to-end battery.

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestIsKlingBase(t *testing.T) {
	for _, base := range []string{
		"https://api-beijing.klingai.com",
		"https://api-singapore.klingai.com",
		"https://api.klingai.com",
	} {
		if !IsKlingBase(base) {
			t.Errorf("IsKlingBase(%q) = false, want true", base)
		}
	}
	for _, base := range []string{
		"https://ark.cn-beijing.volces.com/api/v3",
		"https://dashscope.aliyuncs.com/compatible-mode/v1",
		"https://klingai.com.evil.test",
		"https://example.com/text-to-video/kling-3.0",
	} {
		if IsKlingBase(base) {
			t.Errorf("IsKlingBase(%q) = true, want false", base)
		}
	}
}

func TestKlingModelSupported(t *testing.T) {
	for _, name := range []string{"kling-3.0", "kling-3.0-turbo"} {
		if !KlingModelSupported(name) {
			t.Errorf("KlingModelSupported(%q) = false, want true", name)
		}
	}
	// A prefix match would happily spell garbage into the submit path;
	// the whitelist must not behave like one.
	for _, name := range []string{"kling-2.6", "kling-3.0-omni", "kling-3.0 ", "kling"} {
		if KlingModelSupported(name) {
			t.Errorf("KlingModelSupported(%q) = true, want false", name)
		}
	}
}

func TestMapKlingSizeSharesTheTierTable(t *testing.T) {
	cases := map[string]struct{ res, ratio string }{
		"720x1280":  {"720p", "9:16"},
		"1280x720":  {"720p", "16:9"},
		"1024x1792": {"1080p", "9:16"},
		"1792x1024": {"1080p", "16:9"},
	}
	for size, want := range cases {
		res, ratio, ok := MapKlingSize(size)
		if !ok || res != want.res || ratio != want.ratio {
			t.Errorf("MapKlingSize(%q) = %q,%q,%v; want %q,%q", size, res, ratio, ok, want.res, want.ratio)
		}
		tier, ok := TierForSize(size)
		if !ok || tier != strings.ToUpper(want.res) {
			t.Errorf("TierForSize(%q) = %q,%v; want %q", size, tier, ok, strings.ToUpper(want.res))
		}
	}
	for _, size := range []string{"", "1080x1920"} {
		if _, _, ok := MapKlingSize(size); ok {
			t.Errorf("MapKlingSize(%q) must not map", size)
		}
	}
}

func TestEncodeKlingSubmitShapes(t *testing.T) {
	// Text-only: flat prompt, settings carries the aspect ratio, and no
	// options block exists to carry anything.
	body, err := EncodeKlingSubmit(KlingSubmitRequest{
		Model: "kling-3.0-turbo", Prompt: "p", Resolution: "720p", Ratio: "9:16", Duration: 4,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc struct {
		Prompt   string `json:"prompt"`
		Settings struct {
			Resolution  string `json:"resolution"`
			AspectRatio string `json:"aspect_ratio"`
			Duration    int    `json:"duration"`
		} `json:"settings"`
		Options any `json:"options"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode submit body: %v (%s)", err, body)
	}
	if doc.Prompt != "p" || doc.Settings.Resolution != "720p" || doc.Settings.AspectRatio != "9:16" || doc.Settings.Duration != 4 {
		t.Fatalf("text-only shape wrong: %s", body)
	}
	if doc.Options != nil {
		t.Fatalf("options must be omitted entirely (watermark defaults off): %s", body)
	}

	// Referenced by URL: contents[] carries prompt beside first_frame,
	// settings loses the aspect ratio, and the URL rides verbatim.
	body, err = EncodeKlingSubmit(KlingSubmitRequest{
		Model: "kling-3.0", Prompt: "p", Resolution: "1080p", Ratio: "16:9", Duration: 8,
		RefURL: "https://example.test/first.png",
	})
	if err != nil {
		t.Fatalf("encode with ref: %v", err)
	}
	var ref struct {
		Contents []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			URL  string `json:"url"`
		} `json:"contents"`
		Settings struct {
			AspectRatio string `json:"aspect_ratio"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		t.Fatalf("decode referenced body: %v (%s)", err, body)
	}
	if len(ref.Contents) != 2 || ref.Contents[0].Type != "prompt" || ref.Contents[0].Text != "p" ||
		ref.Contents[1].Type != "first_frame" || ref.Contents[1].URL != "https://example.test/first.png" {
		t.Fatalf("contents shape wrong: %s", body)
	}
	if ref.Settings.AspectRatio != "" {
		t.Fatalf("a referenced submit must not state an aspect ratio: %s", body)
	}

	// A data-URI reference strips to its bare base64 payload — the
	// upstream's documented spelling; the prefixed form is its documented
	// wrong example.
	body, err = EncodeKlingSubmit(KlingSubmitRequest{
		Model: "kling-3.0", Prompt: "p", Resolution: "720p", Duration: 4,
		RefURL: "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("px")),
	})
	if err != nil {
		t.Fatalf("encode data uri: %v", err)
	}
	if !strings.Contains(string(body), `"url":"`+base64.StdEncoding.EncodeToString([]byte("px"))+`"`) {
		t.Fatalf("a data URI must strip to bare base64: %s", body)
	}
	if strings.Contains(string(body), "data:image") {
		t.Fatalf("the data: prefix must not survive: %s", body)
	}

	// Uploaded bytes become bare base64 directly — no data URI detour the
	// other dialects' uploads take.
	body, err = EncodeKlingSubmit(KlingSubmitRequest{
		Model: "kling-3.0", Prompt: "p", Resolution: "720p", Duration: 4,
		RefData: []byte("px"),
	})
	if err != nil {
		t.Fatalf("encode upload: %v", err)
	}
	if !strings.Contains(string(body), `"url":"`+base64.StdEncoding.EncodeToString([]byte("px"))+`"`) {
		t.Fatalf("uploaded bytes must ride as bare base64: %s", body)
	}

	// A model outside the whitelist refuses at encode time; it would
	// otherwise dial a route that does not exist.
	if _, err := EncodeKlingSubmit(KlingSubmitRequest{Model: "kling-2.6", Prompt: "p", Resolution: "720p", Duration: 5}); err == nil {
		t.Fatal("an unwhitelisted model must fail the encode")
	}
}

func TestKlingSubmitPath(t *testing.T) {
	if got := KlingSubmitPath("kling-3.0-turbo", false); got != "/text-to-video/kling-3.0-turbo" {
		t.Fatalf("t2v path = %q", got)
	}
	if got := KlingSubmitPath("kling-3.0", true); got != "/image-to-video/kling-3.0" {
		t.Fatalf("i2v path = %q", got)
	}
}

func TestParseKlingSubmitResponse(t *testing.T) {
	id, biz, err := ParseKlingSubmitResponse([]byte(`{"code":0,"message":"SUCCEED","data":{"id":"893605","status":"submitted"}}`))
	if err != nil || biz != nil || id != "893605" {
		t.Fatalf("plain acceptance: id=%q biz=%v err=%v", id, biz, err)
	}
	_, biz, err = ParseKlingSubmitResponse([]byte(`{"code":1303,"message":"parallel task over resource pack limit","data":null}`))
	if err != nil || biz == nil || biz.Code != "1303" {
		t.Fatalf("refusal: biz=%v err=%v", biz, err)
	}
	if _, _, err := ParseKlingSubmitResponse([]byte(`{"code":0,"data":{}}`)); err == nil {
		t.Fatal("an id-less acceptance must be a decode error")
	}
	if _, _, err := ParseKlingSubmitResponse([]byte(`not json`)); err == nil {
		t.Fatal("a non-JSON answer must be a decode error")
	}
}

func TestParseKlingTaskResponseVocabulary(t *testing.T) {
	cases := map[string]struct {
		status, code, msg string
	}{
		`{"code":0,"data":[{"id":"1","status":"submitted"}]}`:                                                       {"pending", "", ""},
		`{"code":0,"data":[{"id":"1","status":"processing"}]}`:                                                      {"processing", "", ""},
		`{"code":0,"data":[{"id":"1","status":"succeeded","outputs":[{"type":"video","url":"u","duration":"8"}]}]}`: {"completed", "", ""},
		`{"code":0,"data":[{"id":"1","status":"failed","message":"content policy"}]}`:                               {"failed", "kling_task_failed", "content policy"},
	}
	for body, want := range cases {
		obs, biz, err := ParseKlingTaskResponse([]byte(body))
		if err != nil || biz != nil {
			t.Fatalf("%s: biz=%v err=%v", body, biz, err)
		}
		if obs.Status != want.status || obs.ErrorCode != want.code || obs.ErrorMessage != want.msg {
			t.Fatalf("%s: got %q/%q/%q, want %q/%q/%q", body, obs.Status, obs.ErrorCode, obs.ErrorMessage, want.status, want.code, want.msg)
		}
	}

	// The delivered-duration string is the billable seconds, and it is a
	// string on the wire: whole and decimal spellings both parse.
	obs, biz, err := ParseKlingTaskResponse([]byte(`{"code":0,"data":[{"id":"1","status":"succeeded","outputs":[{"type":"video","url":"https://v","duration":"8"}]}]}`))
	if err != nil || biz != nil || obs.VideoURL != "https://v" || obs.UsageSecs != 8 {
		t.Fatalf("completion must carry url and delivered seconds: %+v biz=%v err=%v", obs, biz, err)
	}
	obs, _, _ = ParseKlingTaskResponse([]byte(`{"code":0,"data":[{"id":"1","status":"succeeded","outputs":[{"type":"video","url":"u","duration":"3.0"}]}]}`))
	if obs.UsageSecs != 3 {
		t.Fatalf(`duration "3.0" must read as 3 seconds, got %d`, obs.UsageSecs)
	}
	// An empty duration reads as zero — the poller falls back to the
	// task's echoed ask, decided on the gateway side like the Ark twin.
	obs, _, _ = ParseKlingTaskResponse([]byte(`{"code":0,"data":[{"id":"1","status":"succeeded","outputs":[{"type":"video","url":"u"}]}]}`))
	if obs.UsageSecs != 0 {
		t.Fatalf("an unstated duration must read as zero, got %d", obs.UsageSecs)
	}
	// Non-video outputs (audio beside the video, an image result) are
	// not the deliverable; the video entry is.
	obs, _, _ = ParseKlingTaskResponse([]byte(`{"code":0,"data":[{"id":"1","status":"succeeded","outputs":[{"type":"audio","mp3_url":"a"},{"type":"video","url":"https://v","duration":"4"}]}]}`))
	if obs.VideoURL != "https://v" || obs.UsageSecs != 4 {
		t.Fatalf("the video output decides, got %+v", obs)
	}

	// An unknown task id arrives as an empty data array — observed live —
	// and reads as expired, never as a guess at pending.
	obs, biz, err = ParseKlingTaskResponse([]byte(`{"code":0,"message":"SUCCEED","data":[]}`))
	if err != nil || biz != nil || obs.Status != "expired" || obs.ErrorCode != "task_expired" {
		t.Fatalf("empty data must read as expired: %+v biz=%v err=%v", obs, biz, err)
	}
	_, biz, err = ParseKlingTaskResponse([]byte(`{"code":1200,"message":"bad request"}`))
	if err != nil || biz == nil || biz.Code != "1200" {
		t.Fatalf("envelope refusal: biz=%v err=%v", biz, err)
	}
	if _, _, err := ParseKlingTaskResponse([]byte(`{"code":0,"data":[{"id":"1","status":"succeed"}]}`)); err == nil {
		t.Fatal("the old API's \"succeed\" spelling must be a decode error, never a guess")
	}
}

func TestKlingReferenced(t *testing.T) {
	if KlingReferenced(nil) {
		t.Fatal("no reference is not referenced")
	}
	if KlingReferenced(&InputRef{}) {
		t.Fatal("a present-but-empty reference is the text generation it is")
	}
	if !KlingReferenced(&InputRef{ImageURL: "https://x/1.png"}) {
		t.Fatal("a URL reference is referenced")
	}
	if !KlingReferenced(&InputRef{File: &File{Data: []byte{1}}}) {
		t.Fatal("an uploaded-bytes reference is referenced")
	}
	if KlingReferenced(&InputRef{File: &File{}}) {
		t.Fatal("a file part with no bytes is not a reference")
	}
}
