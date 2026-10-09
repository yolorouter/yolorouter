package audit

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/model"
)

// decodeDetail parses a detail document into the generic shape the
// settings-family assertions inspect: field -> {"old": …, "new": …}.
func decodeDetail(t *testing.T, doc string) map[string]map[string]any {
	t.Helper()
	var out map[string]map[string]any
	if err := json.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatalf("detail is not a field->old/new document (%v): %s", err, doc)
	}
	return out
}

// TestDiffDetailRecordsChangedFieldsOnly is the settings diff's core
// contract: every changed field appears as {"old": …, "new": …}, every
// unchanged field is absent, and a write that changed nothing produces no
// document at all (the empty string the middleware stores as an empty
// detail column — "nothing changed" must not be recorded as a diff with
// nulls).
func TestDiffDetailRecordsChangedFieldsOnly(t *testing.T) {
	doc := DiffDetail(
		map[string]any{"enabled": false, "text": "old prompt", "retention_days": 30},
		map[string]any{"enabled": false, "text": "new prompt", "retention_days": 90},
	)
	diff := decodeDetail(t, doc)
	if len(diff) != 2 {
		t.Fatalf("diff must carry exactly the 2 changed fields, got %d: %s", len(diff), doc)
	}
	if diff["text"]["old"] != "old prompt" || diff["text"]["new"] != "new prompt" {
		t.Errorf("text diff = %v, want old prompt -> new prompt", diff["text"])
	}
	if diff["retention_days"]["old"] != float64(30) || diff["retention_days"]["new"] != float64(90) {
		t.Errorf("retention_days diff = %v, want 30 -> 90", diff["retention_days"])
	}
	if _, present := diff["enabled"]; present {
		t.Errorf("unchanged field enabled must be omitted from the diff: %s", doc)
	}
}

// TestDiffDetailNoChangeIsEmptyDocument pins the nothing-changed edge: the
// audit row still lands (the write happened), but its detail stays empty
// rather than carrying a vacuous diff.
func TestDiffDetailNoChangeIsEmptyDocument(t *testing.T) {
	if doc := DiffDetail(
		map[string]any{"enabled": true, "text": "same"},
		map[string]any{"enabled": true, "text": "same"},
	); doc != "" {
		t.Errorf("a no-change diff must be the empty string, got %s", doc)
	}
}

// TestDiffDetailMissingSideDiffsAgainstNull keeps sparse callers honest: a
// field present on only one side is still a change (against JSON null),
// not a silently dropped field.
func TestDiffDetailMissingSideDiffsAgainstNull(t *testing.T) {
	diff := decodeDetail(t, DiffDetail(
		map[string]any{"model": "gpt-4o"},
		map[string]any{"model": "gpt-4o", "prompt": "describe"},
	))
	if len(diff) != 1 {
		t.Fatalf("only the added field may appear, got %s", diff)
	}
	prompt, ok := diff["prompt"]
	if !ok {
		t.Fatal("the field added on the after side must be diffed")
	}
	if prompt["old"] != nil || prompt["new"] != "describe" {
		t.Errorf("added field diff = %v, want null -> describe", prompt)
	}
}

// TestDiffDetailPassesRedactionPipeline is the merge's safety half: the
// diff document is redacted before it is parked, so a setting value that
// happens to look like a credential never reaches the detail column even
// if the caller forgot to think about it.
func TestDiffDetailPassesRedactionPipeline(t *testing.T) {
	secret := "sk-diffdetail-leak-check-42"
	doc := DiffDetail(
		map[string]any{"text": "harmless"},
		map[string]any{"text": secret},
	)
	if strings.Contains(doc, secret) {
		t.Errorf("key-shaped plaintext leaked through the diff: %s", doc)
	}
	diff := decodeDetail(t, doc)
	if diff["text"]["new"] != "***" {
		t.Errorf("key-shaped new value must be the placeholder, got %v", diff["text"])
	}
}

