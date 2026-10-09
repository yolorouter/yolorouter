// Package playground owns the console Playground run: resolving the
// operator's chosen API key under the console's ownership rules, and
// executing the run's relay calls IN PROCESS against the real gateway
// service. Every run goes through the exact relay pipeline an outside
// caller's request goes through — key state checks, admission
// (concurrency/RPM/TPM/budget), candidate routing, settlement, and the
// request_logs audit row — so the numbers a Playground run reports are the
// numbers production traffic pays. The only differences from an outside
// call are the transport (a direct service call, no socket) and the
// request_logs attribution (the playground source marker).
package playground

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/gateway/osswire"
	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/audio"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// Console-side key resolution failures. They are deliberately distinct
// errors rather than errcode values: the handler maps each to the console
// envelope the contract assigns it (ownership → 403, key state → 401), and
// the service stays transport-free.
var (
	// ErrKeyNotOwned covers both "no such key" and "key belongs to another
	// account" — one answer for both so the endpoint is not an enumeration
	// oracle for other accounts' key ids.
	ErrKeyNotOwned = errors.New("api key not found")
	ErrKeyRevoked  = errors.New("api key revoked")
	ErrKeyExpired  = errors.New("api key expired")
	// ErrOwnerDisabled mirrors the credential auth path's owner gate: a
	// disabled account's keys stop working everywhere at once.
	ErrOwnerDisabled = errors.New("account disabled")
)

// Service executes Playground runs against the real relay service.
type Service struct {
	db        *gorm.DB
	secrets   crypto.SecretBox
	relay     *gateway.Service
	bodiesDir string
	// videoTasks is the video job domain the run endpoint bridges a video
	// column's task polling through (the same port the caller-facing job
	// resource routes read), and videoFetch is the upstream client the
	// finished clip's console playback proxy reads the result URL through.
	// Either may be nil in a bare assembly: a video run then answers with
	// the task domain's own refusal rather than pretending to bridge.
	videoTasks gateway.VideoTasks
	videoFetch *gateway.UpstreamClient
	// videoWaits is the in-flight video bridge registry, keyed by the
	// column's exchange request id — the stop-waiting endpoint's target.
	// See BridgeVideoTask for the lifecycle.
	videoWaitMu sync.Mutex
	videoWaits  map[string]*videoWait
	// retained audio answers, keyed by the exchange's request id — see
	// RetainAudio for what the store is and is not.
	audioMu    sync.Mutex
	audio      map[string]retainedAudio
	audioOrder []string
}

// New builds a Playground service over the same relay instance the gateway
// routes use — passing a second relay instance here would give Playground
// runs a different admission state (rate windows, circuit, key pool) from
// real traffic, which is exactly what the real-chain contract forbids.
// videoTasks/videoFetch pass the SAME video job domain and upstream client
// the router assembled for the relay, so a Playground video column's bridge
// polls through the state machine a real caller's GET polls through, and
// its playback proxy fetches through the same transport rules.
func New(db *gorm.DB, secrets crypto.SecretBox, relay *gateway.Service, videoTasks gateway.VideoTasks, videoFetch *gateway.UpstreamClient, bodiesDir string) *Service {
	return &Service{
		db: db, secrets: secrets, relay: relay, bodiesDir: bodiesDir,
		videoTasks: videoTasks, videoFetch: videoFetch,
		videoWaits: map[string]*videoWait{},
		audio:      map[string]retainedAudio{},
	}
}

// ResolveKey loads the caller's chosen key and applies the console-side
// gates: the key must belong to the viewing account, its owner must be
// enabled, and it must be neither revoked nor expired. Budget and rate
// limits are NOT checked here — those belong to the relay's admission
// chain, where their rejections land in the audit trail exactly like an
// outside caller's.
func (s *Service) ResolveKey(viewerID, keyID uint) (*model.APIKey, error) {
	key, err := repository.FindAPIKeyByID(s.db, keyID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyNotOwned
		}
		return nil, err
	}
	if key.UserID != viewerID {
		return nil, ErrKeyNotOwned
	}
	ownerStatus, err := repository.FindAPIKeyOwnerStatus(s.db, key.UserID)
	if err != nil {
		return nil, err
	}
	if ownerStatus != model.UserStatusEnabled {
		return nil, ErrOwnerDisabled
	}
	if key.Status == model.APIKeyStatusRevoked {
		return nil, ErrKeyRevoked
	}
	if key.ExpiresAt != nil && key.ExpiresAt.Before(time.Now().UTC()) {
		return nil, ErrKeyExpired
	}
	return key, nil
}

// ownKeysListCap bounds the key picker's list. An account with more usable
// keys than this can still run under any of them by id; the picker simply
// stops growing, keeping the endpoint a single bounded query.
const ownKeysListCap = 100

