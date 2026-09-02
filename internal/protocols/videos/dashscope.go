package videos

// The DashScope half of the video dialect: the submit-then-poll
// conversation the wan model families run over X-DashScope-Async. Two
// request shapes exist and the model family decides which one a submit
// carries — wan2.7 and newer take input.prompt with a typed input.media[]
// reference, older families the flat input.img_url — and the poll side is
// one shape for all of them: GET /api/v1/tasks/{id} with the six-value
// task_status vocabulary. This file is the pure dialect (encode, parse,
// classify); the gateway-side poller that carries credentials lives
// outside the protocols tree, like the images dialect's own split.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// DashScopeSubmitPath is the submit route, served from the provider's
// ORIGIN (scheme://host), not its versioned base: a DashScope provider's
// configured base points at the OpenAI-compatible tree while the native
// task tree hangs off the same host's root.
const DashScopeSubmitPath = "/api/v1/services/aigc/video-generation/video-synthesis"

// DashScopeTaskPathPrefix is the poll route's fixed prefix; the task id
// the submit returned is appended to it. Served from the provider's
// ORIGIN like the submit route, and named beside it for the same reason:
// a dialect's routes should not live as half constants, half literals.
const DashScopeTaskPathPrefix = "/api/v1/tasks/"

// DashScopeAsyncHeader must ride every submit or the endpoint refuses the
// call with "current user api does not support synchronous calls" — the
// one header that distinguishes a task submit from a sync generation.
const DashScopeAsyncHeader = "X-DashScope-Async"

// DashScopeFamily is which request shape a model name submits with.
type DashScopeFamily int

const (
	// DashScopeFamilyNone means the name is not a wan video model this
	// dialect knows; a candidate carrying it is refused per candidate
	// rather than guessed at.
	DashScopeFamilyNone DashScopeFamily = iota
	// DashScopeFamilyMedia is wan2.7 and newer (including wan3.0): the
	// typed input.media[] reference form.
	DashScopeFamilyMedia
	// DashScopeFamilyLegacy is wan2.1 through wan2.6 (and the wanx
	// spellings): the flat input.img_url form.
	DashScopeFamilyLegacy
)

// DashScopeModelFamily classifies a provider model name for the request
// shape. Only the wan families the dialect has read shape documentation
// for are classified; anything else is none of its business — adding a
// family means reading its docs, not widening a prefix.
func DashScopeModelFamily(providerModel string) DashScopeFamily {
	name := strings.ToLower(providerModel)
	switch {
	case strings.HasPrefix(name, "wan3."), strings.HasPrefix(name, "wan2.7"):
		return DashScopeFamilyMedia
	case strings.HasPrefix(name, "wan"), strings.HasPrefix(name, "wanx"):
		return DashScopeFamilyLegacy
	}
	return DashScopeFamilyNone
}

// MapDashScopeSize maps a dialect size (WIDTHxHEIGHT) onto the two axes
// DashScope wants — resolution tier and aspect ratio — from the shared
// nearest-neighbor table both vendor maps answer from, so a size cannot
// submit at one tier and price at another. The resolution half is
// uppercase because DashScope's vocabulary is; the pricing tier shares
// the same spelling (TierForSize).
func MapDashScopeSize(dialectSize string) (resolution, ratio string, ok bool) {
	return tierAndRatioForSize(dialectSize)
}

