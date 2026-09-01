package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/service/safehttp"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// DeliveryTools is what the kernel lends a modality for the duration of one
// delivery.
//
// Everything here is a policy the kernel would rather decide once than trust
// each modality to get right: how a failed write to the caller is told apart
// from a failed read from upstream, when a response becomes irreversible, what
// may be downloaded, how much may be buffered. Handing them over as tools
// rather than as rules means a modality cannot quietly do it differently — the
// only way to write to the caller is through the thing that classifies the
// errors, and the only account of what happened that the kernel trusts is the
// one these tools kept.
type DeliveryTools struct {
	Client  ClientResponse
	Capture UpstreamCapture
	Facts   FactSink
	Limits  TransferLimits
	Fetch   SecondaryFetcher
	// Codecs decorates the encoders this delivery builds when it has to convert
	// a response into the caller's protocol. A modality passes the encoder it
	// constructed through it and uses what comes back; the zero value wraps
	// nothing, so a toolbox built without one is safe to use.
	Codecs ResponseCodecs
	// RequestID identifies this request wherever a modality has to name it to
	// somebody outside. It reaches the caller in the text of an error, so that
	// what they quote when they report a problem is the same string the logs
	// are indexed by. A modality cannot mint one: it is the kernel's name for
	// the request, and one made up here would point at nothing.
	RequestID string
}

// ClientResponse is the only way to the caller: header staging, the commit
// that makes a response irreversible, and the body writes after it.
//
// Staging and committing live in one object because there is only one truth
// about whether a response has gone out, and two objects holding it are two
// answers waiting to disagree. Splitting them was tried: the header half could
// still roll back after the write half had committed, because "committed" was
// being read off a writer that had merely remembered a status.
//
// Write and Flush pre-tag their failures as client-side, so a modality never
// has to tell "the caller hung up" apart from "the upstream stopped sending" —
// a distinction that decides whether a provider gets blamed for an outage that
// was never theirs.
type ClientResponse interface {
	// Inject stages headers, replacing any it has already staged under the
	// same name. Replacing rather than accumulating is deliberate: a modality
	// that stages, changes its mind, and stages again means the second set,
	// and a header list that grew every time would send both.
	//
	// Staged headers are held here, not on the live response, so nothing a
	// modality stages can disturb what the kernel already set.
	Inject(h http.Header)
	// Rollback discards staged headers. It fails once committed, rather than
	// silently doing nothing: a caller that rolls back too late has a bug, and
	// the quiet version of this is how headers describing a success end up on
	// top of an error.
	//
	// The kernel never calls this, or Inject. Injecting headers on the way out
	// looks like something that could be done for everyone, and for most
	// modalities it is — but one of them can only tell success from failure
	// after it has read the first byte of the upstream body, and for that one,
	// headers written optimistically are headers that have to be taken back.
	// Leaving the timing to the modality turns "this one is different, do not
	// add a clear here" from a comment into the visible fact that its code
	// contains no Rollback call.
	Rollback() error
	// Commit sends the staged headers and the status, and cannot be undone.
	// After it the caller has seen a status whatever happens next.
	Commit(status int) error
	// Committed reports whether the status is on the wire.
	Committed() bool
	// CallerDone closes when the request is over from the caller's side, for a
	// modality that has to wait on that alongside something else. It is not the
	// same question as CallerGone: this also closes when OUR OWN deadline
	// expires, which is a timeout we caused rather than a caller who left.
	// Anything deciding blame must ask CallerGone.
	CallerDone() <-chan struct{}
	// CallerGone reports whether the caller has given up on the request.
	//
	// It belongs here because the upstream request runs on the caller's own
	// context: when they hang up, the read from the provider fails too, and the
	// two failures are indistinguishable from the error alone. They must not be
	// recorded the same way — one is a caller who left, the other is a provider
	// that broke, and only the second belongs on that provider's record.
	CallerGone() bool
	// CommittedStatus is the status the caller was served, or 0 if none was.
	//
	// This is the kernel's own record of what went out, and it exists to be
	// checked against what a modality afterwards claims happened. A delivery
	// is a report, and a report can be wrong; this is the fact.
	CommittedStatus() int
	// Write sends body bytes. It refuses before Commit rather than letting the
	// framework commit a 200 nobody chose. A failed write wraps
	// protocols.ErrClientWrite; the refusal does not, because it is a bug here
	// rather than a caller who left.
	Write(p []byte) (int, error)
	// WatchCaller closes upstream when the caller goes away, and returns the
	// function that disarms it.
	//
	// It is here rather than left to a modality because noticing takes more
	// than watching CallerDone: under HTTP/1.x the framework only begins
	// propagating a client FIN once the connection has been asked about, and
	// asking is something only this side of it can do. A modality that tried
	// would get a channel that never closes and an upstream that keeps
	// generating for a caller who left.
	//
	// Which is why a delivery that spends itself inside one long read has to
	// arm this — and why one that never arms it must not lean on CallerDone or
	// CallerGone at all. Without the watch, the framework never begins
	// propagating the FIN, the channel never closes, and CallerGone stays
	// false for a caller who is long gone: the only signal such a delivery
	// ever gets is its own write failing. The pass-through stream pumps run in
	// exactly that mode today, which is why their disconnect handling keys off
	// write errors rather than off these accessors.
	WatchCaller(upstream io.Closer) (stop func())
	// Flush pushes buffered bytes onto the socket and reports the failure a
	// buffered Write can hide. A failed write to the caller wraps
	// protocols.ErrClientWrite so blame lands on the right side; being called
	// before Commit is a programming error instead, and says so plainly.
	//
	// It is also where written bytes become part of the audit trail: a
	// buffered Write returns nil without the caller having received anything,
	// so anything recorded earlier would be evidence for a delivery that never
	// happened.
	Flush() error
}

