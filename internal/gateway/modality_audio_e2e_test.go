package gateway

// End-to-end speech tests: a scriptable OpenAI-shaped upstream, the real
// service, and assertions on what the caller received, what the upstream was
// asked, and what the audit row settled. The rig mirrors the video e2e rig's
// shape — one fake upstream, one provider/key/model/candidate fixture, the
// dialect table overridden onto the local server.

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// speechUpstream is a scriptable OpenAI-shaped speech upstream: it records
// every request it served and answers from the fixture fields.
type speechUpstream struct {
	mu           sync.Mutex
	hits         int
	servedBodies []string
	status       int
	contentType  string
	body         string
	// rejectKeys answers 401 for requests whose Authorization carries one of
	// these plaintexts — the deterministic shape of "this key is bad, the
	// next one works" that key rotation exercises.
	rejectKeys []string
}

func (u *speechUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.hits++
	served, _ := io.ReadAll(r.Body)
	u.servedBodies = append(u.servedBodies, string(served))
	status, ct, body := u.status, u.contentType, u.body
	auth := r.Header.Get("Authorization")
	reject := false
	for _, k := range u.rejectKeys {
		if strings.Contains(auth, k) {
			reject = true
			break
		}
	}
	u.mu.Unlock()
	if reject {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (u *speechUpstream) hitCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

func (u *speechUpstream) lastBody(t *testing.T) map[string]any {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.servedBodies) == 0 {
		t.Fatal("the speech upstream was never asked anything")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(u.servedBodies[len(u.servedBodies)-1]), &doc); err != nil {
		t.Fatalf("upstream body did not parse: %v (%s)", err, u.servedBodies[len(u.servedBodies)-1])
	}
	return doc
}

// speechRig is one audio-endpoint fixture: fake upstream, provider with a
// key, an audio model with an audio-billed candidate, and a caller key.
type speechRig struct {
	svc      *Service
	db       *gorm.DB
	key      *model.APIKey
	upstream *speechUpstream
	server   *httptest.Server
	modelID  uint
}

// overrideSpeechDialect points the dialect table at the rig's local server,
// restoring the real one on cleanup — the same override pattern the video
// dialect gates use.
func overrideSpeechDialect(t *testing.T, serverURL string, d speechDialect) {
	t.Helper()
	prev := speechDialectFor
	speechDialectFor = func(baseURL string) speechDialect {
		if baseURL == serverURL {
			return d
		}
		return prev(baseURL)
	}
	t.Cleanup(func() { speechDialectFor = prev })
}

func newSpeechRig(t *testing.T, pricePerMillion float64) *speechRig {
	t.Helper()
	rig := &speechRig{upstream: &speechUpstream{contentType: "audio/mpeg", body: "ID3fake-mp3-bytes"}}
	rig.server = httptest.NewServer(rig.upstream)
	t.Cleanup(rig.server.Close)

	rig.db = testutil.NewSQLiteDB(t)
	rig.svc = newSvc(t, rig.db)
	p := createProvider(t, rig.db, "speech-provider", rig.server.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, p.ID, "sk-speech-up", "speech-key", 1, true)
	m := createModelAndCandidate(t, rig.db, p, "speech-model", "speech-01", false, false, 1)
	setOutputModalities(t, rig.db, m.ID, `["audio"]`)
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", m.ID).Updates(map[string]any{
		"billing_mode":     model.BillingModeAudio,
		"audio_unit_price": pricePerMillion,
	}).Error; err != nil {
		t.Fatalf("seed audio candidate pricing: %v", err)
	}
	rig.modelID = m.ID
	rig.key = createAPIKey(t, rig.db, model.APIKeyStatusActive, []uint{m.ID})
	return rig
}

func (r *speechRig) speak(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	c, w := newCtxPath(SpeechPath, []byte(body))
	c.Set("request_id", "req-speech-e2e")
	c.Set(BodiesDirContextKey, t.TempDir())
	r.svc.Handle(c, r.key)
	return w
}

func (r *speechRig) latestLog(t *testing.T) model.RequestLog {
	t.Helper()
	var row model.RequestLog
	if err := r.db.Order("id DESC").First(&row).Error; err != nil {
		t.Fatalf("load request log: %v", err)
	}
	return row
}