// DashScopeSubmitRequest is one video submit, already mapped: the
// resolution and ratio came from MapDashScopeSize, the duration is the
// dialect's seconds verbatim. Reference is the caller's input_reference
// already resolved to bytes or a URL — at most one of the two is set; a
// submit without one is a text generation.
type DashScopeSubmitRequest struct {
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

// EncodeDashScopeSubmit builds the native submit body for the model's
// family.
//
// The ratio rule both families share: a text-to-video submit states the
// aspect ratio it wants; an image-referenced one does not, because the
// reference image decides the aspect and the upstream's ratio knob is
// documented as ignored (or invalid) there.
func EncodeDashScopeSubmit(req DashScopeSubmitRequest) ([]byte, error) {
	family := DashScopeModelFamily(req.Model)
	if family == DashScopeFamilyNone {
		return nil, fmt.Errorf("model %q is not one the dashscope video dialect knows", req.Model)
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
	case DashScopeFamilyMedia:
		// wan2.7 and newer: input.prompt with the reference, when there is
		// one, as a typed input.media[] entry — the flat form the
		// image-to-video and wan3.0 references both document.
		input = map[string]any{"prompt": req.Prompt}
		if ref != "" {
			input["media"] = []any{map[string]any{"type": "first_frame", "url": ref}}
		}
	case DashScopeFamilyLegacy:
		input = map[string]any{"prompt": req.Prompt}
		if ref != "" {
			input["img_url"] = ref
		}
	}
	return json.Marshal(map[string]any{"model": req.Model, "input": input, "parameters": params})
}

// DashScopeTaskObservation is one poll's answer in normalized form; the
// status spellings are the gateway's six, with the vendor's UNKNOWN
// already mapped to expired — a task the upstream cannot know is one this
// gateway must stop asking about.
type DashScopeTaskObservation struct {
	Status       string
	VideoURL     string
	UsageSecs    int
	ErrorCode    string
	ErrorMessage string
}

// DashScopeBizError is a refusal that arrived inside an HTTP 200: the
// request was rejected, not the transport.
type DashScopeBizError struct {
	Code    string
	Message string
}

func (e *DashScopeBizError) Error() string { return e.Code + ": " + e.Message }

// dashScopeSubmitResponse is the wire shape of a submit answer.
type dashScopeSubmitResponse struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	} `json:"output"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// ParseDashScopeSubmitResponse reads a submit answer: a task id to keep
// asking about, a business refusal to answer the caller with, or a shape
// this dialect cannot read.
func ParseDashScopeSubmitResponse(body []byte) (taskID string, biz *DashScopeBizError, err error) {
	var resp dashScopeSubmitResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", nil, fmt.Errorf("dashscope submit response is not JSON: %w", jerr)
	}
	if resp.Code != "" {
		return "", &DashScopeBizError{Code: resp.Code, Message: resp.Message}, nil
	}
	if resp.Output.TaskID == "" {
		return "", nil, fmt.Errorf("dashscope submit response carries no task_id")
	}
	return resp.Output.TaskID, nil, nil
}

// dashScopeTaskResponse is the wire shape of a poll answer.
type dashScopeTaskResponse struct {
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

// mapDashScopeTaskStatus maps the vendor's status word onto the gateway's
// task vocabulary, with the error-channel words for the states the wire
// cannot say directly.
func mapDashScopeTaskStatus(word string) (status, errCode, errMsg string, err error) {
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
	return "", "", "", fmt.Errorf("dashscope task_status %q is outside the documented vocabulary", word)
}

// ParseDashScopeTaskResponse reads a poll answer into the normalized
// observation. The observation is built in one literal — the fields say
// what the status word means, so they are decided where it is decided,
// not patched on afterwards.
func ParseDashScopeTaskResponse(body []byte) (DashScopeTaskObservation, *DashScopeBizError, error) {
	var resp dashScopeTaskResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return DashScopeTaskObservation{}, nil, fmt.Errorf("dashscope task response is not JSON: %w", jerr)
	}
	if resp.Code != "" {
		return DashScopeTaskObservation{}, &DashScopeBizError{Code: resp.Code, Message: resp.Message}, nil
	}
	status, errCode, errMsg, merr := mapDashScopeTaskStatus(resp.Output.TaskStatus)
	if merr != nil {
		return DashScopeTaskObservation{}, nil, merr
	}
	// A FAILED answer names itself in the output block; the stock code is
	// only the fallback for one that arrived nameless.
	if status == "failed" {
		errMsg = resp.Output.Message
		if resp.Output.Code != "" {
			errCode = resp.Output.Code
		}
	}
	return DashScopeTaskObservation{
		Status:       status,
		VideoURL:     resp.Output.VideoURL,
		UsageSecs:    resp.Usage.Duration,
		ErrorCode:    errCode,
		ErrorMessage: errMsg,
	}, nil, nil
}

// Origin strips a base URL down to scheme://host — the native task
// trees hang off the provider's origin, and any configured path would
// corrupt the route. Named without a vendor: every task dialect answers
// it the same way.
func Origin(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return strings.TrimRight(baseURL, "/")
	}
	return u.Scheme + "://" + u.Host
}

// ReadTaskBounded reads a task response body whole up to a small cap —
// task payloads are status reads measured in bytes, not media. Vendor
// neutral for the same reason Origin is.
func ReadTaskBounded(r io.Reader) []byte {
	const capBytes = 1 << 20
	body, _ := io.ReadAll(io.LimitReader(r, capBytes))
	return body
}
