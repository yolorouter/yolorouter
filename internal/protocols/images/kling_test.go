package images

// Unit pins for the Kling image dialect: base detection, the model
// whitelist, the size mapping, the submit and task parsers, and the
// OpenAI shaping of a delivered task. The submit-poll-answer round trip
// is held by the gateway's stub battery.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsKlingBase(t *testing.T) {
	for _, base := range []string{
		"https://api-beijing.klingai.com",
		"https://api-singapore.klingai.com",
	} {
		if !IsKlingBase(base) {
			t.Errorf("IsKlingBase(%q) = false, want true", base)
		}
	}
	for _, base := range []string{
		"https://dashscope.aliyuncs.com/compatible-mode/v1",
		"https://klingai.com.evil.test",
	} {
		if IsKlingBase(base) {
			t.Errorf("IsKlingBase(%q) = true, want false", base)
		}
	}
}

func TestKlingImageModelSupported(t *testing.T) {
	if !KlingImageModelSupported("kling-v3") {
		t.Error("kling-v3 must be supported")
	}
	for _, name := range []string{"kling-v2-1", "kling-v1", "kling-v3 ", ""} {
		if KlingImageModelSupported(name) {
			t.Errorf("KlingImageModelSupported(%q) = true, want false", name)
		}
	}
}

func TestMapKlingImageSize(t *testing.T) {
	cases := map[string]struct{ res, aspect string }{
		"":          {"", ""},
		"1k":        {"1k", ""},
		"2K":        {"2k", ""},
		"1024x1024": {"1k", "1:1"},
		"720x1280":  {"1k", "9:16"},
		"1280x720":  {"1k", "16:9"},
		"2048x2048": {"2k", "1:1"},
		"3840x1646": {"2k", "21:9"},
		"864x1152":  {"1k", "3:4"},
		// factor 2.0: |2.0-2.33|=0.33 vs |2.0-1.78|=0.22 → 16:9 is nearer.
		"2000x1000": {"2k", "16:9"},
	}
	for size, want := range cases {
		res, aspect, ok := MapKlingImageSize(size)
		if !ok || res != want.res || aspect != want.aspect {
			t.Errorf("MapKlingImageSize(%q) = %q,%q,%v; want %q,%q", size, res, aspect, ok, want.res, want.aspect)
		}
	}
	for _, size := range []string{"garbage", "300", "0x1024", "wide"} {
		if _, _, ok := MapKlingImageSize(size); ok {
			t.Errorf("MapKlingImageSize(%q) must not map", size)
		}
	}
	// A small-but-valid pixel size maps — the endpoint's minimums are
	// reference-image constraints, not size-vocabulary ones.
	if res, aspect, ok := MapKlingImageSize("100x100"); !ok || res != "1k" || aspect != "1:1" {
		t.Errorf("MapKlingImageSize(\"100x100\") = %q,%q,%v; want 1k,1:1", res, aspect, ok)
	}
}

