package gateway

// The video modality: the caller-facing half of the OpenAI Videos job
// dialect over the wan task dialect. The door parses and judges the
// caller's ask (ticket 01); here the payload picks candidates that can
// speak a video task upstream, submits on the kernel's wire, and turns an
// accepted task into a durable row plus the queued job resource the
// caller polls. Settlement is permanently absent from the submit request:
// a video bill is decided when a completed job is observed, not when the
// job is accepted, so FinalizeUsage is nil on this path by design rather
// than by omission.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols/videos"
	"github.com/yolorouter/yolorouter/internal/service/videotask"
)

// ModalityVideo names the video modality. The spelling is the model row's
// output_modalities value, the audit trail's modality column, and nothing
// else — one vocabulary, three readers.
const ModalityVideo ModalityID = "video"

type videoModality struct{}

// NewVideoModality returns the stateless half of the video modality.
func NewVideoModality() Modality { return videoModality{} }

func (videoModality) ID() ModalityID { return ModalityVideo }

// Limits declares nothing beyond the shared budgets: a create call's
// answer is one small JSON job resource, bounded by the kernel's own
// caps, and a modality that repeated them here would be one more place
// for them to drift.
func (videoModality) Limits() TransferLimits { return TransferLimits{} }

func (videoModality) Admit(ctx context.Context, in Ingress) (Payload, *Rejection) {
	req, err := videos.ParseCreateRequest(in.ContentType, in.Body)
	if err != nil {
		message := "invalid request body"
		if strings.HasPrefix(in.ContentType, "multipart/") {
			message = "invalid multipart body"
		}
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: message, FailReason: "parse: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	if req.Model == "" {
		return nil, rejectMissingField("model", "empty_model")
	}
	if req.Prompt == "" {
		return nil, rejectMissingField("prompt", "empty_prompt")
	}
	// The dialect's vocabulary is judged here rather than parsed away: a
	// caller who asks for a 5-second clip must learn the dialect has no
	// such knob, not receive a silently different clip.
	if req.Seconds != 0 && !videos.ValidSeconds(req.Seconds) {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "seconds must be one of 4, 8, or 12",
			FailReason: "invalid_seconds",
			Fault:      fact.FaultClient,
		}
	}
	if req.Size != "" && !videos.ValidSize(req.Size) {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "size must be one of 720x1280, 1280x720, 1024x1792, 1792x1024",
			FailReason: "invalid_size",
			Fault:      fact.FaultClient,
		}
	}
	// A Files-API id names an upload this gateway has no Files API to
	// hold; pretending to serve it would strand the caller's reference.
	// URL and file attachments are the shapes this gateway can carry.
	if ref := req.InputReference; ref != nil && ref.FileID != "" {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "input_reference.file_id is not supported; provide image_url or attach the reference image",
			FailReason: "input_reference_file_id_unsupported",
			Fault:      fact.FaultClient,
		}
	}
	payload := &videoPayload{body: in.Body, contentType: in.ContentType, apiKeyID: in.APIKeyID, req: req}
	// The budget pre-gate: a create call every enabled candidate would
	// price past the key's ceiling is refused here, before anything is
	// dialled — once a provider task is submitted it renders at the
	// operator's cost whether or not the caller is ever billed. Only a
	// certain refusal answers; every other failure of the precheck
	// (unreadable tables, a transient read error) stays silent because
	// the exact gate in Create still runs.
	if videoTasks != nil {
		if err := videoTasks.PrecheckBudget(ctx, in.APIKeyID, req.Model, payload.effectiveSize(), payload.effectiveSeconds()); err != nil {
			var budget *videotask.BudgetExceededError
			if errors.As(err, &budget) {
				return nil, &Rejection{
					Status: http.StatusTooManyRequests, ErrorType: errTypeInsufficientQuota,
					Message: budget.Error(), FailReason: failVideoBudgetExceeded,
					Fault: fact.FaultClient,
				}
			}
		}
	}
	return payload, nil
}

