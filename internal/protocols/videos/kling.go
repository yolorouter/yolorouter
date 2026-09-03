package videos

// The Kling half of the video dialect: the new-design task conversation —
// the model version rides in the path, auth is the single API key, and
// there is exactly one query route for every capability. The endpoint
// family accepts any integer duration 3..15, so the dialect's seconds
// pass through verbatim with no mapping. The poll side reports the
// delivered clip's own duration as a decimal string; that string is the
// billable seconds, falling back to the task's echo of what was asked —
// the same stance the Ark half takes — only when it arrives empty.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// KlingT2VPathPrefix and KlingI2VPathPrefix are the submit routes' fixed
// prefixes; the provider model name is appended to form the full path —
// the version-in-path spelling that distinguishes the new-design API.
// Served from the provider's ORIGIN.
const (
	KlingT2VPathPrefix = "/text-to-video/"
	KlingI2VPathPrefix = "/image-to-video/"
)

// KlingTaskQueryPath is the poll route, served from the provider's ORIGIN
// with the task id as the task_ids query parameter — a query-param route,
// unlike the path-append routes the other two dialects spell.
const KlingTaskQueryPath = "/tasks"

// KlingTaskRoute builds the poll route for one task id: the one spelling
// of the query-param shape, escape included, so the gateway's poller and
// the verification probe cannot diverge on it.
func KlingTaskRoute(taskID string) string {
	return KlingTaskQueryPath + "?task_ids=" + url.QueryEscape(taskID)
}

// IsKlingBase reports whether a provider base URL points at Kling AI's
// API. The host suffix is the whole signal: it carries the regional
// spelling (api-beijing, api-singapore) and the legacy api.klingai.com
// alike, and the task routes are the same on all of them. The images
// dialect keeps a twin of this rule (images.IsKlingBase) — one vendor,
// one rule, written twice with the comments pointing at each other.
func IsKlingBase(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), ".klingai.com")
}

// KlingModelSupported reports whether a provider model name is one the
// new-design endpoints this dialect encodes have verbatim paths for. The
// path is the model name, so a name outside the list would dial a route
// that does not exist — a whitelist, not a prefix, because a prefix match
// on "kling-" would happily spell garbage into the URL.
func KlingModelSupported(providerModel string) bool {
	switch providerModel {
	case "kling-3.0", "kling-3.0-turbo":
		return true
	}
	return false
}

// MapKlingSize maps a dialect size (WIDTHxHEIGHT) onto Kling's two axes
// from the shared nearest-neighbor table, so a size cannot submit at one
// tier and price at another. The resolution is lowercase because Kling's
// vocabulary is; the pricing tier keeps its one uppercase spelling
// (TierForSize).
func MapKlingSize(dialectSize string) (resolution, ratio string, ok bool) {
	tier, ratio, ok := tierAndRatioForSize(dialectSize)
	if !ok {
		return "", "", false
	}
	return strings.ToLower(tier), ratio, true
}

// KlingSubmitRequest is one video submit, already mapped: resolution and
// ratio came from MapKlingSize, duration is the dialect's seconds
// verbatim. Reference is the caller's input_reference already resolved to
// bytes or a URL — at most one of the two is set; a submit without one is
// a text generation.
type KlingSubmitRequest struct {
	Model      string
	Prompt     string
	Resolution string
	Ratio      string
	Duration   int
	// RefURL is the reference image as the caller gave it (a URL or a
	// data URI); RefData is the uploaded-file form. Kling's first_frame
	// slot takes a URL or bare base64 in the same field, so both normalize
	// to whichever spelling the source already is — and a bare-base64
	// payload carries no content type, hence no RefContentType twin here.
	RefURL  string
	RefData []byte
}

// KlingReferenced reports whether an input reference carries an image the
// first_frame slot can hold — the one judgment the endpoint choice and the
// encoder's own body shape both answer from, so a reference that arrived
// present-but-empty routes and encodes as the text generation it is (the
// wan and ark halves treat an empty reference the same way, silently).
func KlingReferenced(ref *InputRef) bool {
	if ref == nil {
		return false
	}
	var data []byte
	if ref.File != nil {
		data = ref.File.Data
	}
	// Delegates to the encoder's own reference normalizer, so the
	// endpoint choice and the body shape answer from one judgment.
	return klingReference(ref.ImageURL, data) != ""
}

// KlingSubmitPath builds the submit route for a model: the image-to-video
// endpoint when the reference carries an image, the text-to-video one when
// not.
func KlingSubmitPath(model string, referenced bool) string {
	if referenced {
		return KlingI2VPathPrefix + model
	}
	return KlingT2VPathPrefix + model
}

// EncodeKlingSubmit builds the native submit body. A text generation
// states prompt and the full settings block; an image-referenced one
// moves the prompt into contents[] beside the first_frame entry and
// leaves aspect_ratio out of settings — the reference image decides the
// aspect, exactly the ratio rule the other two dialects already follow.
//
// options is omitted entirely: its three knobs (callback, external id,
// watermark) all default to off/absent, and off is this gateway's stance.
func EncodeKlingSubmit(req KlingSubmitRequest) ([]byte, error) {
	if !KlingModelSupported(req.Model) {
		return nil, fmt.Errorf("model %q is not one the kling video dialect knows", req.Model)
	}
	settings := map[string]any{"resolution": req.Resolution, "duration": req.Duration}
	var body map[string]any
	if ref := klingReference(req.RefURL, req.RefData); ref == "" {
		settings["aspect_ratio"] = req.Ratio
		body = map[string]any{"prompt": req.Prompt, "settings": settings}
	} else {
		body = map[string]any{
			"contents": []any{
				map[string]any{"type": "prompt", "text": req.Prompt},
				map[string]any{"type": "first_frame", "url": ref},
			},
			"settings": settings,
		}
	}
	return json.Marshal(body)
}

