package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// textModality serves the chat/completions family: JSON in, JSON or SSE out,
// counted in tokens. It is stateless and shared by every request.
type textModality struct{}

var (
	_ Modality = textModality{}
	_ Payload  = (*textPayload)(nil)
)

// NewTextModality returns the modality that serves text payloads.
func NewTextModality() Modality { return textModality{} }

func (textModality) ID() ModalityID { return ModalityText }

// Limits asks for nothing of its own. Text has no frame that a chat line comes
// close to filling and no render that legitimately runs for minutes, so the
// kernel's own caps already describe it; a modality that repeated them here
// would be one more place for them to drift.
func (textModality) Limits() TransferLimits { return TransferLimits{} }

// Admit parses the caller's request far enough to route it, and refuses what
// no candidate could have served.
//
// Everything decided here is a property of the request alone — a body that
// does not parse, a model the caller did not name, a structure the protocol
// requires. Anything that depends on WHICH upstream ends up serving it belongs
// in Supports, where a refusal costs one candidate instead of the request.
func (textModality) Admit(_ context.Context, in Ingress) (Payload, *Rejection) {
	// Gemini carries neither model nor stream in the body: both live in the
	// path, so they have to be recovered before the peek can use them.
	var pathModel string
	var pathStream bool
	if in.Protocol == protocols.ProtocolGemini {
		m, s, ok := parseGeminiPath(in.Path)
		if !ok {
			return nil, &Rejection{
				Status: 400, ErrorType: errTypeInvalidRequest,
				Message: "invalid request path", FailReason: "invalid_gemini_path",
				Fault: fact.FaultClient,
			}
		}
		pathModel, pathStream = m, s
	}

	meta, err := peekIngress(in.Protocol, in.Body, pathModel, pathStream)
	if err != nil {
		return nil, &Rejection{
			Status: 400, ErrorType: errTypeInvalidRequest,
			Message: "invalid request body", FailReason: "parse: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	if meta.Model == "" {
		return nil, &Rejection{
			Status: 400, ErrorType: errTypeInvalidRequest,
			Message: "model is required", FailReason: "empty_model",
			Fault: fact.FaultClient,
		}
	}
	if err := meta.validate(); err != nil {
		return nil, &Rejection{
			Status: 400, ErrorType: errTypeInvalidRequest,
			Message: err.Error(), FailReason: "validate: " + err.Error(),
			Fault: fact.FaultClient,
		}
	}
	// The full structural decode, run once here rather than per candidate.
	// Catching a malformed body now is what keeps it from surfacing later as a
	// misleading "all upstream candidates failed" after the chain has already
	// started.
	if err := validateIngressBody(in.Protocol, in.Body, meta.Model, meta.Stream); err != nil {
		return nil, &Rejection{
			Status: 400, ErrorType: errTypeInvalidRequest,
			Message:    "invalid request body: " + err.Error(),
			FailReason: "invalid_request: " + err.Error(),
			Fault:      fact.FaultClient,
		}
	}

	return &textPayload{ingress: in.Protocol, body: in.Body, meta: meta}, nil
}

// textPayload is one text request.
//
// It holds no kernel object on purpose. Everything it needs to reach the
// outside world arrives as DeliveryTools, which is what makes the seam real:
// a payload that could reach the exchange or the service would settle, bill
// and log for itself, and the eight entry points that each did exactly that
// are what this is replacing.
type textPayload struct {
	ingress protocols.ProtocolID
	body    []byte
	meta    *ingressMeta
	// cand is the candidate PrepareUpstream built for, read back by the
	// delivery that follows. The kernel guarantees the two run in that order
	// and for the same candidate.
	cand Candidate
}

func (p *textPayload) Routing() RoutingIntent {
	return RoutingIntent{Model: p.meta.Model, Stream: p.meta.Stream}
}

// EstimateCost says it cannot tell, which for text is the truth rather than a
// gap. How long the answer runs is the upstream's decision, and even the size
// of the question is not settled until the upstream reports how it counted the
// prompt. Modalities whose quantities are stated in the request — a number of
// images, a length of text to speak — are the ones this question exists for.
func (p *textPayload) EstimateCost(PricingView) CostEstimate {
	return CostEstimate{Known: false, Unit: fact.UnitToken}
}

// Supports accepts every candidate the kernel offers.
//
// Text has no per-candidate requirement the kernel does not already enforce:
// the protocol is negotiated for whichever provider is chosen, and a provider
// that cannot speak one is served through the intermediate representation
// instead, so there is no candidate a text request cannot be sent to.
//
// This is not the same as saying every candidate can serve it well. A request
// carrying tools sent to a candidate whose provider does not support function
// calling will fail upstream and fail over — which is today's behaviour, and
// changing it is a change to what the gateway does, not to how it is
// structured.
func (p *textPayload) Supports(Candidate) CandidateVerdict {
	return CandidateVerdict{OK: true}
}

// PrepareUpstream builds the body and path for one candidate.
//
// The result is only the modality's part of the request: the kernel adds the
// origin and the credentials, and runs whatever body rewriters are registered
// over these bytes afterwards. Rewriting is a capability that applies to every
// modality, so a modality that ran them itself would be one that could forget.
func (p *textPayload) PrepareUpstream(cand Candidate) (*UpstreamCall, error) {
	egress := codecsFor(cand.EgressProtocol)

	var out []byte
	if cand.Passthrough {
		rewritten, err := passthroughRequestBody(cand.EgressProtocol, p.body, cand.ProviderModelName)
		if err != nil {
			return nil, fmt.Errorf("rewrite model field: %w", err)
		}
		out = rewritten
	} else {
		ir, err := codecsFor(p.ingress).RequestDecoder.DecodeRequest(
			json.RawMessage(p.body), cand.ProviderModelName, p.meta.Stream)
		if err != nil {
			return nil, fmt.Errorf("decode ingress request (%s): %w", p.ingress, err)
		}
		encoded, err := egress.RequestEncoder.EncodeRequest(ir)
		if err != nil {
			return nil, fmt.Errorf("encode egress request (%s): %w", cand.EgressProtocol, err)
		}
		out = []byte(encoded)
	}

	if cand.EgressProtocol == protocols.ProtocolOpenAI {
		injected, err := EnsureStreamUsageInjection(out, p.meta.Stream, p.meta.WantsStreamUsage)
		if err != nil {
			return nil, fmt.Errorf("inject stream usage: %w", err)
		}
		out = injected
	}

	path := egress.RequestEncoder.EgressPath(cand.ProviderModelName, p.meta.Stream)
	if cand.EgressProtocol == protocols.ProtocolGemini && p.meta.Stream {
		path = strings.Replace(path, ":generateContent", ":streamGenerateContent?alt=sse", 1)
	}

	p.cand = cand
	return &UpstreamCall{
		Path:        path,
		Body:        out,
		ContentType: "application/json",
		Progressive: false,
	}, nil
}

// NormalizeUpstreamError decides what the caller is told about an upstream
// failure.
//
// The provider's own wording never reaches the caller. An error body from an
// upstream can name the provider, the model behind the alias, or the account
// it was billed to, none of which the caller is entitled to know.
func (p *textPayload) NormalizeUpstreamError(status int, _ []byte, _ string) ErrorEnvelope {
	class := classifyUpstreamStatus(status)
	errType := class.ErrorType
	if errType == "" {
		errType = errTypeUpstream
	}
	return ErrorEnvelope{
		Status:    status,
		ErrorType: errType,
		Message:   safeUpstreamMessage(status),
	}
}

// FinalizeUsage hands back what the settled delivery reported.
//
// It reads the delivery rather than any running total the payload might keep,
// because the delivery is the one that belongs to the attempt being settled.
// A payload-level accumulator would survive an attempt that failed, and be
// charged under whatever ended the request.
func (p *textPayload) FinalizeUsage(d fact.Delivery) *fact.UsageReported {
	return d.Usage
}

// LogPolicy keeps all four bodies. They are JSON, they are what a protocol
// conversion bug has to be diagnosed from, and this table exists to hold them.
func (p *textPayload) LogPolicy() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{
		BodyClientRequest:    BodyStoredRaw,
		BodyUpstreamRequest:  BodyStoredRaw,
		BodyUpstreamResponse: BodyStoredRaw,
		BodyClientResponse:   BodyStoredRaw,
	}}
}

