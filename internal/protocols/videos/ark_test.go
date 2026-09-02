package videos

// Unit pins for the Ark half of the video dialect: base detection, the
// shared size table through Ark's spelling, the content[] submit shape,
// and the response parsers. The submit/poll round trips are held by the
// gateway's end-to-end battery.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsArkBase(t *testing.T) {
	for _, base := range []string{
		"https://ark.cn-beijing.volces.com/api/v3",
		"https://ark.ap-southeast.volces.com",
		"https://ark.volces.com",
	} {
		if !IsArkBase(base) {
			t.Errorf("IsArkBase(%q) = false, want true", base)
		}
	}
	for _, base := range []string{
		"https://dashscope.aliyuncs.com/compatible-mode/v1",
		"https://ark.example.com",
		"https://volces.com.evil.test",
	} {
		if IsArkBase(base) {
			t.Errorf("IsArkBase(%q) = true, want false", base)
		}
	}
}

func TestMapArkSizeSharesTheTierTable(t *testing.T) {
	cases := map[string]struct{ res, ratio string }{
		"720x1280":  {"720p", "9:16"},
		"1280x720":  {"720p", "16:9"},
		"1024x1792": {"1080p", "9:16"},
		"1792x1024": {"1080p", "16:9"},
	}
	for size, want := range cases {
		res, ratio, ok := MapArkSize(size)
		if !ok || res != want.res || ratio != want.ratio {
			t.Errorf("MapArkSize(%q) = %q,%q,%v; want %q,%q", size, res, ratio, ok, want.res, want.ratio)
		}
		// The pricing tier is the uppercase twin of the API spelling —
		// one table, two casings, never two answers.
		tier, ok := TierForSize(size)
		if !ok || tier != strings.ToUpper(want.res) {
			t.Errorf("TierForSize(%q) = %q,%v; want %q", size, tier, ok, strings.ToUpper(want.res))
		}
	}
	for _, size := range []string{"", "1080x1920"} {
		if _, _, ok := MapArkSize(size); ok {
			t.Errorf("MapArkSize(%q) must not map", size)
		}
	}
}

func TestEncodeArkSubmitShapes(t *testing.T) {
	// Text-only: one text item, the ratio stated.
	body, err := EncodeArkSubmit(ArkSubmitRequest{
		Model: "doubao-seedance-2-0-260128", Prompt: "p", Resolution: "720p", Ratio: "16:9", Duration: 4,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Role     string `json:"role"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
		Ratio    string `json:"ratio"`
		Duration int    `json:"duration"`
	}
	decodeArkBody(t, body, &doc)
	if len(doc.Content) != 1 || doc.Content[0].Type != "text" || doc.Content[0].Text != "p" {
		t.Fatalf("text-only content shape wrong: %s", body)
	}
	if doc.Ratio != "16:9" || doc.Duration != 4 {
		t.Fatalf("knobs wrong: %s", body)
	}
	// With a reference: the image item carries the first_frame role, and
	// no ratio is stated — the adaptive default follows the image.
	body, err = EncodeArkSubmit(ArkSubmitRequest{
		Model: "doubao-seedance-2-0-260128", Prompt: "p", Resolution: "1080p", Ratio: "16:9", Duration: 8,
		RefURL: "https://example.test/first.png",
	})
	if err != nil {
		t.Fatalf("encode with ref: %v", err)
	}
	var ref struct {
		Content []struct {
			Type     string `json:"type"`
			Role     string `json:"role"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
		Ratio string `json:"ratio"`
	}
	decodeArkBody(t, body, &ref)
	if len(ref.Content) != 2 || ref.Content[1].Type != "image_url" || ref.Content[1].Role != "first_frame" ||
		ref.Content[1].ImageURL == nil || ref.Content[1].ImageURL.URL != "https://example.test/first.png" {
		t.Fatalf("reference item shape wrong: %s", body)
	}
	if ref.Ratio != "" {
		t.Fatalf("an image-referenced submit must not state a ratio: %s", body)
	}
	// An uploaded file normalizes to the data URI spelling.
	body, err = EncodeArkSubmit(ArkSubmitRequest{
		Model: "doubao-seedance-2-0-260128", Prompt: "p", Resolution: "720p", Duration: 4,
		RefData: []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
	})
	if err != nil {
		t.Fatalf("encode upload: %v", err)
	}
	if !strings.Contains(string(body), "data:image/png;base64,") {
		t.Fatalf("an unlabelled PNG upload must sniff to image/png: %s", body)
	}
}

func TestParseArkSubmitResponse(t *testing.T) {
	id, biz, err := ParseArkSubmitResponse([]byte(`{"id":"cgt-1"}`))
	if err != nil || biz != nil || id != "cgt-1" {
		t.Fatalf("plain acceptance: id=%q biz=%v err=%v", id, biz, err)
	}
	_, biz, err = ParseArkSubmitResponse([]byte(`{"error":{"code":"InvalidApiKey","message":"bad"}}`))
	if err != nil || biz == nil || biz.Code != "InvalidApiKey" {
		t.Fatalf("refusal: biz=%v err=%v", biz, err)
	}
	if _, _, err := ParseArkSubmitResponse([]byte(`{}`)); err == nil {
		t.Fatal("an id-less acceptance must be a decode error")
	}
}

func TestParseArkTaskResponseVocabulary(t *testing.T) {
	cases := map[string]struct {
		status, code string
		biz          bool
	}{
		`{"id":"c","status":"queued"}`:                                             {"pending", "", false},
		`{"id":"c","status":"running"}`:                                            {"processing", "", false},
		`{"id":"c","status":"succeeded","content":{"video_url":"u"},"duration":8}`: {"completed", "", false},
		`{"id":"c","status":"failed","error":{"code":"X","message":"m"}}`:          {"failed", "X", false},
		`{"id":"c","status":"failed"}`:                                             {"failed", "upstream_failed", false},
		`{"id":"c","status":"cancelled"}`:                                          {"cancelled", "", false},
		`{"id":"c","status":"expired"}`:                                            {"expired", "", false},
	}
	for body, want := range cases {
		obs, err := ParseArkTaskResponse([]byte(body))
		if err != nil {
			t.Fatalf("%s: err=%v", body, err)
		}
		if obs.Status != want.status || obs.ErrorCode != want.code {
			t.Fatalf("%s: got %q/%q, want %q/%q", body, obs.Status, obs.ErrorCode, want.status, want.code)
		}
	}
	obs, err := ParseArkTaskResponse([]byte(`{"id":"c","status":"succeeded","content":{"video_url":"https://v"},"duration":8}`))
	if err != nil || obs.VideoURL != "https://v" || obs.UsageSecs != 8 {
		// Usage is the echoed duration: Ark reports no seconds field
		// anywhere, and this dialect always states its seconds.
		t.Fatalf("completion must carry url and echoed duration: %+v err=%v", obs, err)
	}
	if _, err := ParseArkTaskResponse([]byte(`{"id":"c","status":"WEIRD"}`)); err == nil {
		t.Fatal("an undocumented status word must be a decode error, never a guess")
	}
}

// decodeArkBody is the battery's one JSON decode: assertions read the
// decoded shape, not the marshaled key order a map happens to produce.
func decodeArkBody(t *testing.T, body []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("decode submit body: %v (%s)", err, body)
	}
}
