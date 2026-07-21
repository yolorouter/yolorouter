// Package repository additions for M4: APIKey / APIKeyModel pure data access.
// See design doc .claude/docs/2026-07-19-m4-apikey-design.md §3.
package repository

import (
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

func FindAPIKeyByID(db *gorm.DB, id uint) (*model.APIKey, error) {
	var k model.APIKey
	if err := db.Where("id = ?", id).First(&k).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// applyAPIKeySearch adds the free-text WHERE clause (matched against
// key_prefix / owner_label / remark) when q is non-empty. LOWER() on both
// sides keeps SQLite's case-sensitive LIKE and Postgres's case-sensitive LIKE
// behaving identically — KEY-07 search must not depend on the driver.
func applyAPIKeySearch(tx *gorm.DB, q string) *gorm.DB {
	if q == "" {
		return tx
	}
	// Escape LIKE metacharacters so a search for "100%" or "a_b" matches
	// literally rather than as wildcards (KEY-07). Backslash is the escape
	// char on both SQLite and Postgres.
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q)
	like := "%" + escaped + "%"
	const pattern = "LOWER(key_prefix) LIKE LOWER(?) ESCAPE '\\' OR LOWER(owner_label) LIKE LOWER(?) ESCAPE '\\' OR LOWER(remark) LIKE LOWER(?) ESCAPE '\\'"
	return tx.Where(pattern, like, like, like)
}

// CountAPIKeys returns the total row count matching q (empty q = no filter).
func CountAPIKeys(db *gorm.DB, q string) (int64, error) {
	var total int64
	if err := applyAPIKeySearch(db.Model(&model.APIKey{}), q).Count(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

// SearchAPIKeys returns one page (newest first) of API keys matching q.
func SearchAPIKeys(db *gorm.DB, q string, offset, limit int) ([]model.APIKey, error) {
	var keys []model.APIKey
	if err := applyAPIKeySearch(db.Order("id DESC"), q).Offset(offset).Limit(limit).Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// CreateAPIKey inserts the key row then its allowlist rows in one transaction,
// so a partial write can never leave a key with fewer whitelisted models than
// requested (PRD §6.4.3 requires at least one at create time).
func CreateAPIKey(db *gorm.DB, key *model.APIKey, modelIDs []uint, now time.Time) error {
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
// listing keys (same fix shape as M3's ListModelCandidatesByModelIDs).
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
// and, when modelIDs is non-nil, replaces the allowlist — all in one
// transaction. modelIDs == nil leaves the whitelist unchanged; modelIDs == []
// clears it (PRD §6.4.7 allows an empty whitelist).
func UpdateAPIKey(db *gorm.DB, id uint, updates map[string]interface{}, modelIDs []uint, now time.Time) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// updated_at is always bumped — even a whitelist-only change is a real
		// edit and should refresh the row's last-modified timestamp.
		updates["updated_at"] = now
		if err := tx.Model(&model.APIKey{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		if modelIDs != nil {
			if err := tx.Where("api_key_id = ?", id).Delete(&model.APIKeyModel{}).Error; err != nil {
				return err
			}
			if err := insertAPIKeyModels(tx, id, modelIDs, now); err != nil {
				return err
			}
		}
		return nil
	})
}

// RevokeAPIKey marks a single active key revoked. The WHERE status = active
// clause makes the UPDATE itself idempotent (0 rows if already revoked) —
// deliberate defense-in-depth alongside service.RevokeAPIKey's pre-check
// short-circuit, not redundant: the pre-check avoids the write on the common
// "revoke an already-revoked key" path, this clause keeps the write correct
// even if that pre-check read was stale (PRD §6.4.5 KEY-06).
func RevokeAPIKey(db *gorm.DB, id uint, now time.Time) error {
	return db.Model(&model.APIKey{}).
		Where("id = ? AND status = ?", id, model.APIKeyStatusActive).
		Updates(map[string]interface{}{
			"status":     model.APIKeyStatusRevoked,
			"revoked_at": now,
			"updated_at": now,
		}).Error
}

// FindAPIKeyByHash looks up a key by its SHA-256 hash — the gateway auth path
// (PRD §6.5 step 1). The plaintext is never stored or indexed; the caller
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

// HasAPIKeyModelAccess reports whether modelID is in the key's allowlist
// (PRD §6.5 step 5). Stored by id, so renaming a model does not break
// whitelists (design doc §3 / M4 APIKeyModel). A key with an empty whitelist
// matches nothing — M4 allows creating one, the gateway rejects every call.
func HasAPIKeyModelAccess(db *gorm.DB, apiKeyID, modelID uint) (bool, error) {
	var cnt int64
	if err := db.Model(&model.APIKeyModel{}).
		Where("api_key_id = ? AND model_id = ?", apiKeyID, modelID).Count(&cnt).Error; err != nil {
		return false, err
	}
	return cnt > 0, nil
}