// SanitizeForLog returns the bytes as they are.
//
// Text is the one modality for which that is safe, and saying so explicitly is
// the point of the method: the modalities that follow carry audio and image
// data that must never be stored as characters, and they cannot rely on the
// logger to know that.
func (p *textPayload) SanitizeForLog(_ BodyKind, _ string, body []byte) string {
	return string(body)
}

// usageReportOf translates what an upstream said it charged into the audit
// vocabulary, and does nothing else.
//
// No coherence check, no cache-convention normalisation: those are settlement
// policy and stay in the kernel, where they apply to every modality at once.
// What a modality knows is the raw counts and what they count — tokens here —
// and stating that is the whole of its job.
func usageReportOf(u *protocols.IRUsage) *fact.UsageReported {
	if u == nil {
		return nil
	}
	return &fact.UsageReported{
		Unit:                  fact.UnitToken,
		Source:                fact.UsageFromUpstream,
		Prompt:                u.PromptTokens,
		Completion:            u.CompletionTokens,
		Total:                 u.TotalTokens,
		CacheRead:             u.CacheReadTokens,
		CacheWrite:            u.CacheWriteTokens,
		CacheIncludedInPrompt: u.CacheIncludedInPrompt,
		Reasoning:             u.ReasoningTokens,
		Incoherent:            u.Invalid,
		WebSearchCount:        u.WebSearchCount,
	}
}

