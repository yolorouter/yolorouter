package videos

// Unit pins for the MiniMax half of the video dialect: base detection,
// the model whitelist, the model-dependent size table, the duration
// gate, both submit shapes, and the response parsers. The submit/poll
// round trips are held by the gateway's end-to-end battery.

import (
	"encoding/json"
	"testing"
)

func TestIsMiniMaxBase(t *testing.T) {
	if !IsMiniMaxBase("https://api.minimax.cn") {
		t.Error("IsMiniMaxBase(api.minimax.cn) = false, want true")
	}
	if !IsMiniMaxBase("https://api.minimax.cn/v1") {
		t.Error("a base with a path suffix must still detect by host")
	}
	// The docs name one host and no other; a suffix or subdomain match
	// would admit hosts nobody documented.
	for _, base := range []string{
		"https://api.minimaxi.com",
		"https://api.minimax.cn.evil.test",
		"https://minimax.cn",
		"https://example.com/v2/video_generation",
	} {
		if IsMiniMaxBase(base) {
			t.Errorf("IsMiniMaxBase(%q) = true, want false", base)
		}
	}
}

func TestMiniMaxModelSupported(t *testing.T) {
	for _, name := range []string{MiniMaxH3Model, MiniMaxH3MaxModel} {
		if !MiniMaxModelSupported(name) {
			t.Errorf("MiniMaxModelSupported(%q) = false, want true", name)
		}
	}
	// The V1 family and every near-miss spelling stay outside: the enum
	// lives in the request body and the upstream would refuse them.
	for _, name := range []string{"MiniMax-Hailuo-2.3", "MiniMax-H3 ", "minimax-h3", "MiniMax-H4"} {
		if MiniMaxModelSupported(name) {
			t.Errorf("MiniMaxModelSupported(%q) = true, want false", name)
		}
	}
}

func TestMapMiniMaxSizePerModel(t *testing.T) {
	cases := []struct {
		model, size, res, ratio string
	}{
		{MiniMaxH3Model, "720x1280", "768P", "9:16"},
		{MiniMaxH3Model, "1280x720", "768P", "16:9"},
		{MiniMaxH3Model, "1024x1792", "2K", "9:16"},
		{MiniMaxH3Model, "1792x1024", "2K", "16:9"},
		// H3-Max has no 2K: the large door sizes stay at its top.
		{MiniMaxH3MaxModel, "720x1280", "768P", "9:16"},
		{MiniMaxH3MaxModel, "1280x720", "768P", "16:9"},
		{MiniMaxH3MaxModel, "1024x1792", "768P", "9:16"},
		{MiniMaxH3MaxModel, "1792x1024", "768P", "16:9"},
	}
	for _, tc := range cases {
		res, ratio, ok := MapMiniMaxSize(tc.model, tc.size)
		if !ok || res != tc.res || ratio != tc.ratio {
			t.Errorf("MapMiniMaxSize(%q,%q) = %q,%q,%v; want %q,%q", tc.model, tc.size, res, ratio, ok, tc.res, tc.ratio)
		}
	}
	for _, size := range []string{"", "1080x1920"} {
		if _, _, ok := MapMiniMaxSize(MiniMaxH3Model, size); ok {
			t.Errorf("MapMiniMaxSize(%q) must not map", size)
		}
	}
	// A model outside the whitelist maps nothing — the gate answers
	// first, and the map must not invent a tier for a name the enum
	// refuses.
	if _, _, ok := MapMiniMaxSize("MiniMax-Hailuo-2.3", "720x1280"); ok {
		t.Error("MapMiniMaxSize must not map for a model outside the whitelist")
	}
}

func TestMiniMaxDurationSupported(t *testing.T) {
	for _, seconds := range []int{4, 8, 12} {
		if !MiniMaxDurationSupported(MiniMaxH3Model, seconds) {
			t.Errorf("H3 must accept %d seconds", seconds)
		}
	}
	if MiniMaxDurationSupported(MiniMaxH3MaxModel, 4) {
		t.Error("H3-Max must refuse 4 seconds — its documented floor is 5")
	}
	for _, seconds := range []int{5, 8, 12} {
		if !MiniMaxDurationSupported(MiniMaxH3MaxModel, seconds) {
			t.Errorf("H3-Max must accept %d seconds", seconds)
		}
	}
}