// OwnKey is one row of the key picker: the identifying bits of an API key
// the operator may run comparisons under. Nothing here can spend anything
// on its own — every run re-resolves the key by id through ResolveKey, so
// a stale picker row (revoked a minute ago) is refused at run time.
type OwnKey struct {
	ID        uint   `json:"id"`
	KeyPrefix string `json:"key_prefix"`
	Remark    string `json:"remark"`
}

// ListOwnKeys returns the keys the viewer may still run under: their own,
// not revoked, not expired, and not already over the budget cap (the
// admission chain refuses those on the next call anyway, so offering them
// would only present columns that cannot start). Ownership is pinned by
// the caller's own id — both roles — because the run endpoint accepts a
// key only from its owner, so the picker must never offer another row.
func (s *Service) ListOwnKeys(viewerID uint) ([]OwnKey, error) {
	userID := viewerID
	keys, err := repository.SearchAPIKeys(s.db, repository.APIKeyFilter{
		Status: "active", UserID: &userID, Now: time.Now().UTC(),
	}, 0, ownKeysListCap)
	if err != nil {
		return nil, err
	}
	out := make([]OwnKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, OwnKey{ID: k.ID, KeyPrefix: k.KeyPrefix, Remark: k.Remark})
	}
	return out, nil
}

// Message is one chat turn of the Playground request body. Content is
// either plain text or OpenAI vision content parts — the exact wire shapes
// the chat ingress decodes, so an attached image travels in its real
// encoding (a base64 data URL inside an image_url part) rather than a
// console-invented form.
type Message struct {
	Role    string
	Content any // string | []ContentPart
}

// ContentPart is one member of a vision content array. Type selects the
// shape; each shape carries its own field and nothing else, which is what
// keeps the console's whitelist airtight even for image turns — an unknown
// member of a caller's part cannot survive into the upstream body because
// there is nowhere in this struct to put it.
type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL carries a vision attachment in the dialect's own encoding: a
// data URL whose bytes the provider decodes itself.
type ImageURL struct {
	URL string `json:"url"`
}

// TextRun is one text-modality column: the model to call, the conversation
// to send, and the exposed sampling parameters. The body builder
// serializes EXACTLY these fields — anything the Playground does not
// expose must not reach the upstream request body.
type TextRun struct {
	Model       string
	Messages    []Message
	System      string
	Temperature *float64
	MaxTokens   *int
}

// ColumnResult is the outcome of one column's relay call: the relay's own
// request id (the audit row's key), the HTTP status it produced, and the
// response bytes in the ingress dialect's native shape — errors included,
// verbatim, so the column can render the same error a real caller would
// see. For a streaming exchange the bytes are the SSE frames the relay
// wrote to the column's caller side (bounded by columnBodyCap); the text
// itself already reached the tap's sink as it was written. Truncated is
// set when the exchange wrote past that cap — the retained Body is then a
// prefix, and the channel says so rather than passing it off as whole.
// ContentType is what the exchange announced (the staged header the relay
// committed); a speech answer's audio/mpeg is what a playback reference
// must re-announce. AudioPlayable says the delivered audio was retained
// whole and a playback reference would be honest (see RunAudio).
type ColumnResult struct {
	RequestID     string
	Status        int
	Body          []byte
	Truncated     bool
	ContentType   string
	AudioPlayable bool
}

// FailReasonClientDisconnected is the audit row's fail_reason for an
// exchange the caller aborted by leaving. The console channel maps it to
// its cancel acknowledgement rather than a done or error event: the row,
// not the unwritten response, is the truth about an aborted column.
const FailReasonClientDisconnected = "client_disconnected"

// columnBodyCap bounds the response bytes a tap retains for the terminal
// read. Error bodies are small, and a stream's text has already flowed to
// the sink live, so this is a guard against an exchange whose bytes grow
// without bound, not a limit any honest answer hits.
const columnBodyCap = 1 << 20

// audioRetentionCap is what an AUDIO column's tap retains: a whole clip a
// page can play back, not the 1 MiB reading copy the text/image taps keep.
// A synthesis runs to a few megabytes ("a long synthesis" in the speech
// modality's own words), so the cap sits above honest answers while still
// bounding what a misbehaving upstream can park in the console process; a
// clip that overruns it is marked truncated and gets NO playback
// reference (a cut audio clip is not playable, and the channel never
// claims completeness it does not have).
const audioRetentionCap = 8 << 20

