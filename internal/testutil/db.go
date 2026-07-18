// Package testutil provides shared test helpers that need a real migrated
// database connection. Only ever imported from _test.go files across
// internal/repository, internal/service, internal/handler, and
// internal/middleware — never from production code.
package testutil

import (
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/migrations"
	"github.com/yolorouter/yolorouter-ce/pkg/database"
)

// NewSQLiteDB opens a fresh temp-file SQLite database, runs every embedded
// migration against it via the same database.RunMigrations call production
// startup uses (see cmd/yolorouter-ce/serve.go), and returns the resulting
// *gorm.DB. Each call gets its own temp file (t.TempDir()).
//
// database.Init sets the package-level database.DB variable rather than
// returning a connection directly (see pkg/database/database.go) — this
// helper captures that value into a local variable immediately after Init
// returns, so a later call from a different test (which reassigns the same
// global) can't retroactively affect an already-captured *gorm.DB. Do not
// mark tests using this helper t.Parallel(): concurrent calls would race on
// the database.DB global itself while it's being read here.
func NewSQLiteDB(t *testing.T) *gorm.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	if err := database.Init(database.Config{Driver: "sqlite", SQLitePath: dbPath}); err != nil {
		t.Fatalf("database.Init failed: %v", err)
	}
	gormDB := database.DB

	sqlDB, err := gormDB.DB()
	if err != nil {
		t.Fatalf("get underlying *sql.DB: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := database.RunMigrations(sqlDB, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	return gormDB
}
