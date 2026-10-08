package audit

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
)

// detailContextKey is the gin-context seat where the explicit-event layer
// parks the (already redacted, already serialized) detail document for the
// request in flight. The middleware reads it once, at request exit, so a
// request that both matched the automatic layer and raised an explicit
// event still produces exactly one row — the merge contract between the
// two layers.
const detailContextKey = "audit.detail"

// SetDetail parks the request's audit detail JSON on the context. Called
// by the explicit-event layer (services that know business semantics the
// route shape cannot express: which fields changed and from what to what,
// which key was revealed). The middleware overwrites nothing: the last
// write before request exit wins, matching one-request-one-event.
func SetDetail(c *gin.Context, detailJSON string) {
	c.Set(detailContextKey, detailJSON)
}

// DetailFrom reads the detail parked on the context, if any. Exported so
// tests (and later the merge hook's own tests) can drive the seat without
// stringly-typed context access of their own.
func DetailFrom(c *gin.Context) string {
	if v, ok := c.Get(detailContextKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// ExplicitEvent is the explicit-event layer's contribution to the row the
// middleware writes at request exit: a finer-grained action than the route
// shape can express, a status the HTTP outcome cannot carry, and the
// (already redacted) detail document. Parking an event MERGES into the
// automatic layer's row — one request still produces exactly one record,
// now speaking the service's language instead of the router's.
//
// Zero fields keep their middleware-derived values: an event carrying only
// a Detail behaves exactly like SetDetail, an event with an Action but no
// Status keeps the outcome-derived status.
type ExplicitEvent struct {
	// Action replaces the route-derived action ("auth.login" becomes
	// "auth.login_denied"). ResourceType is recomputed from the override's
	// namespace; ResourceID keeps the path-derived value.
	Action string
	// Status optionally pins the row's status. The login outcomes use it to
	// fix "denied" by contract rather than by inference from whatever
	// envelope shape carries the refusal. Empty keeps the classification.
	// A panic always wins over this override — a stack unwind is an error
	// no matter what the handler had already concluded.
	Status string
	// Detail is the event's (already redacted) JSON document. It takes
	// precedence over a plain SetDetail in the same request: the explicit
	// event is the semantically richer document.
	Detail string
}

// explicitEventKey is the gin-context seat carrying the merged event.
const explicitEventKey = "audit.explicit_event"

// SetExplicitEvent parks the explicit event for the request in flight. The
// middleware reads it once, at request exit, in buildRecord.
func SetExplicitEvent(c *gin.Context, ev ExplicitEvent) {
	c.Set(explicitEventKey, ev)
}

// ExplicitEventFrom reads the parked event, if any. Exported so tests can
// drive the merge seat without stringly-typed context access.
func ExplicitEventFrom(c *gin.Context) (ExplicitEvent, bool) {
	v, ok := c.Get(explicitEventKey)
	if !ok {
		return ExplicitEvent{}, false
	}
	ev, ok := v.(ExplicitEvent)
	return ev, ok
}

// userAgentCap bounds the stored User-Agent. Console traffic sends
// browser UA strings well under this; a cap exists so a hostile client
// cannot turn the audit table into blob storage. Truncation is at the
// byte boundary of the string's first cap bytes, which for real UA
// strings never splits anything meaningful.
const userAgentCap = 512

// isWriteMethod filters what the automatic layer captures: every HTTP
// method that mutates. Reads are deliberately excluded — an audit trail
// of every console GET would bury the mutations it exists to record. The
// sensitive-read exceptions (key reveal) are explicit events, not
// middleware captures.
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// Middleware builds the automatic capture layer for the /api/admin group
// root. Mounted once at the root — not on any subgroup — so every console
// route shares it: default (admin-protected), session-only, ownership-
// scoped, and the public auth endpoints alike. Mounting it on a subgroup
// would silently blind the audit trail to exactly the endpoints where
// security events are most interesting (failed logins live on the public
// group).
//
// The middleware wraps the rest of the chain and records at request exit:
// whatever the chain did — succeeded, refused access, failed validation,
// even aborted before the handler — one record per matched write request
// is submitted. Submission is asynchronous and non-blocking (see Worker);
// nothing here does database work on the hot path except the one
// primary-key read that snapshots the acting account's username, which
// the row needs so history survives the account's later rename or
// deletion.
func Middleware(db *gorm.DB, worker *Worker) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !isWriteMethod(c.Request.Method) {
			c.Next()
			return
		}

		// Deferred so the record is also written when a handler panics:
		// the panic unwinds through this frame either way, and an audit
		// layer that goes quiet exactly when things catch fire would be
		// missing its most interesting rows. The recovered value is
		// re-panicked untouched — the engine-level recovery middleware
		// still owns turning it into a 500 envelope.
		defer func() {
			r := recover()
			panicked := r != nil
			worker.Submit(buildRecord(c, db, panicked))
			if panicked {
				panic(r)
			}
		}()
		c.Next()
	}
}

