package videos

// The MiniMax half of the video dialect: the V2 task conversation — one
// submit route for every capability with the model in the body, and a
// query route with the task id in the path. Errors arrive as real HTTP
// status codes in an OpenAI-shaped body, unlike the 200-with-code
// envelopes the kling and dashscope halves read, so this dialect has no
// business-refusal arm on the submit parse: a non-200 answer is parsed
// for its error face where the caller can surface the vendor's own
// message. Duration is an integer 4..15 per model; the door's 4/8/12
// passes through verbatim except on MiniMax-H3-Max, whose floor is 5 —
// the duration gate refuses a 4-second ask rather than silently
// rewriting it into a clip the caller never chose. The poll side states
// the billable seconds in usage.output_seconds; the task object's own
// echo of the requested duration answers only when that field arrives
// empty — the same stance the ark and kling queriers take, read from
// the same body here because this vendor carries the echo beside the
// usage.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// The provider model names the V2 endpoints enumerate. They ride in the
// request body, not the path, so a name outside the enumeration is a
// body value the upstream would refuse — the whitelist keeps the refusal
// this side of the wire.
const (
	MiniMaxH3Model    = "MiniMax-H3"
	MiniMaxH3MaxModel = "MiniMax-H3-Max"
)

// MiniMaxSubmitPath is the submit route, served from the provider's
// ORIGIN. The V2 family is the current API; the V1 /v1 endpoints with
// their base_resp envelopes and per-mode routes are a legacy dialect
// this build does not wire.
const MiniMaxSubmitPath = "/v2/video_generation"

// MiniMaxTaskQueryPathPrefix is the poll route's fixed prefix; the task
// id the submit returned is appended as a path segment.
const MiniMaxTaskQueryPathPrefix = "/v2/query/video_generation/"

// MiniMaxTaskRoute builds the poll route for one task id: the one
// spelling of the path-append shape, escape included, so the gateway's
// poller and the verification probe cannot diverge on it.
func MiniMaxTaskRoute(taskID string) string {
	return MiniMaxTaskQueryPathPrefix + url.PathEscape(taskID)
}

// IsMiniMaxBase reports whether a provider base URL points at MiniMax's
// API. The single documented host is the whole signal — every V2 page
// names api.minimax.cn and no other — so unlike the regional-suffix
// detectors this one compares the hostname exactly.
func IsMiniMaxBase(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return u.Hostname() == "api.minimax.cn"
}

// MiniMaxModelSupported reports whether a provider model name is one the
// V2 endpoints enumerate. A body enum, so a name outside it would be
// refused upstream — the gate keeps that answer local.
func MiniMaxModelSupported(providerModel string) bool {
	switch providerModel {
	case MiniMaxH3Model, MiniMaxH3MaxModel:
		return true
	}
	return false
}

// MapMiniMaxSize maps a dialect size (WIDTHxHEIGHT) onto MiniMax's two
// axes, and the answer depends on the model: both models' base tier is
// 768P, but only MiniMax-H3 has a 2K top, so the large door sizes ride
// at 2K there and stay at 768P on H3-Max. The ratio comes from the
// shared nearest-neighbor table — same door sizes, same aspect answers
// as every other dialect. The pricing tier is deliberately NOT this
// table's vocabulary: candidates price at the door-size tiers
// (TierForSize), and the vendor's resolution word lives only on the
// wire.
func MapMiniMaxSize(model, dialectSize string) (resolution, ratio string, ok bool) {
	tier, ratio, ok := tierAndRatioForSize(dialectSize)
	if !ok {
		return "", "", false
	}
	switch model {
	case MiniMaxH3Model:
		if tier == "1080P" {
			return "2K", ratio, true
		}
		return "768P", ratio, true
	case MiniMaxH3MaxModel:
		return "768P", ratio, true
	}
	return "", "", false
}

// MiniMaxDurationSupported reports whether a seconds ask is one the
// model can generate: both models top out at 15, and MiniMax-H3-Max
// floors at 5 — a 4-second clip is legal on MiniMax-H3 and refused on
// H3-Max. The door has already judged the seconds vocabulary; this gate
// judges the one model-dependent edge in it, and a refusal surfaces the
// reason rather than rewriting the ask.
func MiniMaxDurationSupported(model string, seconds int) bool {
	return model != MiniMaxH3MaxModel || seconds >= 5
}

// MiniMaxSubmitRequest is one video submit, already mapped: resolution
// and ratio came from MapMiniMaxSize, duration is the dialect's seconds
// verbatim. Reference is the caller's input_reference already resolved
// to bytes or a URL — at most one of the two is set; a submit without
// one is a text generation.
type MiniMaxSubmitRequest struct {
	Model      string
	Prompt     string
	Resolution string
	Ratio      string
	Duration   int
	// RefURL is the reference image as the caller gave it (a URL or a
	// data URI); RefData/RefContentType is the uploaded-file form. The
	// data URI spelling carries the content type inline, so the upload
	// form keeps the slot the kling request deliberately dropped.
	RefURL         string
	RefData        []byte
	RefContentType string
}