// videoPayload is one create call: the caller's bytes as they arrived,
// the parsed request the door and the router read, and — once a candidate
// is chosen — the submit that candidate is owed, memoized by Supports and
// read back by PrepareUpstream exactly as the call-order contract allows.
type videoPayload struct {
	body        []byte
	contentType string
	apiKeyID    uint
	req         *videos.CreateRequest
	// cand is the candidate Supports approved; submitErr is what
	// PrepareUpstream would fail with. Set by Supports, consumed by
	// PrepareUpstream.
	cand      *Candidate
	submitErr error
}

func (p *videoPayload) Routing() RoutingIntent {
	return RoutingIntent{Model: p.req.Model}
}

// EstimateCost cannot be known at submit time in the token-priced view
// the kernel asks about — a video bill is seconds × a resolution-tier
// price, settled when completion is observed. The estimate the budget
// gate needs is computed from the request's own seconds and the
// candidate's tier table on the task path, not from this view.
func (p *videoPayload) EstimateCost(PricingView) CostEstimate { return CostEstimate{} }

// failVideoBudgetExceeded is the fail reason both budget refusals of this
// modality carry — the door's certain precheck and Create's exact gate —
// so analytics keeps one spelling however a caller hit the ceiling.
const failVideoBudgetExceeded = "video_budget_exceeded"

const (
	// openAIVideoUnsupported is the verdict for a candidate on a base
	// neither task dialect speaks: proxying an OpenAI-shaped video
	// upstream is not a thing this build does.
	openAIVideoUnsupported = "video tasks are served through the dashscope, ark, and kling native dialects in this build"
	// wanFamilyUnsupported is the verdict for a DashScope candidate whose
	// provider model is not a wan video family the dialect can encode.
	wanFamilyUnsupported = "model not in a wan video family the dashscope dialect encodes"
	// klingModelUnsupported is the verdict for a Kling candidate whose
	// provider model is not on the new-design endpoint list: the model
	// name rides in the submit path, so a name without an endpoint would
	// dial a route that does not exist.
	klingModelUnsupported = "model not on the kling new-design endpoint list this dialect encodes"
)

// isArkBase is videos.IsArkBase, overridable in tests for the same
// reason isDashScopeBase is: a local httptest server never carries the
// real hostname, and the branch needs exercising against a live stub.
var isArkBase = videos.IsArkBase

// isKlingBase is videos.IsKlingBase, overridable in tests for the same
// reason isArkBase is. One package, one spelling: both the video and the
// images modality's kling branches route on this detector.
var isKlingBase = videos.IsKlingBase

func (p *videoPayload) Supports(cand Candidate) CandidateVerdict {
	switch {
	case isArkBase(cand.BaseURL):
		// One uniform endpoint family serves every Ark video model, and
		// the model field also accepts inference endpoint ids — a name
		// gate would refuse spellings the endpoint answers to, so the
		// probe judges the mapping instead.
	case isDashScopeBase(cand.BaseURL):
		if videos.DashScopeModelFamily(cand.ProviderModelName) == videos.DashScopeFamilyNone {
			return CandidateVerdict{OK: false, Reason: wanFamilyUnsupported}
		}
	case isKlingBase(cand.BaseURL):
		if !videos.KlingModelSupported(cand.ProviderModelName) {
			return CandidateVerdict{OK: false, Reason: klingModelUnsupported}
		}
	default:
		return CandidateVerdict{OK: false, Reason: openAIVideoUnsupported}
	}
	// Memoize the winner so PrepareUpstream builds the same submit this
	// verdict approved — the coupling the call-order contract sanctions.
	p.cand = &cand
	return CandidateVerdict{OK: true}
}

// attachReference carries the caller's input_reference onto a submit
// request's reference slots, whichever vendor's struct they live in —
// the one block both dialect branches spelled verbatim.
func attachReference(refURL *string, refData *[]byte, refContentType *string, ref *videos.InputRef) {
	if ref == nil {
		return
	}
	*refURL = ref.ImageURL
	if ref.File != nil {
		*refData = ref.File.Data
		*refContentType = ref.File.ContentType
	}
}

