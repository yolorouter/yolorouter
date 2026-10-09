package keysuffixbackfill

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/migrations"
)

// captureLogs swaps the package's logging seams for collectors, runs body,
// and returns every line the task emitted. The seams exist exactly for
// this: the zero-plaintext rule is asserted against the task's real log
// output, not against a promise in a comment.
func captureLogs(t *testing.T, body func()) []string {
	t.Helper()
	var lines []string
	oldInfo, oldWarn := logInfo, logWarn
	logInfo = func(msg string, _ ...zap.Field) { lines = append(lines, msg) }
	logWarn = func(msg string, _ ...zap.Field) { lines = append(lines, msg) }
	t.Cleanup(func() { logInfo, logWarn = oldInfo, oldWarn })
	body()
	return lines
}

// seedLegacyKey inserts one pre-column-style row: real ciphertext, empty
// suffix — the exact residue the startup pass exists to drain. The seeded
// timestamp is returned so the test can pin that the backfill touches
// none of the row's other columns.
func seedLegacyKey(t *testing.T, db *gorm.DB, providerID uint, order int, label, plaintext string) time.Time {
	t.Helper()
	ciphertext, err := testutil.ProviderSecrets().Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt legacy key: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	key := &model.ProviderKey{ProviderID: providerID, Label: label,
		EncryptedKey: ciphertext, KeyPrefix: "", KeySuffix: "",
		SortOrder: order, TestModel: "gpt-4o-mini",
		ManagementStatus:             model.ProviderKeyStatusDisabled,
		VerificationStatus:           model.VerificationStatusUntested,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 0,
		CreatedAt: now, UpdatedAt: now}
	if err := db.Create(key).Error; err != nil {
		t.Fatalf("seed legacy key %s: %v", label, err)
	}
	return now
}

// findKeyByLabel reloads one seeded row by its label.
func findKeyByLabel(t *testing.T, db *gorm.DB, label string) (model.ProviderKey, error) {
	t.Helper()
	var key model.ProviderKey
	err := db.Where("label = ?", label).First(&key).Error
	return key, err
}

// assertNoPlaintext is the reverse assertion the case's red line demands:
// none of the planted key plaintexts may appear anywhere in the given
// text — a leak into a log line or a migration file fails here.
func assertNoPlaintext(t *testing.T, where string, text string, plaintexts ...string) {
	t.Helper()
	for _, plaintext := range plaintexts {
		if plaintext != "" && strings.Contains(text, plaintext) {
			t.Errorf("key plaintext leaked into %s:\n%s", where, text)
		}
	}
}

