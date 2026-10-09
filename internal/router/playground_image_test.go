package router

// Image-modality tests for the console Playground run endpoint: the text
// channel's second dialect. A run under the image tab speaks the OpenAI
// Images API end to end — the exposed axes (size, count) and nothing else
// reach the upstream body, the column is start-then-terminal (no chunk
// events for a JSON answer), the done event carries the delivered images
// with the audit row's per-image count and figures, an over-cap response
// is marked as truncated rather than passed off as whole, and a console
// disconnect propagates into the in-flight image exchange exactly the way
// it does for a text one.

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

// newImagesUpstream stands in for an upstream that answers the OpenAI
// Images API shape: it records every request body it receives and answers
// the fixed two-image body below — two artifact references a done event
// must carry through, plus the token counts a token-billed image model
// would report.
func newImagesUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1770000000,"data":[` +
			`{"url":"https://img.example/one.png","revised_prompt":"a red cat"},` +
			`{"url":"https://img.example/two.png"}` +
			`],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream, bodies
}

// seedPlaygroundImageModel seeds the provider / provider key / model /
// candidate quadruple for an IMAGE-billed upstream: the model declares
// image output, and the candidate prices per delivered image through a
// default-price tier table (0.04 per image — two delivered images settle
// 80000 micros), so the audit figures a done event reports are real
// settlement output rather than a token-priced accident.
func seedPlaygroundImageModel(t *testing.T, db *gorm.DB, provider, modelName, providerModelName, baseURL string) *model.Model {
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
		SchedulingMode: model.ModelSchedulingModeFailover, OutputModalities: `["image"]`,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model %s: %v", modelName, err)
	}
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: providerModelName,
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		BillingMode:       model.BillingModeImage,
		ImagePricingTiers: `{"mode":"per_image","default_price":0.04}`,
		ManagementStatus:  model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate %s: %v", providerModelName, err)
	}
	return m
}

// imageRunBody builds an image-modality run request body with the exposed
// axes set.
func imageRunBody(keyID uint, models string, size string, n int) string {
	return fmt.Sprintf(`{"key_id":%d,"modality":"image","models":[%s],"request":{"prompt":"a red cat"},"params":{"size":%q,"n":%d}}`,
		keyID, models, size, n)
}

