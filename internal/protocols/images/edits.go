package images

// The edits half of the OpenAI Images API: multipart in, the generations
// JSON shape out. An edits upload carries pixels, so unlike generations —
// where a one-field JSON patch rewrites the model — the body is parsed once
// for routing and billing, re-encoded only to swap the model field, and
// rendered to a text placeholder form for the audit trail, which is not an
// image store either.

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EditPath is the ingress route of the edits half, and the egress route on
// OpenAI-compatible providers.
const EditPath = "/v1/images/edits"

// EditFile is one uploaded file of an edits request.
type EditFile struct {
	FieldName   string
	FileName    string
	ContentType string
	Data        []byte
}

// EditRequest is the routing- and billing-relevant view of a multipart
// edits upload. Fields the gateway does not act on stay only in the
// caller's bytes, which the passthrough forward carries as written.
type EditRequest struct {
	Model          string
	Prompt         string
	N              int
	Size           string
	Quality        string
	ResponseFormat string
	Stream         bool
	// Images holds every uploaded image part. The wire name is "image";
	// the bracketed "image[]" spelling some SDK stacks emit means the same
	// thing and is accepted for both it and the mask below.
	Images []EditFile
	// Mask is the mask upload when the caller sent one. The native edit
	// dialect has no mask field, so its presence becomes a per-candidate
	// verdict there rather than a door refusal.
	Mask *EditFile
	// UnmappedFields names the scalar fields the caller sent that only
	// exist in the gpt-image dialect — output-shaping knobs the native
	// dialect has no promise for. Collected rather than silently dropped:
	// a candidate that cannot carry them says so, so the caller learns the
	// edit they asked for is not the edit they would get.
	UnmappedFields []string
}

// gptImageOnlyFields are the edits form fields that shape a gpt-image
// output the native dialect cannot promise. Fields with a neutral reading
// (user is an abuse-monitoring hint, not an output knob) stay out: refusing
// those would break every OpenAI SDK default call for no semantic gain.
var gptImageOnlyFields = map[string]bool{
	"background":         true,
	"input_fidelity":     true,
	"moderation":         true,
	"output_compression": true,
	"output_format":      true,
	"partial_images":     true,
}

// multipartBoundary parses a multipart content type into its boundary,
// refusing what no reader in this package could make sense of: a type that
// is not multipart, or one that carries no boundary. The two strict readers
// share it; the audit renderer keeps its own lenient parse, whose failures
// are notes in its output rather than errors.
func multipartBoundary(contentType string) (string, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("invalid Content-Type: %w", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return "", fmt.Errorf("expected multipart/form-data, got %s", mediaType)
	}
	if params["boundary"] == "" {
		return "", fmt.Errorf("multipart Content-Type missing boundary")
	}
	return params["boundary"], nil
}

// formFieldCap bounds the scalar form fields the gateway acts on. They are
// enumerations and names, not essays; a field longer than this is not a
// value any candidate would accept, and truncating one would misroute.
const formFieldCap = 1024

// ParseEditRequest reads a multipart body far enough to route and bill it.
// It refuses what it cannot read — a content type that is not multipart, a
// missing boundary, a broken part — but does not judge presence: that is
// the modality's door, which answers the caller.
func ParseEditRequest(contentType string, body []byte) (*EditRequest, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return nil, err
	}

	out := &EditRequest{N: 1}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart part: %w", err)
		}
		if part.FileName() == "" {
			err = readEditField(part, out)
		} else {
			err = readEditFile(part, out)
		}
		_ = part.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readEditField reads one scalar form field into the request. The prompt is
// read whole — an edit instruction is forwarded, not sampled, and a
// silently truncated one would edit the wrong picture — while vocabulary
// fields are capped by formFieldCap and refused rather than truncated.
func readEditField(part *multipart.Part, out *EditRequest) error {
	name := part.FormName()
	data, err := io.ReadAll(io.LimitReader(part, formFieldCap+1))
	if err != nil {
		return fmt.Errorf("read form field %q: %w", name, err)
	}
	if len(data) > formFieldCap {
		return fmt.Errorf("form field %q exceeds %d bytes", name, formFieldCap)
	}
	val := strings.TrimSpace(string(data))
	switch name {
	case "model":
		if out.Model != "" {
			return fmt.Errorf("duplicate model field not allowed")
		}
		out.Model = val
	case "prompt":
		out.Prompt = val
	case "n":
		// An unparsable n keeps the default rather than failing the parse:
		// the passthrough forward keeps the caller's truth for the upstream
		// to judge, and the default is what the snapshot's Requested shows.
		if v, perr := strconv.Atoi(val); perr == nil && v > 0 {
			out.N = v
		}
	case "size":
		out.Size = val
	case "quality":
		out.Quality = val
	case "response_format":
		out.ResponseFormat = val
	case "stream":
		out.Stream = val == "true" || val == "1"
	default:
		if gptImageOnlyFields[name] {
			out.UnmappedFields = append(out.UnmappedFields, name)
		}
	}
	return nil
}

