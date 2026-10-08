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
	"slices"
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

// playgroundRequestBody is the modality-shaped request half of a run: the
// chat turns a text run sends, or the prompt an image run renders. Each
// modality validates the member it owns before the stream starts (below),
// so the fields stay optional at the binding layer — the shapes of two
// modalities share one envelope, and each refuses its own absences.
type playgroundRequestBody struct {
	Messages []playgroundMessage `json:"messages" binding:"omitempty,min=1,dive"`
	Prompt   string              `json:"prompt" binding:"omitempty,max=32000"`
}

// playgroundRunParams are the parameters the modalities expose, one
// family per modality. They are applied uniformly to every column of the
// run (the comparison varies the model, not the sampling), and only the
// active modality's family reaches its upstream request body — the body
// builders are whitelists, so the other family's set values have nowhere
// to go. The audio family's one axis is the response format; the VOICE is
// deliberately not here — it is the run's one per-column parameter, a
// vendor-private id whose shared value would 400 every foreign column of
// a mixed-vendor comparison (see playgroundRunRequest.Voices).
type playgroundRunParams struct {
	System      *string  `json:"system" binding:"omitempty,max=32000"`
	Temperature *float64 `json:"temperature" binding:"omitempty,gte=0,lte=2"`
	MaxTokens   *int     `json:"max_tokens" binding:"omitempty,gte=1,lte=100000"`
	// Size and N are the image modality's axes — the two request fields
	// the images dialect itself declares. Size is a short axis string
	// ("1024x1024"); N is the image count.
	Size *string `json:"size" binding:"omitempty,max=32"`
	N    *int    `json:"n" binding:"omitempty,gte=1,lte=10"`
	// Format is the audio modality's shared axis — the speech dialect's
	// own response_format vocabulary, applied to every speech column
	// alike (a column whose dialect does not serve the chosen format
	// fails in its own native error shape, isolation intact). Empty
	// leaves each column's dialect default in force.
	Format *string `json:"format" binding:"omitempty,oneof=mp3 opus aac flac wav pcm"`
}

