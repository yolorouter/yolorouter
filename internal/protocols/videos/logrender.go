package videos

// The audit renderer: what an audit row may hold of a create call. The
// pixels of a reference upload are worth less to a debug row than the
// row's own size, and a base64 data URL in a JSON body is the same pixels
// in different clothes — both render as their shape, not their bytes.

import (
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"unicode/utf8"
)

// RenderBodyForLog renders a create body as the text an audit row can
// hold: file parts become a note of their name and size, text fields keep
// their value capped per field, and a body that is not multipart renders
// as redacted JSON (see RedactRequestBody). The renderer is deliberately
// lenient where the strict parser is not: its failures are notes in its
// output, not errors, because an audit row about a broken body is more
// useful than no row at all.
func RenderBodyForLog(contentType string, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if !strings.HasPrefix(contentType, "multipart/") {
		if !utf8.Valid(body) {
			return fmt.Sprintf("[BINARY:%d bytes]", len(body))
		}
		return RedactRequestBody(body)
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return fmt.Sprintf("[multipart parse error: %v, %d bytes]", err, len(body))
	}
	b := params["boundary"]
	if b == "" {
		return fmt.Sprintf("[multipart missing boundary, %d bytes]", len(body))
	}

	var buf strings.Builder
	reader := multipart.NewReader(strings.NewReader(string(body)), b)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			_, _ = fmt.Fprintf(&buf, "[multipart read error: %v]\n", err)
			break
		}
		_, _ = fmt.Fprintf(&buf, "--%s\nname=%q", b, part.FormName())
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
	_, _ = fmt.Fprintf(&buf, "--%s--\n", b)
	return buf.String()
}
