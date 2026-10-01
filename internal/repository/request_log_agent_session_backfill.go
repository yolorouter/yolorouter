// The agent-session backfill engine: the startup task's SECOND pass, which
// runs after the agent-client pass (request_log_agent_backfill.go) over the
// same table with the same batched-round shape and the same cursor
// discipline. Where that pass promoted the calling tool's NAME from the
// stored header snapshot, this one promotes the tool's own SESSION id from
// the raw data a historical row kept beside it — the verbatim request body
// and the same masked snapshot — onto rows the live write path could not
// populate because their session headers were masked before the capture
// allowlist existed. The schedule stays in internal/agentbackfill; this file
// owns only the candidate query and the guarded update.
package repository

import (
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// agentSessionBackfillClients lists the only tools this pass extracts a
// session for: claude-code stamps it inside the raw request body's
// metadata.user_id, and codex inside the unmasked X-Codex-Turn-Metadata
// header blob (the body's client_metadata blob being the future-shaped
// second source). Every other tool is out of scope even when a session
// header exists — opencode's sessions were captured live, and no other
// tool's stored captures carry a session these rules can recover.
var agentSessionBackfillClients = []string{"claude-code", "codex"}

// AgentSessionExtractor promotes one already-attributed row's stored capture
// — the raw request body plus the masked header snapshot request_log_bodies
// keeps beside it — to the calling tool's own session id, "" when the
// capture carries no session the rules can recover. A parameter for the
// same reason as AgentClientRecognizer: the repository stays free of
// parsing, and the production rules (internal/agentbackfill, outside the
// gateway parity set) inject through this seam with their own table-driven
// suite beside them.
type AgentSessionExtractor func(agentClient string, body, headerSnapshot []byte) string

// sessionBackfillCandidate is one row one round is deciding about: the
// cursor key, the attribution the extractor dispatches on, and the two
// stored captures it reads.
type sessionBackfillCandidate struct {
	ID             uint
	AgentClient    string
	RequestBody    string
	RequestHeaders string
}

// BackfillAgentSessionRound scans up to limit request_logs rows whose
// agent_client is one of agentSessionBackfillClients, whose agent_session_id
// is still NULL, and whose request_log_bodies row captured at least one of
// the raw body or the header snapshot, whose id is strictly greater than
// afterID (oldest first); it runs extract on each row's stored capture and
// writes the returned session id onto every row it names — nothing else.
//
// It returns the highest id the round scanned (the caller's next afterID),
// how many rows the round scanned, and how many it attributed. A short
// batch (scanned < limit) means the pass has reached the end of the table.
//
// Idempotence is structural, exactly like the client pass. Selection only
// ever considers NULL agent_session_id rows, and every UPDATE re-checks
// that guard by id, so re-running the pass — or racing it against any other
// writer — can only ever fill an empty column, never overwrite a value
// another writer put there first.
//
// Rows whose capture yields no session are left NULL on purpose — never the
// empty string. NULL doubles as "not yet scanned" and "scanned, nothing to
// recover", so the residue is rescanned on every startup: when an extraction
// rule later learns a new client format, the rows it unlocks are picked up
// by the next boot for free. An empty-string marker would break that
// contract twice over: the column's meaning is that a non-NULL value was
// always a real caller-supplied id, and the session queries filter on
// IS NOT NULL, so an empty-string row would group into a pseudo-session no
// message page could ever open.
//
// Only agent_session_id is ever written. The row's agent_client is read,
// never touched — attribution is the first pass's business, and a row that
// reaches this engine has already been named.
func BackfillAgentSessionRound(db *gorm.DB, afterID uint, limit int, extract AgentSessionExtractor) (lastID uint, scanned, updated int, err error) {
	if limit <= 0 {
		limit = DefaultAgentBackfillBatchSize
	}
	// INNER JOIN like the client pass, plus the either-carrier term: a row
	// whose capture is missing entirely (no body row beside it, or body and
	// snapshot both empty) has nothing this pass could read, so it is not a
	// candidate to skip but a row with nothing to extract — it stays NULL
	// until nothing, exactly like the headerless rows of the first pass.
	var batch []sessionBackfillCandidate
	if err := db.Table("request_logs AS rl").
		Select("rl.id AS id, rl.agent_client AS agent_client, rb.request_body AS request_body, rb.request_headers AS request_headers").
		Joins("JOIN request_log_bodies rb ON rb.request_id = rl.request_id").
		Where("rl.agent_client IN ?", agentSessionBackfillClients).
		Where("rl.agent_session_id IS NULL").
		Where("rl.id > ?", afterID).
		Where("rb.request_body <> '' OR rb.request_headers <> ''").
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
		session := extract(row.AgentClient, []byte(row.RequestBody), []byte(row.RequestHeaders))
		if session == "" {
			continue
		}
		// The IS NULL term is the idempotence guard, the client pass's
		// counterpart: between this round's SELECT and this UPDATE another
		// writer may have filled the session (the gateway's own request
		// path, or a previous pass), and that writer's value wins —
		// RowsAffected 0 means "already answered", not "failed".
		// UpdateColumn because the backfill must touch exactly this one
		// column.
		res := db.Model(&model.RequestLog{}).
			Where("id = ? AND agent_session_id IS NULL", row.ID).
			UpdateColumn("agent_session_id", session)
		if res.Error != nil {
			return lastID, scanned, updated, res.Error
		}
		updated += int(res.RowsAffected)
	}
	return lastID, scanned, updated, nil
}
