package providerclient

// The image-input chat probe: one minimal vision request, in the protocol
// the mapping already speaks. It answers a tri-state question — does this
// chat mapping accept an image as input — and is deliberately cheap: a 1x1
// PNG rides inline as a data URI (or the protocol's native inline spelling),
// and max_tokens keeps the answer to a word. Unlike every probe that feeds
// a stored verdict, this one only reports to the admin console, so its
// request-rejected mapping may stay probe-specific instead of entering the
// shared classification.

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/protocols"
)

// visionProbePrompt is what the probe asks the model to do with the image:
// a complete, answerable instruction, so a model that genuinely looked at
// the image has something to say within one token's budget.
const visionProbePrompt = "What color is this image? Answer in one word."

// visionProbeImage is the 1x1 solid-colour PNG every image-input probe
// sends, rendered with the standard library on first use rather than
// embedded as a byte literal or loaded from a file the binary would have
// to carry. One pixel keeps the base64 payload (and the upstream's billing
// for the request) as small as a valid PNG allows.
var visionProbeImage = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 30, G: 120, B: 200, A: 255}}, image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic("providerclient: render vision probe image: " + err.Error())
	}
	return buf.Bytes()
}()

// visionProbeImageBase64 is the PNG's base64 form. The OpenAI, Anthropic and
// Gemini vision shapes all carry raw base64 (their wrappers differ); only
// the OpenAI-family data URI adds the mime prefix on top.
var visionProbeImageBase64 = base64.StdEncoding.EncodeToString(visionProbeImage)

// TestImageInputChat probes whether baseURL+model accepts one chat request
// carrying an image, spoken in proto's own wire shape so the answer applies
// to the mapping the admin is about to trust. A 200 whose body certifies as
// a real completion passes; a 400 or 415 is the upstream rejecting the
// request's shape — decisive evidence the model does not take image input,
// since the same endpoint, key and model just passed the text probe. Every
// other status classifies through the shared rules: a 404 still means the
// model is not served here, and auth, rate and 5xx answers say nothing
// about vision either way.
func (c *HTTPProviderClient) TestImageInputChat(ctx context.Context, proto protocols.ProtocolID, baseURL, apiKey, model string) (TestResult, error) {
	payload := probeSpecFor(proto).visionPayload(model)
	return c.runTestRequest(ctx, proto, baseURL, apiKey, model, payload, func(resp *http.Response, duration int64) (TestResult, error) {
		body, ok := readBoundedBody(resp)
		if !ok {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
		}
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnsupportedMediaType {
			return TestResult{
				Outcome:    TestRequestRejected,
				DurationMs: duration,
				Detail:     upstreamErrorDetail(proto, resp.StatusCode, body),
			}, nil
		}
		return classifyResponse(proto, resp, body, model, duration), nil
	})
}
