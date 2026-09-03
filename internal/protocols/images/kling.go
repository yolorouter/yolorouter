package images

// The Kling native image-generation dialect: a task conversation on the
// /v1/images/generations family (the image line has not migrated to the
// version-in-path endpoints the video line uses — model_name is still a
// request parameter and the success spelling is still "succeed"). A request
// that arrived in the OpenAI images shape is re-encoded into this dialect,
// and the task answer is decoded back — the endpoint answers a submit with
// a task id, so the gateway-side poller drives the conversation to a
// terminal task before this package can shape an answer.

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// KlingImageSubmitPath is the submit route of the generations endpoint,
// served from the provider's ORIGIN. The path collides with the OpenAI
// images route by name only — the body and the answer are the task
// dialect's own.
const KlingImageSubmitPath = "/v1/images/generations"

// KlingImageTaskPathPrefix is the generations endpoint's poll route; the
// task id the submit returned is appended. Same origin rule as the submit
// route.
const KlingImageTaskPathPrefix = KlingImageSubmitPath + "/"

// KlingOmniImageSubmitPath is the omni endpoint's own route — the
// multi-reference family (kling-v3-omni, kling-image-o1) lives here, not
// on the generations route, with its own poll route beside it.
const KlingOmniImageSubmitPath = "/v1/images/omni-image"

// KlingOmniImageTaskPathPrefix is the omni endpoint's poll route.
const KlingOmniImageTaskPathPrefix = KlingOmniImageSubmitPath + "/"

// KlingImageSubmitPathFor routes a model to its endpoint's submit route.
func KlingImageSubmitPathFor(model string) string {
	if KlingOmniImageModel(model) {
		return KlingOmniImageSubmitPath
	}
	return KlingImageSubmitPath
}

// KlingImageTaskPathPrefixFor routes a model to its endpoint's poll route.
func KlingImageTaskPathPrefixFor(model string) string {
	if KlingOmniImageModel(model) {
		return KlingOmniImageTaskPathPrefix
	}
	return KlingImageTaskPathPrefix
}

// KlingOmniImageModel reports whether a provider model is one the omni
// endpoint serves.
func KlingOmniImageModel(model string) bool {
	return model == "kling-v3-omni" || model == "kling-image-o1"
}

// IsKlingBase reports whether a provider base URL points at Kling AI's
// API. The host rule is the videos dialect's own (videos.IsKlingBase) —
// one vendor, one rule, written twice with the comments pointing at each
// other so the two modalities cannot drift (the ConvertSize /
// normalizeSizeAxis precedent).
func IsKlingBase(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	return strings.HasSuffix(u.Hostname(), ".klingai.com")
}

// KlingImageModelSupported reports whether a provider model name is one
// this dialect encodes: kling-v3 is Kling Image 3.0 on the generations
// endpoint, and the omni endpoint's pair (kling-v3-omni, kling-image-o1)
// beside it. The 2026-09-15 retirement wave takes v1/v1-5/v2/v2-new and
// leaves these — a whitelist, because the model name rides as model_name
// and a name outside the list bills a task the upstream will refuse.
func KlingImageModelSupported(providerModel string) bool {
	return providerModel == "kling-v3" || KlingOmniImageModel(providerModel)
}

// klingAspectRatios is the aspect vocabulary the endpoint documents, as
// numeric factors for nearest-neighbor matching against a caller's
// WIDTHxHEIGHT ask.
var klingAspectRatios = []struct {
	name   string
	factor float64
}{
	{"16:9", 16.0 / 9}, {"9:16", 9.0 / 16.0}, {"1:1", 1},
	{"4:3", 4.0 / 3}, {"3:4", 3.0 / 4}, {"3:2", 1.5}, {"2:3", 2.0 / 3}, {"21:9", 21.0 / 9},
}