// playgroundRunRequest is the run contract: whose key to spend, which
// modality's shape the request body has, which models to compare (1–4
// columns), and the parameter set applied to all of them. Voices is the
// audio modality's per-column exception to that set: model name → the
// voice that column speaks in. Voices do not travel between vendors (a
// foreign voice is a terminal 400 at the upstream), so one shared value
// would break every mixed-vendor comparison — the dropdown lists each
// column's own vendor's voices, and the run carries one voice per column.
// Entries for models not in the run are ignored; a selected model without
// a non-empty voice is refused before the stream starts.
type playgroundRunRequest struct {
	KeyID    uint                  `json:"key_id" binding:"required"`
	Modality string                `json:"modality" binding:"omitempty,oneof=text image audio"`
	Models   []string              `json:"models" binding:"required,min=1,max=4,dive,required"`
	Request  playgroundRequestBody `json:"request"`
	Params   playgroundRunParams   `json:"params"`
	Voices   map[string]string     `json:"voices" binding:"omitempty"`
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
// and the usage/cost/latency figures the audit row settled on. Media
// modalities — whose answers do not stream chunk by chunk — additionally
// carry the delivered response under Response: for a body that fit the
// inline cap it is the response's own JSON embedded raw (a consumer
// receives the parsed object); for one that did not, it is the capped
// byte prefix as a plain string with Truncated set, so an over-cap answer
// is marked as cut rather than silently passed off as the whole body. A
// body with no honest form under that two-shape contract — not JSON at
// all (an empty body, an HTML proxy page), or JSON that is not
// object-shaped (a bare string would be indistinguishable from a cut
// prefix) — carries no Response at all. Text columns never carry
// Response either — their text already arrived as chunk events, and
// re-sending it whole would double the channel's payload for nothing.
// Speech columns carry Audio instead: the delivered bytes are binary
// audio, which the two-shape Response contract cannot represent, so the
// service retains the whole clip and the event references it by its
// console playback URL with the content type to announce it with.
type sseDoneEvent struct {
	Slot       int                    `json:"slot"`
	Model      string                 `json:"model"`
	Status     int                    `json:"status"`
	RequestID  string                 `json:"request_id"`
	Usage      playground.ColumnUsage `json:"usage"`
	CostMicros int64                  `json:"cost_micros"`
	CostKnown  bool                   `json:"cost_known"`
	DurationMs int64                  `json:"duration_ms"`
	Response   json.RawMessage        `json:"response,omitempty"`
	Truncated  bool                   `json:"response_truncated,omitempty"`
	Audio      *sseAudioRef           `json:"audio,omitempty"`
}

// sseAudioRef is a speech column's playback reference: the console URL
// that serves the retained clip (same session, same origin), the content
// type the exchange announced, and the clip's size. Present only when the
// whole clip is retained — a cut clip gets no reference at all.
type sseAudioRef struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Bytes       int    `json:"bytes"`
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
// run addresses the model by, the declared output modalities the picker
// filters on, and — for a model that speaks the speech modality — the
// preset voice vocabulary its vendors serve (the union across the model's
// routable candidates' dialects, first-seen order). Voices ride the row
// because the voice control is PER COLUMN: a voice is a vendor-private
// id, and each column's dropdown must offer only the voices its own
// model's vendors know. Carrying only these fields keeps the
// member-readable list to the vocabulary a run actually needs.
type playgroundModelOption struct {
	Name             string   `json:"name"`
	OutputModalities []string `json:"output_modalities"`
	Voices           []string `json:"voices,omitempty"`
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
// payload to names, modality declarations, and the speech models' preset
// voice vocabularies — nothing about providers, pricing, or topology.
// Voices come from the speech dialect table keyed by the routable
// candidates' provider base URLs (pgSvc), so the dropdown and the routing
// bill read the same table.
func GetPlaygroundModels(svc *modeladmin.ModelService, pgSvc *playground.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		views, err := svc.ListModels()
		if err != nil {
			writeServiceError(c, err)
			return
		}
		// One base-URL read for every provider an audio model routes to;
		// non-audio models never join this query.
		providerIDs := map[uint]struct{}{}
		for _, v := range views {
			if v.ManagementStatus != model.ModelStatusEnabled {
				continue
			}
			if !slices.Contains(v.OutputModalities, model.OutputModalityAudio) {
				continue
			}
			for _, cand := range v.Candidates {
				if cand.Routable {
					providerIDs[cand.ProviderID] = struct{}{}
				}
			}
		}
		ids := make([]uint, 0, len(providerIDs))
		for id := range providerIDs {
			ids = append(ids, id)
		}
		voicesByProvider, err := pgSvc.ProviderSpeechVoices(ids)
		if err != nil {
			response.InternalError(c, err.Error())
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
			option := playgroundModelOption{Name: v.Name, OutputModalities: v.OutputModalities}
			// A speech model's dropdown vocabulary: the union of its
			// routable candidates' dialect voices, deduped in first-seen
			// order. For the overwhelmingly common single-vendor model
			// that IS its vendor's list; a multi-vendor model honestly
			// offers what any of its candidates could serve.
			if slices.Contains(v.OutputModalities, model.OutputModalityAudio) {
				seen := map[string]struct{}{}
				for _, cand := range v.Candidates {
					if !cand.Routable {
						continue
					}
					for _, voice := range voicesByProvider[cand.ProviderID] {
						if _, dup := seen[voice]; dup {
							continue
						}
						seen[voice] = struct{}{}
						option.Voices = append(option.Voices, voice)
					}
				}
			}
			out = append(out, option)
		}
		response.Success(c, gin.H{"list": out})
	}
}

// GetPlaygroundAudio handles GET /api/admin/playground/audio/:request_id —
// a finished speech column's retained clip, served for the page session
// that ran it. The reference arrives on the done event; this endpoint is
// what it points at. Ownership is pinned to the account that ran the
// column, and an id that is absent, expired, or another account's answers
// the same nothing — the endpoint is not an enumeration oracle.
func GetPlaygroundAudio(svc *playground.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		viewerID := c.MustGet(middleware.UserIDKey).(uint)
		body, contentType, ok := svc.PlaybackAudio(c.Param("request_id"), viewerID)
		if !ok {
			response.NotFound(c, errcode.RequestLogNotFound, errcode.GetMessage(errcode.RequestLogNotFound))
			return
		}
		c.Header("Content-Type", contentType)
		c.Header("Cache-Control", "private, no-store")
		c.Status(http.StatusOK)
		_, _ = c.Writer.Write(body)
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
// while the others are still generating. It carries the service so the
// shared per-column settle choreography can read the audit row without
// the modality drivers each threading the handle themselves.
type aggregateStream struct {
	c   *gin.Context
	svc *playground.Service
	mu  sync.Mutex
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

		// The modality's own body is decoded and validated before the
		// stream starts so a malformed turn (a missing prompt, an empty
		// content, a smuggled content shape) answers with the console
		// envelope and a real 400 — no column ever ran.
		var runColumn func(ctx context.Context, stream *aggregateStream, slot int, model string)
		switch req.Modality {
		case "image":
			if strings.TrimSpace(req.Request.Prompt) == "" {
				response.ParamError(c, "an image run requires a prompt")
				return
			}
			imageRun := playground.ImageRun{
				Prompt: req.Request.Prompt,
				Size:   derefString(req.Params.Size),
				N:      derefInt(req.Params.N),
			}
			runColumn = func(ctx context.Context, stream *aggregateStream, slot int, model string) {
				columnRun := imageRun
				columnRun.Model = model
				// No payload sink: the images dialect answers one JSON
				// body, so an image column has start and terminal events
				// only — the response arrives with the terminal read.
				result, err := svc.RunImage(ctx, key, columnRun)
				settlePlaygroundColumn(stream, slot, model, result, err, attachImageResponse(result))
			}
		case "audio":
			if strings.TrimSpace(req.Request.Prompt) == "" {
				response.ParamError(c, "an audio run requires a prompt")
				return
			}
			// Every selected model must carry its own voice — the run's
			// one per-column parameter, refused up front rather than as
			// per-column upstream noise.
			voices := make(map[string]string, len(req.Models))
			for _, m := range req.Models {
				voice := strings.TrimSpace(req.Voices[m])
				if voice == "" {
					response.ParamError(c, "an audio run requires a voice for every selected model")
					return
				}
				voices[m] = voice
			}
			audioRun := playground.AudioRun{
				Input:          req.Request.Prompt,
				Voice:          "", // per column, below
				ResponseFormat: derefString(req.Params.Format),
			}
			runColumn = func(ctx context.Context, stream *aggregateStream, slot int, model string) {
				columnRun := audioRun
				columnRun.Model = model
				columnRun.Voice = voices[model]
				// No payload sink and no inline response: the speech
				// dialect answers binary audio, which the channel's
				// two-shape response contract cannot carry — the service
				// retains the clip and the done event references it.
				result, err := svc.RunAudio(ctx, key, columnRun)
				settlePlaygroundColumn(stream, slot, model, result, err, attachAudioRef(result))
			}
		default: // text
			if len(req.Request.Messages) == 0 {
				response.ParamError(c, "a text run requires at least one message")
				return
			}
			textRun := playground.TextRun{
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
				textRun.Messages[i] = playground.Message{Role: m.Role, Content: content}
			}
			runColumn = func(ctx context.Context, stream *aggregateStream, slot int, model string) {
				columnRun := textRun
				columnRun.Model = model
				// The sink runs inside the relay's delivery, on this
				// goroutine: each complete SSE payload the relay writes to
				// the column's caller side becomes a chunk event the
				// moment it lands.
				result, err := svc.RunText(ctx, key, columnRun, func(payload string) {
					if delta := streamChunkDelta(payload); delta != "" {
						stream.write("chunk", sseChunkEvent{Slot: slot, Model: model, Delta: delta})
					}
				})
				settlePlaygroundColumn(stream, slot, model, result, err, nil)
			}
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

		stream := &aggregateStream{c: c, svc: svc}
		// The console caller's context is every column's relay context's
		// parent: a caller disconnect cancels the in-flight columns the
		// same way a gateway caller's disconnect cancels its attempt —
		// whichever modality those columns speak.
		ctx := c.Request.Context()
		var wg sync.WaitGroup
		for slot, model := range req.Models {
			wg.Add(1)
			go func(slot int, model string) {
				defer wg.Done()
				runColumn(ctx, stream, slot, model)
			}(slot, model)
		}
		wg.Wait()
	}
}

// settlePlaygroundColumn drives one comparison column from its exchange's
// outcome to its terminal event: the audit row supplies the figures, the
// row's fail_reason decides the aborted-by-disconnect case, and the relay
// status decides done versus the native error shape. decorate amends a
// done event with the modality's delivered-answer member — the images
// family embeds the response body (see sseDoneEvent for the two shapes),
// the audio family attaches the playback reference, and text columns pass
// nil (their text already streamed).
func settlePlaygroundColumn(stream *aggregateStream, slot int, model string, result *playground.ColumnResult, err error, decorate func(*sseDoneEvent)) {
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
	audit, auditErr := stream.svc.Audit(result.RequestID)
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
		done := sseDoneEvent{
			Slot: slot, Model: model, Status: result.Status, RequestID: result.RequestID,
			Usage: audit.Usage, CostMicros: audit.CostMicros, CostKnown: audit.CostKnown,
			DurationMs: audit.DurationMs,
		}
		if decorate != nil {
			decorate(&done)
		}
		stream.write("done", done)
		return
	}
	stream.write("error", sseErrorEvent{
		Slot: slot, Model: model, Status: result.Status, RequestID: result.RequestID,
		Error: nativeErrorBody(result.Body),
	})
}

// playgroundAudioRoute is the console's playback route for a retained
// speech column, prefixed the way the done event's reference spells it.
const playgroundAudioRoute = "/api/admin/playground/audio/"

// attachImageResponse decorates a done event with the images family's
// delivered body under the channel's two-shape response contract.
func attachImageResponse(result *playground.ColumnResult) func(*sseDoneEvent) {
	return func(e *sseDoneEvent) {
		e.Response, e.Truncated = inlineColumnResponse(result)
	}
}

// attachAudioRef decorates a done event with a speech column's playback
// reference — present only when the service retained the whole clip
// (RunAudio's own verdict; a cut or failed clip gets no reference).
func attachAudioRef(result *playground.ColumnResult) func(*sseDoneEvent) {
	return func(e *sseDoneEvent) {
		if result == nil || !result.AudioPlayable {
			return
		}
		e.Audio = &sseAudioRef{
			URL:         playgroundAudioRoute + result.RequestID,
			ContentType: result.ContentType,
			Bytes:       len(result.Body),
		}
	}
}

// inlineColumnResponse shapes a terminal response body for a done event.
// The channel's contract is two-shaped and the demux folds by shape alone
// — a plain string always means a cut body — so only a body that keeps the
// two shapes tellable apart rides the event: the body's own JSON embedded
// raw when it is whole and object-shaped (a consumer receives the parsed
// object), and the capped byte prefix as a plain string with the Truncated
// flag set when the exchange overran the inline cap. A body with no honest
// form under that contract — one that is not JSON at all (an empty body,
// an HTML proxy page) or JSON that is not object-shaped (a bare string
// would arrive indistinguishable from a cut prefix) — is not inlined: the
// column still settles with the audit row's figures, and nothing on the
// channel claims to be the delivered answer.
func inlineColumnResponse(result *playground.ColumnResult) (json.RawMessage, bool) {
	if result.Truncated {
		return json.RawMessage(jsonEscape(string(result.Body))), true
	}
	trimmed := strings.TrimSpace(string(result.Body))
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') && json.Valid([]byte(trimmed)) {
		return json.RawMessage(result.Body), false
	}
	return nil, false
}

// derefInt returns *i or 0 for nil.
func derefInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
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
