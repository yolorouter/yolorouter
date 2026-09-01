package gateway

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// A multipart upload is the payload that breaks the assumption every JSON
// modality is built on: that a body can be parsed, edited, and written back.
// It cannot. Changing one field means decoding every part and re-encoding them
// under a NEW boundary, which produces a body whose content type no longer
// matches the one the request arrived with.
//
// Three consequences, and the fake below leans on each:
//   - a rewritten body is useless without its content type, so the interface
//     that carries the body has to carry that too;
//   - re-encoding twenty megabytes is not something to do once per candidate,
//     so the result has to survive between attempts;
//   - the parts are binary, so what goes in the audit row cannot be the bytes.

const fakeImagePart = "\x89PNG\r\n\x1a\n" + "not a real png, but not text either"

// fakeMultipartPayload stands in for an image-edit request: a couple of text
// fields and a file, where the model name is one of the text fields and has to
// be rewritten per candidate.
type fakeMultipartPayload struct {
	fakePayloadBase

	model       string
	contentType string
	body        []byte

	// encoded is the re-encoded body kept for reuse, and encodedFor is the
	// candidate it was built for. Both live on the payload rather than in the
	// method that built them, which is the whole reason a payload is an object:
	// a local variable cannot outlive the attempt that produced it.
	encoded     *UpstreamCall
	encodedFor  string
	encodeCount int
}

func (p *fakeMultipartPayload) Routing() RoutingIntent {
	return RoutingIntent{Model: p.model}
}

// PrepareUpstream re-encodes the whole body with the candidate's model name.
//
// The cache is keyed on the candidate's model name rather than simply "have I
// encoded before", because that is what the encoding depends on: a second
// candidate at the same provider name reuses the work, a different one cannot.
func (p *fakeMultipartPayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	if p.encoded != nil && p.encodedFor == cand.ProviderModelName {
		return p.encoded, nil
	}
	body, contentType, err := reencodeMultipart(p.body, p.contentType, cand.ProviderModelName)
	if err != nil {
		return nil, err
	}
	p.encodeCount++
	p.encoded = &UpstreamCall{
		Path: "/v1/images/edits", Body: body, ContentType: contentType,
	}
	p.encodedFor = cand.ProviderModelName
	return p.encoded, nil
}

// LogPolicy declares which bodies of this exchange may be stored.
//
// The response bodies are left out because they are the audio or the image, and
// this is the modality that knows that. MaxBytes is left at zero rather than
// set to a plausible-looking cap: nothing reads a LogPolicy yet, and a limit
// stated here would be a number no test can show taking effect.
func (p *fakeMultipartPayload) LogPolicy() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{BodyClientRequest: BodyStoredRaw, BodyUpstreamRequest: BodyStoredRaw}}
}

// SanitizeForLog renders a multipart body as its shape rather than its bytes.
//
// A binary part written verbatim into a text column is unreadable at best and
// several megabytes of it at worst. What an operator actually needs from this
// row is which parts were sent and how big each one was.
func (p *fakeMultipartPayload) SanitizeForLog(k BodyKind, contentType string, body []byte) string {
	if k != BodyClientRequest && k != BodyUpstreamRequest {
		return ""
	}
	parts, err := readMultipart(body, contentType)
	if err != nil {
		return "multipart: unreadable"
	}
	var b strings.Builder
	for _, part := range parts {
		if part.filename != "" {
			fmt.Fprintf(&b, "%s=<%d bytes binary>;", part.name, len(part.value))
			continue
		}
		fmt.Fprintf(&b, "%s=%s;", part.name, part.value)
	}
	return b.String()
}

type multipartField struct {
	name     string
	filename string
	value    string
}

func readMultipart(body []byte, contentType string) ([]multipartField, error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, err
	}
	boundary, ok := params["boundary"]
	if !ok {
		return nil, fmt.Errorf("no boundary in %q", contentType)
	}
	r := multipart.NewReader(bytes.NewReader(body), boundary)
	form, err := r.ReadForm(1 << 20)
	if err != nil {
		return nil, err
	}
	defer func() { _ = form.RemoveAll() }()

	var out []multipartField
	for name, values := range form.Value {
		for _, v := range values {
			out = append(out, multipartField{name: name, value: v})
		}
	}
	for name, files := range form.File {
		for _, fh := range files {
			f, err := fh.Open()
			if err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(f); err != nil {
				_ = f.Close()
				return nil, err
			}
			_ = f.Close()
			out = append(out, multipartField{name: name, filename: fh.Filename, value: buf.String()})
		}
	}
	return out, nil
}

