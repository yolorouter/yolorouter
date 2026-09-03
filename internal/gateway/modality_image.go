package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

// imageModality serves the OpenAI Images API family — generations in JSON,
// edits in multipart — answered in the API's JSON shape and counted in
// images. It is stateless and shared by every request.
//
// There is no IR here and none is wanted. An images request is routed on a
// subset of its fields and forwarded with only the model field rewritten —
// every other byte the caller sent reaches the provider exactly as written,
// which is both cheaper than a round trip and the only way provider-private
// request fields survive the gateway at all.
type imageModality struct{}

var (
	_ Modality = imageModality{}
	_ Payload  = (*imagePayload)(nil)
)

// NewImageModality returns the modality that serves image payloads.
func NewImageModality() Modality { return imageModality{} }

// ModalityImage names the image modality. The spelling is the model row's
// output-modalities vocabulary too, which is what lets the kernel's
// modality gate match an endpoint's modality against a model's declaration
// without either side knowing the other's types.
const ModalityImage ModalityID = "image"

// dashScopeURLsOnlyReason is the per-candidate refusal a b64_json ask earns
// on a DashScope base, whichever half the request came in on.
const dashScopeURLsOnlyReason = "dashscope serves image URLs only and cannot answer a b64_json request"

// isDashScopeBase is the provider-dialect detector for the images modality:
// candidates whose provider base points at a DashScope host are served
// through the native dialect instead of the OpenAI-compatible passthrough.
// A package variable so tests can point the dialect at their fake upstream;
// production never reassigns it.
var isDashScopeBase = images.IsDashScopeBase

// klingImageURLsOnlyReason and friends are the per-candidate refusals a
// Kling base earns on the shapes its dialect cannot serve.
const (
	klingImageURLsOnlyReason = "kling serves image URLs only and cannot answer a b64_json request"
	klingImageEditsReason    = "the kling image dialect serves generations only in this build"
	klingImageModelReason    = "model not on the kling image endpoint list this dialect encodes"
	klingImageNoStreamReason = "the kling native dialect does not stream images"
)

// imageRequestBudget caps one image-generation exchange. Generation is slow
// in a way chat is not — a high-quality render legitimately takes minutes —
// but not unboundedly slow, and a request that outlives this has stalled
// rather than rendered. Narrowed from the kernel's own request budget, so a
// deployment with a shorter configured budget keeps its shorter budget.
const imageRequestBudget = 10 * time.Minute

func (imageModality) ID() ModalityID { return ModalityImage }

// Limits declares the media budget above and nothing else: the response is
// a bounded JSON body the kernel's own caps already describe, and a modality
// that repeated them here would be one more place for them to drift.
func (imageModality) Limits() TransferLimits {
	return TransferLimits{TotalBudget: imageRequestBudget}
}

// Admit parses the caller's request far enough to route it, and refuses what
// no candidate could have served — a body that does not parse, a missing
// model or prompt, a streaming ask for a model family whose upstreams do
// not stream. The edits route parses its multipart upload instead of JSON;
// the two halves share the response side, which is the same OpenAI images
// shape either way.
func (imageModality) Admit(_ context.Context, in Ingress) (Payload, *Rejection) {
	if in.Path == images.EditPath {
		return admitEdit(in)
	}
	req, err := images.ParseRequest(in.Body)
	if err != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: "invalid request body", FailReason: "parse: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	if req.Model == "" {
		return nil, rejectMissingField("model", "empty_model")
	}
	if req.Prompt == "" {
		return nil, rejectMissingField("prompt", "empty_prompt")
	}
	if req.Stream && !strings.HasPrefix(req.Model, gptImagePrefix) {
		return nil, rejectStreamingModel()
	}
	return &imagePayload{body: in.Body, req: req}, nil
}

// rejectMissingField is the door refusal both halves share for a required
// field the caller did not send.
func rejectMissingField(field, reason string) *Rejection {
	return &Rejection{
		Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
		Message: field + " is required", FailReason: reason,
		Fault: fact.FaultClient,
	}
}

// rejectStreamingModel is the door refusal both halves share for a
// streaming ask outside the family whose upstreams stream.
func rejectStreamingModel() *Rejection {
	return &Rejection{
		Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
		Message:    "Streaming is only supported for gpt-image-* models",
		FailReason: "image_streaming_model_unsupported",
		Fault:      fact.FaultClient,
	}
}