// columnTap is the caller side of one Playground column's relay exchange.
// The relay writes the exact bytes a real caller of the same ingress would
// receive — SSE frames for a streaming exchange, a JSON error body for a
// refused one — and the tap does two things with them: keeps a bounded
// copy for the terminal read, and hands each complete SSE data payload to
// the column's sink as it arrives. The sink firing per frame is the whole
// point of the aggregate channel: a column's text reaches the console
// while the exchange is still running, not after it retires.
//
// The tap is deliberately NOT an httptest.ResponseRecorder: a recorder
// only reveals its bytes once the handler returns, which would serialize
// every column's display to the end of its own exchange. It implements
// just enough of http.ResponseWriter (plus the flush and write-deadline
// hooks net/http's ResponseController asks for) for the relay's writes to
// land on it synchronously.
type columnTap struct {
	sink func(payload string)
	// cap is the body retention bound for this exchange — columnBodyCap
	// for a reading copy, audioRetentionCap for a playable clip.
	cap int
	// frame holds the bytes of the SSE frame currently being written; a
	// frame is only complete once its blank-line terminator arrives, so a
	// write that ends mid-frame waits here for the rest.
	frame []byte
	body  []byte
	// truncated records that the exchange wrote MORE bytes than the body
	// cap retains. The done event reports it, so an over-cap response is
	// marked as cut on the channel rather than passed off as the whole
	// body.
	truncated bool
	// status is what the exchange committed; 0 until something did.
	status int
	header http.Header
}

func newColumnTap(sink func(payload string), cap int) *columnTap {
	return &columnTap{sink: sink, cap: cap, header: http.Header{}}
}

func (t *columnTap) Header() http.Header { return t.header }

func (t *columnTap) WriteHeader(status int) { t.status = status }

// Write records the bytes and drains whatever complete SSE frames they
// completed. It always reports full success: there is no socket whose
// backpressure to honor, and a refusal here would be read by the relay as
// a caller who left.
func (t *columnTap) Write(p []byte) (int, error) {
	if t.status == 0 {
		// net/http semantics: a body write commits 200 when no explicit
		// status was written first.
		t.status = http.StatusOK
	}
	if room := t.cap - len(t.body); room > 0 {
		if len(p) <= room {
			t.body = append(t.body, p...)
		} else {
			t.body = append(t.body, p[:room]...)
			t.truncated = true
		}
	} else if len(p) > 0 {
		t.truncated = true
	}
	// The frame buffer exists only to feed the sink; a column without one
	// (a JSON or audio answer — nothing streams) would otherwise park the
	// whole response a second time for no reader.
	if t.sink != nil {
		t.frame = append(t.frame, p...)
		t.drainFrames(false)
	}
	return len(p), nil
}

// Flush is a no-op with the same always-succeed contract as Write; the
// frames were handed to the sink at write time.
func (t *columnTap) Flush() {}

// SetWriteDeadline satisfies http.NewResponseController's optional hook so
// the relay's per-write deadline slides succeed silently instead of
// warning once per request that this writer cannot take a deadline.
func (t *columnTap) SetWriteDeadline(time.Time) error { return nil }

// drainFrames hands complete SSE frames to the sink. trailing=false stops
// at the first incomplete frame (the mid-stream mode); trailing=true also
// emits a final unterminated frame, for the moment an exchange ends —
// some encoders close the body right after the last data line without a
// blank line, and the last payload must not be lost to that.
func (t *columnTap) drainFrames(trailing bool) {
	// Frame boundaries come from protocols.SSEFrameEnd — the codebase's
	// one answer (sse.go: a framer scanning for "\n\n" alone never finds
	// a frame in a CRLF-throughout stream and drops it whole). The tap
	// already shares SSEDataPayloadStart for the data-line rule; sharing
	// the frame rule too is what keeps this reader from drifting into a
	// second, private definition of a frame.
	buf := string(t.frame)
	consumed := 0
	for {
		idx, sepLen := protocols.SSEFrameEnd(buf[consumed:])
		if idx < 0 {
			break
		}
		t.emitFrame(t.frame[consumed : consumed+idx])
		consumed += idx + sepLen
	}
	if consumed > 0 {
		t.frame = append(t.frame[:0], t.frame[consumed:]...)
	}
	if trailing && len(bytes.TrimSpace(t.frame)) > 0 {
		t.emitFrame(t.frame)
		t.frame = nil
	}
}

// emitFrame hands one SSE frame's data payloads to the sink, using the
// protocols package's own data-line prefix rule so what counts as a data
// line cannot drift between the relay and this tap. Non-data lines
// (comments, event:/id:/retry: directives, blank separators) carry no
// text; the terminal figures come from the audit row, not the stream.
func (t *columnTap) emitFrame(frame []byte) {
	if t.sink == nil {
		return
	}
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		start, ok := protocols.SSEDataPayloadStart(line)
		if !ok {
			continue
		}
		if payload := string(line[start:]); payload != "" {
			t.sink(payload)
		}
	}
}

// chatCompletionsPath is the ingress the synthetic relay request presents.
// It selects the OpenAI dialect end to end — the wire protocol the text
// modality speaks.
const chatCompletionsPath = "/v1/chat/completions"

