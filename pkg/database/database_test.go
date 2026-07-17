package database

import (
	"context"
	"database/sql/driver"
	"path/filepath"
	"testing"
)

// TestInitEnablesForeignKeysAcrossReconnect guards the fix for foreign key
// enforcement silently dropping on connection pool churn: an earlier
// version ran a one-off "PRAGMA foreign_keys = ON" against whichever
// physical connection happened to be open right after Init returned —
// SetMaxOpenConns(1) bounds the pool to one connection at a time, but
// database/sql can still discard and reopen a fresh physical connection
// (e.g. after a driver-reported bad connection), which would silently come
// back with foreign key enforcement off again. The fix moves this into the
// DSN itself (_pragma=foreign_keys(1)), which glebarez/go-sqlite applies
// every time it opens a new physical connection, not just the first one.
func TestInitEnablesForeignKeysAcrossReconnect(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fk.db")

	if err := Init(Config{Driver: "sqlite", SQLitePath: dbPath}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	sqlDB, err := DB.DB()
	if err != nil {
		t.Fatalf("DB.DB(): %v", err)
	}
	defer func() { _ = sqlDB.Close() }()

	assertForeignKeysOn := func(label string) {
		t.Helper()
		var fk int
		if err := sqlDB.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("%s: query foreign_keys pragma: %v", label, err)
		}
		if fk != 1 {
			t.Fatalf("%s: expected foreign_keys=1, got %d", label, fk)
		}
	}

	assertForeignKeysOn("initial connection")

	// Force database/sql to discard the current physical connection and
	// open a fresh one on the next query — simulating exactly the pool
	// churn scenario this fix protects against, rather than just trusting
	// the DSN string looks right. Raw returns whatever the callback
	// returns, so a driver.ErrBadConn result here is the expected outcome
	// (it's what signals database/sql to discard the connection) — if this
	// mechanism silently didn't work, the test below would otherwise just
	// keep re-checking the same still-good original connection and never
	// actually exercise a reconnect at all.
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("get conn: %v", err)
	}
	if rawErr := conn.Raw(func(driverConn any) error {
		return driver.ErrBadConn
	}); rawErr != driver.ErrBadConn {
		t.Fatalf("expected conn.Raw to report driver.ErrBadConn, got: %v", rawErr)
	}
	// database/sql already discards a connection the moment Raw reports it
	// bad, so this Close is just cleanup of an already-dead handle — its
	// own return value ("sql: connection is already closed") isn't
	// meaningful here.
	_ = conn.Close()

	assertForeignKeysOn("after forced reconnect")
}
