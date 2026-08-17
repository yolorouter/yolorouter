// Package repository provides APIKey / APIKeyModel data access. Most functions
// are pure storage; UpdateAPIKey additionally enforces a couple of
// resulting-state invariants that the DB can't express and that need the
// transaction's row lock to check atomically (see its doc comment).
package repository

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// ErrEmptyCustomAllowlist is returned by UpdateAPIKey when an update would
// leave a custom-scope key (allow_all_models=false) with an explicitly-empty
// allowlist — such a key could call nothing. The service translates it to the
// caller-facing errcode; this package stays storage-only.
var ErrEmptyCustomAllowlist = errors.New("custom scope requires at least one model")

// ErrAPIKeyOwnerMissing is returned by CreateAPIKey when the key carries no
// owning user. The column's DEFAULT 0 exists only so the migration could add
// it NOT NULL; the application must never persist an ownerless key — GORM
// writes every mapped column, so a caller that forgot to set UserID would
// otherwise insert 0 silently and the key would belong to no one's view.
var ErrAPIKeyOwnerMissing = errors.New("api key requires an owning user")

// validateCSPInvariants is the per-key custom-system-prompt resulting-state
// rule: when the override is on and CSP is enabled, the key must carry its own
// non-empty prompt text — otherwise it toggles CSP on with nothing to send.
// Centralized so CreateAPIKey's initial-state guard and UpdateAPIKey's
// under-lock re-read can't drift apart.
func validateCSPInvariants(override, enabled bool, text string) error {
	if override && enabled && text == "" {
		return errcode.ErrCustomSystemPromptEmpty
	}
	return nil
}

