// Package repository additions for M2: pure data access for
// providers/provider_keys — no business judgment here (that's
// internal/service/provider_service.go's job). See design doc
// .claude/docs/2026-07-18-m2-provider-design.md §3.
package repository

import (
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/yolorouter/yolorouter-ce/internal/model"
)

// CreateProviderWithKey inserts a Provider and its first ProviderKey in one
// transaction (design doc §6: "同一个事务内建 Provider + ProviderKey 两
// 行", mirroring M1's Setup's admin+session pattern). Caller must have
// already populated key.AuthorizedDestinationVersion == provider's intended
// DestinationVersion (1 for a brand-new provider) before calling.
func CreateProviderWithKey(db *gorm.DB, provider *model.Provider, key *model.ProviderKey) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(provider).Error; err != nil {
			return err
		}
		key.ProviderID = provider.ID
		return tx.Create(key).Error
	})
}

// FindProviderByID returns gorm.ErrRecordNotFound if id doesn't exist.
func FindProviderByID(db *gorm.DB, id uint) (*model.Provider, error) {
	var provider model.Provider
	if err := db.Where("id = ?", id).First(&provider).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// FindProviderByName returns gorm.ErrRecordNotFound if no provider has that
// exact name — used by the service layer for a friendly pre-check before
// insert (the real uniqueness guarantee is the UNIQUE constraint itself;
// see service.CreateProvider).
func FindProviderByName(db *gorm.DB, name string) (*model.Provider, error) {
	var provider model.Provider
	if err := db.Where("name = ?", name).First(&provider).Error; err != nil {
		return nil, err
	}
	return &provider, nil
}

// ListProviders returns every provider, ordered by name for a stable list
// display (v0.1 has no pagination — provider counts are expected to be
// small, design doc §9).
func ListProviders(db *gorm.DB) ([]model.Provider, error) {
	var providers []model.Provider
	if err := db.Order("name ASC").Find(&providers).Error; err != nil {
		return nil, err
	}
	return providers, nil
}

// UpdateProviderNameNote updates only name/note — must never touch
// destination_version (that's UpdateProviderBaseURL's sole responsibility,
// design doc §3).
func UpdateProviderNameNote(db *gorm.DB, id uint, name, note string, now time.Time) error {
	return db.Model(&model.Provider{}).Where("id = ?", id).
		Updates(map[string]interface{}{"name": name, "note": note, "updated_at": now}).Error
}

// UpdateProviderBaseURL atomically writes the new base_url and bumps
// destination_version in the SAME UPDATE statement (design doc §3's
// "目的地版本绑定": the address change and the version bump must never be
// two separable writes). Returns the resulting destination_version.
func UpdateProviderBaseURL(db *gorm.DB, id uint, baseURL string, now time.Time) (int, error) {
	var result struct {
		DestinationVersion int `gorm:"column:destination_version"`
	}
	err := db.Raw(`
		UPDATE providers
		SET base_url = ?, destination_version = destination_version + 1, updated_at = ?
		WHERE id = ?
		RETURNING destination_version
	`, baseURL, now, id).Scan(&result).Error
	if err != nil {
		return 0, err
	}
	return result.DestinationVersion, nil
}

// UpdateProviderManagementStatus enables/disables a provider (PRD §6.2.6).
// UpdateProviderManagementStatus returns applied=false (no error) if id
// doesn't exist — a /simplify efficiency-review finding: the service layer
// used to do a separate FindProviderByID existence check before this write;
// RowsAffected already tells the caller whether a row existed, collapsing
// two round trips into one.
func UpdateProviderManagementStatus(db *gorm.DB, id uint, status int, now time.Time) (bool, error) {
	result := db.Model(&model.Provider{}).Where("id = ?", id).
		Updates(map[string]interface{}{"management_status": status, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// FindProviderKeyByID returns gorm.ErrRecordNotFound if id doesn't exist.
func FindProviderKeyByID(db *gorm.DB, id uint) (*model.ProviderKey, error) {
	var key model.ProviderKey
	if err := db.Where("id = ?", id).First(&key).Error; err != nil {
		return nil, err
	}
	return &key, nil
}

// FindProviderKeyByLabel looks up a key by its provider-scoped label.
func FindProviderKeyByLabel(db *gorm.DB, providerID uint, label string) (*model.ProviderKey, error) {
	var key model.ProviderKey
	if err := db.Where("provider_id = ? AND label = ?", providerID, label).First(&key).Error; err != nil {
		return nil, err
	}
	return &key, nil
}

// ListProviderKeysByProvider returns every key for a provider ordered by
// sort_order (the display/routing order PRD §6.2.7 requires).
func ListProviderKeysByProvider(db *gorm.DB, providerID uint) ([]model.ProviderKey, error) {
	var keys []model.ProviderKey
	if err := db.Where("provider_id = ?", providerID).Order("sort_order ASC").Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// ListProviderKeysByProviderIDs returns every key belonging to any of the
// given providers in one query, ordered by (provider_id, sort_order) —
// batches what would otherwise be one ListProviderKeysByProvider call per
// provider (an N+1 query pattern) into a single round trip. Callers group
// the flat result by ProviderID themselves.
func ListProviderKeysByProviderIDs(db *gorm.DB, providerIDs []uint) ([]model.ProviderKey, error) {
	if len(providerIDs) == 0 {
		return nil, nil
	}
	var keys []model.ProviderKey
	if err := db.Where("provider_id IN ?", providerIDs).Order("provider_id ASC, sort_order ASC").Find(&keys).Error; err != nil {
		return nil, err
	}
	return keys, nil
}

// NextSortOrder returns 1 + the current maximum sort_order for a provider
// (1 if it has no keys yet) — the position a newly appended key should
// take.
func NextSortOrder(db *gorm.DB, providerID uint) (int, error) {
	var max *int
	if err := db.Model(&model.ProviderKey{}).Where("provider_id = ?", providerID).
		Select("MAX(sort_order)").Scan(&max).Error; err != nil {
		return 0, err
	}
	if max == nil {
		return 1, nil
	}
	return *max + 1, nil
}

// CreateProviderKeyPendingTest inserts a brand-new key row, snapshotting
// the parent provider's current destination_version into
// key.AuthorizedDestinationVersion. config_version and test_generation are
// forced to 1 regardless of whatever the caller set (a just-created row has
// no prior test attempt to race against, so both start "already claimed" —
// design doc §3). Returns the snapshot version the caller must test
// against and later pass to CommitProviderKeyPlaintextTestResult.
//
// No explicit row lock is taken on the provider read: the real guard
// against a concurrent base_url change is the LATER write-back's CAS
// condition (re-reading destination_version at commit time, long after the
// real network test has run) — a race during this split-second initial
// read is superseded by that final check regardless.
func CreateProviderKeyPendingTest(db *gorm.DB, key *model.ProviderKey, now time.Time) (int, error) {
	var snapshotVersion int
	err := db.Transaction(func(tx *gorm.DB) error {
		var provider model.Provider
		if err := tx.Select("destination_version").Where("id = ?", key.ProviderID).First(&provider).Error; err != nil {
			return err
		}
		snapshotVersion = provider.DestinationVersion
		key.AuthorizedDestinationVersion = snapshotVersion
		key.ConfigVersion = 1
		key.TestGeneration = 1
		return tx.Create(key).Error
	})
	if err != nil {
		return 0, err
	}
	return snapshotVersion, nil
}

// SwapProviderKeyPlaintext atomically writes EVERYTHING involved in
// replacing an existing key's plaintext (edit-with-new-key / re-entry
// after an address change — creation is handled entirely by
// CreateProviderKeyPendingTest instead, which has no prior row to race
// against) in a SINGLE transaction: label, test_model, the new
// encrypted_key/key_prefix, management_status (the caller always passes
// disabled here — the service layer decides the real final status only
// after the out-of-transaction network test completes), config_version+1,
// verification_status forced to untested, and a claimed test_generation —
// while reading the parent provider's current destination_version as the
// snapshot the caller must test against.
//
// A codex adversarial review round found that doing this as 3 separate
// statements (label/status update, then a separate ciphertext/prefix
// update, then a separate config_version/verification_status/generation
// update) left a window where a crash, DB error, or concurrent read
// between them could observe new ciphertext still paired with the OLD
// credential's config_version/verification_status — letting a replacement
// key transiently or permanently inherit the previous secret's "passed"
// status. Folding every column into one UPDATE removes that window
// entirely: the row is either fully old or fully new, never in between.
func SwapProviderKeyPlaintext(db *gorm.DB, keyID uint, label, testModel, encryptedKey, keyPrefix string, managementStatus int, now time.Time) (configVersion, testGeneration, snapshotVersion int, err error) {
	err = db.Transaction(func(tx *gorm.DB) error {
		var key model.ProviderKey
		if fetchErr := tx.Select("provider_id").Where("id = ?", keyID).First(&key).Error; fetchErr != nil {
			return fetchErr
		}
		var provider model.Provider
		if fetchErr := tx.Select("destination_version").Where("id = ?", key.ProviderID).First(&provider).Error; fetchErr != nil {
			return fetchErr
		}
		snapshotVersion = provider.DestinationVersion

		var result struct {
			ConfigVersion  int `gorm:"column:config_version"`
			TestGeneration int `gorm:"column:test_generation"`
		}
		updateErr := tx.Raw(`
			UPDATE provider_keys
			SET label = ?, test_model = ?, encrypted_key = ?, key_prefix = ?,
			    management_status = ?, verification_status = ?,
			    config_version = config_version + 1,
			    test_generation = test_generation + 1,
			    updated_at = ?
			WHERE id = ?
			RETURNING config_version, test_generation
		`, label, testModel, encryptedKey, keyPrefix, managementStatus, model.VerificationStatusUntested, now, keyID).
			Scan(&result).Error
		if updateErr != nil {
			return updateErr
		}
		configVersion = result.ConfigVersion
		testGeneration = result.TestGeneration
		return nil
	})
	return configVersion, testGeneration, snapshotVersion, err
}

// BeginProviderKeyRetest claims a new test_generation for a retest (same
// plaintext, no config_version bump, verification_status untouched),
// reads the current config_version so it can be passed back unchanged to
// CommitProviderKeyRetestResult's CAS condition (design doc §3's "重测"),
// and atomically returns the current encrypted_key in the SAME statement.
//
// A codex adversarial review round found that a caller reading and
// decrypting encrypted_key BEFORE calling this to claim a generation could
// race against a concurrent plaintext replacement: the claim would then
// return the NEW config_version while the network test actually ran
// against the OLD, already-decrypted plaintext — and the later CAS write
// would incorrectly accept writing the OLD credential's result onto the
// NEW credential's row (config_version matches; nothing about the CAS
// condition itself catches this, since it never re-examines what plaintext
// was actually tested). Returning the encrypted_key snapshot atomically
// with the claim closes this: callers must decrypt THIS return value, not
// one read at any earlier point in time.
func BeginProviderKeyRetest(db *gorm.DB, keyID uint) (configVersion, testGeneration int, encryptedKey string, err error) {
	var result struct {
		ConfigVersion  int    `gorm:"column:config_version"`
		TestGeneration int    `gorm:"column:test_generation"`
		EncryptedKey   string `gorm:"column:encrypted_key"`
	}
	err = db.Raw(`
		UPDATE provider_keys SET test_generation = test_generation + 1
		WHERE id = ?
		RETURNING config_version, test_generation, encrypted_key
	`, keyID).Scan(&result).Error
	return result.ConfigVersion, result.TestGeneration, result.EncryptedKey, err
}

// CommitProviderKeyPlaintextTestResult writes back a test result for a key
// that just received brand-new plaintext (creation, edit-with-new-key, or
// re-entry) — design doc §3's "提交新明文" CAS. overwriteVerification is
// the service layer's design-doc-§5 classification decision: when false,
// verification_status is left OUT of the SET clause entirely (it was
// already forced to untested by CreateProviderKeyPendingTest's insert or
// BeginProviderKeyPlaintextSwap, so "leave untouched" correctly means
// "still untested", never a stale value from a prior credential).
// authorized_destination_version is unconditionally SET to snapshotVersion
// (never compared against the old value — the old value is expected to be
// stale, that's the whole point of this flow). Returns applied=false if any
// CAS condition didn't match (result discarded, a race was detected) —
// callers must not treat that as an error.
func CommitProviderKeyPlaintextTestResult(
	db *gorm.DB, keyID uint, configVersion, testGeneration, snapshotVersion int,
	overwriteVerification bool, verificationStatus int,
	lastTestResult *int, lastTestModel string, lastTestDurationMs int64, now time.Time,
) (bool, error) {
	setClause, args := verificationSetClause(overwriteVerification, verificationStatus)
	args = append(args, lastTestResult, snapshotVersion, lastTestModel, lastTestDurationMs, now, now,
		keyID, configVersion, testGeneration, snapshotVersion)

	query := `
		UPDATE provider_keys
		SET ` + setClause + `last_test_result = ?, authorized_destination_version = ?,
		    last_test_model = ?, last_test_duration_ms = ?, last_tested_at = ?, updated_at = ?
		WHERE id = ?
		  AND config_version = ?
		  AND test_generation = ?
		  AND (SELECT destination_version FROM providers WHERE providers.id = provider_keys.provider_id) = ?
	`
	return execReturningApplied(db, query, args...)
}

// verificationSetClause builds the shared "SET verification_status = ?, "
// fragment (and its leading arg) that both CAS write-back functions below
// need — when overwrite is false, verification_status is deliberately left
// OUT of the SET clause entirely (see CommitProviderKeyPlaintextTestResult's
// doc comment for why "leave untouched" is correct there), so the fragment
// itself is empty and no arg is added.
func verificationSetClause(overwrite bool, verificationStatus int) (string, []interface{}) {
	if !overwrite {
		return "", nil
	}
	return "verification_status = ?, ", []interface{}{verificationStatus}
}

// execReturningApplied runs a CAS UPDATE and reports whether it actually
// matched a row (RowsAffected > 0) — shared tail for both CAS write-back
// functions below, since a query that matched no rows is deliberately "not
// applied", not a Go error the caller should propagate.
func execReturningApplied(db *gorm.DB, query string, args ...interface{}) (bool, error) {
	result := db.Exec(query, args...)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// UpdateProviderKeyLabelAndStatus updates label + test_model + management_status
// only (no plaintext change) — does not touch verification_status/config_version
// (design doc §8: "改标签/顺序不影响 verification_status"). test_model can
// still be changed here (which model a future test uses) independently of
// whether the plaintext itself changes.
func UpdateProviderKeyLabelAndStatus(db *gorm.DB, keyID uint, label, testModel string, managementStatus int, now time.Time) error {
	return db.Model(&model.ProviderKey{}).Where("id = ?", keyID).
		Updates(map[string]interface{}{"label": label, "test_model": testModel, "management_status": managementStatus, "updated_at": now}).Error
}

// SetProviderKeyManagementStatus enables/disables a key with no other field
// changes (PATCH .../keys/:keyId/status).
func SetProviderKeyManagementStatus(db *gorm.DB, keyID uint, status int, now time.Time) error {
	return db.Model(&model.ProviderKey{}).Where("id = ?", keyID).
		Updates(map[string]interface{}{"management_status": status, "updated_at": now}).Error
}

// UpdateProviderKeyLabelAndStatusIfVerified is UpdateProviderKeyLabelAndStatus's
// CAS-guarded counterpart for the one transition that matters: writing
// management_status=Enabled. A max-effort code-review round found the
// service layer's verifyKeyEnableAllowed check (verification_status/
// authorized_destination_version) and the actual write were two separate
// steps with no guard between them — a concurrent base_url change or
// retest landing in that window could invalidate the key, yet the stale
// "enable" write still committed unconditionally. Guarding the write on
// the same two columns the check just read closes that window: applied
// stays false (no error) if either no longer matches by the time this
// runs.
func UpdateProviderKeyLabelAndStatusIfVerified(db *gorm.DB, keyID uint, label, testModel string, status,
	expectedVerificationStatus, expectedAuthorizedDestinationVersion int, now time.Time) (bool, error) {
	return execReturningApplied(db,
		`UPDATE provider_keys SET label = ?, test_model = ?, management_status = ?, updated_at = ?
		 WHERE id = ? AND verification_status = ? AND authorized_destination_version = ?`,
		label, testModel, status, now, keyID, expectedVerificationStatus, expectedAuthorizedDestinationVersion)
}

// SetProviderKeyManagementStatusIfVerified is SetProviderKeyManagementStatus's
// CAS-guarded counterpart, for the identical reason and same two guard
// columns as UpdateProviderKeyLabelAndStatusIfVerified above — used by
// SetProviderKeyStatus's enable path.
func SetProviderKeyManagementStatusIfVerified(db *gorm.DB, keyID uint, status,
	expectedVerificationStatus, expectedAuthorizedDestinationVersion int, now time.Time) (bool, error) {
	return execReturningApplied(db,
		`UPDATE provider_keys SET management_status = ?, updated_at = ?
		 WHERE id = ? AND verification_status = ? AND authorized_destination_version = ?`,
		status, now, keyID, expectedVerificationStatus, expectedAuthorizedDestinationVersion)
}

// CASProviderKeyManagementStatus writes management_status only if the row's
// current value still matches expectedCurrent — a max-effort code-review
// round found runNewPlaintextTestAndCommit's final "enable if the test
// passed" write used SetProviderKeyManagementStatus's unconditional
// UPDATE, so a legitimate concurrent PATCH .../status call landing in the
// window between the CAS-committed verification result and this write
// could be silently clobbered by the stale plaintext-test request's own
// enable/disable intent. Returns applied=false (no error) if the row no
// longer matches, the same "lost the race, do nothing" contract as
// execReturningApplied's other callers.
func CASProviderKeyManagementStatus(db *gorm.DB, keyID uint, expectedCurrent, status int, now time.Time) (bool, error) {
	return execReturningApplied(db,
		`UPDATE provider_keys SET management_status = ?, updated_at = ? WHERE id = ? AND management_status = ?`,
		status, now, keyID, expectedCurrent)
}

// SwapProviderKeySortOrder atomically swaps sort_order between keyID and
// its immediate neighbor in the given direction ("up" = the key with the
// next lower sort_order, "down" = next higher). Returns ok=false (no error)
// if keyID is already at that boundary — the service layer treats this as
// a harmless no-op. The intermediate negative value avoids momentarily
// violating the UNIQUE(provider_id, sort_order) constraint mid-swap on
// either SQLite or Postgres (both check UNIQUE per-statement, not
// deferred).
func SwapProviderKeySortOrder(db *gorm.DB, providerID, keyID uint, direction string, now time.Time) (bool, error) {
	var applied bool
	err := db.Transaction(func(tx *gorm.DB) error {
		var current model.ProviderKey
		if err := tx.Select("id, sort_order").Where("id = ? AND provider_id = ?", keyID, providerID).First(&current).Error; err != nil {
			return err
		}

		var neighbor model.ProviderKey
		var neighborErr error
		if direction == "up" {
			neighborErr = tx.Select("id, sort_order").Where("provider_id = ? AND sort_order < ?", providerID, current.SortOrder).
				Order("sort_order DESC").Limit(1).First(&neighbor).Error
		} else {
			neighborErr = tx.Select("id, sort_order").Where("provider_id = ? AND sort_order > ?", providerID, current.SortOrder).
				Order("sort_order ASC").Limit(1).First(&neighbor).Error
		}
		if errors.Is(neighborErr, gorm.ErrRecordNotFound) {
			applied = false
			return nil
		}
		if neighborErr != nil {
			return neighborErr
		}

		if err := tx.Model(&model.ProviderKey{}).Where("id = ?", current.ID).
			Updates(map[string]interface{}{"sort_order": -current.SortOrder, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ProviderKey{}).Where("id = ?", neighbor.ID).
			Updates(map[string]interface{}{"sort_order": current.SortOrder, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ProviderKey{}).Where("id = ?", current.ID).
			Updates(map[string]interface{}{"sort_order": neighbor.SortOrder, "updated_at": now}).Error; err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

// ClaimProviderKeyFingerprintIfAbsent atomically inserts the single
// fingerprint row ONLY if it doesn't already exist — design doc §5's
// master-key/backup-restore mismatch detector. Uses gorm's
// clause.OnConflict{DoNothing: true}, which GORM translates to the correct
// per-dialect syntax (SQLite's "INSERT OR IGNORE", Postgres's "ON CONFLICT
// DO NOTHING"), so it never overwrites an existing row regardless of
// driver.
//
// A codex adversarial review round found the original "check not-found,
// then unconditionally Save" sequence was itself a check-then-act race:
// two instances booting concurrently against the same fresh database with
// DIFFERENT master keys could both observe "not found" and both attempt to
// write, with the later Save silently overwriting the earlier one's probe
// — leaving one instance's key permanently, silently mismatched with no
// error at that moment. Because this call can never overwrite a winner,
// service.VerifyMasterKeyFingerprint (Task 8) always re-reads and
// decrypt-verifies AFTER calling this, regardless of whether its own
// claim actually won or lost — the losing instance correctly fails that
// verification instead of silently believing it succeeded.
func ClaimProviderKeyFingerprintIfAbsent(db *gorm.DB, encryptedProbe string, now time.Time) error {
	fp := model.ProviderKeyFingerprint{ID: 1, EncryptedProbe: encryptedProbe, CreatedAt: now}
	return db.Clauses(clause.OnConflict{DoNothing: true}).Create(&fp).Error
}

// GetProviderKeyFingerprint returns gorm.ErrRecordNotFound if no fingerprint
// has ever been written (a genuinely brand-new instance).
func GetProviderKeyFingerprint(db *gorm.DB) (*model.ProviderKeyFingerprint, error) {
	var fp model.ProviderKeyFingerprint
	if err := db.Where("id = ?", 1).First(&fp).Error; err != nil {
		return nil, err
	}
	return &fp, nil
}

// CommitProviderKeyRetestResult writes back a retest result (plaintext
// unchanged) — design doc §3's "重测" CAS: guards config_version (no
// concurrent plaintext edit happened), test_generation (no later-claimed
// retest raced ahead), and a DIRECT comparison of
// authorized_destination_version against the current destination_version
// (not a snapshot — a retest has no "new" version to snapshot).
func CommitProviderKeyRetestResult(
	db *gorm.DB, keyID uint, configVersion, testGeneration int,
	overwriteVerification bool, verificationStatus int,
	lastTestResult *int, lastTestModel string, lastTestDurationMs int64, now time.Time,
) (bool, error) {
	setClause, args := verificationSetClause(overwriteVerification, verificationStatus)
	args = append(args, lastTestResult, lastTestModel, lastTestDurationMs, now, now,
		keyID, configVersion, testGeneration)

	query := `
		UPDATE provider_keys
		SET ` + setClause + `last_test_result = ?, last_test_model = ?,
		    last_test_duration_ms = ?, last_tested_at = ?, updated_at = ?
		WHERE id = ?
		  AND config_version = ?
		  AND test_generation = ?
		  AND authorized_destination_version = (SELECT destination_version FROM providers WHERE providers.id = provider_keys.provider_id)
	`
	return execReturningApplied(db, query, args...)
}

// MarkProviderKeyVerificationFailedIfCurrent is the gateway's CAS write to
// invalidate a provider key after a real upstream 401 (GATE-16). Unlike the
// M2 test-flow CAS functions above (CommitProviderKeyPlaintextTestResult /
// CommitProviderKeyRetestResult, which guard on config_version +
// test_generation because they belong to a plaintext/retest lifecycle), this
// guards only on verification_status=Passed AND authorized_destination_version
// = the destination the key was just sent to — i.e. "the key was valid for
// exactly this destination, and the upstream rejected the credential".
// CommitProviderKeyRetestResult's CAS does NOT check verification_status, so
// a gateway-invalidated key can still be recovered by a later M2 retest.
// Returns applied=false (no error) if the row no longer matches (concurrent
// edit, destination change, or already invalidated) — callers treat that as
// a benign lost race, not an error.
func MarkProviderKeyVerificationFailedIfCurrent(db *gorm.DB, keyID uint, expectedDestinationVersion int, now time.Time) (bool, error) {
	return execReturningApplied(db,
		`UPDATE provider_keys SET verification_status = ?, updated_at = ?
		 WHERE id = ? AND verification_status = ? AND authorized_destination_version = ?`,
		model.VerificationStatusFailed, now, keyID, model.VerificationStatusPassed, expectedDestinationVersion)
}
