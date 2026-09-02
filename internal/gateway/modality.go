package gateway

import (
	"context"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// A modality is a kind of thing a caller can ask for: a chat completion, an
// image, spoken audio. They differ in almost every way a request can differ —
// how the body is encoded, whether the response has frames, what is being
// counted when the bill is worked out, whether the bytes can go in a log at
// all — and until now each of them carried its own copy of the parts they do
// NOT differ in: settling, billing, recording, deciding what the caller is
// told when an upstream fails.
//
// The split here is between what varies and what does not. A modality answers
// questions about its own kind of payload; the kernel does everything that is
// the same for all of them. The point is not less code — the modalities are
// large and stay large — it is that a change to a cross-cutting concern has
// one place to land instead of one per entry point.

// ModalityID names a modality, for registration and for the audit trail.
type ModalityID string

// ModalityText is the chat/completions family: JSON in, JSON or SSE out,
// billed in tokens.
const ModalityText ModalityID = "text"

// Modality is the stateless half: one per kind of payload, registered against
// the ingress protocols it serves, shared by every request.
type Modality interface {
	ID() ModalityID
	// Limits is what this kind of payload needs of the transfer budgets. The
	// kernel narrows its own limits by these and never widens them, so a
	// modality states what it needs rather than what it may have.
	Limits() TransferLimits
	// Admit turns a caller's request into the per-request half, or refuses it.
	//
	// A refusal here is one that no candidate could have changed — a body that
	// does not parse, a field the protocol requires and the caller omitted.
	// Anything that depends on which upstream is chosen belongs in Supports.
	Admit(ctx context.Context, in Ingress) (Payload, *Rejection)
}

// Ingress is the caller's request as a modality receives it: raw material, not
// a transport handle.
//
// The kernel keeps the connection to itself. A modality holding one could
// answer the caller behind the kernel's back, which is the arrangement this
// seam exists to end — and it is how the entry points that bypassed the shared
// pipeline came to each have their own idea of what a response is.
type Ingress struct {
	Protocol protocols.ProtocolID
	// Path is the raw request path. Some protocols carry routing information
	// there that is not in the body, so the resolved protocol is not enough.
	Path string
	// ContentType is the caller's, verbatim. Multipart bodies cannot be read
	// without the boundary it carries.
	ContentType string
	Body        []byte
	// APIKeyID owns the request. A payload whose effects outlive the
	// request — a task a caller will poll for later — needs the owner at
	// admission; request-scoped modalities ignore it.
	APIKeyID uint
}

// Rejection is a refusal decided before any upstream was involved.
//
// It carries both what the caller is told and what is recorded, because those
// are different strings with different audiences: Message is written into the
// error body, FailReason is the stable code a dashboard groups by.
type Rejection struct {
	Status     int
	ErrorType  string
	Message    string
	FailReason string
	Fault      fact.Fault
}

// Payload is the stateful half: one per request, and the only object that
// knows what kind of payload this request carries.
//
// Everything a modality has to remember across attempts is an ordinary field
// on the implementation — a re-encoded multipart body worth keeping so the
// next attempt need not build it again, the last upstream error envelope, the
// last model that turned out not to support streaming. Those are local
// variables in a handler today, which is exactly why they cannot survive the
// attempt that produced them.
//
// Call order is fixed and asserted by the kernel:
//
//	Routing → EstimateCost → { Supports → PrepareUpstream → Deliver }* →
//	FinalizeUsage → LogPolicy/SanitizeForLog
//
// Every method runs on the SAME goroutine, serially. That is a real constraint
// and not just an implementation note: Supports is allowed to memoize the
// choice it made for a candidate and have PrepareUpstream read it back, so the
// two are coupled through the Payload rather than through their arguments.
type Payload interface {
	// Routing is what the kernel needs to pick candidates.
	Routing() RoutingIntent
	// EstimateCost is what this request might cost before it runs. A modality
	// that cannot say returns an estimate that says so.
	EstimateCost(p PricingView) CostEstimate
	// Supports answers whether this candidate can serve this payload at all.
	Supports(cand Candidate) CandidateVerdict
	// PrepareUpstream builds the request to send to one candidate.
	PrepareUpstream(cand Candidate) (*UpstreamCall, error)
	// Deliver gets the upstream's response to the caller and reports what
	// happened. It settles nothing: what the request costs and how it is
	// recorded are the kernel's, decided from the Delivery this returns.
	Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery
	// NormalizeUpstreamError turns an upstream failure into what the caller is
	// told about it.
	//
	// NOTE: the kernel's error paths do not call this yet — upstream failures
	// are still shaped by the kernel's own classification. The call site lands
	// when a modality whose error bodies are not JSON needs it.
	NormalizeUpstreamError(status int, body []byte, upstreamContentType string) ErrorEnvelope
	// FinalizeUsage states the billable quantities for the delivery that ended
	// the request, in whatever unit this modality counts. Returns nil when
	// there is nothing to bill.
	FinalizeUsage(d fact.Delivery) *fact.UsageReported
	// LogPolicy says what of this request may be persisted.
	LogPolicy() LogPolicy
	// SanitizeForLog renders one body for the audit trail. A modality whose
	// bodies are not text is what this exists for: the caller of a log must
	// never assume the bytes can be stored, or read, as they arrived.
	SanitizeForLog(k BodyKind, contentType string, body []byte) string
}

// RoutingIntent is what a request asks for, independent of who serves it.
type RoutingIntent struct {
	// Model is the name the caller used, before any candidate rewrites it.
	Model string
	// Stream is whether the caller asked to be served incrementally. It is
	// separate from whether the upstream response has frames: a modality can
	// be obliged to stream from upstream regardless (see UpstreamCall).
	Stream bool
}

// PricingView is the priced shape of the model this request resolved to, as
// much of it as an estimate can be made from before a candidate is chosen.
type PricingView struct {
	InputPricePerMillion  float64
	OutputPricePerMillion float64
	RequestPrice          float64
}

// CostEstimate is what a request might cost before it runs.
//
// Known is false for a modality that cannot say, and text cannot: how long the
// answer runs is the upstream's decision, and even the size of the question is
// not settled until the upstream reports how it counted. Modalities whose
// quantities are stated in the request up front — a number of images, a length
// of text to speak — can answer, which is why the question is asked at all.
type CostEstimate struct {
	Known  bool
	Micros int64
	Unit   fact.Unit
}

// Candidate is one routing option, as much of it as a modality is allowed to
// see. Credentials are deliberately absent: choosing a candidate and holding
// the key that talks to it are separate concerns, and only the kernel needs
// both.
type Candidate struct {
	// ProviderModelName is what this provider calls the model, which is what
	// has to appear in the outgoing body.
	ProviderModelName string
	// EgressProtocol is the wire protocol the kernel negotiated for this
	// candidate, and Passthrough says whether it matched the ingress protocol
	// — the difference between rewriting one field and a full re-encode.
	EgressProtocol protocols.ProtocolID
	Passthrough    bool
	BaseURL        string

	SupportsStreaming       bool
	SupportsFunctionCalling bool
	MaxOutput               int

	// Identity fields: which model, candidate, and provider destination
	// this option is. Deliberately after the request-scoped knobs: a
	// payload whose effects outlive the request — a task a caller polls
	// later — snapshots these; request-scoped modalities never read them.
	ModelID            uint
	CandidateID        uint
	ProviderID         uint
	DestinationVersion int
}

// CandidateVerdict is a modality's answer to whether one candidate can serve
// this payload.
//
// Reason is required when OK is false and becomes the attempt record's note:
// a candidate skipped without a reason is indistinguishable, afterwards, from
// one that was never considered.
type CandidateVerdict struct {
	OK     bool
	Reason string
}

// UpstreamCall is one request to one candidate, built by the modality and sent
// by the kernel.
type UpstreamCall struct {
	// Path is appended to the candidate's base URL by the kernel, and may
	// carry a query string. A modality never states a whole URL.
	//
	// The kernel attaches the provider's credentials to this request, so a
	// destination the modality chose freely is a destination those credentials
	// could be sent to — one mistake in joining a path would be enough to post
	// somebody's API key to another host. Keeping the origin on the kernel's
	// side of the interface means the worst a modality can get wrong is the
	// path within a provider it was already talking to.
	Path string
	Body []byte
	// ContentType is required, not defaulted.
	//
	// A body that is re-encoded rather than edited gets a new boundary, and a
	// content type that is filled in from somewhere else is one that can
	// disagree with the bytes. Making it a field the builder must supply
	// removes the step that could be forgotten; the kernel derives the length
	// from the body itself for the same reason.
	//
	// When stated, it is authoritative over the egress codec's own header:
	// the codec's value is right for the JSON protocols and wrong for a body
	// that is not JSON. The kernel applies it after the codec has set every
	// transport and credential header, so the codec keeps what is its own.
	ContentType string
	// Progressive is the payload's declaration that this response must be
	// forwarded as it arrives — some responses cannot be buffered whole in any
	// sensible amount of memory.
	//
	// The kernel honours it alongside (not instead of) the caller's stream
	// ask: delivery tooling is built for either, because they are different
	// questions — a response that cannot be buffered must be forwarded
	// incrementally on a request nobody marked as streaming.
	Progressive bool
	// OriginRelative says Path is relative to the provider's ORIGIN
	// (scheme://host) rather than to its configured base path. A provider
	// whose API lives at several path branches of one host — an
	// OpenAI-compatible chat route and a native image route on the same
	// domain — serves the second from a path the base's version segment
	// would otherwise corrupt. The origin is still the kernel's to take from
	// the provider's own base URL: the payload names a path within the
	// provider it was already talking to, same as ever.
	OriginRelative bool
	// Headers are extra request headers the dialect this payload speaks
	// requires beyond transport and credentials — a task-mode switch an
	// upstream gates its endpoint on, not a value the egress codec could
	// know. Applied after the codec's own headers and the content type,
	// with the same last-word authority: the payload knows what its body
	// is asking for.
	Headers map[string]string
}

// ErrorEnvelope is an upstream failure translated into what the caller is
// told. The upstream's own status and wording do not reach the caller
// unexamined: a provider's error body can name the provider, the model, or the
// account it was billed to.
type ErrorEnvelope struct {
	Status    int
	ErrorType string
	Message   string
}

// BodyKind names which body is being handed to SanitizeForLog. The four are
// genuinely different: what is safe to keep of a caller's request is not what
// is useful to keep of an upstream's error.
type BodyKind uint8

const (
	BodyClientRequest BodyKind = iota
	BodyUpstreamRequest
	BodyUpstreamResponse
	BodyClientResponse
)

// String names a body kind for logs and audit rows. The four spellings are
// stable identifiers a dashboard can rely on, not prose: they name the same
// bodies the log policy's Store map is keyed by.
func (k BodyKind) String() string {
	switch k {
	case BodyClientRequest:
		return "client_request"
	case BodyUpstreamRequest:
		return "upstream_request"
	case BodyUpstreamResponse:
		return "upstream_response"
	case BodyClientResponse:
		return "client_response"
	default:
		return "unknown_body_kind"
	}
}

// LogPolicy says what of a request may be persisted, decided by the modality
// because only it knows what its bytes are.
//
// The default — the zero value — keeps nothing. A modality that wants its
// bodies stored has to say so, which is the safe direction to be wrong in: a
// new modality that forgets this logs too little, rather than writing megabytes
// of binary audio into a text column.
//
// The kernel reads the policy once at admission and enforces it over the
// captured bodies after settlement, before anything records them: dropped
// bodies never reach storage, raw bodies are kept as the bytes that arrived,
// rendered bodies are kept as SanitizeForLog writes them, and MaxBytes caps
// either form. A request the modality refused at Admit never got a policy and
// keeps the kernel's own account of its bodies.
type LogPolicy struct {
	// Store says, for each body, whether it may be persisted and in which
	// form. Anything absent (or BodyDropped) never reaches storage.
	Store map[BodyKind]BodyStorage
	// MaxBytes caps each stored body. Zero leaves the cap to whoever stores it.
	MaxBytes int64
}

// Storage reports how this policy may persist one body kind.
func (p LogPolicy) Storage(k BodyKind) BodyStorage { return p.Store[k] }

// BodyStorage is the form in which one admitted body may be persisted.
type BodyStorage uint8

const (
	// BodyDropped keeps nothing. The zero value, so a policy that says
	// nothing about a body keeps none of it.
	BodyDropped BodyStorage = iota
	// BodyStoredRaw keeps the bytes exactly as they arrived. The kernel
	// holds on to the slice it was given — no copy, no rendering — which is
	// the form a modality whose bodies are already text asks for: on a
	// twenty-megabyte request body the copy is a real cost, and there is
	// nothing to render.
	BodyStoredRaw
	// BodyStoredRendered keeps what SanitizeForLog makes of the bytes. This
	// is the form for bodies that must not be stored as they arrived —
	// binary payloads, or text with regions the modality redacts. Rendering
	// copies; that is the price of the form, and a modality that does not
	// need it says raw instead.
	BodyStoredRendered
)
