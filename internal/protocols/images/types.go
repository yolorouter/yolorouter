// Package images owns the wire shapes of the OpenAI Images API: what a
// caller sends to generate images, and the subset of it the gateway reads to
// route and bill. Conversion for providers whose native API is not this
// shape lives here too, one file per provider dialect.
package images

import "encoding/json"

// Request is the routing-relevant subset of an images/generations request.
//
// Only the fields the gateway itself reads are modelled. Everything else a
// caller sends — style, background, user, provider-private extensions — is
// not decoded here at all: the body is forwarded with only the model field
// rewritten, so an unmodelled field reaches the upstream exactly as the
// caller wrote it, which is the property passthrough exists for.
type Request struct {
	// Model is the name the caller used, before any candidate rewrites it.
	Model string `json:"model"`
	// Prompt is what the caller asked to see. Required by the API and
	// refused at admission rather than after a candidate was chosen.
	Prompt string `json:"prompt"`
	// Stream asks for progressive partial images. Not served: admission
	// refuses it so the caller learns immediately instead of mid-request.
	Stream bool `json:"stream"`
	// N is how many images the caller asked for. Zero is the API's own
	// default (one) and is normalized by the reader that cares, not here —
	// a struct field that silently rewrites its input hides the difference
	// between "asked for one" and "asked for none".
	N int `json:"n"`
	// Quality and Size are the pricing axes of a per-image request: a tier
	// table is keyed by the pair. Carried verbatim, empty when unset.
	Quality string `json:"quality"`
	Size    string `json:"size"`
	// ResponseFormat says which delivery shape the caller asked for ("url"
	// or "b64_json"). Some upstreams can only answer with URLs, so a
	// b64_json ask is one a candidate may have to refuse.
	ResponseFormat string `json:"response_format"`
}

// ParseRequest decodes the routing-relevant fields of an images request.
//
// Unknown fields are ignored by design (see Request). A body that is not a
// JSON object is an error; a JSON object that carries the wrong types for
// the modelled fields is an error too, because forwarding a body whose
// model field is not a string would send the upstream a request the gateway
// could not have rewritten honestly.
func ParseRequest(body []byte) (*Request, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// Response is the delivered-images subset of an images/generations response:
// how many images actually came back, and the token counts the API reports
// for the models that bill that way.
//
// The delivered body is forwarded verbatim; this type exists for the numbers
// the gateway needs after the fact (billing counts images, some models bill
// tokens), not for re-encoding anything.
type Response struct {
	Created int64 `json:"created"`
	Data    []struct {
		URL           string `json:"url"`
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// ParseResponse decodes the countable fields of an images response. The
// bytes the caller receives are never rebuilt from this.
func ParseResponse(body []byte) (*Response, error) {
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ImageCount is how many images the response actually delivered. Zero when
// the response carried no data array at all — the "succeeded but produced
// nothing" shape a caller cannot use and must not be billed for.
func (r *Response) ImageCount() int {
	if r == nil {
		return 0
	}
	return len(r.Data)
}
