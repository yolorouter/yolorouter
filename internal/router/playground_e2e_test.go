package router

// Full-chain tests for the console Playground run endpoint: a real router
// (session + member scope + handler + relay + capabilities + audit
// recorder) against a loopback fake upstream, the same shape the other
// router-level relay e2e tests use. What is under test is the endpoint's
// contract, not the relay's internals: console auth, key ownership, the
// aggregate event family, the audit row's attribution, and the budget /
// rate-limit guardrails.

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

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// newCapturingUpstream stands in for the upstream model provider: it
// records every request body it receives (so pass-through can be asserted
// against what the upstream actually saw) and answers a fixed
// OpenAI-shaped STREAM whose single content chunk carries "pong" and whose
// trailing usage frame reports 5 input + 3 output tokens — pricing to 11
// micros at the seeded 1.0/2.0 per-million rates. The aggregate channel
// always asks for a stream, so the fake upstream answers one.
func newCapturingUpstream(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	bodies := &[]string{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"gpt-4o-real","choices":[{"index":0,"delta":{"content":"pong"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"c1","model":"gpt-4o-real","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	return upstream, bodies
}

// newQuotaFailingUpstream stands in for an upstream that has run out of
// quota: every request answers the OpenAI-shaped 429 insufficient_quota
// error a real provider returns. A column routed here exercises the
// upstream-failure path — a failure from beyond the admission chain,
// unlike the budget/rate refusals which happen inside it.
func newQuotaFailingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You exceeded your current quota","type":"insufficient_quota","code":"insufficient_quota"}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// newHangingUpstream answers nothing until released: a column routed here
// stays genuinely in flight, so a caller-side context cancel reproduces a
// client disconnect of the aggregate channel mid-run. The cleanup closes
// the release gate so a hung handler can never outlive the test; the
// release channel is closed exactly there and nowhere else.
func newHangingUpstream(t *testing.T) (*httptest.Server, chan struct{}, chan struct{}) {
	t.Helper()
	entered := make(chan struct{}, 16) // buffered: a retry re-entering must not block the upstream
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		upstream.Close()
	})
	return upstream, entered, release
}

// seedPlaygroundUpstreamModel seeds the provider / provider key / model /
// candidate quadruple for an ADDITIONAL upstream — the same shape
// seedRelayableModel plants for the healthy one, with caller-chosen names
// so several upstreams can coexist in one test database.
func seedPlaygroundUpstreamModel(t *testing.T, db *gorm.DB, provider, modelName, providerModelName, baseURL string) *model.Model {
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
		SchedulingMode: model.ModelSchedulingModeFailover,
		CreatedAt:      now, UpdatedAt: now,
	}
	if err := db.Create(m).Error; err != nil {
		t.Fatalf("seed model %s: %v", modelName, err)
	}
	cand := &model.ModelCandidate{
		ModelID: m.ID, ProviderID: p.ID, ProviderModelName: providerModelName,
		InputPrice: 1.0, OutputPrice: 2.0, MaxOutput: 4096,
		ManagementStatus: model.ModelCandidateStatusEnabled, SortOrder: 1,
		VerificationStatus: model.ModelVerificationStatusPassed,
		CreatedAt:          now, UpdatedAt: now,
	}
	if err := db.Create(cand).Error; err != nil {
		t.Fatalf("seed candidate %s: %v", providerModelName, err)
	}
	return m
}

// seedConsoleUser creates an enabled account with a live session and
// returns it.
func seedConsoleUser(t *testing.T, db *gorm.DB, username, role, token string) *model.User {
	t.Helper()
	now := time.Now().UTC()
	u := &model.User{Username: username, Role: role, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, u); err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	if err := repository.CreateSession(db, token, u.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("session %s: %v", username, err)
	}
	return u
}

// seedPlaygroundKey creates a key with a KNOWN plaintext (stored encrypted
// the way real keys are) and returns it together with the plaintext, so a
// test can scan response bytes for exactly the material that must never
// appear. extra mutates the row before insert (limits, status, expiry).
func seedPlaygroundKey(t *testing.T, db *gorm.DB, owner uint, raw string, extra func(*model.APIKey)) (*model.APIKey, string) {
	t.Helper()
	now := time.Now().UTC()
	secrets := crypto.NewSecretBox(testutil.ProviderMasterKey())
	enc, err := secrets.Encrypt(raw)
	if err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	prefix := raw
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	k := &model.APIKey{
		KeyHash: crypto.HashToken(raw), KeyPrefix: prefix, UserID: owner,
		EncryptedKey: enc, Status: model.APIKeyStatusActive, AllowAllModels: true,
		CreatedAt: now, UpdatedAt: now,
	}
	if extra != nil {
		extra(k)
	}
	if err := repository.CreateAPIKey(db, k, nil, now); err != nil {
		t.Fatalf("seed key %s: %v", raw, err)
	}
	return k, raw
}

// postPlaygroundRun drives the endpoint the way the console page will:
// session cookie + JSON body. An empty session sends no cookie.
func postPlaygroundRun(r *gin.Engine, session, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "session_id", Value: session})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// sseEvent is one parsed aggregate-channel event.
type sseEvent struct{ Event, Data string }