// effectiveSize / effectiveSeconds apply the dialect defaults the door
// deliberately left unapplied: an omitted field means the API's own
// default, and the snapshot the task row keeps is what the caller
// effectively asked for.
func (p *videoPayload) effectiveSize() string {
	if p.req.Size != "" {
		return p.req.Size
	}
	return videos.DefaultSize
}

func (p *videoPayload) effectiveSeconds() int {
	if p.req.Seconds != 0 {
		return p.req.Seconds
	}
	return videos.DefaultSeconds
}

func (p *videoPayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	// Supports memoized its verdict; a PrepareUpstream for a different
	// candidate would mean the kernel picked one nobody approved.
	memo := p.cand
	if memo == nil || memo.ProviderModelName != cand.ProviderModelName || memo.BaseURL != cand.BaseURL {
		p.submitErr = fmt.Errorf("video payload prepared for a candidate Supports did not approve")
		return nil, p.submitErr
	}
	var body []byte
	var call UpstreamCall
	call.OriginRelative = true
	call.ContentType = "application/json"
	switch {
	case isArkBase(cand.BaseURL):
		resolution, ratio, ok := videos.MapArkSize(p.effectiveSize())
		if !ok {
			// The door validated the vocabulary; this is unreachable
			// defense against the shared table drifting.
			p.submitErr = fmt.Errorf("size %q has no ark mapping", p.effectiveSize())
			return nil, p.submitErr
		}
		submit := videos.ArkSubmitRequest{
			Model: cand.ProviderModelName, Prompt: p.req.Prompt,
			Resolution: resolution, Ratio: ratio, Duration: p.effectiveSeconds(),
		}
		attachReference(&submit.RefURL, &submit.RefData, &submit.RefContentType, p.req.InputReference)
		encoded, err := videos.EncodeArkSubmit(submit)
		if err != nil {
			p.submitErr = err
			return nil, err
		}
		body = encoded
		call.Path = videos.ArkSubmitPath
	case isKlingBase(cand.BaseURL):
		resolution, ratio, ok := videos.MapKlingSize(p.effectiveSize())
		if !ok {
			// The door validated the vocabulary; this is unreachable
			// defense against the shared table drifting.
			p.submitErr = fmt.Errorf("size %q has no kling mapping", p.effectiveSize())
			return nil, p.submitErr
		}
		submit := videos.KlingSubmitRequest{
			Model: cand.ProviderModelName, Prompt: p.req.Prompt,
			Resolution: resolution, Ratio: ratio, Duration: p.effectiveSeconds(),
		}
		// Two slots, not attachReference's three: the kling dialect has no
		// content-type slot — its bare-base64 payload carries none.
		if ref := p.req.InputReference; ref != nil {
			submit.RefURL = ref.ImageURL
			if ref.File != nil {
				submit.RefData = ref.File.Data
			}
		}
		encoded, err := videos.EncodeKlingSubmit(submit)
		if err != nil {
			p.submitErr = err
			return nil, err
		}
		body = encoded
		// The endpoint family follows the reference's content, judged by
		// the same helper the encoder's body shape answers from — a
		// present-but-empty reference is the text generation it is, on
		// both sides of the wire.
		call.Path = videos.KlingSubmitPath(cand.ProviderModelName, videos.KlingReferenced(p.req.InputReference))
	default:
		resolution, ratio, ok := videos.MapDashScopeSize(p.effectiveSize())
		if !ok {
			p.submitErr = fmt.Errorf("size %q has no dashscope mapping", p.effectiveSize())
			return nil, p.submitErr
		}
		submit := videos.DashScopeSubmitRequest{
			Model: cand.ProviderModelName, Prompt: p.req.Prompt,
			Resolution: resolution, Ratio: ratio, Duration: p.effectiveSeconds(),
		}
		attachReference(&submit.RefURL, &submit.RefData, &submit.RefContentType, p.req.InputReference)
		encoded, err := videos.EncodeDashScopeSubmit(submit)
		if err != nil {
			p.submitErr = err
			return nil, err
		}
		body = encoded
		call.Path = videos.DashScopeSubmitPath
		call.Headers = map[string]string{videos.DashScopeAsyncHeader: "enable"}
	}
	call.Body = body
	return &call, nil
}

