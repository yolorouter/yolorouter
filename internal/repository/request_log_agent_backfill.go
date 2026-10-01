// The agent-attribution backfill engine: one bounded round of the startup
// pass that promotes the calling tool's name onto historical request_logs
// rows from the masked header snapshot already stored beside them. The
// schedule (when a round happens, when the pass ends) lives in
// internal/agentbackfill; this file owns only the query and the guarded
// update, mirroring how the rolling-retention split its sweep engine here
// from its loop.
package repository

import (
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// DefaultAgentBackfillBatchSize caps how many candidate rows one round
// scans, the same throttle shape as DefaultRequestLogRetentionBatchLimit:
// every round stays a small, quick query instead of one unbounded scan
// competing with the gateway's live write path while a large history
// drains. Rounds simply repeat under a cursor, so any backlog converges.
const DefaultAgentBackfillBatchSize = 500

// AgentClientRecognizer promotes one masked request-header snapshot (the
// JSON request_log_bodies.request_headers stores) to the normalized calling
// tool's name, "" when the signature matches no known tool. A parameter
// rather than a direct call so this package keeps parsing out of its
// dependency graph, exactly like it imports only gateway/capture elsewhere.
type AgentClientRecognizer func(snapshot []byte) string

// backfillCandidate is one row one round is deciding about: the cursor key
// and the stored snapshot the recognizer runs on.
type backfillCandidate struct {
	ID             uint
	RequestHeaders string
}

// BackfillAgentClientRound scans up to limit request_logs rows that still
// have NULL agent_client, that carry a non-empty request_headers snapshot
// in their request_log_bodies row, and whose id is strictly greater than
// afterID (oldest first), runs recognize on each stored snapshot, and
// writes the returned tool name onto every row it names — nothing else.
//
// It returns the highest id the round scanned (the caller's next afterID),
// how many rows the round scanned, and how many it attributed. A short
// batch (scanned < limit) means the pass has reached the end of the table.
//
// Idempotence is structural, not just behavioral. Selection only ever
// considers NULL agent_client rows, and every UPDATE re-checks that guard
// by id, so re-running the pass — or racing it against the gateway's own
// writer — can only ever fill an empty column, never overwrite a value
// another writer put there first. Rows the recognizer cannot name are left
// NULL on purpose: the column's contract is that a value always means the
// caller's tool identified itself, and an unattributable historical row
// must keep reading as unknown rather than be guessed.
//
// Only agent_client is ever written. The tool's session id is deliberately
// out of this engine's reach — the recognizer type returns no session to
// store — because every pre-allowlist snapshot holds the sanitizer's
// redaction sentinel in the session headers, and persisting that literal
// would poison the column with a value that was never an id. (The snapshot's
// headers are not the only carrier, though: the session pass in
// request_log_agent_session_backfill.go runs after this one and recovers
// ids the stored BODY and the unmasked turn-metadata header still carry.)
func BackfillAgentClientRound(db *gorm.DB, afterID uint, limit int, recognize AgentClientRecognizer) (lastID uint, scanned, updated int, err error) {
	if limit <= 0 {
		limit = DefaultAgentBackfillBatchSize
	}
	// INNER JOIN, not EXISTS: the snapshot is the thing being parsed, so a
	// row whose body capture is missing or headerless is not a candidate to
	// skip but a row with nothing to read — it stays NULL until nothing,
	// because there is no capture beside it to re-derive a name from.
	var batch []backfillCandidate
	if err := db.Table("request_logs AS rl").
		Select("rl.id AS id, rb.request_headers AS request_headers").
		Joins("JOIN request_log_bodies rb ON rb.request_id = rl.request_id").
		Where("rl.agent_client IS NULL").
		Where("rl.id > ?", afterID).
		Where("rb.request_headers <> ''").
		Order("rl.id ASC").
		Limit(limit).
		Scan(&batch).Error; err != nil {
		return 0, 0, 0, err
	}
	if len(batch) == 0 {
		return afterID, 0, 0, nil
	}

	for _, row := range batch {
		lastID = row.ID
		scanned++
		client := recognize([]byte(row.RequestHeaders))
		if client == "" {
			continue
		}
		// The IS NULL term is the idempotence guard: between this round's
		// SELECT and this UPDATE another writer may have attributed the row
		// (the gateway's own request path, or a previous pass), and that
		// writer's value wins — RowsAffected 0 means "already answered",
		// not "failed". UpdateColumn because the row has no updated_at to
		// maintain and the backfill must touch exactly this one column.
		res := db.Model(&model.RequestLog{}).
			Where("id = ? AND agent_client IS NULL", row.ID).
			UpdateColumn("agent_client", client)
		if res.Error != nil {
			return lastID, scanned, updated, res.Error
		}
		updated += int(res.RowsAffected)
	}
	return lastID, scanned, updated, nil
}