// sseEvents parses an aggregate-channel response body into (event, data)
// pairs in order.
func sseEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, block := range strings.Split(body, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var ev, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev != "" {
			out = append(out, sseEvent{ev, data})
		}
	}
	return out
}

// scanResponseBytes returns every byte of a recorder response — header
// values and body — as one string, so a plaintext scan covers the whole
// channel, not just the body.
func scanResponseBytes(w *httptest.ResponseRecorder) string {
	var sb strings.Builder
	for _, values := range w.Header() {
		for _, v := range values {
			sb.WriteString(v)
		}
	}
	sb.WriteString(w.Body.String())
	return sb.String()
}

// assertNoPlaintext fails when needle appears anywhere in the response.
func assertNoPlaintext(t *testing.T, label, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("%s: response bytes contain the key plaintext %q", label, needle)
	}
}

// runBody builds a single-column text run request body.
func runBody(keyID uint, model string) string {
	return fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["%s"],"request":{"messages":[{"role":"user","content":"ping"}]},"params":{"system":"S","temperature":0.7,"max_tokens":128}}`, keyID, model)
}

// TestPlaygroundAuthOwnershipAndKeyState: session gate, ownership gate,
// and key-state gate each answer with their own HTTP status, and none of
// them echo any key material.
func TestPlaygroundAuthOwnershipAndKeyState(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	admin := seedConsoleUser(t, db, "pg-admin", model.RoleAdmin, "tok-pg-admin")
	memberKey, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-member-key-0001", nil)
	adminKey, _ := seedPlaygroundKey(t, db, admin.ID, "sk-yr-pg-admin-key-000002", nil)
	revokedKey, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-revoked-key-0003", func(k *model.APIKey) {
		k.Status = model.APIKeyStatusRevoked
	})
	expiredAt := time.Now().UTC().Add(-time.Minute)
	expiredKey, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-expired-key-0004", func(k *model.APIKey) {
		k.ExpiresAt = &expiredAt
	})

	r := newRelayTestRouter(t, db)

	// No session: the session middleware answers 401 before the handler.
	wNoSession := postPlaygroundRun(r, "", runBody(memberKey.ID, "gpt-4o"))
	if wNoSession.Code != http.StatusUnauthorized {
		t.Fatalf("no session: expected 401, got %d body %s", wNoSession.Code, wNoSession.Body.String())
	}

	// Another account's key: 403, refusing without confirming whether the
	// key id exists at all.
	wForeign := postPlaygroundRun(r, "tok-pg-member", runBody(adminKey.ID, "gpt-4o"))
	if wForeign.Code != http.StatusForbidden {
		t.Fatalf("foreign key: expected 403, got %d body %s", wForeign.Code, wForeign.Body.String())
	}

	// The operator's own revoked key: 401.
	wRevoked := postPlaygroundRun(r, "tok-pg-member", runBody(revokedKey.ID, "gpt-4o"))
	if wRevoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: expected 401, got %d body %s", wRevoked.Code, wRevoked.Body.String())
	}

	// The operator's own expired key: 401 as well.
	wExpired := postPlaygroundRun(r, "tok-pg-member", runBody(expiredKey.ID, "gpt-4o"))
	if wExpired.Code != http.StatusUnauthorized {
		t.Fatalf("expired key: expected 401, got %d body %s", wExpired.Code, wExpired.Body.String())
	}

	// The stored hash is just as secret as the plaintext: every rejection
	// above is scanned against the fingerprint of the very key it refused,
	// so a refusal can never confirm key material by echoing it.
	for _, arm := range []struct {
		label string
		w     *httptest.ResponseRecorder
		hash  string
	}{
		{"no-session rejection", wNoSession, memberKey.KeyHash},
		{"foreign-key rejection", wForeign, adminKey.KeyHash},
		{"revoked-key rejection", wRevoked, revokedKey.KeyHash},
		{"expired-key rejection", wExpired, expiredKey.KeyHash},
	} {
		if strings.Contains(scanResponseBytes(arm.w), arm.hash) {
			t.Errorf("%s: response bytes contain the key's stored hash", arm.label)
		}
	}
}

// TestPlaygroundTextRunRealChain: a single text column runs the real
// chain — the upstream sees the chosen model and exactly the exposed
// parameters (and nothing else), the channel emits the chunk/done pair
// with the audit figures, the audit row carries the playground source,
// and the key's budget accumulator grows by the settled cost.
func TestPlaygroundTextRunRealChain(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, bodies := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, plaintext := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-chain-key-00001", nil)

	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", runBody(key.ID, "gpt-4o"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start (200), got %d body %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("expected an event-stream content type, got %q", ct)
	}

	events := sseEvents(t, w.Body.String())
	if len(events) != 2 {
		t.Fatalf("expected chunk + done events, got %d: %s", len(events), w.Body.String())
	}
	if events[0].Event != "chunk" || events[1].Event != "done" {
		t.Fatalf("expected chunk then done, got %s then %s", events[0].Event, events[1].Event)
	}
	var chunk struct {
		Slot  int    `json:"slot"`
		Model string `json:"model"`
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(events[0].Data), &chunk); err != nil {
		t.Fatalf("chunk event not JSON: %v (%s)", err, events[0].Data)
	}
	if chunk.Slot != 0 || chunk.Model != "gpt-4o" || chunk.Delta != "pong" {
		t.Fatalf("chunk event mismatch: %+v", chunk)
	}
	var done struct {
		Slot      int    `json:"slot"`
		Status    int    `json:"status"`
		RequestID string `json:"request_id"`
		Usage     struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		CostMicros int64 `json:"cost_micros"`
		CostKnown  bool  `json:"cost_known"`
	}
	if err := json.Unmarshal([]byte(events[1].Data), &done); err != nil {
		t.Fatalf("done event not JSON: %v (%s)", err, events[1].Data)
	}
	if done.Status != http.StatusOK || done.RequestID == "" {
		t.Fatalf("done event mismatch: %+v", done)
	}
	// 5 input + 3 output tokens at the seeded 1.0/2.0 per-million rates
	// price to 11 micros — the fake upstream's own accounting.
	if done.Usage.InputTokens != 5 || done.Usage.OutputTokens != 3 {
		t.Fatalf("done usage mismatch: %+v", done.Usage)
	}
	if !done.CostKnown || done.CostMicros != 11 {
		t.Fatalf("done cost mismatch: known=%v micros=%d", done.CostKnown, done.CostMicros)
	}

	// The upstream received the chosen model and every exposed parameter.
	if len(*bodies) != 1 {
		t.Fatalf("expected the upstream to receive exactly one request, got %d", len(*bodies))
	}
	saw := (*bodies)[0]
	for _, want := range []string{
		`"model":"gpt-4o-real"`, // routed to the candidate's provider name
		`"temperature":0.7`,
		`"max_tokens":128`,
		`"role":"system","content":"S"`,
		`"role":"user","content":"ping"`,
	} {
		if !strings.Contains(saw, want) {
			t.Errorf("upstream body missing %q; body: %s", want, saw)
		}
	}
	// Reverse assertion: a parameter the page does not expose must not
	// appear in the upstream body at all.
	for _, banned := range []string{"top_p", "frequency_penalty", "presence_penalty", `"stop"`, `"tools"`, `"n"`} {
		if strings.Contains(saw, banned) {
			t.Errorf("upstream body contains unexposed parameter %q; body: %s", banned, saw)
		}
	}

	// The audit row is attributed to the playground source and carries the
	// same figures the done event reported.
	row, err := repository.GetRequestLogByRequestID(db, done.RequestID)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if row.Source != "playground" {
		t.Fatalf("audit row source = %q, want playground", row.Source)
	}
	if row.StatusCode != http.StatusOK || row.CostMicros != 11 || !row.CostKnown {
		t.Fatalf("audit row mismatch: status=%d cost=%d known=%v", row.StatusCode, row.CostMicros, row.CostKnown)
	}
	if row.InputTokens != 5 || row.OutputTokens != 3 {
		t.Fatalf("audit row token mismatch: %d/%d", row.InputTokens, row.OutputTokens)
	}
	after, err := repository.FindAPIKeyByID(db, key.ID)
	if err != nil {
		t.Fatalf("reload key: %v", err)
	}
	if after.BudgetSpentMicros != 11 {
		t.Fatalf("budget_spent_micros = %d, want 11 (settled cost)", after.BudgetSpentMicros)
	}

	// And none of the run's response bytes leaked the key plaintext.
	assertNoPlaintext(t, "run", scanResponseBytes(w), plaintext)
}

// TestPlaygroundTwoColumnsSlotRouting: the models list runs one column per
// entry, each on its own slot, and a failing column does not take the
// others with it. Columns run concurrently, so events from different slots
// interleave arbitrarily (the 404 for an unknown model answers before the
// healthy column's upstream does); what the channel promises — and what
// this pins — is the per-slot contract: every event names its slot, a
// slot's chunks precede its terminal event, and the failing column's own
// slot carries its native terminal status while the healthy column still
// completes.
func TestPlaygroundTwoColumnsSlotRouting(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-two-col-key-0001", nil)

	body := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["gpt-4o","no-such-model"],"request":{"messages":[{"role":"user","content":"ping"}]}}`, key.ID)
	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}
	events := sseEvents(t, w.Body.String())
	if len(events) != 3 {
		t.Fatalf("expected chunk+done for slot 0 and error for slot 1, got %d events: %s", len(events), w.Body.String())
	}
	// Group by slot, preserving order within each slot.
	bySlot := map[int][]string{}
	for i, ev := range events {
		var payload struct {
			Slot int `json:"slot"`
		}
		if err := json.Unmarshal([]byte(ev.Data), &payload); err != nil {
			t.Fatalf("event %d not JSON: %v", i, err)
		}
		if payload.Slot != 0 && payload.Slot != 1 {
			t.Fatalf("event %d (%s) names slot %d, want 0 or 1", i, ev.Event, payload.Slot)
		}
		bySlot[payload.Slot] = append(bySlot[payload.Slot], ev.Event)
	}
	// Both columns spoke, each exactly its own event family.
	if got := bySlot[0]; len(got) != 2 || got[0] != "chunk" || got[1] != "done" {
		t.Fatalf("slot 0 must be chunk then done (in order), got %v", got)
	}
	if got := bySlot[1]; len(got) != 1 || got[0] != "error" {
		t.Fatalf("slot 1 must be exactly one error, got %v", got)
	}
	// The failing column's terminal event carries its native 404.
	var failed struct {
		Slot   int `json:"slot"`
		Status int `json:"status"`
	}
	for _, ev := range events {
		if ev.Event == "error" {
			if err := json.Unmarshal([]byte(ev.Data), &failed); err != nil {
				t.Fatalf("error event not JSON: %v", err)
			}
		}
	}
	if failed.Slot != 1 || failed.Status != http.StatusNotFound {
		t.Fatalf("second column must be slot 1 with the native 404, got slot %d status %d", failed.Slot, failed.Status)
	}
}