// admitEdit is the edits half of Admit: the same door rules over a multipart
// body, plus the one field the generations half does not owe — a reference
// image, without which no edit candidate has anything to work on.
func admitEdit(in Ingress) (Payload, *Rejection) {
	req, err := images.ParseEditRequest(in.ContentType, in.Body)
	if err != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: "invalid multipart body", FailReason: "parse: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	if req.Model == "" {
		return nil, rejectMissingField("model", "empty_model")
	}
	if req.Prompt == "" {
		return nil, rejectMissingField("prompt", "empty_prompt")
	}
	if len(req.Images) == 0 {
		return nil, rejectMissingField("image", "empty_image")
	}
	if req.Stream && !strings.HasPrefix(req.Model, gptImagePrefix) {
		return nil, rejectStreamingModel()
	}
	return &imagePayload{body: in.Body, contentType: in.ContentType, edit: req}, nil
}

// imagePayload is one image request — a generation or an edit. Like the
// text payload it holds no kernel object: everything it needs to reach the
// outside world arrives as DeliveryTools.
type imagePayload struct {
	body []byte
	// contentType is the caller's own Content-Type, carried for the edits
	// half: a multipart body cannot be re-read without its boundary.
	contentType string
	req         *images.Request
	// edit is non-nil on the edits route and nil on generations; the two
	// halves branch on its presence rather than on the path again, so a
	// payload is self-describing once admitted.
	edit *images.EditRequest
	// cand is the candidate PrepareUpstream built for, read back by the
	// delivery that follows — the same memoization contract the ordered
	// payload wrapper asserts.
	cand Candidate
	// delivered is the settled delivery's response body, kept for the usage
	// the settlement will ask for. Only the delivery that ends the request
	// reaches FinalizeUsage, so there is exactly one body worth keeping.
	delivered []byte
	// rewritten caches the model-rewritten multipart per target provider
	// model: re-encoding a 20 MiB upload is the expensive step of an edits
	// attempt, and failover retries the same provider model far more often
	// than it changes it.
	rewritten map[string]rewrittenMultipart
}

// rewrittenMultipart is one cached edits re-encode: the body and the
// content type that carries the writer's fresh boundary. The two travel
// together or the boundary in the type describes a body that is not the
// one sent.
type rewrittenMultipart struct {
	body        []byte
	contentType string
}

// isEdit reports which half of the Images API this payload serves.
func (p *imagePayload) isEdit() bool { return p.edit != nil }

// requestModel names the model the caller asked for, whichever half the
// request came in on — both spell it as a scalar field the routing reads.
func (p *imagePayload) requestModel() string {
	if p.isEdit() {
		return p.edit.Model
	}
	return p.req.Model
}

// responseFormat states the delivery format the caller asked for, whichever
// half the request came in on — both spell it as a scalar field the
// DashScope verdicts key on.
func (p *imagePayload) responseFormat() string {
	if p.isEdit() {
		return p.edit.ResponseFormat
	}
	return p.req.ResponseFormat
}

// requestAxes states the pricing axes the caller sent — the count asked
// for, the quality and size the snapshot keys on — whichever half the
// request came in on.
func (p *imagePayload) requestAxes() (requested int, quality, size string) {
	if p.isEdit() {
		return p.edit.N, p.edit.Quality, p.edit.Size
	}
	return p.req.N, p.req.Quality, p.req.Size
}

func (p *imagePayload) Routing() RoutingIntent {
	return RoutingIntent{Model: p.requestModel(), Stream: p.streamAsked()}
}

// EstimateCost cannot name a figure before the upstream answers — the count
// that bills is how many images come back, not how many were asked for — so
// it says unknown in this modality's own unit. The unit is the point: it is
// what settlement will count in, before there is a number to fill it with.
func (p *imagePayload) EstimateCost(PricingView) CostEstimate {
	return CostEstimate{Known: false, Unit: fact.UnitImage}
}