// videoTaskStore is the slice of the videotask service a delivery needs;
// a narrow seam so the modality stays testable without a whole service.
type videoTaskStore interface {
	Create(ctx context.Context, task *model.VideoTask, now time.Time) error
	// PrecheckBudget is the create call's budget gate asked before any
	// upstream submit: a certain refusal (every enabled candidate's
	// estimate breaks the key's ceiling) is answered here so no provider
	// task renders that the caller would never be billed for. Its verdict
	// is advisory — the exact gate still runs in Create, where the routed
	// candidate's own price decides.
	PrecheckBudget(ctx context.Context, apiKeyID uint, modelName, size string, seconds int) error
}

// videoTasks is the delivery-side task store, wired by NewService and
// overridable in tests. Nil in a bare assembly (no service built) — a
// delivery that cannot persist a task answers nothing, because a job id
// the caller could never poll again is worse than an error.
var videoTasks videoTaskStore

func (p *videoPayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	if p.submitErr != nil {
		return fact.HandedOn(fact.FaultGateway, "video_prepare: "+p.submitErr.Error(), p.submitErr)
	}
	if p.cand == nil {
		return fact.HandedOn(fact.FaultGateway, "video delivery without an approved candidate", nil)
	}
	defer func() { _ = resp.Body.Close() }()
	// One byte past the budget, so an oversized body is refused rather
	// than silently truncated into something that parses; the budget is
	// the kernel's, not a number this modality invents.
	body, err := io.ReadAll(io.LimitReader(resp.Body, tools.Limits.MaxResponseBytes+1))
	if err != nil {
		if tools.Client.CallerGone() {
			return fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient, "client_disconnected", err)
		}
		return fact.HandedOn(fact.FaultUpstream, "read_body: "+err.Error(), err)
	}
	if int64(len(body)) > tools.Limits.MaxResponseBytes {
		return fact.HandedOn(fact.FaultUpstream, "response_too_large", nil)
	}

	taskID, bizErr, perr := parseVideoSubmitResponse(p.cand.BaseURL, body)
	if perr != nil {
		return fact.HandedOn(fact.FaultUpstream, "video_submit_decode: "+perr.Error(), perr)
	}
	if bizErr != nil {
		// A business refusal inside a 200: the request itself was refused,
		// so it is answered, not failed over — any other candidate of the
		// same dialect would refuse the same body.
		return p.deliverNativeRefusal(tools, body, bizErr)
	}

	if videoTasks == nil {
		return fact.HandedOn(fact.FaultGateway, "video task store is not wired", nil)
	}
	task := &model.VideoTask{
		APIKeyID: p.apiKeyID,
		ModelID:  p.cand.ModelID, ModelName: p.req.Model,
		CandidateID: p.cand.CandidateID, ProviderID: p.cand.ProviderID,
		ProviderModelName:  p.cand.ProviderModelName,
		ProviderTaskID:     taskID,
		DestinationVersion: p.cand.DestinationVersion,
		RequestID:          tools.RequestID,
		RequestSnapshot:    p.SanitizeForLog(BodyClientRequest, p.contentType, p.body),
		Size:               p.effectiveSize(), Seconds: p.effectiveSeconds(),
	}
	// The row outlives this delivery: the upstream accepted the task, so
	// the create must land even if the caller hangs up mid-response —
	// caller-lifetime contexts are exactly the wrong scope for it.
	if err := videoTasks.Create(context.Background(), task, time.Now()); err != nil {
		var budget *videotask.BudgetExceededError
		if errors.As(err, &budget) {
			// The caller's own ceiling, not any candidate's health:
			// answered 429 and settled here so the attempt loop does not
			// walk every other candidate into the same wall.
			body, _ := json.Marshal(map[string]any{"error": map[string]string{
				"message": budget.Error(), "type": errTypeInsufficientQuota,
			}})
			if failed := writeVideoJSON(tools, http.StatusTooManyRequests, body); failed != nil {
				return *failed
			}
			return fact.Rejected(http.StatusTooManyRequests, fact.FaultClient, failVideoBudgetExceeded, budget)
		}
		return fact.HandedOn(fact.FaultGateway, "persist video task: "+err.Error(), err)
	}

	tools.Capture.Upstream(body)
	rendered, _ := json.Marshal(videos.NewResource(task.ID, p.req.Model, p.req.Prompt, task.Size, task.Seconds, task.CreatedAt.Unix()))
	if failed := writeVideoJSON(tools, http.StatusOK, rendered); failed != nil {
		return *failed
	}
	return fact.Succeeded(http.StatusOK)
}

