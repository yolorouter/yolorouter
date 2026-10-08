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
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/gateway/osswire"
	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
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
}

// New builds a Playground service over the same relay instance the gateway
// routes use — passing a second relay instance here would give Playground
// runs a different admission state (rate windows, circuit, key pool) from
// real traffic, which is exactly what the real-chain contract forbids.
func New(db *gorm.DB, secrets crypto.SecretBox, relay *gateway.Service, bodiesDir string) *Service {
	return &Service{db: db, secrets: secrets, relay: relay, bodiesDir: bodiesDir}
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
type ColumnResult struct {
	RequestID string
	Status    int
	Body      []byte
	Truncated bool
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

func newColumnTap(sink func(payload string)) *columnTap {
	return &columnTap{sink: sink, header: http.Header{}}
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
	if room := columnBodyCap - len(t.body); room > 0 {
		if len(p) <= room {
			t.body = append(t.body, p...)
		} else {
			t.body = append(t.body, p[:room]...)
			t.truncated = true
		}
	} else if len(p) > 0 {
		t.truncated = true
	}
	t.frame = append(t.frame, p...)
	t.drainFrames(false)
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
	return s.runExchange(ctx, key, chatCompletionsPath, body, onPayload)
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
	return s.runExchange(ctx, key, imagesGenerationPath, body, nil)
}

// runExchange drives one column's synthetic relay exchange: the request
// carries no Authorization header at all (the relay receives the resolved
// key row and the decrypted credential through gin context slots — the
// same slots the credential middleware fills for a real request — so the
// plaintext cannot appear even in the exchange's masked header capture),
// and the tap stands in for the caller's connection, writes landing on it
// synchronously as the relay delivers. onPayload may be nil.
func (s *Service) runExchange(ctx context.Context, key *model.APIKey, path string, body []byte, onPayload func(payload string)) (*ColumnResult, error) {
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

	tap := newColumnTap(onPayload)
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
	return &ColumnResult{RequestID: requestID, Status: tap.status, Body: tap.body, Truncated: tap.truncated}, nil
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
// per-image count an image exchange settled by (the row's image_count) and
// is absent on token rows, so one reading reads in a single billing unit.
type ColumnUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	Images       int `json:"images,omitempty"`
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
