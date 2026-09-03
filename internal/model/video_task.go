package model

import "time"

// VideoTask is one accepted video generation job: the durable half of a
// create call. The submit request that created it is over the moment it
// returned a job id; everything after that — status, results, settlement —
// hangs off this row, which is why it carries its own snapshots (the
// caller's model name, the candidate's upstream name, the destination
// version the upstream task id belongs to) rather than joining rows that
// an edit could change under it.
//
// Status is the six-state machine the dialect layer maps onto the wire's
// four; transitions are one-way (see the videotask service), and terminal
// rows are never deleted — the row is the billing evidence, in the same
// spirit request_logs rows are kept forever.
type VideoTask struct {
	// ID is the caller-facing job id (vid_ + random), unguessable by
	// construction; ownership checks answer 404 rather than 403 so a
	// foreign id does not even confirm existence.
	ID string `gorm:"primaryKey;type:varchar(32)" json:"id"`
	// APIKeyID owns the task: the key that created it is the only key
	// that may see it.
	APIKeyID uint `gorm:"index;not null" json:"api_key_id"`
	// ModelID and ModelName snapshot the routing target as the caller
	// asked for it; CandidateID and ProviderModelName snapshot what was
	// actually submitted. An operator renaming or re-pointing either side
	// later must not rewrite history the settlement already priced.
	ModelID           uint   `gorm:"not null" json:"model_id"`
	ModelName         string `gorm:"type:varchar(200);not null;default:''" json:"model_name"`
	CandidateID       uint   `gorm:"not null" json:"candidate_id"`
	ProviderID        uint   `gorm:"index;not null" json:"provider_id"`
	ProviderModelName string `gorm:"type:varchar(200);not null;default:''" json:"provider_model_name"`
	// ProviderTaskID is the upstream's task identifier; querying it is
	// only meaningful against the destination recorded in
	// DestinationVersion (a provider address change strands every task id
	// the old destination issued — the change hook expires them).
	ProviderTaskID     string `gorm:"type:varchar(128);not null;default:''" json:"provider_task_id"`
	DestinationVersion int    `gorm:"not null;default:1" json:"destination_version"`
	// RequestID names the request_logs row this task's submit wrote, so
	// settlement can back-fill that row's cost minutes or days after the
	// request itself ended. Empty on tasks created before the column
	// existed — those settle without a projection, exactly as before.
	RequestID string `gorm:"type:varchar(64);not null;default:''" json:"request_id"`

	Status       string `gorm:"type:varchar(20);not null;default:'pending';index" json:"status"`
	ErrorCode    string `gorm:"type:varchar(50);not null;default:''" json:"error_code"`
	ErrorMessage string `gorm:"not null;default:''" json:"error_message"`

	// RequestSnapshot is the sanitized create body as the audit renderer
	// wrote it — shape without pixels.
	RequestSnapshot string `gorm:"not null;default:''" json:"request_snapshot"`
	// Size and Seconds are the request's pricing axes as the caller
	// phrased them (dialect spellings, defaults already applied).
	Size    string `gorm:"type:varchar(20);not null;default:''" json:"size"`
	Seconds int    `gorm:"not null;default:0" json:"seconds"`

	ResultURL string `gorm:"not null;default:''" json:"result_url"`
	// CoverURL is part of the querier contract because task dialects in
	// the survey set report one; the wan dialect does not, so the column
	// stays empty on this build's completed tasks.
	CoverURL string `gorm:"not null;default:''" json:"cover_url"`
	// UsageSeconds is the observed billable duration the upstream
	// reported (normalized by the querier); zero until completion.
	UsageSeconds int `gorm:"not null;default:0" json:"usage_seconds"`

	// EstimatedMicros is the exact upper bound known at submit time
	// (seconds × snapshot sell price): the budget gate sums it over
	// unfinished tasks. Billed/BilledMicros are the settlement's, written
	// once under the billed compare-and-set. All three are the settlement
	// ticket's to write — the columns land with the task domain so the
	// row's shape is stable from the first migration, and stay zero until
	// that ticket wires pricing into the submit path.
	EstimatedMicros int64 `gorm:"not null;default:0" json:"estimated_micros"`
	Billed          bool  `gorm:"not null;default:false" json:"billed"`
	BilledMicros    int64 `gorm:"not null;default:0" json:"billed_micros"`

	// ExpiresAt is the zombie horizon: the time after which the upstream
	// window is assumed closed and a non-terminal task may be expired
	// without another query. Not the asset-URL expiry the wire's
	// expires_at reports — that is the upstream result URL's own clock.
	ExpiresAt *time.Time `gorm:"index" json:"expires_at"`
	// LastPolledAt throttles and audits the lazy poll path.
	LastPolledAt *time.Time `json:"last_polled_at"`

	UpstreamSubmittedAt time.Time  `gorm:"not null" json:"upstream_submitted_at"`
	UpstreamCompletedAt *time.Time `json:"upstream_completed_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// The task state machine. The spellings match the dialect layer's
// wire-mapping inputs one for one (internal/protocols/videos); the
// equality is pinned by the videotask service's tests rather than a
// package dependency, so the model layer keeps importing nothing.
const (
	VideoTaskPending    = "pending"
	VideoTaskProcessing = "processing"
	VideoTaskCompleted  = "completed"
	VideoTaskFailed     = "failed"
	VideoTaskCancelled  = "cancelled"
	VideoTaskExpired    = "expired"
)

// VideoTaskTerminal reports whether a status is one the state machine
// never leaves. A terminal task is not polled again and is not deleted.
func VideoTaskTerminal(status string) bool {
	switch status {
	case VideoTaskCompleted, VideoTaskFailed, VideoTaskCancelled, VideoTaskExpired:
		return true
	}
	return false
}

func (VideoTask) TableName() string { return "video_tasks" }