// captureBuffer adapts the delivery's upstream capture to what the relay
// helpers expect. The relay records two byte streams through one object; the
// caller-facing half is dropped here because ClientResponse already records it,
// and only it knows when the bytes actually left.
type captureBuffer struct{ capture UpstreamCapture }

func (b captureBuffer) AppendUpstream(data []byte) { b.capture.Upstream(data) }
func (b captureBuffer) SetBody(data []byte)        { b.capture.Upstream(data) }
func (captureBuffer) SetResponseBody([]byte)       {}
func (captureBuffer) AppendResponse([]byte)        {}

// Deliver gets the upstream's response to the caller.
//
// The split is on how the caller reads it, not on what it contains: a whole
// response can be checked and rewritten before any of it goes out, while a
// stream commits to a status on its first frame and lives with it. Everything
// below follows from that one difference.
func (p *textPayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	if p.meta.Stream {
		return p.deliverStream(tools, resp)
	}
	return p.deliverNonStream(tools, resp)
}

// deliverNonStream gets a whole upstream response to the caller.
func (p *textPayload) deliverNonStream(tools DeliveryTools, resp *http.Response) fact.Delivery {
	if p.cand.Passthrough {
		return p.deliverPassthroughNonStream(tools, resp)
	}
	return p.deliverTranslatedNonStream(tools, resp)
}