// TestPlaygroundImageRunRealChain: two image columns run the real chain.
// The upstream receives the prompt, BOTH exposed axes (size and count) and
// the routed model — and none of the parameters the page does not expose.
// Each column answers with exactly one terminal done event (no chunks: a
// JSON answer has no deltas), whose usage counts the delivered images,
// whose cost is the audit row's per-image settlement, and whose response
// embeds the delivered artifact references. The audit rows carry the
// playground source with the image count, and the key's budget grows by
// the settled cost.
func TestPlaygroundImageRunRealChain(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, bodies := newImagesUpstream(t)
	seedPlaygroundImageModel(t, db, "pg-img-a", "img-model-a", "img-model-a-real", upstream.URL)
	seedPlaygroundImageModel(t, db, "pg-img-b", "img-model-b", "img-model-b-real", upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, plaintext := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-image-key-0001", nil)

	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", imageRunBody(key.ID, `"img-model-a","img-model-b"`, "1024x1024", 2))
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}

	// The image column's two-state contract on the channel: start, then
	// the terminal event — no chunk family exists for a JSON answer.
	if chunks := eventsOfKind(t, w, "chunk"); len(chunks) != 0 {
		t.Fatalf("an image column must not emit chunk events, got %d: %s", len(chunks), w.Body.String())
	}
	dones := eventsOfKind(t, w, "done")
	if len(dones) != 2 {
		t.Fatalf("expected one done per image column, got %d: %s", len(dones), w.Body.String())
	}
	slots := map[int]bool{}
	for _, data := range dones {
		var done struct {
			Slot      int    `json:"slot"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
			Usage     struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				Images       int `json:"images"`
			} `json:"usage"`
			CostMicros int64           `json:"cost_micros"`
			CostKnown  bool            `json:"cost_known"`
			Response   json.RawMessage `json:"response"`
			Truncated  bool            `json:"response_truncated"`
		}
		parseAs(t, data, &done)
		if done.Status != http.StatusOK || done.RequestID == "" {
			t.Fatalf("done event mismatch: %s", data)
		}
		// Per-image usage: the audit row counted two delivered images (the
		// token sub-counts ride along for a token-billed reader).
		if done.Usage.Images != 2 {
			t.Fatalf("done usage must count 2 delivered images, got %+v: %s", done.Usage, data)
		}
		// Per-image settlement: 2 images at the seeded 0.04 default price
		// is 80000 micros — the audit row's own arithmetic, not a second
		// computation.
		if !done.CostKnown || done.CostMicros != 80000 {
			t.Fatalf("done cost must be the per-image settlement 80000 micros, got known=%v micros=%d", done.CostKnown, done.CostMicros)
		}
		// The artifact references: the response is embedded as its own
		// JSON, whole (not truncated), with both delivered images inside.
		if done.Truncated {
			t.Fatalf("a response under the inline cap must not be marked truncated: %s", data)
		}
		if len(done.Response) == 0 || done.Response[0] != '{' {
			t.Fatalf("done response must embed the response's own JSON object, got %s", string(done.Response))
		}
		var delivered struct {
			Data []struct {
				URL string `json:"url"`
			} `json:"data"`
		}
		if err := json.Unmarshal(done.Response, &delivered); err != nil {
			t.Fatalf("done response is not the images JSON: %v (%s)", err, string(done.Response))
		}
		if len(delivered.Data) != 2 || delivered.Data[0].URL != "https://img.example/one.png" || delivered.Data[1].URL != "https://img.example/two.png" {
			t.Fatalf("done response must carry both artifact references, got %+v", delivered.Data)
		}
		slots[done.Slot] = true
	}
	if !slots[0] || !slots[1] {
		t.Fatalf("both image columns must settle on their own slots, got %v", slots)
	}

	// Parameter pass-through and the reverse assertion: the upstream saw
	// the prompt, the size and count axes, and the routed provider model —
	// and nothing the page does not expose.
	if len(*bodies) != 2 {
		t.Fatalf("expected one upstream request per column, got %d", len(*bodies))
	}
	// The columns run concurrently, so arrival order is not slot order;
	// each body is matched against its own routed model name.
	sawnModels := map[string]bool{}
	for _, saw := range *bodies {
		modelName := ""
		for _, cand := range []string{`"model":"img-model-a-real"`, `"model":"img-model-b-real"`} {
			if strings.Contains(saw, cand) {
				modelName = cand
			}
		}
		if modelName == "" {
			t.Fatalf("upstream body names neither routed model: %s", saw)
		}
		if sawnModels[modelName] {
			t.Fatalf("the same model's column ran twice: %s", saw)
		}
		sawnModels[modelName] = true
		for _, want := range []string{
			`"prompt":"a red cat"`,
			`"size":"1024x1024"`,
			`"n":2`,
			modelName, // routed to the candidate's provider name
		} {
			if !strings.Contains(saw, want) {
				t.Errorf("upstream body missing %q; body: %s", want, saw)
			}
		}
		for _, banned := range []string{
			"temperature", "max_tokens", "system", "messages", // the TEXT family
			"quality", "response_format", "style", "user", // images axes the page does not expose
			"stream",
		} {
			if strings.Contains(saw, banned) {
				t.Errorf("upstream body contains unexposed parameter %q; body: %s", banned, saw)
			}
		}
	}
	if len(sawnModels) != 2 {
		t.Fatalf("expected both routed models upstream, saw %v", sawnModels)
	}

	// The audit rows: playground source, per-image count, per-image
	// settlement — and the key's budget grew by both columns' costs.
	var rows []model.RequestLog
	if err := db.Where("api_key_id = ?", key.ID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected one audit row per image column, got %d", len(rows))
	}
	for _, row := range rows {
		if row.Source != "playground" || row.StatusCode != http.StatusOK {
			t.Fatalf("image audit row = source %q status %d, want playground 200", row.Source, row.StatusCode)
		}
		if row.ImageCount != 2 {
			t.Fatalf("image audit row must count 2 delivered images, got %d", row.ImageCount)
		}
		if row.CostMicros != 80000 || !row.CostKnown {
			t.Fatalf("image audit row cost = %d known=%v, want 80000 true", row.CostMicros, row.CostKnown)
		}
	}
	after, err := repository.FindAPIKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if after.BudgetSpentMicros != 160000 {
		t.Fatalf("budget_spent_micros = %d, want 160000 (two columns' settled costs)", after.BudgetSpentMicros)
	}

	// And none of the run's response bytes leaked the key plaintext.
	assertNoPlaintext(t, "image run", scanResponseBytes(w), plaintext)
}

// TestPlaygroundImageRunRequiresPrompt: the image modality's body is
// validated before the stream starts — a run without a prompt answers
// with the console envelope and a real 400, and no column ever ran.
func TestPlaygroundImageRunRequiresPrompt(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, bodies := newImagesUpstream(t)
	seedPlaygroundImageModel(t, db, "pg-img", "img-model", "img-model-real", upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-image-nop-0001", nil)

	r := newRelayTestRouter(t, db)
	body := fmt.Sprintf(`{"key_id":%d,"modality":"image","models":["img-model"],"request":{"prompt":"   "}}`, key.ID)
	w := postPlaygroundRun(r, "tok-pg-member", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a promptless image run must be refused with 400, got %d body %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("the refusal must not open the stream")
	}
	if len(*bodies) != 0 {
		t.Fatalf("a refused run must not reach the upstream, got %d requests", len(*bodies))
	}
}

// TestPlaygroundImageResponseTruncated: an image answer larger than the
// channel's inline response cap is delivered marked, not silently — the
// done event carries the capped byte prefix as a plain string with the
// truncation flag set, so a consumer can tell a cut body from a whole one
// and the channel never claims completeness it does not have.
func TestPlaygroundImageResponseTruncated(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	// One image whose base64 payload pushes the answer past the channel's
	// 1 MiB inline cap (and stays far under the gateway's own 32 MiB
	// non-stream cap, so the exchange itself succeeds).
	big := strings.Repeat("A", 1<<20+4096)
	fullBody := `{"created":1,"data":[{"b64_json":"` + big + `"}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullBody))
	}))
	t.Cleanup(upstream.Close)
	seedPlaygroundImageModel(t, db, "pg-img-big", "img-big", "img-big-real", upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-image-big-0001", nil)

	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", imageRunBody(key.ID, `"img-big"`, "1024x1024", 1))
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}
	dones := eventsOfKind(t, w, "done")
	if len(dones) != 1 {
		t.Fatalf("the over-cap column still settles as done: %s", w.Body.String())
	}
	var done struct {
		Usage struct {
			Images int `json:"images"`
		} `json:"usage"`
		Response  json.RawMessage `json:"response"`
		Truncated bool            `json:"response_truncated"`
	}
	parseAs(t, dones[0], &done)
	if !done.Truncated {
		t.Fatalf("an over-cap response must carry the truncation flag: %s", dones[0])
	}
	if len(done.Response) == 0 || done.Response[0] != '"' {
		t.Fatalf("a truncated response must embed the byte prefix as a string, got %s", string(done.Response))
	}
	var prefix string
	if err := json.Unmarshal(done.Response, &prefix); err != nil {
		t.Fatalf("the truncated prefix is not a JSON string: %v", err)
	}
	// The prefix is bounded by the inline cap and is genuinely a prefix of
	// what the upstream sent — cut, not swallowed whole.
	if len(prefix) > 1<<20 {
		t.Fatalf("the embedded prefix must respect the inline cap, got %d bytes", len(prefix))
	}
	if !strings.HasPrefix(fullBody, prefix) {
		t.Fatalf("the embedded bytes must be a prefix of the delivered body")
	}
	// The figures the page needs do not depend on the body bytes: the
	// audit row counted the image all the same.
	if done.Usage.Images != 1 {
		t.Fatalf("the over-cap column's usage must still count 1 image, got %+v", done.Usage)
	}
}

