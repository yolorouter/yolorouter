package repository

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/migrations"
	"github.com/yolorouter/yolorouter/pkg/database"
)

// The admin-audit repository's Postgres arm. Every repository test below it
// runs on SQLite; these are the only tests that drive the audit INSERT
// shape, the filter/keyset read paths, the retention sweep, and the key
// suffix backfill engine through the exact GORM models against a real
// Postgres — the backend half of production. Gated on TEST_POSTGRES_DSN the
// same way pkg/database's Postgres suite is: unset means skip, so the suite
// stays green on machines without a server.

// newPostgresRepositoryDB opens TEST_POSTGRES_DSN as a GORM connection
// configured exactly the way production's database.Init configures the
// postgres dialector, wipes the public schema, and applies the full
// postgres migration tree — so what the caller exercises is the real
// schema, not a hand-rolled stand-in. Skips when the variable is unset.
// The connection deliberately does NOT go through database.Init: that call
// assigns a process-global, and these tests must not clobber the SQLite
// one testutil.NewSQLiteDB hands to other tests in the same binary.
func newPostgresRepositoryDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set, skipping Postgres repository test")
	}
	db, err := gorm.Open(postgres.New(postgres.Config{DSN: dsn, PreferSimpleProtocol: true}), &gorm.Config{
		Logger: &database.GormLogger{LogLevel: gormlogger.Silent},
		NowFunc: func() time.Time {
			return time.Now().UTC()
		},
	})
	if err != nil {
		t.Fatalf("open test postgres: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := sqlDB.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset public schema: %v", err)
	}
	if err := database.RunMigrations(sqlDB, "postgres", migrations.PostgresFS, "postgres"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	return db
}

// auditScenarioBase is the fixed instant every seeded row hangs off, so the
// start/end filter edges and the retention boundary are exact rather than
// relative to wall time.
var auditScenarioBase = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

// auditScenarioGrid is the six-row seed grid the query legs run against.
// Every filter dimension crosses: two admins, three actions, two resource
// types, three statuses, a NULL-identity row, one row per interesting time
// offset, and a snapshot username shaped like a spreadsheet formula (what
// the CSV export's injection guard exists for).
func auditScenarioGrid() []model.AdminAuditLog {
	adminA := uint(1)
	adminB := uint(2)
	return []model.AdminAuditLog{
		{CreatedAt: auditScenarioBase.Add(-4 * time.Hour), AdminUserID: nil, AdminUsername: "",
			Action: "auth.login", ResourceType: "auth", Status: model.AuditStatusSuccess},
		{CreatedAt: auditScenarioBase.Add(-3 * time.Hour), AdminUserID: &adminA, AdminUsername: "alice",
			Action: "system_settings.update", ResourceType: "system_setting", ResourceID: "custom-system-prompt",
			Status: model.AuditStatusSuccess},
		{CreatedAt: auditScenarioBase.Add(-2 * time.Hour), AdminUserID: &adminA, AdminUsername: "alice",
			Action: "provider.create", ResourceType: "provider", ResourceID: "7", Status: model.AuditStatusSuccess},
		{CreatedAt: auditScenarioBase.Add(-90 * time.Minute), AdminUserID: &adminB, AdminUsername: "bob",
			Action: "provider.create", ResourceType: "provider", ResourceID: "8", Status: model.AuditStatusDenied},
		{CreatedAt: auditScenarioBase.Add(-30 * time.Minute), AdminUserID: &adminB, AdminUsername: "bob",
			Action: "system_settings.update", ResourceType: "system_setting", ResourceID: "input-compression",
			Status: model.AuditStatusError, ErrorBrief: "http_status=500"},
		{CreatedAt: auditScenarioBase.Add(-10 * time.Minute), AdminUserID: &adminA, AdminUsername: "=SUM(A1:A9)",
			Action: "provider.create", ResourceType: "provider", ResourceID: "9", Status: model.AuditStatusSuccess},
	}
}

// auditScenarioExpectation is the pinned output of
// runAdminAuditRepositoryScenario. The scenario prints one line per
// behavior; the test asserts the SQLite run AND the Postgres run both
// produce exactly these lines, which is simultaneously the absolute
// contract (values are right) and the parity contract (same scenario, same
// expectations, two backends — any backend-specific divergence is a line
// that differs).
var auditScenarioExpectation = []string{
	"create-empty-slice:ok",
	"count-all:6",
	"count-admin-1:3",
	"count-admin-2:2",
	"count-admin-99:0",
	"count-action-provider.create:3",
	"count-resource-type-system_setting:2",
	"count-resource-id-7:1",
	"count-status-denied:1",
	"count-status-error:1",
	"count-status-success:4",
	"count-start-inclusive:5",
	"count-end-exclusive:3",
	"count-combined-admin1-success:3",
	"list-newest-first:6,5,4,3,2,1|total=6",
	"list-page2-size2:4,3|total=6",
	"list-defaults:6,5,4,3,2,1|total=6",
	"list-clamped-size:6,5,4,3,2,1|total=6",
	"keyset-walk-limit4:6,5,4,3,2,1",
	"keyset-filter-success:6,3,2,1",
	"sweep-7days:2",
	"remaining-ids:1,2,3,4,5,6,9",
	"sweep-7days-again:0",
	"sweep-0days:0",
	"sweep-negative-days:0",
	"remaining-ids-final:1,2,3,4,5,6,9",
}

