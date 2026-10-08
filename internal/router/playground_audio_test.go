package router

// Speech-modality tests for the console Playground run endpoint: the text
// channel's third dialect. A run under the audio tab speaks the OpenAI
// speech API end to end — the input text, the per-column voice, and the
// shared response format reach the upstream body, and nothing else does.
// The voice is the run's one per-column parameter (a vendor-private id),
// the done event carries a playback reference to the retained clip plus
// the audit row's counted characters, the clip is served owner-pinned by
// the console's playback route, and a console disconnect propagates into
// the in-flight speech exchange exactly the way it does for text and
// image ones.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// newSpeechUpstream stands in for a speech vendor's upstream: it answers
// the OpenAI /v1/audio/speech shape with binary audio, and — the property
// the whole per-column voice design turns on — it VALIDATES the voice
// against its own legal dictionary, refusing a foreign one with the
// vendor's own 400. Two upstreams with two different dictionaries are the
// two vendors of a mixed comparison.
func newSpeechUpstream(t *testing.T, legal map[string]bool, clip []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(body))
		mu.Unlock()
		var req struct {
			Model          string `json:"model"`
			Input          string `json:"input"`
			Voice          string `json:"voice"`
			ResponseFormat string `json:"response_format"`
		}
		if err := json.Unmarshal(body, &req); err != nil || req.Input == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"malformed speech request","type":"invalid_request_error"}}`))
			return
		}
		if !legal[req.Voice] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"voice ` + req.Voice + ` is not served by this vendor","type":"invalid_request_error","code":"invalid_voice"}}`))
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(clip)
	}))
	t.Cleanup(upstream.Close)
	return upstream, bodies
}

