// Package handler addition for the console Playground run endpoint: the
// SSE aggregate channel that turns console sessions into real relay
// exchanges. Pre-relay failures (bind errors, key ownership/state) answer
// with the console JSON envelope and a real HTTP status; everything after
// the first event is on the stream, where each column speaks for itself —
// a relay failure is that column's native error shape, not a console
// envelope.
package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/service/playground"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// playgroundMessage is one chat turn of the text-modality request body.
// Typed structs end to end (here, the service, the body builder) are what
// makes the upstream parameter whitelist airtight: a message cannot carry
// an extra JSON member because none of these shapes has anywhere to put
// one.
type playgroundMessage struct {
	Role    string `json:"role" binding:"required,oneof=user assistant system"`
	Content string `json:"content" binding:"required"`
}

// playgroundTextRequest is the modality-shaped request body for text runs.
type playgroundTextRequest struct {
	Messages []playgroundMessage `json:"messages" binding:"required,min=1,dive"`
}

// playgroundTextParams are the text modality's exposed parameters. They
// are applied uniformly to every column of the run (the comparison varies
// the model, not the sampling), and only these reach the upstream request
// body.
type playgroundTextParams struct {
	System      *string  `json:"system" binding:"omitempty,max=32000"`
	Temperature *float64 `json:"temperature" binding:"omitempty,gte=0,lte=2"`
	MaxTokens   *int     `json:"max_tokens" binding:"omitempty,gte=1,lte=100000"`
}

// playgroundRunRequest is the run contract: whose key to spend, which
// modality's shape the request body has, which models to compare (1–4
// columns), and the parameter set applied to all of them.
type playgroundRunRequest struct {
	KeyID    uint                  `json:"key_id" binding:"required"`
	Modality string                `json:"modality" binding:"omitempty,oneof=text"`
	Models   []string              `json:"models" binding:"required,min=1,max=4,dive,required"`
	Request  playgroundTextRequest `json:"request"`
	Params   playgroundTextParams  `json:"params"`
}

// The aggregate channel's event payloads. slot is the column's stable
// position in the models list; every event names its slot so a consumer
// can route interleaved events to the right column without guessing.

// sseChunkEvent carries one text delta for a column. A non-streaming relay
// exchange delivers the whole text as one delta followed by its done
// event; a streamed exchange delivers many.
type sseChunkEvent struct {
	Slot  int    `json:"slot"`
	Model string `json:"model"`
	Delta string `json:"delta"`
}

// sseDoneEvent is a column's terminal success reading: the relay's own
// status, the audit row's id (the request-log page's key for this run),
// and the usage/cost/latency figures the audit row settled on.
type sseDoneEvent struct {
	Slot       int                    `json:"slot"`
	Model      string                 `json:"model"`
	Status     int                    `json:"status"`
	RequestID  string                 `json:"request_id"`
	Usage      playground.ColumnUsage `json:"usage"`
	CostMicros int64                  `json:"cost_micros"`
	CostKnown  bool                   `json:"cost_known"`
	DurationMs int64                  `json:"duration_ms"`
}

// sseErrorEvent is a column's terminal failure. Error is the relay's
// response body embedded verbatim — the exact native error shape a real
// caller of the same ingress would have received, so the column can show
// quota, rate-limit, and upstream failures the way the API reports them.
type sseErrorEvent struct {
	Slot      int             `json:"slot"`
	Model     string          `json:"model"`
	Status    int             `json:"status"`
	RequestID string          `json:"request_id"`
	Error     json.RawMessage `json:"error"`
}

// writePlaygroundEvent writes one aggregate-channel SSE event. A marshal
// failure skips the event rather than the stream: a payload json.Marshal
// refuses costs its own event — never the stream, never a panic — and every
// payload the run path here passes is a plain struct or pre-encoded JSON,
// so only a direct caller can reach the skip
// (TestWritePlaygroundEventMarshalFailure pins exactly that contract).
func writePlaygroundEvent(c *gin.Context, event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = c.Writer.WriteString("event: " + event + "\ndata: " + string(data) + "\n\n")
}

// textDelta extracts the assistant text out of an OpenAI-shaped chat
// completion response body. An empty answer (no choices, a non-string
// content, a body that is not JSON at all) is "" — the caller emits no
// chunk for it, and the done event still carries the settled figures.
func textDelta(body []byte) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	if len(parsed.Choices) == 0 {
		return ""
	}
	return parsed.Choices[0].Message.Content
}