// TestBackfillFillsLegacyKeysWithoutPlaintextTraces covers the legacy
// leg: keys stored before the column existed get their suffix derived by one
// pass, in-process, and the zero-plaintext rule holds everywhere the pass
// could have written — the filled column carries exactly four display
// characters, the emitted log lines carry none of the plaintexts, and
// neither migration tree's SQL files contain them (the DDL a backfill
// runs alongside must never be a carrier either).
func TestBackfillFillsLegacyKeysWithoutPlaintextTraces(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	provider := &model.Provider{Name: "legacy-pool", ProviderType: "openai",
		BaseURL: "https://legacy.example.com", ManagementStatus: model.ProviderStatusEnabled,
		DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	first := "sk-legacy-alpha-credential-aa11"
	second := "sk-legacy-beta-credential-bb22"
	seededAt1 := seedLegacyKey(t, db, provider.ID, 1, "old-1", first)
	seededAt2 := seedLegacyKey(t, db, provider.ID, 2, "old-2", second)

	task := NewTask(Config{DB: db, Secrets: testutil.ProviderSecrets(), BatchSize: 1, BatchDelay: 0})
	lines := captureLogs(t, func() { task.Run(context.Background()) })

	var keys []model.ProviderKey
	if err := db.Order("id ASC").Find(&keys).Error; err != nil {
		t.Fatalf("reload keys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("both legacy keys must survive the pass, got %d", len(keys))
	}
	if keys[0].KeySuffix != "aa11" || keys[1].KeySuffix != "bb22" {
		t.Errorf("backfilled suffixes = %q / %q, want aa11 / bb22", keys[0].KeySuffix, keys[1].KeySuffix)
	}
	// The backfill touches exactly one column: the version/CAS and
	// timestamp columns a display pass has no business bumping.
	if !keys[0].UpdatedAt.Equal(seededAt1) || !keys[1].UpdatedAt.Equal(seededAt2) {
		t.Errorf("backfill must not touch updated_at (got %v / %v, seeded %v / %v)",
			keys[0].UpdatedAt, keys[1].UpdatedAt, seededAt1, seededAt2)
	}
	if keys[0].ConfigVersion != 1 {
		t.Errorf("backfill must not bump config_version, got %d", keys[0].ConfigVersion)
	}

	// Reverse assertions, red-line shape: zero plaintext in the log output
	// the pass emitted, zero plaintext in every migration file on either
	// tree.
	for _, line := range lines {
		assertNoPlaintext(t, "backfill log output", line, first, second)
	}
	for _, tree := range []struct {
		name string
		fsys fs.FS
	}{{"sqlite", migrations.SQLiteFS}, {"postgres", migrations.PostgresFS}} {
		err := fs.WalkDir(tree.fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".sql") {
				return err
			}
			raw, readErr := fs.ReadFile(tree.fsys, path)
			if readErr != nil {
				return readErr
			}
			assertNoPlaintext(t, tree.name+" migration "+path, string(raw), first, second)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s migration tree: %v", tree.name, err)
		}
	}

	// The pass is a bounded walk, not a loop: a second run finds the
	// residue drained and emits nothing.
	again := captureLogs(t, func() { task.Run(context.Background()) })
	if len(again) != 0 {
		t.Errorf("a converged pass must log nothing, got %v", again)
	}
	if err := db.Order("id ASC").Find(&keys).Error; err != nil {
		t.Fatalf("reload keys after second pass: %v", err)
	}
	if keys[0].KeySuffix != "aa11" || keys[1].KeySuffix != "bb22" {
		t.Errorf("second pass must not disturb filled suffixes, got %q / %q", keys[0].KeySuffix, keys[1].KeySuffix)
	}
}

// TestBackfillLeavesUndecryptableRowsEmpty pins the failure posture: a
// row whose ciphertext will not decrypt keeps the not-yet-derived marker
// (the next startup rescans it), the pass says so with a count — and with
// nothing else: no id list, no error text, no material of any kind.
func TestBackfillLeavesUndecryptableRowsEmpty(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	provider := &model.Provider{Name: "corrupt-pool", ProviderType: "openai",
		BaseURL: "https://corrupt.example.com", ManagementStatus: model.ProviderStatusEnabled,
		DestinationVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(provider).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	broken := &model.ProviderKey{ProviderID: provider.ID, Label: "broken",
		EncryptedKey: "not-a-valid-ciphertext", KeySuffix: "", SortOrder: 1, TestModel: "gpt-4o-mini",
		ManagementStatus: model.ProviderKeyStatusDisabled, VerificationStatus: model.VerificationStatusUntested,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(broken).Error; err != nil {
		t.Fatalf("seed broken key: %v", err)
	}

	task := NewTask(Config{DB: db, Secrets: testutil.ProviderSecrets()})
	lines := captureLogs(t, func() { task.Run(context.Background()) })

	key, err := findKeyByLabel(t, db, "broken")
	if err != nil {
		t.Fatalf("reload broken key: %v", err)
	}
	if key.KeySuffix != "" {
		t.Errorf("an undecryptable row must keep the empty marker, got %q", key.KeySuffix)
	}
	warned := false
	for _, line := range lines {
		if strings.Contains(line, "could not derive") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the pass must warn about underivable rows (count only), got %v", lines)
	}
}
