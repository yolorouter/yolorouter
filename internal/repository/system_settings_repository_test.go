package repository

import (
	"errors"
	"testing"

	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/errcode"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newSettingsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db.Exec(`CREATE TABLE system_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '', version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP)`)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('custom_system_prompt_enabled','false'),('custom_system_prompt','')`)
	return db
}

func TestGetCustomSystemPromptReadsBothRows(t *testing.T) {
	db := newSettingsTestDB(t)
	s, ver, err := GetCustomSystemPrompt(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Enabled || s.Text != "" {
		t.Fatalf("want disabled/empty, got %+v", s)
	}
	if ver != 1 {
		t.Fatalf("version = %d, want 1", ver)
	}
}

func TestGetCustomSystemPromptRejectsCorruptEnabled(t *testing.T) {
	db := newSettingsTestDB(t)
	db.Exec(`UPDATE system_settings SET value='maybe' WHERE key='custom_system_prompt_enabled'`)
	if _, _, err := GetCustomSystemPrompt(db); err == nil {
		t.Fatal("expected error for corrupt enabled value, got nil")
	}
}

func TestUpdateCustomSystemPromptCASConflict(t *testing.T) {
	db := newSettingsTestDB(t)
	// first successful update bumps version 1 -> 2
	if _, _, err := UpdateCustomSystemPrompt(db, 1, true, "hello"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	// stale expectedVersion=1 must conflict
	_, _, err := UpdateCustomSystemPrompt(db, 1, false, "")
	if !errors.Is(err, errcode.ErrCustomSystemPromptConflict) {
		t.Fatalf("want ErrCustomSystemPromptConflict, got %v", err)
	}
}

func TestUpdateCustomSystemPromptReturnsNewSnapshot(t *testing.T) {
	db := newSettingsTestDB(t)
	s, ver, err := UpdateCustomSystemPrompt(db, 1, true, "hi")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !s.Enabled || s.Text != "hi" || ver != 2 {
		t.Fatalf("want enabled/hi/v2, got %+v v%d", s, ver)
	}
	// persisted?
	got, gver, err := GetCustomSystemPrompt(db)
	if err != nil || !got.Enabled || got.Text != "hi" || gver != 2 {
		t.Fatalf("read-back mismatch: %+v v%d err=%v", got, gver, err)
	}
	_ = settings.CustomSystemPromptSetting{} // keep import if assertions above evolve
}

// --- Input compression repository -------------------------------------------

// newSettingsTestDBWithIC returns a settings test DB with the
// input_compression_enabled row also seeded at v1 disabled.
func newSettingsTestDBWithIC(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSettingsTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('input_compression_enabled','false')`)
	return db
}

func TestGetInputCompressionReadsSeededRow(t *testing.T) {
	db := newSettingsTestDBWithIC(t)
	// Bump to v3 + enabled to confirm version + value are both read.
	db.Exec(`UPDATE system_settings SET value='true', version=3 WHERE key='input_compression_enabled'`)
	enabled, ver, err := GetInputCompression(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !enabled || ver != 3 {
		t.Fatalf("want enabled=true/v3, got enabled=%v v%d", enabled, ver)
	}
}

func TestGetInputCompressionMissingRowReturnsDefault(t *testing.T) {
	// newSettingsTestDB seeds only the CSP rows; the IC row is absent.
	db := newSettingsTestDB(t)
	enabled, ver, err := GetInputCompression(db)
	if err != nil {
		t.Fatalf("missing row: want (false,0,nil), got err=%v", err)
	}
	if enabled || ver != 0 {
		t.Fatalf("want disabled/v0, got enabled=%v v%d", enabled, ver)
	}
}

func TestGetInputCompressionRejectsCorruptValue(t *testing.T) {
	db := newSettingsTestDBWithIC(t)
	db.Exec(`UPDATE system_settings SET value='maybe' WHERE key='input_compression_enabled'`)
	if _, _, err := GetInputCompression(db); err == nil {
		t.Fatal("expected error for corrupt input_compression_enabled value, got nil")
	}
}

func TestUpdateInputCompressionSuccess(t *testing.T) {
	db := newSettingsTestDBWithIC(t)
	enabled, ver, err := UpdateInputCompression(db, 1, true)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !enabled || ver != 2 {
		t.Fatalf("want enabled=true/v2, got enabled=%v v%d", enabled, ver)
	}
	// Persisted?
	got, gver, err := GetInputCompression(db)
	if err != nil || !got || gver != 2 {
		t.Fatalf("read-back mismatch: enabled=%v v%d err=%v", got, gver, err)
	}
}

func TestUpdateInputCompressionCASConflict(t *testing.T) {
	db := newSettingsTestDBWithIC(t)
	// First successful update bumps version 1 -> 2.
	if _, _, err := UpdateInputCompression(db, 1, true); err != nil {
		t.Fatalf("first update: %v", err)
	}
	// Stale expectedVersion=1 must conflict.
	_, _, err := UpdateInputCompression(db, 1, false)
	if !errors.Is(err, errcode.ErrInputCompressionConflict) {
		t.Fatalf("want ErrInputCompressionConflict, got %v", err)
	}
}

func newVisionFallbackTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSettingsTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('vision_fallback_model',''),('vision_fallback_prompt','')`)
	return db
}

func TestGetVisionFallbackReadsBothRows(t *testing.T) {
	db := newVisionFallbackTestDB(t)
	db.Exec(`UPDATE system_settings SET value='glm-4v' WHERE key='vision_fallback_model'`)
	s, ver, err := GetVisionFallback(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Model != "glm-4v" || s.Prompt != "" {
		t.Fatalf("snapshot = %+v, want model glm-4v with empty prompt", s)
	}
	if ver != 1 {
		t.Fatalf("version = %d, want 1", ver)
	}
}

// Missing rows (a pre-00022 database) must read as the disabled default, not
// an error — the gateway read path fails open on configuration.
func TestGetVisionFallbackMissingRowsReturnsDefault(t *testing.T) {
	db := newSettingsTestDB(t)
	s, ver, err := GetVisionFallback(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Model != "" || s.Prompt != "" || ver != 0 {
		t.Fatalf("want disabled default, got %+v ver %d", s, ver)
	}
}

func TestUpdateVisionFallbackCASConflict(t *testing.T) {
	db := newVisionFallbackTestDB(t)
	if _, _, err := UpdateVisionFallback(db, 1, "glm-4v", "describe it"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	_, _, err := UpdateVisionFallback(db, 1, "", "")
	if !errors.Is(err, errcode.ErrVisionFallbackConflict) {
		t.Fatalf("want ErrVisionFallbackConflict, got %v", err)
	}
}

func TestUpdateVisionFallbackReturnsNewSnapshot(t *testing.T) {
	db := newVisionFallbackTestDB(t)
	s, ver, err := UpdateVisionFallback(db, 1, "glm-4v", "custom prompt")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.Model != "glm-4v" || s.Prompt != "custom prompt" || ver != 2 {
		t.Fatalf("got %+v ver %d, want committed snapshot at version 2", s, ver)
	}
	// The DB agrees with the returned snapshot.
	got, gotVer, err := GetVisionFallback(db)
	if err != nil || got != s || gotVer != ver {
		t.Fatalf("reread = %+v ver %d err %v, want %+v ver %d", got, gotVer, err, s, ver)
	}
}

// --- Key auto recovery repository --------------------------------------------

// newKeyAutoRecoveryTestDB seeds the pair at the migration default:
// enabled + 30 minutes, both rows at v1.
func newKeyAutoRecoveryTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := newSettingsTestDB(t)
	db.Exec(`INSERT INTO system_settings (key, value) VALUES ('key_auto_recovery_enabled','true'),('key_auto_recovery_interval_minutes','30')`)
	return db
}

func TestGetKeyAutoRecoveryReadsSeededPair(t *testing.T) {
	db := newKeyAutoRecoveryTestDB(t)
	// Bump to v3 + changed values to confirm version + both fields are read.
	db.Exec(`UPDATE system_settings SET value='false' WHERE key='key_auto_recovery_enabled'`)
	db.Exec(`UPDATE system_settings SET value='45' WHERE key='key_auto_recovery_interval_minutes'`)
	db.Exec(`UPDATE system_settings SET version=3 WHERE key IN ('key_auto_recovery_enabled','key_auto_recovery_interval_minutes')`)
	s, ver, err := GetKeyAutoRecovery(db)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s.Enabled || s.IntervalMinutes != 45 || ver != 3 {
		t.Fatalf("want disabled/45/v3, got %+v v%d", s, ver)
	}
}

// Missing rows (a pre-00048 database) must read as the shipped default, not
// an error — the probe loop's read path fails open on configuration.
func TestGetKeyAutoRecoveryMissingRowsReturnDefault(t *testing.T) {
	db := newSettingsTestDB(t) // no key_auto_recovery rows seeded
	s, ver, err := GetKeyAutoRecovery(db)
	if err != nil {
		t.Fatalf("missing rows: want default, got err=%v", err)
	}
	if !s.Enabled || s.IntervalMinutes != settings.KeyAutoRecoveryDefaultIntervalMinutes || ver != 0 {
		t.Fatalf("want enabled/30/v0 (shipped default), got %+v v%d", s, ver)
	}
}

func TestGetKeyAutoRecoveryRejectsCorruptEnabled(t *testing.T) {
	db := newKeyAutoRecoveryTestDB(t)
	db.Exec(`UPDATE system_settings SET value='maybe' WHERE key='key_auto_recovery_enabled'`)
	if _, _, err := GetKeyAutoRecovery(db); err == nil {
		t.Fatal("expected error for corrupt enabled value, got nil")
	}
}

func TestGetKeyAutoRecoveryRejectsCorruptInterval(t *testing.T) {
	db := newKeyAutoRecoveryTestDB(t)
	db.Exec(`UPDATE system_settings SET value='soon' WHERE key='key_auto_recovery_interval_minutes'`)
	if _, _, err := GetKeyAutoRecovery(db); err == nil {
		t.Fatal("expected error for corrupt interval value, got nil")
	}
}

// A lone row of the pair is a torn write, not a default — it must surface as
// an error so the corruption is visible instead of silently masked.
func TestGetKeyAutoRecoveryRejectsLoneRow(t *testing.T) {
	db := newKeyAutoRecoveryTestDB(t)
	db.Exec(`DELETE FROM system_settings WHERE key='key_auto_recovery_interval_minutes'`)
	if _, _, err := GetKeyAutoRecovery(db); err == nil {
		t.Fatal("expected error for a single-row pair, got nil")
	}
}

func TestUpdateKeyAutoRecoveryReturnsNewSnapshot(t *testing.T) {
	db := newKeyAutoRecoveryTestDB(t)
	s, ver, err := UpdateKeyAutoRecovery(db, 1, false, 15)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if s.Enabled || s.IntervalMinutes != 15 || ver != 2 {
		t.Fatalf("want disabled/15/v2, got %+v v%d", s, ver)
	}
	// Persisted?
	got, gver, err := GetKeyAutoRecovery(db)
	if err != nil || got != s || gver != ver {
		t.Fatalf("read-back mismatch: %+v v%d err=%v", got, gver, err)
	}
}

func TestUpdateKeyAutoRecoveryCASConflict(t *testing.T) {
	db := newKeyAutoRecoveryTestDB(t)
	// First successful update bumps version 1 -> 2.
	if _, _, err := UpdateKeyAutoRecovery(db, 1, false, 60); err != nil {
		t.Fatalf("first update: %v", err)
	}
	// Stale expectedVersion=1 must conflict.
	_, _, err := UpdateKeyAutoRecovery(db, 1, true, 30)
	if !errors.Is(err, errcode.ErrKeyAutoRecoveryConflict) {
		t.Fatalf("want ErrKeyAutoRecoveryConflict, got %v", err)
	}
}