func TestEncodeKlingImageRequest(t *testing.T) {
	body, err := EncodeKlingImageRequest("a red cube", "kling-v3", 2, "1024x1024", nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc struct {
		ModelName   string `json:"model_name"`
		Prompt      string `json:"prompt"`
		N           int    `json:"n"`
		Resolution  string `json:"resolution"`
		AspectRatio string `json:"aspect_ratio"`
		Watermark   any    `json:"watermark_info"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if doc.ModelName != "kling-v3" || doc.Prompt != "a red cube" || doc.N != 2 ||
		doc.Resolution != "1k" || doc.AspectRatio != "1:1" {
		t.Fatalf("shape wrong: %s", body)
	}
	if doc.Watermark != nil {
		t.Fatalf("watermark must stay omitted (default off): %s", body)
	}

	// An absent size omits both knobs; an absent n normalizes to one.
	body, err = EncodeKlingImageRequest("p", "kling-v3", 0, "", nil)
	if err != nil {
		t.Fatalf("encode defaults: %v", err)
	}
	var def struct {
		N           int    `json:"n"`
		Resolution  string `json:"resolution"`
		AspectRatio string `json:"aspect_ratio"`
	}
	if err := json.Unmarshal(body, &def); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}
	if def.N != 1 || def.Resolution != "" || def.AspectRatio != "" {
		t.Fatalf("defaults wrong: %s", body)
	}

	if _, err := EncodeKlingImageRequest("p", "kling-v2-1", 1, "", nil); err == nil {
		t.Fatal("an unwhitelisted model must fail the encode")
	}
	if _, err := EncodeKlingImageRequest("p", "kling-v3", 1, "nonsense", nil); err == nil {
		t.Fatal("an unmappable size must fail the encode")
	}
}

func TestParseKlingImageSubmitResponse(t *testing.T) {
	id, biz, err := ParseKlingImageSubmitResponse([]byte(`{"code":0,"message":"success","data":{"task_id":"951","task_status":"submitted"}}`))
	if err != nil || biz != nil || id != "951" {
		t.Fatalf("acceptance: id=%q biz=%v err=%v", id, biz, err)
	}
	_, biz, err = ParseKlingImageSubmitResponse([]byte(`{"code":1102,"message":"Account balance not enough","data":null}`))
	if err != nil || biz == nil || biz.Code != "1102" {
		t.Fatalf("refusal: biz=%v err=%v", biz, err)
	}
	if _, _, err := ParseKlingImageSubmitResponse([]byte(`{"code":0,"data":{}}`)); err == nil {
		t.Fatal("a task_id-less acceptance must be a decode error")
	}
}

func TestParseKlingImageTaskResponse(t *testing.T) {
	// Non-terminal statuses stay explicitly non-terminal.
	for _, status := range []string{"submitted", "processing"} {
		task, biz, err := ParseKlingImageTaskResponse([]byte(`{"code":0,"data":{"task_status":"` + status + `"}}`))
		if err != nil || biz != nil || task.Terminal {
			t.Fatalf("%s must be non-terminal: %+v biz=%v err=%v", status, task, biz, err)
		}
	}
	// The succeed spelling this endpoint family kept, with its payload.
	task, biz, err := ParseKlingImageTaskResponse([]byte(`{"code":0,"data":{"task_status":"succeed","final_unit_deduction":"0.5","final_balance_deduction":{"quota":"0.5","list_price":"0.5"},"task_result":{"images":[{"index":0,"url":"https://a"},{"index":1,"url":"https://b"}]}}}`))
	if err != nil || biz != nil || !task.Terminal || task.Failed {
		t.Fatalf("succeed must be terminal and not failed: %+v biz=%v err=%v", task, biz, err)
	}
	if len(task.ImageURLs) != 2 || task.ImageURLs[0] != "https://a" || task.UnitDeduction != "0.5" || task.BalanceDeduct != "0.5" || task.BalanceListPrice != "0.5" {
		t.Fatalf("payload wrong: %+v", task)
	}
	// Failure carries the upstream's own reason.
	task, _, err = ParseKlingImageTaskResponse([]byte(`{"code":0,"data":{"task_status":"failed","task_status_msg":"content risk control"}}`))
	if err != nil || !task.Terminal || !task.Failed || task.StatusMsg != "content risk control" {
		t.Fatalf("failed shape wrong: %+v err=%v", task, err)
	}
	_, biz, err = ParseKlingImageTaskResponse([]byte(`{"code":1200,"message":"bad request"}`))
	if err != nil || biz == nil || biz.Code != "1200" {
		t.Fatalf("envelope refusal: biz=%v err=%v", biz, err)
	}
	if _, _, err := ParseKlingImageTaskResponse([]byte(`{"code":0,"data":{"task_status":"succeeded"}}`)); err == nil {
		t.Fatal(`the new API's "succeeded" spelling must be a decode error here, never a guess`)
	}
}

func TestEncodeKlingImagesOpenAI(t *testing.T) {
	body, err := EncodeKlingImagesOpenAI([]string{"https://a", "https://b"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Created == 0 || len(doc.Data) != 2 || doc.Data[1].URL != "https://b" {
		t.Fatalf("shape wrong: %s", body)
	}
}

func TestKlingImageModelAndPathRouting(t *testing.T) {
	for _, name := range []string{"kling-v3", "kling-v3-omni", "kling-image-o1"} {
		if !KlingImageModelSupported(name) {
			t.Errorf("KlingImageModelSupported(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"kling-v2-1", "kling-v1"} {
		if KlingImageModelSupported(name) {
			t.Errorf("KlingImageModelSupported(%q) = true, want false", name)
		}
	}
	if got := KlingImageSubmitPathFor("kling-v3"); got != KlingImageSubmitPath {
		t.Errorf("generations model routed to %q", got)
	}
	if got := KlingImageTaskPathPrefixFor("kling-v3"); got != KlingImageSubmitPath+"/" {
		t.Errorf("generations poll routed to %q", got)
	}
	for _, name := range []string{"kling-v3-omni", "kling-image-o1"} {
		if got := KlingImageSubmitPathFor(name); got != KlingOmniImageSubmitPath {
			t.Errorf("omni model %q routed to %q", name, got)
		}
		if got := KlingImageTaskPathPrefixFor(name); got != KlingOmniImageSubmitPath+"/" {
			t.Errorf("omni poll for %q routed to %q", name, got)
		}
	}
}

func TestMapKlingImageSize4K(t *testing.T) {
	res, _, ok := MapKlingImageSize("4K")
	if !ok || res != "4k" {
		t.Fatalf(`"4K" must map to the 4k tier, got %q,%v`, res, ok)
	}
}

func TestEncodeKlingImageRequestPassesNativeFieldsThrough(t *testing.T) {
	callerBody := []byte(`{"model":"kling-v3-omni","prompt":"p","n":1,` +
		`"image_list":[{"image":"https://a/1.png"},{"image":"data:image/png;base64,QUJD"}],` +
		`"element_list":[{"element_id":160}],` +
		`"result_type":"series","series_amount":"auto"}`)
	native := ParseKlingNativeFields(callerBody)
	body, err := EncodeKlingImageRequest("p", "kling-v3-omni", 1, "2k", native)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var doc struct {
		ModelName    string          `json:"model_name"`
		Resolution   string          `json:"resolution"`
		ImageList    json.RawMessage `json:"image_list"`
		ElementList  json.RawMessage `json:"element_list"`
		ResultType   string          `json:"result_type"`
		SeriesAmount json.RawMessage `json:"series_amount"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if doc.ModelName != "kling-v3-omni" || doc.Resolution != "2k" || doc.ResultType != "series" {
		t.Fatalf("knobs wrong: %s", body)
	}
	if len(doc.ImageList) == 0 || !contains(doc.ImageList, "https://a/1.png") || !contains(doc.ImageList, "QUJD") {
		t.Fatalf("image_list must ride verbatim: %s", doc.ImageList)
	}
	if !contains(doc.ElementList, "160") {
		t.Fatalf("element_list must ride verbatim: %s", doc.ElementList)
	}
	if !contains(doc.SeriesAmount, "auto") {
		t.Fatalf(`series_amount "auto" must ride verbatim: %s`, doc.SeriesAmount)
	}

	// An ordinary OpenAI-shaped body contributes nothing.
	plain := ParseKlingNativeFields([]byte(`{"model":"m","prompt":"p"}`))
	body, err = EncodeKlingImageRequest("p", "kling-v3", 1, "", plain)
	if err != nil {
		t.Fatalf("encode plain: %v", err)
	}
	for _, key := range []string{"image_list", "element_list", "result_type", "series_amount"} {
		if contains(body, key) {
			t.Fatalf("a body without native fields must not carry %q: %s", key, body)
		}
	}
}

func contains(haystack []byte, needle string) bool {
	return strings.Contains(string(haystack), needle)
}

func TestParseKlingImageTaskSeriesImages(t *testing.T) {
	task, biz, err := ParseKlingImageTaskResponse([]byte(`{"code":0,"data":{"task_status":"succeed",` +
		`"task_result":{"result_type":"series","images":[],"series_images":[{"index":0,"url":"https://s/1"},{"index":1,"url":"https://s/2"}]},` +
		`"final_unit_deduction":"0.5"}}`))
	if err != nil || biz != nil {
		t.Fatalf("series parse: biz=%v err=%v", biz, err)
	}
	if len(task.ImageURLs) != 2 || task.ImageURLs[0] != "https://s/1" {
		t.Fatalf("series images must join the delivered surface: %+v", task)
	}
}