// buildChatBody serializes a text run into an OpenAI chat completion
// request body. Field whitelist, not blacklist: only model, messages, and
// the exposed parameters are ever emitted, so an unexposed parameter
// cannot leak into the upstream request by omission of cleanup.
func buildChatBody(run TextRun) ([]byte, error) {
	// chatMessage mirrors the wire shape field for field; typed structs
	// (rather than passing raw JSON through) are what makes the whitelist
	// airtight — a caller cannot smuggle an extra member inside a message.
	// Content is the Message's own string-or-parts value, re-marshaled by
	// the same typed shapes above.
	type chatMessage struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	}
	msgs := make([]chatMessage, 0, len(run.Messages)+1)
	if run.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: run.System})
	}
	for _, m := range run.Messages {
		msgs = append(msgs, chatMessage(m))
	}
	// body is built by adding keys one by one so each optional parameter
	// is present only when the operator set it. stream is the channel's
	// own property rather than an operator parameter: the aggregate
	// console channel streams text columns, so every exchange asks its
	// upstream for a stream. It is deliberately not a TextRun field —
	// there is no buffered mode for the channel to choose between.
	body := map[string]any{"model": run.Model, "messages": msgs, "stream": true}
	if run.Temperature != nil {
		body["temperature"] = *run.Temperature
	}
	if run.MaxTokens != nil {
		body["max_tokens"] = *run.MaxTokens
	}
	return json.Marshal(body)
}

// RunText executes one text column end to end through the real relay
// pipeline. The call is in process: a synthetic request is handed straight
// to the relay service's entry point, so the caller's API key plaintext
// never leaves this process (no socket, no loopback HTTP hop) while the
// exchange still runs the full admission chain — the playground marker it
// sets grants no admission privilege. ctx should be the console caller's
// request context, so a caller disconnect cancels the upstream attempt
// the same way a gateway caller's disconnect does.
//
// onPayload receives each complete SSE data payload the relay writes to
// the column's caller side, as it is written — the bytes a real streaming
// caller of the same ingress would receive, in arrival order. It may be
// nil. The callback runs on this call's goroutine, inside the relay's
// delivery; whatever it does must not block on the exchange finishing.
//
// The key's plaintext is decrypted here solely to seed the relay's
// credential slot — the vision-fallback capability re-presents it when the
// gateway must call itself on this exchange's behalf. A key that predates
// encrypted storage has no recoverable plaintext; the run proceeds with an
// empty credential and such a sub-call would fail its auth, which is the
// honest outcome for a key whose plaintext no longer exists anywhere.
func (s *Service) RunText(ctx context.Context, key *model.APIKey, run TextRun, onPayload func(payload string)) (*ColumnResult, error) {
	body, err := buildChatBody(run)
	if err != nil {
		return nil, err
	}
	return s.runExchange(ctx, key, chatCompletionsPath, body, onPayload, columnBodyCap)
}

// imagesGenerationPath is the ingress the synthetic image relay request
// presents: the OpenAI Images API's generation route, which resolves to the
// images modality end to end — the wire protocol the image modality speaks.
const imagesGenerationPath = "/v1/images/generations"

// ImageRun is one image-modality column: the model to call, the prompt to
// render, and the two request axes the images dialect itself declares —
// size and count. Those are the only parameters the console exposes for
// the modality, and the only ones the body builder can emit.
type ImageRun struct {
	Model  string
	Prompt string
	// Size is the request's size axis (the per-image tier table's key
	// alongside quality); empty leaves it to the upstream's own default.
	Size string
	// N is how many images to ask for; 0 leaves it unset (the API's own
	// default of one applies upstream, not here — see ImageRun callers).
	N int
}

// buildImageBody serializes an image run into an OpenAI images/generations
// request body. Same whitelist discipline as buildChatBody: only model,
// prompt, and the exposed axes are ever emitted, so a parameter the page
// does not expose cannot reach the upstream body by omission of cleanup.
func buildImageBody(run ImageRun) ([]byte, error) {
	body := map[string]any{"model": run.Model, "prompt": run.Prompt}
	if run.Size != "" {
		body["size"] = run.Size
	}
	if run.N > 0 {
		body["n"] = run.N
	}
	return json.Marshal(body)
}

// RunImage executes one image column end to end through the real relay
// pipeline — the same in-process exchange, admission chain, and audit row
// RunText drives, on the images ingress instead of the chat one. There is
// no payload sink to hand over: the images dialect this build serves is
// JSON in, JSON out (no progressive half), so an image column has start
// and terminal events only — the response body arrives with the terminal
// read, bounded by the tap's cap with the truncation flag doing the
// honest marking.
func (s *Service) RunImage(ctx context.Context, key *model.APIKey, run ImageRun) (*ColumnResult, error) {
	body, err := buildImageBody(run)
	if err != nil {
		return nil, err
	}
	return s.runExchange(ctx, key, imagesGenerationPath, body, nil, columnBodyCap)
}

