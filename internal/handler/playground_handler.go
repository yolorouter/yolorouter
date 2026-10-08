// Package handler addition for the console Playground run endpoint: the
// SSE aggregate channel that turns console sessions into real relay
// exchanges. Pre-relay failures (bind errors, key ownership/state) answer
// with the console JSON envelope and a real HTTP status; everything after
// the first event is on the stream, where each column speaks for itself —
// a relay failure is that column's native error shape, not a console
// envelope.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/playground"
	"github.com/yolorouter/yolorouter/pkg/errcode"
	"github.com/yolorouter/yolorouter/pkg/response"
)

// playgroundMessage is one chat turn of the text-modality request body.
// Typed structs end to end (here, the service, the body builder) are what
// makes the upstream parameter whitelist airtight: a message cannot carry
// an extra JSON member because none of these shapes has anywhere to put
// one. Content stays raw until decodePlaygroundContent turns it into
// either a plain string or typed vision parts — the two wire forms the
// page emits and the chat ingress decodes.
type playgroundMessage struct {
	Role    string          `json:"role" binding:"required,oneof=user assistant system"`
	Content json.RawMessage `json:"content" binding:"required"`
}

// playgroundImageURL is the image_url member of a vision content part.
type playgroundImageURL struct {
	URL string `json:"url" binding:"required"`
}

// playgroundContentPart is one member of a vision content array. Each type
// carries its own field and the decode refuses a part that mixes them, so
// a smuggled member has nowhere to survive into the upstream body. These
// parts are unmarshaled by hand out of a RawMessage, so gin's binding tree
// never sees them — the type vocabulary is enforced by the decode's switch,
// not by a tag (a binding tag here would be validation theatre).
type playgroundContentPart struct {
	Type     string              `json:"type"`
	Text     string              `json:"text"`
	ImageURL *playgroundImageURL `json:"image_url"`
}

// Bounds on what an attachment may cost the channel. The per-image cap
// keeps one base64 data URL from filling the request body alone; the count
// cap matches the dialect's own notion of a reasonable vision turn. Both
// refuse before the stream starts, as console envelope 400s.
const (
	maxAttachedImages    = 4
	maxImageDataURLBytes = 8 << 20 // 8 MiB of data-URL characters (~6 MiB of image)
)

// decodePlaygroundContent validates and re-types one message's content:
// either a non-empty string, or an array of text/image_url parts where
// every image arrives as a base64 data URL — the real encoding the page
// produces and the chat ingress decodes, so an attachment reaches the
// upstream in its true form rather than a console-invented shape. Anything
// else is a 400: the whitelist is why an unexposed field cannot reach an
// upstream body.
func decodePlaygroundContent(raw json.RawMessage) (any, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil, errors.New("message content must not be empty")
		}
		return asString, nil
	}
	var parts []playgroundContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("message content must be a string or an array of text/image_url parts")
	}
	if len(parts) == 0 {
		return nil, errors.New("message content parts must not be empty")
	}
	images := 0
	out := make([]playground.ContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if strings.TrimSpace(p.Text) == "" {
				return nil, errors.New("a text part must carry text")
			}
			if p.ImageURL != nil {
				return nil, errors.New("a text part cannot carry an image_url")
			}
			out = append(out, playground.ContentPart{Type: "text", Text: p.Text})
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				return nil, errors.New("an image_url part must carry image_url.url")
			}
			if p.Text != "" {
				return nil, errors.New("an image_url part cannot carry text")
			}
			url := p.ImageURL.URL
			// data: URLs only: the page attaches files it was given, and a
			// remote URL would have the provider fetch an address the
			// console never vouched for.
			media := strings.TrimPrefix(url, "data:image/")
			if media == url || !strings.Contains(url, ";base64,") {
				return nil, errors.New("an attachment must be a base64 data:image/ URL")
			}
			images++
			if images > maxAttachedImages {
				return nil, fmt.Errorf("at most %d attachments per message", maxAttachedImages)
			}
			if len(url) > maxImageDataURLBytes {
				return nil, fmt.Errorf("an attachment exceeds the %d-byte data URL cap", maxImageDataURLBytes)
			}
			out = append(out, playground.ContentPart{Type: "image_url", ImageURL: &playground.ImageURL{URL: url}})
		default:
			// An unknown or absent type is refused, never silently dropped:
			// a dropped part would hollow an all-unknown message into
			// content:[] and send that upstream — exactly the shape the
			// pre-stream whitelist exists to stop. This branch is also what
			// makes the "Anything else is a 400" promise above true for part
			// types (the oneof binding tag cannot do it; see the struct).
			return nil, fmt.Errorf("a content part must be of type text or image_url, got %q", p.Type)
		}
	}
	return out, nil
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