// TestReadWithExplicitEventLandsRow is the sensitive-read door's unit
// contract: a GET never produces a row from the route shape alone, but a
// GET whose handler raised an explicit event lands exactly that event's
// row — the mechanism behind the audited-read exceptions (key reveal)
// while every other console read stays silent. The event carries its own
// action and resource id because the read path has no automatic
// resolution worth keeping (the dictionary names write routes only).
func TestReadWithExplicitEventLandsRow(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.GET("/api-keys/:id/plaintext", func(c *gin.Context) {
		Event(c, ActionAPIKeyReveal, c.Param("id"), "")
	})

	if w := f.do(t, http.MethodGet, "/api/admin/api-keys/7/plaintext", ""); w.Code != http.StatusOK {
		t.Fatalf("reveal route must answer 200, got %d", w.Code)
	}

	rows := f.rows(t)
	if len(rows) != 1 {
		t.Fatalf("the event-carrying read must land exactly one row, got %d", len(rows))
	}
	row := rows[0]
	if row.Action != ActionAPIKeyReveal {
		t.Errorf("action = %q, want %q from the event, not a route derivation", row.Action, ActionAPIKeyReveal)
	}
	if row.ResourceType != "api_key" {
		t.Errorf("resource type = %q, want the event action's namespace api_key", row.ResourceType)
	}
	if row.ResourceID != "7" {
		t.Errorf("resource id = %q, want the event's id 7", row.ResourceID)
	}
	if row.Status != model.AuditStatusSuccess {
		t.Errorf("status = %q, want success", row.Status)
	}
	if row.Detail != "" {
		t.Errorf("detail = %q, want empty — the reveal names the key by id, never by value", row.Detail)
	}
}

// TestEventResourceOverrideReachesRow drives Event through the full
// middleware: an event carrying a resource id that no path parameter could
// supply (a reveal names the exact key) must land on the row, while the
// empty overrides of a detail-only event keep the route's own resolution.
// The reveal route is deliberately unlisted so the dictionary cannot name
// it — the event's own action is what the row must carry, which is the
// explicit layer's whole reason to exist.
func TestEventResourceOverrideReachesRow(t *testing.T) {
	f := newCaptureFixture(t)
	f.group.POST("/system/update", func(c *gin.Context) {
		Event(c, "", "", `{"target_version":"v9.9.9"}`)
	})
	f.group.POST("/reveal", func(c *gin.Context) {
		Event(c, "provider_key.reveal", "42", `{"key_suffix":"ab12"}`)
	})

	if w := f.do(t, http.MethodPost, "/api/admin/system/update", `{}`); w.Code != http.StatusOK {
		t.Fatalf("update route must answer 200, got %d", w.Code)
	}
	if w := f.do(t, http.MethodPost, "/api/admin/reveal", `{}`); w.Code != http.StatusOK {
		t.Fatalf("reveal route must answer 200, got %d", w.Code)
	}

	rows := f.rows(t)
	if len(rows) != 2 {
		t.Fatalf("two events on two requests must land exactly two rows, got %d", len(rows))
	}
	byAction := map[string]model.AdminAuditLog{}
	for _, row := range rows {
		byAction[row.Action] = row
	}

	// Detail-only event: action and resource keep the route resolution (the
	// dictionary names /system/update; its resource id is empty because the
	// path carries no parameter), the detail rides along verbatim.
	update, ok := byAction["system.update"]
	if !ok {
		t.Fatalf("no system.update row; have %v", byAction)
	}
	if update.ResourceID != "" {
		t.Errorf("an event with no resource override must keep the path-derived id (empty here), got %q", update.ResourceID)
	}
	if update.Detail != `{"target_version":"v9.9.9"}` {
		t.Errorf("update detail = %s, want the parked target version", update.Detail)
	}
	if update.Status != model.AuditStatusSuccess {
		t.Errorf("update status = %q, want success", update.Status)
	}

	// Full event: action, resource type (from the override's namespace),
	// and resource id all come from the event.
	reveal, ok := byAction["provider_key.reveal"]
	if !ok {
		t.Fatalf("no provider_key.reveal row; have %v", byAction)
	}
	if reveal.ResourceType != "provider_key" {
		t.Errorf("resource type = %q, want the override action's namespace provider_key", reveal.ResourceType)
	}
	if reveal.ResourceID != "42" {
		t.Errorf("resource id = %q, want the event's override 42", reveal.ResourceID)
	}
	if reveal.Detail != `{"key_suffix":"ab12"}` {
		t.Errorf("reveal detail = %s, want the parked suffix document", reveal.Detail)
	}
}
