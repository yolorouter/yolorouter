package database

import (
	"database/sql"
	"testing"

	"github.com/yolorouter/yolorouter/migrations"
)

// The agent-attribution columns (migration 00051), tested in the same two
// postures TestMigration00049 pins the w3c_trace_id column in: what a fresh
// deployment gets from the whole chain on an empty database, and what an
// in-place upgrade of a populated database does to the rows that predate
// the columns.

// TestMigration00051FreshDatabaseCarriesAgentAttributionColumns proves the
// fresh start-up path: the whole migration chain on an empty database must
// create request_logs.agent_client and request_logs.agent_session_id as
// nullable TEXT, defaulting to NULL, and must round-trip written values
// verbatim.
func TestMigration00051FreshDatabaseCarriesAgentAttributionColumns(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}

	for _, column := range []string{"agent_client", "agent_session_id"} {
		colType, notNull, found := sqliteColumnInfo(t, db, "request_logs", column)
		if !found {
			t.Fatalf("expected request_logs.%s column after the full migration chain, not found", column)
		}
		if colType != "TEXT" {
			t.Fatalf("expected %s to be TEXT, got %q", column, colType)
		}
		if notNull {
			t.Fatalf("expected %s to be nullable — rows without a recognized tool must store NULL", column)
		}
	}

	// A row written the way every pre-attribution writer does, omitting
	// both columns, reads NULL on both — absent attribution is NULL, never
	// an empty string.
	res, err := db.Exec(`INSERT INTO request_logs (request_id, model_name, status_code, created_at)
		VALUES ('req-agent-fresh-default', 'm', 200, '2026-01-02 03:04:05')`)
	if err != nil {
		t.Fatalf("insert without agent columns: %v", err)
	}
	defaultID, _ := res.LastInsertId()
	var client, session sql.NullString
	if err := db.QueryRow(`SELECT agent_client, agent_session_id FROM request_logs WHERE id = ?`, defaultID).
		Scan(&client, &session); err != nil {
		t.Fatalf("read default agent columns: %v", err)
	}
	if client.Valid || session.Valid {
		t.Fatalf("omitted attribution = (%q, %q), want (NULL, NULL)", client.String, session.String)
	}

	// Values written by an attribution-aware writer survive unchanged, and
	// the redaction sentinel is never what a writer stores here.
	const (
		wantClient  = "claude-code"
		wantSession = "3f9d2c81-6a54-4d0f-9a3e-2b1c8d7e5a4f"
	)
	if _, err := db.Exec(`INSERT INTO request_logs (request_id, model_name, status_code, agent_client, agent_session_id, created_at)
		VALUES ('req-agent-fresh-attributed', 'm', 200, ?, ?, '2026-01-02 03:04:06')`, wantClient, wantSession); err != nil {
		t.Fatalf("insert with agent columns: %v", err)
	}
	if err := db.QueryRow(`SELECT agent_client, agent_session_id FROM request_logs WHERE request_id = 'req-agent-fresh-attributed'`).
		Scan(&client, &session); err != nil {
		t.Fatalf("read written agent columns: %v", err)
	}
	if !client.Valid || client.String != wantClient {
		t.Fatalf("agent_client round-trip = %+v, want %q", client, wantClient)
	}
	if !session.Valid || session.String != wantSession {
		t.Fatalf("agent_session_id round-trip = %+v, want %q", session, wantSession)
	}
}