// TestPlaygroundZeroPlaintextAllPaths: zero-plaintext scan across the
// channel's whole response surface — the success path, the console
// rejections, the relay-level budget rejection, the upstream-failure
// column, and the caller disconnect mid-run — headers and bodies alike.
func TestPlaygroundZeroPlaintextAllPaths(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	// A second upstream that refuses everything, and a third that answers
	// nothing until released — the columns routed to them produce the two
	// remaining path families the scan owes: the upstream refusal and the
	// mid-run disconnect.
	seedPlaygroundUpstreamModel(t, db, "pg-quota", "gpt-quota", "gpt-quota-real", newQuotaFailingUpstream(t).URL)
	hangUpstream, hangEntered, _ := newHangingUpstream(t)
	seedPlaygroundUpstreamModel(t, db, "pg-hang", "gpt-hang", "gpt-hang-real", hangUpstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	admin := seedConsoleUser(t, db, "pg-admin", model.RoleAdmin, "tok-pg-admin")
	goodKey, goodPlain := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-zero-good-0001", nil)
	foreignKey, foreignPlain := seedPlaygroundKey(t, db, admin.ID, "sk-yr-pg-zero-admin-0002", nil)
	revokedKey, revokedPlain := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-zero-revok-003", func(k *model.APIKey) {
		k.Status = model.APIKeyStatusRevoked
	})
	// A budget the first run exhausts, so the second run exercises the
	// rejected-relay path on this same channel.
	poorKey, poorPlain := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-zero-poor-0004", func(k *model.APIKey) {
		limit := int64(10)
		k.BudgetLimitMicros = &limit
	})

	r := newRelayTestRouter(t, db)

	responses := []*httptest.ResponseRecorder{
		postPlaygroundRun(r, "tok-pg-member", runBody(goodKey.ID, "gpt-4o")),
		postPlaygroundRun(r, "", runBody(goodKey.ID, "gpt-4o")),
		postPlaygroundRun(r, "tok-pg-member", runBody(foreignKey.ID, "gpt-4o")),
		postPlaygroundRun(r, "tok-pg-member", runBody(revokedKey.ID, "gpt-4o")),
		postPlaygroundRun(r, "tok-pg-member", runBody(poorKey.ID, "gpt-4o")), // passes, exhausts budget
		postPlaygroundRun(r, "tok-pg-member", runBody(poorKey.ID, "gpt-4o")), // rejected by budget
		// The upstream-refusal path: the upstream itself refuses (429 from
		// beyond the admission chain).
		postPlaygroundRun(r, "tok-pg-member", runBody(goodKey.ID, "gpt-quota")),
	}

	// The disconnect path: the caller disconnects while the column is
	// genuinely in flight — the upstream has been entered and answers
	// nothing, the caller's context is cancelled, and the channel carries
	// whatever the aborted run wrote.
	ctx, cancel := context.WithCancel(context.Background())
	discReq := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run", strings.NewReader(runBody(goodKey.ID, "gpt-hang")))
	discReq.Header.Set("Content-Type", "application/json")
	discReq.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	discReq = discReq.WithContext(ctx)
	discW := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		r.ServeHTTP(discW, discReq)
		close(served)
	}()
	<-hangEntered
	cancel()
	<-served
	responses = append(responses, discW)

	for i, w := range responses {
		if w.Code != http.StatusOK && w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Fatalf("path %d: unexpected status %d body %s", i, w.Code, w.Body.String())
		}
	}
	for _, needle := range []string{goodPlain, foreignPlain, revokedPlain, poorPlain} {
		for i, w := range responses {
			assertNoPlaintext(t, fmt.Sprintf("path %d", i), scanResponseBytes(w), needle)
		}
	}
	// The rejected path really was exercised: its row is tagged and its
	// column answered with the native quota error.
	var rejected model.RequestLog
	if err := db.Where("source = ? AND status_code = ?", "playground", http.StatusTooManyRequests).
		Order("id DESC").First(&rejected).Error; err != nil {
		t.Fatalf("expected a tagged 429 playground row: %v", err)
	}
	if !strings.Contains(responses[5].Body.String(), "insufficient_quota") {
		t.Fatalf("expected the native insufficient_quota error on the budget-rejected stream, got: %s", responses[5].Body.String())
	}
	// So were the two added families: the upstream-failure column carries
	// the relay's native verdict for a refusing upstream — a 429 with the
	// rate_limit_error code, distinct from the budget arm's
	// insufficient_quota because this 429 came from beyond the admission
	// chain.
	quotaResp := responses[6].Body.String()
	if !strings.Contains(quotaResp, `"status":429`) || !strings.Contains(quotaResp, `"type":"rate_limit_error"`) {
		t.Fatalf("expected the upstream-refusal verdict (429 rate_limit_error) on the stream, got: %s", quotaResp)
	}
	// The disconnect really propagated through the chain: the aborted
	// column's audit row records the caller going away (499,
	// client_disconnected) and is still attributed to the playground
	// source. The channel's own acknowledgement for an aborted column
	// (cancel_ack, decided by exactly this fail_reason) is pinned by the
	// streaming test's disconnect arm; here the durable evidence is the
	// row.
	var aborted model.RequestLog
	if err := db.Where("api_key_id = ? AND fail_reason = ?", goodKey.ID, "client_disconnected").
		First(&aborted).Error; err != nil {
		t.Fatalf("expected a client_disconnected playground row for the aborted run: %v", err)
	}
	if aborted.StatusCode != 499 || aborted.Source != "playground" {
		t.Fatalf("aborted row = status %d source %q, want 499 playground", aborted.StatusCode, aborted.Source)
	}
}

