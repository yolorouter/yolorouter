package audio

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestEncodeMiniMaxSpeechPutsKnobsInVendorSlots(t *testing.T) {
	speed := 1.25
	body, err := EncodeMiniMaxSpeech(MiniMaxSpeechRequest{
		Model:        "speech-2.8-hd",
		Text:         "你好",
		VoiceSetting: MiniMaxVoiceSetting{VoiceID: "male-qn-qingse", Speed: &speed},
		AudioSetting: MiniMaxAudioSetting{Format: "flac"},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		`"model":"speech-2.8-hd"`,
		`"voice_setting":{"voice_id":"male-qn-qingse","speed":1.25}`,
		`"audio_setting":{"format":"flac"}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("body %s does not contain %s", got, want)
		}
	}
	// An unstated speed is omitted, not sent as a zero.
	body, err = EncodeMiniMaxSpeech(MiniMaxSpeechRequest{
		Model: "m", Text: "t", VoiceSetting: MiniMaxVoiceSetting{VoiceID: "v"},
		AudioSetting: MiniMaxAudioSetting{Format: "mp3"},
	})
	if err != nil || strings.Contains(string(body), "speed") {
		t.Errorf("unstated speed leaked into %s (err %v)", body, err)
	}
}

func TestEncodeMiniMaxSpeechRequiresVoiceAndText(t *testing.T) {
	if _, err := EncodeMiniMaxSpeech(MiniMaxSpeechRequest{Text: "t", AudioSetting: MiniMaxAudioSetting{Format: "mp3"}}); err == nil {
		t.Error("accepted an empty voice id")
	}
	if _, err := EncodeMiniMaxSpeech(MiniMaxSpeechRequest{VoiceSetting: MiniMaxVoiceSetting{VoiceID: "v"}, AudioSetting: MiniMaxAudioSetting{Format: "mp3"}}); err == nil {
		t.Error("accepted empty text")
	}
}

func TestParseMiniMaxSpeechResponseDecodesHexAndUsage(t *testing.T) {
	const audio = "ID3fake"
	body := `{"data":{"audio":"` + hex.EncodeToString([]byte(audio)) + `","status":2,"format":"mp3"},` +
		`"extra_info":{"usage_characters":240},"base_resp":{"status_code":0,"status_msg":""}}`
	obs, refusal, err := ParseMiniMaxSpeechResponse([]byte(body))
	if err != nil || refusal != nil {
		t.Fatalf("parse: obs=%+v refusal=%+v err=%v", obs, refusal, err)
	}
	if string(obs.Audio) != audio {
		t.Errorf("audio = %q, want the hex-decoded bytes", obs.Audio)
	}
	if !obs.UsageStated || obs.UsageChars != 240 {
		t.Errorf("usage = %d stated=%v, want 240 stated", obs.UsageChars, obs.UsageStated)
	}
	if obs.Format != "mp3" {
		t.Errorf("format = %q, want the echoed mp3", obs.Format)
	}
}

func TestParseMiniMaxSpeechResponseBusinessRefusal(t *testing.T) {
	obs, refusal, err := ParseMiniMaxSpeechResponse([]byte(
		`{"data":{"audio":""},"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if refusal == nil || refusal.Code != 1004 || refusal.Message != "invalid api key" {
		t.Fatalf("refusal = %+v, want 1004/invalid api key", refusal)
	}
	if obs.Audio != nil {
		t.Error("a refusal carried audio")
	}
}

func TestParseMiniMaxSpeechResponseRejectsBadShapes(t *testing.T) {
	for name, body := range map[string]string{
		"not json":       `{`,
		"no audio":       `{"data":{"audio":""},"base_resp":{"status_code":0}}`,
		"not hex":        `{"data":{"audio":"zz"},"base_resp":{"status_code":0}}`,
		"audio not text": `{"data":{"audio":123},"base_resp":{"status_code":0}}`,
	} {
		if _, _, err := ParseMiniMaxSpeechResponse([]byte(body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
}

func TestMiniMaxMeterDoublesCJKOnly(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},        // ASCII singles
		{"你好", 4},         // Han doubles
		{"你a", 3},         // mixed
		{"あい", 4},         // kana double
		{"한국", 4},         // hangul double
		{"caf\u00e9", 4},  // latin-1 accented is a single
		{"\U00020000", 2}, // supplementary plane CJK double
	}
	for _, tc := range cases {
		if got := MiniMaxMeter(tc.in); got != tc.want {
			t.Errorf("MiniMaxMeter(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMiniMaxSpeechBaseMatchesEverySpelling(t *testing.T) {
	yes := []string{
		"https://api.minimax.cn",
		"https://api.minimax.cn/v1",
		"api.minimax.cn",
		"https://API.Minimax.CN/v1",
		"http://api.minimax.cn:4430/",
	}
	no := []string{
		"",
		"https://api.minimax.io", // the international host is a different deployment
		"https://api.minimax.cn.evil.example.com",
		"https://open.bigmodel.cn",
	}
	for _, b := range yes {
		if !MiniMaxSpeechBase(b) {
			t.Errorf("MiniMaxSpeechBase(%q) = false, want true", b)
		}
	}
	for _, b := range no {
		if MiniMaxSpeechBase(b) {
			t.Errorf("MiniMaxSpeechBase(%q) = true, want false", b)
		}
	}
}

// A stated zero is the vendor's own count and prices the bill at zero —
// official-first means official even when the official number is smaller
// than the re-count.
func TestParseMiniMaxSpeechResponseStatedZeroWins(t *testing.T) {
	body := `{"data":{"audio":"` + hex.EncodeToString([]byte("x")) + `"},` +
		`"extra_info":{"usage_characters":0},"base_resp":{"status_code":0}}`
	obs, _, err := ParseMiniMaxSpeechResponse([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !obs.UsageStated || obs.UsageChars != 0 {
		t.Errorf("usage = %d stated=%v, want the stated 0 to win over any re-count", obs.UsageChars, obs.UsageStated)
	}
}
