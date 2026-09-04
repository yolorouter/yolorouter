package providerclient

// The speech probe: one short synthesis, in the dialect the base speaks —
// the same table the gateway routes and bills by, so a probe can never
// measure an endpoint a routed request would not hit. Cheap by design (a
// few characters of audio), which is also why, unlike the video and image
// fallbacks, it is NOT gated on a media-dialect hostname: a base whose chat
// probe says the model is not served there earns one cheap speech probe on
// any host, and a host without audio answers it with an unbilled 404.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/audio"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
)

// speechDialectFor routes the probe through the shared dialect table; a var
// so a local test server can carry any dialect, the same reason every
// dialect gate here is one.
var speechDialectFor = audio.DialectFor

// speechProbeText is what the probe asks to synthesise: short on every
// meter, ASCII so no dialect's CJK doubling inflates it, and a sentence so
// nothing about it reads as a degenerate request.
const speechProbeText = "Hello from the speech probe."

// TestSpeechGeneration probes a speech mapping the way a speech request
// reaches the provider: one synthesis in the dialect the base speaks. A 200
// that carries audio (announced as audio, or bare octets like the modality
// accepts) passes; a 200 that carries anything else is read through the
// dialect's own parser, which answers for the minimax envelope and rejects
// the rest.
func (c *HTTPProviderClient) TestSpeechGeneration(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	dialect := speechDialectFor(baseURL)
	if isMiniMaxSpeechBase(baseURL) {
		body, err := audio.EncodeMiniMaxSpeech(audio.MiniMaxSpeechRequest{
			Model:        model,
			Text:         speechProbeText,
			VoiceSetting: audio.MiniMaxVoiceSetting{VoiceID: dialect.ProbeVoice},
			AudioSetting: audio.MiniMaxAudioSetting{Format: dialect.DefaultFormat},
		})
		if err != nil {
			return TestResult{}, err
		}
		return c.runRawTestRequestAt(ctx, videos.Origin(baseURL)+audio.MiniMaxSpeechPath,
			protocols.ProtocolOpenAI, apiKey, "application/json", body, nil,
			func(resp *http.Response, duration int64) (TestResult, error) {
				return parseSpeechProbeResponse(resp, duration, model, func(body []byte) error {
					_, refusal, perr := audio.ParseMiniMaxSpeechResponse(body)
					if perr != nil {
						return perr
					}
					if refusal != nil {
						return fmt.Errorf("HTTP 200: minimax error %d: %s", refusal.Code, refusal.Message)
					}
					return nil
				})
			})
	}

	payload := map[string]interface{}{
		"model":           model,
		"input":           speechProbeText,
		"response_format": dialect.DefaultFormat,
	}
	if dialect.ProbeVoice != "" {
		payload["voice"] = dialect.ProbeVoice
	}
	speechURL := protocols.JoinUpstreamURL(baseURL, "/audio/speech", protocols.ProtocolOpenAI)
	return c.runTestRequestAt(ctx, speechURL, protocols.ProtocolOpenAI, apiKey, model, payload,
		func(resp *http.Response, duration int64) (TestResult, error) {
			return parseSpeechProbeResponse(resp, duration, model, nil)
		})
}

// parseSpeechProbeResponse is the shared verdict over one speech probe
// answer: a 200 that announces audio, or announces bare octets, passes on
// the announced type alone — the body IS the synthesized audio, easily a few
// hundred KB (Zhipu's ~4s wav is ~200KB, live 2026-09-05) and well past the
// probe's bounded read, so a probe that slurped it to "verify" would misread
// every long answer as an upstream error. The acceptance is deliberately
// stricter than the modality's delivery rule, which also re-announces an
// empty content type from the request's effective format: a probe has
// nothing to re-announce, so an unannounced 200 must prove itself through
// the body paths below instead of being trusted. A non-200 goes through the
// standard classification; a 200 that carries anything else is handed to the
// dialect's body parser when it has one and refused otherwise — a JSON error
// envelope wearing a success status is the provider failing, not the mapping
// working.
func parseSpeechProbeResponse(resp *http.Response, duration int64, model string, parseBody func([]byte) error) (TestResult, error) {
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode == http.StatusOK && (strings.HasPrefix(ct, "audio/") || ct == "application/octet-stream") {
		return TestResult{Outcome: TestSuccess, DurationMs: duration}, nil
	}
	body, ok := readBoundedBody(resp)
	if !ok {
		return TestResult{Outcome: TestUpstreamError, DurationMs: duration}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return classifyResponse(protocols.ProtocolOpenAI, resp, body, model, duration), nil
	}
	if parseBody != nil {
		if err := parseBody(body); err != nil {
			return TestResult{Outcome: TestUpstreamError, DurationMs: duration, Detail: "speech probe body: " + err.Error()}, nil
		}
		return TestResult{Outcome: TestSuccess, DurationMs: duration}, nil
	}
	return TestResult{Outcome: TestUpstreamError, DurationMs: duration,
		Detail: fmt.Sprintf("speech probe: 200 with content type %s carries no audio", ct)}, nil
}