// Supports accepts the candidates whose provider can serve an images
// request: the OpenAI wire family's base (the images API rides the same JSON
// shape, bearer credential, and base URL), or a DashScope host, which the
// native dialect serves through its own endpoint on the same origin. A
// provider negotiated to any other primary protocol has no images API to
// forward to, and refusing it here costs one candidate instead of a request.
//
// The DashScope dialect answers with URLs only, so a caller's b64_json ask
// is one it cannot honour and it says so per candidate rather than at the
// door, where it would wrongly refuse an OpenAI-compatible alternative.
func (p *imagePayload) Supports(cand Candidate) CandidateVerdict {
	if cand.EgressProtocol != protocols.ProtocolOpenAI {
		return CandidateVerdict{
			OK:     false,
			Reason: "provider does not serve the images API (egress " + string(cand.EgressProtocol) + ")",
		}
	}
	if isDashScopeBase(cand.BaseURL) && p.responseFormat() == "b64_json" {
		return CandidateVerdict{
			OK:     false,
			Reason: dashScopeURLsOnlyReason,
		}
	}
	// The Kling dialect serves the generation half only, from a whitelist
	// the 2026-09-15 retirement leaves one mainline model on, answering
	// with URLs — the same shapes a DashScope candidate refuses, in the
	// Kling dialect's own words.
	if isKlingBase(cand.BaseURL) {
		switch {
		case p.responseFormat() == "b64_json":
			return CandidateVerdict{OK: false, Reason: klingImageURLsOnlyReason}
		case p.isEdit():
			return CandidateVerdict{OK: false, Reason: klingImageEditsReason}
		case !images.KlingImageModelSupported(cand.ProviderModelName):
			return CandidateVerdict{OK: false, Reason: klingImageModelReason}
		case p.streamAsked():
			return CandidateVerdict{OK: false, Reason: klingImageNoStreamReason}
		}
	}
	if p.isEdit() && isDashScopeBase(cand.BaseURL) {
		if p.edit.Mask != nil {
			return CandidateVerdict{
				OK:     false,
				Reason: "the dashscope edit dialect has no field to carry a mask upload",
			}
		}
		if len(p.edit.UnmappedFields) > 0 {
			return CandidateVerdict{
				OK: false,
				Reason: "the dashscope edit dialect has no field for: " +
					strings.Join(p.edit.UnmappedFields, ", "),
			}
		}
	}
	if p.streamAsked() {
		// The alias between the caller's name and the provider's can change
		// this answer, which is why the door's prefix check is repeated here
		// against the provider's own name for the model. The native dialect
		// has no streaming half at all, prefix or no.
		if isDashScopeBase(cand.BaseURL) {
			return CandidateVerdict{
				OK:     false,
				Reason: "the dashscope native dialect does not stream images",
			}
		}
		if !strings.HasPrefix(cand.ProviderModelName, gptImagePrefix) {
			return CandidateVerdict{
				OK:     false,
				Reason: "streaming is supported for gpt-image-* models only",
			}
		}
	}
	return CandidateVerdict{OK: true}
}

// PrepareUpstream builds the body and path for one candidate.
//
// An OpenAI-compatible provider gets the caller's bytes with only the model
// field rewritten, posted to the images endpoint on the provider's base. A
// DashScope provider gets a native request re-encoded from the fields the
// gateway parsed, posted to the dialect's own endpoint on the provider's
// origin — the base's version segment belongs to the compatible-mode route
// and would corrupt the native path. On the edits half the model rewrite is
// a multipart re-encode, cached per target model because it is the
// expensive step of an attempt.
func (p *imagePayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	p.cand = cand
	if p.isEdit() {
		return p.prepareEditUpstream(cand)
	}
	if isKlingBase(cand.BaseURL) {
		// The kling-native extension fields ride along from the caller's
		// own body — the dialect branch of the passthrough promise.
		body, err := images.EncodeKlingImageRequest(p.req.Prompt, cand.ProviderModelName, p.req.N, p.req.Size,
			images.ParseKlingNativeFields(p.body))
		if err != nil {
			return nil, fmt.Errorf("encode kling image request: %w", err)
		}
		return &UpstreamCall{
			Path:           images.KlingImageSubmitPathFor(cand.ProviderModelName),
			Body:           body,
			ContentType:    "application/json",
			OriginRelative: true,
		}, nil
	}
	if isDashScopeBase(cand.BaseURL) {
		body, err := images.EncodeRequest(p.req.Prompt, cand.ProviderModelName, p.req.N, p.req.Size)
		if err != nil {
			return nil, fmt.Errorf("encode dashscope request: %w", err)
		}
		return &UpstreamCall{
			Path:           images.GenerationPath,
			Body:           body,
			ContentType:    "application/json",
			OriginRelative: true,
		}, nil
	}
	out, err := rewriteModelField(p.body, cand.ProviderModelName)
	if err != nil {
		return nil, fmt.Errorf("rewrite model field: %w", err)
	}
	return &UpstreamCall{
		Path:        "/v1/images/generations",
		Body:        out,
		ContentType: "application/json",
	}, nil
}