// sseChunkEvent carries one text delta for a column, forwarded the moment
// the relay wrote it to that column's caller side — the streaming half of
// the channel: a fast column's text reaches the console while a slow
// neighbour is still generating.
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

// sseCancelAckEvent acknowledges a column the console caller aborted by
// leaving: the disconnect propagated to the exchange, the audit row
// recorded it (499, client_disconnected), and whatever the run had already
// cost stands. It is written best-effort onto a channel that is usually
// already gone — the durable record of the abort is the audit row, and no
// amount of writing after a disconnect reaches the caller that left.
type sseCancelAckEvent struct {
	Slot      int    `json:"slot"`
	Model     string `json:"model"`
	RequestID string `json:"request_id"`
}

// playgroundModelOption is one row of the model picker: the routing name a
// run addresses the model by, plus the declared output modalities the
// picker filters on. Carrying only these two fields keeps the
// member-readable list to the vocabulary a run actually needs.
type playgroundModelOption struct {
	Name             string   `json:"name"`
	OutputModalities []string `json:"output_modalities"`
}

// GetPlaygroundKeys handles GET /api/admin/playground/keys — the key
// picker's list. The rows are pinned to the session's own account
// server-side for both roles: the run endpoint accepts a key only from its
// owner, so the picker never offers another account's row to begin with.
func GetPlaygroundKeys(svc *playground.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		viewerID := c.MustGet(middleware.UserIDKey).(uint)
		keys, err := svc.ListOwnKeys(viewerID)
		if err != nil {
			response.InternalError(c, err.Error())
			return
		}
		response.Success(c, gin.H{"list": keys})
	}
}

// GetPlaygroundModels handles GET /api/admin/playground/models — the model
// picker's list: every routing-enabled model with at least one routable
// candidate. Any signed-in account may read it because members run
// comparisons under their own keys too; the projection above keeps the
// payload to names and modality declarations, nothing about providers,
// pricing, or topology.
func GetPlaygroundModels(svc *modeladmin.ModelService) gin.HandlerFunc {
	return func(c *gin.Context) {
		views, err := svc.ListModels()
		if err != nil {
			writeServiceError(c, err)
			return
		}
		out := make([]playgroundModelOption, 0, len(views))
		for _, v := range views {
			if v.ManagementStatus != model.ModelStatusEnabled {
				continue
			}
			routable := false
			for _, cand := range v.Candidates {
				if cand.Routable {
					routable = true
					break
				}
			}
			if !routable {
				continue
			}
			out = append(out, playgroundModelOption{Name: v.Name, OutputModalities: v.OutputModalities})
		}
		response.Success(c, gin.H{"list": out})
	}
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

// streamChunkDelta extracts the assistant text delta out of one SSE data
// payload of an OpenAI-shaped chat stream — the ingress dialect every run
// speaks, so the same parse covers pass-through chunks (model rewritten by
// the relay) and translated ones alike. A frame without a delta (usage
// frames, [DONE], pings) answers "".
func streamChunkDelta(payload string) string {
	var parsed struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return ""
	}
	if len(parsed.Choices) == 0 {
		return ""
	}
	return parsed.Choices[0].Delta.Content
}

// aggregateStream serializes event writes from the concurrently running
// columns onto the one console response. Every event is flushed as it is
// written: the channel's whole value is delivering each column's progress
// while the others are still generating.
type aggregateStream struct {
	c  *gin.Context
	mu sync.Mutex
}

func (s *aggregateStream) write(event string, payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writePlaygroundEvent(s.c, event, payload)
	s.c.Writer.Flush()
}