// The happy path in the default dialect: bytes out, announced as what the
// caller effectively asked for, billed by the rune meter, and both halves of
// the audit row (the request text kept, the response reduced to a digest).
func TestSpeechServesBytesAndSettlesByCharacter(t *testing.T) {
	rig := newSpeechRig(t, 200)
	w := rig.speak(t, `{"model":"speech-model","input":"你好 hello","voice":"tongtong"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want the upstream's audio/mpeg announcement", got)
	}
	if got := w.Body.String(); got != "ID3fake-mp3-bytes" {
		t.Errorf("caller received %q, want the upstream bytes verbatim", got)
	}

	sent := rig.upstream.lastBody(t)
	if sent["model"] != "speech-01" {
		t.Errorf("upstream model = %v, want the candidate's provider model name", sent["model"])
	}
	if sent["voice"] != "tongtong" {
		t.Errorf("voice = %v, want the caller's choice passed through", sent["voice"])
	}
	if sent["response_format"] != "mp3" {
		t.Errorf("response_format = %v, want the dialect default stated explicitly", sent["response_format"])
	}
	if _, has := sent["speed"]; has {
		t.Error("speed was sent when the caller did not state one")
	}

	// The default meter counts characters, spaces included: 8 of them here.
	row := rig.latestLog(t)
	if row.UsageCharacters != 8 {
		t.Errorf("usage_characters = %d, want 8 (the character meter's count)", row.UsageCharacters)
	}
	if !row.CostKnown || row.CostMicros != 200*8 {
		t.Errorf("settlement = known:%v micros:%d, want 1600 known (8 chars at 200 per million)", row.CostKnown, row.CostMicros)
	}
	if row.AudioPricingSnapshot == "" || !strings.Contains(row.AudioPricingSnapshot, `"characters":8`) {
		t.Errorf("audio snapshot = %q, want the metered count inside", row.AudioPricingSnapshot)
	}
	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", row.RequestID).First(&bodyRow).Error; err != nil {
		t.Fatalf("load body row: %v", err)
	}
	if !strings.Contains(bodyRow.RequestBody, "你好 hello") {
		t.Errorf("request body = %q, want the caller's text kept", bodyRow.RequestBody)
	}
	if strings.Contains(bodyRow.UpstreamResponseBody, "ID3fake") {
		t.Error("audio bytes reached the inline audit column")
	}
	// The policy drops the caller-facing body, and the kernel opens no
	// capture file for a policy that keeps none — so served audio is stored
	// nowhere at all, which is the point.
	if bodyRow.StreamBodyPath != "" {
		t.Errorf("served audio landed in a capture file (%s) the policy never asked for", bodyRow.StreamBodyPath)
	}
}

// refusedCandidatesReason asserts the attempt trail carries the gate's
// human-readable refusal, where the exhausted-chain terminal puts it.
func refusedCandidatesReason(t *testing.T, rig *speechRig, want string) {
	t.Helper()
	detail := rig.latestLog(t).AttemptsDetail
	if detail == nil || !strings.Contains(*detail, want) {
		t.Fatalf("attempts detail %v does not carry %q", detail, want)
	}
}

// Speed rides through when stated; the range is the upstream's to judge.
func TestSpeechSpeedRidesThrough(t *testing.T) {
	rig := newSpeechRig(t, 200)
	rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","speed":1.5}`)
	if got := rig.upstream.lastBody(t)["speed"]; got != 1.5 {
		t.Errorf("speed = %v, want 1.5 passed through untouched", got)
	}
}

// The zhipu dialect: wav is the default (not mp3), pcm is served, and mp3 is
// refused by the candidate gate — the caller asked for a format the dialect
// does not have, and the refusal names the set.
func TestSpeechZhipuDialectServesWavByDefault(t *testing.T) {
	rig := newSpeechRig(t, 200)
	overrideSpeechDialect(t, rig.server.URL, speechDialectZhipu)
	rig.upstream.contentType = "audio/wav"
	rig.upstream.body = "RIFFfake-wav-bytes"

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "audio/wav" {
		t.Fatalf("status = %d ct = %q, want 200 audio/wav", w.Code, w.Header().Get("Content-Type"))
	}
	if got := rig.upstream.lastBody(t)["response_format"]; got != "wav" {
		t.Errorf("response_format = %v, want the zhipu default wav", got)
	}

	rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","response_format":"pcm"}`)
	if got := rig.upstream.lastBody(t)["response_format"]; got != "pcm" {
		t.Errorf("pcm = %v, want an explicitly stated supported format through", got)
	}

	w = rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","response_format":"mp3"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want the exhausted chain's 502 for a refused candidate", w.Code)
	}
	refusedCandidatesReason(t, rig, "the zhipu speech dialect serves only wav, pcm")
}

