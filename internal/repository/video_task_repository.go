package repository

// The video task store. Deliberately thin: the state machine's rules
// (one-way transitions, who may flip what) live in the videotask service
// so there is one place that knows them; these are the reads and writes
// it is built from, each a single statement a transaction can compose.

import (
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
)

// videoTaskNonTerminal lists the statuses the state machine may still
// leave. Named once because it is the identity of the one-way rule —
// every guarded write matches against this list, and a second spelling
// would be a second rule.
var videoTaskNonTerminal = []string{model.VideoTaskPending, model.VideoTaskProcessing}

// ErrVideoTaskNotFound is the store's one miss: the caller asked for a
// task that does not exist — or exists under another key, which the
// ownership-filtered reads make indistinguishable on purpose.
var ErrVideoTaskNotFound = errors.New("video task not found")

// CreateVideoTask inserts one accepted task.
func CreateVideoTask(db *gorm.DB, task *model.VideoTask) error {
	return db.Create(task).Error
}

// FindVideoTaskForOwner reads one task under the owning key's filter.
func FindVideoTaskForOwner(db *gorm.DB, apiKeyID uint, id string) (*model.VideoTask, error) {
	var task model.VideoTask
	err := db.Where("api_key_id = ? AND id = ?", apiKeyID, id).First(&task).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrVideoTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// ClaimVideoTaskPoll compare-and-sets last_polled_at for a task that is
// due a poll: the update matches only while the stored stamp is still the
// one the caller read (nil meaning never polled), so two racing pollers
// cannot both believe they won — the loser's update affects zero rows and
// reports not-ok. The stamp is claimed BEFORE the upstream query: a query
// that never returns still costs the next poller its interval, which is
// the throttle working, and a lost claim leaves the winner's stamp in
// place rather than doubling the upstream traffic.
func ClaimVideoTaskPoll(db *gorm.DB, apiKeyID uint, id string, prev, next time.Time) (bool, error) {
	q := db.Model(&model.VideoTask{}).
		Where("api_key_id = ? AND id = ? AND status IN ?", apiKeyID, id, videoTaskNonTerminal)
	if prev.IsZero() {
		q = q.Where("last_polled_at IS NULL")
	} else {
		q = q.Where("last_polled_at = ?", prev)
	}
	tx := q.Updates(map[string]any{"last_polled_at": next, "updated_at": next})
	if tx.Error != nil {
		return false, tx.Error
	}
	return tx.RowsAffected > 0, nil
}

// SaveVideoTaskPollResult writes a poll's outcome under the state
// machine's one-way rule: the update matches only when the status is
// still non-terminal, so a transition that lost a race to a terminal
// write is discarded rather than resurrecting a finished task.
func SaveVideoTaskPollResult(db *gorm.DB, id string, result map[string]any, now time.Time) (bool, error) {
	tx := db.Model(&model.VideoTask{}).
		Where("id = ? AND status IN ?", id, videoTaskNonTerminal).
		Updates(result)
	if tx.Error != nil {
		return false, tx.Error
	}
	return tx.RowsAffected > 0, nil
}

// terminalVideoTaskUpdate is the one writer of a terminal transition's
// field set, so the status, its code, and its message cannot drift into
// three spellings across the paths that retire tasks.
func terminalVideoTaskUpdate(status, code, message string, now time.Time) map[string]any {
	return map[string]any{"status": status, "error_code": code, "error_message": message, "updated_at": now}
}

// ExpireStaleVideoTasks moves every non-terminal task past its zombie
// horizon to expired, and reports how many moved. Terminal rows are never
// touched and never deleted — expiry is a state transition, not cleanup.
func ExpireStaleVideoTasks(db *gorm.DB, now time.Time) (int64, error) {
	tx := db.Model(&model.VideoTask{}).
		Where("status IN ? AND expires_at IS NOT NULL AND expires_at < ?", videoTaskNonTerminal, now).
		Updates(terminalVideoTaskUpdate(model.VideoTaskExpired, "task_expired",
			"the upstream task window closed before completion", now))
	return tx.RowsAffected, tx.Error
}

// ExpireProviderInFlightVideoTasks is the provider-change hook: when a
// provider's destination changes (base URL, protocol), every task id that
// destination issued is unqueryable at the new one, so its non-terminal
// tasks are expired rather than left to poll a destination that cannot
// know them.
func ExpireProviderInFlightVideoTasks(db *gorm.DB, providerID uint, exceptDestinationVersion int, now time.Time) (int64, error) {
	tx := db.Model(&model.VideoTask{}).
		Where("provider_id = ? AND destination_version != ? AND status IN ?", providerID, exceptDestinationVersion, videoTaskNonTerminal).
		Updates(terminalVideoTaskUpdate(model.VideoTaskExpired, "provider_destination_changed",
			"the provider address changed after this task was submitted", now))
	return tx.RowsAffected, tx.Error
}

// ListVideoTasksFilter is the admin list's query shape. Every field is
// optional; zero values mean "no filter on this axis".
type ListVideoTasksFilter struct {
	APIKeyID uint
	ModelID  uint
	Status   string
	Page     int
	PageSize int
}

// ListVideoTasks reads one page of tasks, newest first.
func ListVideoTasks(db *gorm.DB, f ListVideoTasksFilter) ([]model.VideoTask, int64, error) {
	q := db.Model(&model.VideoTask{})
	if f.APIKeyID != 0 {
		q = q.Where("api_key_id = ?", f.APIKeyID)
	}
	if f.ModelID != 0 {
		q = q.Where("model_id = ?", f.ModelID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	pageSize := f.PageSize
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 50
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	var tasks []model.VideoTask
	err := q.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&tasks).Error
	return tasks, total, err
}

// SumInFlightVideoEstimated totals the submit-time cost bounds of one
// key's unfinished tasks: what the budget gate must reserve against, on
// top of what has already been spent. A completed-but-unsettled row
// counts too — its charge is coming, exactly once, and the window
// between the observation and the settle must not be a hole the next
// submit slips through. Unpriced tasks contribute nothing — there is no
// number to reserve.
func SumInFlightVideoEstimated(db *gorm.DB, apiKeyID uint) (int64, error) {
	var sum struct{ Total int64 }
	err := db.Model(&model.VideoTask{}).
		Select("COALESCE(SUM(estimated_micros), 0) AS total").
		Where("api_key_id = ? AND billed = ? AND (status IN ? OR status = ?)",
			apiKeyID, false, videoTaskNonTerminal, model.VideoTaskCompleted).
		Scan(&sum).Error
	return sum.Total, err
}

// ListUnbilledCompletedVideoTasks returns the rows the reaper's
// reconciliation pass settles: completed, never charged, in no
// particular order — the billed compare-and-set decides the winner, not
// the walk.
func ListUnbilledCompletedVideoTasks(db *gorm.DB) ([]model.VideoTask, error) {
	var tasks []model.VideoTask
	err := db.Where("status = ? AND billed = ?", model.VideoTaskCompleted, false).
		Limit(500).Find(&tasks).Error
	return tasks, err
}

// ChargeVideoTask settles one completed task exactly once: the billed
// compare-and-set claims the row, and only the claimer books the charge
// on the owning key's spend. Both writes share a transaction so a task
// can never read billed while its charge is missing. Returns false when
// the row was already settled — the caller treats that as idempotent
// success, not a race lost.
func ChargeVideoTask(db *gorm.DB, taskID string, micros int64, now time.Time) (bool, error) {
	var applied bool
	err := db.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.VideoTask{}).
			Where("id = ? AND billed = ? AND status = ?", taskID, false, model.VideoTaskCompleted).
			Updates(map[string]any{"billed": true, "billed_micros": micros, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		applied = res.RowsAffected > 0
		if !applied {
			return nil
		}
		if micros == 0 {
			return nil
		}
		return tx.Model(&model.APIKey{}).
			Where("id = (SELECT api_key_id FROM video_tasks WHERE id = ?)", taskID).
			Update("budget_spent_micros", gorm.Expr("budget_spent_micros + ?", micros)).Error
	})
	return applied, err
}
