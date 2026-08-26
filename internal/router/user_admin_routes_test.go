package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// TestDisableCascadesToKeysAndSessions walks the acceptance chain over
// real HTTP: a member's gateway key works, the admin disables the
// account, the key answers 401 on the very next request and the member's
// admin session dies; re-enabling restores the key with no repair step.
func TestDisableCascadesToKeysAndSessions(t *testing.T) {
	f := newMemberScopeFixture(t)

	// The fixture stores hashes; mint a presentable raw credential for
	// alice by inserting one more key whose hash we control.
	const rawKey = "sk-yr-alice-live-credential"
	now := time.Now().UTC()
	k := &model.APIKey{KeyHash: crypto.HashToken(rawKey), KeyPrefix: rawKey[:8], UserID: f.aliceID,
		Status: model.APIKeyStatusActive, AllowAllModels: true, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAPIKey(f.db, k, nil, now); err != nil {
		t.Fatalf("seed raw key: %v", err)
	}
	gatewayModels := func() (int, string) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+rawKey)
		w := httptest.NewRecorder()
		f.r.ServeHTTP(w, req)
		return w.Code, w.Body.String()
	}

	if code, body := gatewayModels(); code != http.StatusOK {
		t.Fatalf("enabled owner: expected 200 from /v1/models, got %d %s", code, body)
	}

	// Admin disables alice.
	w := f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(f.aliceID)+"/status", `{"status":"disabled"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("disable alice: %d %s", w.Code, w.Body.String())
	}

	// Her key dies immediately, with the disabled-account message.
	if code, body := gatewayModels(); code != http.StatusUnauthorized || !strings.Contains(body, "account disabled") {
		t.Fatalf("disabled owner: expected 401 account disabled, got %d %s", code, body)
	}
	// The rejection's audit row stays attributed to both the key AND the
	// owning account — account-scoped log views filter on
	// request_logs.user_id, and an unattributed 401 would vanish from them.
	var auditRow model.RequestLog
	if err := f.db.Where("status_code = 401 AND api_key_id = ?", k.ID).
		Order("id DESC").First(&auditRow).Error; err != nil {
		t.Fatalf("load disabled-owner audit row: %v", err)
	}
	if auditRow.UserID == nil || *auditRow.UserID != f.aliceID {
		t.Fatalf("disabled-owner audit row must carry the owner's user_id, got %+v", auditRow.UserID)
	}
	// Her admin session is gone too — not merely rejected, deleted.
	w = f.do(t, http.MethodGet, "/api/admin/auth/me", "", f.aliceCk)
	if w.Code == http.StatusOK {
		t.Fatalf("disabled member session must not answer 200, got %s", w.Body.String())
	}

	// Re-enable: the same key works again without touching key rows.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(f.aliceID)+"/status", `{"status":"enabled"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enable alice: %d %s", w.Code, w.Body.String())
	}
	if code, body := gatewayModels(); code != http.StatusOK {
		t.Fatalf("re-enabled owner: expected 200, got %d %s", code, body)
	}
}