// UpstreamCapture keeps the provider's bytes for the audit trail.
//
// Only the provider's. What the caller received is recorded by ClientResponse,
// which is the only thing that knows when bytes actually left.
type UpstreamCapture interface {
	// Upstream records bytes as they arrived from the provider. It stops at
	// the delivery's response limit rather than growing without bound, and
	// reports whether everything it was given was kept.
	Upstream(p []byte) (kept bool)
}

// FactSink is where a modality reports what it observed. It is the same sink
// the capabilities report through, so a modality's observations land in the
// audit trail in the same order and with the same provenance as everything
// else.
type FactSink interface {
	Report(facts ...fact.Fact)
	Note(records ...fact.Record)
}

// TransferLimits are the resolved size and time budgets for this delivery.
//
// They are values rather than constants because they are properties of the
// payload, not of the gateway: a frame of base64 image data is orders of
// magnitude larger than a line of chat, and a long render legitimately runs
// for a length of time that would mean a hung connection on a text request. A
// modality declares what it needs, the kernel resolves the two against each
// other, and the modality then reads one number instead of carrying its own
// literal.
type TransferLimits struct {
	// MaxResponseBytes caps a whole non-progressive response body.
	MaxResponseBytes int64
	// MaxFrameBytes caps one frame of a progressive response.
	MaxFrameBytes int
	// WriteWindow is how long a single write to the caller may take before the
	// caller is treated as gone. ClientResponse applies it to every write.
	WriteWindow time.Duration
	// TotalBudget is how long the whole delivery may run. A separate dimension
	// from WriteWindow on purpose: a render that legitimately takes ten minutes
	// still must not stall for thirty seconds on any one write.
	//
	// Enforced at admission, where the request deadline is narrowed to it —
	// every downstream deadline (per-attempt budgets, the request context)
	// derives from that one, so the declaration caps all of them at once. A
	// modality can only narrow the kernel's own budget, never outlive it.
	TotalBudget time.Duration
}