// MapKlingImageSize maps a caller's size axis onto the endpoint's two
// knobs. The empty size omits both (the endpoint's own defaults apply);
// "1k"/"2k" state the resolution alone; a WIDTHxHEIGHT spelling derives
// the nearest documented aspect and picks the resolution by long side
// (>=1920 is the 2K class). Anything else does not map — the caller asked
// for a spelling this dialect cannot state honestly.
func MapKlingImageSize(size string) (resolution, aspectRatio string, ok bool) {
	switch strings.ToLower(size) {
	case "":
		return "", "", true
	case "1k", "2k", "4k":
		// 4k is the omni endpoint's own tier; the generations endpoint
		// does not serve it and says so itself, which is the honest
		// answer for a 4k ask against kling-v3.
		return strings.ToLower(size), "", true
	}
	// Validity is parse-level only (both sides positive): the endpoint's
	// own minimums are reference-image constraints, not size-vocabulary
	// ones, and the upstream answers a too-small ask in its own words.
	w, h, err := parsePixelSize(size)
	if err != nil || w <= 0 || h <= 0 {
		return "", "", false
	}
	ask := float64(w) / float64(h)
	best := klingAspectRatios[0]
	for _, r := range klingAspectRatios[1:] {
		if math.Abs(ask-r.factor) < math.Abs(ask-best.factor) {
			best = r
		}
	}
	res := "1k"
	if max(w, h) >= 1920 {
		res = "2k"
	}
	return res, best.name, true
}

func parsePixelSize(size string) (w, h int, err error) {
	parts := strings.SplitN(strings.ToLower(size), "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("not WIDTHxHEIGHT")
	}
	w, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	h, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

// KlingNativeFields is the kling-native extension fields a caller may
// attach to an OpenAI-shaped request for the shapes that dialect has and
// the OpenAI vocabulary has no name for — the multi-reference and
// series-image knobs of the omni endpoint. They ride verbatim (raw JSON)
// into the re-encoded submit, which is the dialect branch of the promise
// the passthrough path already makes: fields you send reach the upstream.
type KlingNativeFields struct {
	ImageList    json.RawMessage `json:"image_list"`
	ElementList  json.RawMessage `json:"element_list"`
	ResultType   string          `json:"result_type"`
	SeriesAmount json.RawMessage `json:"series_amount"`
}

// ParseKlingNativeFields reads the extension fields from a caller's
// request body. Lenient by design: a body that carries none of them (the
// ordinary OpenAI-shaped ask) reads as all-empty.
func ParseKlingNativeFields(body []byte) *KlingNativeFields {
	var f KlingNativeFields
	_ = json.Unmarshal(body, &f)
	return &f
}

// EncodeKlingImageRequest builds the native submit body for one
// generation. Watermark and callback stay omitted — both default to off,
// and off is this gateway's stance. An unmappable size is an error the
// caller's attempt answers, not a silent default.
func EncodeKlingImageRequest(prompt, model string, n int, size string, native *KlingNativeFields) ([]byte, error) {
	if !KlingImageModelSupported(model) {
		return nil, fmt.Errorf("model %q is not one the kling image dialect knows", model)
	}
	resolution, aspect, ok := MapKlingImageSize(size)
	if !ok {
		return nil, fmt.Errorf("size %q has no kling mapping", size)
	}
	if n <= 0 {
		n = 1
	}
	body := map[string]any{"model_name": model, "prompt": prompt, "n": n}
	if resolution != "" {
		body["resolution"] = resolution
	}
	if aspect != "" {
		body["aspect_ratio"] = aspect
	}
	if native != nil {
		if len(native.ImageList) > 0 {
			body["image_list"] = native.ImageList
		}
		if len(native.ElementList) > 0 {
			body["element_list"] = native.ElementList
		}
		if native.ResultType != "" {
			body["result_type"] = native.ResultType
		}
		if len(native.SeriesAmount) > 0 {
			body["series_amount"] = native.SeriesAmount
		}
	}
	return json.Marshal(body)
}

// KlingImageBizError is a refusal that arrived inside an HTTP 200: the
// request itself failed (code non-empty) and retrying the same body
// changes nothing. Same face as the DashScope business error.
type KlingImageBizError struct {
	Code    string
	Message string
}

func (e *KlingImageBizError) Error() string {
	return fmt.Sprintf("kling business error %s: %s", e.Code, e.Message)
}

// klingImageEnvelope is the half of every Kling answer that precedes the
// payload: zero means success, anything else is a refusal.
type klingImageEnvelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// klingImageSubmitResponse is the wire shape of a submit answer.
type klingImageSubmitResponse struct {
	klingImageEnvelope
	Data struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	} `json:"data"`
}