// The siliconflow meter counts UTF-8 bytes: the same text settles at a
// different count than the character meter, and the bill follows the meter.
func TestSpeechSiliconFlowMeterCountsBytes(t *testing.T) {
	rig := newSpeechRig(t, 50)
	overrideSpeechDialect(t, rig.server.URL, speechDialectSiliconFlow)
	rig.speak(t, `{"model":"speech-model","input":"你好","voice":"v"}`)

	// 2 CJK glyphs = 6 UTF-8 bytes.
	row := rig.latestLog(t)
	if row.UsageCharacters != 6 {
		t.Errorf("usage_characters = %d, want 6 (the byte meter's count)", row.UsageCharacters)
	}
	if !row.CostKnown || row.CostMicros != 50*6 {
		t.Errorf("settlement = known:%v micros:%d, want 300 (6 bytes at 50 per million)", row.CostKnown, row.CostMicros)
	}
	if !strings.Contains(row.AudioPricingSnapshot, `"meter":"utf8_bytes"`) {
		t.Errorf("snapshot %s does not name the byte meter — the bill's basis must travel with it", row.AudioPricingSnapshot)
	}
}

// Admission refusals: the four shapes the door owns — a missing field, the
// two unsupported params, and a format outside the endpoint's vocabulary.
func TestSpeechAdmissionRefusals(t *testing.T) {
	rig := newSpeechRig(t, 200)
	cases := []struct {
		name   string
		body   string
		want   string
		reason string
	}{
		{"no input", `{"model":"speech-model","voice":"v"}`, "input is required", "empty_input"},
		{"no voice", `{"model":"speech-model","input":"hi"}`, "voice is required", "empty_voice"},
		{"instructions", `{"model":"speech-model","input":"hi","voice":"v","instructions":"be calm"}`,
			"instructions are not supported", "instructions_unsupported"},
		{"stream_format", `{"model":"speech-model","input":"hi","voice":"v","stream_format":"sse"}`,
			"stream_format is not supported", "stream_format_unsupported"},
		{"unknown format", `{"model":"speech-model","input":"hi","voice":"v","response_format":"xyz"}`,
			"response_format must be one of", "invalid_response_format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := rig.speak(t, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body %q does not say %q", w.Body.String(), tc.want)
			}
			reason := rig.latestLog(t).FailReason
			if reason == nil || !strings.Contains(*reason, tc.reason) {
				t.Errorf("fail reason %v does not carry %q", reason, tc.reason)
			}
		})
	}
	if rig.upstream.hitCount() != 0 {
		t.Fatalf("a refused-at-the-door request reached the upstream %d times", rig.upstream.hitCount())
	}
}