// resolveAgainst narrows kernel limits by what a modality asked for.
//
// A modality can only ask for less. A zero field on its side means it has no
// opinion and keeps the kernel's value. Narrowing rather than replacing is what
// stops a modality from voting itself an unbounded buffer, which is what this
// would be if the declaration were simply trusted.
//
// A zero on the KERNEL's side means no limit, so anything finite the modality
// asks for is narrower and wins. Reading zero as "smallest" instead would have
// an unset kernel budget silently override every modality that asked for one.
func (l TransferLimits) resolveAgainst(want TransferLimits) TransferLimits {
	out := l
	if want.MaxResponseBytes > 0 && (out.MaxResponseBytes <= 0 || want.MaxResponseBytes < out.MaxResponseBytes) {
		out.MaxResponseBytes = want.MaxResponseBytes
	}
	if want.MaxFrameBytes > 0 && (out.MaxFrameBytes <= 0 || want.MaxFrameBytes < out.MaxFrameBytes) {
		out.MaxFrameBytes = want.MaxFrameBytes
	}
	if want.WriteWindow > 0 && (out.WriteWindow <= 0 || want.WriteWindow < out.WriteWindow) {
		out.WriteWindow = want.WriteWindow
	}
	if want.TotalBudget > 0 && (out.TotalBudget <= 0 || want.TotalBudget < out.TotalBudget) {
		out.TotalBudget = want.TotalBudget
	}
	return out
}

// withPositiveBuffers guarantees the byte caps are numbers a buffer can use.
//
// Zero means "no limit" to resolveAgainst and "keep nothing" to every buffer
// that reads these, and one struct cannot carry both readings. The kernel's own
// caps are constants today so a zero cannot currently get this far, which is
// exactly why the rule is written down rather than relied upon: the resolver is
// free to grow a path that produces one.
func (l TransferLimits) withPositiveBuffers() TransferLimits {
	if l.MaxResponseBytes <= 0 {
		l.MaxResponseBytes = maxNonStreamResponseBytes
	}
	if l.MaxFrameBytes <= 0 {
		l.MaxFrameBytes = maxStreamLineBytes
	}
	return l
}

// SecondaryFetcher downloads a response a provider referred to rather than
// returned.
//
// Some upstreams answer with a URL instead of the bytes, which turns the
// gateway into something that fetches an address chosen by a third party — the
// shape of a server-side request forgery, if the fetching is done casually.
// Doing it once here means the restrictions apply to every modality that needs
// it rather than to whichever one remembered.
type SecondaryFetcher interface {
	Fetch(ctx context.Context, rawURL string, p FetchPolicy) ([]byte, error)
}

// FetchPolicy is what a modality is allowed to say about a secondary fetch.
// Notably it cannot turn any of the restrictions off — it can only narrow
// them.
type FetchPolicy struct {
	// AllowedHosts is required. Entries are hosts, optionally with a port; an
	// entry naming no port matches only the scheme's default, and one naming a
	// port matches only that port. An empty list fetches nothing: a policy that
	// forgot to name its hosts must not be the one that permits everything.
	AllowedHosts []string
	// MaxBytes narrows the download limit. Zero, or anything above the
	// delivery's own response limit, leaves the kernel's cap in force.
	MaxBytes int64
}

// ─────────────────────────── kernel implementation ───────────────────────────

// ginClientResponse is the ClientResponse over a live gin response.
type ginClientResponse struct {
	c  *gin.Context
	rc *Exchange
	// staged holds what the modality injected. Keeping it here rather than on
	// the live header is what lets a rollback discard exactly what was staged:
	// writing straight to c.Writer.Header() would overwrite whatever the
	// kernel had already set under the same name, and no rollback can restore
	// a value nobody kept.
	staged http.Header
	// window is the per-write deadline, slid before each write so a caller
	// that stops reading cannot hold the handler open.
	window time.Duration
	// status is what was committed, kept because the caller of Deliver needs
	// something to check a modality's account against.
	status int
	// pending holds bytes written but not yet flushed, and sent holds what the
	// caller has actually received. Splitting them is the point: net/http can
	// take a Write into its own buffer and return nil, so only a flush proves
	// delivery.
	pending []byte
	sent    []byte
	limit   int64
	// progressive selects where flushed bytes are recorded, the same question
	// newCapture answers for the upstream's. It is the kernel's because it is
	// about storage: a modality that chose would be one that could put a stream
	// somewhere sized for a single response.
	progressive bool
}

