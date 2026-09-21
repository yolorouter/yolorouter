package repository

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/errcode"

	"gorm.io/gorm"
)

// GetCustomSystemPrompt reads both CSP rows in a single query and validates
// atomicity: exactly 2 rows, equal version, and a strictly-parsed enabled.
// Two separate queries under READ COMMITTED could tear (enabled=N, text=N+1),
// and a wrong version could be accepted by the monotonic cache for a long time.
func GetCustomSystemPrompt(db *gorm.DB) (settings.CustomSystemPromptSetting, int64, error) {
	var rows []struct {
		Key     string
		Value   string
		Version int64
	}
	if err := db.Table("system_settings").
		Select("key, value, version").
		Where("key IN ?", []string{"custom_system_prompt_enabled", "custom_system_prompt"}).
		Find(&rows).Error; err != nil {
		return settings.CustomSystemPromptSetting{}, 0, err
	}
	if len(rows) != 2 {
		return settings.CustomSystemPromptSetting{}, 0, fmt.Errorf("system_settings: expected 2 rows, got %d", len(rows))
	}
	var s settings.CustomSystemPromptSetting
	ver := rows[0].Version
	for _, r := range rows {
		if r.Version != ver {
			return settings.CustomSystemPromptSetting{}, 0, errors.New("system_settings: version mismatch between rows")
		}
		switch r.Key {
		case "custom_system_prompt_enabled":
			switch r.Value {
			case "true":
				s.Enabled = true
			case "false":
				s.Enabled = false
			default:
				return settings.CustomSystemPromptSetting{}, 0, fmt.Errorf("system_settings: corrupt enabled value %q", r.Value)
			}
		case "custom_system_prompt":
			s.Text = r.Value
		}
	}
	return s, ver, nil
}

// inputCompressionKey is the single system_settings row holding the global
// input-compression switch. Its value is strictly "true" or "false"; the row
// is seeded by migrations/sqlite/00016 and migrations/postgres/00016.
const inputCompressionKey = "input_compression_enabled"

// GetInputCompression reads the single input_compression_enabled row and
// returns (enabled, version, nil). A missing row is treated as the implicit
// default (false, 0) so a not-yet-migrated database behaves as disabled
// without erroring — the gateway read path must never fail-closed on this.
// A value outside {"true","false"} is corrupt data and surfaces as an error.
func GetInputCompression(db *gorm.DB) (enabled bool, version int64, err error) {
	var row struct {
		Value   string
		Version int64
	}
	if err = db.Table("system_settings").
		Select("value, version").
		Where("key = ?", inputCompressionKey).
		Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	switch row.Value {
	case "true":
		return true, row.Version, nil
	case "false":
		return false, row.Version, nil
	default:
		return false, 0, fmt.Errorf("system_settings: corrupt %s value %q", inputCompressionKey, row.Value)
	}
}

// UpdateInputCompression CAS-updates the single input_compression_enabled row:
//
//	UPDATE system_settings
//	SET value = ?, version = version + 1
//	WHERE key = 'input_compression_enabled' AND version = ?
//
// RowsAffected == 1 means the CAS held and the row is now at expectedVersion+1;
// anything else means another writer committed first => conflict. Returns the
// committed value + new version so the handler can hand the fresh version back
// to the caller; a second save with the stale version would otherwise always
// conflict.
func UpdateInputCompression(db *gorm.DB, expectedVersion int64, enabled bool) (bool, int64, error) {
	value := "false"
	if enabled {
		value = "true"
	}
	res := db.Table("system_settings").
		Where("key = ? AND version = ?", inputCompressionKey, expectedVersion).
		Updates(map[string]interface{}{
			"value":   value,
			"version": gorm.Expr("version + 1"),
		})
	if res.Error != nil {
		return false, 0, res.Error
	}
	if res.RowsAffected != 1 {
		return false, 0, errcode.ErrInputCompressionConflict
	}
	return enabled, expectedVersion + 1, nil
}