func TestEncodeMiniMaxSubmitShapes(t *testing.T) {
	// Text-only: the required text item, the required resolution and
	// duration, the ratio stated, and no watermark knob anywhere — off
	// is the default and the stance.
	body, err := EncodeMiniMaxSubmit(MiniMaxSubmitRequest{
		Model: MiniMaxH3Model, Prompt: "p", Resolution: "768P", Ratio: "16:9", Duration: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got["ratio"] != "16:9" {
		t.Errorf("text submit must state the ratio, got %v", got["ratio"])
	}
	if _, exists := got["aigc_watermark"]; exists {
		t.Error("aigc_watermark must be omitted, not stated")
	}
	content := got["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["type"] != "text" {
		t.Errorf("text submit content = %v, want one text item", content)
	}

	// Image-referenced: the image rides as an image_url item with the
	// first_frame role, and no ratio key exists — the upstream forces
	// adaptive there and states other values are ignored.
	body, err = EncodeMiniMaxSubmit(MiniMaxSubmitRequest{
		Model: MiniMaxH3MaxModel, Prompt: "p", Resolution: "768P", Duration: 5,
		RefURL: "https://example.com/frame.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["ratio"]; exists {
		t.Error("image submit must not state a ratio")
	}
	content = got["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("image submit content has %d items, want 2", len(content))
	}
	image := content[1].(map[string]any)
	if image["type"] != "image_url" || image["role"] != "first_frame" {
		t.Errorf("image item = %v, want image_url with first_frame role", image)
	}
	if image["image_url"].(map[string]any)["url"] != "https://example.com/frame.png" {
		t.Errorf("image url not carried verbatim: %v", image)
	}

	// A caller's uppercase data-URI media type is normalized to the
	// lowercase token the upstream documents; the payload is untouched.
	body, err = EncodeMiniMaxSubmit(MiniMaxSubmitRequest{
		Model: MiniMaxH3Model, Prompt: "p", Resolution: "768P", Duration: 4,
		RefURL: "data:image/PNG;base64,QUJD",
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	image = got["content"].([]any)[1].(map[string]any)
	if url := image["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,QUJD" {
		t.Errorf("data URI media type not lowercased: %q", url)
	}

	// Uploaded bytes with no content type ride as the shared data-URI
	// builder's sniffed type.
	body, err = EncodeMiniMaxSubmit(MiniMaxSubmitRequest{
		Model: MiniMaxH3Model, Prompt: "p", Resolution: "768P", Duration: 4,
		RefData: []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'},
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	image = got["content"].([]any)[1].(map[string]any)
	if url := image["image_url"].(map[string]any)["url"]; url != "data:image/png;base64,iVBORw0KGgo=" {
		t.Errorf("sniffed data URI = %q", url)
	}

	// An upload that states its own content type is trusted with it —
	// the same trust the ark and dashscope halves place in the part's
	// header, sniffing only when the header is absent or generic.
	body, err = EncodeMiniMaxSubmit(MiniMaxSubmitRequest{
		Model: MiniMaxH3Model, Prompt: "p", Resolution: "768P", Duration: 4,
		RefData:        []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'},
		RefContentType: "image/webp",
	})
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	image = got["content"].([]any)[1].(map[string]any)
	if url := image["image_url"].(map[string]any)["url"]; url != "data:image/webp;base64,iVBORw0KGgo=" {
		t.Errorf("declared upload content type must be honored, got %q", url)
	}

	if _, err := EncodeMiniMaxSubmit(MiniMaxSubmitRequest{Model: "MiniMax-Hailuo-2.3"}); err == nil {
		t.Error("encode must refuse a model outside the whitelist")
	}
}

func TestMiniMaxTaskRoute(t *testing.T) {
	if got := MiniMaxTaskRoute("424010985738629"); got != "/v2/query/video_generation/424010985738629" {
		t.Errorf("MiniMaxTaskRoute = %q", got)
	}
}

func TestParseMiniMaxSubmitResponse(t *testing.T) {
	id, err := ParseMiniMaxSubmitResponse([]byte(`{"task_id":"424010985738629"}`))
	if err != nil || id != "424010985738629" {
		t.Errorf("parse = %q,%v", id, err)
	}
	if _, err := ParseMiniMaxSubmitResponse([]byte(`{}`)); err == nil {
		t.Error("a body without a task id must be an error")
	}
	if _, err := ParseMiniMaxSubmitResponse([]byte(`not json`)); err == nil {
		t.Error("a non-JSON body must be an error")
	}
}

func TestParseMiniMaxTaskResponseVocabulary(t *testing.T) {
	// Completed with usage: the stated output seconds are the billable
	// ones, and the video URL rides on the content object.
	body := []byte(`{"task":{"id":"1","model":"MiniMax-H3","status":"succeeded","content":{"url":"https://cdn.example.test/v.mp4"},"resolution":"2K","duration":5,"usage":{"total_seconds":5,"input_seconds":0,"output_seconds":5,"input_image_count":0},"ratio":"16:9","task_type":"generation","modality":"video"}}`)
	obs, err := ParseMiniMaxTaskResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != StatusCompleted || obs.VideoURL != "https://cdn.example.test/v.mp4" || obs.UsageSecs != 5 {
		t.Errorf("completed observation = %+v", obs)
	}

	// Completed with an empty usage object: the task's own duration echo
	// answers — the fallback the other dialects take from the task row,
	// read from this body because the echo rides beside the usage.
	body = []byte(`{"task":{"id":"1","status":"succeeded","content":{"url":"u"},"duration":8,"usage":{}}}`)
	obs, err = ParseMiniMaxTaskResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if obs.UsageSecs != 8 {
		t.Errorf("usage fallback = %d, want the duration echo 8", obs.UsageSecs)
	}

	// Failed with the vendor's error face.
	body = []byte(`{"task":{"id":"1","status":"failed","error":{"code":"1026","message":"video description contains sensitive content"},"duration":5,"usage":{}}}`)
	obs, err = ParseMiniMaxTaskResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != StatusFailed || obs.ErrorCode != "1026" || obs.ErrorMessage != "video description contains sensitive content" {
		t.Errorf("failed observation = %+v", obs)
	}

	// Failed without an error face keeps a code the caller can branch on.
	obs, err = ParseMiniMaxTaskResponse([]byte(`{"task":{"id":"1","status":"failed"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if obs.ErrorCode != "minimax_task_failed" {
		t.Errorf("faceless failed code = %q", obs.ErrorCode)
	}

	// A completion without its clip URL cannot be acted on: a parse
	// error, never a billable completion.
	if _, err := ParseMiniMaxTaskResponse([]byte(`{"task":{"id":"1","status":"succeeded","duration":5,"usage":{"output_seconds":5}}}`)); err == nil {
		t.Error("a succeeded task with no content url must be a parse error")
	}

	// The vendor can say cancelled; the internal vocabulary keeps it and
	// the wire renders it as a failure with the task_cancelled code.
	obs, err = ParseMiniMaxTaskResponse([]byte(`{"task":{"id":"1","status":"cancelled"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if obs.Status != StatusCancelled {
		t.Errorf("cancelled status = %q, want the internal cancelled", obs.Status)
	}
	if wire, errCode := MapWireStatus(obs.Status); wire != WireFailed || errCode != ErrCodeTaskCancelled {
		t.Errorf("cancelled wire rendering = %q,%q", wire, errCode)
	}

	// Non-terminal statuses and a word outside the vocabulary.
	for _, tc := range []struct{ word, want string }{
		{"queued", StatusPending}, {"running", StatusProcessing},
	} {
		obs, err := ParseMiniMaxTaskResponse([]byte(`{"task":{"status":"` + tc.word + `"}}`))
		if err != nil || obs.Status != tc.want {
			t.Errorf("status %q = %q,%v; want %q", tc.word, obs.Status, err, tc.want)
		}
	}
	if _, err := ParseMiniMaxTaskResponse([]byte(`{"task":{"status":"Success"}}`)); err == nil {
		t.Error("the V1 capital-S spelling must be outside the V2 vocabulary")
	}
}