// PostPlaygroundRun handles POST /api/admin/playground/run — the console
// Playground's aggregate channel. The run executes each column through the
// real relay pipeline in process (the service's own comments carry the
// chain rationale); this handler owns the console contract: session
// identity, key ownership, the stream, and mapping each column's relay
// outcome onto the event family.
//
// Every column runs concurrently and streams as it goes: text deltas are
// forwarded as chunk events the moment the relay writes them to a column's
// caller side, so a fast model's answer reaches the console while a slow
// one is still generating. Each event names its slot (the column's stable
// position in the models list), which is all a consumer needs to route an
// arbitrarily interleaved stream. A column aborted by the console caller's
// own disconnect answers with cancel_ack (best-effort — the audit row is
// the durable record); every other column keeps its own terminal event.
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

		// The text body is decoded before the stream starts so a malformed
		// turn (empty content, a smuggled content shape) answers with the
		// console envelope and a real 400 — no column ever ran.
		run := playground.TextRun{
			Messages:    make([]playground.Message, len(req.Request.Messages)),
			System:      derefString(req.Params.System),
			Temperature: req.Params.Temperature,
			MaxTokens:   req.Params.MaxTokens,
		}
		for i, m := range req.Request.Messages {
			content, err := decodePlaygroundContent(m.Content)
			if err != nil {
				response.ParamError(c, err.Error())
				return
			}
			run.Messages[i] = playground.Message{Role: m.Role, Content: content}
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

		stream := &aggregateStream{c: c}
		// The console caller's context is every column's relay context's
		// parent: a caller disconnect cancels the in-flight columns the
		// same way a gateway caller's disconnect cancels its attempt.
		ctx := c.Request.Context()
		var wg sync.WaitGroup
		for slot, model := range req.Models {
			wg.Add(1)
			go func(slot int, model string) {
				defer wg.Done()
				runPlaygroundColumn(ctx, svc, stream, key, run, slot, model)
			}(slot, model)
		}
		wg.Wait()
	}
}

// runPlaygroundColumn drives one comparison column to its terminal event:
// text deltas stream out as chunk events while the exchange runs, and the
// terminal event is decided by what actually happened — the audit row for
// the figures, the row's fail_reason for the aborted-by-disconnect case.
func runPlaygroundColumn(ctx context.Context, svc *playground.Service, stream *aggregateStream, key *model.APIKey, run playground.TextRun, slot int, model string) {
	columnRun := run
	columnRun.Model = model
	// The sink runs inside the relay's delivery, on this goroutine: each
	// complete SSE payload the relay writes to the column's caller side
	// becomes a chunk event the moment it lands.
	result, err := svc.RunText(ctx, key, columnRun, func(payload string) {
		if delta := streamChunkDelta(payload); delta != "" {
			stream.write("chunk", sseChunkEvent{Slot: slot, Model: model, Delta: delta})
		}
	})
	if err != nil {
		// Pre-relay failure (body build, key decrypt): nothing ran, so
		// there is no native shape to embed. The column gets a terminal
		// error naming the failure; the rest of the run proceeds.
		stream.write("error", sseErrorEvent{
			Slot: slot, Model: model, Status: http.StatusInternalServerError,
			Error: json.RawMessage(`{"message":"playground run failed: ` + jsonEscape(err.Error()) + `"}`),
		})
		return
	}
	audit, auditErr := svc.Audit(result.RequestID)
	if auditErr == nil && audit.FailReason == playground.FailReasonClientDisconnected {
		// The console caller went away and the disconnect propagated
		// through the exchange; the row has already recorded what it cost.
		stream.write("cancel_ack", sseCancelAckEvent{Slot: slot, Model: model, RequestID: result.RequestID})
		return
	}
	if auditErr != nil {
		stream.write("error", sseErrorEvent{
			Slot: slot, Model: model, Status: http.StatusInternalServerError,
			RequestID: result.RequestID,
			Error:     json.RawMessage(`{"message":"playground audit row unavailable"}`),
		})
		return
	}
	if result.Status >= 200 && result.Status < 300 {
		stream.write("done", sseDoneEvent{
			Slot: slot, Model: model, Status: result.Status, RequestID: result.RequestID,
			Usage: audit.Usage, CostMicros: audit.CostMicros, CostKnown: audit.CostKnown,
			DurationMs: audit.DurationMs,
		})
		return
	}
	stream.write("error", sseErrorEvent{
		Slot: slot, Model: model, Status: result.Status, RequestID: result.RequestID,
		Error: nativeErrorBody(result.Body),
	})
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
