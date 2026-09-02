// Package wan speaks Alibaba DashScope's native video task dialect: the
// submit-then-poll conversation the wan model families run over
// X-DashScope-Async. Two request shapes exist and the model family decides
// which one a submit carries — wan2.7 and newer take the input.media[]
// array, older families the flat input.img_url — and the poll side is one
// shape for all of them: GET /api/v1/tasks/{id} with the six-value
// task_status vocabulary.
package wan

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// SubmitPath is the submit route, served from the provider's ORIGIN
// (scheme://host), not its versioned base: a DashScope provider's
// configured base points at the OpenAI-compatible tree while the native
// task tree hangs off the same host's root.
const SubmitPath = "/api/v1/services/aigc/video-generation/video-synthesis"

// AsyncHeader must ride every submit or the endpoint refuses the call
// with "current user api does not support synchronous calls" — the one
// header that distinguishes a task submit from a sync generation.
const AsyncHeader = "X-DashScope-Async"

// Family is which request shape a model name submits with.
type Family int

const (
	// FamilyNone means the name is not a wan video model this dialect
	// knows; a candidate carrying it is refused per candidate rather than
	// guessed at.
	FamilyNone Family = iota
	// FamilyMedia is wan2.7 and newer (including wan3.0): input.media[]
	// with typed entries.
	FamilyMedia
	// FamilyLegacy is wan2.1 through wan2.6 (and the wanx spellings):
	// the flat input.img_url form.
	FamilyLegacy
)

// ModelFamily classifies a provider model name for the request shape.
// Only the wan families the dialect has read shape documentation for are
// classified; anything else is FamilyNone and refused per candidate
// rather than guessed at — adding a family means reading its docs, not
// widening a prefix.
func ModelFamily(providerModel string) Family {
	name := strings.ToLower(providerModel)
	switch {
	case strings.HasPrefix(name, "wan3."), strings.HasPrefix(name, "wan2.7"):
		return FamilyMedia
	case strings.HasPrefix(name, "wan"), strings.HasPrefix(name, "wanx"):
		return FamilyLegacy
	}
	return FamilyNone
}

// MapSize maps a dialect size (WIDTHxHEIGHT) onto the two axes DashScope
// wants — resolution tier and aspect ratio — by nearest neighbor, since
// the dialect's four sizes are pixel coordinates the upstream's coarse
// tiers only approximate. The resolution half is also the pricing tier
// key, so the mapping is written once here and both readers use it.
func MapSize(dialectSize string) (resolution, ratio string, ok bool) {
	switch dialectSize {
	case "720x1280":
		return "720P", "9:16", true
	case "1280x720":
		return "720P", "16:9", true
	case "1024x1792":
		return "1080P", "9:16", true
	case "1792x1024":
		return "1080P", "16:9", true
	}
	return "", "", false
}

// SubmitRequest is one video submit, already mapped: the resolution and
// ratio came from MapSize, the duration is the dialect's seconds verbatim.
// Reference is the caller's input_reference already resolved to bytes or
// a URL — at most one of the two is set; a submit without one is a text
// generation.
type SubmitRequest struct {
	Model      string
	Prompt     string
	Resolution string
	Ratio      string
	Duration   int
	// RefURL is the reference image as the caller gave it (a URL or a
	// data URI); RefData/RefContentType is the uploaded-file form. The
	// file form is normalized to a data URI here, because the upstream
	// accepts the same spelling for both and the family shapes differ
	// only in the field that carries it.
	RefURL         string
	RefData        []byte
	RefContentType string
}

// EncodeSubmit builds the native submit body for the model's family.
//
// The ratio rule both families share: a text-to-video submit states the
// aspect ratio it wants; an image-referenced one does not, because the
// reference image decides the aspect and the upstream's ratio knob is
// documented as ignored (or invalid) there.
func EncodeSubmit(req SubmitRequest) ([]byte, error) {
	family := ModelFamily(req.Model)
	if family == FamilyNone {
		return nil, fmt.Errorf("model %q is not one the wan video dialect knows", req.Model)
	}
	ref := req.RefURL
	if ref == "" && len(req.RefData) > 0 {
		ref = dataURI(req.RefContentType, req.RefData)
	}

	// prompt_extend on is this gateway's standing choice (a short prompt
	// paints a better picture expanded), and watermark off is the neutral
	// delivery default. Both are the gateway's own knobs, not the
	// caller's: the dialect exposes no field for them.
	params := map[string]any{
		"resolution":    req.Resolution,
		"duration":      req.Duration,
		"prompt_extend": true,
		"watermark":     false,
	}
	if ref == "" && req.Ratio != "" {
		params["ratio"] = req.Ratio
	}

	var input map[string]any
	switch family {
	case FamilyMedia:
		// wan2.7 and newer: input.prompt with the reference, when there is
		// one, as a typed input.media[] entry — the flat form the
		// image-to-video and wan3.0 references both document.
		input = map[string]any{"prompt": req.Prompt}
		if ref != "" {
			input["media"] = []any{map[string]any{"type": "first_frame", "url": ref}}
		}
	case FamilyLegacy:
		input = map[string]any{"prompt": req.Prompt}
		if ref != "" {
			input["img_url"] = ref
		}
	}
	return json.Marshal(map[string]any{"model": req.Model, "input": input, "parameters": params})
}

