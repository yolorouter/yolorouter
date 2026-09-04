package gateway

// The speech modality: the caller-facing half of the OpenAI speech API
// (POST /v1/audio/speech). One JSON request in, one binary audio response
// out, billed by the character in the settling candidate's own meter.
//
// Three properties define it against the rest of the gateway. Headers go out
// only once there is audio to send them with — a provider can answer 200 with
// a JSON error envelope, and a response already announced as audio/mpeg is
// one the caller has been told about. The bytes are forwarded as they arrive
// — a minute of speech is megabytes over seconds, and buffering it whole
// would spend the caller's first-word latency on nothing. And a failed
// speech request never moves to another provider: the voice is a thing the
// caller named, a different provider would speak it differently, and an
// error the caller can act on beats an audio they did not ask for.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols/audio"
)

// ModalityAudio names the speech modality. The spelling is the model row's
// output_modalities value and nothing else — one vocabulary, two readers.
const ModalityAudio ModalityID = "audio"

// SpeechPath is the caller-facing speech endpoint, the OpenAI shape this
// gateway answers byte for byte.
const SpeechPath = "/v1/audio/speech"

func NewAudioModality() Modality { return audioModality{} }

type audioModality struct{}

// audioResponseCeiling caps one speech response. A long synthesis runs to a
// few megabytes; the ceiling leaves room for the longest legal inputs while
// still bounding what a misbehaving upstream can push through the pump. The
// kernel narrows further whenever its own cap is lower.
const audioResponseCeiling = 32 << 20

func (audioModality) ID() ModalityID { return ModalityAudio }

func (audioModality) Limits() TransferLimits {
	return TransferLimits{MaxResponseBytes: audioResponseCeiling}
}

// speechRequest is the caller's body, in the OpenAI speech shape. The two
// pointer fields exist to judge PRESENCE, not value: a caller who sends
// instructions or stream_format must be told this build does not serve them,
// and silently dropping the field would let them believe it took effect.
type speechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed"`
	Instructions   *string  `json:"instructions"`
	StreamFormat   *string  `json:"stream_format"`
}

// speechFormats and the membership test stay as named seams of the door —
// the vocabulary check is admission policy (a caller typo), not dialect
// knowledge — but derive from the shared table's one canonical order.
var speechFormats = audio.Formats()

var speechFormatVocabulary = audio.FormatVocabulary()

func (audioModality) Admit(ctx context.Context, in Ingress) (Payload, *Rejection) {
	var req speechRequest
	if err := json.Unmarshal(in.Body, &req); err != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: "invalid request body", FailReason: "parse: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	if req.Model == "" {
		return nil, rejectMissingField("model", "empty_model")
	}
	if req.Input == "" {
		return nil, rejectMissingField("input", "empty_input")
	}
	if req.Voice == "" {
		return nil, rejectMissingField("voice", "empty_voice")
	}
	if req.Instructions != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "instructions are not supported by speech models in this build",
			FailReason: "instructions_unsupported", Fault: fact.FaultClient,
		}
	}
	if req.StreamFormat != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "stream_format is not supported by speech models in this build",
			FailReason: "stream_format_unsupported", Fault: fact.FaultClient,
		}
	}
	if req.ResponseFormat != "" && !speechFormatVocabulary[req.ResponseFormat] {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "response_format must be one of " + strings.Join(speechFormats, ", "),
			FailReason: "invalid_response_format", Fault: fact.FaultClient,
		}
	}
	payload := &audioPayload{req: req}
	// The budget pre-gate: the cheapest estimate across this model's enabled
	// audio candidates is held against the key's ceiling before anything is
	// dialled, because a synthesis the caller could never pay for still
	// renders at the operator's cost. Only a certain refusal answers — every
	// other failure of the precheck stays silent, exactly as the video door's
	// does, for the same reason: the authoritative accounting runs at settle.
	if audioBudgetPrecheck != nil {
		if err := audioBudgetPrecheck(ctx, in.APIKeyID, req.Model, req.Input); err != nil {
			var budget *audioBudgetExceededError
			if errors.As(err, &budget) {
				return nil, &Rejection{
					Status: http.StatusTooManyRequests, ErrorType: errTypeInsufficientQuota,
					Message: budget.Error(), FailReason: failAudioBudgetExceeded,
					Fault: fact.FaultClient,
				}
			}
		}
	}
	return payload, nil
}