// buildRecord assembles the audit record from the finished request. Kept
// as a plain function (not a closure over the handler) so tests can drive
// it through the middleware exactly as production does.
func buildRecord(c *gin.Context, db *gorm.DB, panicked bool) Record {
	resolution := ResolveAction(c, c.Request.Method, c.FullPath())
	status, brief := classifyOutcome(panicked, c.Writer.Status())

	rec := Record{
		At:           time.Now().UTC(),
		Action:       resolution.Action,
		ResourceType: resolution.ResourceType,
		ResourceID:   resolution.ResourceID,
		Detail:       DetailFrom(c),
		Status:       status,
		ErrorBrief:   brief,
		SourceIP:     c.ClientIP(),
		UserAgent:    truncate(c.Request.UserAgent(), userAgentCap),
		RequestID:    requestID(c),
	}

	// Merge the explicit event, if the request raised one. Action and
	// detail carry the service's semantics; the status override yields to
	// a panic (a request that never finished has no outcome to pin).
	if ev, ok := ExplicitEventFrom(c); ok {
		if ev.Action != "" {
			rec.Action = ev.Action
			rec.ResourceType = actionNamespace(ev.Action)
		}
		if ev.Status != "" && !panicked {
			rec.Status = ev.Status
		}
		if ev.Detail != "" {
			rec.Detail = ev.Detail
		}
	}
	// Last gate before the row exists: every detail document passes the
	// redaction pipeline again here, whatever its origin. Callers are
	// contractually expected to redact before parking (RedactDetail), and
	// this second pass is what turns that convention into a guarantee.
	rec.Detail = sanitizeDetail(rec.Detail)

	// Identity: whatever the session middleware resolved during the
	// request. Absent for the public auth endpoints (no session exists
	// yet) and for unauthenticated probes — those rows keep a NULL
	// admin_user_id, which is itself evidence. The username is looked up
	// live and snapshotted into the row rather than joined at read time:
	// renaming or deleting the account later must not rewrite history.
	if v, ok := c.Get(middleware.UserIDKey); ok {
		if id, ok := v.(uint); ok {
			rec.AdminUserID = &id
			if user, err := repository.FindUserByID(db, id); err == nil {
				rec.AdminUsername = user.Username
			}
			// A lookup that fails (account deleted mid-session, database
			// hiccup) still records the id with an empty snapshot name —
			// losing the whole row over a display name would be the wrong
			// trade.
		}
	}
	return rec
}

// classifyOutcome maps the finished request to the audit status tri-state
// plus the optional short reason. Access-control refusals (401/403) are
// "denied" — the attempt is the signal. Every other non-2xx outcome,
// including validation failures, is "error" with a brief derived solely
// from the status code: the response body may quote request payloads and
// error internals, and error_brief must never become a second channel for
// those. A panic never has a meaningful status (the envelope has not been
// written yet when this frame unwinds), so it is classified directly.
func classifyOutcome(panicked bool, httpStatus int) (status, brief string) {
	if panicked {
		return model.AuditStatusError, "handler panic"
	}
	switch {
	case httpStatus < 400:
		return model.AuditStatusSuccess, ""
	case httpStatus == http.StatusUnauthorized, httpStatus == http.StatusForbidden:
		return model.AuditStatusDenied, ""
	default:
		return model.AuditStatusError, fmt.Sprintf("http_status=%d", httpStatus)
	}
}

// requestID returns the bare UUID the request-id middleware issued. The
// context value is authoritative; the response header is the same value
// by construction and serves as the fallback so the row still correlates
// if a future wiring change drops the context copy.
func requestID(c *gin.Context) string {
	if v, ok := c.Get(middleware.RequestIDKey); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return c.Writer.Header().Get("X-Request-Id")
}

// truncate returns s cut to at most limit bytes.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// sanitizeDetail is the redaction pipeline's enforcement point at the
// capture boundary: whatever detail reached the row gets one more pass
// through RedactJSON before it is submitted. A document that cannot even
// be parsed is dropped rather than stored — an unverifiable blob is
// exactly the shape a leaked secret would hide in, and losing one detail
// column costs far less than losing the guarantee.
func sanitizeDetail(doc string) string {
	if doc == "" {
		return ""
	}
	out, err := RedactJSON(doc)
	if err != nil {
		return ""
	}
	return out
}