// TestUserAdminEndpointGuards: the self-operation refusal surfaces as a
// 409 with its own code; member sessions cannot reach the endpoints at
// all; role changes flow through and take effect on the next request
// (RequireSession re-reads the role per request).
func TestUserAdminEndpointGuards(t *testing.T) {
	f := newMemberScopeFixture(t)

	// Admin acting on their own account: refused.
	var adminID uint
	w := f.do(t, http.MethodGet, "/api/admin/users", "", f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("list users: %d", w.Code)
	}
	var env struct {
		Data struct {
			Users []struct {
				ID       uint   `json:"id"`
				Username string `json:"username"`
			} `json:"users"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, u := range env.Data.Users {
		if u.Username == "boss" {
			adminID = u.ID
		}
	}
	if adminID == 0 {
		t.Fatalf("admin missing from directory")
	}
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(adminID)+"/status", `{"status":"disabled"}`, f.adminCk)
	if w.Code != http.StatusConflict {
		t.Fatalf("self-disable over http: expected 409, got %d %s", w.Code, w.Body.String())
	}
	var errEnv struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errEnv.Code != errcode.AccountSelfOperation {
		t.Fatalf("self-disable code: expected %d, got %d", errcode.AccountSelfOperation, errEnv.Code)
	}

	// Members cannot reach the management endpoints.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(adminID)+"/status", `{"status":"disabled"}`, f.aliceCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member reaching user management: expected 403, got %d", w.Code)
	}

	// Promote alice to admin. Her existing session dies with the role
	// change (a live SPA only learns its role at login, so the stale
	// session would keep her locked in the member shell); after a fresh
	// login the admin surface opens up.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(f.aliceID)+"/role", `{"role":"admin"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("promote alice: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", f.aliceCk)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("promoted alice's old session: expected 401 (forced re-login), got %d", w.Code)
	}
	now := time.Now().UTC()
	if err := repository.CreateSession(f.db, "tok-alice-admin", f.aliceID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("relogin alice: %v", err)
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", &http.Cookie{Name: "session_id", Value: "tok-alice-admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("re-logged-in admin alice: expected 200, got %d", w.Code)
	}

	// Invalid payloads are 400s, not silent no-ops.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(f.aliceID)+"/role", `{"role":"superuser"}`, f.adminCk)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid role: expected 400, got %d", w.Code)
	}
}

// TestCreateUserProvisionsPasswordLogin walks the console-created account
// over real HTTP: the admin provisions a local member, the member signs
// in through the ordinary password form, and the endpoint's guards hold
// (member sessions cannot provision; duplicate names collide with the
// username-taken code; weak passwords die in binding).
func TestCreateUserProvisionsPasswordLogin(t *testing.T) {
	f := newMemberScopeFixture(t)

	w := f.do(t, http.MethodPost, "/api/admin/users",
		`{"username":"carol","display_name":"Carol","password":"correct-horse-1"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("create carol: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			User struct {
				Username    string `json:"username"`
				Role        string `json:"role"`
				IsLocal     bool   `json:"is_local"`
				IsBootstrap bool   `json:"is_bootstrap"`
			} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Data.User.Username != "carol" || env.Data.User.Role != model.RoleMember ||
		!env.Data.User.IsLocal || env.Data.User.IsBootstrap {
		t.Fatalf("created view has wrong shape: %+v", env.Data.User)
	}
	if strings.Contains(w.Body.String(), "correct-horse-1") {
		t.Fatalf("response must not echo the password")
	}

	// The created member signs in through the ordinary password form.
	w = f.do(t, http.MethodPost, "/api/admin/auth/login", `{"username":"carol","password":"correct-horse-1"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("carol password login: %d %s", w.Code, w.Body.String())
	}

	// Disabling the console-created member is allowed (no bootstrap
	// protection) and rejects her password login immediately; re-enable
	// restores the account for the assertions below.
	var carolID uint
	if err := f.db.Raw("SELECT id FROM users WHERE username = ?", "carol").Scan(&carolID).Error; err != nil || carolID == 0 {
		t.Fatalf("load carol id: %v %d", err, carolID)
	}
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(carolID)+"/status", `{"status":"disabled"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("disable carol: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/api/admin/auth/login", `{"username":"carol","password":"correct-horse-1"}`, nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("disabled carol login: expected 403, got %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(carolID)+"/status", `{"status":"enabled"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("re-enable carol: %d %s", w.Code, w.Body.String())
	}

	// A non-local (externally provisioned) username is never reachable
	// through the password form, even though the seeded row carries a
	// hash-like placeholder — same invalid-credentials answer as an
	// unknown username.
	w = f.do(t, http.MethodPost, "/api/admin/auth/login", `{"username":"bob","password":"whatever-1"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("non-local password login: expected 401, got %d %s", w.Code, w.Body.String())
	}

	// An optional, informational email lands in the directory; a malformed
	// one dies in binding.
	w = f.do(t, http.MethodPost, "/api/admin/users",
		`{"username":"erin","email":"erin@ops.example","password":"correct-horse-1"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("create erin with email: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", f.adminCk)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "erin@ops.example") {
		t.Fatalf("directory must show the recorded email: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/api/admin/users",
		`{"username":"frank","email":"not-an-email","password":"correct-horse-1"}`, f.adminCk)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed email: expected 400, got %d %s", w.Code, w.Body.String())
	}

	// Duplicate name — with carol or any existing account — is a 409 with
	// the username-taken code.
	w = f.do(t, http.MethodPost, "/api/admin/users", `{"username":"carol","password":"correct-horse-1"}`, f.adminCk)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate username: expected 409, got %d %s", w.Code, w.Body.String())
	}
	var errEnv struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &errEnv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if errEnv.Code != errcode.AccountUsernameTaken {
		t.Fatalf("duplicate username code: expected %d, got %d", errcode.AccountUsernameTaken, errEnv.Code)
	}

	// Member sessions cannot provision accounts.
	w = f.do(t, http.MethodPost, "/api/admin/users", `{"username":"eve","password":"correct-horse-1"}`, f.aliceCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member provisioning a user: expected 403, got %d", w.Code)
	}

	// Weak password is rejected by binding, not stored.
	w = f.do(t, http.MethodPost, "/api/admin/users", `{"username":"frank","password":"allletters"}`, f.adminCk)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("weak password: expected 400, got %d %s", w.Code, w.Body.String())
	}
}

// TestUserPasswordResetEndpoint walks the bootstrap-only password reset
// over real HTTP: a promoted admin is refused with the dedicated code,
// nobody resets their own, the bootstrap admin's reset kills the target's
// sessions, and only the new password signs in afterwards.
func TestUserPasswordResetEndpoint(t *testing.T) {
	f := newMemberScopeFixture(t)
	now := time.Now().UTC()

	w := f.do(t, http.MethodPost, "/api/admin/users", `{"username":"carol","password":"correct-horse-1"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("create carol: %d %s", w.Code, w.Body.String())
	}
	var carolID uint
	if err := f.db.Raw("SELECT id FROM users WHERE username = ?", "carol").Scan(&carolID).Error; err != nil || carolID == 0 {
		t.Fatalf("load carol id: %v %d", err, carolID)
	}
	var bossID uint
	if err := f.db.Raw("SELECT id FROM users WHERE username = ?", "boss").Scan(&bossID).Error; err != nil || bossID == 0 {
		t.Fatalf("load boss id: %v %d", err, bossID)
	}
	if err := repository.CreateSession(f.db, "tok-carol-live", carolID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed carol session: %v", err)
	}

	// Promote alice to admin — a legitimate admin, but not the bootstrap
	// one, so the reset power stays out of her hands.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(f.aliceID)+"/role", `{"role":"admin"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("promote alice: %d %s", w.Code, w.Body.String())
	}
	if err := repository.CreateSession(f.db, "tok-alice-promoted", f.aliceID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("relogin alice: %v", err)
	}
	aliceCk := &http.Cookie{Name: "session_id", Value: "tok-alice-promoted"}

	for _, tc := range []struct {
		name string
		path string
		ck   *http.Cookie
	}{
		{"promoted admin", "/api/admin/users/" + uintToString(carolID) + "/password", aliceCk},
		{"bootstrap self", "/api/admin/users/" + uintToString(bossID) + "/password", f.adminCk},
	} {
		w = f.do(t, http.MethodPost, tc.path, `{"password":"fresh-horse-2"}`, tc.ck)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d %s", tc.name, w.Code, w.Body.String())
		}
		var errEnv struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errEnv); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		if errEnv.Code != errcode.AccountPasswordResetDenied {
			t.Fatalf("%s: expected code %d, got %d", tc.name, errcode.AccountPasswordResetDenied, errEnv.Code)
		}
	}

	// The bootstrap admin resets carol: sessions die, old password fails,
	// the new one signs in.
	w = f.do(t, http.MethodPost, "/api/admin/users/"+uintToString(carolID)+"/password", `{"password":"fresh-horse-2"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap reset carol: %d %s", w.Code, w.Body.String())
	}
	if _, err := repository.FindUserByValidSession(f.db, "tok-carol-live", now); err == nil {
		t.Fatalf("reset must kill the target's live sessions")
	}
	w = f.do(t, http.MethodPost, "/api/admin/auth/login", `{"username":"carol","password":"correct-horse-1"}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("old password after reset: expected 401, got %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodPost, "/api/admin/auth/login", `{"username":"carol","password":"fresh-horse-2"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("new password after reset: %d %s", w.Code, w.Body.String())
	}
}

// TestSystemEndpointReadableByMembers pins where the gateway address sits in
// the access classification: any signed-in account may read it (the
// rationale lives on GetSystemEndpoint) — registering it beside the
// admin-only build info would 403 every member.
func TestSystemEndpointReadableByMembers(t *testing.T) {
	f := newMemberScopeFixture(t)

	for _, tc := range []struct {
		name string
		ck   *http.Cookie
	}{
		{"member", f.aliceCk},
		{"admin", f.adminCk},
	} {
		w := f.do(t, http.MethodGet, "/api/admin/system/endpoint", "", tc.ck)
		if w.Code != http.StatusOK {
			t.Fatalf("%s reading the gateway endpoint: expected 200, got %d %s", tc.name, w.Code, w.Body.String())
		}
		var env struct {
			Data struct {
				Endpoint string `json:"endpoint"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal %s response: %v", tc.name, err)
		}
		if env.Data.Endpoint == "" {
			t.Fatalf("%s got an empty endpoint: %s", tc.name, w.Body.String())
		}
	}

	// Readable by members, but still behind a session.
	w := f.do(t, http.MethodGet, "/api/admin/system/endpoint", "", nil)
	if w.Code == http.StatusOK {
		t.Fatalf("anonymous read of the gateway endpoint must not be 200, got %s", w.Body.String())
	}

	// The build info it used to ride along on stays admin-only.
	w = f.do(t, http.MethodGet, "/api/admin/system/version", "", f.aliceCk)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member reading build info: expected 403, got %d %s", w.Code, w.Body.String())
	}
}

// TestUserProfileEditEndpoint walks the bootstrap-only profile edit over
// real HTTP: the edit lands and the directory reflects it, a promoted
// admin and the bootstrap admin editing themselves are both refused with
// the dedicated code, and the target's session survives the edit.
func TestUserProfileEditEndpoint(t *testing.T) {
	f := newMemberScopeFixture(t)
	now := time.Now().UTC()

	// Carol: a console-created local member; alice gets promoted so the
	// "legitimate admin but not the bootstrap one" lane is reachable.
	w := f.do(t, http.MethodPost, "/api/admin/users", `{"username":"carol","display_name":"Old Name","email":"old@ops.example","password":"correct-horse-1"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("create carol: %d %s", w.Code, w.Body.String())
	}
	var carolID uint
	if err := f.db.Raw("SELECT id FROM users WHERE username = ?", "carol").Scan(&carolID).Error; err != nil || carolID == 0 {
		t.Fatalf("load carol id: %v %d", err, carolID)
	}
	var bossID uint
	if err := f.db.Raw("SELECT id FROM users WHERE username = ?", "boss").Scan(&bossID).Error; err != nil || bossID == 0 {
		t.Fatalf("load boss id: %v %d", err, bossID)
	}
	if err := repository.CreateSession(f.db, "tok-carol-edit", carolID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("seed carol session: %v", err)
	}
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(f.aliceID)+"/role", `{"role":"admin"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("promote alice: %d %s", w.Code, w.Body.String())
	}
	if err := repository.CreateSession(f.db, "tok-alice-edit", f.aliceID, now.Add(time.Hour), now); err != nil {
		t.Fatalf("relogin alice: %v", err)
	}
	aliceCk := &http.Cookie{Name: "session_id", Value: "tok-alice-edit"}

	for _, tc := range []struct {
		name string
		path string
		ck   *http.Cookie
	}{
		{"promoted admin", "/api/admin/users/" + uintToString(carolID) + "/profile", aliceCk},
		{"bootstrap self", "/api/admin/users/" + uintToString(bossID) + "/profile", f.adminCk},
	} {
		w = f.do(t, http.MethodPatch, tc.path, `{"display_name":"X"}`, tc.ck)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: expected 403, got %d %s", tc.name, w.Code, w.Body.String())
		}
		var errEnv struct {
			Code int `json:"code"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &errEnv); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		if errEnv.Code != errcode.AccountProfileEditDenied {
			t.Fatalf("%s: expected code %d, got %d", tc.name, errcode.AccountProfileEditDenied, errEnv.Code)
		}
	}

	// The bootstrap admin edits carol: fields land, the directory shows
	// them, and her live session is untouched.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(carolID)+"/profile",
		`{"display_name":"Carol Ng","email":"carol@ops.example"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap edit carol profile: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", f.adminCk)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Carol Ng") || !strings.Contains(w.Body.String(), "carol@ops.example") {
		t.Fatalf("directory must reflect the edited profile: %d %s", w.Code, w.Body.String())
	}
	if _, err := repository.FindUserByValidSession(f.db, "tok-carol-edit", now); err != nil {
		t.Fatalf("a profile edit must not kill the target's sessions")
	}

	// The same edit over the same route against bob, who arrived through a
	// login provider rather than the console: externally-provisioned
	// accounts are editable targets too, not just local password ones.
	var bobID uint
	if err := f.db.Raw("SELECT id FROM users WHERE username = ?", "bob").Scan(&bobID).Error; err != nil || bobID == 0 {
		t.Fatalf("load bob id: %v %d", err, bobID)
	}
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(bobID)+"/profile",
		`{"display_name":"Bob External","email":"bob@ops.example"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("bootstrap edit external profile: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", f.adminCk)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Bob External") || !strings.Contains(w.Body.String(), "bob@ops.example") {
		t.Fatalf("directory must reflect the edited external profile: %d %s", w.Code, w.Body.String())
	}
	if _, err := repository.FindUserByValidSession(f.db, "tok-bob", now); err != nil {
		t.Fatalf("editing an external account must not kill its sessions")
	}

	// Sparse patch: a display-name-only request must leave the email
	// column alone (nil vs empty string are different things on the wire).
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(carolID)+"/profile",
		`{"display_name":"Carol Sparse"}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("sparse patch: %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", f.adminCk)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Carol Sparse") || !strings.Contains(w.Body.String(), "carol@ops.example") {
		t.Fatalf("sparse patch must keep the untouched field: %d %s", w.Code, w.Body.String())
	}

	// Malformed email dies in binding.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(carolID)+"/profile",
		`{"email":"not-an-email"}`, f.adminCk)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed email: expected 400, got %d %s", w.Code, w.Body.String())
	}

	// An explicit empty email must pass binding and clear the column — it
	// is the modal's everyday payload for any account without an email
	// (the form always sends both fields), not an edge case. A validator
	// that runs the email check on "" would 400 here and lock those
	// accounts out of profile edits entirely.
	w = f.do(t, http.MethodPatch, "/api/admin/users/"+uintToString(carolID)+"/profile",
		`{"display_name":"Carol NoMail","email":""}`, f.adminCk)
	if w.Code != http.StatusOK {
		t.Fatalf("empty email: expected 200, got %d %s", w.Code, w.Body.String())
	}
	w = f.do(t, http.MethodGet, "/api/admin/users", "", f.adminCk)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Carol NoMail") || strings.Contains(w.Body.String(), "carol@ops.example") {
		t.Fatalf("empty email must land and clear the column: %d %s", w.Code, w.Body.String())
	}
}