// prepareEditUpstream is the edits half of PrepareUpstream. An
// OpenAI-compatible provider gets the rewritten multipart, cached per
// target model because the re-encode is the expensive step of an attempt.
// A DashScope provider gets the upload re-encoded into the native dialect:
// the reference images become base64 data URI content items beside the
// instruction text, posted to the dialect's own endpoint on the provider's
// origin — the same arrangement the generation half uses.
func (p *imagePayload) prepareEditUpstream(cand Candidate) (*UpstreamCall, error) {
	if isDashScopeBase(cand.BaseURL) {
		body, err := images.EncodeEditRequest(p.edit.Prompt, cand.ProviderModelName, p.edit.Images, p.edit.N, p.edit.Size)
		if err != nil {
			return nil, fmt.Errorf("encode dashscope edit request: %w", err)
		}
		return &UpstreamCall{
			Path:           images.GenerationPath,
			Body:           body,
			ContentType:    "application/json",
			OriginRelative: true,
		}, nil
	}
	if p.rewritten == nil {
		p.rewritten = make(map[string]rewrittenMultipart)
	}
	if cached, ok := p.rewritten[cand.ProviderModelName]; ok {
		return &UpstreamCall{Path: images.EditPath, Body: cached.body, ContentType: cached.contentType}, nil
	}
	out, contentType, err := images.RewriteEditModelField(p.contentType, p.body, cand.ProviderModelName)
	if err != nil {
		return nil, fmt.Errorf("rewrite model field: %w", err)
	}
	p.rewritten[cand.ProviderModelName] = rewrittenMultipart{body: out, contentType: contentType}
	return &UpstreamCall{Path: images.EditPath, Body: out, ContentType: contentType}, nil
}

// NormalizeUpstreamError decides what the caller is told about an upstream
// failure: the status class, and a message that does not quote the upstream
// body, which can name the provider, the model behind the alias, or the
// account it was billed to.
func (p *imagePayload) NormalizeUpstreamError(status int, _ []byte, _ string) ErrorEnvelope {
	class := classifyUpstreamStatus(status)
	errType := class.ErrorType
	if errType == "" {
		errType = errTypeUpstream
	}
	return ErrorEnvelope{Status: status, ErrorType: errType, Message: safeUpstreamMessage(status)}
}

// Deliver forwards a whole upstream response to the caller.
//
// There is no re-encode: an images response is a bounded JSON body whose
// model name never appears in it (the API's response shape has no model
// field), so what the upstream sent is what the caller gets, byte for byte.
// The streaming half is the one delivery this modality forwards as it
// arrives rather than whole.
func (p *imagePayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	if p.streamAsked() {
		return p.deliverStream(tools, resp)
	}
	defer func() { _ = resp.Body.Close() }()

	// One byte past the cap, so an oversized body is refused rather than
	// silently truncated into something that parses.
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

	// A Kling answer is the first half of a task conversation: the delivery
	// drives it to the terminal task here and shapes the OpenAI answer from
	// what the task delivered. The poll runs on its own bounded budget,
	// deliberately decoupled from the caller's connection — the accepted
	// task renders and bills upstream whether or not the caller waits, so
	// settlement must observe completion (the video task domain's own
	// caller-lifetime-contexts-are-wrong-scope stance).
	if isKlingBase(p.cand.BaseURL) {
		return p.deliverKling(tools, body)
	}

	// A DashScope answer arrives in the dialect's own shape and becomes the
	// OpenAI shape the caller asked in. A business error arrives with HTTP
	// 200 and a code: the request itself was refused, so it is answered, not
	// failed over — the same body would be refused by any other retry.
	delivered := body
	if isDashScopeBase(p.cand.BaseURL) {
		converted, _, derr := images.DecodeResponse(body)
		if derr != nil {
			if images.IsBusinessError(derr) {
				return p.deliverDashScopeRefusal(tools, body, derr)
			}
			return fact.HandedOn(fact.FaultUpstream, "dashscope_decode: "+derr.Error(), derr)
		}
		delivered = converted
	}

	// An OK answer that delivered no images is not a delivery: the caller
	// cannot use it and must not be billed for it. Handed back to the chain
	// rather than settled, so a healthier candidate can still answer.
	parsed, perr := images.ParseResponse(delivered)
	if perr != nil || parsed.ImageCount() == 0 {
		return fact.HandedOn(fact.FaultUpstream, "image_response_empty", nil)
	}

	tools.Capture.Upstream(body)
	p.delivered = delivered

	if failed := commitJSONAnswer(tools, resp.StatusCode, delivered); failed != nil {
		return *failed
	}
	return fact.Succeeded(resp.StatusCode)
}