func FindAPIKeyByID(db *gorm.DB, id uint) (*model.APIKey, error) {
	var k model.APIKey
	if err := db.Where("id = ?", id).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// APIKeyFilter is the set of list filters applied together (AND). All fields
// are optional — an empty field is a no-op. Now anchors the status filter's
// expiry comparison and must be supplied by the caller (same clock the display
// status is computed against).
type APIKeyFilter struct {
	Query  string
	Status string
	// UserID narrows to keys owned by one account; nil = no constraint —
	// the same pointer convention RequestLogFilter uses.
	UserID *uint
	Now    time.Time
}

// escapeLike escapes the LIKE metacharacters so a search for "100%" or "a_b"
// matches literally rather than as wildcards. Backslash is the escape char on
// both SQLite and Postgres. Shared by the two pattern wrappers below.
func escapeLike(q string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
}

// likeContainsPattern wraps q in a LIKE "contains" pattern.
func likeContainsPattern(q string) string {
	return "%" + escapeLike(q) + "%"
}

// likePrefixPattern is the starts-with sibling: anchored at the start.
func likePrefixPattern(q string) string {
	return escapeLike(q) + "%"
}

// applyAPIKeyFilters ANDs together the free-text search, the owner filter and
// the display-status filter. LOWER() on both sides keeps SQLite's and
// Postgres's case-sensitive LIKE behaving identically — search must not depend
// on the driver.
func applyAPIKeyFilters(tx *gorm.DB, f APIKeyFilter) *gorm.DB {
	// Free-text search matches the key prefix or remark.
	if f.Query != "" {
		like := likeContainsPattern(f.Query)
		tx = tx.Where("LOWER(key_prefix) LIKE LOWER(?) ESCAPE '\\' OR LOWER(remark) LIKE LOWER(?) ESCAPE '\\'", like, like)
	}
	if f.UserID != nil {
		tx = tx.Where("user_id = ?", *f.UserID)
	}
	// Status filter mirrors computeAPIKeyDisplayStatus exactly, including its
	// precedence: revoked > expired > budget-exhausted > active.
	switch f.Status {
	case "revoked":
		tx = tx.Where("status = ?", model.APIKeyStatusRevoked)
	case "expired":
		tx = tx.Where("status = ? AND expires_at IS NOT NULL AND expires_at < ?", model.APIKeyStatusActive, f.Now)
	case "budget_exhausted":
		tx = tx.Where(
			"status = ? AND (expires_at IS NULL OR expires_at >= ?) AND budget_limit_micros IS NOT NULL AND budget_spent_micros >= budget_limit_micros",
			model.APIKeyStatusActive, f.Now,
		)
	case "active":
		tx = tx.Where(
			"status = ? AND (expires_at IS NULL OR expires_at >= ?) AND (budget_limit_micros IS NULL OR budget_spent_micros < budget_limit_micros)",
			model.APIKeyStatusActive, f.Now,
		)
	}
	return tx
}

// CountAPIKeys returns the total row count matching the filter.
func CountAPIKeys(db *gorm.DB, f APIKeyFilter) (int64, error) {
	var total int64
	if err := applyAPIKeyFilters(db.Model(&model.APIKey{}), f).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// SearchAPIKeys returns one page (newest first) of API keys matching the filter.
func SearchAPIKeys(db *gorm.DB, f APIKeyFilter, offset, limit int) ([]model.APIKey, error) {
	var keys []model.APIKey
	if err := applyAPIKeyFilters(db.Order("id DESC"), f).Offset(offset).Limit(limit).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// CreateAPIKey inserts the key row then its allowlist rows in one transaction,
// so a partial write can never leave a key with fewer whitelisted models than
// requested (at least one is required at create time). The CSP initial-state
// guard runs before the insert: override && enabled with empty text would
// produce a key that toggles CSP on but has nothing to send.
func CreateAPIKey(db *gorm.DB, key *model.APIKey, modelIDs []uint, now time.Time) error {
	if key.UserID == 0 {
		return ErrAPIKeyOwnerMissing
	}
	if err := validateCSPInvariants(key.CustomSystemPromptEnabledOverride, key.CustomSystemPromptEnabled, key.CustomSystemPrompt); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		key.CreatedAt = now
		key.UpdatedAt = now
		if err := tx.Create(key).Error; err != nil {
			return err
		}
		return insertAPIKeyModels(tx, key.ID, modelIDs, now)
	})
}

// insertAPIKeyModels bulk-inserts the allowlist rows for one key. Empty slice
// is a no-op (UpdateAPIKey uses this when clearing a whitelist).
func insertAPIKeyModels(tx *gorm.DB, apiKeyID uint, modelIDs []uint, now time.Time) error {
	if len(modelIDs) == 0 {
		return nil
	}
	rows := make([]model.APIKeyModel, 0, len(modelIDs))
	for _, mid := range modelIDs {
		rows = append(rows, model.APIKeyModel{APIKeyID: apiKeyID, ModelID: mid, CreatedAt: now})
	}
	return tx.Create(&rows).Error
}

// FindAPIKeyModelIDs returns the model_id allowlist for one key.
func FindAPIKeyModelIDs(db *gorm.DB, apiKeyID uint) ([]uint, error) {
	var ids []uint
	if err := db.Model(&model.APIKeyModel{}).Where("api_key_id = ?", apiKeyID).
		Order("model_id ASC").Pluck("model_id", &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// FindAPIKeyModelsByAPIKeyIDs batches the N+1 of per-key allowlist lookup when
// listing keys (the same fix shape used elsewhere, e.g. ListModelCandidatesByModelIDs).
func FindAPIKeyModelsByAPIKeyIDs(db *gorm.DB, apiKeyIDs []uint) ([]model.APIKeyModel, error) {
	if len(apiKeyIDs) == 0 {
		return nil, nil
	}
	var rows []model.APIKeyModel
	if err := db.Where("api_key_id IN ?", apiKeyIDs).
		Order("api_key_id ASC, model_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateAPIKey applies a sparse column update (only keys present in updates)
// and reconciles the allowlist — all in one transaction. modelIDs == nil
// leaves the allowlist unchanged; modelIDs == [] clears it; a non-nil slice
// replaces it. Three invariants are enforced here rather than in the service,
// because only this layer sees the *resulting* state atomically: the column
// update takes a row lock, the flag is re-read under that lock, and the
// decision is made on what the key will actually end up as, not on the request
// shape. (1) An all-models key owns no allowlist rows. (2) A custom key must
// end up with a non-empty allowlist — otherwise it could call nothing; this
// covers every path that lands there, including a sparse PATCH that flips
// allow_all_models off without supplying ids. Returns ErrEmptyCustomAllowlist
// when the second invariant would be violated (rolls back the whole update).
// (3) A key with the CSP override on and CSP enabled must carry non-empty
// prompt text — otherwise it toggles CSP on with nothing to send; this is
// checked on every exit path so a combined PATCH that rewrites the allowlist
// or flips scope in the same call as the CSP fields can't bypass it. Returns
// errcode.ErrCustomSystemPromptEmpty when the third invariant would be
// violated. scopeChanged tells the second check whether this update touched
// allow_all_models (the caller knows; the repo doesn't probe the updates map
// for it) — a scope flip to custom with ids omitted must re-validate the
// inherited allowlist, an unrelated field edit must not.
//
// expectedUpdatedAt optionally enables compare-and-set: when non-nil the
// UPDATE is qualified with `AND updated_at = ?`, and 0 rows affected means
// another writer committed first (the row's updated_at no longer matches the
// snapshot the caller read) → errcode.ErrAPIKeyConflict. A nil pointer
// disables CAS entirely (the legacy behavior used by EditKeyModal/CreateKeyModal
// and every path that doesn't carry an authoritative GET snapshot) — those
// callers don't have a fresher-than-the-list baseline to compare against, so a
// CAS check there would only false-positive on benign concurrent edits. The
// row's updated_at column is the CAS token: every UpdateAPIKey bumps it under
// the row lock the same UPDATE takes, so a concurrent writer that committed
// between the caller's GET and this UPDATE is observable as a mismatch.
func UpdateAPIKey(db *gorm.DB, id uint, updates map[string]interface{}, modelIDs []uint, scopeChanged bool, now time.Time, expectedUpdatedAt *time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// updated_at is always bumped — even a whitelist-only change is a real
		// edit and should refresh the row's last-modified timestamp. This UPDATE
		// also locks the row for the rest of the transaction.
		updates["updated_at"] = now
		// Qualify the UPDATE with the CAS predicate when the caller supplied an
		// expected updated_at. RowsAffected == 0 under CAS means the row was
		// modified after the caller's GET — surface a conflict rather than
		// silently overwriting the newer state. Without CAS the UPDATE is
		// unconditional (matching the pre-CAS behavior): the service layer has
		// already checked existence, so 0 rows is not a distinguishable signal
		// here.
		query := tx.Model(&model.APIKey{}).Where("id = ?", id)
		if expectedUpdatedAt != nil {
			query = query.Where("updated_at = ?", *expectedUpdatedAt)
		}
		res := query.Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if expectedUpdatedAt != nil && res.RowsAffected == 0 {
			return errcode.ErrAPIKeyConflict
		}
		// checkCSP re-reads the merged CSP columns under the row lock the UPDATE
		// just took, so every exit path decides on the *resulting* row rather
		// than the request shape. override=false inherits the global default and
		// never uses this key's own text, so only override && enabled requires
		// non-empty text. A sparse PATCH that sets enabled=true alone can't be
		// caught by validating the request fields in isolation (the prior text
		// may be empty); this re-read sees the merged result. The three allowlist
		// branches below (all-models clear, explicit allowlist replace, and the
		// unchanged-allowlist fall-through) all converge on this single tail
		// call, so a combined PATCH that flips scope or rewrites the allowlist in
		// the same call as the CSP fields can't bypass the guard via an early
		// return.
		checkCSP := func() error {
			var csp model.APIKey
			if err := tx.Select("custom_system_prompt_enabled_override, custom_system_prompt_enabled, custom_system_prompt").
				Where("id = ?", id).First(&csp).Error; err != nil {
				return err
			}
			return validateCSPInvariants(csp.CustomSystemPromptEnabledOverride, csp.CustomSystemPromptEnabled, csp.CustomSystemPrompt)
		}
		// Re-read the flag under the lock the UPDATE just took, so the effective
		// value includes any allow_all_models change we just wrote and can't race
		// a concurrent scope change. The branches below are mutually exclusive
		// (if/else-if/else) so the tail checkCSP runs exactly once per update
		// after every happy path; the ErrEmptyCustomAllowlist cases short-circuit
		// before it (they leave nothing for CSP to say).
		var k model.APIKey
		if err := tx.Select("allow_all_models").Where("id = ?", id).First(&k).Error; err != nil {
			return err
		}
		if k.AllowAllModels {
			// All-models keys own no allowlist rows: clear any that exist and
			// ignore whatever the caller supplied.
			if err := tx.Where("api_key_id = ?", id).Delete(&model.APIKeyModel{}).Error; err != nil {
				return err
			}
		} else if modelIDs != nil {
			// Custom scope, explicit allowlist replacement. Empty slice is the
			// "clear" sentinel and is rejected here — a custom key must keep at
			// least one model.
			if len(modelIDs) == 0 {
				return ErrEmptyCustomAllowlist
			}
			if err := tx.Where("api_key_id = ?", id).Delete(&model.APIKeyModel{}).Error; err != nil {
				return err
			}
			if err := insertAPIKeyModels(tx, id, modelIDs, now); err != nil {
				return err
			}
		} else {
			// Allowlist left unchanged. If this update just switched scope to
			// custom (the flag is in updates), the rows it inherits must be
			// non-empty — a true->false flip supplying no ids would otherwise
			// leave an empty custom key. An update that doesn't touch scope
			// skips this, so a sparse PATCH (e.g. remark-only) never re-validates
			// rows it isn't changing.
			if scopeChanged {
				var cnt int64
				if err := tx.Model(&model.APIKeyModel{}).Where("api_key_id = ?", id).Count(&cnt).Error; err != nil {
					return err
				}
				if cnt == 0 {
					return ErrEmptyCustomAllowlist
				}
			}
		}
		return checkCSP()
	})
}

// RevokeAPIKey marks a single active key revoked. The WHERE status = active
// clause makes the UPDATE itself idempotent (0 rows if already revoked) —
// deliberate defense-in-depth alongside service.RevokeAPIKey's pre-check
// short-circuit, not redundant: the pre-check avoids the write on the common
// "revoke an already-revoked key" path, this clause keeps the write correct
// even if that pre-check read was stale.
func RevokeAPIKey(db *gorm.DB, id uint, now time.Time) error {
	return db.Model(&model.APIKey{}).
		Where("id = ? AND status = ?", id, model.APIKeyStatusActive).
		Updates(map[string]interface{}{
			"status":     model.APIKeyStatusRevoked,
			"revoked_at": now,
			"updated_at": now,
		}).Error
}

// FindAPIKeyByHash looks up a key by its SHA-256 hash — the gateway auth path.
// The plaintext is never stored or indexed; the caller
// hashes the bearer token and looks the row up by hash. Returns
// gorm.ErrRecordNotFound for an unknown key (the service layer maps that to
// ErrAPIKeyInvalid — never "not found", to avoid leaking which keys exist).
func FindAPIKeyByHash(db *gorm.DB, hash string) (*model.APIKey, error) {
	var k model.APIKey
	if err := db.Where("key_hash = ?", hash).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// HasAPIKeyModelAccess reports whether modelID is in the key's allowlist.
// Stored by id, so renaming a model does not break allowlists. An empty
// allowlist matches nothing; the gateway only consults this for custom-scope
// keys (all-models keys bypass it upstream), and UpdateAPIKey/CreateAPIKey keep
// a custom key's allowlist non-empty — so a false result here is the
// defense-in-depth floor, not an expected steady state.
func HasAPIKeyModelAccess(db *gorm.DB, apiKeyID, modelID uint) (bool, error) {
	var cnt int64
	if err := db.Model(&model.APIKeyModel{}).
		Where("api_key_id = ? AND model_id = ?", apiKeyID, modelID).Count(&cnt).Error; err != nil {
		return false, err
	}
	return cnt > 0, nil
}

// callableAPIKeyScope narrows to keys that can currently make a request —
// the same predicate as the "active" display-status filter above: not
// revoked, not expired, budget not exhausted. Impact previews use it so a
// "this key is affected" claim never counts a key that could not call anyway.
func callableAPIKeyScope(db *gorm.DB, now time.Time) *gorm.DB {
	return db.Where(
		"api_keys.status = ? AND (api_keys.expires_at IS NULL OR api_keys.expires_at >= ?) AND (api_keys.budget_limit_micros IS NULL OR api_keys.budget_spent_micros < api_keys.budget_limit_micros)",
		model.APIKeyStatusActive, now,
	)
}

// ListCallableAPIKeysAllowlisting returns the keys that can currently call
// and whose allowlist explicitly names the model — the keys an operator
// breaks by disabling it.
func ListCallableAPIKeysAllowlisting(db *gorm.DB, modelID uint, now time.Time) ([]model.APIKey, error) {
	return ListCallableAPIKeysAllowlistingAny(db, []uint{modelID}, now)
}

// ListCallableAPIKeysAllowlistingAny is the multi-model form: keys that can
// currently call and allowlist at least one of the models. Each key appears
// once however many of the models it names. Empty input returns nil without
// querying.
func ListCallableAPIKeysAllowlistingAny(db *gorm.DB, modelIDs []uint, now time.Time) ([]model.APIKey, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	var keys []model.APIKey
	err := callableAPIKeyScope(db.Model(&model.APIKey{}), now).
		Distinct("api_keys.*").
		Joins("JOIN api_key_models ON api_key_models.api_key_id = api_keys.id").
		Where("api_key_models.model_id IN ?", modelIDs).
		Order("api_keys.id ASC").
		Find(&keys).Error
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// CountCallableAllowAllAPIKeys counts keys that can currently call any
// enabled model. They are affected by every model action, so impact previews
// report them as a count rather than by name.
func CountCallableAllowAllAPIKeys(db *gorm.DB, now time.Time) (int64, error) {
	var cnt int64
	err := callableAPIKeyScope(db.Model(&model.APIKey{}), now).
		Where("api_keys.allow_all_models = ?", true).
		Count(&cnt).Error
	return cnt, err
}

// FindAPIKeyOwnerStatus returns the account status of the key's owning
// user in one indexed lookup. Split from FindAPIKeyByHash rather than
// folded into it as a JOIN so the key lookup's error contract
// (gorm.ErrRecordNotFound = unknown credential) stays untouched; a key
// whose owner row is missing is impossible under the NOT NULL foreign
// key and surfaces as an error here rather than as "unknown key".
func FindAPIKeyOwnerStatus(db *gorm.DB, userID uint) (int, error) {
	var status int
	err := db.Model(&model.User{}).Where("id = ?", userID).
		Select("status").Take(&status).Error
	return status, err
}
