package videos

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"strings"
	"testing"
)

func TestParseCreateRequestJSON(t *testing.T) {
	body := `{"model":"sora-2","prompt":"a calico cat at a piano","seconds":8,"size":"1280x720","input_reference":{"image_url":"https://example.test/cat.png"},"future_knob":true}`
	req, err := ParseCreateRequest("application/json", []byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Model != "sora-2" || req.Prompt != "a calico cat at a piano" {
		t.Fatalf("model/prompt not read: %+v", req)
	}
	if req.Seconds != 8 || req.Size != "1280x720" {
		t.Fatalf("seconds/size not read: %+v", req)
	}
	if req.InputReference == nil || req.InputReference.ImageURL != "https://example.test/cat.png" {
		t.Fatalf("input_reference not read: %+v", req.InputReference)
	}
}

func TestParseCreateRequestJSONDefaultsAndOmissions(t *testing.T) {
	req, err := ParseCreateRequest("application/json", []byte(`{"model":"m","prompt":"p"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Seconds != 0 || req.Size != "" || req.InputReference != nil {
		t.Fatalf("omitted fields must stay unset, got %+v", req)
	}
}

func TestParseCreateRequestJSONWrongTypeRefused(t *testing.T) {
	if _, err := ParseCreateRequest("application/json", []byte(`{"model":42,"prompt":"p"}`)); err == nil {
		t.Fatal("a non-string model must be a parse error, not a routed request")
	}
	if _, err := ParseCreateRequest("application/json", []byte(`{"seconds":"8","prompt":"p"}`)); err == nil {
		t.Fatal("a non-integer seconds must be a parse error")
	}
}

func TestParseCreateRequestMultipartWithFile(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "sora-2")
	_ = w.WriteField("prompt", "piano cat")
	_ = w.WriteField("seconds", "12")
	_ = w.WriteField("size", "1024x1792")
	fw, _ := w.CreateFormFile("input_reference", "cat.png")
	_, _ = fw.Write([]byte("png-bytes"))
	_ = w.Close()

	req, err := ParseCreateRequest(w.FormDataContentType(), buf.Bytes())
	if err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	if req.Model != "sora-2" || req.Prompt != "piano cat" || req.Seconds != 12 || req.Size != "1024x1792" {
		t.Fatalf("fields not read: %+v", req)
	}
	if req.InputReference == nil || req.InputReference.File == nil {
		t.Fatalf("file part not attached: %+v", req.InputReference)
	}
	if req.InputReference.File.FileName != "cat.png" || string(req.InputReference.File.Data) != "png-bytes" {
		t.Fatalf("file not read: %+v", req.InputReference.File)
	}
}

func TestParseCreateRequestMultipartReferenceAsJSONField(t *testing.T) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("model", "m")
	_ = w.WriteField("prompt", "p")
	_ = w.WriteField("input_reference", `{"image_url":"data:image/png;base64,AAAA"}`)
	_ = w.Close()

	req, err := ParseCreateRequest(w.FormDataContentType(), buf.Bytes())
	if err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	if req.InputReference == nil || req.InputReference.ImageURL != "data:image/png;base64,AAAA" {
		t.Fatalf("reference object field not read: %+v", req.InputReference)
	}
}

func TestParseCreateRequestMultipartLongPromptWhole(t *testing.T) {
	long := strings.Repeat("word ", 300) // past formFieldCap; the prompt is exempt
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("prompt", long)
	_ = w.Close()

	req, err := ParseCreateRequest(w.FormDataContentType(), buf.Bytes())
	if err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	if req.Prompt != strings.TrimSpace(long) {
		t.Fatalf("prompt must be read whole, got %d bytes", len(req.Prompt))
	}
}

func TestParseCreateRequestMultipartRefusals(t *testing.T) {
	build := func(fn func(w *multipart.Writer)) (string, []byte) {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		fn(w)
		_ = w.Close()
		return w.FormDataContentType(), buf.Bytes()
	}
	ct, body := build(func(w *multipart.Writer) {
		_ = w.WriteField("model", "m")
		_ = w.WriteField("model", "again")
	})
	if _, err := ParseCreateRequest(ct, body); err == nil {
		t.Fatal("duplicate model must be refused")
	}
	ct, body = build(func(w *multipart.Writer) {
		_ = w.WriteField("model", "m")
		_ = w.WriteField("input_reference", "not-json")
	})
	if _, err := ParseCreateRequest(ct, body); err == nil {
		t.Fatal("a non-object input_reference field must be refused")
	}
	if _, err := ParseCreateRequest("multipart/form-data", []byte("x")); err == nil {
		t.Fatal("a multipart type without boundary must be refused")
	}
}

func TestVocabularyPredicates(t *testing.T) {
	for _, v := range []int{4, 8, 12} {
		if !ValidSeconds(v) {
			t.Fatalf("seconds %d must be valid", v)
		}
	}
	for _, v := range []int{0, 5, 16, -4} {
		if ValidSeconds(v) {
			t.Fatalf("seconds %d must not be valid", v)
		}
	}
	for _, s := range []string{"720x1280", "1280x720", "1024x1792", "1792x1024"} {
		if !ValidSize(s) {
			t.Fatalf("size %s must be valid", s)
		}
	}
	for _, s := range []string{"", "720p", "1080x1920", "720X1280"} {
		if ValidSize(s) {
			t.Fatalf("size %q must not be valid", s)
		}
	}
}

func TestMapWireStatus(t *testing.T) {
	cases := []struct {
		in, wire, errCode string
	}{
		{StatusPending, WireQueued, ""},
		{StatusProcessing, WireInProgress, ""},
		{StatusCompleted, WireCompleted, ""},
		{StatusFailed, WireFailed, ""},
		{StatusCancelled, WireFailed, ErrCodeTaskCancelled},
		{StatusExpired, WireFailed, ErrCodeTaskExpired},
		{"nonsense", WireFailed, ""},
	}
	for _, c := range cases {
		wire, code := MapWireStatus(c.in)
		if wire != c.wire || code != c.errCode {
			t.Fatalf("MapWireStatus(%q) = (%q,%q), want (%q,%q)", c.in, wire, code, c.wire, c.errCode)
		}
	}
}

func TestNewResourceRendersRequiredShape(t *testing.T) {
	res := NewResource("vid_1", "sora-2", "piano cat", "720x1280", 4, 1700000000)
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"id", "object", "model", "status", "progress", "created_at",
		"completed_at", "expires_at", "prompt", "size", "seconds", "remixed_from_video_id", "error"} {
		if _, ok := doc[field]; !ok {
			t.Fatalf("required field %q missing from rendered resource: %s", field, raw)
		}
	}
	if doc["object"] != "video" || doc["status"] != WireQueued {
		t.Fatalf("object/status wrong: %s", raw)
	}
	// Seconds is a string on the resource even though the create call
	// sends an integer — the API defines it so, and strict SDKs follow.
	if s, _ := doc["seconds"].(string); s != "4" {
		t.Fatalf("seconds must render as the string \"4\", got %v", doc["seconds"])
	}
	for _, nullable := range []string{"completed_at", "expires_at", "remixed_from_video_id", "error"} {
		if doc[nullable] != nil {
			t.Fatalf("%s must render as null before it has a value, got %v", nullable, doc[nullable])
		}
	}
}

func TestRedactRequestBodyCoversUpstreamSpellings(t *testing.T) {
	// The same reference image travels under three spellings between the
	// caller's body and the native dialects' re-encodings; the redactor
	// must catch all of them or the pixels re-enter one hop after the
	// caller's copy was scrubbed.
	pixels := "QUJDREVG" + strings.Repeat("h6tF", 260) // >1000 base64 chars
	bodies := []string{
		`{"input_reference":{"image_url":"data:image/png;base64,` + pixels + `"}}`,                                                                      // caller
		`{"model":"m","input":{"img_url":"data:image/png;base64,` + pixels + `","prompt":"p"}}`,                                                         // legacy upstream
		`{"input":{"prompt":"p","media":[{"type":"first_frame","url":"data:image/png;base64,` + pixels + `"}]}}`,                                        // media upstream
		`{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + pixels + `"},"role":"first_frame"},{"type":"text","text":"p"}]}`, // ark upstream
	}
	for _, body := range bodies {
		out := RedactRequestBody([]byte(body))
		if strings.Contains(out, pixels) {
			t.Fatalf("reference pixels survived redaction: %.120s", out)
		}
		if !strings.Contains(out, "[base64 image omitted:") {
			t.Fatalf("omission note missing: %.120s", out)
		}
	}
	// Plain URLs under the nested key survive: an operator's debug row
	// needs the reference's address, just not its bytes.
	out := RedactRequestBody([]byte(`{"media":[{"url":"https://example.test/ref.png"}]}`))
	if !strings.Contains(out, "https://example.test/ref.png") {
		t.Fatalf("plain URLs must survive: %s", out)
	}
}