// ParseKlingImageSubmitResponse reads a submit answer: a task id to keep
// asking about, a business refusal, or a shape this dialect cannot read.
func ParseKlingImageSubmitResponse(body []byte) (taskID string, biz *KlingImageBizError, err error) {
	var resp klingImageSubmitResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", nil, fmt.Errorf("kling image submit response is not JSON: %w", jerr)
	}
	if resp.Code != 0 {
		return "", &KlingImageBizError{Code: strconv.Itoa(resp.Code), Message: resp.Message}, nil
	}
	if resp.Data.TaskID == "" {
		return "", nil, fmt.Errorf("kling image submit response carries no task_id")
	}
	return resp.Data.TaskID, nil, nil
}

// KlingImageTask is one poll's answer in the terms the sync wrapper needs:
// whether the task reached a terminal state, what it delivered, and what
// it cost — the deduction string is kept verbatim for reconciliation, not
// priced here.
type KlingImageTask struct {
	Terminal      bool
	Failed        bool
	StatusMsg     string
	ImageURLs     []string
	UnitDeduction string
	// BalanceDeduct is the postpaid-discount figure and BalanceListPrice
	// the list price it discounted from — both kept verbatim for
	// reconciliation, priced nowhere here.
	BalanceDeduct    string
	BalanceListPrice string
}

// klingImageTaskResponse is the wire shape of a poll answer.
type klingImageTaskResponse struct {
	klingImageEnvelope
	Data struct {
		TaskStatus    string `json:"task_status"`
		TaskStatusMsg string `json:"task_status_msg"`
		FinalUnit     string `json:"final_unit_deduction"`
		FinalBalance  struct {
			Quota     string `json:"quota"`
			ListPrice string `json:"list_price"`
		} `json:"final_balance_deduction"`
		TaskResult struct {
			Images []struct {
				URL string `json:"url"`
			} `json:"images"`
			// series_images is the omni endpoint's series mode delivery —
			// the same per-image billing surface as images, beside it.
			SeriesImages []struct {
				URL string `json:"url"`
			} `json:"series_images"`
		} `json:"task_result"`
	} `json:"data"`
}

// ParseKlingImageTaskResponse reads a poll answer. The status vocabulary
// is the four-value one with the "succeed" spelling this endpoint family
// kept; submitted and processing are explicitly non-terminal — a poller
// that read them as anything else would either spin forever or give up
// early.
func ParseKlingImageTaskResponse(body []byte) (KlingImageTask, *KlingImageBizError, error) {
	var resp klingImageTaskResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return KlingImageTask{}, nil, fmt.Errorf("kling image task response is not JSON: %w", jerr)
	}
	if resp.Code != 0 {
		return KlingImageTask{}, &KlingImageBizError{Code: strconv.Itoa(resp.Code), Message: resp.Message}, nil
	}
	var task KlingImageTask
	switch resp.Data.TaskStatus {
	case "submitted", "processing":
		task.Terminal = false
	case "succeed":
		task.Terminal, task.Failed = true, false
	case "failed":
		task.Terminal, task.Failed = true, true
		task.StatusMsg = resp.Data.TaskStatusMsg
	default:
		return KlingImageTask{}, nil, fmt.Errorf("kling image task_status %q is outside the documented vocabulary", resp.Data.TaskStatus)
	}
	task.UnitDeduction = resp.Data.FinalUnit
	task.BalanceDeduct = resp.Data.FinalBalance.Quota
	task.BalanceListPrice = resp.Data.FinalBalance.ListPrice
	for _, img := range resp.Data.TaskResult.Images {
		if img.URL != "" {
			task.ImageURLs = append(task.ImageURLs, img.URL)
		}
	}
	for _, img := range resp.Data.TaskResult.SeriesImages {
		if img.URL != "" {
			task.ImageURLs = append(task.ImageURLs, img.URL)
		}
	}
	return task, nil, nil
}

// EncodeKlingImagesOpenAI shapes a terminal task's URLs into the OpenAI
// images response the caller asked in — the same delivery shape the
// DashScope dialect answers with.
func EncodeKlingImagesOpenAI(urls []string) ([]byte, error) {
	items := make([]openAIImageItem, 0, len(urls))
	for _, u := range urls {
		items = append(items, openAIImageItem{URL: u})
	}
	out := struct {
		Created int64             `json:"created"`
		Data    []openAIImageItem `json:"data"`
	}{Created: time.Now().Unix(), Data: items}
	return json.Marshal(out)
}