// seedPlaygroundAudioModel seeds the provider / provider key / model /
// candidate quadruple for a CHARACTER-billed speech upstream: the model
// declares audio output, the candidate prices per million counted
// characters (2.0 — eight characters settle 18 micros), so a done event's
// figures are real per-character settlement output.
func seedPlaygroundAudioModel(t *testing.T, db *gorm.DB, provider, modelName, providerModelName, baseURL string) *model.Model {
	t.Helper()
	now := time.Now().UTC()
	p := &model.Provider{
		Name: provider, ProviderType: "openai", BaseURL: baseURL,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider %s: %v", provider, err)
	}
	secrets := crypto.NewSecretBox(testutil.ProviderMasterKey())
	enc, err := secrets.Encrypt("sk-upstream-" + provider)
	if err != nil {
		t.Fatalf("encrypt upstream key: %v", err)
	}
	pk := &model.ProviderKey{
		ProviderID: p.ID, Label: "k1", EncryptedKey: enc, KeyPrefix: "sk-upstream-" + provider,
		SortOrder: 1, TestModel: "m", ManagementStatus: model.ProviderKeyStatusEnabled,
		VerificationStatus:           model.VerificationStatusPassed,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(pk).Error; err != nil {
		t.Fatalf("seed provider key %s: %v", provider, err)
	}
	m := &model.Model{
		Name: modelName, ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: `["audio"]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model %s: %v", modelName, err)
	}
	price := 2.0
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: providerModelName,
		InputPrice: 0, OutputPrice: 0, MaxOutput: 4096,
		BillingMode:      model.BillingModeAudio,
		AudioUnitPrice:   &price,
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate %s: %v", providerModelName, err)
	}
	return m
}

// audioRunBody builds an audio-modality run request: the prompt, the
// per-model voices, and the shared response format.
func audioRunBody(keyID uint, models string, voices string, format string) string {
	return fmt.Sprintf(`{"key_id":%d,"modality":"audio","models":[%s],"request":{"prompt":"a red cat"},"voices":{%s},"params":{"format":%q}}`,
		keyID, models, voices, format)
}

// TestPlaygroundAudioRunRealChain: two speech columns of two DIFFERENT
// vendor dictionaries run the real chain, each with a voice from its own
// vendor's list. Both columns succeed — no column 400s on a foreign voice
// — and each upstream received its own column's voice, input, shared
// response format, and routed model, with nothing the page does not
// expose. The done events carry the audit row's counted characters and
// per-character settlement, plus the playback reference that the console
// serves owner-pinned. The second phase is the counterfactual: ONE shared
// voice for both columns (the design this per-column scheme replaces)
// 400s the foreign column while its neighbour completes.
func TestPlaygroundAudioRunRealChain(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	clipA := []byte("ID3\x03audio-column-a-payload")
	clipB := []byte("ID3\x03audio-column-b-payload-longer")
	// Two vendors' private voice dictionaries: neither accepts the other's
	// ids, exactly like the live vendors whose voices do not travel.
	upA, bodiesA := newSpeechUpstream(t, map[string]bool{"alloy": true, "echo": true}, clipA)
	upB, bodiesB := newSpeechUpstream(t, map[string]bool{"nova": true, "shimmer": true}, clipB)
	seedPlaygroundAudioModel(t, db, "pg-aud-a", "audio-model-a", "audio-model-a-real", upA.URL)
	seedPlaygroundAudioModel(t, db, "pg-aud-b", "audio-model-b", "audio-model-b-real", upB.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, plaintext := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-audio-key-0001", nil)

	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", audioRunBody(key.ID,
		`"audio-model-a","audio-model-b"`,
		`"audio-model-a":"echo","audio-model-b":"nova"`, "mp3"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}

	// A speech column is start-then-terminal like an image one: binary
	// audio has no chunk family.
	if chunks := eventsOfKind(t, w, "chunk"); len(chunks) != 0 {
		t.Fatalf("a speech column must not emit chunk events, got %d: %s", len(chunks), w.Body.String())
	}
	dones := eventsOfKind(t, w, "done")
	if len(dones) != 2 {
		t.Fatalf("expected one done per speech column, got %d: %s", len(dones), w.Body.String())
	}
	refs := map[string]string{} // model → playback url
	slots := map[int]bool{}
	for _, data := range dones {
		var done struct {
			Slot      int    `json:"slot"`
			Model     string `json:"model"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
			Usage     struct {
				Characters int `json:"characters"`
				Images     int `json:"images"`
			} `json:"usage"`
			CostMicros int64           `json:"cost_micros"`
			CostKnown  bool            `json:"cost_known"`
			Response   json.RawMessage `json:"response"`
			Audio      *struct {
				URL         string `json:"url"`
				ContentType string `json:"content_type"`
				Bytes       int    `json:"bytes"`
			} `json:"audio"`
		}
		parseAs(t, data, &done)
		if done.Status != http.StatusOK || done.RequestID == "" {
			t.Fatalf("done event mismatch: %s", data)
		}
		// Per-character usage: the audit row counted the input's characters
		// in the settling candidate's own meter ("a red cat" = 9 runes),
		// and the per-character settlement priced them (9 × 2.0 = 18
		// micros — the row's arithmetic, not a second computation).
		if done.Usage.Characters != 9 {
			t.Fatalf("done usage must count 9 characters, got %+v: %s", done.Usage, data)
		}
		if done.Usage.Images != 0 {
			t.Fatalf("a speech row counts characters, not images: %s", data)
		}
		if !done.CostKnown || done.CostMicros != 18 {
			t.Fatalf("done cost must be the per-character settlement 18 micros, got known=%v micros=%d", done.CostKnown, done.CostMicros)
		}
		// Binary audio cannot ride the two-shape response contract; the
		// reference is the channel's honest form for it.
		if len(done.Response) != 0 {
			t.Fatalf("a speech done must not inline the binary body, got %s", string(done.Response))
		}
		if done.Audio == nil {
			t.Fatalf("a finished speech column must carry a playback reference: %s", data)
		}
		if !strings.HasPrefix(done.Audio.URL, "/api/admin/playground/audio/") || !strings.HasSuffix(done.Audio.URL, done.RequestID) {
			t.Fatalf("playback reference must point at the console audio route for this column's row, got %q", done.Audio.URL)
		}
		if done.Audio.ContentType != "audio/mpeg" || done.Audio.Bytes == 0 {
			t.Fatalf("playback reference must announce the exchange's content type and size, got %+v", done.Audio)
		}
		refs[done.Model] = done.Audio.URL
		slots[done.Slot] = true
	}
	if !slots[0] || !slots[1] || refs["audio-model-a"] == "" || refs["audio-model-b"] == "" {
		t.Fatalf("both speech columns must settle on their own slots with references, got %v %v", slots, refs)
	}

	// Pass-through and the reverse assertion, per vendor: each upstream
	// saw ITS column's voice — the per-column parameter — plus the input,
	// the shared response format, and the routed model, and nothing the
	// page does not expose.
	type saw struct {
		bodies             *[]string
		voice, model, clip string
	}
	for _, s := range []saw{
		{bodiesA, `"voice":"echo"`, `"model":"audio-model-a-real"`, string(clipA)},
		{bodiesB, `"voice":"nova"`, `"model":"audio-model-b-real"`, string(clipB)},
	} {
		if len(*s.bodies) != 1 {
			t.Fatalf("expected one upstream request per column, got %d", len(*s.bodies))
		}
		body := (*s.bodies)[0]
		for _, want := range []string{
			`"input":"a red cat"`,
			`"response_format":"mp3"`,
			s.voice,
			s.model,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("upstream body missing %q; body: %s", want, body)
			}
		}
		for _, banned := range []string{
			"temperature", "max_tokens", "system", "messages", // the TEXT family
			"size", "\"n\"", // the IMAGE family
			"stream", "speed", "instructions", "stream_format", // speech axes the page does not expose
		} {
			if strings.Contains(body, banned) {
				t.Errorf("upstream body contains unexposed parameter %q; body: %s", banned, body)
			}
		}
	}

	// The audit rows: playground source, counted characters, per-character
	// settlement — and the key's budget grew by both columns' costs.
	var rows []model.RequestLog
	if err := db.Where("api_key_id = ?", key.ID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected one audit row per speech column, got %d", len(rows))
	}
	for _, row := range rows {
		if row.Source != "playground" || row.StatusCode != http.StatusOK {
			t.Fatalf("speech audit row = source %q status %d, want playground 200", row.Source, row.StatusCode)
		}
		if row.UsageCharacters != 9 {
			t.Fatalf("speech audit row must count 9 characters, got %d", row.UsageCharacters)
		}
		if row.CostMicros != 18 || !row.CostKnown {
			t.Fatalf("speech audit row cost = %d known=%v, want 18 true", row.CostMicros, row.CostKnown)
		}
	}
	after, err := repository.FindAPIKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if after.BudgetSpentMicros != 36 {
		t.Fatalf("budget_spent_micros = %d, want 36 (two columns' settled costs)", after.BudgetSpentMicros)
	}

	// The playback reference is honest in both directions: the owner
	// fetches the very bytes the upstream delivered, another account (and
	// no session at all) gets the same nothing for an id that is not
	// theirs to play.
	fetch := func(path, session string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if session != "" {
			req.AddCookie(&http.Cookie{Name: "session_id", Value: session})
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	ownerA := fetch(refs["audio-model-a"], "tok-pg-member")
	if ownerA.Code != http.StatusOK || ownerA.Header().Get("Content-Type") != "audio/mpeg" || ownerA.Body.String() != string(clipA) {
		t.Fatalf("the owner must fetch the column's delivered clip, got %d %q %q", ownerA.Code, ownerA.Header().Get("Content-Type"), ownerA.Body.String())
	}
	other := seedConsoleUser(t, db, "pg-other", model.RoleMember, "tok-pg-other")
	_ = other
	foreign := fetch(refs["audio-model-a"], "tok-pg-other")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("another account must get 404 for a clip that is not theirs, got %d", foreign.Code)
	}
	if none := fetch(refs["audio-model-a"], ""); none.Code != http.StatusUnauthorized {
		t.Fatalf("a sessionless playback fetch must be refused, got %d", none.Code)
	}

	// None of the run channel's bytes leaked the key plaintext.
	assertNoPlaintext(t, "audio run", scanResponseBytes(w), plaintext)

	// The counterfactual the per-column design exists for: ONE shared
	// voice across both vendors' columns — vendor B's upstream refuses it
	// as a foreign id, and only the column whose vendor knows the voice
	// completes. Per-column voices are what makes the mixed run above
	// succeed.
	w2 := postPlaygroundRun(r, "tok-pg-member", audioRunBody(key.ID,
		`"audio-model-a","audio-model-b"`,
		`"audio-model-a":"alloy","audio-model-b":"alloy"`, "mp3"))
	if w2.Code != http.StatusOK {
		t.Fatalf("expected the second stream to start, got %d body %s", w2.Code, w2.Body.String())
	}
	dones2 := eventsOfKind(t, w2, "done")
	errors2 := eventsOfKind(t, w2, "error")
	if len(dones2) != 1 || len(errors2) != 1 {
		t.Fatalf("a shared foreign voice must 400 exactly the foreign column, got %d done / %d error: %s", len(dones2), len(errors2), w2.Body.String())
	}
	// The failing column keeps its native error shape — what a real
	// caller of the speech ingress receives for a refused voice: the
	// relay's own normalized 400 envelope (the speech path answers
	// upstream failures with its classification, not the vendor's raw
	// body; the vendor's wording lands in the audit row).
	var failure sseErrorPayload
	parseAs(t, errors2[0], &failure)
	if failure.Status != http.StatusBadRequest || !strings.Contains(failure.Error.Message, "400") || failure.Error.Type == "" {
		t.Fatalf("the foreign-voice column must show the speech path's native 400, got %+v: %s", failure, errors2[0])
	}
}

