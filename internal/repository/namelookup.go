// Package repository: batch id→display-name lookups.
//
// Every report and list view enriches rows post-fetch with human-readable
// names (provider name, owning account's username, key prefix). The shape
// is always the same — collect the distinct non-nil ids, one narrow IN
// query, build a map, missing rows simply absent — and it used to be
// re-implemented at every call site. This module is that shape's single
// home: the interface is three lookup functions and one id collector; the
// dedupe, empty-slice guard and column narrowing live behind them once.
package repository

import (
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// CollectPtrIDs gathers the distinct non-nil ids from a row slice. The
// getter receives a pointer so callers don't copy rows; nil ids (a report's
// unattributed bucket) are skipped.
func CollectPtrIDs[T any](items []T, id func(*T) *uint) []uint {
	ids := make([]uint, 0, len(items))
	seen := make(map[uint]struct{}, len(items))
	for i := range items {
		p := id(&items[i])
		if p == nil {
			continue
		}
		if _, ok := seen[*p]; ok {
			continue
		}
		seen[*p] = struct{}{}
		ids = append(ids, *p)
	}
	return ids
}

// backfillByPtrID is the write-back half of the lookup shape: collect the
// distinct non-nil ids, run one batch lookup, then assign each row its
// looked-up value. Rows with a nil id (a report's unattributed bucket) are
// left untouched; rows whose id is absent from the lookup result receive
// V's zero value, so a hard-deleted referent renders as empty rather than
// failing the view.
func backfillByPtrID[R any, V any](db *gorm.DB, rows []R, id func(*R) *uint, lookup func(*gorm.DB, []uint) (map[uint]V, error), assign func(*R, V)) error {
	values, err := lookup(db, CollectPtrIDs(rows, id))
	if err != nil {
		return err
	}
	for i := range rows {
		p := id(&rows[i])
		if p == nil {
			continue
		}
		assign(&rows[i], values[*p])
	}
	return nil
}

// FindProviderNamesByIDs batch-resolves provider ids to display names.
// Hard-deleted / unknown ids are absent from the map — callers render an
// empty name rather than failing the whole view.
func FindProviderNamesByIDs(db *gorm.DB, ids []uint) (map[uint]string, error) {
	names := make(map[uint]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	var providers []model.Provider
	if err := db.Select("id", "name").Where("id IN ?", ids).Find(&providers).Error; err != nil {
		return nil, err
	}
	for _, p := range providers {
		names[p.ID] = p.Name
	}
	return names, nil
}

// FindUsernamesByIDs batch-resolves user ids to usernames for display
// (the same N+1 fix shape as FindAPIKeyModelsByAPIKeyIDs). Unknown ids are
// simply absent from the map — callers render an empty owner rather than
// failing the whole list.
func FindUsernamesByIDs(db *gorm.DB, ids []uint) (map[uint]string, error) {
	result := make(map[uint]string)
	if len(ids) == 0 {
		return result, nil
	}
	var rows []model.User
	if err := db.Select("id, username").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, u := range rows {
		result[u.ID] = u.Username
	}
	return result, nil
}

// OwnerIdentity is what a per-key aggregate row displays: the owning
// account's username plus the key's own prefix. One account usually owns
// several keys, so the username alone would render indistinguishable
// duplicate rows — the prefix is what tells them apart.
type OwnerIdentity struct {
	Username  string
	KeyPrefix string
}

// FindOwnerIdentitiesByKeyIDs batch-resolves API-key ids to their owner identity
// via one query joining api_keys to users. Keys that are missing (and keys
// whose owner row is gone) are absent from the map — callers render zero
// values for those rows.
func FindOwnerIdentitiesByKeyIDs(db *gorm.DB, ids []uint) (map[uint]OwnerIdentity, error) {
	names := make(map[uint]OwnerIdentity, len(ids))
	if len(ids) == 0 {
		return names, nil
	}
	var rows []struct {
		ID        uint
		Username  string
		KeyPrefix string
	}
	if err := db.Table("api_keys").
		Select("api_keys.id AS id, users.username AS username, api_keys.key_prefix AS key_prefix").
		Joins("JOIN users ON users.id = api_keys.user_id").
		Where("api_keys.id IN ?", ids).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		names[r.ID] = OwnerIdentity{Username: r.Username, KeyPrefix: r.KeyPrefix}
	}
	return names, nil
}