// reencodeMultipart rebuilds the body with a different model, under a boundary
// of its own choosing, and returns the content type that describes it.
func reencodeMultipart(body []byte, contentType, model string) ([]byte, string, error) {
	parts, err := readMultipart(body, contentType)
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	w := multipart.NewWriter(&out)
	for _, part := range parts {
		value := part.value
		if part.name == "model" {
			value = model
		}
		if part.filename != "" {
			fw, err := w.CreateFormFile(part.name, part.filename)
			if err != nil {
				return nil, "", err
			}
			if _, err := fw.Write([]byte(value)); err != nil {
				return nil, "", err
			}
			continue
		}
		if err := w.WriteField(part.name, value); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return out.Bytes(), w.FormDataContentType(), nil
}

// newFakeMultipartRequest builds a caller's request: a model field, a prompt,
// and a binary file.
func newFakeMultipartRequest(t *testing.T, model string) (body []byte, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("model", model); err != nil {
		t.Fatalf("write model field: %v", err)
	}
	if err := w.WriteField("prompt", "make it blue"); err != nil {
		t.Fatalf("write prompt field: %v", err)
	}
	fw, err := w.CreateFormFile("image", "source.png")
	if err != nil {
		t.Fatalf("create file part: %v", err)
	}
	if _, err := fw.Write([]byte(fakeImagePart)); err != nil {
		t.Fatalf("write file part: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

// fakeMultipartModality admits a multipart body. It refuses anything else, for
// the reason the interface hands it the content type at all: without the
// boundary the body cannot be read, and a modality that guessed would be
// parsing a body it had not been told the shape of.
type fakeMultipartModality struct{}

func (fakeMultipartModality) ID() ModalityID         { return ModalityID("fake_multipart") }
func (fakeMultipartModality) Limits() TransferLimits { return TransferLimits{} }

func (fakeMultipartModality) Admit(_ context.Context, in Ingress) (Payload, *Rejection) {
	mediaType, _, err := mime.ParseMediaType(in.ContentType)
	if err != nil || mediaType != "multipart/form-data" {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: "invalid_request_error",
			Message: "expected a multipart body", FailReason: "not_multipart", Fault: fact.FaultClient,
		}
	}
	fields, err := readMultipart(in.Body, in.ContentType)
	if err != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: "invalid_request_error",
			Message: "unreadable multipart body", FailReason: "multipart_unreadable", Fault: fact.FaultClient,
		}
	}
	model := ""
	for _, f := range fields {
		if f.name == "model" {
			model = f.value
		}
	}
	return &fakeMultipartPayload{model: model, contentType: in.ContentType, body: in.Body}, nil
}

// TestARewrittenMultipartBodyCarriesItsOwnContentType is the first of the three.
//
// The boundary is not decoration: a body re-encoded under a new boundary and
// sent with the old content type is a body the upstream cannot parse, and the
// failure is a 400 that looks like the caller's fault. There is nowhere else
// for the new content type to travel — it is not derivable from the bytes by
// anybody downstream — so the type carrying the body has to carry it.
func TestARewrittenMultipartBodyCarriesItsOwnContentType(t *testing.T) {
	body, contentType := newFakeMultipartRequest(t, "gpt-image-1")

	payload, rej := fakeMultipartModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/images/edits",
		ContentType: contentType, Body: body,
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid multipart body: %+v", rej)
	}

	call, err := payload.PrepareUpstream(Candidate{ProviderModelName: "provider-image-model"})
	if err != nil {
		t.Fatalf("PrepareUpstream: %v", err)
	}

	if call.ContentType == contentType {
		t.Fatal("the re-encoded body came back under the caller's content type; " +
			"either nothing was re-encoded or the boundary was reused")
	}
	if !strings.HasPrefix(call.ContentType, "multipart/form-data; boundary=") {
		t.Fatalf("content type = %q, want one naming the new boundary", call.ContentType)
	}

	// The proof that the two agree is that the body parses under the type.
	parts, err := readMultipart(call.Body, call.ContentType)
	if err != nil {
		t.Fatalf("the upstream body does not parse under the content type it was sent with: %v", err)
	}
	var model, file string
	for _, p := range parts {
		switch p.name {
		case "model":
			model = p.value
		case "image":
			file = p.value
		}
	}
	if model != "provider-image-model" {
		t.Errorf("model = %q, want the candidate's name", model)
	}
	if file != fakeImagePart {
		t.Errorf("the binary part did not survive the re-encode")
	}
}

