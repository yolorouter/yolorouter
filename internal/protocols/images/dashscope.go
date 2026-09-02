package images

// The DashScope native image-generation dialect: the multimodal-generation
// endpoint Alibaba Cloud exposes for qwen-image and wan* models. A request
// that arrived in the OpenAI images shape is re-encoded into this dialect,
// and a response in it is decoded back, because these models are not served
// by the OpenAI-compatible endpoint at their native quality tiers.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// GenerationPath is the DashScope native image-generation endpoint, absolute
// from the provider's origin (scheme://host), not from a versioned base
// path: a provider configured against the compatible-mode base still reaches
// this endpoint on the same host.
const GenerationPath = "/api/v1/services/aigc/multimodal-generation/generation"

// UpstreamURL joins a provider base URL onto the generation endpoint by
// origin: "https://dashscope.aliyuncs.com/compatible-mode/v1" becomes
// "https://dashscope.aliyuncs.com" + GenerationPath. Delegates to the shared
// origin-join helper so this dialect and the kernel agree on one
// implementation of "origin + path".
func UpstreamURL(baseURL string) string {
	return protocols.OriginURL(baseURL, GenerationPath)
}

// IsDashScopeBase reports whether a provider's base URL points at a
// DashScope host (the mainland and international endpoints, and any
// subdomain of either), or at a Model Studio workspace domain under
// maas.aliyuncs.com, which serves the same native endpoints.
func IsDashScopeBase(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	return h == "dashscope.aliyuncs.com" ||
		h == "dashscope-intl.aliyuncs.com" ||
		strings.HasSuffix(h, ".dashscope.aliyuncs.com") ||
		strings.HasSuffix(h, ".dashscope-intl.aliyuncs.com") ||
		strings.HasSuffix(h, ".maas.aliyuncs.com")
}

// ConvertSize rewrites the OpenAI separator ("1024x1024") into the one the
// star-spelled dialects expect ("1024*1024"). Already-converted values pass
// through unchanged, which is also what makes this safe to run twice.
// Exported because the OpenAI-compatible route needs the same rewrite for
// the same model families. The billing table keeps a byte-identical fold on
// its side of the wire boundary (normalizeSizeAxis in internal/model, which
// deliberately imports nothing from the protocol packages): the rule is
// written twice and the two comments point at each other — change one,
// change both.
func ConvertSize(size string) string {
	return strings.ReplaceAll(strings.ReplaceAll(size, "x", "*"), "X", "*")
}

type dashScopeReq struct {
	Model      string          `json:"model"`
	Input      dashScopeInput  `json:"input"`
	Parameters dashScopeParams `json:"parameters"`
}

type dashScopeInput struct {
	Messages []dashScopeMessage `json:"messages"`
}

type dashScopeMessage struct {
	Role    string             `json:"role"`
	Content []dashScopeContent `json:"content"`
}

type dashScopeContent struct {
	// One content item is a text or an image; the empty half is omitted so
	// an image item never carries a "text":"" field the dialect would have
	// to make sense of.
	Text  string `json:"text,omitempty"`
	Image string `json:"image,omitempty"`
}

type dashScopeParams struct {
	N    int    `json:"n"`
	Size string `json:"size,omitempty"`
}

// EncodeRequest builds the DashScope native request for one generation. An
// absent size is omitted so DashScope applies its own default rather than an
// empty string it would have to reject.
func EncodeRequest(prompt, model string, n int, size string) ([]byte, error) {
	if n <= 0 {
		n = 1
	}
	req := dashScopeReq{
		Model: model,
		Input: dashScopeInput{
			Messages: []dashScopeMessage{
				{Role: "user", Content: []dashScopeContent{{Text: prompt}}},
			},
		},
		Parameters: dashScopeParams{N: n, Size: ConvertSize(size)},
	}
	return json.Marshal(req)
}

// EncodeEditRequest builds the DashScope native request for one edit: the
// uploaded reference images travel as base64 data URIs in the content
// array — the dialect accepts the data form anywhere it accepts a hosted
// image URL — with the instruction as the trailing text item. The endpoint
// is the same multimodal-generation one the generation half posts to.
func EncodeEditRequest(prompt, model string, images []EditFile, n int, size string) ([]byte, error) {
	if n <= 0 {
		n = 1
	}
	content := make([]dashScopeContent, 0, len(images)+1)
	for _, img := range images {
		content = append(content, dashScopeContent{Image: imageDataURI(img)})
	}
	content = append(content, dashScopeContent{Text: prompt})
	req := dashScopeReq{
		Model: model,
		Input: dashScopeInput{
			Messages: []dashScopeMessage{{Role: "user", Content: content}},
		},
		Parameters: dashScopeParams{N: n, Size: ConvertSize(size)},
	}
	return json.Marshal(req)
}