// AudioRun is one speech column: the model to speak through, the input
// text, the VOICE the operator picked for this column (the run's one
// per-column parameter — a voice is a vendor-private id, and one shared
// value across mixed vendors would 400 every foreign column), and the
// response format the shared parameter row selected (empty leaves the
// dialect's own default in force). Speed and every other axis the speech
// dialect declares stay unexposed: the page's audio family is format and
// voice, nothing else.
type AudioRun struct {
	Model          string
	Input          string
	Voice          string
	ResponseFormat string
}

// buildAudioBody serializes a speech run into an OpenAI audio/speech
// request body. Same whitelist discipline as the other builders: only
// model, input, voice, and — when the operator chose one — the response
// format are ever emitted.
func buildAudioBody(run AudioRun) ([]byte, error) {
	body := map[string]any{"model": run.Model, "input": run.Input, "voice": run.Voice}
	if run.ResponseFormat != "" {
		body["response_format"] = run.ResponseFormat
	}
	return json.Marshal(body)
}

// RunAudio executes one speech column end to end through the real relay
// pipeline — the same in-process exchange, admission chain, per-character
// settlement, and audit row the other modalities drive, on the speech
// ingress. The delivered answer is binary audio, which the SSE event
// family cannot inline honestly (its two-shape response contract is
// object-JSON-or-cut-prefix, and audio bytes are neither); instead the
// whole clip — up to audioRetentionCap — is retained in the service's
// page-session store and the done event carries a playback reference to
// it. A clip that overran the retention cap is NOT retained and NOT
// referenced: the column still settles with the audit row's figures, and
// nothing on the channel claims an audio it cannot serve. The speech
// modality's no-cross-vendor failover keeps the named voice's affinity,
// exactly as for an outside caller.
func (s *Service) RunAudio(ctx context.Context, key *model.APIKey, run AudioRun) (*ColumnResult, error) {
	body, err := buildAudioBody(run)
	if err != nil {
		return nil, err
	}
	result, err := s.runExchange(ctx, key, gateway.SpeechPath, body, nil, audioRetentionCap)
	if err != nil {
		return nil, err
	}
	if result.Status >= 200 && result.Status < 300 && !result.Truncated && len(result.Body) > 0 && result.ContentType != "" {
		s.RetainAudio(result.RequestID, result.Body, result.ContentType, key.UserID)
		result.AudioPlayable = true
	}
	return result, nil
}

// videosCreatePath is the ingress the synthetic video relay request
// presents: the OpenAI Videos API's create route (the videos package's
// own CreatePath), which resolves to the video modality end to end — a
// submit that answers the queued job resource the bridge then polls.
const videosCreatePath = "/v1/videos"

// VideoRun is one video-modality column: the model to generate through,
// the prompt to render, and the two request axes the video dialect itself
// declares — clip seconds and size. Those are the only parameters the
// console exposes for the modality, and the only ones the body builder
// can emit; the vocabulary (seconds 4/8/12, the four sizes) is the
// dialect's own, enforced before the stream starts.
type VideoRun struct {
	Model  string
	Prompt string
	// Seconds is the requested clip duration; 0 leaves it unset and the
	// dialect's own default applies. Size is the requested resolution; ""
	// leaves it unset the same way.
	Seconds int
	Size    string
}

// buildVideoBody serializes a video run into an OpenAI videos create
// request body. Same whitelist discipline as the other builders: only
// model, prompt, and the two exposed axes are ever emitted.
func buildVideoBody(run VideoRun) ([]byte, error) {
	body := map[string]any{"model": run.Model, "prompt": run.Prompt}
	if run.Seconds > 0 {
		body["seconds"] = run.Seconds
	}
	if run.Size != "" {
		body["size"] = run.Size
	}
	return json.Marshal(body)
}

// RunVideo executes one video column's SUBMIT through the real relay
// pipeline — the same in-process exchange, admission chain, and audit row
// the other modalities drive, on the videos create ingress. The answer is
// the queued job resource (the task id the bridge polls); nothing about
// the clip is billable yet, and the exchange's own duration is the submit
// round-trip, not the render. The caller's context is the exchange's
// context as for every other modality.
func (s *Service) RunVideo(ctx context.Context, key *model.APIKey, run VideoRun) (*ColumnResult, error) {
	body, err := buildVideoBody(run)
	if err != nil {
		return nil, err
	}
	return s.runExchange(ctx, key, videosCreatePath, body, nil, columnBodyCap)
}