// failAudioBudgetExceeded is the fail reason the door's budget refusal
// carries, one spelling for analytics to group by.
const failAudioBudgetExceeded = "audio_budget_exceeded"

// speechDialect and the table behind speechDialectFor live in the audio
// protocol package: the provider client's probes route by the same table
// the gateway routes and bills by, and two private copies of it would be a
// drift class, not a saving. The aliases keep this file's spelling.
type speechDialect = audio.Dialect

var (
	speechDialectSiliconFlow = audio.DialectSiliconFlow
	speechDialectZhipu       = audio.DialectZhipu
	speechDialectOpenAI      = audio.DialectOpenAI
	speechDialectMiniMax     = audio.DialectMiniMax
)

// isMiniMaxSpeechBase is the speech side's minimax gate, overridable in
// tests for the same reason every dialect gate is. Deliberately its own
// detector rather than the video dialect's: same vendor and host, different
// dialect family, and the encode/decode branches must agree with the table
// below on every spelling of the base.
var isMiniMaxSpeechBase = audio.MiniMaxSpeechBase

// speechDialectFor picks the dialect a provider's base URL speaks. A
// package var over the shared table's decision: a local test server never
// carries a real hostname, and the branch needs exercising against a live
// stub.
var speechDialectFor = audio.DialectFor

// audioPayload is one speech request: the parsed ask, and — once Supports
// has chosen — the dialect that will encode it and the candidate it was
// chosen for. The caller's original bytes are not carried: the kernel keeps
// what it captured at admission, and nothing here outlives the request.
type audioPayload struct {
	req speechRequest
	// dialect and cand are what Supports approved; prepareErr is what
	// PrepareUpstream would fail with. Set by Supports, consumed by
	// PrepareUpstream and Deliver, exactly the coupling the call-order
	// contract sanctions.
	dialect    *speechDialect
	cand       *Candidate
	prepareErr error
}

// The voice the caller named is only theirs to name per provider: voices do
// not travel between vendors, so a failed attempt never moves to another
// provider — an audio served in a different voice is an answer to a question
// nobody asked. Key rotation inside the one provider keeps working.
func (p *audioPayload) Routing() RoutingIntent {
	return RoutingIntent{Model: p.req.Model, NoCrossProviderFailover: true}
}

// EstimateCost cannot answer in the token-priced view the kernel asks
// about — the speech bill is characters × the candidate's own price, and
// the money gate that matters runs at the door (audioBudgetPrecheck) and
// again at settle. Stating the unit keeps the estimate honest about what
// would be counted even though no number is known here.
func (p *audioPayload) EstimateCost(PricingView) CostEstimate {
	return CostEstimate{Unit: fact.UnitCharacter}
}

func (p *audioPayload) Supports(cand Candidate) CandidateVerdict {
	dialect := speechDialectFor(cand.BaseURL)
	if p.req.ResponseFormat != "" && !dialect.Formats[p.req.ResponseFormat] {
		return CandidateVerdict{OK: false, Reason: fmt.Sprintf(
			"the %s speech dialect serves only %s", dialect.Name, dialectFormatList(dialect))}
	}
	p.dialect = &dialect
	p.cand = &cand
	return CandidateVerdict{OK: true}
}

// dialectFormatList renders a dialect's format set in the vocabulary's
// canonical order, so the refusal names a stable list rather than whatever
// order a map iteration happens to produce.
func dialectFormatList(d speechDialect) string { return audio.FormatList(d) }

// effectiveFormat is the format this request will actually be served in: the
// caller's explicit choice, or the dialect's own default when they sent
// none. It is what the upstream is asked for and what the response is
// announced as, so the two can never disagree.
func (p *audioPayload) effectiveFormat() string {
	if p.req.ResponseFormat != "" {
		return p.req.ResponseFormat
	}
	if p.dialect != nil {
		return p.dialect.DefaultFormat
	}
	return speechDialectOpenAI.DefaultFormat
}

