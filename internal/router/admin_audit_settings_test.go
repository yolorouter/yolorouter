package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// diffDoc builds the expected detail document the same way DiffDetail
// serializes (map -> json.Marshal), so the assertion pins the VALUE shape
// ({field: {old, new}}) without hard-coding Go's alphabetical key order.
func diffDoc(t *testing.T, field string, oldV, newV any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{field: map[string]any{"old": oldV, "new": newV}})
	if err != nil {
		t.Fatalf("marshal expected diff: %v", err)
	}
	return string(raw)
}

// TestAdminAuditSettingsUpdateDiff is the two-layer merge's headline
// case: a settings PUT lands exactly one row whose detail carries the
// field-level before/after diff of what the write actually changed. The
// first four legs pin the whole shape:
//
//   - text-only change      -> detail diffs text, omits the untouched field
//   - enabled-only change   -> detail diffs exactly that field
//   - identical re-write    -> row still lands, detail stays empty (no
//     vacuous diff for a write that changed nothing)
//   - CAS conflict (409)    -> row still lands as an error with NO diff —
//     a refused write changed nothing and must not claim it did
//
// and one leg per remaining family (input-compression, vision-fallback,
// key-auto-recovery, request-log-retention, admin-audit-retention) proves
// the merge is the settings family's SHARED shape: every one of the six
// PUTs parks its diff through the same audit.Event call, and each leg
// asserts that family's own diff content — remove any family's parking
// call and its leg goes red on the detail assertion. Every row reconciles
// with its request's X-Request-Id, so "exactly one row per PUT" is
// asserted per request, not just in aggregate.
func TestAdminAuditSettingsUpdateDiff(t *testing.T) {
	f := newAuditFixture(t)

	rowsByRequest := make(map[string]model.AdminAuditLog)
	put := func(path, body string, wantCode int) string {
		t.Helper()
		w := f.doBody(t, http.MethodPut, path, body, f.adminCk)
		if w.Code != wantCode {
			t.Fatalf("PUT %s must answer %d, got %d: %s", path, wantCode, w.Code, w.Body.String())
		}
		return w.Header().Get("X-Request-Id")
	}

	// Leg 1: change only the text. The seeded setting (enabled=false,
	// text="") is read through the same repository the handler's before-
	// read uses, so the expected diff is derived, not assumed.
	before, ver, err := repository.GetCustomSystemPrompt(f.db)
	if err != nil {
		t.Fatalf("read seeded prompt: %v", err)
	}
	newText := "Be terse and cite sources."
	rid1 := put("/api/admin/system-settings/custom-system-prompt",
		fmt.Sprintf(`{"enabled":%v,"text":%q,"version":%d}`, before.Enabled, newText, ver), http.StatusOK)

	// Leg 2: change only the switch, keeping the text from leg 1.
	mid, ver2, err := repository.GetCustomSystemPrompt(f.db)
	if err != nil {
		t.Fatalf("read prompt after leg 1: %v", err)
	}
	rid2 := put("/api/admin/system-settings/custom-system-prompt",
		fmt.Sprintf(`{"enabled":%v,"text":%q,"version":%d}`, !before.Enabled, mid.Text, ver2), http.StatusOK)

	// Leg 3: write the exact same values again — the CAS accepts, nothing
	// changes, and the audit row must say nothing diff-shaped.
	same, ver3, err := repository.GetCustomSystemPrompt(f.db)
	if err != nil {
		t.Fatalf("read prompt after leg 2: %v", err)
	}
	rid3 := put("/api/admin/system-settings/custom-system-prompt",
		fmt.Sprintf(`{"enabled":%v,"text":%q,"version":%d}`, same.Enabled, same.Text, ver3), http.StatusOK)

	// Leg 4: stale version — the optimistic lock refuses, the write changes
	// nothing, and its row must not carry a diff.
	rid4 := put("/api/admin/system-settings/custom-system-prompt",
		fmt.Sprintf(`{"enabled":%v,"text":"ghost writer","version":1}`, same.Enabled), http.StatusConflict)

	// Leg 5: the input-compression family carries the same merge shape.
	compBefore, compVer, err := repository.GetInputCompression(f.db)
	if err != nil {
		t.Fatalf("read seeded input compression: %v", err)
	}
	rid5 := put("/api/admin/system-settings/input-compression",
		fmt.Sprintf(`{"enabled":%v,"version":%d}`, !compBefore, compVer), http.StatusOK)

	// Leg 6: the vision-fallback family — change only the prompt, keeping
	// the committed model (the shipped seed is the empty/disabled model,
	// which the service accepts as-is; a model name would have to exist in
	// the models catalog, a concern orthogonal to the diff shape).
	vfBefore, vfVer, err := repository.GetVisionFallback(f.db)
	if err != nil {
		t.Fatalf("read seeded vision fallback: %v", err)
	}
	vfPrompt := "Describe each attachment tersely."
	rid6 := put("/api/admin/system-settings/vision-fallback",
		fmt.Sprintf(`{"model":%q,"prompt":%q,"version":%d}`, vfBefore.Model, vfPrompt, vfVer), http.StatusOK)

	// Leg 7: the key-auto-recovery family — flip the switch, keep the
	// interval (the seeded 30 minutes is in-domain either way).
	karBefore, karVer, err := repository.GetKeyAutoRecovery(f.db)
	if err != nil {
		t.Fatalf("read seeded key auto recovery: %v", err)
	}
	rid7 := put("/api/admin/system-settings/key-auto-recovery",
		fmt.Sprintf(`{"enabled":%v,"interval_minutes":%d,"version":%d}`,
			!karBefore.Enabled, karBefore.IntervalMinutes, karVer), http.StatusOK)

	// Leg 8: the request-log-retention family — move the days off the
	// seeded value (0 = keep forever; 180 is the design's default
	// suggestion and provably different).
	rlrBefore, rlrVer, err := repository.GetRequestLogRetention(f.db)
	if err != nil {
		t.Fatalf("read seeded request log retention: %v", err)
	}
	rlrDays := 180
	if rlrBefore.Days == 180 {
		rlrDays = 90
	}
	rid8 := put("/api/admin/system-settings/request-log-retention",
		fmt.Sprintf(`{"retention_days":%d,"version":%d}`, rlrDays, rlrVer), http.StatusOK)

	// Leg 9: the admin-audit-retention family — the trail's own expiry
	// window. Move the days off the seeded value (the migration plants 180;
	// the leg picks a provably different day count either way).
	aarBefore, aarVer, err := repository.GetAdminAuditRetention(f.db)
	if err != nil {
		t.Fatalf("read seeded admin audit retention: %v", err)
	}
	aarDays := 90
	if aarBefore.Days == 90 {
		aarDays = 365
	}
	rid9 := put("/api/admin/system-settings/admin-audit-retention",
		fmt.Sprintf(`{"retention_days":%d,"version":%d}`, aarDays, aarVer), http.StatusOK)

	rows := f.drain(t)
	if len(rows) != 9 {
		t.Fatalf("nine settings PUTs must land exactly nine rows, got %d", len(rows))
	}
	for _, row := range rows {
		rowsByRequest[row.RequestID] = row
	}

	assertRow := func(rid, wantDetail, wantStatus string) {
		t.Helper()
		row, ok := rowsByRequest[rid]
		if !ok {
			t.Fatalf("no audit row reconciles with request %s", rid)
		}
		if row.Action != "system_settings.update" {
			t.Errorf("request %s: action = %q, want system_settings.update", rid, row.Action)
		}
		if row.Status != wantStatus {
			t.Errorf("request %s: status = %q, want %q", rid, row.Status, wantStatus)
		}
		if row.Detail != wantDetail {
			t.Errorf("request %s: detail = %s, want %s", rid, row.Detail, wantDetail)
		}
	}

	// Leg 1: only text changed — the untouched enabled field must not bleed
	// into the diff.
	assertRow(rid1, diffDoc(t, "text", before.Text, newText), model.AuditStatusSuccess)
	// Leg 2: only the switch changed.
	assertRow(rid2, diffDoc(t, "enabled", before.Enabled, !before.Enabled), model.AuditStatusSuccess)
	// Leg 3: identical values — empty detail, still exactly one row.
	assertRow(rid3, "", model.AuditStatusSuccess)
	// Leg 4: the conflict is an error outcome carrying no diff.
	assertRow(rid4, "", model.AuditStatusError)
	// Leg 5: the family shape holds on a second setting.
	assertRow(rid5, diffDoc(t, "enabled", compBefore, !compBefore), model.AuditStatusSuccess)
	// Legs 6-9: …and on the four remaining families, each with its own
	// changed field — every family's parking call is under test, not just
	// the first two endpoints'.
	assertRow(rid6, diffDoc(t, "prompt", vfBefore.Prompt, vfPrompt), model.AuditStatusSuccess)
	assertRow(rid7, diffDoc(t, "enabled", karBefore.Enabled, !karBefore.Enabled), model.AuditStatusSuccess)
	assertRow(rid8, diffDoc(t, "retention_days", rlrBefore.Days, rlrDays), model.AuditStatusSuccess)
	assertRow(rid9, diffDoc(t, "retention_days", aarBefore.Days, aarDays), model.AuditStatusSuccess)

	// resource_id: each settings route identifies its subject by the final
	// path segment (the stable slug), whichever leg fired.
	for _, rid := range []string{rid1, rid2, rid3, rid4} {
		if row := rowsByRequest[rid]; row.ResourceID != "custom-system-prompt" {
			t.Errorf("request %s: resource_id = %q, want custom-system-prompt", rid, row.ResourceID)
		}
	}
	wantSlug := map[string]string{
		rid5: "input-compression",
		rid6: "vision-fallback",
		rid7: "key-auto-recovery",
		rid8: "request-log-retention",
		rid9: "admin-audit-retention",
	}
	for rid, slug := range wantSlug {
		if row := rowsByRequest[rid]; row.ResourceID != slug {
			t.Errorf("request %s: resource_id = %q, want %s", rid, row.ResourceID, slug)
		}
	}
}