// PostPlaygroundRun handles POST /api/admin/playground/run — the console
// Playground's aggregate channel. The run executes each column through the
// real relay pipeline in process (the service's own comments carry the
// chain rationale); this handler owns the console contract: session
// identity, key ownership, the stream, and mapping each column's relay
// outcome onto the event family.
//
// Columns currently run one after another and the stream is written as
// each finishes — the non-streaming skeleton. Concurrent per-column
// streaming arrives with the streaming channel; the event contract (slot
// on every event) is already the one that mode speaks.
func PostPlaygroundRun(svc *playground.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req playgroundRunRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.ParamError(c, err.Error())
			return
		}
		if req.Modality == "" {
			req.Modality = "text"
		}

		viewerID := c.MustGet(middleware.UserIDKey).(uint)
		key, err := svc.ResolveKey(viewerID, req.KeyID)
		if err != nil {
			switch err {
			case playground.ErrKeyNotOwned:
				// One answer for "missing" and "someone else's": the
				// endpoint must not confirm which key ids exist under
				// other accounts.
				response.Forbidden(c, errcode.APIKeyNotFound, errcode.GetMessage(errcode.APIKeyNotFound))
			case playground.ErrKeyRevoked:
				response.Unauthorized(c, errcode.APIKeyRevoked, errcode.GetMessage(errcode.APIKeyRevoked))
			case playground.ErrKeyExpired:
				response.Unauthorized(c, errcode.APIKeyExpired, errcode.GetMessage(errcode.APIKeyExpired))
			case playground.ErrOwnerDisabled:
				response.Unauthorized(c, errcode.AccountDisabled, errcode.GetMessage(errcode.AccountDisabled))
			default:
				response.InternalError(c, err.Error())
			}
			return
		}

		// The stream starts only once the console-side gates have passed,
		// so every pre-stream failure keeps its real HTTP status and JSON
		// envelope. From here on the channel itself is the response: 200,
		// event-stream, one terminal event per column.
		c.Header("Content-Type", "text/event-stream; charset=utf-8")
		c.Header("Cache-Control", "no-cache")
		c.Header("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)
		c.Writer.Flush()

		run := playground.TextRun{
			Messages:    make([]playground.Message, len(req.Request.Messages)),
			System:      derefString(req.Params.System),
			Temperature: req.Params.Temperature,
			MaxTokens:   req.Params.MaxTokens,
		}
		for i, m := range req.Request.Messages {
			run.Messages[i] = playground.Message{Role: m.Role, Content: m.Content}
		}

		for slot, model := range req.Models {
			run.Model = model
			// The console caller's context is the relay context's parent:
			// a caller disconnect cancels the in-flight column the same
			// way a gateway caller's disconnect cancels its attempt.
			result, err := svc.RunText(c.Request.Context(), key, run)
			if err != nil {
				// Pre-relay failure (body build, key decrypt): nothing ran,
				// so there is no native shape to embed. The column gets a
				// terminal error naming the failure; the rest of the run
				// proceeds.
				writePlaygroundEvent(c, "error", sseErrorEvent{
					Slot: slot, Model: model, Status: http.StatusInternalServerError,
					Error: json.RawMessage(`{"message":"playground run failed: ` + jsonEscape(err.Error()) + `"}`),
				})
				c.Writer.Flush()
				continue
			}
			audit, auditErr := svc.Audit(result.RequestID)
			if auditErr != nil {
				writePlaygroundEvent(c, "error", sseErrorEvent{
					Slot: slot, Model: model, Status: http.StatusInternalServerError,
					RequestID: result.RequestID,
					Error:     json.RawMessage(`{"message":"playground audit row unavailable"}`),
				})
				c.Writer.Flush()
				continue
			}
			if result.Status >= 200 && result.Status < 300 {
				if delta := textDelta(result.Body); delta != "" {
					writePlaygroundEvent(c, "chunk", sseChunkEvent{Slot: slot, Model: model, Delta: delta})
				}
				writePlaygroundEvent(c, "done", sseDoneEvent{
					Slot: slot, Model: model, Status: result.Status, RequestID: result.RequestID,
					Usage: audit.Usage, CostMicros: audit.CostMicros, CostKnown: audit.CostKnown,
					DurationMs: audit.DurationMs,
				})
			} else {
				writePlaygroundEvent(c, "error", sseErrorEvent{
					Slot: slot, Model: model, Status: result.Status, RequestID: result.RequestID,
					Error: nativeErrorBody(result.Body),
				})
			}
			c.Writer.Flush()
		}
	}
}

// derefString returns *s or "" for nil.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// nativeErrorBody passes a relay error body through as the event's error
// value. Relay error bodies are JSON in the ingress dialect's native
// shape, which nests the error itself one member deep ({"error":{...}});
// that member is unwrapped so a column's payload carries the dialect's
// error object directly. A body with no such member — or one that is not
// JSON at all — is embedded as-is (or wrapped as a string), so the event
// always stays parseable and never loses information.
func nativeErrorBody(body []byte) json.RawMessage {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return json.RawMessage(`{"message":"upstream returned an empty error body"}`)
	}
	if !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{"message":` + jsonEscape(trimmed) + `}`)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &top); err == nil {
		if inner, ok := top["error"]; ok {
			return inner
		}
	}
	return json.RawMessage(trimmed)
}

// jsonEscape renders s as a JSON string literal (quotes included). A Go
// string is always marshalable, so json.Marshal has no failure case here to
// branch on.
func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
