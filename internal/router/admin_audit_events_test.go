package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/audit"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/apikey"
	"github.com/yolorouter/yolorouter/internal/service/auth"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// doBody fires one request with an explicit body and cookie (nil cookie =
// none — the shape the public auth endpoints see). The sweep fixture's own
// do/doWith always send an empty JSON object; the audit-event cases here
// carry real payloads whose plaintext must never reach the audit table.
func (f *auditFixture) doBody(t *testing.T, method, path, body string, ck *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ck != nil {
		req.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	f.r.ServeHTTP(w, req)
	return w
}

// serializeRow renders one row the way a leak would travel: every stored
// column in one document, so the zero-plaintext assertions scan everything
// the pipeline persisted, not just the detail column.
func serializeRow(t *testing.T, row model.AdminAuditLog) string {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	return string(raw)
}

// assertRowsCarryNoPlaintext is the zero-plaintext reverse assertion the
// redaction contract demands, applied to whole rows: none of the planted
// secrets may appear anywhere in any persisted column.
func assertRowsCarryNoPlaintext(t *testing.T, rows []model.AdminAuditLog, secrets ...string) {
	t.Helper()
	for _, row := range rows {
		doc := serializeRow(t, row)
		for _, secret := range secrets {
			if secret != "" && strings.Contains(doc, secret) {
				t.Errorf("plaintext %q leaked into audit row (action %s):\n%s", secret, row.Action, doc)
			}
		}
	}
}

// seedLocalAccount creates an enabled local account with a real bcrypt
// password — the login-flow cases need the genuine hash round-trip, not
// the fixture admin's placeholder "hash".
func seedLocalAccount(t *testing.T, f *auditFixture, username, password string) (uint, string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password for %s: %v", username, err)
	}
	now := time.Now().UTC()
	user := &model.User{Username: username, Role: model.RoleMember, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(f.db, user); err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
	return user.ID, password
}

// TestAdminAuditLoginThreeBranches is the login-event encoding contract:
// success lands the route's own action with status success, wrong
// credentials land auth.login_denied, and the lockout threshold's final
// refusal lands auth.login_locked — both fine-grained events with the
// status pinned to denied and the attempted username (never the password,
// never an exists/doesn't-exist distinction) in the detail.
func TestAdminAuditLoginThreeBranches(t *testing.T) {
	f := newAuditFixture(t)

	davePass := "CorrectHorse99!"
	seedLocalAccount(t, f, "dave", davePass)
	erinPass := "RealPass1234!"
	seedLocalAccount(t, f, "erin", erinPass)
	wrongPass := "WrongPass0000!"

	login := func(username, password string) int {
		body := `{"username":"` + username + `","password":"` + password + `"}` //nolint:gosec // test fixture value, not a credential
		w := f.doBody(t, http.MethodPost, "/api/admin/auth/login", body, nil)
		return w.Code
	}

	// Branch 1: correct credentials.
	if code := login("dave", davePass); code != http.StatusOK {
		t.Fatalf("correct-password login must succeed, got %d", code)
	}
	// Branch 2: wrong credentials, below the lockout threshold.
	for i := 0; i < 4; i++ {
		if code := login("erin", wrongPass); code != http.StatusUnauthorized {
			t.Fatalf("wrong-password attempt %d must answer 401, got %d", i+1, code)
		}
	}
	// Branch 3: the threshold-crossing failure locks the account.
	if code := login("erin", wrongPass); code != http.StatusForbidden {
		t.Fatalf("threshold-crossing failure must answer 403 (locked), got %d", code)
	}

	rows := f.drain(t)
	if len(rows) != 6 {
		t.Fatalf("six logins must produce six audit rows, got %d", len(rows))
	}
	counts := map[string]int{}
	states := map[string]string{}
	details := map[string]string{}
	identities := map[string]*uint{}
	for _, row := range rows {
		counts[row.Action]++
		states[row.Action] = row.Status
		details[row.Action] = row.Detail
		identities[row.Action] = row.AdminUserID
	}
	if counts[audit.ActionLogin] != 1 || states[audit.ActionLogin] != model.AuditStatusSuccess {
		t.Errorf("success branch = %d rows of %q, want exactly 1 success", counts[audit.ActionLogin], states[audit.ActionLogin])
	}
	if counts[audit.ActionLoginDenied] != 4 || states[audit.ActionLoginDenied] != model.AuditStatusDenied {
		t.Errorf("wrong-credential branch = %d rows of %q, want 4 denied", counts[audit.ActionLoginDenied], states[audit.ActionLoginDenied])
	}
	if counts[audit.ActionLoginLocked] != 1 || states[audit.ActionLoginLocked] != model.AuditStatusDenied {
		t.Errorf("lockout branch = %d rows of %q, want exactly 1 denied", counts[audit.ActionLoginLocked], states[audit.ActionLoginLocked])
	}
	// Public endpoint: no session exists while it runs, so the rows carry
	// no resolved identity — the identity of a login event is the outcome
	// itself, and the attempted username rides the detail instead.
	for _, action := range []string{audit.ActionLogin, audit.ActionLoginDenied, audit.ActionLoginLocked} {
		if identities[action] != nil {
			t.Errorf("%s row must carry a NULL admin_user_id (no session yet), got %d", action, *identities[action])
		}
	}
	if want := `{"username":"erin"}`; details[audit.ActionLoginDenied] != want || details[audit.ActionLoginLocked] != want {
		t.Errorf("denied/locked detail = %q / %q, want %q (attempted username, nothing else)",
			details[audit.ActionLoginDenied], details[audit.ActionLoginLocked], want)
	}
	if details[audit.ActionLogin] != "" {
		t.Errorf("success row needs no detail, got %q", details[audit.ActionLogin])
	}
	// Zero-plaintext reverse assertion: every password that crossed the
	// wire must be absent from every stored column.
	assertRowsCarryNoPlaintext(t, rows, davePass, erinPass, wrongPass)
}

// TestAdminAuditSensitiveReadRevealIsAuditedAndOtherReadsSilent pins the
// sensitive-read boundary as an enumeration, not a promise: the
// one enumerated sensitive read — GET /api-keys/:id/plaintext — must land
// its api_key.reveal row, while EVERY other admin GET, enumerated from
// the live router so a future GET cannot ship outside the assertion,
// adds nothing. The reveal runs against a real key created through the
// same service and master key the router wires, so the decryption
// genuinely succeeds and the event genuinely parks; the admin session is
// unscoped, exactly the reach-into-any-key case the design names as the
// subject worth remembering.
func TestAdminAuditSensitiveReadRevealIsAuditedAndOtherReadsSilent(t *testing.T) {
	f := newAuditFixture(t)

	// A caller key created through the service the router itself wires
	// (same master key), so the reveal decrypts for real.
	keySvc := apikey.NewAPIKeyService(f.db, crypto.NewSecretBox(testutil.ProviderMasterKey()))
	created, err := keySvc.CreateAPIKey(apikey.CreateAPIKeyInput{UserID: f.adminID, AllowAllModels: true}, time.Now().UTC())
	if err != nil {
		t.Fatalf("seed caller key: %v", err)
	}
	idStr := strconv.FormatUint(uint64(created.APIKey.ID), 10)
	revealKey := "GET /api/admin/api-keys/:id/plaintext"

	// Enumerate the live admin GET surface; the reveal route must be on
	// it — a wiring regression that drops the route fails loudly here
	// instead of quietly passing the zero-noise half of the test.
	liveReads := map[string]bool{}
	for _, ri := range f.r.Routes() {
		if ri.Method != http.MethodGet || !strings.HasPrefix(ri.Path, "/api/admin") {
			continue
		}
		liveReads[ri.Method+" "+ri.Path] = true
	}
	if !liveReads[revealKey] {
		t.Fatalf("the audited sensitive read %s is not registered on the live router", revealKey)
	}
	if len(liveReads) < 2 {
		t.Fatalf("expected a realistic admin GET surface to sweep, got %d routes", len(liveReads))
	}

	// Fire every admin GET with the admin session. Path parameters get
	// concrete values — :id resolves to the seeded key so the reveal
	// succeeds; the others just need to route.
	revealHeader := ""
	for key := range liveReads {
		_, pattern := splitRouteKey(key)
		concrete := strings.NewReplacer(":requestId", "req-tc11", ":sessionId", "sess-tc11", ":id", idStr).Replace(pattern)
		w := f.doWith(t, http.MethodGet, concrete, f.adminCk)
		if key == revealKey {
			if w.Code != http.StatusOK {
				t.Fatalf("the reveal must succeed against the seeded key, got %d: %s", w.Code, w.Body.String())
			}
			revealHeader = w.Header().Get("X-Request-Id")
		}
	}
	if revealHeader == "" {
		t.Fatal("the reveal response carried no X-Request-Id")
	}

	// Positive half: every fired GET together leaves exactly one row, and
	// it is the reveal's — reconciled by request id, naming the revealed
	// key, carrying the acting admin's identity snapshot.
	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("%d admin GETs (incl. the reveal) must leave exactly 1 audit row — the reveal alone — got %d", len(liveReads), len(rows))
	}
	row := rows[0]
	if row.Action != audit.ActionAPIKeyReveal {
		t.Errorf("action = %q, want %q", row.Action, audit.ActionAPIKeyReveal)
	}
	if row.ResourceType != "api_key" {
		t.Errorf("resource type = %q, want api_key", row.ResourceType)
	}
	if row.ResourceID != idStr {
		t.Errorf("resource id = %q, want the revealed key %s", row.ResourceID, idStr)
	}
	if row.Status != model.AuditStatusSuccess {
		t.Errorf("status = %q, want success", row.Status)
	}
	if row.RequestID != revealHeader {
		t.Errorf("request id = %q, want the reveal response's %q", row.RequestID, revealHeader)
	}
	if row.AdminUserID == nil || *row.AdminUserID != f.adminID || row.AdminUsername != "boss" {
		t.Errorf("identity = id:%v name:%q, want the acting admin %d boss", row.AdminUserID, row.AdminUsername, f.adminID)
	}
	// Negative half, restated: the count above is the zero-noise
	// assertion — every GET that is not the reveal produced no row. And
	// the plaintext that crossed the wire never reached any stored
	// column: the reveal row names the key, it does not quote it.
	assertRowsCarryNoPlaintext(t, rows, created.PlaintextKey)
}