// runAdminAuditRepositoryScenario drives the audit repository's whole
// public surface — batch INSERT, plain count, every filter dimension (and
// their intersection), offset pagination with defaults and clamps, the
// keyset cursor walk, and the retention sweep with its boundary rules —
// against a freshly seeded database, returning one canonical line per
// behavior. Backend-neutral by construction: the identical function runs
// against SQLite and Postgres, so a line emitted differently on one backend
// fails the comparison.
func runAdminAuditRepositoryScenario(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var lines []string
	emit := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	if err := CreateAdminAuditLogs(db, nil); err != nil {
		t.Fatalf("empty-batch insert: %v", err)
	}
	emit("create-empty-slice:ok")

	if err := CreateAdminAuditLogs(db, auditScenarioGrid()); err != nil {
		t.Fatalf("seed audit grid: %v", err)
	}

	count := func(label string, f *AdminAuditLogFilter) {
		t.Helper()
		n, err := CountAdminAuditLogsByFilter(db, f)
		if err != nil {
			t.Fatalf("count %s: %v", label, err)
		}
		emit("count-%s:%d", label, n)
	}
	all, err := CountAdminAuditLogs(db)
	if err != nil {
		t.Fatalf("CountAdminAuditLogs: %v", err)
	}
	emit("count-all:%d", all)

	admin1, admin2, admin99 := uint(1), uint(2), uint(99)
	count("admin-1", &AdminAuditLogFilter{AdminUserID: &admin1})
	count("admin-2", &AdminAuditLogFilter{AdminUserID: &admin2})
	// The NULL-identity row (public auth endpoints before a session
	// exists) must never match an admin filter on either backend.
	count("admin-99", &AdminAuditLogFilter{AdminUserID: &admin99})
	count("action-provider.create", &AdminAuditLogFilter{Action: "provider.create"})
	count("resource-type-system_setting", &AdminAuditLogFilter{ResourceType: "system_setting"})
	count("resource-id-7", &AdminAuditLogFilter{ResourceID: "7"})
	count("status-denied", &AdminAuditLogFilter{Status: model.AuditStatusDenied})
	count("status-error", &AdminAuditLogFilter{Status: model.AuditStatusError})
	count("status-success", &AdminAuditLogFilter{Status: model.AuditStatusSuccess})

	// Start is inclusive (the row exactly at the boundary counts), end is
	// exclusive (the row exactly at the boundary does not) — both edges
	// land on a seeded row on purpose.
	start := auditScenarioBase.Add(-3 * time.Hour)
	end := auditScenarioBase.Add(-90 * time.Minute)
	count("start-inclusive", &AdminAuditLogFilter{StartTime: &start})
	count("end-exclusive", &AdminAuditLogFilter{EndTime: &end})
	count("combined-admin1-success", &AdminAuditLogFilter{AdminUserID: &admin1, Status: model.AuditStatusSuccess})

	listIDs := func(label string, f *AdminAuditLogFilter) []string {
		t.Helper()
		rows, total, err := ListAdminAuditLogsByFilter(db, f)
		if err != nil {
			t.Fatalf("list %s: %v", label, err)
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, fmt.Sprint(r.ID))
		}
		emit("list-%s:%s|total=%d", label, strings.Join(ids, ","), total)
		return ids
	}
	listIDs("newest-first", &AdminAuditLogFilter{Page: 1, PageSize: 20})
	listIDs("page2-size2", &AdminAuditLogFilter{Page: 2, PageSize: 2})
	listIDs("defaults", &AdminAuditLogFilter{})
	listIDs("clamped-size", &AdminAuditLogFilter{Page: 1, PageSize: 500})

	keysetIDs := func(label string, f *AdminAuditLogFilter, limit int) {
		t.Helper()
		var ids []string
		var cursor *AdminAuditCursor
		for {
			rows, err := ListAdminAuditLogsKeyset(db, f, cursor, limit)
			if err != nil {
				t.Fatalf("keyset %s: %v", label, err)
			}
			if len(rows) == 0 {
				break
			}
			for _, r := range rows {
				ids = append(ids, fmt.Sprint(r.ID))
			}
			last := rows[len(rows)-1]
			cursor = &AdminAuditCursor{CreatedAt: last.CreatedAt, ID: last.ID}
			if len(rows) < limit {
				break
			}
		}
		emit("keyset-%s:%s", label, strings.Join(ids, ","))
	}
	keysetIDs("walk-limit4", &AdminAuditLogFilter{}, 4)
	keysetIDs("filter-success", &AdminAuditLogFilter{Status: model.AuditStatusSuccess}, 500)

	// Retention leg. Three more rows around a 7-day cutoff: one clearly
	// older (deleted), one a second older (deleted — expiry is strict),
	// one exactly on the boundary (kept — keeping N days includes day N).
	if err := CreateAdminAuditLogs(db, []model.AdminAuditLog{
		{CreatedAt: auditScenarioBase.Add(-200 * time.Hour), Action: "user.create", Status: model.AuditStatusSuccess},
		{CreatedAt: auditScenarioBase.Add(-7*24*time.Hour - time.Second), Action: "user.status", Status: model.AuditStatusSuccess},
		{CreatedAt: auditScenarioBase.Add(-7 * 24 * time.Hour), Action: "user.role", Status: model.AuditStatusSuccess},
	}); err != nil {
		t.Fatalf("seed retention rows: %v", err)
	}
	remainingIDs := func(label string) {
		t.Helper()
		var ids []int64
		if err := db.Model(&model.AdminAuditLog{}).Order("id ASC").Pluck("id", &ids).Error; err != nil {
			t.Fatalf("pluck remaining ids: %v", err)
		}
		parts := make([]string, 0, len(ids))
		for _, id := range ids {
			parts = append(parts, fmt.Sprint(id))
		}
		emit("%s:%s", label, strings.Join(parts, ","))
	}
	sweep := func(label string, days int) {
		t.Helper()
		n, err := SweepExpiredAdminAuditLogs(db, auditScenarioBase, days, 0)
		if err != nil {
			t.Fatalf("sweep %s: %v", label, err)
		}
		emit("sweep-%s:%d", label, n)
	}
	sweep("7days", 7)
	remainingIDs("remaining-ids")
	sweep("7days-again", 7)
	sweep("0days", 0)
	sweep("negative-days", -3)
	remainingIDs("remaining-ids-final")

	return lines
}

