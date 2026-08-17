// Package service additions: API Key management business logic — key
// generation/hashing, sparse PATCH with 0-sentinel limit clearing, runtime
// display-status computation, and free-text search. Limit *enforcement*
// (RPM/TPM/concurrency/budget rejection at request time) is deliberately NOT
// here — it belongs to the gateway module.
package service

import (
	"errors"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/crypto"
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
	db        *gorm.DB
	masterKey []byte
}

func NewAPIKeyService(db *gorm.DB, masterKey []byte) *APIKeyService {
	return &APIKeyService{db: db, masterKey: masterKey}
}

// APIKeyView is the API-facing shape. Status is the stored active/revoked
// value; DisplayStatus is the runtime-computed value the UI shows. ModelIDs
// is the key's allowlist (never nil — empty array means "no models").
type APIKeyView struct {
	ID        uint   `json:"id"`
	KeyPrefix string `json:"key_prefix"`
	// UserID / OwnerUsername identify the owning account. OwnerUsername is
	// resolved from the users table for display (empty when the owner row
	// is missing).
	UserID            uint       `json:"user_id"`
	OwnerUsername     string     `json:"owner_username"`
	Remark            string     `json:"remark"`
	Status            int        `json:"status"`
	DisplayStatus     string     `json:"display_status"`
	ExpiresAt         *time.Time `json:"expires_at"`
	RPMLimit          *int       `json:"rpm_limit"`
	TPMLimit          *int       `json:"tpm_limit"`
	ConcurrencyLimit  *int       `json:"concurrency_limit"`
	BudgetLimitMicros *int64     `json:"budget_limit_micros"`
	BudgetSpentMicros int64      `json:"budget_spent_micros"`
	AllowAllModels    bool       `json:"allow_all_models"`
	ModelIDs          []uint     `json:"model_ids"`
	// CSP per-key override: when CustomSystemPromptEnabledOverride is true the
	// key uses its own CustomSystemPromptEnabled/CustomSystemPrompt pair
	// instead of the system-wide default.
	CustomSystemPromptEnabledOverride bool   `json:"custom_system_prompt_enabled_override"`
	CustomSystemPromptEnabled         bool   `json:"custom_system_prompt_enabled"`
	CustomSystemPrompt                string `json:"custom_system_prompt"`
	// Input-compression per-key override: when CompressEnabledOverride is
	// true the key uses its own CompressEnabled flag instead of the
	// system-wide default; when false the key inherits the global setting
	// and CompressEnabled is ignored.
	CompressEnabledOverride bool      `json:"compress_enabled_override"`
	CompressEnabled         bool      `json:"compress_enabled"`
	CreatedAt               time.Time `json:"created_at"`
	UpdatedAt               time.Time `json:"updated_at"`
}

type CreateAPIKeyInput struct {
	// UserID is the owning account — required; repository.CreateAPIKey
	// rejects 0. The handler fills it from the authenticated session.
	UserID            uint
	Remark            string
	AllowAllModels    bool
	ModelIDs          []uint
	ExpiresAt         *time.Time
	RPMLimit          *int
	TPMLimit          *int
	ConcurrencyLimit  *int
	BudgetLimitMicros *int64
	// CSP per-key override fields at create time (value types — zero is the
	// wire default and means "inherit the global setting").
	CustomSystemPromptEnabledOverride bool
	CustomSystemPromptEnabled         bool
	CustomSystemPrompt                string
	// Input-compression per-key override at create time (value types —
	// false is the wire default and means "inherit the global setting").
	CompressEnabledOverride bool
	CompressEnabled         bool
}

// CreateAPIKeyResult carries the plaintext key once at create time. The same
// plaintext is also persisted encrypted (model.APIKey.EncryptedKey), so it can
// be recovered later via GetAPIKeyPlaintext — PlaintextKey here is just the
// create-time convenience that keeps the existing modal UX intact.
type CreateAPIKeyResult struct {
	PlaintextKey string
	APIKey       APIKeyView
}