// TestAdminAuditMemberDeniedOnAdminWrite is the denied contract: a member
// session refused by RequireAdmin still lands exactly one row — same
// action as an authorized attempt would have produced, status denied, the
// member's identity snapshotted. The refusal is the signal; the audit
// trail must not go quiet exactly when someone tried something they may
// not do.
func TestAdminAuditMemberDeniedOnAdminWrite(t *testing.T) {
	f := newAuditFixture(t)

	now := time.Now().UTC()
	hash, err := auth.HashPassword("MemberPass123!")
	if err != nil {
		t.Fatalf("hash member password: %v", err)
	}
	member := &model.User{Username: "mallory", Role: model.RoleMember, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(f.db, member); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := repository.CreateSession(f.db, "tok-audit-denied", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed member session: %v", err)
	}
	memberCk := &http.Cookie{Name: "session_id", Value: "tok-audit-denied"}

	plantedKey := "sk-denied-leak-check-000"
	w := f.doBody(t, http.MethodPatch, "/api/admin/providers/1",
		`{"name":"x","api_key":"`+plantedKey+`"}`, memberCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member writing an admin endpoint must be refused 403, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("the refused write must still land exactly one row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "provider.update" {
		t.Errorf("action = %q, want the unchanged route action provider.update", row.Action)
	}
	if row.Status != model.AuditStatusDenied {
		t.Errorf("status = %q, want denied", row.Status)
	}
	if row.AdminUserID == nil || *row.AdminUserID != member.ID {
		t.Errorf("row must carry the refusing member's id %d, got %v", member.ID, row.AdminUserID)
	}
	if row.AdminUsername != "mallory" {
		t.Errorf("username snapshot = %q, want mallory", row.AdminUsername)
	}
	if row.ErrorBrief != "" {
		t.Errorf("a denial is not an error, error_brief = %q", row.ErrorBrief)
	}
	// The capture layer never reads request bodies: the key the refused
	// request carried must not appear anywhere in the row.
	assertRowsCarryNoPlaintext(t, rows, plantedKey)
}

// TestAdminAuditHandlerErrorRow is the error contract: a write whose
// handler fails mid-execution lands status error with an error_brief built
// from the status code alone — present, non-empty, and provably free of
// the request's sensitive payload. The failure is induced by dropping the
// table the handler depends on, so the blast radius is one handler while
// the session and audit tables (which the row's own persistence needs)
// stay alive.
func TestAdminAuditHandlerErrorRow(t *testing.T) {
	f := newAuditFixture(t)
	if err := f.db.Exec("DROP TABLE oauth_providers").Error; err != nil {
		t.Fatalf("drop oauth_providers to force the handler failure: %v", err)
	}

	plantedSecret := "sk-error-leak-check-999"
	w := f.doBody(t, http.MethodPatch, "/api/admin/oauth-providers/1",
		`{"name":"NewName","client_secret":"`+plantedSecret+`"}`, f.adminCk)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("handler against a missing table must fail 500, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("the failed write must still land exactly one row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != "oauth_provider.update" {
		t.Errorf("action = %q, want oauth_provider.update", row.Action)
	}
	if row.Status != model.AuditStatusError {
		t.Errorf("status = %q, want error", row.Status)
	}
	if row.ErrorBrief == "" {
		t.Error("error rows must carry a non-empty error_brief")
	}
	// The attempted change is still audited — redacted.
	if want := `{"client_secret":"***","name":"NewName"}`; row.Detail != want {
		t.Errorf("detail = %s, want %s (changed fields, secret redacted)", row.Detail, want)
	}
	assertRowsCarryNoPlaintext(t, rows, plantedSecret)
}

// TestAdminAuditOAuthClientSecretRedacted is the OAuth provider config's
// redaction-focused walk: create, update, and delete each land their own
// row; the client_secret's PRESENCE is audited (field name plus inert
// placeholder) while its value — create-time and rotated alike — never
// reaches any column; the update's detail carries exactly the patched
// fields, not the whole config.
func TestAdminAuditOAuthClientSecretRedacted(t *testing.T) {
	f := newAuditFixture(t)

	createSecret := "super-secret-abc-123"
	rotateSecret := "rotated-secret-xyz-789"
	createBody := `{"slug":"github","name":"GitHub","client_id":"cid-1","client_secret":"` + createSecret + `",` +
		`"authorization_endpoint":"https://idp.example/authorize","token_endpoint":"https://idp.example/token",` +
		`"userinfo_endpoint":"https://idp.example/userinfo"}`
	w := f.doBody(t, http.MethodPost, "/api/admin/oauth-providers", createBody, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("provider create must succeed, got %d: %s", w.Code, w.Body.String())
	}
	w = f.doBody(t, http.MethodPatch, "/api/admin/oauth-providers/1",
		`{"name":"GitHub 2","client_secret":"`+rotateSecret+`"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("provider update must succeed, got %d: %s", w.Code, w.Body.String())
	}
	w = f.doBody(t, http.MethodDelete, "/api/admin/oauth-providers/1", "{}", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("provider delete must succeed, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 3 {
		t.Fatalf("create+update+delete must land 3 rows, got %d", len(rows))
	}
	byAction := map[string]model.AdminAuditLog{}
	for _, row := range rows {
		byAction[row.Action] = row
		if row.Status != model.AuditStatusSuccess {
			t.Errorf("%s status = %q, want success", row.Action, row.Status)
		}
	}
	create, ok := byAction["oauth_provider.create"]
	if !ok {
		t.Fatalf("no oauth_provider.create row; have %v", byAction)
	}
	// Positive shape: the field name and placeholder survive — the row
	// says a secret was set, never what it was.
	if want := `{"client_secret":"***","slug":"github"}`; create.Detail != want {
		t.Errorf("create detail = %s, want %s", create.Detail, want)
	}
	update, ok := byAction["oauth_provider.update"]
	if !ok {
		t.Fatalf("no oauth_provider.update row; have %v", byAction)
	}
	if want := `{"client_secret":"***","name":"GitHub 2"}`; update.Detail != want {
		t.Errorf("update detail = %s, want %s", update.Detail, want)
	}
	// Changed fields only: the untouched config (endpoints, client_id)
	// must not bleed into a sparse PATCH's detail.
	for _, absent := range []string{"authorization_endpoint", "token_endpoint", "userinfo_endpoint", "client_id"} {
		if strings.Contains(update.Detail, absent) {
			t.Errorf("update detail carries untouched field %s — changed fields only: %s", absent, update.Detail)
		}
	}
	if _, ok := byAction["oauth_provider.delete"]; !ok {
		t.Fatalf("no oauth_provider.delete row; have %v", byAction)
	}
	// Zero-plaintext reverse assertion across all three rows, both secrets.
	assertRowsCarryNoPlaintext(t, rows, createSecret, rotateSecret)
}