// deliverKling answers a Kling generation: the submit's 200 carried a task
// id, the poller drives that task to its terminal state, and the caller
// receives either the OpenAI images shape built from the task's URLs or
// the refusal the task produced. Everything after the accepted submit is
// settled here — the task is rendering and billing upstream, so handing a
// poll failure back to the attempt loop would buy the caller a second
// billable task, not a second chance.
func (p *imagePayload) deliverKling(tools DeliveryTools, submitBody []byte) fact.Delivery {
	taskID, biz, err := images.ParseKlingImageSubmitResponse(submitBody)
	if err != nil {
		// The submit answered HTTP 200, so the task was most likely
		// accepted and is billing upstream already — a decode failure here
		// settles as an upstream error rather than handing back to the
		// attempt loop, whose re-submit would buy a second billable task.
		tools.Capture.Upstream(submitBody)
		return p.settleKlingUpstreamError(tools, "kling_submit_decode: "+err.Error(), err)
	}
	if biz != nil {
		tools.Capture.Upstream(submitBody)
		return p.deliverKlingRefusal(tools, biz.Code, biz.Message)
	}
	if klingImagePoll == nil {
		// The task is in flight upstream; this deployment cannot drive it
		// to settlement. Settled, for the same re-submit reason as above.
		tools.Capture.Upstream(submitBody)
		return p.settleKlingUpstreamError(tools, "kling image poller is not wired", nil)
	}
	task, finalBody, biz, perr := klingImagePoll.Poll(context.Background(), p.cand.ProviderID, p.cand.DestinationVersion, taskID,
		images.KlingImageTaskPathPrefixFor(p.cand.ProviderModelName))
	if perr != nil {
		// No terminal body arrived; the submit answer is the only
		// upstream evidence this delivery has.
		tools.Capture.Upstream(submitBody)
		return p.settleKlingUpstreamError(tools, "kling_image_poll: "+perr.Error(), perr)
	}
	// The terminal task body is the upstream response this delivery is
	// decided by — it carries the task id, the delivered URLs, the
	// deduction, and the refusal reason, everything the submit answer
	// holds and more — so it is what the audit trail records; capture is
	// an assignment, so the two halves cannot both live in one row.
	tools.Capture.Upstream(finalBody)
	if biz != nil {
		return p.deliverKlingRefusal(tools, biz.Code, biz.Message)
	}
	if task.Failed {
		return p.deliverKlingRefusal(tools, "kling_task_failed", task.StatusMsg)
	}
	if len(task.ImageURLs) == 0 {
		return p.settleKlingUpstreamError(tools, "kling task succeeded without images", nil)
	}
	converted, cerr := images.EncodeKlingImagesOpenAI(task.ImageURLs)
	if cerr != nil {
		return p.settleKlingUpstreamError(tools, "kling answer marshal: "+cerr.Error(), cerr)
	}
	p.delivered = converted
	if failed := commitJSONAnswer(tools, http.StatusOK, converted); failed != nil {
		return *failed
	}
	return fact.Succeeded(http.StatusOK)
}

// deliverKlingRefusal answers a refusal with the OpenAI error shape served
// as 422: the caller's request was rejected, not the provider's — the same
// verdict the DashScope refusal path settles by.
func (p *imagePayload) deliverKlingRefusal(tools DeliveryTools, code, message string) fact.Delivery {
	if message == "" {
		message = "kling refused the request"
	}
	rendered := fmt.Sprintf("kling error %s: %s", code, message)
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": rendered, "type": "invalid_request_error", "code": code},
	})
	if failed := commitJSONAnswer(tools, http.StatusUnprocessableEntity, body); failed != nil {
		return *failed
	}
	return fact.Rejected(http.StatusUnprocessableEntity, fact.FaultClient, "kling_business_error",
		errors.New(code+": "+message))
}

