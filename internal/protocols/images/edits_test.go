package images

// Tests for the edits wire shape: parsing a multipart upload far enough to
// route and bill it, re-encoding it with only the model swapped, and
// rendering it to the text form an audit row can hold. Each one builds the
// upload the way a caller's SDK would, so a wire-shape change anywhere in
// the multipart round trip turns a test red.

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"strings"
	"testing"
)

// pixelMarker stands in for image bytes: short, unmistakable, and never a
// substring of any form field, so "the pixels survived" and "the pixels were
// dropped" are both one strings.Contains away.
const pixelMarker = "\x00PIXELBYTES\x00"

// writeEditForm builds one edits upload from the parts the test adds.
func writeEditForm(t *testing.T, add func(w *multipart.Writer)) (contentType string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	add(w)
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return w.FormDataContentType(), buf.Bytes()
}

// writeImagePart adds one image file part under the given field name.
func writeImagePart(t *testing.T, w *multipart.Writer, field, fileName string) {
	t.Helper()
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="`+field+`"; filename="`+fileName+`"`)
	hdr.Set("Content-Type", "image/png")
	part, err := w.CreatePart(hdr)
	if err != nil {
		t.Fatalf("create image part: %v", err)
	}
	if _, err := part.Write([]byte(pixelMarker)); err != nil {
		t.Fatalf("write image part: %v", err)
	}
}

func writeField(t *testing.T, w *multipart.Writer, name, value string) {
	t.Helper()
	if err := w.WriteField(name, value); err != nil {
		t.Fatalf("write field %q: %v", name, err)
	}
}

// A full upload parses into exactly the routing- and billing-relevant
// fields, with every file accounted for: two reference images and a mask.
func TestParseEditRequestReadsFieldsAndFiles(t *testing.T) {
	ct, body := writeEditForm(t, func(w *multipart.Writer) {
		writeField(t, w, "model", "image-model")
		writeField(t, w, "prompt", "make the fox wear a hat")
		writeField(t, w, "n", "2")
		writeField(t, w, "size", "1024x1024")
		writeField(t, w, "quality", "high")
		writeField(t, w, "response_format", "url")
		writeImagePart(t, w, "image", "fox.png")
		writeImagePart(t, w, "image", "hat.png")
		writeImagePart(t, w, "mask", "mask.png")
	})

	req, err := ParseEditRequest(ct, body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Model != "image-model" || req.Prompt != "make the fox wear a hat" {
		t.Errorf("routing fields = %q / %q", req.Model, req.Prompt)
	}
	if req.N != 2 || req.Size != "1024x1024" || req.Quality != "high" || req.ResponseFormat != "url" {
		t.Errorf("billing axes = n:%d size:%q quality:%q format:%q", req.N, req.Size, req.Quality, req.ResponseFormat)
	}
	if len(req.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(req.Images))
	}
	for i, img := range req.Images {
		if string(img.Data) != pixelMarker {
			t.Errorf("image %d bytes differ from the upload", i)
		}
		if img.FileName == "" || img.ContentType != "image/png" {
			t.Errorf("image %d lost its headers: %q %q", i, img.FileName, img.ContentType)
		}
	}
	if req.Mask == nil || string(req.Mask.Data) != pixelMarker {
		t.Errorf("mask not captured: %+v", req.Mask)
	}
}

// The bracketed "image[]" spelling some SDK stacks emit means the same part
// as "image"; an unparsable n keeps the default rather than failing the
// parse (the passthrough forward keeps the caller's truth); unknown fields
// and unknown uploads are not the parser's business.
func TestParseEditRequestLenience(t *testing.T) {
	ct, body := writeEditForm(t, func(w *multipart.Writer) {
		writeField(t, w, "prompt", "brighten it")
		writeField(t, w, "n", "not-a-number")
		writeField(t, w, "user", "caller-supplied-opaque-field")
		writeField(t, w, "background", "transparent")
		writeImagePart(t, w, "image[]", "a.png")
		writeImagePart(t, w, "reference", "unrelated-upload.png")
	})

	req, err := ParseEditRequest(ct, body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.N != 1 {
		t.Errorf("n = %d, want the default 1", req.N)
	}
	if len(req.Images) != 1 {
		t.Fatalf("images = %d, want the image[] part counted", len(req.Images))
	}
	if req.Mask != nil {
		t.Errorf("unrelated upload collected as mask")
	}
	if req.Stream {
		t.Errorf("stream = true without a stream field")
	}
	// The neutral extra stays out of the unmappable list — refusing a mere
	// abuse-monitoring hint would break every OpenAI SDK default call —
	// while the gpt-image-only knob joins it, named.
	if len(req.UnmappedFields) != 1 || req.UnmappedFields[0] != "background" {
		t.Errorf("unmapped fields = %v, want exactly [background]", req.UnmappedFields)
	}
}

// Bodies the parser cannot read fail loudly: a non-multipart content type,
// a multipart type without a boundary, a body that breaks mid-part, a
// duplicate model field, and a vocabulary field long enough to be garbage.
func TestParseEditRequestRefusesBrokenBodies(t *testing.T) {
	ct, body := writeEditForm(t, func(w *multipart.Writer) {
		writeField(t, w, "model", "image-model")
		writeField(t, w, "prompt", "x")
		writeImagePart(t, w, "image", "a.png")
	})
	for _, tc := range []struct {
		name string
		ct   string
		body []byte
	}{
		{"json content type", "application/json", []byte(`{"model":"m"}`)},
		{"no boundary", "multipart/form-data", body},
		{"truncated body", ct, body[:len(body)/2]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEditRequest(tc.ct, tc.body); err == nil {
				t.Fatal("parse succeeded, want refusal")
			}
		})
	}

	t.Run("duplicate model", func(t *testing.T) {
		ct, body := writeEditForm(t, func(w *multipart.Writer) {
			writeField(t, w, "model", "one")
			writeField(t, w, "model", "two")
		})
		if _, err := ParseEditRequest(ct, body); err == nil {
			t.Fatal("duplicate model accepted, want refusal")
		}
	})

	t.Run("overlong vocabulary field", func(t *testing.T) {
		ct, body := writeEditForm(t, func(w *multipart.Writer) {
			writeField(t, w, "model", strings.Repeat("m", 2048))
		})
		if _, err := ParseEditRequest(ct, body); err == nil {
			t.Fatal("overlong model accepted, want refusal")
		}
	})
}

// The rewrite swaps the model field and nothing else: files byte for byte,
// other fields as written, and a fresh boundary whose content type comes
// back with the body it describes.
func TestRewriteEditModelFieldReplacesModelOnly(t *testing.T) {
	ct, body := writeEditForm(t, func(w *multipart.Writer) {
		writeField(t, w, "model", "image-model")
		writeField(t, w, "prompt", "make the fox wear a hat")
		writeField(t, w, "size", "1024x1024")
		writeImagePart(t, w, "image", "fox.png")
		writeImagePart(t, w, "mask", "mask.png")
	})

	out, outCT, err := RewriteEditModelField(ct, body, "provider-model")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !strings.HasPrefix(outCT, "multipart/form-data; boundary=") {
		t.Fatalf("content type = %q, want multipart with boundary", outCT)
	}
	req, err := ParseEditRequest(outCT, out)
	if err != nil {
		t.Fatalf("rewritten body does not parse: %v", err)
	}
	if req.Model != "provider-model" {
		t.Errorf("rewritten model = %q", req.Model)
	}
	if req.Prompt != "make the fox wear a hat" || req.Size != "1024x1024" {
		t.Errorf("untouched fields changed: %q %q", req.Prompt, req.Size)
	}
	if len(req.Images) != 1 || string(req.Images[0].Data) != pixelMarker || req.Images[0].FileName != "fox.png" {
		t.Errorf("image part did not survive byte for byte")
	}
	if req.Mask == nil || string(req.Mask.Data) != pixelMarker {
		t.Errorf("mask part did not survive byte for byte")
	}
}

// The audit rendering keeps the upload's shape — field names, file names,
// sizes, the prompt — and none of its pixels.
func TestRenderEditBodyForLogKeepsShapeNotPixels(t *testing.T) {
	ct, body := writeEditForm(t, func(w *multipart.Writer) {
		writeField(t, w, "model", "image-model")
		writeField(t, w, "prompt", "make the fox wear a hat")
		writeImagePart(t, w, "image", "fox.png")
	})

	got := RenderEditBodyForLog(ct, body)
	for _, want := range []string{
		`name="model"`, "image-model", "make the fox wear a hat",
		`name="image"`, `filename="fox.png"`, "[BINARY:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, pixelMarker) {
		t.Errorf("render kept the pixel bytes:\n%s", got)
	}

	// A non-multipart, non-UTF-8 body renders as its length rather than a
	// string a text column would refuse.
	if got := RenderEditBodyForLog("application/octet-stream", []byte{0xff, 0xfe}); got != "[BINARY:2 bytes]" {
		t.Errorf("non-utf8 render = %q", got)
	}
}