// klingReference normalizes the reference into the one field Kling's
// first_frame slot has: an http(s) URL verbatim, a data URI stripped to
// its bare base64 payload (the upstream's documented spelling — a
// prefixed data URI is its documented wrong example), and uploaded bytes
// as bare base64 directly. A malformed data URI without a comma passes
// through untouched for the upstream to refuse: guessing a repair would
// hide the one fact the caller needs to learn.
func klingReference(refURL string, data []byte) string {
	if refURL != "" {
		if strings.HasPrefix(refURL, "data:") {
			if i := strings.IndexByte(refURL, ','); i >= 0 {
				return refURL[i+1:]
			}
		}
		return refURL
	}
	if len(data) > 0 {
		return base64.StdEncoding.EncodeToString(data)
	}
	return ""
}

// KlingBizError is a refusal that arrived inside an HTTP 200: the request
// was rejected, not the transport. The wire's code is numeric; the error
// face keeps the string spelling the other dialects' refusals share.
type KlingBizError struct {
	Code    string
	Message string
}

func (e *KlingBizError) Error() string { return e.Code + ": " + e.Message }

// klingEnvelope is the half of every Kling answer that precedes the
// payload: zero means success, anything else is a refusal with its
// message beside it.
type klingEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// klingSubmitResponse is the wire shape of a submit answer: the envelope
// plus the task object it accepted.
type klingSubmitResponse struct {
	klingEnvelope
	Data struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	} `json:"data"`
}

// ParseKlingSubmitResponse reads a submit answer: a task id to keep
// asking about, a business refusal to answer the caller with, or a shape
// this dialect cannot read.
func ParseKlingSubmitResponse(body []byte) (taskID string, biz *KlingBizError, err error) {
	var resp klingSubmitResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", nil, fmt.Errorf("kling submit response is not JSON: %w", jerr)
	}
	if resp.Code != 0 {
		return "", &KlingBizError{Code: strconv.Itoa(resp.Code), Message: resp.Message}, nil
	}
	if resp.Data.ID == "" {
		return "", nil, fmt.Errorf("kling submit response carries no task id")
	}
	return resp.Data.ID, nil, nil
}

// KlingTaskObservation is one poll's answer in normalized form; the
// status spellings are the gateway's, and the billable duration is the
// delivered clip's own seconds when the upstream states them.
type KlingTaskObservation struct {
	Status       string
	VideoURL     string
	UsageSecs    int
	ErrorCode    string
	ErrorMessage string
}

// klingTaskResponse is the wire shape of a poll answer: the envelope plus
// a data array — one element per task id asked about, this dialect asks
// about one.
type klingTaskResponse struct {
	klingEnvelope
	Data []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Outputs []struct {
			Type     string `json:"type"`
			URL      string `json:"url"`
			Duration string `json:"duration"`
		} `json:"outputs"`
	} `json:"data"`
}

// mapKlingTaskStatus maps the vendor's status word onto the gateway's
// task vocabulary. The wire cannot say cancelled or expired as statuses;
// this vendor has no such words, and an unknown task id arrives as an
// empty data array instead.
func mapKlingTaskStatus(word string) (status string, err error) {
	switch word {
	case "submitted":
		return "pending", nil
	case "processing":
		return "processing", nil
	case "succeeded":
		return "completed", nil
	case "failed":
		return "failed", nil
	}
	return "", fmt.Errorf("kling task status %q is outside the documented vocabulary", word)
}

// ParseKlingTaskResponse reads a poll answer into the normalized
// observation, with a business refusal behind its own face the way the
// dashscope twin carries one. An empty data array is the upstream saying
// it knows no such task — observed live against a fabricated id, which
// answered 200 with code 0 and no elements — and that is the expired
// spelling here, the same reading the dashscope half gives its UNKNOWN.
func ParseKlingTaskResponse(body []byte) (KlingTaskObservation, *KlingBizError, error) {
	var resp klingTaskResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return KlingTaskObservation{}, nil, fmt.Errorf("kling task response is not JSON: %w", jerr)
	}
	if resp.Code != 0 {
		return KlingTaskObservation{}, &KlingBizError{Code: strconv.Itoa(resp.Code), Message: resp.Message}, nil
	}
	if len(resp.Data) == 0 {
		return KlingTaskObservation{Status: "expired", ErrorCode: "task_expired",
			ErrorMessage: "task id is unknown to the upstream"}, nil, nil
	}
	task := resp.Data[0]
	status, merr := mapKlingTaskStatus(task.Status)
	if merr != nil {
		return KlingTaskObservation{}, nil, merr
	}
	obs := KlingTaskObservation{Status: status}
	if status == "failed" {
		obs.ErrorMessage = task.Message
		obs.ErrorCode = "kling_task_failed"
	}
	for _, out := range task.Outputs {
		if out.Type == "video" {
			obs.VideoURL = out.URL
			obs.UsageSecs = parseKlingSeconds(out.Duration)
			break
		}
	}
	return obs, nil, nil
}

// parseKlingSeconds reads the delivered-duration string: whole seconds in
// practice ("3", sometimes "3.0"), decimal in principle. The fractional
// part never survives into billing semantics — the vocabulary has no
// half-second clip — so a fractional spelling rounds to nearest rather
// than flooring, which would quietly underbill every x.6 and up.
func parseKlingSeconds(s string) int {
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(math.Round(f))
	}
	return 0
}