// settleKlingUpstreamError reports a post-acceptance failure to the caller
// as a settled upstream error: the accepted task is billing upstream, so
// this delivery must end here rather than fail over.
func (p *imagePayload) settleKlingUpstreamError(tools DeliveryTools, reason string, err error) fact.Delivery {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": reason, "type": "upstream_error", "code": "upstream_error"},
	})
	if failed := commitJSONAnswer(tools, http.StatusBadGateway, body); failed != nil {
		return *failed
	}
	return fact.Undelivered(http.StatusBadGateway, fact.VerdictSettled, fact.FaultUpstream, reason, err)
}

// deliverDashScopeRefusal answers a business refusal with the dialect's own
// error rendered into the OpenAI shape, served as 422: the caller's request
// was rejected, not the provider's. Delivered rather than failed over —
// settling with no usage bills nothing, and the status the caller sees is
// the refusal's, not a gateway 5xx that reads like an outage.
func (p *imagePayload) deliverDashScopeRefusal(tools DeliveryTools, upstreamBody []byte, err error) fact.Delivery {
	body, contentType := images.NormalizeError(http.StatusUnprocessableEntity, upstreamBody)
	tools.Capture.Upstream(upstreamBody)
	tools.Client.Inject(http.Header{"Content-Type": {contentType}})
	if cerr := tools.Client.Commit(http.StatusUnprocessableEntity); cerr != nil {
		return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled, fact.FaultGateway,
			"commit_failed: "+cerr.Error(), cerr)
	}
	if _, werr := tools.Client.Write(body); werr != nil {
		return fact.Truncated(http.StatusUnprocessableEntity, 499, fact.FaultClient, "client_write_timeout", werr)
	}
	if ferr := tools.Client.Flush(); ferr != nil {
		return fact.Truncated(http.StatusUnprocessableEntity, 499, fact.FaultClient, "client_write_timeout", ferr)
	}
	return fact.Rejected(http.StatusUnprocessableEntity, fact.FaultClient, "dashscope_business_error", err)
}

// FinalizeUsage states the billable quantities of the settled delivery:
// how many images arrived (the count that bills), what was asked for (the
// discrepancy an operator will be questioned about), the pricing axes the
// caller sent, and — when the upstream reported them — the token counts a
// token-billed image model settles by. The unit says what the modality
// counts; which quantity actually prices the request is the candidate's
// billing mode, decided by settlement, not here.
func (p *imagePayload) FinalizeUsage(d fact.Delivery) *fact.UsageReported {
	// A stream settles from what its pump attached to the delivery — there
	// is no whole body to parse. Returning the delivery's own usage here is
	// what keeps it: settlement re-asks this method, and a nil answer would
	// clear what the pump reported.
	if d.Usage != nil {
		return d.Usage
	}
	parsed, err := images.ParseResponse(p.delivered)
	if err != nil || parsed == nil {
		return nil
	}
	requested, quality, size := p.requestAxes()
	report := &fact.UsageReported{
		Unit:      fact.UnitImage,
		Source:    fact.UsageFromUpstream,
		Count:     parsed.ImageCount(),
		Requested: requested,
		Quality:   quality,
		Size:      size,
	}
	if parsed.Usage != nil {
		report.Prompt = parsed.Usage.InputTokens
		report.Completion = parsed.Usage.OutputTokens
		report.Total = parsed.Usage.TotalTokens
	}
	return report
}

// requestStorage says how this request's two halves keep their request
// bodies: a generations request is the caller's own small JSON and stays
// raw; an edits upload renders to its multipart shape so its pixels never
// reach storage.
func (p *imagePayload) requestStorage() BodyStorage {
	if p.isEdit() {
		return BodyStoredRendered
	}
	return BodyStoredRaw
}