// A provider 5xx is answered, never failed over: the second provider's
// candidate exists and would serve, but the caller named this model's head
// voice and a different provider would speak it differently.
func TestSpeechUpstreamErrorDoesNotFailOver(t *testing.T) {
	rig := newSpeechRig(t, 200)
	rig.upstream.status = http.StatusInternalServerError
	rig.upstream.contentType = "application/json"
	rig.upstream.body = `{"error":{"message":"overloaded"}}`

	// A second, healthy provider with its own candidate for the same model.
	p2 := createProvider(t, rig.db, "speech-provider-2", "https://other-upstream.example.com")
	createProviderKey(t, rig.db, rig.svc.secrets, p2.ID, "sk-speech-2", "speech-key-2", 2, true)
	now := time.Now().UTC()
	if err := rig.db.Create(&model.ModelCandidate{
		ModelID: rig.modelID, ProviderID: p2.ID, ProviderModelName: "speech-01",
		BillingMode: model.BillingModeAudio, ManagementStatus: model.ModelCandidateStatusEnabled,
		VerificationStatus: model.ModelVerificationStatusPassed, SortOrder: 2,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatalf("seed second candidate: %v", err)
	}

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code == http.StatusOK {
		t.Fatal("a 5xx upstream answered 200")
	}
	if rig.upstream.hitCount() != 1 {
		t.Errorf("the failing provider was asked %d times, want exactly one attempt", rig.upstream.hitCount())
	}
	row := rig.latestLog(t)
	if row.CostKnown && row.CostMicros != 0 {
		t.Errorf("a request with no delivered audio settled micros=%d", row.CostMicros)
	}
	if row.UsageCharacters != 0 {
		t.Errorf("usage_characters = %d on a request whose synthesis never produced audio", row.UsageCharacters)
	}
}

// A 200 whose body is not audio is the provider's error envelope wearing a
// success status: the caller receives a proper error, never a fake content
// type, and nothing is billed — no synthesis produced anything.
func TestSpeech200WithoutAudioIsTerminal(t *testing.T) {
	rig := newSpeechRig(t, 200)
	rig.upstream.contentType = "application/json"
	rig.upstream.body = `{"error":{"message":"model overloaded"}}`

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); strings.HasPrefix(ct, "audio/") {
		t.Errorf("Content-Type = %q on a response that carries no audio", ct)
	}
	if !strings.Contains(w.Body.String(), "no audio") {
		t.Errorf("body %q does not say no audio was produced", w.Body.String())
	}
	if rig.upstream.hitCount() != 1 {
		t.Errorf("the no-audio answer retried the upstream %d times", rig.upstream.hitCount()-1)
	}
	row := rig.latestLog(t)
	if row.CostKnown && row.CostMicros != 0 {
		t.Errorf("no-audio delivery settled micros=%d", row.CostMicros)
	}
}

// The budget pre-gate: a key whose ceiling the cheapest estimate would pass
// is turned away before anything is dialled.
func TestSpeechBudgetPrecheckRefusesBeforeDialling(t *testing.T) {
	rig := newSpeechRig(t, 200)
	limit := int64(10)
	if err := rig.db.Model(&model.APIKey{}).Where("id = ?", rig.key.ID).Updates(map[string]any{
		"budget_limit_micros": limit, "budget_spent_micros": int64(9),
	}).Error; err != nil {
		t.Fatalf("seed key budget: %v", err)
	}
	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "audio budget exceeded") {
		t.Errorf("body %q does not name the budget refusal", w.Body.String())
	}
	if rig.upstream.hitCount() != 0 {
		t.Errorf("a budget-refused request dialled the upstream %d times", rig.upstream.hitCount())
	}
}

// overrideMiniMaxSpeechBase points the speech side's minimax gate at the
// rig's local server, restoring the real one on cleanup.
func overrideMiniMaxSpeechBase(t *testing.T, serverURL string) {
	t.Helper()
	prev := isMiniMaxSpeechBase
	isMiniMaxSpeechBase = func(baseURL string) bool { return baseURL == serverURL }
	t.Cleanup(func() { isMiniMaxSpeechBase = prev })
}

// The minimax rig: the base gate and the dialect table both pointed at the
// local server, and the fake upstream answers in the t2a_v2 envelope with
// hex-encoded audio. usageChars < 0 means the answer carries no extra_info.
func newMiniMaxSpeechRig(t *testing.T, price float64, hexAudio string, usageChars int) *speechRig {
	t.Helper()
	rig := newSpeechRig(t, price)
	rig.upstream.contentType = "application/json"
	rig.upstream.body = minimaxSpeechBody(t, hexAudio, usageChars)
	overrideMiniMaxSpeechBase(t, rig.server.URL)
	overrideSpeechDialect(t, rig.server.URL, speechDialectMiniMax)
	return rig
}

func minimaxSpeechBody(t *testing.T, hexAudio string, usageChars int) string {
	t.Helper()
	extra := ""
	if usageChars >= 0 {
		extra = `,"extra_info":{"usage_characters":` + strconv.Itoa(usageChars) + `}`
	}
	return `{"data":{"audio":"` + hexAudio + `","status":2}` + extra + `,"base_resp":{"status_code":0,"status_msg":""}}`
}

func hexEncode(t *testing.T, s string) string {
	t.Helper()
	return hex.EncodeToString([]byte(s))
}

