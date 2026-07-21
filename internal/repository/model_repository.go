// Package repository provides Model / ModelCandidate pure data access.
package repository

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

func FindModelByID(db *gorm.DB, id uint) (*model.Model, error) {
	var m model.Model
	if err := db.Where("id = ?", id).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func FindModelByName(db *gorm.DB, name string) (*model.Model, error) {
	var m model.Model
	if err := db.Where("name = ?", name).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

func ListModels(db *gorm.DB) ([]model.Model, error) {
	var models []model.Model
	if err := db.Order("id ASC").Find(&models).Error; err != nil {
		return nil, err
	}
	return models, nil
}

func CreateModel(db *gorm.DB, m *model.Model) error {
	return db.Create(m).Error
}

func UpdateModelNameStatus(db *gorm.DB, id uint, name string, status int, now time.Time) error {
	return db.Model(&model.Model{}).Where("id = ?", id).
		Updates(map[string]interface{}{"name": name, "management_status": status, "updated_at": now}).Error
}

func ListModelCandidatesByModelID(db *gorm.DB, modelID uint) ([]model.ModelCandidate, error) {
	var candidates []model.ModelCandidate
	if err := db.Preload("Provider").Where("model_id = ?", modelID).Order("sort_order ASC").Find(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

// ListModelCandidatesByModelIDs batches the N+1 that a naive per-model
// candidate lookup would cause when listing models (the same fix used for
// ListProviderKeysByProviderIDs).
func ListModelCandidatesByModelIDs(db *gorm.DB, modelIDs []uint) ([]model.ModelCandidate, error) {
	if len(modelIDs) == 0 {
		return nil, nil
	}
	var candidates []model.ModelCandidate
	if err := db.Preload("Provider").Where("model_id IN ?", modelIDs).Order("model_id ASC, sort_order ASC").Find(&candidates).Error; err != nil {
		return nil, err
	}
	return candidates, nil
}

func FindModelCandidateByID(db *gorm.DB, id uint) (*model.ModelCandidate, error) {
	var c model.ModelCandidate
	if err := db.Where("id = ?", id).First(&c).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func NextCandidateSortOrder(db *gorm.DB, modelID uint) (int, error) {
	var maxOrder int
	if err := db.Model(&model.ModelCandidate{}).Where("model_id = ?", modelID).
		Select("COALESCE(MAX(sort_order), 0)").Scan(&maxOrder).Error; err != nil {
		return 0, err
	}
	return maxOrder + 1, nil
}

func CreateModelCandidate(db *gorm.DB, c *model.ModelCandidate) error {
	return db.Create(c).Error
}

func UpdateModelCandidate(db *gorm.DB, id uint, providerModelName string, inputPrice, outputPrice float64,
	cacheWritePrice, cacheReadPrice *float64, maxOutput int, resetVerification bool, now time.Time) error {
	updates := map[string]interface{}{
		"provider_model_name": providerModelName,
		"input_price":         inputPrice,
		"output_price":        outputPrice,
		"cache_write_price":   cacheWritePrice,
		"cache_read_price":    cacheReadPrice,
		"max_output":          maxOutput,
		"updated_at":          now,
	}
	if resetVerification {
		// The mapping test and capability probes validated the OLD
		// provider_model_name; a new name makes them stale, so clear them —
		// the candidate must be re-tested before it can route or be enabled
		// again. A map-based Updates writes these zero values (a struct-based
		// one would skip them).
		updates["verification_status"] = model.ModelVerificationStatusUntested
		updates["supports_streaming"] = false
		updates["supports_function_calling"] = false
	}
	return db.Model(&model.ModelCandidate{}).Where("id = ?", id).Updates(updates).Error
}

func SetModelCandidateManagementStatus(db *gorm.DB, id uint, status int, now time.Time) error {
	return db.Model(&model.ModelCandidate{}).Where("id = ?", id).
		Updates(map[string]interface{}{"management_status": status, "updated_at": now}).Error
}

// SetModelCandidateManagementStatusIfVerified CAS-guards enabling a
// candidate on verification_status still reading Passed — closes the
// check-then-act window between the service layer's gate
// check and an unconditional write (same fix, same reasoning applied here).
func SetModelCandidateManagementStatusIfVerified(db *gorm.DB, id uint, status int, now time.Time) (bool, error) {
	result := db.Model(&model.ModelCandidate{}).
		Where("id = ? AND verification_status = ?", id, model.ModelVerificationStatusPassed).
		Updates(map[string]interface{}{"management_status": status, "updated_at": now})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

func CommitModelCandidateBasicTestResult(db *gorm.DB, id uint, verificationStatus int, lastTestResult *int, durationMs int64, now time.Time) error {
	return db.Model(&model.ModelCandidate{}).Where("id = ?", id).Updates(map[string]interface{}{
		"verification_status":   verificationStatus,
		"last_test_result":      lastTestResult,
		"last_test_duration_ms": durationMs,
		"last_tested_at":        now,
		"updated_at":            now,
	}).Error
}

// CommitModelCandidateCapabilityTestResult writes the result of a
// streaming or function-calling test — these never touch
// verification_status: they're independent capability
// flags, not a re-run of the basic-text gate.
func CommitModelCandidateCapabilityTestResult(db *gorm.DB, id uint, capability string, passed bool, lastTestResult *int, durationMs int64, now time.Time) error {
	column := "supports_streaming"
	if capability == "function_calling" {
		column = "supports_function_calling"
	}
	return db.Model(&model.ModelCandidate{}).Where("id = ?", id).Updates(map[string]interface{}{
		column:                  passed,
		"last_test_result":      lastTestResult,
		"last_test_duration_ms": durationMs,
		"last_tested_at":        now,
		"updated_at":            now,
	}).Error
}

// SwapModelCandidateSortOrder atomically swaps sort_order between
// candidateID and its immediate neighbor in the given direction, scoped to
// modelID (a candidate's route-chain position is only meaningful within
// its own model). Same intermediate-negative-value pattern
// as SwapProviderKeySortOrder to avoid momentarily violating
// UNIQUE(model_id, sort_order) mid-swap.
func SwapModelCandidateSortOrder(db *gorm.DB, modelID, candidateID uint, direction string) (bool, error) {
	var applied bool
	err := db.Transaction(func(tx *gorm.DB) error {
		var current model.ModelCandidate
		if err := tx.Select("id, sort_order").Where("id = ? AND model_id = ?", candidateID, modelID).First(&current).Error; err != nil {
			return err
		}

		var neighbor model.ModelCandidate
		var neighborErr error
		if direction == "up" {
			neighborErr = tx.Select("id, sort_order").Where("model_id = ? AND sort_order < ?", modelID, current.SortOrder).
				Order("sort_order DESC").Limit(1).First(&neighbor).Error
		} else {
			neighborErr = tx.Select("id, sort_order").Where("model_id = ? AND sort_order > ?", modelID, current.SortOrder).
				Order("sort_order ASC").Limit(1).First(&neighbor).Error
		}
		if errors.Is(neighborErr, gorm.ErrRecordNotFound) {
			applied = false
			return nil
		}
		if neighborErr != nil {
			return neighborErr
		}

		const tempOrder = -1
		if err := tx.Model(&model.ModelCandidate{}).Where("id = ?", current.ID).Update("sort_order", tempOrder).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ModelCandidate{}).Where("id = ?", neighbor.ID).Update("sort_order", current.SortOrder).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.ModelCandidate{}).Where("id = ?", current.ID).Update("sort_order", neighbor.SortOrder).Error; err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func DeleteModelCandidate(db *gorm.DB, id uint) error {
	return db.Where("id = ?", id).Delete(&model.ModelCandidate{}).Error
}