// UpdateAPIKeyInput is a sparse PATCH: pointer fields are nil = leave
// unchanged. For the numeric limits, a non-nil 0 is a sentinel meaning "clear
// this limit" (no cap), so a PATCH touching only one field can't
// silently wipe the others. ModelIDs is nil =
// leave whitelist unchanged; a non-nil slice replaces it (empty slice clears
// it). ExpiresAt has no clear-sentinel (no clean zero-value wire
// representation) — to remove an expiry, revoke and create a new key.
// ExpectedUpdatedAt enables optimistic locking: when non-nil the repository
// qualifies its UPDATE with `AND updated_at = ?` and the PATCH returns 11013
// if another writer committed first. Only KeyCustomPromptModal populates this
// (it does a fresh GET before editing); EditKeyModal/CreateKeyModal omit it,
// preserving their legacy non-CAS behavior.
type UpdateAPIKeyInput struct {
	Remark            *string
	AllowAllModels    *bool
	ModelIDs          []uint
	ExpiresAt         *time.Time
	RPMLimit          *int
	TPMLimit          *int
	ConcurrencyLimit  *int
	BudgetLimitMicros *int64
	// CSP per-key override PATCH fields (pointer — nil means leave unchanged).
	CustomSystemPromptEnabledOverride *bool
	CustomSystemPromptEnabled         *bool
	CustomSystemPrompt                *string
	// Input-compression per-key override PATCH fields (pointer — nil means
	// leave unchanged). When CompressEnabledOverride is set to true,
	// CompressEnabled must also be supplied; when set to false, any provided
	// CompressEnabled is ignored (the key inherits the global setting).
	CompressEnabledOverride *bool
	CompressEnabled         *bool
	// ExpectedUpdatedAt is the optimistic-lock CAS token (nil = no CAS).
	ExpectedUpdatedAt *time.Time
}

// ListAPIKeys narrows to one account when userID is non-nil (a member's
// pinned view or an admin's filter); nil lists every account's keys.
func (s *APIKeyService) ListAPIKeys(q, status string, userID *uint, page, pageSize int) ([]APIKeyView, int64, error) {
	// Anchor the status filter's expiry check AND the rendered display status
	// to one clock, so a key expiring mid-request can't be filtered as active
	// while being rendered as expired.
	now := time.Now().UTC()
	filter := repository.APIKeyFilter{Query: q, Status: status, UserID: userID, Now: now}
	total, err := repository.CountAPIKeys(s.db, filter)
	if err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return []APIKeyView{}, 0, nil
	}
	offset := (page - 1) * pageSize
	keys, err := repository.SearchAPIKeys(s.db, filter, offset, pageSize)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]uint, len(keys))
	userIDs := make([]uint, 0, len(keys))
	for i, k := range keys {
		ids[i] = k.ID
		userIDs = append(userIDs, k.UserID)
	}
	allAllow, err := repository.FindAPIKeyModelsByAPIKeyIDs(s.db, ids)
	if err != nil {
		return nil, 0, err
	}
	usernames, err := repository.FindUsernamesByIDs(s.db, userIDs)
	if err != nil {
		return nil, 0, err
	}
	allowByKey := make(map[uint][]uint, len(keys))
	for _, am := range allAllow {
		allowByKey[am.APIKeyID] = append(allowByKey[am.APIKeyID], am.ModelID)
	}
	views := make([]APIKeyView, 0, len(keys))
	for _, k := range keys {
		v := toAPIKeyView(k, allowByKey[k.ID], now)
		v.OwnerUsername = usernames[k.UserID]
		views = append(views, v)
	}
	return views, total, nil
}

// validateCSPLen enforces the rune-count bound on custom system prompt text.
// Empty text is allowed here (the empty-when-enabled rule is a separate
// invariant owned by the repository layer); the caller passes the text it's
// about to persist and this helper rejects anything past the cap. Centralized
// so the Create and Update paths can't drift on the boundary check.
func validateCSPLen(text string) error {
	if text != "" && utf8.RuneCountInString(text) > MaxCustomSystemPromptLen {
		return errcode.ErrCustomSystemPromptTooLong
	}
	return nil
}