func (r *ginClientResponse) Inject(h http.Header) {
	if r.staged == nil {
		r.staged = http.Header{}
	}
	for name, values := range h {
		r.staged.Del(name)
		for _, v := range values {
			r.staged.Add(name, v)
		}
	}
}

func (r *ginClientResponse) Rollback() error {
	if r.Committed() {
		return errors.New("gateway: cannot roll back headers after the response is committed")
	}
	r.staged = nil
	return nil
}

func (r *ginClientResponse) Commit(status int) error {
	if status <= 0 {
		return fmt.Errorf("gateway: cannot commit a response with status %d", status)
	}
	if r.Committed() {
		// Carries the sentinel because the relays wrap whatever Commit returns
		// in their own client-write error, and a refusal that arrived wearing
		// that would be read as a caller who stopped reading — putting a
		// double-commit of ours on their record.
		return fmt.Errorf("%w: %w", errClientCommitRefused, errAlreadyCommitted)
	}
	for name, values := range r.staged {
		r.c.Writer.Header().Del(name)
		for _, v := range values {
			r.c.Writer.Header().Add(name, v)
		}
	}
	r.staged = nil
	r.slideDeadline()
	r.c.Writer.WriteHeader(status)
	// WriteHeader alone only remembers the status: gin holds it until the first
	// body write, and until then Written() stays false and a second WriteHeader
	// silently replaces it. WriteHeaderNow is what puts it on the wire, which
	// is the entire meaning of committing.
	r.c.Writer.WriteHeaderNow()
	r.status = status
	// A committed response has reached the caller, which is what this flag
	// means and what the audit record reports as "delivered".
	r.rc.markFirstByteSent()
	return nil
}

func (r *ginClientResponse) Committed() bool { return r.c.Writer.Written() }

func (r *ginClientResponse) CallerGone() bool { return isClientDisconnected(r.c) }

func (r *ginClientResponse) CallerDone() <-chan struct{} { return r.c.Request.Context().Done() }

func (r *ginClientResponse) WatchCaller(upstream io.Closer) (stop func()) {
	return protocols.WatchClientClose(r.c, upstream)
}

func (r *ginClientResponse) CommittedStatus() int {
	if !r.Committed() {
		return 0
	}
	if r.status != 0 {
		return r.status
	}
	// Committed without going through Commit. That is what every write outside
	// this type looks like — the error writers, and all of the dispatch code
	// that has not moved behind this interface yet — and answering 0 there
	// would report "nothing was served" for a caller holding a response. The
	// writer's own status is the wire's account, so it is the one to give.
	return r.c.Writer.Status()
}

func (r *ginClientResponse) Write(p []byte) (int, error) {
	if !r.Committed() {
		// Writing first would have the framework commit a 200 that nothing
		// chose, at a moment nothing recorded.
		return 0, errors.New("gateway: write before the response was committed")
	}
	r.slideDeadline()
	n, err := r.c.Writer.Write(p)
	if n > 0 {
		r.pending = append(r.pending, p[:n]...)
	}
	if err != nil {
		return n, fmt.Errorf("%w: write to client: %w", protocols.ErrClientWrite, err)
	}
	return n, nil
}

func (r *ginClientResponse) Flush() error {
	if !r.Committed() {
		return errors.New("gateway: flush before the response was committed")
	}
	r.slideDeadline()
	if err := flushAndCheckError(r.c); err != nil {
		return fmt.Errorf("%w: flush to client: %w", protocols.ErrClientWrite, err)
	}
	// The bytes are gone now, so this is the first moment they can honestly be
	// recorded as received.
	if r.progressive {
		// Same reasoning as the upstream half, on the caller's side of it: a
		// stream has no size the in-memory record was built for. Kept there it
		// would hit the non-stream cap, flag every stream as truncated, and
		// leave the file that exists for this empty.
		appendStreamBodyLine(r.rc, r.pending)
		r.pending = nil
		return nil
	}
	room := r.limit - int64(len(r.sent))
	if room > 0 {
		switch {
		case int64(len(r.pending)) > room:
			r.sent = append(r.sent, r.pending[:room]...)
		case len(r.sent) == 0:
			// First flush: take ownership instead of copying. pending is set
			// to nil below, so nothing writes into these bytes afterwards —
			// and for a non-streaming response this is the flush, of the whole
			// body, so the copy this avoids is a second full-body allocation.
			r.sent = r.pending
		default:
			r.sent = append(r.sent, r.pending...)
		}
		// Assigned, not appended to whatever was there: this is what THIS
		// attempt delivered, and an earlier attempt's bytes are a different
		// response that must not be concatenated onto it.
		r.rc.bodies.SetResponse(r.sent)
	}
	r.pending = nil
	return nil
}