// TestPlaygroundAudioPickerVoices: the model picker carries each speech
// model's vendor voice vocabulary — the dialect table read by the model's
// routable providers' base URLs. A SiliconFlow-backed model lists the
// CosyVoice presets, a Zhipu-backed one its own seven, an unknown base
// falls to the OpenAI dialect, and a non-speech model carries no voices
// member at all.
func TestPlaygroundAudioPickerVoices(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// Real vendor hosts, not a live dial: the picker only READS the base
	// URL to pick the dialect, and nothing in this test dispatches.
	seedPlaygroundAudioModel(t, db, "pg-sf", "sf-speech", "sf-real", "https://api.siliconflow.cn/v1")
	seedPlaygroundAudioModel(t, db, "pg-zp", "zp-speech", "zp-real", "https://open.bigmodel.cn/api/paas/v4")
	seedPlaygroundAudioModel(t, db, "pg-any", "any-speech", "any-real", "https://tts.example.internal")
	seedPlaygroundImageModel(t, db, "pg-img", "img-model", "img-real", "https://img.example.internal")
	seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")

	r := newRelayTestRouter(t, db)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/playground/models", nil)
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("picker refused: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Data struct {
			List []struct {
				Name             string   `json:"name"`
				OutputModalities []string `json:"output_modalities"`
				Voices           []string `json:"voices"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("parse picker page: %v (%s)", err, w.Body.String())
	}
	byName := map[string][]string{}
	for _, row := range page.Data.List {
		byName[row.Name] = row.Voices
	}
	if got := byName["sf-speech"]; len(got) != 8 || got[0] != "FunAudioLLM/CosyVoice2-0.5B:alex" || got[7] != "FunAudioLLM/CosyVoice2-0.5B:diana" {
		t.Fatalf("SiliconFlow-backed speech model must list the CosyVoice presets, got %v", got)
	}
	if got := byName["zp-speech"]; len(got) != 7 || got[0] != "tongtong" || got[6] != "luodo" {
		t.Fatalf("Zhipu-backed speech model must list its pinned voice table, got %v", got)
	}
	if got := byName["any-speech"]; len(got) != 6 || got[0] != "alloy" {
		t.Fatalf("an unknown base must fall to the OpenAI dialect's voices, got %v", got)
	}
	// The voice vocabulary belongs to speech models alone: a non-speech
	// row carries no voices member (omitempty on the wire).
	if got := byName["img-model"]; got != nil {
		t.Fatalf("a non-speech model must not carry voices, got %v", got)
	}
	// And the two vendors' lists are disjoint — the property that makes a
	// shared voice value across a mixed comparison certain to 400.
	for _, sf := range byName["sf-speech"] {
		for _, zp := range byName["zp-speech"] {
			if sf == zp {
				t.Fatalf("vendor voice dictionaries must be disjoint, shared %q", sf)
			}
		}
	}
}

// TestPlaygroundAudioRunRequiresVoicesAndPrompt: the audio modality's own
// body is validated before the stream starts — a blank prompt, a missing
// voice for a selected model, or a format outside the speech vocabulary
// answers with the console envelope and a real 400, and no column ever
// ran.
func TestPlaygroundAudioRunRequiresVoicesAndPrompt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, bodies := newSpeechUpstream(t, map[string]bool{"alloy": true}, []byte("ID3clip"))
	seedPlaygroundAudioModel(t, db, "pg-aud-v", "audio-model", "audio-model-real", upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-audio-req-001", nil)

	r := newRelayTestRouter(t, db)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"blank prompt", fmt.Sprintf(`{"key_id":%d,"modality":"audio","models":["audio-model"],"request":{"prompt":"   "},"voices":{"audio-model":"alloy"}}`, key.ID), "an audio run requires a prompt"},
		{"missing voice for a selected model", fmt.Sprintf(`{"key_id":%d,"modality":"audio","models":["audio-model"],"request":{"prompt":"hi"},"voices":{}}`, key.ID), "an audio run requires a voice for every selected model"},
		{"blank voice for one of two models", fmt.Sprintf(`{"key_id":%d,"modality":"audio","models":["audio-model","audio-model"],"request":{"prompt":"hi"},"voices":{"audio-model":""}}`, key.ID), "an audio run requires a voice for every selected model"},
		{"format outside the speech vocabulary", fmt.Sprintf(`{"key_id":%d,"modality":"audio","models":["audio-model"],"request":{"prompt":"hi"},"voices":{"audio-model":"alloy"},"params":{"format":"mp4"}}`, key.ID), ""},
	}
	for _, tc := range cases {
		w := postPlaygroundRun(r, "tok-pg-member", tc.body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: must be refused with 400, got %d body %s", tc.name, w.Code, w.Body.String())
		}
		if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("%s: the refusal must not open the stream", tc.name)
		}
		if tc.want != "" && !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: refusal must name the failure %q, got %s", tc.name, tc.want, w.Body.String())
		}
	}
	if len(*bodies) != 0 {
		t.Fatalf("a refused run must not reach the upstream, got %d requests", len(*bodies))
	}
}

// TestPlaygroundAudioDisconnectCancelsColumn: while a speech column is in
// flight, the console caller disconnects — the cancel must propagate
// through the speech exchange the same way it does through text and image
// ones: the upstream observes its connection abort, the column answers
// with a best-effort cancel acknowledgement naming its audit row, no done
// event is invented, and the abort lands in the audit trail as
// 499/client_disconnected under the playground source.
func TestPlaygroundAudioDisconnectCancelsColumn(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, aborted, entered := cancelObservingUpstream(t)
	seedPlaygroundAudioModel(t, db, "pg-aud-cx", "audio-cx", "audio-cx-real", upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-aud-cancel-001", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run",
		strings.NewReader(audioRunBody(key.ID, `"audio-cx"`, `"audio-cx":"alloy"`, "mp3")))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	r := newRelayTestRouter(t, db)
	served := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(served)
	}()

	// The speech column is genuinely in flight before the disconnect.
	<-entered
	cancel()
	select {
	case <-served:
	case <-time.After(15 * time.Second):
		t.Fatal("the speech run did not settle after the caller disconnected")
	}

	// The disconnect reached the upstream connection itself.
	deadline := time.Now().Add(5 * time.Second)
	for aborted.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := aborted.Load(); got != 1 {
		t.Fatalf("the speech column's upstream must observe the abort, got %d observations", got)
	}
	// The acknowledgement names the column's audit row; no done is
	// invented for an aborted exchange.
	acks := eventsOfKind(t, w, "cancel_ack")
	if len(acks) != 1 {
		t.Fatalf("expected one cancel_ack for the aborted speech column, got %d: %s", len(acks), w.Body.String())
	}
	var ack sseCancelPayload
	parseAs(t, acks[0], &ack)
	if ack.Slot != 0 || ack.RequestID == "" {
		t.Fatalf("cancel_ack must name slot 0 and the audit row's request id: %s", acks[0])
	}
	if got := eventsOfKind(t, w, "done"); len(got) != 0 {
		t.Fatalf("an aborted speech column must not report done, got %d: %s", len(got), w.Body.String())
	}
	// The durable record: the abort is in the audit trail as a playground
	// row with the caller-gone status.
	var abortedRow model.RequestLog
	if err := db.Where("api_key_id = ? AND fail_reason = ?", key.ID, "client_disconnected").First(&abortedRow).Error; err != nil {
		t.Fatalf("expected a client_disconnected playground row for the aborted speech column: %v", err)
	}
	if abortedRow.StatusCode != 499 || abortedRow.Source != "playground" {
		t.Fatalf("aborted speech row = status %d source %q, want 499 playground", abortedRow.StatusCode, abortedRow.Source)
	}
}