// EncodeMiniMaxSubmit builds the native submit body: a content array —
// the prompt as the required text item, the reference as an image_url
// with its role when one rides along — plus the required resolution and
// duration. The ratio rule mirrors the ark half: a text-to-video submit
// states it (the upstream requires one); an image-referenced one omits
// it and lets the forced-adaptive default follow the reference.
//
// aigc_watermark is omitted entirely: it defaults to false, and off is
// this gateway's stance.
func EncodeMiniMaxSubmit(req MiniMaxSubmitRequest) ([]byte, error) {
	if !MiniMaxModelSupported(req.Model) {
		return nil, fmt.Errorf("model %q is not one the minimax video dialect knows", req.Model)
	}
	ref := minimaxReference(req.RefURL, req.RefContentType, req.RefData)
	content := []any{map[string]any{"type": "text", "text": req.Prompt}}
	if ref != "" {
		content = append(content, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": ref}, "role": "first_frame",
		})
	}
	body := map[string]any{
		"model": req.Model, "content": content,
		"resolution": req.Resolution, "duration": req.Duration,
	}
	if ref == "" && req.Ratio != "" {
		body["ratio"] = req.Ratio
	}
	return json.Marshal(body)
}

// minimaxReference normalizes the reference into the url field the
// image_url item carries: an http(s) URL verbatim, a data URI with its
// media-type token lowercased (the upstream documents the token as
// lowercase — a caller's uppercase spelling is normalized rather than
// passed along to be refused), and uploaded bytes as the videos
// dialects' shared data-URI builder. A malformed data URI without a
// comma passes through untouched for the upstream to refuse: guessing a
// repair would hide the one fact the caller needs to learn. The upload
// form honors the part's own content type when it carries one, the same
// trust the ark and dashscope halves place in it.
func minimaxReference(refURL, contentType string, data []byte) string {
	if refURL != "" {
		if strings.HasPrefix(refURL, "data:") {
			if i := strings.IndexByte(refURL, ','); i >= 0 {
				return strings.ToLower(refURL[:i]) + refURL[i:]
			}
		}
		return refURL
	}
	if len(data) > 0 {
		return dataURI(contentType, data)
	}
	return ""
}

// ParseMiniMaxSubmitResponse reads a submit answer: a task id to keep
// asking about. No refusal arm — this vendor carries refusals as real
// HTTP statuses, which never reach a 200-answer parser; a body without
// a task id is a shape this dialect cannot read.
func ParseMiniMaxSubmitResponse(body []byte) (taskID string, err error) {
	var resp struct {
		TaskID string `json:"task_id"`
	}
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", fmt.Errorf("minimax submit response is not JSON: %w", jerr)
	}
	if resp.TaskID == "" {
		return "", fmt.Errorf("minimax submit response carries no task id")
	}
	return resp.TaskID, nil
}

// MiniMaxTaskObservation is one poll's answer in normalized form; the
// status spellings are the gateway's, and the billable seconds are the
// usage the upstream states, with the task's own duration echo as the
// fallback the ark and kling queriers take from the task row — this
// vendor carries the echo in the same body, so the fallback reads it
// here.
type MiniMaxTaskObservation struct {
	Status       string
	VideoURL     string
	UsageSecs    int
	ErrorCode    string
	ErrorMessage string
}

// minimaxTaskResponse is the wire shape of a poll answer: one task
// object the query endpoint wraps.
type minimaxTaskResponse struct {
	Task struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Content struct {
			URL string `json:"url"`
		} `json:"content"`
		Duration int `json:"duration"`
		Usage    struct {
			OutputSeconds int `json:"output_seconds"`
		} `json:"usage"`
	} `json:"task"`
}

// mapMiniMaxTaskStatus maps the vendor's status word onto the gateway's
// task vocabulary. The vendor can say cancelled, and the internal
// vocabulary keeps it — the wire's four-value contract renders it as a
// failure with the task_cancelled code, the same rendering every
// dialect's cancelled lands on.
func mapMiniMaxTaskStatus(word string) (status string, err error) {
	switch word {
	case "queued":
		return StatusPending, nil
	case "running":
		return StatusProcessing, nil
	case "succeeded":
		return StatusCompleted, nil
	case "failed":
		return StatusFailed, nil
	case "cancelled":
		return StatusCancelled, nil
	}
	return "", fmt.Errorf("minimax task status %q is outside the documented vocabulary", word)
}

// ParseMiniMaxTaskResponse reads a poll answer into the normalized
// observation. No business-refusal half: this vendor carries refusals as
// real HTTP statuses, and they land in the transport layer, not a 200
// body.
func ParseMiniMaxTaskResponse(body []byte) (MiniMaxTaskObservation, error) {
	var resp minimaxTaskResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return MiniMaxTaskObservation{}, fmt.Errorf("minimax task response is not JSON: %w", jerr)
	}
	task := resp.Task
	status, merr := mapMiniMaxTaskStatus(task.Status)
	if merr != nil {
		return MiniMaxTaskObservation{}, merr
	}
	obs := MiniMaxTaskObservation{
		Status: status, VideoURL: task.Content.URL,
		UsageSecs: task.Usage.OutputSeconds,
	}
	if obs.UsageSecs == 0 {
		obs.UsageSecs = task.Duration
	}
	if task.Error != nil && task.Error.Code != "" {
		obs.ErrorCode, obs.ErrorMessage = task.Error.Code, task.Error.Message
	}
	if status == StatusFailed && obs.ErrorCode == "" {
		obs.ErrorCode = "minimax_task_failed"
	}
	// A completion without its clip URL is a shape this dialect cannot
	// act on — reporting it completed would bill seconds for a video
	// nobody can retrieve. Read as a parse error instead: the task keeps
	// its state and the zombie horizon expires it unbilled. The shape has
	// never been observed live; the guard pins it defensively.
	if status == StatusCompleted && obs.VideoURL == "" {
		return MiniMaxTaskObservation{}, fmt.Errorf("minimax succeeded task carries no content url")
	}
	return obs, nil
}