// slideDeadline pushes the per-write deadline out before a write, using this
// delivery's own resolved window rather than a package-wide one — otherwise a
// modality that asked for a short window would read its own number back out of
// the limits and still be held open for the global one.
func (r *ginClientResponse) slideDeadline() {
	if r.window <= 0 {
		return
	}
	if err := http.NewResponseController(r.c.Writer).SetWriteDeadline(time.Now().Add(r.window)); err != nil {
		warnWriteDeadlineUnsupported(r.c, r.rc.requestID, err)
	}
}

// exchangeCapture records one attempt's upstream bytes, up to a cap.
type exchangeCapture struct {
	rc    *Exchange
	limit int64
	// buf is this attempt's own bytes. The exchange field it publishes to is
	// shared by every attempt, so measuring room against that field would let
	// an earlier candidate's error body eat this one's budget, and appending to
	// it would leave two providers' responses concatenated in the column an
	// operator reads to find out what one of them said.
	buf []byte
	// published is how much of buf the exchange last saw. Anything else means
	// somebody outside wrote or cleared the field, and this capture has to
	// defer rather than republish over them.
	published int
}

// newCapture picks where this attempt's upstream bytes are kept, and opens the
// capture file when the delivery will need one.
//
// A whole response is bounded and fits in memory. A stream's upstream bytes are
// kept nowhere at all — the file is for what the CALLER received, and the raw
// pre-rewrite lines are a different account of the same response that would
// leave neither readable. The file is still opened here because the caller's
// half needs it, and choosing storage is the kernel's job either way.
//
// The streaming pumps also open it themselves, and will until they are handed
// these tools; opening is idempotent within an attempt so the two callers
// cannot cost a descriptor between them. Closing belongs to the release this
// toolbox came with.
func (s *Service) newCapture(c *gin.Context, rc *Exchange, limit int64, progressive bool) UpstreamCapture {
	if progressive {
		openStreamBodyFile(c, rc)
		return droppedCapture{}
	}
	return newExchangeCapture(rc, limit)
}

// newExchangeCapture starts an attempt's capture and clears what the last one
// left.
//
// Clearing here rather than on the first write is the difference between an
// audit row that says nothing and one that says something false: an attempt
// that never produces a body — because it failed before the response, or the
// response was empty — would otherwise leave the previous provider's bytes
// standing under this attempt's heading.
func newExchangeCapture(rc *Exchange, limit int64) *exchangeCapture {
	rc.bodies.BeginDeliveryCapture()
	return &exchangeCapture{rc: rc, limit: limit}
}

func (e *exchangeCapture) Upstream(p []byte) bool {
	if len(e.rc.bodies.UpstreamResponse()) != e.published {
		// Somebody else wrote or cleared the field since the last publish.
		// Deliberate clears exist on the dispatch paths, and republishing the
		// whole accumulated buffer over one would resurrect exactly the bytes
		// it had just dropped.
		e.buf = nil
		e.published = 0
	}
	room := e.limit - int64(len(e.buf))
	if room <= 0 {
		return false
	}
	kept := true
	if int64(len(p)) > room {
		p = p[:room]
		kept = false
	}
	// Copied, not aliased, and that is a contract rather than a habit: the
	// streaming callers hand this a slice into a read buffer they reuse on the
	// next chunk, so keeping a reference would let later reads rewrite what
	// this capture already recorded. For a large non-streaming body the copy
	// is a real cost — accepted, because an audit that can be silently edited
	// after the fact is not an audit.
	e.buf = append(e.buf, p...)
	// Replaces rather than extends: last attempt wins, matching every other
	// writer of this field.
	e.rc.bodies.SetUpstreamResponse(e.buf)
	e.published = len(e.buf)
	return kept
}

