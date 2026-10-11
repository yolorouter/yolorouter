// The turn engine's Postgres arm: the write path, the candidate query, and
// the live-vs-backfill parity driven against the real Postgres schema —
// the backend half of production. Gated on TEST_POSTGRES_DSN the same way
// the repository package's audit suite is: unset means skip, so the suite
// stays green on machines without a server.
package agentturns

import (
	"context"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/migrations"
	"github.com/yolorouter/yolorouter/pkg/database"
)

// newPostgresTurnsDB opens TEST_POSTGRES_DSN as a GORM connection configured
// exactly the way production's database.Init configures the postgres
// dialector, wipes the public schema, and applies the full postgres
// migration tree — so what the caller exercises is the real schema (the
// 00056 tree included), not a hand-rolled stand-in. Skips when the variable
// is unset.
func newPostgresTurnsDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set, skipping Postgres turns test")
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

// TestTurnEngineOnPostgres drives the whole turn surface against Postgres:
// migration 00056's tree applied above, the live write path, the read
// queries, the raced-duplicate guard (the dialect's unique-violation
// shape), and the backfill parity.
func TestTurnEngineOnPostgres(t *testing.T) {
	db := newPostgresTurnsDB(t)
	base := time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)
	sess := "pg-turns"

	const b1 = `{"system":"S","messages":[{"role":"user","content":"q1"}]}`
	const b2 = `{"system":"S","messages":[{"role":"user","content":"q1"},{"role":"assistant","content":"r1"},{"role":"user","content":"q2"}]}`

	pk1 := seedTurnFixtureRow(t, db, sess, "pg-1", base)
	if err := db.Create(&model.RequestLogBody{RequestID: "pg-1", RequestBody: b1}).Error; err != nil {
		t.Fatalf("body pg-1: %v", err)
	}
	pk2 := seedTurnFixtureRow(t, db, sess, "pg-2", base.Add(time.Minute))
	if err := db.Create(&model.RequestLogBody{RequestID: "pg-2", RequestBody: b2}).Error; err != nil {
		t.Fatalf("body pg-2: %v", err)
	}

	// The live write path, both requests in order.
	fixtureWrite(t, db, sess, "pg-1", pk1, base, b1, "", "")
	fixtureWrite(t, db, sess, "pg-2", pk2, base.Add(time.Minute), b2, "", "")

	// The read queries return the batch with the supplement attributed to
	// the predecessor.
	turns, err := ListTurnsByRequestIDs(db, []string{"pg-1", "pg-2"})
	if err != nil {
		t.Fatalf("ListTurnsByRequestIDs: %v", err)
	}
	if len(turns["pg-1"]) != 4 { // system + user_input + assistant + resend supplement
		t.Fatalf("pg-1 turns = %d rows, want 4 (own batch + resend supplement): %+v", len(turns["pg-1"]), turns["pg-1"])
	}
	meta, err := ListSystemTurnMeta(db, sess)
	if err != nil || len(meta) != 1 || meta[0].SeenAtRequest != "pg-1" {
		t.Fatalf("system meta = %+v (err %v), want one entry from pg-1", meta, err)
	}
	entries, err := ListSystemTurnEntries(db, sess)
	if err != nil || len(entries) != 1 || entries[0].Raw != "S" {
		t.Fatalf("system entries = %+v (err %v), want one entry with S", entries, err)
	}

	// The raced-duplicate guard on the dialect's violation shape.
	fixtureWrite(t, db, sess, "pg-2", pk2, base.Add(time.Minute), b2, "", "")
	// 4 rows on pg-1 (own batch of 3 + the resend supplement) + 2 on pg-2.
	if rows := allTurns(t, db); len(rows) != 6 {
		t.Fatalf("rows after raced duplicate = %d, want 6 (duplicate is a no-op)", len(rows))
	}

	// The backfill parity: wipe, backfill, compare content.
	if err := db.Where("1 = 1").Delete(&model.AgentSessionTurn{}).Error; err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if _, err := BackfillAll(context.Background(), db); err != nil {
		t.Fatalf("BackfillAll: %v", err)
	}
	var after []model.AgentSessionTurn
	if err := db.Order("id ASC").Find(&after).Error; err != nil {
		t.Fatalf("read backfill rows: %v", err)
	}
	if len(after) != 6 {
		t.Fatalf("backfilled rows = %d, want 6 (the live leg produced 6)", len(after))
	}
}