// Happy path: hex decoded, bytes forwarded, announced by the echoed format,
// and billed on the vendor's own usage_characters — the official number the
// invoice would carry, not the gateway's re-count.
func TestSpeechMiniMaxHexHappyPathBillsOfficialCharacters(t *testing.T) {
	rig := newMiniMaxSpeechRig(t, 350, hexEncode(t, "ID3-minimax-audio"), 240)
	w := rig.speak(t, `{"model":"speech-model","input":"任意文本","voice":"male-qn-qingse"}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "ID3-minimax-audio" {
		t.Errorf("caller received %q, want the hex-decoded bytes", got)
	}
	if got := w.Header().Get("Content-Type"); got != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", got)
	}

	// The submit spoke the t2a_v2 shape: voice and speed inside
	// voice_setting, format inside audio_setting.
	sent := rig.upstream.lastBody(t)
	voiceSetting, _ := sent["voice_setting"].(map[string]any)
	if voiceSetting["voice_id"] != "male-qn-qingse" {
		t.Errorf("voice_setting = %v, want the caller's voice in the vendor slot", sent["voice_setting"])
	}
	audioSetting, _ := sent["audio_setting"].(map[string]any)
	if audioSetting["format"] != "mp3" {
		t.Errorf("audio_setting = %v, want the dialect default mp3 stated", sent["audio_setting"])
	}

	// The bill follows the official count, meter named.
	row := rig.latestLog(t)
	if row.UsageCharacters != 240 {
		t.Errorf("usage_characters = %d, want the vendor's 240 (not the re-count)", row.UsageCharacters)
	}
	if !row.CostKnown || row.CostMicros != 350*240 {
		t.Errorf("settlement = known:%v micros:%d, want 84000 (official 240 at 350)", row.CostKnown, row.CostMicros)
	}
	if !strings.Contains(row.AudioPricingSnapshot, `"meter":"minimax_characters"`) || !strings.Contains(row.AudioPricingSnapshot, `"source":"upstream"`) {
		t.Errorf("snapshot %s does not name the minimax meter and upstream source", row.AudioPricingSnapshot)
	}
}

// Speed rides into voice_setting, not the top level.
func TestSpeechMiniMaxSpeedRidesIntoVoiceSetting(t *testing.T) {
	rig := newMiniMaxSpeechRig(t, 350, hexEncode(t, "x"), -1)
	rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","speed":1.2}`)
	sent := rig.upstream.lastBody(t)
	voiceSetting, _ := sent["voice_setting"].(map[string]any)
	if voiceSetting["speed"] != 1.2 {
		t.Errorf("voice_setting.speed = %v, want 1.2 in the vendor slot", sent["voice_setting"])
	}
}

// No usage_characters in the answer: the meter's own count prices the bill,
// the same estimate the pre-gate used (CJK doubled, ASCII single).
func TestSpeechMiniMaxFallsBackToMeterWhenUsageAbsent(t *testing.T) {
	rig := newMiniMaxSpeechRig(t, 350, hexEncode(t, "x"), -1)
	rig.speak(t, `{"model":"speech-model","input":"你a","voice":"v"}`)
	row := rig.latestLog(t)
	if row.UsageCharacters != 3 { // one CJK glyph as two, one ASCII as one
		t.Errorf("usage_characters = %d, want the meter's 3", row.UsageCharacters)
	}
	if !strings.Contains(row.AudioPricingSnapshot, `"source":"request"`) {
		t.Errorf("snapshot %s does not mark the count as request-derived", row.AudioPricingSnapshot)
	}
}

// The format gate names the minimax set: flac serves, aac refuses.
func TestSpeechMiniMaxFormatGate(t *testing.T) {
	rig := newMiniMaxSpeechRig(t, 350, hexEncode(t, "x"), -1)
	if w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","response_format":"flac"}`); w.Code != http.StatusOK {
		t.Fatalf("flac status = %d, body %s", w.Code, w.Body.String())
	}
	audioSetting, _ := rig.upstream.lastBody(t)["audio_setting"].(map[string]any)
	if audioSetting["format"] != "flac" {
		t.Errorf("audio_setting = %v, want flac through", rig.upstream.lastBody(t)["audio_setting"])
	}
	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","response_format":"aac"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("aac status = %d, want the exhausted chain's 502", w.Code)
	}
	refusedCandidatesReason(t, rig, "the minimax speech dialect serves only mp3, opus, flac, wav, pcm")
}