// TestPostgresAdminAuditRepositoryBehavesLikeSQLite runs the identical
// repository scenario against a migrated SQLite database and a migrated
// Postgres database and requires both to produce the pinned expectation
// line for line: the audit surface's behavior on Postgres is the SQLite
// behavior, by assertion rather than by assumption. This is where a
// backend-only quirk in the batch INSERT, a filter predicate, the keyset
// row-value comparison, or the retention sweep's transaction would first
// turn red.
func TestPostgresAdminAuditRepositoryBehavesLikeSQLite(t *testing.T) {
	sqliteLines := runAdminAuditRepositoryScenario(t, testutil.NewSQLiteDB(t))
	pgLines := runAdminAuditRepositoryScenario(t, newPostgresRepositoryDB(t))

	for i, backendLines := range map[string][]string{"sqlite": sqliteLines, "postgres": pgLines} {
		if len(backendLines) != len(auditScenarioExpectation) {
			t.Fatalf("%s scenario produced %d lines, want %d", i, len(backendLines), len(auditScenarioExpectation))
		}
		for j, want := range auditScenarioExpectation {
			if backendLines[j] != want {
				t.Fatalf("%s line %d = %q, want %q", i, j, backendLines[j], want)
			}
		}
	}
}

// TestPostgresProviderKeySuffixBackfillRoundOnPG drives the key-suffix
// backfill engine against real Postgres: the cursor walk splitting a pool
// into bounded rounds, the empty-suffix selection guard that leaves
// already-filled rows alone, the not-derivable row that stays empty and is
// re-selected by the next pass, and the UpdateColumn discipline that never
// touches updated_at or the CAS columns. The deriver is a pure function of
// the ciphertext (last four characters, "corrupt" as the underivable
// sentinel): the decryption itself is backend-independent and covered
// elsewhere — what can break per backend is the SELECT/UPDATE shape.
func TestPostgresProviderKeySuffixBackfillRoundOnPG(t *testing.T) {
	db := newPostgresRepositoryDB(t)

	seeded := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := db.Exec(`INSERT INTO providers (name, base_url, created_at, updated_at)
		VALUES ('suffix-provider', 'https://api.example.com', ?, ?)`, seeded, seeded).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	// Six keys: five with the empty not-yet-derived marker (one of them
	// underivable) and one a previous pass already filled.
	keySeed := []struct {
		label  string
		cipher string
		suffix string
	}{
		{"k1", "cipher-aaaa", ""},
		{"k2", "corrupt", ""},
		{"k3", "cipher-bbbb", ""},
		{"k4", "cipher-cccc", ""},
		{"k5", "cipher-dddd", ""},
		{"k6", "cipher-eeee", "zzzz"},
	}
	for i, k := range keySeed {
		if err := db.Exec(`INSERT INTO provider_keys
			(provider_id, label, encrypted_key, key_prefix, key_suffix, sort_order, test_model,
			 authorized_destination_version, config_version, created_at, updated_at)
			VALUES (1, ?, ?, 'sk-', ?, ?, 'test-model', 1, 7, ?, ?)`,
			k.label, k.cipher, k.suffix, i+1, seeded, seeded).Error; err != nil {
			t.Fatalf("seed key %s: %v", k.label, err)
		}
	}

	derive := func(encrypted string) string {
		if encrypted == "corrupt" {
			return ""
		}
		return encrypted[len(encrypted)-4:]
	}

	// Round 1 from the top of the table, limit 3: scans k1..k3 (k6 is not
	// empty-suffix, so the guard skips it), fills k1 and k3.
	lastID, scanned, updated, err := BackfillProviderKeySuffixRound(db, 0, 3, derive)
	if err != nil {
		t.Fatalf("round 1: %v", err)
	}
	if lastID != 3 || scanned != 3 || updated != 2 {
		t.Fatalf("round 1 = (lastID %d, scanned %d, updated %d), want (3, 3, 2)", lastID, scanned, updated)
	}
	// Round 2 continues past the cursor: k4, k5 — a short batch, so the
	// pass's loop would end here.
	lastID, scanned, updated, err = BackfillProviderKeySuffixRound(db, lastID, 3, derive)
	if err != nil {
		t.Fatalf("round 2: %v", err)
	}
	if lastID != 5 || scanned != 2 || updated != 2 {
		t.Fatalf("round 2 = (lastID %d, scanned %d, updated %d), want (5, 2, 2)", lastID, scanned, updated)
	}
	// The next startup's pass starts from the top again: the only
	// empty-suffix row left is the underivable k2, which it rescans but
	// cannot fill — "stays empty for the next startup" must not turn into
	// "silently forgotten", and the default limit path (limit <= 0 falls
	// back to the engine default) is exercised by the same call.
	lastID, scanned, updated, err = BackfillProviderKeySuffixRound(db, 0, 0, derive)
	if err != nil {
		t.Fatalf("fresh pass round: %v", err)
	}
	if lastID != 2 || scanned != 1 || updated != 0 {
		t.Fatalf("fresh pass round = (lastID %d, scanned %d, updated %d), want (2, 1, 0)", lastID, scanned, updated)
	}

	for _, k := range []struct {
		label string
		want  string
	}{
		{"k1", "aaaa"}, {"k2", ""}, {"k3", "bbbb"}, {"k4", "cccc"}, {"k5", "dddd"}, {"k6", "zzzz"},
	} {
		var got string
		if err := db.Raw(`SELECT key_suffix FROM provider_keys WHERE label = ?`, k.label).Scan(&got).Error; err != nil {
			t.Fatalf("read suffix of %s: %v", k.label, err)
		}
		if got != k.want {
			t.Fatalf("key %s suffix = %q, want %q", k.label, got, k.want)
		}
	}

	// The update discipline: only key_suffix moved. updated_at keeps its
	// seeded instant and config_version (the write-back CAS) is untouched —
	// a display backfill that bumped either would invalidate live keys.
	var (
		updatedAt     time.Time
		configVersion int
	)
	if err := db.Raw(`SELECT updated_at, config_version FROM provider_keys WHERE label = 'k1'`).Row().
		Scan(&updatedAt, &configVersion); err != nil {
		t.Fatalf("read k1 discipline columns: %v", err)
	}
	if !updatedAt.Equal(seeded) {
		t.Fatalf("k1 updated_at = %v, want the seeded %v (UpdateColumn must not bump it)", updatedAt, seeded)
	}
	if configVersion != 7 {
		t.Fatalf("k1 config_version = %d, want the seeded 7", configVersion)
	}

	// A fully converged pool is one cheap empty query: nothing left to
	// select except the underivable row, and a guarded cursor past it
	// finds nothing at all.
	lastID, scanned, updated, err = BackfillProviderKeySuffixRound(db, 5, 200, derive)
	if err != nil {
		t.Fatalf("converged round: %v", err)
	}
	if lastID != 5 || scanned != 0 || updated != 0 {
		t.Fatalf("converged round = (lastID %d, scanned %d, updated %d), want (5, 0, 0)", lastID, scanned, updated)
	}
}
