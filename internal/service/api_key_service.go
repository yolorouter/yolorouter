// Package service additions: API Key management business logic — key
// generation/hashing, sparse PATCH with 0-sentinel limit clearing, runtime
// display-status computation, and free-text search. Limit *enforcement*
// (RPM/TPM/concurrency/budget rejection at request time) is deliberately NOT
// here — it belongs to the gateway module.
package service

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

const (
	apiKeyPrefixTag    = "sk-yr-" // example format
	apiKeyDisplayChars = 16       // KeyPrefix display length, list-distinguishing only
	randKeyBytes       = 32       // 32 random bytes -> ~43 base64 chars, far longer than the prefix
)

// APIKey display statuses returned to the UI. Computed at read time, never
// stored — same "running status not persisted" pattern as
// ModelRunningStatus. Budget-exhausted is currently unreachable (nothing writes
// budget_spent_micros until the gateway records per-request cost) but is kept
// here so the status is correct from day one once the gateway wires the spend write.
const (
	APIKeyDisplayActive    = "active"
	APIKeyDisplayExpired   = "expired"
	APIKeyDisplayRevoked   = "revoked"
	APIKeyDisplayBudgetHit = "budget_exhausted"
)

type APIKeyService struct {
	db *gorm.DB
}

func NewAPIKeyService(db *gorm.DB) *APIKeyService {
	return &APIKeyService{db: db}
}

