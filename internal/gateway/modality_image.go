package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

// imageModality serves the OpenAI Images API family: JSON in, JSON out,
// counted in images. It is stateless and shared by every request.
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

// isDashScopeBase is the provider-dialect detector for the images modality:
// candidates whose provider base points at a DashScope host are served
// through the native dialect instead of the OpenAI-compatible passthrough.
// A package variable so tests can point the dialect at their fake upstream;
// production never reassigns it.
var isDashScopeBase = images.IsDashScopeBase

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
// model or prompt, a streaming ask this modality does not carry.
func (imageModality) Admit(_ context.Context, in Ingress) (Payload, *Rejection) {
	req, err := images.ParseRequest(in.Body)
	if err != nil {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: "invalid request body", FailReason: "parse: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	if req.Model == "" {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: "model is required", FailReason: "empty_model",
			Fault: fact.FaultClient,
		}
	}
	if req.Prompt == "" {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message: "prompt is required", FailReason: "empty_prompt",
			Fault: fact.FaultClient,
		}
	}
	if req.Stream {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: errTypeInvalidRequest,
			Message:    "streaming is not supported for image generation",
			FailReason: "image_streaming_unsupported",
			Fault:      fact.FaultClient,
		}
	}
	return &imagePayload{body: in.Body, req: req}, nil
}

// imagePayload is one image request. Like the text payload it holds no
// kernel object: everything it needs to reach the outside world arrives as
// DeliveryTools.
type imagePayload struct {
	body []byte
	req  *images.Request
	// cand is the candidate PrepareUpstream built for, read back by the
	// delivery that follows — the same memoization contract the ordered
	// payload wrapper asserts.
	cand Candidate
	// delivered is the settled delivery's response body, kept for the usage
	// the settlement will ask for. Only the delivery that ends the request
	// reaches FinalizeUsage, so there is exactly one body worth keeping.
	delivered []byte
}

func (p *imagePayload) Routing() RoutingIntent {
	return RoutingIntent{Model: p.req.Model}
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
	if isDashScopeBase(cand.BaseURL) && p.req.ResponseFormat == "b64_json" {
		return CandidateVerdict{
			OK:     false,
			Reason: "dashscope serves image URLs only and cannot answer a b64_json request",
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
// and would corrupt the native path.
func (p *imagePayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	p.cand = cand
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
// There is no streaming half and no re-encode: an images response is a
// bounded JSON body whose model name never appears in it (the API's response
// shape has no model field), so what the upstream sent is what the caller
// gets, byte for byte.
func (p *imagePayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
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

	tools.Client.Inject(http.Header{"Content-Type": {"application/json"}})
	if cerr := tools.Client.Commit(resp.StatusCode); cerr != nil {
		return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled, fact.FaultGateway,
			"commit_failed: "+cerr.Error(), cerr)
	}
	if _, werr := tools.Client.Write(delivered); werr != nil {
		return p.clientWriteFailure(resp.StatusCode, werr)
	}
	// A few KB of JSON can sit entirely inside net/http's buffer; only the
	// flush tells the truth about whether the caller received anything.
	if ferr := tools.Client.Flush(); ferr != nil {
		return p.clientWriteFailure(resp.StatusCode, ferr)
	}
	return fact.Succeeded(resp.StatusCode)
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
		return p.clientWriteFailure(http.StatusUnprocessableEntity, werr)
	}
	if ferr := tools.Client.Flush(); ferr != nil {
		return p.clientWriteFailure(http.StatusUnprocessableEntity, ferr)
	}
	return fact.Rejected(http.StatusUnprocessableEntity, fact.FaultClient, "dashscope_business_error", err)
}

// clientWriteFailure reports a caller who stopped receiving after the
// response was committed: served the status they got, settled as 499,
// because the bytes never landed.
func (p *imagePayload) clientWriteFailure(served int, err error) fact.Delivery {
	return fact.Truncated(served, 499, fact.FaultClient, "client_write_timeout", err)
}

// FinalizeUsage states the billable quantities of the settled delivery:
// how many images arrived (the count that bills), what was asked for (the
// discrepancy an operator will be questioned about), the pricing axes the
// caller sent, and — when the upstream reported them — the token counts a
// token-billed image model settles by. The unit says what the modality
// counts; which quantity actually prices the request is the candidate's
// billing mode, decided by settlement, not here.
func (p *imagePayload) FinalizeUsage(fact.Delivery) *fact.UsageReported {
	parsed, err := images.ParseResponse(p.delivered)
	if err != nil || parsed == nil {
		return nil
	}
	report := &fact.UsageReported{
		Unit:      fact.UnitImage,
		Source:    fact.UsageFromUpstream,
		Count:     parsed.ImageCount(),
		Requested: p.req.N,
		Quality:   p.req.Quality,
		Size:      p.req.Size,
	}
	if parsed.Usage != nil {
		report.Prompt = parsed.Usage.InputTokens
		report.Completion = parsed.Usage.OutputTokens
		report.Total = parsed.Usage.TotalTokens
	}
	return report
}

// LogPolicy keeps the requests raw and stores the responses rendered: a
// b64_json answer is megabytes of base64 whose only diagnostic value is that
// it existed and how big it was, and a debug table is not an image store.
func (p *imagePayload) LogPolicy() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    BodyStoredRaw,
		BodyUpstreamRequest:  BodyStoredRaw,
		BodyUpstreamResponse: BodyStoredRendered,
		BodyClientResponse:   BodyStoredRendered,
	}}
}

// SanitizeForLog renders one body for the audit trail. Requests are the
// caller's own small JSON and pass through untouched; responses run through
// the base64 redactor, which keeps every field but the image payloads
// themselves.
func (p *imagePayload) SanitizeForLog(k BodyKind, _ string, body []byte) string {
	if k != BodyUpstreamResponse && k != BodyClientResponse {
		return string(body)
	}
	return redactBase64Images(body)
}

// b64ImageJSON matches a base64 image payload long enough that keeping it
// verbatim is the difference between a debug row and a megabyte blob. The
// well-known field names cover the OpenAI images shape and the b64 fields
// providers nest inside content arrays; everything else in the body is kept.
// The length floor is as high as Go's regexp repeat limit allows (1000): a
// real image payload is orders of magnitude longer, and short values stay.
var b64ImageJSON = regexp.MustCompile(`"(b64_json|image)"\s*:\s*"([A-Za-z0-9+/=]{1000,})"`)

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
		return `"` + m[1] + `":"` + fmt.Sprintf(b64OmitNote, len(m[2])) + `"`
	})
}