// VideoTaskID reads the caller-facing job id out of a successful submit's
// delivered body — the relay-rendered job resource whose id member the
// bridge polls the task domain with. An answer without one (an empty or
// malformed body on a 2xx) answers "", which the caller reports as the
// column's failure rather than bridging a task it cannot name.
func VideoTaskID(submitBody []byte) string {
	var resource struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(submitBody, &resource); err != nil {
		return ""
	}
	return resource.ID
}

// videoBridgeTick is how often the bridge asks the task domain about one
// task. The domain itself paces upstream polls (one query per task per
// DefaultPollInterval, younger answers served from the row), so this is
// the latency of NOTICING a terminal state, not the upstream load: a tick
// is a row read except once per domain interval.
const videoBridgeTick = 500 * time.Millisecond

// VideoBridgeSink receives the bridge's channel-worthy outcomes. The
// service stays transport-free: the handler maps these onto the
// aggregate channel's event family.
type VideoBridgeSink interface {
	// VideoDone is the terminal success: the audit row's settled figures
	// (the late settlement the completed observation just projected onto
	// the submit's row) and the delivered seconds the bill counted.
	VideoDone(audit *ColumnAudit, seconds int)
	// VideoTaskFailed is a terminal task state that is not success
	// (failed/cancelled/expired): the task's own error vocabulary.
	VideoTaskFailed(code, message string)
	// VideoWaitStopped is the operator stopping the wait from the page:
	// the bridge stops polling, the task itself keeps running, and its
	// cost settles into the request log when completion is next observed.
	VideoWaitStopped()
}

// videoWait is one in-flight video bridge's registry entry. stopped is set
// by StopVideoWait BEFORE the cancel fires, so the bridge can tell "the
// page stopped waiting" (an event is still deliverable — the channel is
// open) from "the page left" (nothing to deliver; the audit row is the
// durable record either way). stopped is only ever touched under
// videoWaitMu: the registry's mutex is the one synchronization edge shared
// by StopVideoWait's write and the bridge's exit read — the context's own
// cancel path orders only a stop that fired its own cancel, never a
// concurrent caller-left read.
type videoWait struct {
	cancel  context.CancelFunc
	owner   uint
	stopped bool
}

// BridgeVideoTask polls one submitted video task to terminal inside the
// page session, on the operator's behalf: each round goes through the task
// domain's own Get — the same lazy poll, single-flight and settlement
// backstop a real caller's GET gets — so the terminal reading the column
// reports is the state machine's, not a second one invented here.
//
// The wait's context is the run channel's context: a console caller who
// leaves cancels it, the bridge stops polling, and the task is NOT
// touched — the video domain has no cancel, the bill is decided when
// completion is observed, and the reaper (or any later poll) settles a
// task nobody watched. StopVideoWait cancels the same context through the
// registry with stopped set, which is the difference between an honest
// wait_stopped event and silence.
func (s *Service) BridgeVideoTask(runCtx context.Context, key *model.APIKey, requestID, taskID string, sink VideoBridgeSink) {
	if s.videoTasks == nil {
		sink.VideoTaskFailed("video_tasks_unwired", "the video job domain is not wired in this deployment")
		return
	}
	waitCtx, cancel := context.WithCancel(runCtx)
	s.videoWaitMu.Lock()
	wait := &videoWait{cancel: cancel, owner: key.UserID}
	s.videoWaits[requestID] = wait
	s.videoWaitMu.Unlock()
	defer func() {
		s.videoWaitMu.Lock()
		if s.videoWaits[requestID] == wait {
			delete(s.videoWaits, requestID)
		}
		s.videoWaitMu.Unlock()
		cancel()
	}()

	ticker := time.NewTicker(videoBridgeTick)
	defer ticker.Stop()
	for {
		task, err := s.videoTasks.Get(waitCtx, key.ID, taskID, time.Now())
		switch {
		case err == nil && task != nil:
			switch task.Status {
			case gateway.VideoTaskCompleted:
				// The completing observation has already settled the task
				// and projected the digest onto the submit's audit row; a
				// fresh read of that row is the column's terminal figures.
				audit, auditErr := s.Audit(requestID)
				if auditErr != nil {
					sink.VideoTaskFailed("video_settlement_unreadable", auditErr.Error())
					return
				}
				sink.VideoDone(audit, task.UsageSeconds)
				return
			case gateway.VideoTaskFailed, gateway.VideoTaskCancelled, gateway.VideoTaskExpired:
				code := task.ErrorCode
				if code == "" {
					code = "video_task_" + task.Status
				}
				sink.VideoTaskFailed(code, task.ErrorMessage)
				return
			}
			// pending/processing: the wait continues.
		case err != nil && waitCtx.Err() == nil:
			// A poll failure is not a task failure — the domain's own
			// stance. The next tick retries; the channel's lifetime (the
			// console caller's own timeout or disconnect) bounds the wait.
		}
		select {
		case <-waitCtx.Done():
			// stopped is read under the registry mutex — the same lock
			// StopVideoWait writes it under. The stop's own cancel closing
			// this channel would order its write anyway, but the
			// caller-left path here (runCtx cancelled by the console
			// caller's disconnect) races an in-flight stop-wait POST with
			// no such edge; the mutex is the one edge both sides share.
			s.videoWaitMu.Lock()
			stopped := wait.stopped
			s.videoWaitMu.Unlock()
			if stopped {
				sink.VideoWaitStopped()
			}
			// Not stopped: the console caller left. Nothing is
			// deliverable on a channel that is gone; the task keeps
			// running and settles when completion is next observed.
			return
		case <-ticker.C:
		}
	}
}

