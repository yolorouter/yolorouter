package videos

// The job resource half of the dialect: what a caller polls after the
// create call returns. The wire status vocabulary is four values because
// the official SDK's model types it as a strict literal — sending a fifth
// value the gateway might prefer (cancelled, expired) fails parsing on
// the client, not gracefully. Those two gateway-internal states therefore
// travel as failed plus a machine-readable error code, which is also how
// the caller learns which of the two it was.

import "strconv"

// Task status values as the gateway models them internally — the task
// domain's state machine speaks these. Six states: four that map onto
// the wire one to one, and cancelled/expired, which the wire cannot say.
const (
	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusCompleted  = "completed"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusExpired    = "expired"
)

// The wire status vocabulary: exactly the four values the official SDK's
// strict typing accepts, and no more.
const (
	WireQueued     = "queued"
	WireInProgress = "in_progress"
	WireCompleted  = "completed"
	WireFailed     = "failed"
)

// Error codes the gateway puts in a failed resource's error payload to
// say which internal terminal state the caller is looking at. These are
// stable identifiers a client can branch on, not prose.
const (
	ErrCodeTaskCancelled = "task_cancelled"
	ErrCodeTaskExpired   = "task_expired"
)

// MapWireStatus maps an internal task status onto the wire vocabulary.
// The returned error code is empty unless the wire status hides an
// internal one — the cancelled and expired cases — in which case it says
// which. An unknown internal status maps to failed with no code rather
// than to a wire value the SDK would reject.
func MapWireStatus(taskStatus string) (wire, errCode string) {
	switch taskStatus {
	case StatusPending:
		return WireQueued, ""
	case StatusProcessing:
		return WireInProgress, ""
	case StatusCompleted:
		return WireCompleted, ""
	case StatusCancelled:
		return WireFailed, ErrCodeTaskCancelled
	case StatusExpired:
		return WireFailed, ErrCodeTaskExpired
	default:
		return WireFailed, ""
	}
}

// ResourceError is the error payload of a failed job, in the two fields
// the API defines: a machine-readable code and a human-readable message.
type ResourceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Resource is the video job object, field for field the API's shape —
// including the fields the API marks required even when their value is
// null, because a caller's strict decoder rejects their absence, not
// their emptiness. Seconds is a string on this object even though the
// create call sends an integer: the API defines it that way, and the
// SDK's typing follows.
type Resource struct {
	ID        string `json:"id"`
	Object    string `json:"object"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Progress  int    `json:"progress"`
	CreatedAt int64  `json:"created_at"`
	// CompletedAt, ExpiresAt and RemixedFromVideoID are nullable required
	// fields: pointers so an unset one renders as JSON null rather than
	// being omitted.
	CompletedAt        *int64         `json:"completed_at"`
	ExpiresAt          *int64         `json:"expires_at"`
	Prompt             *string        `json:"prompt"`
	Size               string         `json:"size"`
	Seconds            string         `json:"seconds"`
	RemixedFromVideoID *string        `json:"remixed_from_video_id"`
	Error              *ResourceError `json:"error"`
}

// NewResource renders one job in the state the caller just made of it:
// queued, nothing done, no nullable field set beyond the prompt the
// caller sent. Every later field is the poll path's to fill.
func NewResource(id, model, prompt, size string, seconds int, createdAt int64) Resource {
	return Resource{
		ID:        id,
		Object:    "video",
		Model:     model,
		Status:    WireQueued,
		Progress:  0,
		CreatedAt: createdAt,
		Prompt:    &prompt,
		Size:      size,
		Seconds:   strconv.Itoa(seconds),
	}
}
