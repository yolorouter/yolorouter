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
// key-auto-recovery, request-log-retention, admin-audit-retention,
// outbound-proxy, capability-enforcement) proves the merge is the settings
// family's SHARED shape: every one of the eight PUTs parks its diff through
// the same audit.Event call, and each leg asserts that family's own diff
// content — remove any
// family's parking call and its leg goes red on the detail assertion. The
// outbound-proxy legs additionally pin that family's redaction contract:
// proxy_url's diff entry is the placeholder, never the address. Every row
// reconciles with its request's X-Request-Id, so "exactly one row per PUT"
// is asserted per request, not just in aggregate.
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

	// Leg 10: the outbound-proxy family — turn the shipped default (off,
	// empty URL) into custom with a URL carrying embedded basic auth, so the
	// leg asserts both changed fields at once AND the redaction contract:
	// the mode diffs in clear, while proxy_url's diff entry collapses to
	// the placeholder and the credential-bearing URL must never reach any
	// column of the row.
	opBefore, opVer, err := repository.GetOutboundProxy(f.db)
	if err != nil {
		t.Fatalf("read seeded outbound proxy: %v", err)
	}
	opURL := "http://proxuser:proxpass@127.0.0.1:3128"
	rid10 := put("/api/admin/system-settings/outbound-proxy",
		fmt.Sprintf(`{"mode":"custom","url":%q,"version":%d}`, opURL, opVer), http.StatusOK)

	// Leg 11: write the exact same values again — the CAS accepts, nothing
	// changes, no diff-shaped detail.
	opSame, opVer2, err := repository.GetOutboundProxy(f.db)
	if err != nil {
		t.Fatalf("read outbound proxy after leg 10: %v", err)
	}
	rid11 := put("/api/admin/system-settings/outbound-proxy",
		fmt.Sprintf(`{"mode":%q,"url":%q,"version":%d}`, opSame.Mode, opSame.URL, opVer2), http.StatusOK)

	// Leg 12: stale version — the optimistic lock refuses and the row must
	// carry no diff.
	rid12 := put("/api/admin/system-settings/outbound-proxy",
		`{"mode":"off","url":"","version":1}`, http.StatusConflict)

	// Leg 13: the capability-enforcement family — turn two reasoning
	// switches on and widen the margin in one save, so the leg asserts the
	// multi-field diff shape the card's whole-snapshot PUT produces.
	ceBefore, ceVer, err := repository.GetCapabilityEnforcement(f.db)
	if err != nil {
		t.Fatalf("read seeded capability enforcement: %v", err)
	}
	ceMargin := ceBefore.ContextMarginPercent + 5
	rid13 := put("/api/admin/system-settings/capability-enforcement",
		fmt.Sprintf(`{"context_limit_block":%v,"temperature_default":%v,"temperature_force":%v,"tools_strict":%v,"reasoning_strict":true,"reasoning_egress_clear":true,"context_margin_percent":%d,"version":%d}`,
			ceBefore.ContextLimitBlock, ceBefore.TemperatureDefault, ceBefore.TemperatureForce,
			ceBefore.ToolsStrict, ceMargin, ceVer), http.StatusOK)

	// Leg 14: stale version — the seven-row CAS refuses and the row must
	// carry no diff.
	rid14 := put("/api/admin/system-settings/capability-enforcement",
		`{"context_limit_block":false,"temperature_default":false,"temperature_force":false,"tools_strict":false,"reasoning_strict":false,"reasoning_egress_clear":false,"context_margin_percent":5,"version":1}`, http.StatusConflict)

	rows := f.drain(t)
	if len(rows) != 14 {
		t.Fatalf("fourteen settings PUTs must land exactly fourteen rows, got %d", len(rows))
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
	// Leg 10: the outbound-proxy family's redaction shape — the mode's
	// {old,new} diff in clear next to proxy_url collapsed to the placeholder
	// (the change is recorded, the address and its embedded credential are
	// not), built with json.Marshal the same way DiffDetail serializes so
	// the assertion pins the shape without hard-coding key order.
	redactedProxyDoc, err := json.Marshal(map[string]any{
		"mode":      map[string]any{"old": opBefore.Mode, "new": "custom"},
		"proxy_url": "***",
	})
	if err != nil {
		t.Fatalf("marshal expected redacted proxy diff: %v", err)
	}
	assertRow(rid10, string(redactedProxyDoc), model.AuditStatusSuccess)
	assertRowsCarryNoPlaintext(t, []model.AdminAuditLog{rowsByRequest[rid10]}, opURL, "proxpass", "proxuser")
	// Leg 11: identical values — empty detail, still exactly one row.
	assertRow(rid11, "", model.AuditStatusSuccess)
	// Leg 12: the conflict is an error outcome carrying no diff.
	assertRow(rid12, "", model.AuditStatusError)
	// Leg 13: the capability-enforcement family's diff — exactly the three
	// fields the save flipped (two switches on, margin widened), with the
	// four untouched switches absent from the diff.
	ceDiff, err := json.Marshal(map[string]any{
		"reasoning_strict":       map[string]any{"old": ceBefore.ReasoningStrict, "new": true},
		"reasoning_egress_clear": map[string]any{"old": ceBefore.ReasoningEgressClear, "new": true},
		"context_margin_percent": map[string]any{"old": ceBefore.ContextMarginPercent, "new": ceMargin},
	})
	if err != nil {
		t.Fatalf("marshal expected capability diff: %v", err)
	}
	assertRow(rid13, string(ceDiff), model.AuditStatusSuccess)
	// Leg 14: the refused whole-snapshot CAS is an error outcome carrying no
	// diff.
	assertRow(rid14, "", model.AuditStatusError)

	// resource_id: each settings route identifies its subject by the final
	// path segment (the stable slug), whichever leg fired.
	for _, rid := range []string{rid1, rid2, rid3, rid4} {
		if row := rowsByRequest[rid]; row.ResourceID != "custom-system-prompt" {
			t.Errorf("request %s: resource_id = %q, want custom-system-prompt", rid, row.ResourceID)
		}
	}
	wantSlug := map[string]string{
		rid5:  "input-compression",
		rid6:  "vision-fallback",
		rid7:  "key-auto-recovery",
		rid8:  "request-log-retention",
		rid9:  "admin-audit-retention",
		rid10: "outbound-proxy",
		rid11: "outbound-proxy",
		rid12: "outbound-proxy",
		rid13: "capability-enforcement",
		rid14: "capability-enforcement",
	}
	for rid, slug := range wantSlug {
		if row := rowsByRequest[rid]; row.ResourceID != slug {
			t.Errorf("request %s: resource_id = %q, want %s", rid, row.ResourceID, slug)
		}
	}
}
