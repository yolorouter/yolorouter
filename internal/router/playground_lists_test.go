package router

// Picker-list tests for the console Playground's two member-reachable GET
// endpoints: the key picker (pinned to the session's own account for both
// roles, unrevoked/unexpired only) and the model picker (routing-enabled
// models with at least one routable candidate, projected to name plus
// modality declaration). The run endpoint those pickers feed has its own
// full-chain coverage next door.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// getWithSession drives a GET the way the console page will: session cookie
// only.
func getWithSession(r *gin.Engine, path, session string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "session_id", Value: session})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// listedKeyIDs decodes the key picker's envelope into its id list.
func listedKeyIDs(t *testing.T, body string) []uint {
	t.Helper()
	var env struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				ID        uint   `json:"id"`
				KeyPrefix string `json:"key_prefix"`
				Remark    string `json:"remark"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("key picker response not JSON: %v (%s)", err, body)
	}
	if env.Code != 0 {
		t.Fatalf("key picker response carried business code %d: %s", env.Code, body)
	}
	ids := make([]uint, 0, len(env.Data.List))
	for _, k := range env.Data.List {
		ids = append(ids, k.ID)
	}
	return ids
}

func TestPlaygroundKeyListingPinnedToSessionAccount(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	member := seedConsoleUser(t, db, "pg-pick-member", model.RoleMember, "tok-pg-pick-member")
	admin := seedConsoleUser(t, db, "pg-pick-admin", model.RoleAdmin, "tok-pg-pick-admin")

	own, ownRaw := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-picker-own-01", func(k *model.APIKey) {
		k.Remark = "daily driver"
	})
	revoked, revokedRaw := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-picker-revok01", func(k *model.APIKey) {
		k.Status = model.APIKeyStatusRevoked
	})
	expiredAt := time.Now().UTC().Add(-time.Hour)
	expired, expiredRaw := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-picker-expir01", func(k *model.APIKey) {
		k.ExpiresAt = &expiredAt
	})
	// Another account's key (the admin's) — foreign only from the member's
	// side; the admin's own picker below must list it like any own key.
	foreign, foreignRaw := seedPlaygroundKey(t, db, admin.ID, "sk-yr-pg-picker-admkey1", nil)
	adminOwn, adminOwnRaw := seedPlaygroundKey(t, db, admin.ID, "sk-yr-pg-picker-admown1", nil)

	r := newRelayTestRouter(t, db)

	// The member's picker: exactly their own usable key. Revoked, expired,
	// and the admin's keys are all absent — the run endpoint refuses every
	// one of them, so the picker must not have offered them.
	w := getWithSession(r, "/api/admin/playground/keys", "tok-pg-pick-member")
	if w.Code != http.StatusOK {
		t.Fatalf("member key picker: expected 200, got %d %s", w.Code, w.Body.String())
	}
	ids := listedKeyIDs(t, w.Body.String())
	if len(ids) != 1 || ids[0] != own.ID {
		t.Fatalf("member key picker: expected exactly own key %d, got %v (%s)", own.ID, ids, w.Body.String())
	}
	for _, absent := range []uint{revoked.ID, expired.ID, foreign.ID, adminOwn.ID} {
		for _, id := range ids {
			if id == absent {
				t.Fatalf("member key picker offered key %d it must not (body %s)", absent, w.Body.String())
			}
		}
	}
	// Identifying fields the picker renders: prefix and remark, never the
	// plaintext of any key on this instance.
	body := w.Body.String()
	if !strings.Contains(body, own.KeyPrefix) || !strings.Contains(body, "daily driver") {
		t.Fatalf("member key picker row lacks prefix/remark: %s", body)
	}
	for _, raw := range []string{ownRaw, revokedRaw, expiredRaw, foreignRaw, adminOwnRaw} {
		if strings.Contains(body, raw) {
			t.Fatalf("key picker response leaked a key plaintext: %s", body)
		}
	}

	// The admin's picker is pinned the same way: exactly their own two keys,
	// not the member's — the run endpoint enforces ownership for every role,
	// so a picker offering another account's row would only produce a 403
	// run.
	w = getWithSession(r, "/api/admin/playground/keys", "tok-pg-pick-admin")
	if w.Code != http.StatusOK {
		t.Fatalf("admin key picker: expected 200, got %d %s", w.Code, w.Body.String())
	}
	ids = listedKeyIDs(t, w.Body.String())
	if len(ids) != 2 {
		t.Fatalf("admin key picker: expected their own two keys, got %v (%s)", ids, w.Body.String())
	}
	for _, want := range []uint{adminOwn.ID, foreign.ID} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("admin key picker omitted own key %d: %s", want, w.Body.String())
		}
	}
	for _, id := range ids {
		if id == own.ID {
			t.Fatalf("admin key picker offered the member's key: %s", w.Body.String())
		}
	}
}

// listedModels decodes the model picker's envelope into its rows.
func listedModels(t *testing.T, body string) map[string][]string {
	t.Helper()
	var env struct {
		Code int `json:"code"`
		Data struct {
			List []struct {
				Name             string   `json:"name"`
				OutputModalities []string `json:"output_modalities"`
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("model picker response not JSON: %v (%s)", err, body)
	}
	if env.Code != 0 {
		t.Fatalf("model picker response carried business code %d: %s", env.Code, body)
	}
	out := make(map[string][]string, len(env.Data.List))
	for _, m := range env.Data.List {
		out[m.Name] = m.OutputModalities
	}
	return out
}

func TestPlaygroundModelListingRoutableOnly(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL) // "gpt-4o": enabled + routable, text default

	// A model that also serves images: in the list, carrying its
	// declaration verbatim — the picker's modality filter is fed by exactly
	// this array.
	mixed := seedPlaygroundUpstreamModel(t, db, "pg-pick-mixed", "pg-pick-mixed-model", "pg-pick-mixed-upstream", upstream.URL)
	if err := db.Model(mixed).Update("output_modalities", `["text","image"]`).Error; err != nil {
		t.Fatalf("seed mixed modalities: %v", err)
	}
	// Routing disabled at the model level: a comparison against it cannot
	// start, so the picker must not offer it.
	disabled := seedPlaygroundUpstreamModel(t, db, "pg-pick-dis", "pg-pick-disabled-model", "pg-pick-disabled-upstream", upstream.URL)
	if err := db.Model(disabled).Update("management_status", model.ModelStatusDisabled).Error; err != nil {
		t.Fatalf("seed disabled model: %v", err)
	}
	// Enabled model whose only candidate is switched off: same verdict.
	unroutable := seedPlaygroundUpstreamModel(t, db, "pg-pick-unr", "pg-pick-unroutable-model", "pg-pick-unroutable-upstream", upstream.URL)
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", unroutable.ID).
		Update("management_status", model.ModelCandidateStatusDisabled).Error; err != nil {
		t.Fatalf("seed unroutable candidate: %v", err)
	}
	// Enabled model with no candidates at all.
	now := time.Now().UTC()
	if err := db.Create(&model.Model{Name: "pg-pick-bare-model", ManagementStatus: model.ModelStatusEnabled,
		SchedulingMode: model.ModelSchedulingModeFailover, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatalf("seed bare model: %v", err)
	}

	seedConsoleUser(t, db, "pg-pick-models-member", model.RoleMember, "tok-pick-models")
	r := newRelayTestRouter(t, db)
	w := getWithSession(r, "/api/admin/playground/models", "tok-pick-models")
	if w.Code != http.StatusOK {
		t.Fatalf("model picker: expected 200, got %d %s", w.Code, w.Body.String())
	}
	got := listedModels(t, w.Body.String())

	if mods, ok := got["gpt-4o"]; !ok {
		t.Fatalf("model picker omitted the routable text model: %v", got)
	} else if len(mods) != 1 || mods[0] != model.OutputModalityText {
		t.Fatalf("model picker text default declaration mismatch: %v", mods)
	}
	if mods, ok := got["pg-pick-mixed-model"]; !ok {
		t.Fatalf("model picker omitted the mixed-modality model: %v", got)
	} else if len(mods) != 2 || mods[0] != "text" || mods[1] != "image" {
		t.Fatalf("mixed-modality declaration not carried verbatim: %v", mods)
	}
	for _, absent := range []string{"pg-pick-disabled-model", "pg-pick-unroutable-model", "pg-pick-bare-model"} {
		if _, ok := got[absent]; ok {
			t.Fatalf("model picker offered %s, which cannot be routed to: %v", absent, got)
		}
	}
}

// The rows the picker projections promise to carry are the rows the run
// endpoint accepts: sanity-check that a picker-listed model name really is
// runnable and a picker-listed key really is resolvable (the projection
// feeding the run must not drift from the run's own gates).
func TestPlaygroundPickerRowsAreRunnable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-pick-run-member", model.RoleMember, "tok-pick-run")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-picker-runnbl1", nil)

	r := newRelayTestRouter(t, db)
	models := listedModels(t, getWithSession(r, "/api/admin/playground/models", "tok-pick-run").Body.String())
	if _, ok := models["gpt-4o"]; !ok {
		t.Fatalf("picker/run drift: gpt-4o is not listed but is seeded runnable: %v", models)
	}
	keys := listedKeyIDs(t, getWithSession(r, "/api/admin/playground/keys", "tok-pick-run").Body.String())
	found := false
	for _, id := range keys {
		if id == key.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("picker/run drift: seeded key %d is not listed: %v", key.ID, keys)
	}
	// And one full run through the listed pair, proving the picker fed the
	// run with exactly the vocabulary the run accepts.
	w := postPlaygroundRun(r, "tok-pick-run", runBody(key.ID, "gpt-4o"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: done") {
		t.Fatalf("run with picker-listed pair did not complete: %d %s", w.Code, w.Body.String())
	}
}
