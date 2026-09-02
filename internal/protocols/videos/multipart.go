package videos

// The multipart half of the create call. The official SDK always posts
// this shape — it forces the multipart content type before sending — so
// this reader is not a compatibility afterthought: it is the body most
// real callers arrive with. Field names match the SDK's form encoding:
// scalar knobs as text fields, the reference image either as a file part
// or as a JSON-serialized text field under the same name.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"strconv"
	"strings"
)

// boundary parses a multipart content type into its boundary, refusing
// what no reader could make sense of: a type that is not multipart, or
// one that carries no boundary.
func boundary(contentType string) (string, error) {
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

// formFieldCap bounds the scalar form fields the gateway acts on. They
// are enumerations, names, and short references; a field longer than
// this is not a value any candidate would accept, and truncating one
// would misroute. The prompt is exempt — it is forwarded whole or not at
// all, and the ingress body cap already bounds its size.
const formFieldCap = 1024

// parseMultipartCreate reads a multipart create body far enough to route
// it. It refuses what it cannot read — a content type that is not
// multipart, a missing boundary, a broken part, a scalar field that
// cannot be the shape its JSON counterpart would be — but does not judge
// presence or legality of values: that is the modality's door.
func parseMultipartCreate(contentType string, body []byte) (*CreateRequest, error) {
	b, err := boundary(contentType)
	if err != nil {
		return nil, err
	}

	out := &CreateRequest{}
	reader := multipart.NewReader(bytes.NewReader(body), b)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read multipart part: %w", err)
		}
		if part.FileName() == "" {
			err = readCreateField(part, out)
		} else {
			err = readCreateFile(part, out)
		}
		_ = part.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readCreateField reads one scalar form field into the request. The
// input_reference field is the one surprise: when it is not a file part
// it carries the JSON object form, so the field's bytes are parsed as
// that object rather than kept as a string.
func readCreateField(part *multipart.Part, out *CreateRequest) error {
	name := part.FormName()
	if name == "prompt" {
		data, err := io.ReadAll(part)
		if err != nil {
			return fmt.Errorf("read form field %q: %w", name, err)
		}
		out.Prompt = strings.TrimSpace(string(data))
		return nil
	}

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
	case "seconds":
		if val == "" {
			return nil
		}
		v, perr := strconv.Atoi(val)
		if perr != nil {
			return fmt.Errorf("field %q is not an integer: %w", name, perr)
		}
		out.Seconds = v
	case "size":
		out.Size = val
	case "input_reference":
		var ref inputRefJSON
		if jerr := json.Unmarshal([]byte(val), &ref); jerr != nil {
			return fmt.Errorf("field %q is not a reference object: %w", name, jerr)
		}
		if out.InputReference == nil {
			out.InputReference = &InputRef{}
		}
		out.InputReference.ImageURL = ref.ImageURL
		out.InputReference.FileID = ref.FileID
	}
	return nil
}

// readCreateFile reads one file part into the request. The only file the
// create call defines is the input_reference image; the bytes are already
// bounded by the ingress body cap, so a part cannot outgrow the memory
// its body already occupies. An upload under an unknown field name is not
// one the gateway acts on; it is ignored here and the door decides what
// to tell the caller, keeping the reader free of vocabulary it would only
// duplicate.
func readCreateFile(part *multipart.Part, out *CreateRequest) error {
	name := part.FormName()
	if name != "input_reference" {
		_, _ = io.Copy(io.Discard, part)
		return nil
	}
	data, err := io.ReadAll(part)
	if err != nil {
		return fmt.Errorf("read file field %q: %w", name, err)
	}
	if out.InputReference == nil {
		out.InputReference = &InputRef{}
	}
	out.InputReference.File = &File{
		FieldName:   name,
		FileName:    part.FileName(),
		ContentType: part.Header.Get("Content-Type"),
		Data:        data,
	}
	return nil
}