// speechUpstreamRequest is the OpenAI speech body this gateway sends
// upstream. response_format is always stated, never left to the upstream's
// default: the default differs per vendor, and the content type this gateway
// announces must match the bytes that come back.
type speechUpstreamRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	ResponseFormat string   `json:"response_format"`
	Speed          *float64 `json:"speed,omitempty"`
}

func (p *audioPayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	memo := p.cand
	if memo == nil || memo.ProviderModelName != cand.ProviderModelName || memo.BaseURL != cand.BaseURL {
		p.prepareErr = fmt.Errorf("speech payload prepared for a candidate Supports did not approve")
		return nil, p.prepareErr
	}
	// The t2a_v2 dialect has its own body and path; the OpenAI shape rides
	// the provider's base path. Both answers are audio this gateway must
	// forward as it arrives, so both declare progressive.
	call := UpstreamCall{ContentType: "application/json", Progressive: true}
	if isMiniMaxSpeechBase(cand.BaseURL) {
		encoded, err := audio.EncodeMiniMaxSpeech(audio.MiniMaxSpeechRequest{
			Model:        cand.ProviderModelName,
			Text:         p.req.Input,
			VoiceSetting: audio.MiniMaxVoiceSetting{VoiceID: p.req.Voice, Speed: p.req.Speed},
			AudioSetting: audio.MiniMaxAudioSetting{Format: p.effectiveFormat()},
		})
		if err != nil {
			p.prepareErr = err
			return nil, err
		}
		// The path carries its own /v1 segment, so it hangs off the
		// provider's origin rather than a versioned chat base.
		call.Path = audio.MiniMaxSpeechPath
		call.OriginRelative = true
		call.Body = encoded
		return &call, nil
	}
	encoded, err := json.Marshal(speechUpstreamRequest{
		Model:          cand.ProviderModelName,
		Input:          p.req.Input,
		Voice:          p.req.Voice,
		ResponseFormat: p.effectiveFormat(),
		Speed:          p.req.Speed,
	})
	if err != nil {
		p.prepareErr = err
		return nil, err
	}
	call.Path = "/audio/speech"
	call.Body = encoded
	return &call, nil
}

// deliverMiniMax reads one t2a_v2 answer whole (bounded), decodes it, and
// hands the decoded bytes to the caller through the same late-commit shape
// the stream pump uses. A business refusal inside the 200 is the caller's
// own to act on — answered 422 with the vendor's code and message, like the
// video dialects answer theirs.
func (p *audioPayload) deliverMiniMax(tools DeliveryTools, usage *fact.UsageReported, resp *http.Response) fact.Delivery {
	body, err := io.ReadAll(io.LimitReader(resp.Body, tools.Limits.MaxResponseBytes+1))
	if err != nil {
		return deliverNoAudio(tools, "audio_read: "+err.Error())
	}
	if int64(len(body)) > tools.Limits.MaxResponseBytes {
		return deliverNoAudio(tools, "response_too_large")
	}
	obs, refusal, perr := audio.ParseMiniMaxSpeechResponse(body)
	if perr != nil {
		return deliverNoAudio(tools, perr.Error())
	}
	if refusal != nil {
		errBody, _ := json.Marshal(map[string]any{"error": map[string]string{
			"code": strconv.Itoa(refusal.Code), "message": refusal.Message,
		}})
		if failed := commitJSONAnswer(tools, http.StatusUnprocessableEntity, errBody); failed != nil {
			return *failed
		}
		return fact.Rejected(http.StatusUnprocessableEntity, fact.FaultClient,
			"speech_business_error", errors.New(strconv.Itoa(refusal.Code)+": "+refusal.Message))
	}
	// The vendor's own count is the bill when it states one — the request's
	// re-count is the estimate the pre-gate used, and the settlement
	// corrects to the invoice's number (the same estimate-then-actual
	// correction the chat path performs on tokens).
	if obs.UsageStated {
		usage.Count = obs.UsageChars
		usage.Source = fact.UsageFromUpstream
	}
	format := obs.Format
	if format == "" {
		format = p.effectiveFormat()
	}
	return deliverAudioBuffer(tools, usage, speechContentType(format), obs.Audio)
}