// dataURI renders uploaded bytes as the data URI the upstream accepts,
// sniffing the content type when the upload's own header is missing or is
// the multipart default curl sends — the same lesson the images dialect
// learned: an octet-stream from a file part is still an image here.
func dataURI(contentType string, data []byte) string {
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// TaskObservation is one poll's answer in normalized form; the status
// spellings are the gateway's six, with the vendor's UNKNOWN already
// mapped to expired — a task the upstream cannot know is one this gateway
// must stop asking about.
type TaskObservation struct {
	Status       string
	VideoURL     string
	UsageSecs    int
	ErrorCode    string
	ErrorMessage string
}

// BizError is a refusal that arrived inside an HTTP 200: the request was
// rejected, not the transport.
type BizError struct {
	Code    string
	Message string
}

func (e *BizError) Error() string { return e.Code + ": " + e.Message }

// submitResponse is the wire shape of a submit answer.
type submitResponse struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	} `json:"output"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// ParseSubmitResponse reads a submit answer: a task id to keep asking
// about, a business refusal to answer the caller with, or a shape this
// dialect cannot read.
func ParseSubmitResponse(body []byte) (taskID string, biz *BizError, err error) {
	var resp submitResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", nil, fmt.Errorf("wan submit response is not JSON: %w", jerr)
	}
	if resp.Code != "" {
		return "", &BizError{Code: resp.Code, Message: resp.Message}, nil
	}
	if resp.Output.TaskID == "" {
		return "", nil, fmt.Errorf("wan submit response carries no task_id")
	}
	return resp.Output.TaskID, nil, nil
}

// taskResponse is the wire shape of a poll answer.
type taskResponse struct {
	Output struct {
		TaskStatus string `json:"task_status"`
		VideoURL   string `json:"video_url"`
		Code       string `json:"code"`
		Message    string `json:"message"`
	} `json:"output"`
	Usage struct {
		Duration int `json:"duration"`
	} `json:"usage"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mapTaskStatus maps the vendor's status word onto the gateway's task
// vocabulary, with the error-channel words for the states the wire cannot
// say directly.
func mapTaskStatus(word string) (status, errCode, errMsg string, err error) {
	switch word {
	case "PENDING":
		return "pending", "", "", nil
	case "RUNNING":
		return "processing", "", "", nil
	case "SUCCEEDED":
		return "completed", "", "", nil
	case "FAILED":
		return "failed", "upstream_failed", "", nil
	case "CANCELED":
		return "cancelled", "task_cancelled", "", nil
	case "UNKNOWN":
		// The vendor's own "this id means nothing to me" — past the task
		// window or never theirs. Asking again cannot change the answer.
		return "expired", "task_expired", "", nil
	}
	return "", "", "", fmt.Errorf("wan task_status %q is outside the documented vocabulary", word)
}

// ParseTaskResponse reads a poll answer into the normalized observation.
// The observation is built in one literal — the fields say what the status
// word means, so they are decided where it is decided, not patched on
// afterwards.
func ParseTaskResponse(body []byte) (TaskObservation, *BizError, error) {
	var resp taskResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return TaskObservation{}, nil, fmt.Errorf("wan task response is not JSON: %w", jerr)
	}
	if resp.Code != "" {
		return TaskObservation{}, &BizError{Code: resp.Code, Message: resp.Message}, nil
	}
	status, errCode, errMsg, merr := mapTaskStatus(resp.Output.TaskStatus)
	if merr != nil {
		return TaskObservation{}, nil, merr
	}
	// A FAILED answer names itself in the output block; the stock code is
	// only the fallback for one that arrived nameless.
	if status == "failed" {
		errMsg = resp.Output.Message
		if resp.Output.Code != "" {
			errCode = resp.Output.Code
		}
	}
	return TaskObservation{
		Status:       status,
		VideoURL:     resp.Output.VideoURL,
		UsageSecs:    resp.Usage.Duration,
		ErrorCode:    errCode,
		ErrorMessage: errMsg,
	}, nil, nil
}

// OriginOf strips a base URL down to scheme://host — the native task tree
// hangs off the provider's origin, and any configured path would corrupt
// the route.
func OriginOf(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return strings.TrimRight(baseURL, "/")
	}
	return u.Scheme + "://" + u.Host
}

// readBounded reads a response body whole up to a small cap — task
// payloads are status reads measured in bytes, not media.
func readBounded(resp *http.Response) []byte {
	const capBytes = 1 << 20
	body, _ := io.ReadAll(io.LimitReader(resp.Body, capBytes))
	return body
}
