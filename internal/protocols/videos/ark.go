package videos

// The Ark half of the video dialect: Volcengine's content-generation
// task conversation. One uniform endpoint family serves every Seedance
// model, so unlike the dashscope half there is no family gate — a model
// name (or an inference endpoint id, which the same field accepts) is the
// operator's own spelling, and the probe judges whether it works. The
// poll side speaks the six lowercase statuses and reports no seconds
// anywhere in its usage, so the observation's billable duration is the
// task's own echoed duration — exact for this dialect, which always
// states its seconds rather than asking for a smart one.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// ArkSubmitPath is the submit route, served from the provider's ORIGIN:
// the path carries its own /api/v3 segment, so a configured base with or
// without that prefix must not be joined ahead of it.
const ArkSubmitPath = "/api/v3/contents/generations/tasks"

// ArkTaskPathPrefix is the poll route's fixed prefix; the task id the
// submit returned is appended. Same origin rule as the submit route.
const ArkTaskPathPrefix = ArkSubmitPath + "/"

// IsArkBase reports whether a provider base URL points at Volcengine
// Ark. The host is the whole signal — the task routes are the same on
// every regional spelling.
func IsArkBase(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), ".volces.com")
}

// MapArkSize maps a dialect size (WIDTHxHEIGHT) onto Ark's two axes.
// The resolution is lowercase because Ark's vocabulary is; the tier
// vocabulary the pricing table keys on stays uppercase everywhere
// (TierForSize is its one spelling).
func MapArkSize(dialectSize string) (resolution, ratio string, ok bool) {
	tier, ratio, ok := tierAndRatioForSize(dialectSize)
	if !ok {
		return "", "", false
	}
	return strings.ToLower(tier), ratio, true
}

// TierForSize is the pricing tier a dialect size prices at — the one
// spelling of the resolution axis the whole video surface shares: the
// candidate tables key on it and both vendor maps answer it, so a size
// cannot price at one tier and submit at another.
func TierForSize(dialectSize string) (string, bool) {
	tier, _, ok := tierAndRatioForSize(dialectSize)
	return tier, ok
}

// tierAndRatioForSize is the shared nearest-neighbor table both vendor
// maps answer from — pixel coordinates onto coarse tiers, written once
// so the two dialects cannot drift apart.
func tierAndRatioForSize(dialectSize string) (tier, ratio string, ok bool) {
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

// ArkSubmitRequest is one video submit, already mapped: resolution and
// ratio came from MapArkSize, duration is the dialect's seconds
// verbatim.
type ArkSubmitRequest struct {
	Model      string
	Prompt     string
	Resolution string
	Ratio      string
	Duration   int
	// RefURL is the reference image as the caller gave it (a URL or a
	// data URI); RefData/RefContentType is the uploaded-file form,
	// normalized here exactly as the dashscope half normalizes it.
	RefURL         string
	RefData        []byte
	RefContentType string
}

// EncodeArkSubmit builds the native submit body: a content[] of typed
// items — the prompt as text, the reference as an image_url with its
// role — plus the output knobs on top.
//
// The ratio rule mirrors the dashscope half: a text-to-video submit
// states the aspect ratio; an image-referenced one omits it and lets
// Ark's adaptive default follow the reference, which its own docs say
// is what the knob does there.
//
// Audio is deliberately left to the upstream default (the newer
// Seedance models generate it by default and bill it in their token
// count): the dialect exposes no audio knob, and silently forcing one
// would misprice the other direction.
func EncodeArkSubmit(req ArkSubmitRequest) ([]byte, error) {
	ref := req.RefURL
	if ref == "" && len(req.RefData) > 0 {
		ref = dataURI(req.RefContentType, req.RefData)
	}

	content := []any{map[string]any{"type": "text", "text": req.Prompt}}
	if ref != "" {
		content = append(content, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": ref}, "role": "first_frame",
		})
	}
	body := map[string]any{
		"model": req.Model, "content": content,
		"resolution": req.Resolution, "duration": req.Duration,
		// The deployment's own standing knobs, not the caller's — the
		// same stance the dashscope half takes on its pair.
		"camera_fixed": false, "watermark": false,
	}
	if ref == "" && req.Ratio != "" {
		body["ratio"] = req.Ratio
	}
	return json.Marshal(body)
}