// deliverAudioBuffer forwards one whole audio buffer under the late-commit
// policy: headers go out with the first (only) write, so a buffer that turns
// out empty is answered rather than committed as a silent success.
func deliverAudioBuffer(tools DeliveryTools, usage *fact.UsageReported, contentType string, buf []byte) fact.Delivery {
	if len(buf) == 0 {
		return deliverNoAudio(tools, "empty_audio")
	}
	tools.Client.Inject(http.Header{"Content-Type": {contentType}})
	if cerr := tools.Client.Commit(http.StatusOK); cerr != nil {
		return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
			fact.FaultGateway, "commit_failed: "+cerr.Error(), cerr).WithUsage(usage)
	}
	if _, werr := tools.Client.Write(buf); werr != nil {
		return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_write", werr).WithUsage(usage)
	}
	if ferr := tools.Client.Flush(); ferr != nil {
		return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_flush", ferr).WithUsage(usage)
	}
	return fact.Succeeded(http.StatusOK).WithUsage(usage)
}

// speechContentType maps a response format to the content type the response
// is announced as — the fallback for a provider that announced nothing or
// bare octets, since the announcement must match the bytes.
func speechContentType(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "opus":
		return "audio/ogg"
	case "aac":
		return "audio/aac"
	case "flac":
		return "audio/flac"
	case "wav":
		return "audio/wav"
	case "pcm":
		return "audio/pcm"
	default:
		return "application/octet-stream"
	}
}

// deliverNoAudio answers a no-audio failure: an upstream 200 whose body is
// not audio (a JSON error envelope wearing a success status), an empty
// body, or a read that failed before the first byte. Nothing has been
// committed, so the caller receives a proper error rather than a truncated
// nothing — and nothing is billed: no audio ever existed, so no synthesis
// ran for the operator's money.
func deliverNoAudio(tools DeliveryTools, reason string) fact.Delivery {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"message": "upstream produced no audio", "type": errTypeUpstream,
	}})
	if failed := commitJSONAnswer(tools, http.StatusBadGateway, body); failed != nil {
		return *failed
	}
	return fact.Rejected(http.StatusBadGateway, fact.FaultUpstream, "upstream_200_without_audio: "+reason, nil)
}

func (p *audioPayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	if p.prepareErr != nil {
		return p.deliverGatewayFault(tools, "audio_prepare: "+p.prepareErr.Error(), p.prepareErr)
	}
	if p.dialect == nil || p.cand == nil {
		return p.deliverGatewayFault(tools, "audio delivery without an approved candidate", nil)
	}
	defer func() { _ = resp.Body.Close() }()

	// Incurred the moment synthesis starts, carried on every exit below: a
	// caller who hangs up mid-sentence still cost the operator the audio.
	usage := &fact.UsageReported{
		Unit:   fact.UnitCharacter,
		Source: fact.UsageFromRequest,
		Count:  p.dialect.Meter(p.req.Input),
		Meter:  p.dialect.MeterLabel,
	}

	// The t2a_v2 dialect answers with one JSON envelope whose audio is hex
	// encoded — nothing to forward until the whole answer has arrived and
	// been decoded, so the branch is its own rather than the stream pump's.
	if isMiniMaxSpeechBase(p.cand.BaseURL) {
		return p.deliverMiniMax(tools, usage, resp)
	}

	// A 200 that does not announce audio is the provider's error envelope
	// wearing a success status — the one shape status alone cannot see. It
	// is read whole (bounded) and answered, never forwarded: announcing a
	// content type and then sending JSON behind it is exactly what the late
	// commit exists to make impossible. A provider that announces nothing,
	// or announces bare octets, is still serving audio — the format the
	// request effectively asked for is the only announcement left, and it
	// is what the caller is told.
	announce := resp.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(announce, "audio/"):
	case announce == "" || announce == "application/octet-stream":
		announce = speechContentType(p.effectiveFormat())
	default:
		body, err := io.ReadAll(io.LimitReader(resp.Body, tools.Limits.MaxResponseBytes+1))
		if err != nil {
			return deliverNoAudio(tools, "audio_read: "+err.Error())
		}
		if int64(len(body)) > tools.Limits.MaxResponseBytes {
			return deliverNoAudio(tools, "response_too_large")
		}
		return deliverNoAudio(tools, "200 with content type "+announce)
	}

	buf := make([]byte, 32<<10)
	forwarded := 0
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if !tools.Client.Committed() {
				tools.Client.Inject(http.Header{"Content-Type": {announce}})
				if cerr := tools.Client.Commit(http.StatusOK); cerr != nil {
					return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
						fact.FaultGateway, "commit_failed: "+cerr.Error(), cerr).WithUsage(usage)
				}
			}
			if _, werr := tools.Client.Write(chunk); werr != nil {
				return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_write", werr).WithUsage(usage)
			}
			if ferr := tools.Client.Flush(); ferr != nil {
				return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_flush", ferr).WithUsage(usage)
			}
			forwarded += n
			if int64(forwarded) > tools.Limits.MaxResponseBytes {
				return fact.Truncated(http.StatusOK, http.StatusBadGateway, fact.FaultUpstream,
					"response_too_large", nil).WithUsage(usage)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if !tools.Client.Committed() {
				return deliverNoAudio(tools, "audio_read: "+rerr.Error())
			}
			return fact.Truncated(http.StatusOK, http.StatusBadGateway, fact.FaultUpstream,
				"audio_cut_short: "+rerr.Error(), rerr).WithUsage(usage)
		}
	}
	if !tools.Client.Committed() {
		return deliverNoAudio(tools, "empty_audio")
	}
	return fact.Succeeded(http.StatusOK).WithUsage(usage)
}