// APIKeyView is the API-facing shape. Status is the stored active/revoked
// value; DisplayStatus is the runtime-computed value the UI shows. ModelIDs
// is the key's allowlist (never nil — empty array means "no models").
type APIKeyView struct {
	ID                uint       `json:"id"`
	KeyPrefix         string     `json:"key_prefix"`
	OwnerLabel        string     `json:"owner_label"`
	Remark            string     `json:"remark"`
	Status            int        `json:"status"`
	DisplayStatus     string     `json:"display_status"`
	ExpiresAt         *time.Time `json:"expires_at"`
	RPMLimit          *int       `json:"rpm_limit"`
	TPMLimit          *int       `json:"tpm_limit"`
	ConcurrencyLimit  *int       `json:"concurrency_limit"`
	BudgetLimitMicros *int64     `json:"budget_limit_micros"`
	BudgetSpentMicros int64      `json:"budget_spent_micros"`
	ModelIDs          []uint     `json:"model_ids"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CreateAPIKeyInput struct {
	OwnerLabel        string
	Remark            string
	ModelIDs          []uint
	ExpiresAt         *time.Time
	RPMLimit          *int
	TPMLimit          *int
	ConcurrencyLimit  *int
	BudgetLimitMicros *int64
}

// CreateAPIKeyResult carries the plaintext key exactly once — PlaintextKey is
// never persisted and never obtainable again afterwards.
type CreateAPIKeyResult struct {
	PlaintextKey string
	APIKey       APIKeyView
}

// UpdateAPIKeyInput is a sparse PATCH: pointer fields are nil = leave
// unchanged. For the numeric limits, a non-nil 0 is a sentinel meaning "clear
// this limit" (no cap) — same convention as the reference project, so a PATCH
// touching only one field can't silently wipe the others. ModelIDs is nil =
// leave whitelist unchanged; a non-nil slice replaces it (empty slice clears
// it). ExpiresAt has no clear-sentinel (no clean zero-value wire
// representation) — to remove an expiry, revoke and create a new key.
type UpdateAPIKeyInput struct {
	OwnerLabel        *string
	Remark            *string
	ModelIDs          []uint
	ExpiresAt         *time.Time
	RPMLimit          *int
	TPMLimit          *int
	ConcurrencyLimit  *int
	BudgetLimitMicros *int64
}

func (s *APIKeyService) ListAPIKeys(q string, page, pageSize int) ([]APIKeyView, int64, error) {
	total, err := repository.CountAPIKeys(s.db, q)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []APIKeyView{}, 0, nil
	}
	offset := (page - 1) * pageSize
	keys, err := repository.SearchAPIKeys(s.db, q, offset, pageSize)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]uint, len(keys))
	for i, k := range keys {
		ids[i] = k.ID
	}
	allAllow, err := repository.FindAPIKeyModelsByAPIKeyIDs(s.db, ids)
	if err != nil {
		return nil, 0, err
	}
	allowByKey := make(map[uint][]uint, len(keys))
	for _, am := range allAllow {
		allowByKey[am.APIKeyID] = append(allowByKey[am.APIKeyID], am.ModelID)
	}
	views := make([]APIKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, toAPIKeyView(k, allowByKey[k.ID]))
	}
	return views, total, nil
}

func (s *APIKeyService) CreateAPIKey(input CreateAPIKeyInput, now time.Time) (*CreateAPIKeyResult, error) {
	modelIDs := uniqueUint(input.ModelIDs)
	if err := s.assertModelsExist(modelIDs); err != nil {
		return nil, err
	}
	rawKey, err := generateAPIKey()
	if err != nil {
		return nil, err
	}
	key := &model.APIKey{
		KeyHash:           hashToken(rawKey),
		KeyPrefix:         truncatePrefix(rawKey),
		OwnerLabel:        input.OwnerLabel,
		Remark:            input.Remark,
		Status:            model.APIKeyStatusActive,
		ExpiresAt:         input.ExpiresAt,
		RPMLimit:          limitPtrOrNil(input.RPMLimit),
		TPMLimit:          limitPtrOrNil(input.TPMLimit),
		ConcurrencyLimit:  limitPtrOrNil(input.ConcurrencyLimit),
		BudgetLimitMicros: limitPtrOrNil(input.BudgetLimitMicros),
	}
	if err := repository.CreateAPIKey(s.db, key, modelIDs, now); err != nil {
		return nil, err
	}
	view := toAPIKeyView(*key, modelIDs)
	return &CreateAPIKeyResult{PlaintextKey: rawKey, APIKey: view}, nil
}

func (s *APIKeyService) GetAPIKey(id uint) (*APIKeyView, error) {
	key, err := repository.FindAPIKeyByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAPIKeyNotFound
		}
		return nil, err
	}
	modelIDs, err := repository.FindAPIKeyModelIDs(s.db, id)
	if err != nil {
		return nil, err
	}
	view := toAPIKeyView(*key, modelIDs)
	return &view, nil
}

func (s *APIKeyService) UpdateAPIKey(id uint, input UpdateAPIKeyInput, now time.Time) (*APIKeyView, error) {
	if _, err := repository.FindAPIKeyByID(s.db, id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAPIKeyNotFound
		}
		return nil, err
	}

	updates := map[string]interface{}{}
	if input.OwnerLabel != nil {
		updates["owner_label"] = *input.OwnerLabel
	}
	if input.Remark != nil {
		updates["remark"] = *input.Remark
	}
	if input.ExpiresAt != nil {
		updates["expires_at"] = *input.ExpiresAt
	}
	if input.RPMLimit != nil {
		updates["rpm_limit"] = numericOrClear(*input.RPMLimit)
	}
	if input.TPMLimit != nil {
		updates["tpm_limit"] = numericOrClear(*input.TPMLimit)
	}
	if input.ConcurrencyLimit != nil {
		updates["concurrency_limit"] = numericOrClear(*input.ConcurrencyLimit)
	}
	if input.BudgetLimitMicros != nil {
		updates["budget_limit_micros"] = numericOrClear(*input.BudgetLimitMicros)
	}

	// nil ModelIDs = leave whitelist untouched; non-nil (after dedup) replaces
	// it. assertModelsExist is skipped for an empty replacement — clearing the
	// whitelist is a valid state.
	var modelIDs []uint
	if input.ModelIDs != nil {
		modelIDs = uniqueUint(input.ModelIDs)
		if err := s.assertModelsExist(modelIDs); err != nil {
			return nil, err
		}
	}

	if err := repository.UpdateAPIKey(s.db, id, updates, modelIDs, now); err != nil {
		return nil, err
	}
	return s.GetAPIKey(id)
}

func (s *APIKeyService) RevokeAPIKey(id uint, now time.Time) error {
	key, err := repository.FindAPIKeyByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errcode.ErrAPIKeyNotFound
		}
		return err
	}
	if key.Status == model.APIKeyStatusRevoked {
		return nil
	}
	return repository.RevokeAPIKey(s.db, id, now)
}

// assertModelsExist verifies every id resolves to an existing models row, so
// a stale client can't whitelist a deleted model id. Empty input is a no-op
// (used by UpdateAPIKey when clearing a whitelist).
func (s *APIKeyService) assertModelsExist(modelIDs []uint) error {
	if len(modelIDs) == 0 {
		return nil
	}
	var cnt int64
	if err := s.db.Model(&model.Model{}).Where("id IN ?", modelIDs).Count(&cnt).Error; err != nil {
		return err
	}
	if int64(len(modelIDs)) != cnt {
		return errcode.ErrModelNotFound
	}
	return nil
}

func toAPIKeyView(k model.APIKey, modelIDs []uint) APIKeyView {
	if modelIDs == nil {
		modelIDs = []uint{}
	}
	return APIKeyView{
		ID: k.ID, KeyPrefix: k.KeyPrefix, OwnerLabel: k.OwnerLabel, Remark: k.Remark,
		Status: k.Status, DisplayStatus: computeAPIKeyDisplayStatus(k),
		ExpiresAt: k.ExpiresAt, RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit,
		ConcurrencyLimit: k.ConcurrencyLimit, BudgetLimitMicros: k.BudgetLimitMicros,
		BudgetSpentMicros: k.BudgetSpentMicros, ModelIDs: modelIDs,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
}

// computeAPIKeyDisplayStatus derives the UI status from stored fields. Order
// matters: revoked wins over everything; then expiry; then budget. Active is
// the fallback.
func computeAPIKeyDisplayStatus(k model.APIKey) string {
	if k.Status == model.APIKeyStatusRevoked {
		return APIKeyDisplayRevoked
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now().UTC()) {
		return APIKeyDisplayExpired
	}
	if k.BudgetLimitMicros != nil && k.BudgetSpentMicros >= *k.BudgetLimitMicros {
		return APIKeyDisplayBudgetHit
	}
	return APIKeyDisplayActive
}

// generateAPIKey produces a new plaintext key: 32 random bytes, base64
// URL-safe encoded, with the sk-yr- prefix. Reuses the same
// generateRandomToken recipe as session tokens — one implementation, not two.
func generateAPIKey() (string, error) {
	return generateRandomToken(randKeyBytes, apiKeyPrefixTag)
}

// truncatePrefix takes the first apiKeyDisplayChars chars of the raw key as
// the list-distinguishing prefix — enough to tell keys apart, not enough to
// reconstruct the full key.
func truncatePrefix(rawKey string) string {
	if len(rawKey) <= apiKeyDisplayChars {
		return rawKey
	}
	return rawKey[:apiKeyDisplayChars]
}

// numericOrClear maps the 0 sentinel to nil (clears the limit); any other
// value passes through as a pointer. Same convention as the reference
// project's APIKeyLimits — 0 is otherwise-unused wire space, so a PATCH can
// clear a limit without a separate "clear" verb. Generic over int/int64 so
// the same recipe covers RPM/TPM/concurrency (int) and budget cents (int64)
// without two byte-identical bodies drifting apart.
func numericOrClear[T int | int64](v T) *T {
	if v == 0 {
		return nil
	}
	return &v
}

// limitPtrOrNil applies the same 0-sentinel convention as numericOrClear to a
// nullable input pointer (nil stays nil). CreateAPIKey uses this so a client
// sending rpm_limit=0 at create time is treated as "no cap" — the same
// meaning 0 has on the PATCH path — instead of persisting a literal 0 that
// the gateway would later read as "0 requests allowed".
func limitPtrOrNil[T int | int64](v *T) *T {
	if v == nil {
		return nil
	}
	return numericOrClear(*v)
}

// uniqueUint de-duplicates while preserving order, so a whitelist with
// repeated ids stores each id once and the count check in assertModelsExist
// isn't fooled by duplicates.
func uniqueUint(ids []uint) []uint {
	seen := make(map[uint]struct{}, len(ids))
	out := make([]uint, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
