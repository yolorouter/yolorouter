// The provider key-suffix backfill engine: one bounded round of the startup
// pass that fills the display-suffix column onto provider_keys rows stored
// before it existed. The schedule (when a round happens, when the pass
// ends) lives in internal/keysuffixbackfill; this file owns only the query
// and the guarded update, mirroring how the agent-attribution backfill
// split its engine here from its task.
package repository

import (
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// DefaultProviderKeySuffixBatchSize caps how many candidate rows one round
// scans — the same throttle shape as DefaultAgentBackfillBatchSize: every
// round stays a small, quick query instead of one unbounded scan competing
// with the live write path while a large key pool drains. Rounds repeat
// under a cursor, so any backlog converges. Decryption runs per row, so a
// smaller default than the request-log engines' 500 keeps each round's
// crypto work short too.
const DefaultProviderKeySuffixBatchSize = 200

// ProviderKeySuffixDeriver promotes one stored ciphertext to the key's
// display suffix, "" when no suffix can be derived (the ciphertext will not
// decrypt, or the plaintext is too short for a suffix that is not the whole
// secret). A parameter rather than a direct decrypt call so this package
// keeps crypto and key-shape policy out of its dependency graph — the task
// package injects decrypt + provider.KeySuffixFor as one function.
type ProviderKeySuffixDeriver func(encryptedKey string) string

// providerKeySuffixCandidate is one row one round is deciding about: the
// cursor key and the ciphertext the deriver runs on. Nothing else is read
// — the engine never selects a column it does not need, and it has no use
// for anything but the ciphertext.
type providerKeySuffixCandidate struct {
	ID           uint
	EncryptedKey string
}

// BackfillProviderKeySuffixRound scans up to limit provider_keys rows
// whose key_suffix is still the empty string (the column's not-yet-derived
// marker) and whose id is strictly greater than afterID (oldest first),
// runs derive on each stored ciphertext, and writes the returned suffix
// onto every row it names — nothing else.
//
// It returns the highest id the round scanned (the caller's next afterID),
// how many rows the round scanned, and how many it filled. A short batch
// (scanned < limit) means the pass has reached the end of the table.
//
// Idempotence is structural, not just behavioral. Selection only ever
// considers empty-suffix rows, and every UPDATE re-checks that guard by
// id, so re-running the pass — or racing it against the request path's own
// key writes — can only ever fill an empty column, never overwrite a value
// another writer put there first. Rows whose suffix cannot be derived are
// left empty on purpose and picked up again by the next startup's pass:
// the empty string reads as "not yet derived", never as "key has no
// suffix", and the engine never guesses a stand-in.
//
// Only key_suffix is ever written, by UpdateColumn: the row's updated_at
// and every CAS/version column must stay untouched — a display backfill
// that bumped config_version would invalidate live keys.
func BackfillProviderKeySuffixRound(db *gorm.DB, afterID uint, limit int, derive ProviderKeySuffixDeriver) (lastID uint, scanned, updated int, err error) {
	if limit <= 0 {
		limit = DefaultProviderKeySuffixBatchSize
	}
	var batch []providerKeySuffixCandidate
	if err := db.Model(&model.ProviderKey{}).
		Select("id, encrypted_key").
		Where("key_suffix = ''").
		Where("id > ?", afterID).
		Order("id ASC").
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
		suffix := derive(row.EncryptedKey)
		if suffix == "" {
			continue
		}
		// The empty-suffix term is the idempotence guard: between this
		// round's SELECT and this UPDATE another writer may have filled the
		// row (the request path's own key writes, or a previous pass), and
		// that writer's value wins — RowsAffected 0 means "already
		// answered", not "failed".
		res := db.Model(&model.ProviderKey{}).
			Where("id = ? AND key_suffix = ''", row.ID).
			UpdateColumn("key_suffix", suffix)
		if res.Error != nil {
			return lastID, scanned, updated, res.Error
		}
		updated += int(res.RowsAffected)
	}
	return lastID, scanned, updated, nil
}