// UpdateCustomSystemPrompt CAS-upserts both rows in ONE statement:
//
//	UPDATE system_settings
//	SET value = CASE key WHEN 'custom_system_prompt_enabled' THEN ?
//	                     WHEN 'custom_system_prompt'        THEN ? END,
//	    version = version + 1
//	WHERE key IN (?, ?) AND version = ?
//
// Both rows share a version (the read path enforces this), so a single WHERE
// on version is correct and RowsAffected == 2 means the CAS held atomically —
// a concurrent writer that bumped only one row is impossible under the read
// invariant. Anything other than 2 rows affected => another writer committed
// first => conflict. Returns the committed snapshot + new version so the
// handler can hand the fresh version back to the caller; a second save with
// the stale version would otherwise always conflict.
func UpdateCustomSystemPrompt(db *gorm.DB, expectedVersion int64, enabled bool, text string) (settings.CustomSystemPromptSetting, int64, error) {
	enabledVal := "false"
	if enabled {
		enabledVal = "true"
	}
	const (
		keyEnabled = "custom_system_prompt_enabled"
		keyText    = "custom_system_prompt"
	)
	var newVersion int64
	err := db.Transaction(func(tx *gorm.DB) error {
		// CASE-driven SET so a single UPDATE writes the per-key value while
		// still hitting both rows with one WHERE clause. The two keys are the
		// only rows at this version, so RowsAffected == 2 is the CAS witness.
		res := tx.Table("system_settings").
			Where("key IN ? AND version = ?", []string{keyEnabled, keyText}, expectedVersion).
			Updates(map[string]interface{}{
				"value":   gorm.Expr("CASE key WHEN ? THEN ? WHEN ? THEN ? END", keyEnabled, enabledVal, keyText, text),
				"version": gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 2 {
			return errcode.ErrCustomSystemPromptConflict
		}
		newVersion = expectedVersion + 1
		return nil
	})
	if err != nil {
		return settings.CustomSystemPromptSetting{}, 0, err
	}
	return settings.CustomSystemPromptSetting{Enabled: enabled, Text: text}, newVersion, nil
}

// The vision-fallback settings pair is seeded by migration 00022 and shares
// one version, same contract as the custom-system-prompt pair above.
const (
	visionFallbackModelKey  = "vision_fallback_model"
	visionFallbackPromptKey = "vision_fallback_prompt"
)

// GetVisionFallback reads both vision-fallback rows as one snapshot. Missing
// rows (a database migrated before 00022 mid-rollout) degrade to the disabled
// default rather than erroring — the gateway read path must never fail-closed
// on configuration.
func GetVisionFallback(db *gorm.DB) (settings.VisionFallbackSetting, int64, error) {
	var rows []struct {
		Key     string
		Value   string
		Version int64
	}
	if err := db.Table("system_settings").
		Select("key, value, version").
		Where("key IN ?", []string{visionFallbackModelKey, visionFallbackPromptKey}).
		Find(&rows).Error; err != nil {
		return settings.VisionFallbackSetting{}, 0, err
	}
	if len(rows) == 0 {
		return settings.VisionFallbackSetting{}, 0, nil
	}
	if len(rows) != 2 {
		return settings.VisionFallbackSetting{}, 0, fmt.Errorf("system_settings: expected 2 vision_fallback rows, got %d", len(rows))
	}
	var s settings.VisionFallbackSetting
	ver := rows[0].Version
	for _, r := range rows {
		if r.Version != ver {
			return settings.VisionFallbackSetting{}, 0, errors.New("system_settings: version mismatch between vision_fallback rows")
		}
		switch r.Key {
		case visionFallbackModelKey:
			s.Model = r.Value
		case visionFallbackPromptKey:
			s.Prompt = r.Value
		}
	}
	return s, ver, nil
}

// UpdateVisionFallback CAS-updates both rows in one statement; RowsAffected
// == 2 is the CAS witness (both rows share the version, enforced by the read
// path). Returns the committed snapshot + new version.
func UpdateVisionFallback(db *gorm.DB, expectedVersion int64, model, prompt string) (settings.VisionFallbackSetting, int64, error) {
	var newVersion int64
	err := db.Transaction(func(tx *gorm.DB) error {
		res := tx.Table("system_settings").
			Where("key IN ? AND version = ?", []string{visionFallbackModelKey, visionFallbackPromptKey}, expectedVersion).
			Updates(map[string]interface{}{
				"value":   gorm.Expr("CASE key WHEN ? THEN ? WHEN ? THEN ? END", visionFallbackModelKey, model, visionFallbackPromptKey, prompt),
				"version": gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 2 {
			return errcode.ErrVisionFallbackConflict
		}
		newVersion = expectedVersion + 1
		return nil
	})
	if err != nil {
		return settings.VisionFallbackSetting{}, 0, err
	}
	return settings.VisionFallbackSetting{Model: model, Prompt: prompt}, newVersion, nil
}

// ClearVisionFallbackModel follows a model deletion into the vision-fallback
// setting: when the stored describe model is the deleted one, the reference
// is cleared so the setting cannot point at a model that no longer exists —
// an empty model means the feature is off, the same state as never having
// configured it. The prompt text survives: it says how to describe images,
// not which model does it, so the next configure reuses it. A no-match is a
// clean no-op — most deletions aren't the fallback model. Same shared-version
// advance as RenameVisionFallbackModel, for the same CAS/cache reasons.
func ClearVisionFallbackModel(db *gorm.DB, name string) error {
	if name == "" {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		res := tx.Table("system_settings").
			Where("key = ? AND value = ?", visionFallbackModelKey, name).
			Update("value", "")
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		return tx.Table("system_settings").
			Where("key IN ?", []string{visionFallbackModelKey, visionFallbackPromptKey}).
			Update("version", gorm.Expr("version + 1")).Error
	})
}

// RenameVisionFallbackModel follows a model rename into the vision-fallback
// setting: when the stored describe model is the renamed one, the reference
// is rewritten and the pair's shared version advances so CAS writers and
// cache refreshes see the change. A no-match is a clean no-op — most renames
// aren't the fallback model.
func RenameVisionFallbackModel(db *gorm.DB, oldName, newName string) error {
	if oldName == "" || oldName == newName {
		return nil
	}
	return db.Transaction(func(tx *gorm.DB) error {
		res := tx.Table("system_settings").
			Where("key = ? AND value = ?", visionFallbackModelKey, oldName).
			Update("value", newName)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		// The version bump must land on BOTH rows of the pair: the read path
		// rejects a version mismatch as corruption, and the CAS save needs
		// both rows at one version — bumping only the rewritten row would
		// leave the setting unreadable and unsavable in the same stroke.
		return tx.Table("system_settings").
			Where("key IN ?", []string{visionFallbackModelKey, visionFallbackPromptKey}).
			Update("version", gorm.Expr("version + 1")).Error
	})
}

// The key-auto-recovery settings pair is seeded by migration 00048 and
// shares one version, same contract as the custom-system-prompt pair.
const (
	keyAutoRecoveryEnabledKey  = "key_auto_recovery_enabled"
	keyAutoRecoveryIntervalKey = "key_auto_recovery_interval_minutes"
)

// GetKeyAutoRecovery reads both key-auto-recovery rows as one snapshot.
// Missing rows (a database migrated before 00048, where seeding has not
// run yet) degrade to the shipped default rather than erroring — the
// background loop's read path must never fail-closed on configuration.
// A lone row of the pair, a version mismatch, a non-boolean enabled, or a
// non-integer / negative interval is corrupt data and surfaces as an
// error. The interval deliberately carries NO positive floor here: the
// 1..1440 bounds are the write path's rule (service-layer validation on
// every update), and a hand-edited 0 is honored as "scan point always
// reached" instead of being treated as corruption.
func GetKeyAutoRecovery(db *gorm.DB) (settings.KeyAutoRecoverySetting, int64, error) {
	var rows []struct {
		Key     string
		Value   string
		Version int64
	}
	if err := db.Table("system_settings").
		Select("key, value, version").
		Where("key IN ?", []string{keyAutoRecoveryEnabledKey, keyAutoRecoveryIntervalKey}).
		Find(&rows).Error; err != nil {
		return settings.KeyAutoRecoverySetting{}, 0, err
	}
	if len(rows) == 0 {
		return settings.DefaultKeyAutoRecoverySetting(), 0, nil
	}
	if len(rows) != 2 {
		return settings.KeyAutoRecoverySetting{}, 0, fmt.Errorf("system_settings: expected 2 key_auto_recovery rows, got %d", len(rows))
	}
	var s settings.KeyAutoRecoverySetting
	ver := rows[0].Version
	for _, r := range rows {
		if r.Version != ver {
			return settings.KeyAutoRecoverySetting{}, 0, errors.New("system_settings: version mismatch between key_auto_recovery rows")
		}
		switch r.Key {
		case keyAutoRecoveryEnabledKey:
			switch r.Value {
			case "true":
				s.Enabled = true
			case "false":
				s.Enabled = false
			default:
				return settings.KeyAutoRecoverySetting{}, 0, fmt.Errorf("system_settings: corrupt %s value %q", keyAutoRecoveryEnabledKey, r.Value)
			}
		case keyAutoRecoveryIntervalKey:
			n, err := strconv.Atoi(r.Value)
			if err != nil || n < 0 {
				return settings.KeyAutoRecoverySetting{}, 0, fmt.Errorf("system_settings: corrupt %s value %q", keyAutoRecoveryIntervalKey, r.Value)
			}
			s.IntervalMinutes = n
		}
	}
	return s, ver, nil
}

// UpdateKeyAutoRecovery CAS-updates both rows in one statement; RowsAffected
// == 2 is the CAS witness (both rows share the version, enforced by the read
// path — same shape as UpdateCustomSystemPrompt). Returns the committed
// snapshot + new version so the handler can hand the fresh version back to
// the caller; a second save with the stale version would otherwise always
// conflict.
func UpdateKeyAutoRecovery(db *gorm.DB, expectedVersion int64, enabled bool, intervalMinutes int) (settings.KeyAutoRecoverySetting, int64, error) {
	enabledVal := "false"
	if enabled {
		enabledVal = "true"
	}
	intervalVal := strconv.Itoa(intervalMinutes)
	var newVersion int64
	err := db.Transaction(func(tx *gorm.DB) error {
		res := tx.Table("system_settings").
			Where("key IN ? AND version = ?", []string{keyAutoRecoveryEnabledKey, keyAutoRecoveryIntervalKey}, expectedVersion).
			Updates(map[string]interface{}{
				"value":   gorm.Expr("CASE key WHEN ? THEN ? WHEN ? THEN ? END", keyAutoRecoveryEnabledKey, enabledVal, keyAutoRecoveryIntervalKey, intervalVal),
				"version": gorm.Expr("version + 1"),
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 2 {
			return errcode.ErrKeyAutoRecoveryConflict
		}
		newVersion = expectedVersion + 1
		return nil
	})
	if err != nil {
		return settings.KeyAutoRecoverySetting{}, 0, err
	}
	return settings.KeyAutoRecoverySetting{Enabled: enabled, IntervalMinutes: intervalMinutes}, newVersion, nil
}