// droppedCapture is the upstream capture a progressive delivery gets: one that
// keeps nothing.
//
// The exchange's capture file promises exactly what the caller received, and a
// stream's raw upstream lines are not that — they are the pre-rewrite version
// of the same response, so putting both in one file leaves neither account
// readable. Keeping them in memory is out for the reason streams get a file in
// the first place. So they are dropped, and the caller-facing half is recorded
// where it can be recorded honestly: by the response object, which is the only
// thing that knows when bytes actually left.
//
// Truncated is false because nothing was cut short: nothing was kept at all,
// and saying "truncated" would describe a partial record where there is none.
type droppedCapture struct{}

func (droppedCapture) Upstream([]byte) bool { return true }

// safeFetcher is the kernel's SecondaryFetcher.
type safeFetcher struct {
	// client must carry a transport of its own. A download whose destination a
	// third party chose is exactly the one that must not fall back to
	// net/http's default: that one resolves any address, including this
	// network's own, and honours the proxy environment the gateway's transport
	// deliberately ignores. A nil client, or one with no transport, therefore
	// fetches nothing rather than fetching unprotected.
	client *http.Client
	limit  int64
}

func (f *safeFetcher) Fetch(ctx context.Context, rawURL string, p FetchPolicy) ([]byte, error) {
	if f.client == nil || f.client.Transport == nil {
		return nil, errors.New("secondary fetch: no safe transport available")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		// The URL never appears in an error returned from here. It came from a
		// third party, it ends up in logs and in error bodies, and it is the
		// kind of thing that carries a signature or a token in its query.
		return nil, errors.New("secondary fetch: malformed url")
	}
	if u.Scheme != "https" {
		return nil, errors.New("secondary fetch: scheme is not https")
	}
	if !hostAllowed(u, p.AllowedHosts) {
		return nil, errors.New("secondary fetch: host is not allowed")
	}
	limit := f.limit
	if p.MaxBytes > 0 && p.MaxBytes < limit {
		limit = p.MaxBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("secondary fetch: cannot build request")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, errors.New("secondary fetch: request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A redirect arrives here too: the client is told not to follow one, so
		// a 3xx comes back as a response rather than as a second request. Not
		// following is the point — an allowed host that redirects onward would
		// hand the request to a denied one in a single hop.
		return nil, fmt.Errorf("secondary fetch: upstream returned %d", resp.StatusCode)
	}

	// One byte past the limit, so an oversized body is detected rather than
	// silently truncated into something that looks complete.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, errors.New("secondary fetch: read failed")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("secondary fetch: body exceeds the size limit")
	}
	return body, nil
}

// hostAllowed matches a URL's host against the policy's list.
//
// The port is part of the match — naming a host is not consent to every
// service running on it — but a URL on the scheme's default port and an entry
// that spells that port out are the same destination, so both spellings are
// accepted. Without that, an allowlist written the explicit way would deny
// every ordinary https URL while looking correct. An empty list matches
// nothing.
func hostAllowed(u *url.URL, allowed []string) bool {
	host := strings.ToLower(u.Host)
	withPort := host
	if u.Port() == "" {
		withPort = host + ":443"
	}
	bare := strings.TrimSuffix(host, ":443")
	for _, a := range allowed {
		a = strings.ToLower(a)
		if a == host || a == withPort || a == bare {
			return true
		}
	}
	return false
}

// secondaryFetchTimeout bounds a whole secondary download. The context usually
// carries the request budget, but a fetch is an extra round trip on top of a
// request that has already spent most of it, and inheriting only the caller's
// deadline lets a slow host consume whatever is left.
const secondaryFetchTimeout = 30 * time.Second