// imageDataURI renders one uploaded file as the data URI the native dialect
// reads in place of a hosted URL. A part that arrived without a content
// type — or with curl's default application/octet-stream, which -F sends
// whatever the file is — gets its bytes sniffed rather than passing the
// upload off as a stream of nothing, which the edit endpoint would refuse.
func imageDataURI(f EditFile) string {
	ct := f.ContentType
	if ct == "" || ct == "application/octet-stream" {
		ct = http.DetectContentType(f.Data)
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(f.Data)
}

// The DashScope response shape: output.choices[].message.content[] carries
// the images as {type:"image", image:url}; a non-empty top-level code marks
// a business error delivered with HTTP 200.
type dashScopeResp struct {
	RequestID string          `json:"request_id"`
	Output    dashScopeOutput `json:"output"`
	Code      string          `json:"code"`
	Message   string          `json:"message"`
}

type dashScopeOutput struct {
	Choices  []dashScopeChoice `json:"choices"`
	Finished bool              `json:"finished"`
}

type dashScopeChoice struct {
	Message dashScopeRespMsg `json:"message"`
}

type dashScopeRespMsg struct {
	Content []dashScopeRespContent `json:"content"`
}

type dashScopeRespContent struct {
	// The live endpoint omits "type" on content items that carry an image
	// URL — a non-empty image field is unambiguous on its own, so the type
	// tag is not part of the shape this decoder keys on.
	Image string `json:"image"`
}

// BusinessError is a DashScope answer that arrived with HTTP 200 and a
// non-empty code: the request itself failed (quota, content policy, bad
// parameter) and no amount of retrying the same body will change that.
type BusinessError struct {
	Code    string
	Message string
}

func (e *BusinessError) Error() string {
	return fmt.Sprintf("dashscope business error %s: %s", e.Code, e.Message)
}

// IsBusinessError reports whether err is a DashScope business error, so the
// caller can tell a refused request from a broken one.
func IsBusinessError(err error) bool {
	var be *BusinessError
	return errors.As(err, &be)
}

// openAIImageItem is one delivered image in the OpenAI images shape the
// caller receives.
type openAIImageItem struct {
	URL           string `json:"url"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// DecodeResponse converts a DashScope image response into the OpenAI images
// shape. It returns (openAIBody, imageCount, err):
//   - err is a *BusinessError: the request was refused (code non-empty)
//   - err is anything else: a protocol failure (unparseable body, or a
//     choices array that delivered no images)
//   - err is nil: imageCount is how many images the response carried, which
//     is what per-image billing counts
func DecodeResponse(body []byte) (openAIBody []byte, imageCount int, err error) {
	var resp dashScopeResp
	if parseErr := json.Unmarshal(body, &resp); parseErr != nil {
		return nil, 0, fmt.Errorf("dashscope image decode: %w", parseErr)
	}
	if resp.Code != "" {
		return nil, 0, &BusinessError{Code: resp.Code, Message: resp.Message}
	}
	items := make([]openAIImageItem, 0)
	for _, choice := range resp.Output.Choices {
		for _, c := range choice.Message.Content {
			if c.Image != "" {
				items = append(items, openAIImageItem{URL: c.Image})
			}
		}
	}
	if len(items) == 0 {
		return nil, 0, fmt.Errorf("dashscope image: no images in response choices")
	}
	out := struct {
		Created int64             `json:"created"`
		Data    []openAIImageItem `json:"data"`
	}{Created: time.Now().Unix(), Data: items}
	outBytes, marshalErr := json.Marshal(out)
	if marshalErr != nil {
		return nil, 0, fmt.Errorf("dashscope image marshal response: %w", marshalErr)
	}
	return outBytes, len(items), nil
}

// NormalizeError renders a DashScope failure into the OpenAI error shape.
// statusCode is what the caller will be served, which for a business error
// is not the HTTP status the upstream sent (200) but the one the refusal
// deserves.
func NormalizeError(statusCode int, body []byte) ([]byte, string) {
	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	msg := fmt.Sprintf("dashscope upstream error (http %d)", statusCode)
	if json.Unmarshal(body, &resp) == nil && resp.Message != "" {
		if resp.Code != "" {
			msg = fmt.Sprintf("dashscope error %s: %s", resp.Code, resp.Message)
		} else {
			msg = resp.Message
		}
	}
	errBody, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "upstream_error",
			"code":    statusCode,
		},
	})
	return errBody, "application/json"
}