// parseVideoSubmitResponse reads a submit answer in the dialect the
// approved candidate speaks — one branch per vendor, one normalized
// answer out.
func parseVideoSubmitResponse(baseURL string, body []byte) (string, *videos.Refusal, error) {
	if isArkBase(baseURL) {
		id, biz, err := videos.ParseArkSubmitResponse(body)
		if biz != nil {
			return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
		}
		return id, nil, err
	}
	if isKlingBase(baseURL) {
		id, biz, err := videos.ParseKlingSubmitResponse(body)
		return id, klingRefusal(biz), err
	}
	id, biz, err := videos.ParseDashScopeSubmitResponse(body)
	if biz != nil {
		return id, &videos.Refusal{Code: biz.Code, Message: biz.Message}, nil
	}
	return id, nil, err
}

// deliverNativeRefusal answers a business refusal with the dialect's own
// error rendered into the videos error shape, served as 422: the caller's
// request was rejected, not the provider's. Delivered rather than failed
// over — settling with no task bills nothing, and the status the caller
// sees is the refusal's, not a gateway 5xx that reads like an outage.
func (p *videoPayload) deliverNativeRefusal(tools DeliveryTools, upstreamBody []byte, bizErr *videos.Refusal) fact.Delivery {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": bizErr.Code, "message": bizErr.Message}})
	tools.Capture.Upstream(upstreamBody)
	if failed := writeVideoJSON(tools, http.StatusUnprocessableEntity, body); failed != nil {
		return *failed
	}
	return fact.Rejected(http.StatusUnprocessableEntity, fact.FaultClient, "video_business_error", errors.New(bizErr.Code+": "+bizErr.Message))
}

// writeVideoJSON hands the video answers' handshake to the gateway-wide
// commitJSONAnswer — kept as a thin alias so the video call sites keep
// their modality-local spelling.
func writeVideoJSON(tools DeliveryTools, status int, body []byte) *fact.Delivery {
	return commitJSONAnswer(tools, status, body)
}

// NormalizeUpstreamError is not on the video path: the kernel's own
// classification shapes upstream failures before a delivery ever runs.
func (p *videoPayload) NormalizeUpstreamError(status int, _ []byte, _ string) ErrorEnvelope {
	return ErrorEnvelope{Status: status, ErrorType: errTypeUpstream, Message: "upstream error"}
}

// FinalizeUsage is nil on the submit path by design: nothing about a
// create call is billable at the moment it returns. The bill is decided
// when a completed job is first observed, on the task path.
func (p *videoPayload) FinalizeUsage(fact.Delivery) *fact.UsageReported { return nil }

func (p *videoPayload) LogPolicy() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    BodyStoredRendered,
		BodyUpstreamRequest:  BodyStoredRendered,
		BodyUpstreamResponse: BodyStoredRendered,
		BodyClientResponse:   BodyStoredRendered,
	}}
}

func (p *videoPayload) SanitizeForLog(k BodyKind, contentType string, body []byte) string {
	if k == BodyClientResponse {
		// The submit answer is a job resource this gateway rendered —
		// URLs and statuses, no pixels.
		return string(body)
	}
	if strings.HasPrefix(contentType, "multipart/") {
		return videos.RenderBodyForLog(contentType, body)
	}
	if utf8.Valid(body) {
		return videos.RedactRequestBody(body)
	}
	return "[BINARY:" + strconv.Itoa(len(body)) + " bytes]"
}