func (s *APIKeyService) CreateAPIKey(input CreateAPIKeyInput, now time.Time) (*CreateAPIKeyResult, error) {
	if err := validateCSPLen(input.CustomSystemPrompt); err != nil {
		return nil, err
	}
	modelIDs := uniqueUint(input.ModelIDs)
	if input.AllowAllModels {
		// An all-models key owns no allowlist rows; drop any ids the caller sent
		// so the bypass flag and the join table can never disagree.
		modelIDs = nil
	} else {
		// A custom-scope key must name at least one model — otherwise it could
		// call nothing. The Update path enforces the same rule atomically in
		// repository.UpdateAPIKey.
		if len(modelIDs) == 0 {
			return nil, errcode.ErrAPIKeyEmptyAllowlist
		}
		if err := s.assertModelsExist(modelIDs); err != nil {
			return nil, err
		}
	}
	rawKey, err := generateAPIKey()
	if err != nil {
		return nil, err
	}
	// Persist an AES-GCM ciphertext of the plaintext so the full key can be
	// revealed again from the list page. The auth path never touches this —
	// it still looks the key up by key_hash. Mirrors the provider-key model.
	encryptedKey, err := crypto.Encrypt(s.masterKey, rawKey)
	if err != nil {
		return nil, err
	}
	key := &model.APIKey{
		KeyHash:                           hashToken(rawKey),
		EncryptedKey:                      encryptedKey,
		KeyPrefix:                         truncatePrefix(rawKey),
		UserID:                            input.UserID,
		Remark:                            input.Remark,
		Status:                            model.APIKeyStatusActive,
		AllowAllModels:                    input.AllowAllModels,
		ExpiresAt:                         input.ExpiresAt,
		RPMLimit:                          limitPtrOrNil(input.RPMLimit),
		TPMLimit:                          limitPtrOrNil(input.TPMLimit),
		ConcurrencyLimit:                  limitPtrOrNil(input.ConcurrencyLimit),
		BudgetLimitMicros:                 limitPtrOrNil(input.BudgetLimitMicros),
		CustomSystemPromptEnabledOverride: input.CustomSystemPromptEnabledOverride,
		CustomSystemPromptEnabled:         input.CustomSystemPromptEnabled,
		CustomSystemPrompt:                input.CustomSystemPrompt,
		CompressEnabledOverride:           input.CompressEnabledOverride,
		CompressEnabled:                   input.CompressEnabled,
	}
	if err := repository.CreateAPIKey(s.db, key, modelIDs, now); err != nil {
		return nil, err
	}
	view := toAPIKeyView(*key, modelIDs, now)
	// Same owner-username enrichment the list/get paths perform — the POST
	// response is what the create form renders next, and without this the
	// freshly created key would display with no owner identity.
	usernames, err := repository.FindUsernamesByIDs(s.db, []uint{key.UserID})
	if err != nil {
		return nil, err
	}
	view.OwnerUsername = usernames[key.UserID]
	return &CreateAPIKeyResult{PlaintextKey: rawKey, APIKey: view}, nil
}

// requireKeyOwner is the ownership floor for every by-id key operation:
// when requiredOwner is set (a member session), a key owned by anyone
// else answers exactly like a nonexistent one — never a 403 that would
// confirm the id exists.
func requireKeyOwner(key *model.APIKey, requiredOwner *uint) error {
	if requiredOwner != nil && key.UserID != *requiredOwner {
		return errcode.ErrAPIKeyNotFound
	}
	return nil
}

func (s *APIKeyService) GetAPIKey(id uint, requiredOwner *uint) (*APIKeyView, error) {
	key, err := repository.FindAPIKeyByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAPIKeyNotFound
		}
		return nil, err
	}
	if err := requireKeyOwner(key, requiredOwner); err != nil {
		return nil, err
	}
	modelIDs, err := repository.FindAPIKeyModelIDs(s.db, id)
	if err != nil {
		return nil, err
	}
	view := toAPIKeyView(*key, modelIDs, time.Now().UTC())
	usernames, err := repository.FindUsernamesByIDs(s.db, []uint{key.UserID})
	if err != nil {
		return nil, err
	}
	view.OwnerUsername = usernames[key.UserID]
	return &view, nil
}

// GetAPIKeyPlaintext decrypts and returns the full plaintext key for the
// list-page reveal. Returns ErrAPIKeyPlaintextUnavailable when the row predates
// the encrypted_key column (its plaintext was never stored), and
// ErrAPIKeyNotFound when the id does not exist. Auth is unaffected — the
// gateway never calls this; it authenticates by key_hash.
func (s *APIKeyService) GetAPIKeyPlaintext(id uint, requiredOwner *uint) (string, error) {
	key, err := repository.FindAPIKeyByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", errcode.ErrAPIKeyNotFound
		}
		return "", err
	}
	if err := requireKeyOwner(key, requiredOwner); err != nil {
		return "", err
	}
	if key.EncryptedKey == "" {
		return "", errcode.ErrAPIKeyPlaintextUnavailable
	}
	plaintext, err := crypto.Decrypt(s.masterKey, key.EncryptedKey)
	if err != nil {
		return "", err
	}
	return plaintext, nil
}