// StopVideoWait stops the page's bridged polling of one video column:
// the task is NOT terminated (the video domain has no cancel and the bill
// is already committed), only the waiting stops. Owner-pinned: another
// account's id answers the same false as an absent one.
func (s *Service) StopVideoWait(requestID string, viewerID uint) bool {
	s.videoWaitMu.Lock()
	defer s.videoWaitMu.Unlock()
	wait, ok := s.videoWaits[requestID]
	if !ok || wait.owner != viewerID {
		return false
	}
	wait.stopped = true
	wait.cancel()
	return true
}

// VideoContent opens the finished clip's byte stream for the page that
// ran the column: the task is resolved durably by the column's audit id
// (the registry only covers in-flight waits), pinned to the account that
// ran it, and the bytes come from the upstream's own result URL through
// the same transport the relay uses — this service stores no media, the
// same rehosting-ruled-out stance the gateway's own content route takes.
// The caller must close the returned body. A miss (unknown or foreign id,
// unfinished task, dead result URL, unwired client) answers ok=false.
func (s *Service) VideoContent(ctx context.Context, requestID string, viewerID uint) (io.ReadCloser, string, bool) {
	if s.videoFetch == nil {
		return nil, "", false
	}
	task, err := repository.FindVideoTaskByRequestID(s.db, requestID)
	if err != nil {
		return nil, "", false
	}
	key, err := repository.FindAPIKeyByID(s.db, task.APIKeyID)
	if err != nil || key.UserID != viewerID {
		return nil, "", false
	}
	if task.Status != model.VideoTaskCompleted || task.ResultURL == "" {
		return nil, "", false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, task.ResultURL, nil)
	if err != nil {
		return nil, "", false
	}
	resp, err := s.videoFetch.SendUpstreamRequest(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, "", false
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "video/mp4"
	}
	return resp.Body, contentType, true
}

// runExchange drives one column's synthetic relay exchange: the request
// carries no Authorization header at all (the relay receives the resolved
// key row and the decrypted credential through gin context slots — the
// same slots the credential middleware fills for a real request — so the
// plaintext cannot appear even in the exchange's masked header capture),
// and the tap stands in for the caller's connection, writes landing on it
// synchronously as the relay delivers. onPayload may be nil; bodyCap is
// the tap's retention bound for this exchange's response bytes.
func (s *Service) runExchange(ctx context.Context, key *model.APIKey, path string, body []byte, onPayload func(payload string), bodyCap int) (*ColumnResult, error) {
	credential := ""
	if key.EncryptedKey != "" {
		var err error
		credential, err = s.secrets.Decrypt(key.EncryptedKey)
		if err != nil {
			return nil, err
		}
	}
	// A constant method and a constant path cannot fail the constructor,
	// so there is no error case here to branch on.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	tap := newColumnTap(onPayload, bodyCap)
	gc, _ := gin.CreateTestContext(tap)
	gc.Request = req
	requestID := uuid.NewString()
	// The relay reads the id from the context slot the RequestID
	// middleware normally fills; supplying a fresh one gives every column
	// its own audit identity (multiple columns of one console request are
	// separate exchanges and must never share a request_logs row id).
	gc.Set(middleware.RequestIDKey, requestID)
	gc.Set(gateway.BodiesDirContextKey, s.bodiesDir)
	gateway.SetGatewayAuth(gc, osswire.APIKey(key))
	gateway.SetGatewayCredential(gc, credential)
	gateway.SetPlaygroundCall(gc)

	s.relay.Handle(gc, osswire.APIKey(key))
	tap.drainFrames(true)
	return &ColumnResult{
		RequestID:   requestID,
		Status:      tap.status,
		Body:        tap.body,
		Truncated:   tap.truncated,
		ContentType: tap.header.Get("Content-Type"),
	}, nil
}

// retainedAudio is one playable speech answer held for the page session
// that asked for it. This is deliberately NOT run history: the page keeps
// no history, the request log keeps no audio bytes (the speech modality's
// own LogPolicy drops the caller-facing half), and a reference has to
// point somewhere — so the answer itself is held in process, bounded and
// ephemeral, exactly long enough for the page that rendered the column to
// play it back.
type retainedAudio struct {
	body        []byte
	contentType string
	owner       uint
	expiresAt   time.Time
}

