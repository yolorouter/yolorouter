package router

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/audit"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// These tests drive the reveal endpoint through the REAL router — session
// and role middleware, the audit capture layer, the action dictionary, and
// the production rate window (10/minute per admin account) all exactly as
// serve.go wires them. The handler-package tests cover the window's own
// behavior (small limits, clock advance, concurrency); these pin what only
// the full stack can: permission refusals landing denied rows, the
// three-state audit contract, the error envelope, and the aggregated
// 429 with its audit row.

// seedRevealProvider inserts one provider plus one key row per plaintext,
// each encrypted under the same master key the router's provider service
// uses, and returns the provider id and the key ids in plaintext order.
func seedRevealProvider(t *testing.T, f *auditFixture, plaintexts ...string) (uint, []uint) {
	t.Helper()
	box := crypto.NewSecretBox(testutil.ProviderMasterKey())
	now := time.Now().UTC()
	p := &model.Provider{Name: "reveal-target", ProviderType: "openai", BaseURL: "https://api.example.com/v1",
		ManagementStatus: model.ProviderStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := f.db.Create(p).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	ids := make([]uint, 0, len(plaintexts))
	for i, plaintext := range plaintexts {
		encrypted, err := box.Encrypt(plaintext)
		if err != nil {
			t.Fatalf("encrypt key %d: %v", i, err)
		}
		k := &model.ProviderKey{
			ProviderID: p.ID, Label: fmt.Sprintf("key-%02d", i), EncryptedKey: encrypted,
			KeyPrefix: "sk-reveal-", KeySuffix: provider.KeySuffixFor(plaintext), TestModel: "gpt-4o-mini",
			SortOrder: i + 1, ManagementStatus: model.ProviderKeyStatusEnabled,
			VerificationStatus: model.VerificationStatusUntested, CreatedAt: now, UpdatedAt: now,
		}
		if err := f.db.Create(k).Error; err != nil {
			t.Fatalf("seed key %d: %v", i, err)
		}
		ids = append(ids, k.ID)
	}
	return p.ID, ids
}

// revealPath builds the concrete reveal path for one key.
func revealPath(providerID, keyID uint) string {
	return "/api/admin/providers/" + strconv.FormatUint(uint64(providerID), 10) +
		"/keys/" + strconv.FormatUint(uint64(keyID), 10) + "/reveal"
}

// seedRevealAdmin adds a second enabled admin with a live session, so the
// cross-account rate leg has a genuinely distinct account (not just a
// second session of the same one).
func seedRevealAdmin(t *testing.T, f *auditFixture, username string) *http.Cookie {
	t.Helper()
	now := time.Now().UTC()
	admin := &model.User{Username: username, Role: model.RoleAdmin, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(f.db, admin); err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
	token := "tok-reveal-" + username
	if err := repository.CreateSession(f.db, token, admin.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed %s session: %v", username, err)
	}
	return &http.Cookie{Name: "session_id", Value: token}
}

// TestProviderKeyRevealPermissionAndContract pins the endpoint's access
// contract and response shape: the admin session gets the full plaintext
// exactly once with a no-store header; a member session and an
// unauthenticated probe are both refused through the unified envelope AND
// each refusal still lands its provider_key.reveal row with status denied
// — an attempted unauthorized reveal is itself the signal the audit trail
// exists to keep.
func TestProviderKeyRevealPermissionAndContract(t *testing.T) {
	f := newAuditFixture(t)

	plaintext := "sk-reveal-permission-check-0001"
	providerID, keyIDs := seedRevealProvider(t, f, plaintext)
	path := revealPath(providerID, keyIDs[0])

	// Member leg: seed a real member session.
	now := time.Now().UTC()
	member := &model.User{Username: "revealer", Role: model.RoleMember, Status: model.UserStatusEnabled,
		IsLocal: true, PasswordHash: "hash", CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(f.db, member); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := repository.CreateSession(f.db, "tok-reveal-member", member.ID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed member session: %v", err)
	}
	memberCk := &http.Cookie{Name: "session_id", Value: "tok-reveal-member"}

	// Leg 1: admin — 200, plaintext exactly once, no-store.
	w := f.doBody(t, http.MethodPost, path, "{}", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("admin reveal must succeed, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the plaintext body must not be cacheable", got)
	}
	if n := strings.Count(w.Body.String(), plaintext); n != 1 {
		t.Errorf("body must carry the plaintext exactly once, found %d occurrences: %s", n, w.Body.String())
	}

	// Leg 2: member — refused by the admin gate, envelope shape.
	w = f.doBody(t, http.MethodPost, path, "{}", memberCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member reveal must be refused 403, got %d: %s", w.Code, w.Body.String())
	}

	// Leg 3: no session — refused before any identity exists.
	w = f.doBody(t, http.MethodPost, path, "{}", nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("sessionless reveal must be refused 401, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 3 {
		t.Fatalf("three reveal attempts (one success, one member denial, one sessionless denial) must land 3 rows, got %d", len(rows))
	}
	byStatus := map[string]model.AdminAuditLog{}
	for _, row := range rows {
		if row.Action != audit.ActionProviderKeyReveal {
			t.Errorf("action = %q, want %q on every outcome — the dictionary entry, not a fallback name", row.Action, audit.ActionProviderKeyReveal)
		}
		byStatus[row.Status] = row
	}
	if len(byStatus) != 2 {
		t.Fatalf("expected one success row and two denied rows, got states %v", byStatus)
	}
	ok, has := byStatus[model.AuditStatusSuccess]
	if !has {
		t.Fatalf("no success row; states present: %v", byStatus)
	}
	if ok.AdminUserID == nil || *ok.AdminUserID != f.adminID || ok.AdminUsername != "boss" {
		t.Errorf("success row identity = id:%v name:%q, want the acting admin %d boss", ok.AdminUserID, ok.AdminUsername, f.adminID)
	}
	if want := `{"key_suffix":"0001"}`; ok.Detail != want {
		t.Errorf("success detail = %s, want %s (display suffix only)", ok.Detail, want)
	}
	if want := strconv.FormatUint(uint64(keyIDs[0]), 10); ok.ResourceID != want {
		t.Errorf("success resource_id = %q, want the revealed key id %s", ok.ResourceID, want)
	}
	if ok.ResourceType != "provider_key" {
		t.Errorf("success resource type = %q, want provider_key", ok.ResourceType)
	}
	denied, has := byStatus[model.AuditStatusDenied]
	if !has {
		t.Fatalf("no denied row; states present: %v", byStatus)
	}
	if denied.ErrorBrief != "" {
		t.Errorf("a denial is not an error, error_brief = %q", denied.ErrorBrief)
	}
	// The two denials differ by identity: the member refusal snapshots the
	// refusing account, the sessionless one carries a NULL id. Pin both
	// shapes instead of letting a map overwrite one with the other.
	var memberDenied, sessionless *model.AdminAuditLog
	for i := range rows {
		if rows[i].Status != model.AuditStatusDenied {
			continue
		}
		if rows[i].AdminUserID != nil {
			memberDenied = &rows[i]
		} else {
			sessionless = &rows[i]
		}
	}
	if memberDenied == nil {
		t.Fatal("the member refusal must land a denied row carrying the refusing member's identity")
	}
	if *memberDenied.AdminUserID != member.ID || memberDenied.AdminUsername != "revealer" {
		t.Errorf("member-denied row identity = id:%v name:%q, want member %d revealer", memberDenied.AdminUserID, memberDenied.AdminUsername, member.ID)
	}
	if sessionless == nil {
		t.Error("the sessionless refusal must land a row with a NULL admin_user_id")
	}
	// Zero-plaintext reverse assertion across every row the test produced.
	assertRowsCarryNoPlaintext(t, rows, plaintext)
}

// TestProviderKeyRevealAuditThreeStates pins the audit contract's error
// half end to end: a decryption failure and a missing key both land
// status=error rows under the reveal action, with an error_brief built
// from the status code alone and the plaintext (decrypted or planted)
// never reaching any stored column. The missing-key leg also pins the
// response envelope: the family's own business code and message, nothing
// that distinguishes a wrong provider from a wrong key id.
func TestProviderKeyRevealAuditThreeStates(t *testing.T) {
	f := newAuditFixture(t)

	healthy := "sk-reveal-healthy-key-0001"
	doomed := "sk-reveal-corrupt-cipher-0002"
	providerID, keyIDs := seedRevealProvider(t, f, healthy, doomed)

	// Success state first (the row's full field shape).
	w := f.doBody(t, http.MethodPost, revealPath(providerID, keyIDs[0]), "{}", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("healthy reveal must succeed, got %d: %s", w.Code, w.Body.String())
	}

	// Decryption failure: corrupt the stored ciphertext so the decrypt
	// genuinely fails inside the service.
	if err := f.db.Model(&model.ProviderKey{}).Where("id = ?", keyIDs[1]).
		Update("encrypted_key", "bogus-not-a-cipher").Error; err != nil {
		t.Fatalf("corrupt ciphertext: %v", err)
	}
	w = f.doBody(t, http.MethodPost, revealPath(providerID, keyIDs[1]), "{}", f.adminCk)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("a failed decrypt must answer the generic 500, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), doomed) || strings.Contains(w.Body.String(), "decrypt") {
		t.Errorf("the 500 envelope must carry only the fixed internal-error text, got %s", w.Body.String())
	}

	// Missing key: the family's not-found envelope (business code +
	// message, the same answer a key under a DIFFERENT provider gets).
	w = f.doBody(t, http.MethodPost, revealPath(providerID, 9999), "{}", f.adminCk)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing-key reveal must answer the family's envelope, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"code":12009`) ||
		!strings.Contains(w.Body.String(), "provider key not found") {
		t.Errorf("missing-key body must carry business code 12009 and its registered message, got %s", w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 3 {
		t.Fatalf("one success + one decrypt failure + one missing key must land 3 rows, got %d", len(rows))
	}
	states := map[string]int{}
	for _, row := range rows {
		if row.Action != audit.ActionProviderKeyReveal {
			t.Errorf("action = %q, want %q on every outcome", row.Action, audit.ActionProviderKeyReveal)
		}
		states[row.Status]++
		if row.Status == model.AuditStatusError && row.ErrorBrief == "" {
			t.Error("error rows must carry a non-empty error_brief")
		}
	}
	if states[model.AuditStatusSuccess] != 1 || states[model.AuditStatusError] != 2 {
		t.Fatalf("expected 1 success + 2 error rows, got %v", states)
	}
	// The decrypted success plaintext and the never-decrypted doomed
	// plaintext must both be absent from every stored column.
	assertRowsCarryNoPlaintext(t, rows, healthy, doomed)
}

// TestProviderKeyRevealLongKeyComplete pins the large-key boundary: a
// 200-character plaintext returns complete and untruncated in the response
// body, while the audit row still names it by the four-character display
// suffix alone.
func TestProviderKeyRevealLongKeyComplete(t *testing.T) {
	f := newAuditFixture(t)

	var b strings.Builder
	for i := 0; b.Len() < 200; i++ {
		fmt.Fprintf(&b, "%02d", i%100)
	}
	longPlaintext := "sk-reveal-" + b.String()[:200-len("sk-reveal-")]
	if len(longPlaintext) != 200 {
		t.Fatalf("fixture builds a 200-char key, got %d", len(longPlaintext))
	}
	suffix := longPlaintext[len(longPlaintext)-4:]

	providerID, keyIDs := seedRevealProvider(t, f, longPlaintext)
	w := f.doBody(t, http.MethodPost, revealPath(providerID, keyIDs[0]), "{}", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("long-key reveal must succeed, got %d: %s", w.Code, w.Body.String())
	}
	if n := strings.Count(w.Body.String(), longPlaintext); n != 1 {
		t.Errorf("body must carry the 200-char plaintext complete and exactly once, found %d occurrences (len %d)", n, w.Body.Len())
	}

	rows := f.drain(t)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 row, got %d", len(rows))
	}
	if want := `{"key_suffix":"` + suffix + `"}`; rows[0].Detail != want {
		t.Errorf("detail = %s, want %s — four characters, never the whole key", rows[0].Detail, want)
	}
	// Everything except the suffix must stay out of the row: assert the
	// first 190 characters (far more than the suffix overlaps) are absent.
	assertRowsCarryNoPlaintext(t, rows, longPlaintext[:190])
}

// TestProviderKeyRevealRateLimitAggregatedAndAudited pins the production
// window through the real router: 10 reveals fit, the 11th is refused 429
// EVEN THOUGH it targets a key id the budget has never counted (the
// enumeration shape — the budget aggregates per admin account, not per
// key), the refusal lands its own error row, and a second admin account
// still sails through on the first one's spent window.
func TestProviderKeyRevealRateLimitAggregatedAndAudited(t *testing.T) {
	f := newAuditFixture(t)

	plaintexts := make([]string, 11)
	for i := range plaintexts {
		plaintexts[i] = fmt.Sprintf("sk-reveal-rate-%02d-abcdefgh", i)
	}
	providerID, keyIDs := seedRevealProvider(t, f, plaintexts...)

	for i := 0; i < 10; i++ {
		w := f.doBody(t, http.MethodPost, revealPath(providerID, keyIDs[i]), "{}", f.adminCk)
		if w.Code != http.StatusOK {
			t.Fatalf("reveal %d within the 10/minute budget must succeed, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}
	// The 11th probe: a DIFFERENT key id (never counted before) — the
	// aggregate per-account window must refuse it anyway.
	w := f.doBody(t, http.MethodPost, revealPath(providerID, keyIDs[10]), "{}", f.adminCk)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("the 11th reveal in one minute must be refused 429, got %d: %s", w.Code, w.Body.String())
	}

	// Cross-account: a second admin on the same spent minute still fits.
	admin2Ck := seedRevealAdmin(t, f, "rate-admin-two")
	w = f.doBody(t, http.MethodPost, revealPath(providerID, keyIDs[10]), "{}", admin2Ck)
	if w.Code != http.StatusOK {
		t.Fatalf("a second admin must not inherit the first one's spent window, got %d: %s", w.Code, w.Body.String())
	}

	rows := f.drain(t)
	if len(rows) != 12 {
		t.Fatalf("10 successes + 1 refused + 1 second-admin success must land 12 rows, got %d", len(rows))
	}
	throttled := 0
	for _, row := range rows {
		if row.Status != model.AuditStatusError {
			continue
		}
		throttled++
		if row.ErrorBrief != "http_status=429" {
			t.Errorf("throttled row error_brief = %q, want http_status=429", row.ErrorBrief)
		}
		if row.Action != audit.ActionProviderKeyReveal {
			t.Errorf("throttled row action = %q, want %q", row.Action, audit.ActionProviderKeyReveal)
		}
	}
	if throttled != 1 {
		t.Fatalf("exactly one throttled row must land, got %d", throttled)
	}
	assertRowsCarryNoPlaintext(t, rows, plaintexts...)
}