// TestPlaygroundImageDisconnectCancelsColumn: while an image column is in
// flight, the console caller disconnects — the cancel must propagate
// through the image exchange the same way it does through a text one: the
// upstream observes its connection abort, the column answers with a
// best-effort cancel acknowledgement naming its audit row, no done event
// is invented, and the abort lands in the audit trail as
// 499/client_disconnected under the playground source.
func TestPlaygroundImageDisconnectCancelsColumn(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, aborted, entered := cancelObservingUpstream(t)
	seedPlaygroundImageModel(t, db, "pg-img-cx", "img-cx", "img-cx-real", upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-img-cancel-001", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run",
		strings.NewReader(imageRunBody(key.ID, `"img-cx"`, "1024x1024", 1)))
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

	// The image column is genuinely in flight before the disconnect.
	<-entered
	cancel()
	select {
	case <-served:
	case <-time.After(15 * time.Second):
		t.Fatal("the image run did not settle after the caller disconnected")
	}

	// The disconnect reached the upstream connection itself.
	deadline := time.Now().Add(5 * time.Second)
	for aborted.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := aborted.Load(); got != 1 {
		t.Fatalf("the image column's upstream must observe the abort, got %d observations", got)
	}
	// The acknowledgement names the column's audit row; no done is
	// invented for an aborted exchange.
	acks := eventsOfKind(t, w, "cancel_ack")
	if len(acks) != 1 {
		t.Fatalf("expected one cancel_ack for the aborted image column, got %d: %s", len(acks), w.Body.String())
	}
	var ack sseCancelPayload
	parseAs(t, acks[0], &ack)
	if ack.Slot != 0 || ack.RequestID == "" {
		t.Fatalf("cancel_ack must name slot 0 and the audit row's request id: %s", acks[0])
	}
	if got := eventsOfKind(t, w, "done"); len(got) != 0 {
		t.Fatalf("an aborted image column must not report done, got %d: %s", len(got), w.Body.String())
	}
	// The durable record: the abort is in the audit trail as a playground
	// row with the caller-gone status.
	var abortedRow model.RequestLog
	if err := db.Where("api_key_id = ? AND fail_reason = ?", key.ID, "client_disconnected").First(&abortedRow).Error; err != nil {
		t.Fatalf("expected a client_disconnected playground row for the aborted image column: %v", err)
	}
	if abortedRow.StatusCode != 499 || abortedRow.Source != "playground" {
		t.Fatalf("aborted image row = status %d source %q, want 499 playground", abortedRow.StatusCode, abortedRow.Source)
	}
}