// TestPlaygroundBudgetAndRPMGuardrails: the guardrails a real caller hits
// are the ones a Playground run hits. Budget: the at-limit run is allowed
// through, the next one is refused with the native 429 insufficient_quota
// shape, and BOTH runs land in the audit trail under the playground
// source. RPM and TPM: a one-request (respectively one-run-of-tokens)
// window refuses the immediate second run the same way, each refusal named
// by its own reason in the audit row — the direct evidence, arm by arm,
// that a Playground run runs the same admission pipeline as an outside
// caller's request.
func TestPlaygroundBudgetAndRPMGuardrails(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")

	// Budget arm: limit 10 micros, a successful run settles 11.
	limit := int64(10)
	budgetKey, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-budget-key-0001", func(k *model.APIKey) {
		k.BudgetLimitMicros = &limit
	})
	r := newRelayTestRouter(t, db)

	first := postPlaygroundRun(r, "tok-pg-member", runBody(budgetKey.ID, "gpt-4o"))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "event: done") {
		t.Fatalf("at-limit run must be allowed through, got %d body %s", first.Code, first.Body.String())
	}
	second := postPlaygroundRun(r, "tok-pg-member", runBody(budgetKey.ID, "gpt-4o"))
	if second.Code != http.StatusOK {
		t.Fatalf("stream must still start for a refused run, got %d", second.Code)
	}
	events := sseEvents(t, second.Body.String())
	if len(events) != 1 || events[0].Event != "error" {
		t.Fatalf("expected a single error event, got %s", second.Body.String())
	}
	var errPayload struct {
		Status int `json:"status"`
		Error  struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[0].Data), &errPayload); err != nil {
		t.Fatalf("error event not JSON: %v", err)
	}
	if errPayload.Status != http.StatusTooManyRequests || errPayload.Error.Type != "insufficient_quota" {
		t.Fatalf("error event = status %d type %q, want 429 insufficient_quota", errPayload.Status, errPayload.Error.Type)
	}

	// Both the successful and the refused run are in the audit trail,
	// tagged as playground runs — the refusal is not invisible just
	// because it came from the console.
	var rows []model.RequestLog
	if err := db.Where("api_key_id = ?", budgetKey.ID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 audit rows for the key, got %d", len(rows))
	}
	if rows[0].Source != "playground" || rows[0].StatusCode != http.StatusOK {
		t.Fatalf("first row must be a tagged 200, got source=%q status=%d", rows[0].Source, rows[0].StatusCode)
	}
	if rows[1].Source != "playground" || rows[1].StatusCode != http.StatusTooManyRequests {
		t.Fatalf("refused row must be a tagged 429, got source=%q status=%d", rows[1].Source, rows[1].StatusCode)
	}

	// RPM arm: a fresh key allowed one request per minute.
	rpm := 1
	rpmKey, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-rpm-key-000002", func(k *model.APIKey) {
		k.RPMLimit = &rpm
	})
	if w := postPlaygroundRun(r, "tok-pg-member", runBody(rpmKey.ID, "gpt-4o")); !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("first RPM run must succeed, got: %s", w.Body.String())
	}
	w := postPlaygroundRun(r, "tok-pg-member", runBody(rpmKey.ID, "gpt-4o"))
	body := w.Body.String()
	if !strings.Contains(body, "event: error") || !strings.Contains(body, `"status":429`) {
		t.Fatalf("second RPM run must be refused with a 429 error event, got: %s", body)
	}
	var rpmRows []model.RequestLog
	if err := db.Where("api_key_id = ?", rpmKey.ID).Order("id ASC").Find(&rpmRows).Error; err != nil {
		t.Fatalf("read rpm audit rows: %v", err)
	}
	if len(rpmRows) != 2 || rpmRows[1].StatusCode != http.StatusTooManyRequests || rpmRows[1].Source != "playground" {
		t.Fatalf("rpm rows mismatch: %+v", rpmRows)
	}
	if rpmRows[1].FailReason == nil || *rpmRows[1].FailReason != "rpm_exceeded" {
		t.Fatalf("rpm refusal must be named rpm_exceeded in the audit row, got %+v", rpmRows[1].FailReason)
	}

	// TPM arm: a fresh key whose token window fits exactly one run — the
	// fake upstream settles 8 tokens (5 prompt + 3 completion), which the
	// admission chain debits on release. The first run passes on an empty
	// window; the second finds it full and is refused with the same native
	// rate-limit shape, its own reason on its audit row. The token-volume
	// face of the same admission chain, asserted directly.
	tpm := 8
	tpmKey, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-tpm-key-000003", func(k *model.APIKey) {
		k.TPMLimit = &tpm
	})
	if w := postPlaygroundRun(r, "tok-pg-member", runBody(tpmKey.ID, "gpt-4o")); !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("first TPM run must succeed (window empty), got: %s", w.Body.String())
	}
	w = postPlaygroundRun(r, "tok-pg-member", runBody(tpmKey.ID, "gpt-4o"))
	tpmEvents := sseEvents(t, w.Body.String())
	if len(tpmEvents) != 1 || tpmEvents[0].Event != "error" {
		t.Fatalf("expected a single error event for the token-window refusal, got %s", w.Body.String())
	}
	var tpmPayload struct {
		Status int `json:"status"`
		Error  struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(tpmEvents[0].Data), &tpmPayload); err != nil {
		t.Fatalf("tpm error event not JSON: %v (%s)", err, tpmEvents[0].Data)
	}
	if tpmPayload.Status != http.StatusTooManyRequests || tpmPayload.Error.Type != "rate_limit_error" {
		t.Fatalf("tpm error event = status %d type %q, want 429 rate_limit_error", tpmPayload.Status, tpmPayload.Error.Type)
	}
	var tpmRows []model.RequestLog
	if err := db.Where("api_key_id = ?", tpmKey.ID).Order("id ASC").Find(&tpmRows).Error; err != nil {
		t.Fatalf("read tpm audit rows: %v", err)
	}
	if len(tpmRows) != 2 || tpmRows[1].StatusCode != http.StatusTooManyRequests || tpmRows[1].Source != "playground" {
		t.Fatalf("tpm rows mismatch: %+v", tpmRows)
	}
	if tpmRows[1].FailReason == nil || *tpmRows[1].FailReason != "tpm_exceeded" {
		t.Fatalf("tpm refusal must be named tpm_exceeded in the audit row, got %+v", tpmRows[1].FailReason)
	}
}