// resolveLimits narrows the kernel's own budgets by what a modality asked for.
func (s *Service) resolveLimits(want TransferLimits) TransferLimits {
	return TransferLimits{
		MaxResponseBytes: maxNonStreamResponseBytes,
		MaxFrameBytes:    maxStreamLineBytes,
		WriteWindow:      protocols.StreamWriteWindow(),
		TotalBudget:      s.gateway.RequestTimeout,
	}.resolveAgainst(want).withPositiveBuffers()
}

// newDeliveryTools assembles the toolbox for one delivery and returns the
// function that takes it back.
//
// The release comes back with the toolbox rather than being left for the caller
// to remember, because this opens a capture file for a progressive delivery.
// A descriptor whose close lives somewhere the compiler cannot see is correct
// only for as long as every caller remembers it.
//
// Prior attempts' bodies are dropped here too. The fields say what THIS attempt
// received, and a delivery that fails before writing them would otherwise leave
// the last candidate's error body standing as this one's answer.
func (s *Service) newDeliveryTools(c *gin.Context, rc *Exchange, want TransferLimits, progressive bool) (DeliveryTools, func()) {
	limits := s.resolveLimits(want)
	rc.clearResponseBodies()

	return DeliveryTools{
		Client: &ginClientResponse{
			c: c, rc: rc, window: limits.WriteWindow, limit: limits.MaxResponseBytes,
			progressive: progressive,
		},
		Capture:   s.newCapture(c, rc, limits.MaxResponseBytes, progressive),
		Facts:     newExchangeSink(rc),
		Limits:    limits,
		Fetch:     &safeFetcher{client: s.secondaryFetchClient(), limit: limits.MaxResponseBytes},
		Codecs:    ResponseCodecs{wrappers: s.responseCodecWrappers, exchange: rc},
		RequestID: rc.requestID,
	}, func() { s.releaseDelivery(c, rc) }
}

// releaseDelivery closes out the capture a delivery was given.
//
// A stream that never sent a byte leaves a file with nothing in it, and left
// there it shows up as a capture worth opening. Removing it before the close is
// deliberate — removal also closes and forgets the handle, so the close that
// follows finds nothing left to do rather than racing it.
func (s *Service) releaseDelivery(_ *gin.Context, rc *Exchange) {
	rc.bodies.DiscardEmptyStream()
	rc.bodies.CloseStream()
}

// secondaryFetchClient returns the shared client secondary fetches run on.
//
// One per Service, not one per delivery. A fresh Transport each time never
// reuses a connection and never has its idle ones closed, so every request
// that fetched would leave its own pooled connections and their goroutines
// behind — a leak that grows with traffic and buys nothing, since the
// isolation that matters comes from the transport's configuration, not from
// its being new.
//
// It is deliberately NOT the upstream transport. That one may be configured to
// permit private addresses, because an operator is allowed to point this
// gateway at a provider on their own network — but that permission was granted
// for destinations an operator configured, and a secondary URL is chosen by
// whoever answered the request.
func (s *Service) secondaryFetchClient() *http.Client {
	s.secondaryFetchOnce.Do(func() {
		s.secondaryFetch = &http.Client{
			Transport: safehttp.NewTransport(false, s.gateway.ConnectTimeout, s.gateway.TLSHandshakeTimeout),
			Timeout:   secondaryFetchTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	})
	return s.secondaryFetch
}

// streamWriteDeadlineWarnedKey gates the sliding-deadline warning to once per
// request: the deadline is slid before every forwarded chunk, so on a writer
// without SetWriteDeadline support the error would otherwise spam the log.
const streamWriteDeadlineWarnedKey = "gateway_stream_write_deadline_warned"

// warnWriteDeadlineUnsupported reports a writer that cannot take a write
// deadline, at most once per request.
//
// Production's *http.response always supports it, so a warning means some
// wrapper has disabled the slow-client protection — worth knowing, but not
// worth one line per chunk.
func warnWriteDeadlineUnsupported(c *gin.Context, requestID string, err error) {
	if c.GetBool(streamWriteDeadlineWarnedKey) {
		return
	}
	c.Set(streamWriteDeadlineWarnedKey, true)
	logger.Warn("gateway: apply write deadline failed",
		zap.String("request_id", requestID), zap.Error(err))
}