// dataURI is the videos dialects' one upload normalizer: sniff the
// content type when the upload's own header is missing or is the
// multipart default curl sends — an octet-stream from a file part is
// still an image here, the lesson the images dialect learned first.
func dataURI(contentType string, data []byte) string {
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = http.DetectContentType(data)
	}
	return "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// ArkBizError is a refusal that arrived in the task's own error shape.
type ArkBizError struct {
	Code    string
	Message string
}

func (e *ArkBizError) Error() string { return e.Code + ": " + e.Message }

// arkSubmitResponse is the wire shape of a submit answer: the task id
// and, on refusal, the error object.
type arkSubmitResponse struct {
	ID    string `json:"id"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseArkSubmitResponse reads a submit answer: a task id to keep asking
// about, a refusal to answer the caller with, or a shape this dialect
// cannot read.
func ParseArkSubmitResponse(body []byte) (taskID string, biz *ArkBizError, err error) {
	var resp arkSubmitResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", nil, fmt.Errorf("ark submit response is not JSON: %w", jerr)
	}
	if resp.Error != nil && resp.Error.Code != "" {
		return "", &ArkBizError{Code: resp.Error.Code, Message: resp.Error.Message}, nil
	}
	if resp.ID == "" {
		return "", nil, fmt.Errorf("ark submit response carries no task id")
	}
	return resp.ID, nil, nil
}

// ArkTaskObservation is one poll's answer in normalized form, with the
// vendor's statuses already on the gateway's six and the billable
// duration taken from the task's own echo of what was asked.
type ArkTaskObservation struct {
	Status       string
	VideoURL     string
	UsageSecs    int
	ErrorCode    string
	ErrorMessage string
}

// arkTaskResponse is the wire shape of a poll answer.
type arkTaskResponse struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Duration int `json:"duration"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mapArkTaskStatus maps the vendor's status word onto the gateway's
// task vocabulary; the wire cannot say cancelled or expired, but this
// vendor can, so they keep their own spellings instead of borrowing the
// error channel.
func mapArkTaskStatus(word string) (status string, err error) {
	switch word {
	case "queued":
		return "pending", nil
	case "running":
		return "processing", nil
	case "succeeded":
		return "completed", nil
	case "failed":
		return "failed", nil
	case "cancelled":
		return "cancelled", nil
	case "expired":
		return "expired", nil
	}
	return "", fmt.Errorf("ark task status %q is outside the documented vocabulary", word)
}

// ParseArkTaskResponse reads a poll answer into the normalized
// observation, built in one literal like its dashscope twin. No business
// refusal half: this vendor carries its refusals inside the task object
// itself, and they land in the observation's error fields.
func ParseArkTaskResponse(body []byte) (ArkTaskObservation, error) {
	var resp arkTaskResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return ArkTaskObservation{}, fmt.Errorf("ark task response is not JSON: %w", jerr)
	}
	status, merr := mapArkTaskStatus(resp.Status)
	if merr != nil {
		return ArkTaskObservation{}, merr
	}
	obs := ArkTaskObservation{Status: status, VideoURL: resp.Content.VideoURL, UsageSecs: resp.Duration}
	if resp.Error != nil && resp.Error.Code != "" {
		obs.ErrorCode, obs.ErrorMessage = resp.Error.Code, resp.Error.Message
	}
	if status == "failed" && obs.ErrorCode == "" {
		obs.ErrorCode = "upstream_failed"
	}
	return obs, nil
}
