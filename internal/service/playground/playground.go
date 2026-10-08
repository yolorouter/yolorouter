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
	"net/http/httptest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/gateway/osswire"
	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
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

// Message is one chat turn of the Playground request body.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
// see.
type ColumnResult struct {
	RequestID string
	Status    int
	Body      []byte
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
	type chatMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	msgs := make([]chatMessage, 0, len(run.Messages)+1)
	if run.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: run.System})
	}
	for _, m := range run.Messages {
		msgs = append(msgs, chatMessage(m))
	}
	// body is built by adding keys one by one so each optional parameter
	// is present only when the operator set it.
	body := map[string]any{"model": run.Model, "messages": msgs}
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
// The key's plaintext is decrypted here solely to seed the relay's
// credential slot — the vision-fallback capability re-presents it when the
// gateway must call itself on this exchange's behalf. A key that predates
// encrypted storage has no recoverable plaintext; the run proceeds with an
// empty credential and such a sub-call would fail its auth, which is the
// honest outcome for a key whose plaintext no longer exists anywhere.
func (s *Service) RunText(ctx context.Context, key *model.APIKey, run TextRun) (*ColumnResult, error) {
	body, err := buildChatBody(run)
	if err != nil {
		return nil, err
	}
	credential := ""
	if key.EncryptedKey != "" {
		credential, err = s.secrets.Decrypt(key.EncryptedKey)
		if err != nil {
			return nil, err
		}
	}

	// The synthetic request carries no Authorization header at all: the
	// relay receives the resolved key row and the decrypted credential
	// through gin context slots (the same slots the credential middleware
	// fills for a real request), so the plaintext cannot appear even in
	// the exchange's masked header capture. A constant method and a
	// constant path cannot fail the constructor, so there is no error
	// case here to branch on.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, chatCompletionsPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(w)
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
	return &ColumnResult{RequestID: requestID, Status: w.Code, Body: w.Body.Bytes()}, nil
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

// ColumnUsage is the volume half of the audit reading.
type ColumnUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