// A business refusal inside the 200 is the caller's to act on: 422 with the
// vendor's code and message, settled unbilled.
func TestSpeechMiniMaxBusinessRefusalAnswered422(t *testing.T) {
	rig := newSpeechRig(t, 350)
	rig.upstream.contentType = "application/json"
	rig.upstream.body = `{"data":{"audio":""},"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`
	overrideMiniMaxSpeechBase(t, rig.server.URL)
	overrideSpeechDialect(t, rig.server.URL, speechDialectMiniMax)

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "1004") || !strings.Contains(w.Body.String(), "invalid api key") {
		t.Errorf("body %q does not carry the vendor's code and message", w.Body.String())
	}
	row := rig.latestLog(t)
	if row.CostKnown && row.CostMicros != 0 {
		t.Errorf("a refused synthesis settled micros=%d", row.CostMicros)
	}
	if row.UsageCharacters != 0 {
		t.Errorf("usage_characters = %d on a refused synthesis", row.UsageCharacters)
	}
}

// A non-2xx answer never reaches a delivery: the kernel classifies it, the
// caller sees the safe status-only message, and the vendor's own wording
// lands in the audit row through the rendered upstream body.
func TestSpeechMiniMaxUpstreamErrorKeepsVendorWordingInAudit(t *testing.T) {
	// 402 (balance) and 412 (parameter) both carry real statuses on this
	// vendor; one kernel path classifies both, so both are pinned against
	// the same assertions.
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"402 balance", http.StatusPaymentRequired},
		{"412 parameter", http.StatusPreconditionFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newSpeechRig(t, 350)
			rig.upstream.status = tc.status
			rig.upstream.contentType = "application/json"
			rig.upstream.body = `{"error":{"message":"insufficient balance","type":"insufficient_balance_error"}}`
			overrideMiniMaxSpeechBase(t, rig.server.URL)
			overrideSpeechDialect(t, rig.server.URL, speechDialectMiniMax)
			assertUpstreamErrorAudit(t, rig, tc.status)
		})
	}
}

func assertUpstreamErrorAudit(t *testing.T, rig *speechRig, status int) {
	t.Helper()
	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("a %d upstream answered 200", status)
	}
	if strings.Contains(w.Body.String(), "insufficient balance") {
		t.Errorf("caller body %q carries the vendor's wording; the safe status message belongs there", w.Body.String())
	}
	var bodyRow model.RequestLogBody
	if err := rig.db.Where("request_id = ?", rig.latestLog(t).RequestID).First(&bodyRow).Error; err != nil {
		t.Fatalf("load body row: %v", err)
	}
	if !strings.Contains(bodyRow.UpstreamResponseBody, "insufficient balance") {
		t.Errorf("audit upstream body %q lost the vendor's wording", bodyRow.UpstreamResponseBody)
	}
}

// Estimate-then-actual: the pre-gate refuses on the meter's count, the
// settlement bills the official count — and when they differ, the bill is
// the official one. Here the official count exceeds the estimate, and the
// settle column proves the correction happened.
func TestSpeechMiniMaxSettlementCorrectsEstimateToOfficial(t *testing.T) {
	// The meter says 3 (你a); the vendor's answer states 7. The door let it
	// through (3 × 350 = 1050 fits a 2000 ceiling); the bill must be
	// 7 × 350 = 2450, correcting to the invoice's number even past the
	// ceiling the estimate cleared.
	rig := newMiniMaxSpeechRig(t, 350, hexEncode(t, "x"), 7)
	limit := int64(2000)
	if err := rig.db.Model(&model.APIKey{}).Where("id = ?", rig.key.ID).
		Update("budget_limit_micros", limit).Error; err != nil {
		t.Fatalf("seed key budget: %v", err)
	}
	rig.speak(t, `{"model":"speech-model","input":"你a","voice":"v"}`)
	row := rig.latestLog(t)
	if row.UsageCharacters != 7 || row.CostMicros != 350*7 {
		t.Errorf("settlement = chars %d micros %d, want the official 7 and 2450 — the estimate only gates the door", row.UsageCharacters, row.CostMicros)
	}
}