// readEditFile reads one file part into the request under its wire name.
// The bytes are already bounded by the ingress body cap, so a part cannot
// outgrow the memory its body already occupies. An upload under an unknown
// field name is not one the gateway acts on; the passthrough forward keeps
// it in the caller's bytes.
func readEditFile(part *multipart.Part, out *EditRequest) error {
	name := part.FormName()
	data, err := io.ReadAll(part)
	if err != nil {
		return fmt.Errorf("read file field %q: %w", name, err)
	}
	file := EditFile{
		FieldName:   name,
		FileName:    part.FileName(),
		ContentType: part.Header.Get("Content-Type"),
		Data:        data,
	}
	switch name {
	case "image", "image[]":
		out.Images = append(out.Images, file)
	case "mask", "mask[]":
		out.Mask = &file
	}
	return nil
}

// RewriteEditModelField re-encodes a multipart body with the model field
// replaced and every other part — files, headers, unknown fields — copied
// through byte for byte. The writer chooses a fresh boundary, so the caller
// must send the returned content type with the returned body. Encoding a
// 20 MiB upload is expensive enough that the caller is expected to cache
// the result per target model rather than re-encode per attempt.
func RewriteEditModelField(contentType string, body []byte, model string) ([]byte, string, error) {
	boundary, err := multipartBoundary(contentType)
	if err != nil {
		return nil, "", err
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read multipart part: %w", err)
		}

		// Carry the original headers — disposition, filename, content type —
		// so the rebuilt part describes the same file the caller uploaded.
		header := make(textproto.MIMEHeader, len(part.Header))
		for k, vs := range part.Header {
			header[k] = append([]string(nil), vs...)
		}
		dest, err := writer.CreatePart(header)
		if err != nil {
			_ = part.Close()
			return nil, "", fmt.Errorf("create multipart part: %w", err)
		}

		if part.FormName() == "model" && part.FileName() == "" {
			_, werr := dest.Write([]byte(model))
			if werr != nil {
				_ = part.Close()
				return nil, "", fmt.Errorf("write rewritten model: %w", werr)
			}
			// Drain the original so the reader's internal state is left
			// consistent for the next part.
			_, _ = io.Copy(io.Discard, part)
		} else {
			if _, cerr := io.Copy(dest, part); cerr != nil {
				_ = part.Close()
				return nil, "", fmt.Errorf("copy multipart part: %w", cerr)
			}
		}
		_ = part.Close()
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart writer: %w", err)
	}
	return buf.Bytes(), writer.FormDataContentType(), nil
}

// RenderEditBodyForLog renders a multipart body as the text an audit row
// can hold: file parts become a note of their name and size, text fields
// keep their value capped per field. A 20 MiB upload's diagnostic value is
// its shape — which fields, which files, how big — and that survives; the
// pixels do not, which is the point.
func RenderEditBodyForLog(contentType string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if !strings.HasPrefix(contentType, "multipart/") {
		if !utf8.Valid(body) {
			return fmt.Sprintf("[BINARY:%d bytes]", len(body))
		}
		return string(body)
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return fmt.Sprintf("[multipart parse error: %v, %d bytes]", err, len(body))
	}
	boundary := params["boundary"]
	if boundary == "" {
		return fmt.Sprintf("[multipart missing boundary, %d bytes]", len(body))
	}

	var buf strings.Builder
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_, _ = fmt.Fprintf(&buf, "[multipart read error: %v]\n", err)
			break
		}
		_, _ = fmt.Fprintf(&buf, "--%s\nname=%q", boundary, part.FormName())
		if fileName := part.FileName(); fileName != "" {
			_, _ = fmt.Fprintf(&buf, " filename=%q", fileName)
		}
		if ct := part.Header.Get("Content-Type"); ct != "" {
			_, _ = fmt.Fprintf(&buf, " content-type=%s", ct)
		}
		buf.WriteString("\n\n")
		if part.FileName() != "" {
			n, _ := io.Copy(io.Discard, part)
			_, _ = fmt.Fprintf(&buf, "[BINARY:%d bytes]\n", n)
		} else {
			data, _ := io.ReadAll(io.LimitReader(part, 4*1024))
			if utf8.Valid(data) {
				buf.Write(data)
			} else {
				_, _ = fmt.Fprintf(&buf, "[non-utf8: %d bytes]", len(data))
			}
			buf.WriteString("\n")
		}
		_ = part.Close()
	}
	_, _ = fmt.Fprintf(&buf, "--%s--\n", boundary)
	return buf.String()
}