// deliverGatewayFault answers this gateway's own failures before any audio
// existed — a build that failed, an approval that is missing. Nothing is
// billed: no synthesis ever started.
func (p *audioPayload) deliverGatewayFault(tools DeliveryTools, reason string, err error) fact.Delivery {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"message": "internal error", "type": errTypeServer,
	}})
	if failed := commitJSONAnswer(tools, http.StatusInternalServerError, body); failed != nil {
		return *failed
	}
	return fact.Rejected(http.StatusInternalServerError, fact.FaultGateway, reason, err)
}

// NormalizeUpstreamError is not on the speech path: the kernel's own
// classification shapes non-2xx upstream failures before a delivery runs,
// and the 2xx-without-audio shape is answered by the delivery itself.
func (p *audioPayload) NormalizeUpstreamError(status int, _ []byte, _ string) ErrorEnvelope {
	return ErrorEnvelope{Status: status, ErrorType: errTypeUpstream, Message: "upstream error"}
}

// FinalizeUsage bills only what a delivery carried. A request that never
// reached a synthesis — refused at the door, every candidate skipped — has
// no usage to state, and inventing one would bill audio nobody rendered.
func (p *audioPayload) FinalizeUsage(d fact.Delivery) *fact.UsageReported {
	return d.Usage
}

// LogPolicy keeps the two request bodies (both JSON text) and renders the
// upstream response: the kernel stores a failed upstream's error envelope
// directly (the non-2xx wording lands in the audit body row), while a
// refusal inside a 200 is answered to the caller and named in the fail
// reason instead — its body never reaches the column a progressive
// delivery keeps empty. The caller-facing half stays dropped — a
// progressive delivery's bytes would otherwise land in the stream capture
// file, and served audio is stored nowhere by design.
func (p *audioPayload) LogPolicy() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    BodyStoredRaw,
		BodyUpstreamRequest:  BodyStoredRaw,
		BodyUpstreamResponse: BodyStoredRendered,
	}}
}

func (p *audioPayload) SanitizeForLog(k BodyKind, contentType string, body []byte) string {
	if k == BodyClientRequest || k == BodyUpstreamRequest {
		return string(body)
	}
	// An error envelope is text an operator reads; anything else is audio
	// bytes, which become a length and a hash — same rule the content type
	// would pick, stated on the bytes themselves because the renderer is
	// invoked without one on the kernel's error path.
	if json.Valid(body) {
		return string(body)
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("<audio %d bytes %s sha256:%s>", len(body), contentType, hex.EncodeToString(sum[:])[:16])
}