// deliverPassthroughNonStream forwards a same-protocol response, rewriting only
// the model name the provider put in it.
func (p *textPayload) deliverPassthroughNonStream(tools DeliveryTools, resp *http.Response) fact.Delivery {
	defer func() { _ = resp.Body.Close() }()

	// One byte past the cap, so an oversized body is refused rather than
	// silently truncated into something that parses.
	body, err := io.ReadAll(io.LimitReader(resp.Body, tools.Limits.MaxResponseBytes+1))
	if err != nil {
		if tools.Client.CallerGone() {
			// The upstream read runs on the caller's context, so their hanging
			// up surfaces here as a read failure. Spending another candidate on
			// a caller who left would be work nobody is waiting for, and
			// blaming the provider for it would be a lie on their record.
			return fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient, "client_disconnected", err)
		}
		return fact.HandedOn(fact.FaultUpstream,
			"read_body: "+err.Error(), err)
	}
	if int64(len(body)) > tools.Limits.MaxResponseBytes {
		return fact.HandedOn(fact.FaultUpstream,
			"response_too_large", nil)
	}

	rewritten, usage, err := passthroughRewriteNonStreamResponse(p.cand.EgressProtocol, body, p.meta.Model)
	if err != nil {
		// Our own rewrite, not the provider's response: blaming the upstream
		// here would put our bug on their record.
		return fact.HandedOn(fact.FaultGateway,
			"response_rewrite_failed: "+err.Error(), err)
	}

	// Recorded before the write: this is what the gateway consumed from the
	// provider, and it is worth having whether or not delivery then succeeds.
	tools.Capture.Upstream(body)
	report := usageReportOf(usage)

	tools.Client.Inject(http.Header{"Content-Type": {"application/json"}})
	if cerr := tools.Client.Commit(resp.StatusCode); cerr != nil {
		// Billed as ours, not as the provider's 2xx. Commit is the step that
		// puts a status on the wire, and it refuses before writing anything, so
		// this response never went out under the upstream's code — filing it
		// under that code would record a request nobody was served as a served
		// one. The streaming side of this same failure already settles on 500.
		//
		// One of the two refusals means the opposite of the other: Commit also
		// refuses a writer something else has already written to, and there the
		// caller does hold a status. Nothing reaches this function after such a
		// write today — a non-stream delivery only runs on a 2xx upstream, and
		// every path that writes early ends the request instead — so the
		// delivery below describes the refusal that is actually reachable, and
		// says nothing about a caller who was already served.
		//
		// Still billed: the provider produced and charged for those tokens
		// whatever happened to them afterwards, which is the same rule the
		// write and flush failures below follow.
		return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled, fact.FaultGateway,
			"commit_failed: "+cerr.Error(), cerr).WithUsage(report)
	}
	if _, werr := tools.Client.Write(rewritten); werr != nil {
		return p.clientWriteFailure(resp.StatusCode, werr, report)
	}
	// A few KB of JSON can sit entirely inside net/http's buffer: Write returns
	// nil without the bytes reaching the socket, and only the flush tells the
	// truth. Without this a caller who disconnected right after a buffered
	// write is recorded as having been served.
	if ferr := tools.Client.Flush(); ferr != nil {
		return p.clientWriteFailure(resp.StatusCode, ferr, report)
	}
	return fact.Succeeded(resp.StatusCode).WithUsage(report)
}

// deliverTranslatedNonStream takes a response in the provider's protocol and
// re-encodes it into the caller's.
func (p *textPayload) deliverTranslatedNonStream(tools DeliveryTools, resp *http.Response) fact.Delivery {
	decoder := codecsFor(p.cand.EgressProtocol).ResponseDecoder
	// Whatever the kernel's registered wrappers do to this encoder, they do it
	// here — this is the conversion path, the only one with an encoder to
	// decorate. Putting the caller's own model name back is one of them.
	encoder := tools.Codecs.WrapResponse(codecsFor(p.ingress).ResponseEncoder)

	usage, err := protocols.IRNonStreamRelay(
		tools.Client, resp, decoder, encoder, captureBuffer{capture: tools.Capture}, nil)
	report := usageReportOf(reportedUsage(usage))

	if err == nil {
		return fact.Succeeded(resp.StatusCode).WithUsage(report)
	}
	if isClientWriteError(err) {
		// Status and headers are already out, so this candidate cannot fail
		// over. The bytes never (fully) landed, so it must not settle as a
		// delivered success either. Usage survives: the upstream was fully
		// consumed whatever happened on the way to the caller.
		return p.clientWriteFailure(resp.StatusCode, err, report)
	}
	if tools.Client.Committed() {
		// The relay's contract is to write nothing when a 2xx body fails to
		// decode, so this is unreachable. It is here to be loud if that ever
		// changes: settling quietly would hide a decode failure that had
		// already put bytes on the wire.
		return fact.Truncated(resp.StatusCode, resp.StatusCode, fact.FaultUpstream,
			"ir_decode_partial: "+err.Error(), err).WithUsage(report)
	}
	return fact.HandedOn(fact.FaultUpstream,
		"ir_decode: "+err.Error(), err).WithUsage(report)
}

// clientWriteFailure is what both non-stream paths report when the caller stops
// receiving after the response was committed.
//
// The status is what they were served; settlement sees 499 regardless, because
// the bytes never landed. Those are two different questions, and one number
// could not answer both.
func (p *textPayload) clientWriteFailure(served int, err error, report *fact.UsageReported) fact.Delivery {
	return fact.Truncated(served, 499, fact.FaultClient, "client_write_timeout", err).WithUsage(report)
}