// A provider that announces nothing (or bare octets) is still serving audio;
// the caller is told the content type of the format the request effectively
// asked for, never a lie the provider's silence would force.
func TestSpeechUnnamedContentTypeFallsBackToFormatTable(t *testing.T) {
	rig := newSpeechRig(t, 200)
	rig.upstream.contentType = "application/octet-stream"
	rig.upstream.body = "raw-bytes-no-id3"

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v","response_format":"wav"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav from the format table", got)
	}
}

// A stream that dies after the first byte: the caller keeps what arrived,
// the delivery settles truncated on the provider's fault, and the synthesis
// is billed — the provider rendered it whether or not it finished sending.
func TestSpeechCutMidStreamBillsAndSettlesTruncated(t *testing.T) {
	rig := newSpeechRig(t, 50)
	// A Content-Length longer than what is written makes the client's read
	// fail mid-body, which is the honest shape of a provider cutting the
	// stream short.
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ID3partial"))
	}))
	t.Cleanup(cut.Close)

	provider := createProvider(t, rig.db, "cut-provider", cut.URL)
	createProviderKey(t, rig.db, rig.svc.secrets, provider.ID, "sk-cut", "cut-key", 2, true)
	// The walk only ever tries the head candidate, so retarget the head's
	// provider rather than appending.
	if err := rig.db.Model(&model.ModelCandidate{}).Where("model_id = ?", rig.modelID).
		Update("provider_id", provider.ID).Error; err != nil {
		t.Fatalf("retarget candidate: %v", err)
	}

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: the first byte committed the 200", w.Code)
	}
	if got := w.Body.String(); got != "ID3partial" {
		t.Errorf("caller received %q, want the partial bytes that did arrive", got)
	}
	row := rig.latestLog(t)
	// The wire said 200 — the first byte committed it — but the row records
	// the billing status, and a provider-fault truncation settles as 502.
	if row.StatusCode != http.StatusBadGateway {
		t.Errorf("row status = %d, want the truncated settlement's 502", row.StatusCode)
	}
	// Truncated-on-upstream-fault still bills: 2 characters were synthesised.
	if !row.CostKnown || row.CostMicros != 50*2 {
		t.Errorf("settlement = known:%v micros:%d, want the synthesised characters billed", row.CostKnown, row.CostMicros)
	}
	if row.UsageCharacters != 2 {
		t.Errorf("usage_characters = %d, want 2", row.UsageCharacters)
	}
	if fail := row.FailReason; fail == nil || !strings.Contains(*fail, "audio_cut_short") && !strings.Contains(*fail, "unexpected EOF") {
		t.Errorf("fail reason %v does not describe the cut stream", fail)
	}
}

// Key rotation inside the ONE provider is the retry no-failover keeps: a 401
// on the first key moves to the second key of the same provider, and the
// caller still gets their audio from the same voice.
func TestSpeechRotatesKeysWithinTheProvider(t *testing.T) {
	rig := newSpeechRig(t, 200)
	rig.upstream.rejectKeys = []string{"sk-speech-up"}
	createProviderKey(t, rig.db, rig.svc.secrets, mustProviderID(t, rig.db, "speech-provider"), "sk-speech-rot", "speech-key-2", 2, true)

	w := rig.speak(t, `{"model":"speech-model","input":"hi","voice":"v"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s — key rotation within the provider must keep working", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "ID3fake-mp3-bytes" {
		t.Errorf("caller received %q", got)
	}
	if hits := rig.upstream.hitCount(); hits != 2 {
		t.Errorf("upstream hits = %d, want one rejected attempt and one served", hits)
	}
}

func mustProviderID(t *testing.T, db *gorm.DB, name string) uint {
	t.Helper()
	var p model.Provider
	if err := db.Where("name = ?", name).First(&p).Error; err != nil {
		t.Fatalf("load provider %q: %v", name, err)
	}
	return p.ID
}

// The ceiling is a stated number, not an accident: pin it so a change is a
// decision, not a drift.
func TestSpeechResponseCeilingIsPinned(t *testing.T) {
	if got := (audioModality{}).Limits().MaxResponseBytes; got != 32<<20 {
		t.Errorf("MaxResponseBytes = %d, want 32MiB", got)
	}
}