const (
	// audioRetentionTTL is how long a retained clip stays playable — a
	// page session's worth of slack, pruned lazily (no background
	// goroutine to own or stop).
	audioRetentionTTL = 10 * time.Minute
	// audioRetentionMax is how many clips the store holds before the
	// oldest are evicted — several full four-column runs, so a re-run
	// never evicts the very columns it just replaced on screen.
	audioRetentionMax = 16
)

// RetainAudio stores one delivered clip for playback. Bounded on both
// axes (age and count), pruned lazily on every write and read.
func (s *Service) RetainAudio(requestID string, body []byte, contentType string, owner uint) {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	if s.audio == nil {
		s.audio = map[string]retainedAudio{}
	}
	now := time.Now()
	// Prune expired entries, then evict oldest until the new clip fits.
	kept := s.audioOrder[:0]
	for _, id := range s.audioOrder {
		if entry, ok := s.audio[id]; ok && entry.expiresAt.After(now) {
			kept = append(kept, id)
		} else {
			delete(s.audio, id)
		}
	}
	s.audioOrder = kept
	for _, id := range s.audioOrder {
		if len(s.audioOrder) < audioRetentionMax {
			break
		}
		delete(s.audio, id)
		s.audioOrder = s.audioOrder[1:]
	}
	s.audio[requestID] = retainedAudio{body: body, contentType: contentType, owner: owner, expiresAt: now.Add(audioRetentionTTL)}
	s.audioOrder = append(s.audioOrder, requestID)
}

// PlaybackAudio answers the playable clip for a finished column, pinned
// to the account that ran it: another account's id — like an absent or
// expired one — answers nothing, so the endpoint is not an enumeration
// oracle for other operators' runs.
func (s *Service) PlaybackAudio(requestID string, viewerID uint) ([]byte, string, bool) {
	s.audioMu.Lock()
	defer s.audioMu.Unlock()
	entry, ok := s.audio[requestID]
	if !ok || entry.owner != viewerID || !entry.expiresAt.After(time.Now()) {
		return nil, "", false
	}
	return entry.body, entry.contentType, true
}

// ProviderSpeechVoices maps each provider id to the preset voice
// vocabulary of the speech dialect its base URL speaks — the model
// picker's per-column voice lists, one dialect-table read per provider.
// Unknown bases resolve to the OpenAI dialect like every other consumer
// of the table.
func (s *Service) ProviderSpeechVoices(providerIDs []uint) (map[uint][]string, error) {
	urls, err := repository.FindProviderBaseURLs(s.db, providerIDs)
	if err != nil {
		return nil, err
	}
	out := make(map[uint][]string, len(urls))
	for id, baseURL := range urls {
		out[id] = audio.DialectFor(baseURL).Voices
	}
	return out, nil
}

// ColumnAudit is the settled audit reading for one column: the numbers the
// request_logs row recorded for the exchange. These are the same figures
// the request-log page shows for the run, read back from the same row — a
// Playground report is never computed a second time from the response.
type ColumnAudit struct {
	Usage      ColumnUsage
	CostMicros int64
	CostKnown  bool
	DurationMs int64
	FailReason string
	Source     string
}

// ColumnUsage is the volume half of the audit reading. Images carries the
// per-image count an image exchange settled by (the row's image_count),
// Characters the counted characters a speech exchange settled by (the
// row's usage_characters, in the settling candidate's own billing meter),
// and Seconds the delivered seconds a video task's late settlement
// projected onto its submit's row (usage_seconds); each is absent on rows
// that did not bill by it, so one reading reads in a single billing unit.
type ColumnUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Images       int `json:"images,omitempty"`
	Characters   int `json:"characters,omitempty"`
	Seconds      int `json:"seconds,omitempty"`
}

// Audit reads the request_logs row one column's exchange wrote. The row is
// written on every exit path of the relay pipeline, so a row is expected
// even for rejected runs; its absence is reported as an error rather than
// silently reported as zero cost.
func (s *Service) Audit(requestID string) (*ColumnAudit, error) {
	row, err := repository.GetRequestLogByRequestID(s.db, requestID)
	if err != nil {
		return nil, err
	}
	audit := &ColumnAudit{
		Usage: ColumnUsage{
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			Images:       row.ImageCount,
			Characters:   row.UsageCharacters,
			Seconds:      row.UsageSeconds,
		},
		CostMicros: row.CostMicros,
		CostKnown:  row.CostKnown,
		DurationMs: row.DurationMs,
	}
	if row.FailReason != nil {
		audit.FailReason = *row.FailReason
	}
	audit.Source = row.Source
	return audit, nil
}