func (s *APIKeyService) UpdateAPIKey(id uint, input UpdateAPIKeyInput, requiredOwner *uint, now time.Time) (*APIKeyView, error) {
	existing, err := repository.FindAPIKeyByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrAPIKeyNotFound
		}
		return nil, err
	}
	if err := requireKeyOwner(existing, requiredOwner); err != nil {
		return nil, err
	}

	if input.CustomSystemPrompt != nil {
		if err := validateCSPLen(*input.CustomSystemPrompt); err != nil {
			return nil, err
		}
	}

	// Compress override combination rule: the enabled flag is meaningful only
	// when the override is on. Setting override=false zeroes compress_enabled
	// for cleanliness (the key inherits the global setting and the stored
	// enabled value is ignored by the gateway). Setting override=true requires
	// the caller to also say what to override to. A lone enabled patch (override
	// left untouched) is allowed and writes just that column.
	updates := map[string]interface{}{}
	if input.CompressEnabledOverride != nil {
		if !*input.CompressEnabledOverride {
			updates["compress_enabled_override"] = false
			updates["compress_enabled"] = false
		} else {
			if input.CompressEnabled == nil {
				return nil, errcode.ErrCompressEnabledRequired
			}
			updates["compress_enabled_override"] = true
			updates["compress_enabled"] = *input.CompressEnabled
		}
	} else if input.CompressEnabled != nil {
		updates["compress_enabled"] = *input.CompressEnabled
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
	scopeChanged := input.AllowAllModels != nil
	if scopeChanged {
		updates["allow_all_models"] = *input.AllowAllModels
	}
	if input.CustomSystemPromptEnabledOverride != nil {
		updates["custom_system_prompt_enabled_override"] = *input.CustomSystemPromptEnabledOverride
	}
	if input.CustomSystemPromptEnabled != nil {
		updates["custom_system_prompt_enabled"] = *input.CustomSystemPromptEnabled
	}
	if input.CustomSystemPrompt != nil {
		updates["custom_system_prompt"] = *input.CustomSystemPrompt
	}

	// nil modelIDs leaves the allowlist untouched; a non-nil slice (after dedup)
	// replaces it. Switching to all-models discards the allowlist in the repo, so
	// skip validating ids the caller may still be sending — mirrors CreateAPIKey.
	// The resulting-state invariants (all-models owns no rows; a custom key keeps
	// a non-empty allowlist) are enforced atomically in repository.UpdateAPIKey,
	// which re-reads the flag under the row lock so a concurrent scope change
	// can't be clobbered by a stale pre-transaction read.
	var modelIDs []uint
	switch {
	case scopeChanged && *input.AllowAllModels:
		modelIDs = nil
	case input.ModelIDs != nil:
		modelIDs = uniqueUint(input.ModelIDs)
		if err := s.assertModelsExist(modelIDs); err != nil {
			return nil, err
		}
	}

	if err := repository.UpdateAPIKey(s.db, id, updates, modelIDs, scopeChanged, now, input.ExpectedUpdatedAt); err != nil {
		if errors.Is(err, repository.ErrEmptyCustomAllowlist) {
			return nil, errcode.ErrAPIKeyEmptyAllowlist
		}
		return nil, err
	}
	// Ownership was already verified above; no need to re-scope the readback.
	return s.GetAPIKey(id, nil)
}

func (s *APIKeyService) RevokeAPIKey(id uint, requiredOwner *uint, now time.Time) error {
	key, err := repository.FindAPIKeyByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errcode.ErrAPIKeyNotFound
		}
		return err
	}
	if err := requireKeyOwner(key, requiredOwner); err != nil {
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

// toAPIKeyView takes `now` (rather than reading the clock itself) so a caller
// that also filters by status can use one consistent timestamp for both — see
// ListAPIKeys, where a key expiring between the SQL filter and this call would
// otherwise be filtered as active but rendered as expired.
func toAPIKeyView(k model.APIKey, modelIDs []uint, now time.Time) APIKeyView {
	if modelIDs == nil {
		modelIDs = []uint{}
	}
	return APIKeyView{
		ID: k.ID, KeyPrefix: k.KeyPrefix, UserID: k.UserID, Remark: k.Remark,
		Status: k.Status, DisplayStatus: computeAPIKeyDisplayStatus(k, now),
		ExpiresAt: k.ExpiresAt, RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit,
		ConcurrencyLimit: k.ConcurrencyLimit, BudgetLimitMicros: k.BudgetLimitMicros,
		BudgetSpentMicros: k.BudgetSpentMicros, AllowAllModels: k.AllowAllModels, ModelIDs: modelIDs,
		CustomSystemPromptEnabledOverride: k.CustomSystemPromptEnabledOverride,
		CustomSystemPromptEnabled:         k.CustomSystemPromptEnabled,
		CustomSystemPrompt:                k.CustomSystemPrompt,
		CompressEnabledOverride:           k.CompressEnabledOverride,
		CompressEnabled:                   k.CompressEnabled,
		CreatedAt:                         k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
}

// computeAPIKeyDisplayStatus derives the UI status from stored fields, as of
// `now`. Order matters: revoked wins over everything; then expiry; then
// budget. Active is the fallback. The expiry boundary (expires_at < now)
// mirrors the SQL in repository.applyAPIKeyFilters, so both must be given the
// same `now` to agree.
func computeAPIKeyDisplayStatus(k model.APIKey, now time.Time) string {
	if k.Status == model.APIKeyStatusRevoked {
		return APIKeyDisplayRevoked
	}
	if k.ExpiresAt != nil && k.ExpiresAt.Before(now) {
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