// TestAMultipartBodyIsReEncodedOncePerCandidateNotOncePerKey pins the cache.
//
// Key rotation retries the same candidate with a different credential. The body
// does not depend on the credential, and re-encoding a large upload for each
// key would spend that cost again for nothing — which is exactly the shape of
// the state a payload exists to hold: it belongs to the request, outlives one
// attempt, and has no business on the exchange.
func TestAMultipartBodyIsReEncodedOncePerCandidateNotOncePerKey(t *testing.T) {
	body, contentType := newFakeMultipartRequest(t, "gpt-image-1")
	payload, rej := fakeMultipartModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/images/edits",
		ContentType: contentType, Body: body,
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid multipart body: %+v", rej)
	}
	fake := payload.(*fakeMultipartPayload)

	first, err := payload.PrepareUpstream(Candidate{ProviderModelName: "same"})
	if err != nil {
		t.Fatalf("PrepareUpstream: %v", err)
	}
	again, err := payload.PrepareUpstream(Candidate{ProviderModelName: "same"})
	if err != nil {
		t.Fatalf("PrepareUpstream: %v", err)
	}
	if fake.encodeCount != 1 {
		t.Errorf("re-encoded %d times for one candidate, want 1", fake.encodeCount)
	}
	if !bytes.Equal(first.Body, again.Body) || first.ContentType != again.ContentType {
		t.Error("the second attempt on the same candidate got a different body")
	}

	// A different candidate is a different body, so the cache must not answer.
	other, err := payload.PrepareUpstream(Candidate{ProviderModelName: "different"})
	if err != nil {
		t.Fatalf("PrepareUpstream: %v", err)
	}
	if fake.encodeCount != 2 {
		t.Errorf("re-encoded %d times across two candidates, want 2 — a cache that answers "+
			"for the wrong candidate sends the previous provider's model name", fake.encodeCount)
	}
	if bytes.Equal(other.Body, first.Body) {
		t.Error("the second candidate got the first candidate's body")
	}
}

// TestABinaryPartIsRecordedAsItsShapeNotItsBytes pins the third.
//
// The audit trail has one hook for this and it belongs to the modality, because
// only the modality knows what its bytes are. A logging capability that assumed
// text would write the file into the row: unreadable, and megabytes of it.
func TestABinaryPartIsRecordedAsItsShapeNotItsBytes(t *testing.T) {
	body, contentType := newFakeMultipartRequest(t, "gpt-image-1")
	payload, rej := fakeMultipartModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/images/edits",
		ContentType: contentType, Body: body,
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid multipart body: %+v", rej)
	}

	got := payload.SanitizeForLog(BodyClientRequest, contentType, body)

	if strings.Contains(got, fakeImagePart) {
		t.Error("the binary part went into the audit row verbatim")
	}
	if !strings.Contains(got, "model=gpt-image-1") || !strings.Contains(got, "prompt=make it blue") {
		t.Errorf("sanitized = %q, want the text fields kept: a row that drops them tells nobody "+
			"what was asked for", got)
	}
	if !strings.Contains(got, fmt.Sprintf("image=<%d bytes binary>", len(fakeImagePart))) {
		t.Errorf("sanitized = %q, want the file recorded as its size — a part dropped entirely "+
			"is indistinguishable from one that was never sent", got)
	}
}

// TestAMultipartBodyThatCannotBeReadIsItsOwnRefusal covers the second rule,
// which the first one hides.
//
// A body announced as multipart/form-data with no boundary parameter passes the
// content-type check and fails the parse: the media type is right, and there is
// no way to tell where one part ends and the next begins. Both refusals are
// 400s from the caller, so only the reason code tells them apart — and "you
// sent JSON" leads somewhere very different from "your form has no boundary".
// Without this the parse branch is unreachable from any test: every other case
// that reaches it is one the first rule already refused.
func TestAMultipartBodyThatCannotBeReadIsItsOwnRefusal(t *testing.T) {
	_, rej := fakeMultipartModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/images/edits",
		ContentType: "multipart/form-data",
		Body:        []byte("--abc\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ngpt\r\n--abc--\r\n"),
	})
	if rej == nil {
		t.Fatal("a form with no boundary was admitted; nothing can say where its parts are")
	}
	if rej.FailReason != "multipart_unreadable" {
		t.Errorf("FailReason = %q, want multipart_unreadable: refused by the content-type rule "+
			"instead of the parse, so the parse branch is still untested", rej.FailReason)
	}
	if rej.Status != http.StatusBadRequest || rej.Fault != fact.FaultClient {
		t.Errorf("status/fault = %d/%v, want 400 and the caller's", rej.Status, rej.Fault)
	}
}

// TestANonMultipartBodyIsRefusedBeforeAnyUpstreamRuns closes the loop on why
// the content type is part of the ingress at all.
func TestANonMultipartBodyIsRefusedBeforeAnyUpstreamRuns(t *testing.T) {
	_, rej := fakeMultipartModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/images/edits",
		ContentType: "application/json", Body: []byte(`{"model":"x"}`),
	})
	if rej == nil {
		t.Fatal("a JSON body was admitted as multipart; the boundary it needs does not exist")
	}
	if rej.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: the caller can fix this, and any 5xx tells them to "+
			"retry a request that will never work", rej.Status)
	}
	if rej.FailReason != "not_multipart" {
		t.Errorf("FailReason = %q, want not_multipart: the reason code is what a dashboard groups "+
			"these refusals by, and the two refusals below it are different problems", rej.FailReason)
	}
	if rej.Fault != fact.FaultClient {
		t.Errorf("fault = %v, want the caller's: they sent the wrong shape", rej.Fault)
	}
}