// LogPolicy stores the responses rendered: a b64_json answer is megabytes of
// base64 whose only diagnostic value is that it existed and how big it was,
// and a debug table is not an image store.
func (p *imagePayload) LogPolicy() LogPolicy {
	if p.streamAsked() {
		// A streamed answer is partial events with whole base64 images
		// inside them — the one images body whose bytes are worth less to a
		// debug row than the row's own size. Dropping the client response
		// also means the capture file is never opened; the requests and the
		// settlement carry what an operator can read.
		return LogPolicy{Store: map[BodyKind]BodyStorage{
			BodyClientRequest:    p.requestStorage(),
			BodyUpstreamRequest:  p.requestStorage(),
			BodyUpstreamResponse: BodyDropped,
			BodyClientResponse:   BodyDropped,
		}}
	}
	return LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    p.requestStorage(),
		BodyUpstreamRequest:  p.requestStorage(),
		BodyUpstreamResponse: BodyStoredRendered,
		BodyClientResponse:   BodyStoredRendered,
	}}
}

// SanitizeForLog renders one body for the audit trail. Generation requests
// are the caller's own small JSON and pass through untouched; an edit
// request renders its multipart shape with file parts as size notes — but
// the native edit request is JSON whose image fields carry the uploads as
// base64 data URIs, and that one gets the same redaction the responses get:
// a debug row is not an image store on either side of the wire. Responses
// run through the base64 redactor, which keeps every field but the image
// payloads themselves.
func (p *imagePayload) SanitizeForLog(k BodyKind, contentType string, body []byte) string {
	if k == BodyUpstreamResponse || k == BodyClientResponse {
		return redactBase64Images(body)
	}
	if p.isEdit() {
		if strings.HasPrefix(contentType, "multipart/") {
			return images.RenderEditBodyForLog(contentType, body)
		}
		return redactImageRequest(body)
	}
	return string(body)
}

// b64ImageJSON matches a base64 image payload long enough that keeping it
// verbatim is the difference between a debug row and a megabyte blob. The
// well-known field names cover the OpenAI images shape and the b64 fields
// providers nest inside content arrays; the optional data-URI prefix covers
// the same payloads in the native edit request, where the upload rides as
// data:<mime>;base64,… inside the image field. Everything else in the body
// is kept. The length floor is as high as Go's regexp repeat limit allows
// (1000): a real image payload is orders of magnitude longer, and short
// values stay.
var b64ImageJSON = regexp.MustCompile(`"(b64_json|image)"\s*:\s*"(data:[^"]{0,128};base64,)?([A-Za-z0-9+/=]{1000,})"`)

// b64OmitNote is what a redacted payload is replaced with — the fact of the
// image and its length, which is all a protocol bug needs to see.
const b64OmitNote = "[base64 image omitted: %d chars]"

// redactBase64Images replaces long base64 image payloads with a note of how
// long they were, keeping the key and everything else around it — the result
// is still the JSON it replaced, minus the image bytes. Byte-level rather
// than a JSON walk: the body may be an error envelope, a partial frame, or a
// provider's private variant — any of which a strict decode would refuse,
// and the audit row must still get its (redacted) copy.
func redactBase64Images(body []byte) string {
	return b64ImageJSON.ReplaceAllStringFunc(string(body), func(match string) string {
		m := b64ImageJSON.FindStringSubmatch(match)
		if m == nil {
			return match
		}
		return `"` + m[1] + `":"` + fmt.Sprintf(b64OmitNote, len(m[3])) + `"`
	})
}

// imageDataURIJSON is the request-side redactor: a data URI inside an image
// field is an image payload by construction — this gateway put it there — so
// it is redacted at any length. The response redactor's length floor exists
// to keep short response values from being mangled as thumbnails; a request
// has no such false positives to guard against, and a caller's upload can
// honestly be a few hundred bytes.
var imageDataURIJSON = regexp.MustCompile(`"(b64_json|image)"\s*:\s*"data:[^"]{0,128};base64,([A-Za-z0-9+/=]+)"`)

// redactImageRequest renders a request body for the audit trail: the
// response redactor first (a caller's JSON request can embed long b64
// values too), then every remaining data-URI image field whatever its
// length.
func redactImageRequest(body []byte) string {
	out := redactBase64Images(body)
	return imageDataURIJSON.ReplaceAllStringFunc(out, func(match string) string {
		m := imageDataURIJSON.FindStringSubmatch(match)
		if m == nil {
			return match
		}
		return `"` + m[1] + `":"` + fmt.Sprintf(b64OmitNote, len(m[2])) + `"`
	})
}
