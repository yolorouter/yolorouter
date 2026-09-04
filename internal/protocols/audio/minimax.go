// Package audio holds the speech dialects whose wire shapes differ from
// the OpenAI /v1/audio/speech form the gateway answers callers in. The
// OpenAI shape itself lives in the modality (it is one JSON encode); what
// needs a codec package is a dialect with its own response envelope.
package audio

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// MiniMaxSpeechPath is the t2a_v2 endpoint, relative to the provider origin
// the kernel resolves from its base URL.
const MiniMaxSpeechPath = "/v1/t2a_v2"

// MiniMaxSpeechBase reports whether a provider base URL points at the one
// host the t2a_v2 dialect lives on. The speech side's own predicate rather
// than the video dialect's: same vendor, same host, different dialect
// family, and a scheme-less or odd-cased base must resolve identically here
// and in the dialect table that gates formats and meters.
func MiniMaxSpeechBase(baseURL string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.minimax.cn")
}

// MiniMaxSpeechRequest is the submit body the t2a_v2 dialect speaks. The
// voice and speed slots live inside voice_setting rather than at the top
// level, and everything else the endpoint offers (pitch, emotion, timbre
// weights, sample rate) is deliberately absent: this gateway's surface is
// the OpenAI one, and a knob it cannot carry is not sent half-addressed.
type MiniMaxSpeechRequest struct {
	Model        string              `json:"model"`
	Text         string              `json:"text"`
	VoiceSetting MiniMaxVoiceSetting `json:"voice_setting"`
	AudioSetting MiniMaxAudioSetting `json:"audio_setting"`
}

// MiniMaxVoiceSetting is the voice_setting slot of the submit body.
type MiniMaxVoiceSetting struct {
	VoiceID string   `json:"voice_id"`
	Speed   *float64 `json:"speed,omitempty"`
}

// MiniMaxAudioSetting is the audio_setting slot of the submit body.
type MiniMaxAudioSetting struct {
	// Format is always stated: the announcement the caller is told must
	// match the bytes that come back, and the vendor's default is not this
	// gateway's to guess.
	Format string `json:"format"`
}

// EncodeMiniMaxSpeech serializes the submit body for one synthesis.
func EncodeMiniMaxSpeech(req MiniMaxSpeechRequest) ([]byte, error) {
	if req.VoiceSetting.VoiceID == "" {
		return nil, fmt.Errorf("minimax speech submit needs a voice id")
	}
	if req.Text == "" {
		return nil, fmt.Errorf("minimax speech submit needs text")
	}
	return json.Marshal(req)
}

// MiniMaxSpeechObservation is one parsed submit answer: the audio bytes
// (hex-decoded from data.audio), the format they were rendered in as the
// endpoint echoed it, and the vendor's own character count when it stated
// one. UsageChars is 0 with UsageStated false when the answer carried no
// extra_info — the caller then falls back to counting the request itself.
type MiniMaxSpeechObservation struct {
	Audio       []byte
	Format      string
	UsageChars  int
	UsageStated bool
}

// MiniMaxRefusal is a business refusal inside a 200: the t2a family carries
// its error in base_resp even on a success status, so the status line alone
// cannot see it.
type MiniMaxRefusal struct {
	Code    int
	Message string
}

// minimaxSpeechResponse is the envelope the endpoint answers with. Fields
// beyond the three this gateway reads are tolerated and ignored.
type minimaxSpeechResponse struct {
	Data struct {
		Audio  string `json:"audio"`
		Status int    `json:"status"`
		Format string `json:"format"`
	} `json:"data"`
	ExtraInfo *struct {
		// A pointer: a stated zero is the vendor's own count and wins over
		// the re-count, the same as any other stated figure; only a field
		// the answer never carried falls back.
		UsageCharacters *int `json:"usage_characters"`
	} `json:"extra_info"`
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// ParseMiniMaxSpeechResponse reads one submit answer. A base_resp status
// other than zero is a refusal even though the HTTP status said 200; an
// answer with neither audio nor a refusal is undecodable rather than empty,
// because "succeeded with no audio" is a shape the endpoint does not have.
func ParseMiniMaxSpeechResponse(body []byte) (MiniMaxSpeechObservation, *MiniMaxRefusal, error) {
	var resp minimaxSpeechResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return MiniMaxSpeechObservation{}, nil, fmt.Errorf("minimax speech decode: %w", err)
	}
	if resp.BaseResp != nil && resp.BaseResp.StatusCode != 0 {
		return MiniMaxSpeechObservation{}, &MiniMaxRefusal{
			Code:    resp.BaseResp.StatusCode,
			Message: resp.BaseResp.StatusMsg,
		}, nil
	}
	if resp.Data.Audio == "" {
		return MiniMaxSpeechObservation{}, nil, fmt.Errorf("minimax speech answer carries no audio")
	}
	audio, err := hex.DecodeString(resp.Data.Audio)
	if err != nil {
		return MiniMaxSpeechObservation{}, nil, fmt.Errorf("minimax speech audio is not hex: %w", err)
	}
	obs := MiniMaxSpeechObservation{Audio: audio, Format: resp.Data.Format}
	if resp.ExtraInfo != nil && resp.ExtraInfo.UsageCharacters != nil {
		obs.UsageChars = *resp.ExtraInfo.UsageCharacters
		obs.UsageStated = true
	}
	return obs, nil, nil
}

// MiniMaxMeter counts text in the vendor's own character rule: one CJK
// glyph bills as two characters, everything else as one. This is the
// estimate basis; the settlement prefers the endpoint's own
// usage_characters when it states one, so a mis-guessed range here
// mis-estimates the pre-gate, never the bill.
func MiniMaxMeter(input string) int {
	count := 0
	for _, r := range input {
		if minimaxCountsDouble(r) {
			count += 2
			continue
		}
		count++
	}
	return count
}

// minimaxCountsDouble reports whether a rune sits in a CJK block — Han and
// its extensions, kana, hangul, the radical and compatibility ranges. The
// exact boundary of the vendor's own rule is unverified against its
// invoice; every block it plausibly counts is included so the estimate
// errs high (refusing early) rather than low.
func minimaxCountsDouble(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // radicals, ext A, unified, kana
	case r >= 0xAC00 && r <= 0xD7AF: // hangul syllables
	case r >= 0xF900 && r <= 0xFAFF: // compatibility ideographs
	case r >= 0x20000: // supplementary ideograph planes
	default:
		return false
	}
	return true
}