// TestMigration00051UpgradeReplayKeepsRequestLogsIntact replays a real
// upgrade: a database holding populated request_logs rows from before the
// columns existed migrates forward and must come out with every row, every
// existing column value, untouched, and both agent columns NULL on all of
// them — attribution means "this caller's tool identified itself", and the
// promotion of recognizable history is a separate task's job (reading the
// stored snapshots), never the migration's. The downgrade direction runs
// too: Down drops both columns with rows present, and the second forward
// replay is lossless, so an operator who rolls back is not stranded.
func TestMigration00051UpgradeReplayKeepsRequestLogsIntact(t *testing.T) {
	db := newMemoryDB(t)
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("RunMigrations failed: %v", err)
	}
	// Back to the pre-attribution schema. The rollback itself exercises
	// the Down: both columns must be gone afterwards.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 50); err != nil {
		t.Fatalf("RollbackTo(50) failed: %v", err)
	}
	for _, column := range []string{"agent_client", "agent_session_id"} {
		if _, _, found := sqliteColumnInfo(t, db, "request_logs", column); found {
			t.Fatalf("%s still present after rolling back to version 50", column)
		}
	}

	// The pre-upgrade world: an owner, a key, a provider, and three
	// request_logs rows in the shapes the table actually stores at version
	// 50 — a fully populated caller row (traced, so the w3c column's
	// carry-over has a value to keep), an unauthenticated audit row, and a
	// vision-fallback sub-call.
	mustExec(t, db, `INSERT INTO users (id, username, password_hash, role, status, is_local, created_at, updated_at)
		VALUES (5, 'boss', 'hash', 'admin', 1, 1, '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	mustExec(t, db, `INSERT INTO api_keys (id, key_hash, key_prefix, status, budget_spent_micros, created_at, updated_at)
		VALUES (11, 'kh-1', 'sk-yr-a', 1, 0, '2026-01-02 00:00:00', '2026-01-02 00:00:00')`)
	mustExec(t, db, `INSERT INTO providers (id, name, provider_type, base_url, created_at, updated_at)
		VALUES (21, 'upgraded-provider', 'openai', 'https://up.example.com', '2026-01-02 03:04:05', '2026-01-02 03:04:05')`)
	mustExec(t, db, `INSERT INTO request_logs (
	    id, request_id, api_key_id, user_id, model_name, provider_id, is_stream, status_code,
	    input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, cost_micros, cost_known,
	    fail_reason, attempts, attempts_detail, duration_ms, created_at,
	    cache_read_saved_micros, cache_write_extra_micros,
	    compress_estimated_tokens_saved, compress_estimated_cost_saved_micros,
	    compress_skip_reason, compressors_applied,
	    request_path, upstream_url, facts_json, source, parent_request_id,
	    settled_input_price, settled_output_price, settled_cache_write_price, settled_cache_read_price,
	    image_pricing_snapshot, image_count, usage_seconds, usage_characters, audio_pricing_snapshot,
	    w3c_trace_id
	) VALUES
	    (101, 'req-full', 11, 5, 'model-x', 21, 1, 200,
	     11, 22, 5, 6, 33, 1,
	     NULL, 2, '[{"provider_name":"upgraded-provider"}]', 44, '2026-01-02 03:04:05',
	     7, 8,
	     9, 10,
	     'skip-reason', 'trim',
	     '/v1/chat/completions', 'https://up.example.com', '{"f":1}', '', '',
	     1.5, 2.5, 3.5, 4.5,
	     NULL, 0, 0, 0, NULL,
	     '4bf92f3577b34da6a3ce929d0e0e4736'),
	    (102, 'req-audit', NULL, NULL, '', NULL, 0, 401,
	     0, 0, 0, 0, 0, 0,
	     'invalid_api_key', 1, NULL, 0, '2026-01-02 03:05:05',
	     0, 0,
	     0, 0,
	     '', '',
	     '', '', '', '', '',
	     NULL, NULL, NULL, NULL,
	     NULL, 0, 0, 0, NULL,
	     NULL),
	    (103, 'req-sub', 11, 5, 'describe-model', 21, 0, 200,
	     3, 4, 0, 0, 55, 1,
	     NULL, 1, NULL, 66, '2026-01-02 03:06:05',
	     0, 0,
	     0, 0,
	     'too_small', '',
	     '/v1/chat/completions', 'https://up.example.com', '', 'vision_fallback', 'req-full',
	     NULL, NULL, NULL, NULL,
	     '{"mode":"image"}', 2, 12, 40, '{"unit":"character"}',
	     NULL)`)

	before := snapshotRequestLogs(t, db)
	if len(before.rows) != 3 {
		t.Fatalf("seed produced %d request_logs rows, want 3", len(before.rows))
	}

	// The upgrade itself.
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-running migrations failed: %v", err)
	}
	afterFirst := snapshotRequestLogs(t, db)

	// The new columns exist now and read NULL on every historical row.
	for _, column := range []string{"agent_client", "agent_session_id"} {
		if _, _, found := sqliteColumnInfo(t, db, "request_logs", column); !found {
			t.Fatalf("%s missing after the upgrade replay", column)
		}
		var attributed int
		if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE ` + column + ` IS NOT NULL`).Scan(&attributed); err != nil {
			t.Fatalf("count non-NULL %s: %v", column, err)
		}
		if attributed != 0 {
			t.Fatalf("expected every pre-column row to read NULL %s, %d rows do not", column, attributed)
		}
	}

	// History is untouched: same rows, same values, column by column.
	assertColumnsCarriedOver(t, before, afterFirst, "forward replay")

	// The downgrade runs against rows this time (the first rollback ran on
	// an empty table), and the second forward replay must be as lossless as
	// the first — a rolled-back operator just migrates again.
	if err := RollbackTo(db, "sqlite", migrations.SQLiteFS, "sqlite", 50); err != nil {
		t.Fatalf("RollbackTo(50) with rows present failed: %v", err)
	}
	for _, column := range []string{"agent_client", "agent_session_id"} {
		if _, _, found := sqliteColumnInfo(t, db, "request_logs", column); found {
			t.Fatalf("%s still present after the second rollback to version 50", column)
		}
	}
	if err := RunMigrations(db, "sqlite", migrations.SQLiteFS, "sqlite"); err != nil {
		t.Fatalf("re-applying migrations after rollback failed: %v", err)
	}
	afterSecond := snapshotRequestLogs(t, db)
	assertColumnsCarriedOver(t, before, afterSecond, "second replay after rollback")

	for _, column := range []string{"agent_client", "agent_session_id"} {
		var attributedAgain int
		if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE ` + column + ` IS NOT NULL`).Scan(&attributedAgain); err != nil {
			t.Fatalf("count non-NULL %s after second replay: %v", column, err)
		}
		if attributedAgain != 0 {
			t.Fatalf("expected NULL %s on every row after the second replay, %d rows do not", column, attributedAgain)
		}
	}
}
