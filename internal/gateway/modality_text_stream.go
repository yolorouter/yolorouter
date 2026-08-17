package gateway

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/chat"
)

// deliverStream sends a response the caller reads as it arrives.
//
// The two shapes below differ in what they do to each frame, not in what they
// are answerable for, so the account of how it went is made once here.
func (p *textPayload) deliverStream(tools DeliveryTools, resp *http.Response) fact.Delivery {
	if !p.cand.Passthrough {
		return p.deliverTranslatedStream(tools, resp)
	}
	var usage *protocols.IRUsage
	var err error
	if p.cand.EgressProtocol == protocols.ProtocolOpenAI {
		usage, err = p.pumpSameProtocol(tools, resp)
	} else {
		usage, err = p.pumpSameProtocolDecoded(tools, resp)
	}
	return p.settleStream(tools, resp, usageReportOf(usage), err)
}

// settleStream turns a pump's outcome into the delivery it amounts to.
//
// Every branch here turns on one question the pump cannot answer for itself:
// has the caller already been committed to. Before that moment nothing has
// reached them and another provider may still try; after it, the status is
// spent and the only thing left to decide is what the record says happened.
func (p *textPayload) settleStream(tools DeliveryTools, resp *http.Response, report *fact.UsageReported, err error) fact.Delivery {
	switch {
	case err == nil:
		return fact.Succeeded(http.StatusOK).WithUsage(report)

	case errors.Is(err, errClientCommitRefused):
		// Somebody else's status is on the wire. Another candidate could not
		// reach this caller and would only spend a provider call to find out.
		return fact.Truncated(tools.Client.CommittedStatus(), http.StatusInternalServerError,
			fact.FaultGateway, "client_commit_refused", err).WithUsage(report)

	case errors.Is(err, errClientDisconnected):
		// A caller can leave before the response was opened or long after, and
		// the pump reports both the same way — it knows the caller went, not
		// what they had by then. Only the response object knows that, which is
		// why the split is here: one caller holds part of an answer, the other
		// holds nothing, and recording them alike would claim a status the
		// second one never saw. Neither continues the chain either way.
		if tools.Client.Committed() {
			return callerLeftMidStream(err, report)
		}
		return fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient, "client_disconnected", err).WithUsage(report)

	case errors.Is(err, errStreamNoDoneTerminator):
		// No error frame here: the caller may well hold the whole completion,
		// since some upstreams simply never send the terminator, and injecting
		// an error into a response that turned out to be complete would break
		// it. What can honestly be said is that the end of the stream was never
		// seen, so it is not recorded as complete.
		return fact.Truncated(http.StatusOK, http.StatusOK, fact.FaultUpstream,
			noDoneTerminatorReason, err).WithUsage(report)

	case errors.Is(err, errStreamEndedUnannounced):
		// Same shape as above and a different reason on purpose: this one is
		// the ending that reads like a success everywhere else, so the reason
		// code is what carries it to somebody's attention.
		return fact.Truncated(http.StatusOK, http.StatusOK, fact.FaultUpstream,
			"stream_ended_unannounced", err).WithUsage(report)

	case tools.Client.Committed():
		return p.midStreamFailure(tools, report, err)
	}
	return fact.HandedOn(fact.FaultUpstream,
		"stream start: "+err.Error(), err).WithUsage(report)
}

// noDoneTerminatorReason marks a stream whose end was never announced. Named
// because two places have to agree on it: this is the one incomplete delivery
// an operator is not asked to look at, and the log level is decided elsewhere.
const noDoneTerminatorReason = "stream_no_done"

// midStreamFailure reports a stream that broke after the caller was committed.
//
// The order of these three is the whole content of the function. A write
// failure is checked first because it is the only unambiguous signal available:
// it comes from the write itself rather than from a side effect, and a write
// deadline expiring can make the caller's context look cancelled a moment
// later. Blame decided the other way around would put our own slow writes on
// the caller's record, and the caller's departure on the provider's.
func (p *textPayload) midStreamFailure(tools DeliveryTools, report *fact.UsageReported, err error) fact.Delivery {
	if isClientWriteError(err) {
		return p.clientWriteFailure(http.StatusOK, err, report)
	}
	if tools.Client.CallerGone() || errors.Is(err, context.Canceled) {
		return callerLeftMidStream(err, report)
	}
	// The upstream broke with the caller still reading. They cannot be given a
	// different status now, so they are given a frame that says so in their own
	// protocol instead of a connection that simply stops.
	_ = writeStreamErrorEvent(tools.Client, p.ingress, tools.RequestID)
	return fact.Truncated(http.StatusOK, http.StatusOK, fact.FaultUpstream,
		"stream_partial: "+err.Error(), err).WithUsage(report)
}

// callerLeftMidStream is what a caller who walks away part-way through amounts
// to, built in one place because two branches reach it: the pump can say the
// caller went, and a failure that looks like something else can turn out to be
// the same thing once the connection is asked about.
//
// Two statuses, because two questions: 200 is what they were served, 499 is
// that they never received the rest.
func callerLeftMidStream(err error, report *fact.UsageReported) fact.Delivery {
	return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_disconnected", err).WithUsage(report)
}

// deliverTranslatedStream re-encodes a provider's stream into the caller's
// protocol, event by event, as it arrives.
//
// The response is committed by the relay on its first encoded event rather than
// up front. Until then nothing has reached the caller and this candidate can
// still be replaced; after it, nothing can. That single moment is why the
// commit lives behind the response object: it is also where the exchange learns
// a first byte went out, and having one place decide both is what stops the two
// answers from disagreeing.
func (p *textPayload) deliverTranslatedStream(tools DeliveryTools, resp *http.Response) fact.Delivery {
	// Armed here and nowhere else: the relay below spends the whole delivery
	// blocked in a read, so a caller who leaves goes unnoticed until the
	// provider decides to stop sending, and the connection and the slot it
	// holds stay spent on nobody. The pumps come up for air between frames and
	// ask for themselves.
	defer tools.Client.WatchCaller(resp.Body)()

	decoder := codecsFor(p.cand.EgressProtocol).NewStreamDecoder()
	inner := codecsFor(p.ingress).NewStreamEncoder()
	// Only an OpenAI caller has a knob for this, and only the concrete encoder
	// carries it — the override wrapper below exposes the interface alone.
	if p.ingress == protocols.ProtocolOpenAI {
		if chatEncoder, ok := inner.(*chat.StreamEncoder); ok {
			chatEncoder.IncludeUsage = p.meta.WantsStreamUsage
		}
	}
	encoder := tools.Codecs.WrapStream(inner)

	buf := captureBuffer{capture: tools.Capture}
	var usage *protocols.IRUsage
	var err error
	if p.cand.EgressProtocol == protocols.ProtocolGemini {
		usage, err = protocols.IRStreamRelayJSONLines(tools.Client, resp, decoder, encoder, buf, nil, nil)
	} else {
		usage, err = protocols.IRStreamRelay(tools.Client, resp, decoder, encoder, buf, nil, nil)
	}
	// Kept even on the error paths: the upstream reported these however the
	// delivery then went, and an attempt that dropped them would be billed as
	// though it had consumed nothing.
	report := usageReportOf(reportedUsage(usage))
	// Settled by the same function the pumps use. A relay reports few distinct
	// failures, but one of them is a failed write to the caller — and telling
	// that apart from a provider that broke is the whole of what settling a
	// stream decides.
	return p.settleStream(tools, resp, report, err)
}

// streamPump is the state both same-protocol pumps carry through their loop.
//
// The two forward different bytes and learn about completion differently, but
// they open the response at the same moment and for the same reason, so that
// part is written once.
type streamPump struct {
	tools DeliveryTools
	// preamble holds the lines an upstream sends before its first data frame —
	// heartbeats, event:/id:/retry: directives, blank separators. They are kept
	// rather than dropped so the framing survives, but they are not sent yet:
	// sending them would commit the response, and the window in which another
	// candidate can still be tried stays open until real data arrives.
	preamble []byte
	// committed is this loop's own record of whether IT opened the response,
	// deliberately not the response object's answer to whether anything did.
	//
	// The two differ in exactly the case that matters. A response committed by
	// something else would make the object say yes, this loop would take that
	// for "already open", and it would forward a body under a status it never
	// chose — the refusal below is what catches that, and asking the object
	// would mean never reaching it.
	committed bool
}

// callerGone reports a caller who has stopped waiting, checked between frames
// so a departure mid-stream is noticed promptly rather than at the next write.
func (s *streamPump) callerGone() bool {
	select {
	case <-s.tools.Client.CallerDone():
		return true
	default:
		return false
	}
}

// open commits the response on the first data frame and releases the preamble
// behind it, in order.
//
// It happens here rather than up front because of what a commit costs: an
// upstream that returns 2xx and then dies without ever sending data has cost
// nothing as long as the caller has no status yet, and this candidate can be
// replaced. One data frame ends that.
func (s *streamPump) open() error {
	if cerr := writeSSEHeader(s.tools.Client); cerr != nil {
		// The writer refuses when the response was already committed by
		// something else. Carrying on would stream a body under a status this
		// loop did not choose. Passed through rather than relabelled: the
		// refusal already says what it is, and the relays reach settlement
		// carrying the same one.
		return fmt.Errorf("commit stream to client: %w", cerr)
	}
	s.committed = true
	if len(s.preamble) > 0 {
		if _, err := s.tools.Client.Write(s.preamble); err != nil {
			return fmt.Errorf("%w: passthrough write preamble to client: %w", protocols.ErrClientWrite, err)
		}
		s.preamble = nil
	}
	return nil
}

// buffer keeps a pre-first-data-frame line, up to a cap. A response body has no
// size guard the way a request body does, so an upstream that never sends a
// data frame must not be able to grow this without bound.
func (s *streamPump) buffer(line []byte) {
	if len(s.preamble)+len(line) <= maxPreambleBytes {
		s.preamble = append(s.preamble, line...)
	}
}

// admit decides what becomes of one line before anything is forwarded: the
// response is already open, or this line opens it, or it waits in the preamble.
// forward is false for the line that waits.
func (s *streamPump) admit(line []byte) (forward bool, err error) {
	switch {
	case s.committed:
		return true, nil
	case isDataLine(line):
		if err := s.open(); err != nil {
			return false, err
		}
		return true, nil
	default:
		s.buffer(line)
		return false, nil
	}
}

// flush pushes what was written and reports the failure a buffered write hides.
func (s *streamPump) flush() error {
	if ferr := s.tools.Client.Flush(); ferr != nil {
		return fmt.Errorf("%w: passthrough flush to client: %w", protocols.ErrClientWrite, ferr)
	}
	return nil
}

// newStreamScanner reads the upstream one SSE line at a time with a bound on
// how long one line may be.
//
// A Scanner rather than a Reader: ReadBytes allocates the whole line before any
// length check could run, which makes the cap decorative against exactly the
// upstream it exists to guard against.
func newStreamScanner(resp *http.Response) *bufio.Scanner {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLineBytes)
	return scanner
}

// pumpSameProtocol forwards an OpenAI stream to an OpenAI caller, rewriting the
// model name in each chunk back to the one the caller asked for and dropping
// the usage frame the gateway asked for on its own behalf.
//
// Returns the usage the upstream reported, and an error only for transport-level
// failures. The sentinels it returns are the ones settleStream reads.
func (p *textPayload) pumpSameProtocol(tools DeliveryTools, resp *http.Response) (*protocols.IRUsage, error) {
	defer func() { _ = resp.Body.Close() }()

	pump := &streamPump{tools: tools}
	var usage *protocols.IRUsage
	doneSeen := false
	scanner := newStreamScanner(resp)
	for scanner.Scan() {
		if pump.callerGone() {
			return clientDisconnectOutcome(usage, doneSeen)
		}
		// ScanLines strips the newline; put it back so every line downstream
		// has the shape it arrived in.
		line := append(scanner.Bytes(), '\n')

		forward, err := pump.admit(line)
		if err != nil {
			return usage, err
		}
		if !forward {
			continue
		}

		_, u, done, _, werr := writeStreamLine(pump.tools.Client, line, p.meta.Model, p.meta.WantsStreamUsage)
		if u != nil {
			usage = u
		}
		if done {
			doneSeen = true
		}
		if werr != nil {
			return usage, fmt.Errorf("%w: passthrough write to client: %w", protocols.ErrClientWrite, werr)
		}
		if ferr := pump.flush(); ferr != nil {
			return usage, ferr
		}
	}
	// The usage frame is OpenAI's own trailing matter, and this gateway asks
	// every upstream for it whether or not the caller wanted it — so its arrival
	// is a fact about the completion that survives a missing terminator.
	return usage, pump.end(scanner.Err(), doneSeen, usage != nil)
}

// pumpSameProtocolDecoded forwards a non-OpenAI stream to a caller who speaks
// the same protocol as the provider.
//
// It differs from pumpSameProtocol in two ways, both consequences of the same
// fact — a Claude, Gemini, or Responses event is not an OpenAI chunk. The model
// name sits somewhere protocol-specific in each, so rewriting it is delegated
// per line; and completion is not a [DONE] sentinel, so the protocol's own
// decoder is run alongside the forwarding purely to learn when the stream
// finished and what it cost. The decoded events themselves are discarded: the
// bytes already forwarded verbatim are what the caller received.
func (p *textPayload) pumpSameProtocolDecoded(tools DeliveryTools, resp *http.Response) (*protocols.IRUsage, error) {
	defer func() { _ = resp.Body.Close() }()

	pump := &streamPump{tools: tools}
	decoder := codecsFor(p.cand.EgressProtocol).NewStreamDecoder()
	var sig protocols.FinishSignals
	var accUsage protocols.IRUsage
	accumulate := func(deltas []protocols.IRStreamDelta) {
		sig.Accumulate(deltas)
		for _, d := range deltas {
			if du, ok := d.(protocols.DeltaUsage); ok {
				accUsage.Merge(du.Usage)
			}
		}
	}
	// Evaluated fresh on each return so a mid-stream failure still reports what
	// was collected before it. An all-zero usage comes back nil: unknown, not
	// zero.
	currentUsage := func() *protocols.IRUsage { return reportedUsage(&accUsage) }

	// claudeModelRewritten latches once the single message_start frame has been
	// rewritten, so later lines skip a parse that cannot find anything.
	claudeModelRewritten := false

	scanner := newStreamScanner(resp)
	for scanner.Scan() {
		if pump.callerGone() {
			return clientDisconnectOutcome(currentUsage(), sig.SawDone)
		}
		line := append(scanner.Bytes(), '\n')
		// Rewritten before both the decode and the write, so the caller, the
		// usage decoder, and the audit trail all see the same bytes.
		line = rewritePassthroughStreamModel(p.cand.EgressProtocol, line, p.meta.Model, &claudeModelRewritten)
		deltas, _ := decoder.DecodeChunk(string(line))
		accumulate(deltas)

		forward, err := pump.admit(line)
		if err != nil {
			return currentUsage(), err
		}
		if !forward {
			continue
		}

		if _, werr := pump.tools.Client.Write(line); werr != nil {
			return currentUsage(), fmt.Errorf("%w: passthrough write to client: %w", protocols.ErrClientWrite, werr)
		}
		if ferr := pump.flush(); ferr != nil {
			return currentUsage(), ferr
		}
	}
	if scanErr := scanner.Err(); scanErr == nil {
		// Clean EOF: some decoders only emit their terminal delta once a
		// trailing boundary confirms the buffered event is complete, which an
		// EOF right after the last byte would otherwise miss.
		if deltas, _ := decoder.Finish(); len(deltas) > 0 {
			accumulate(deltas)
		}
	}
	// One answer to both questions: these protocols carry usage inside their
	// terminal event, so nothing arrives after it that could vouch for a
	// completion the terminal event did not announce. There is also no sentinel
	// for a provider to omit here — unlike `data: [DONE]`, the terminal event is
	// the completion — so a stream that reaches this without one was cut short.
	return currentUsage(), pump.end(scanner.Err(), sig.SawDone, sig.SawDone)
}

// end says what the end of the upstream body amounted to.
//
// The three cases here are easy to get subtly wrong in two places, which is why
// both pumps come through: a caller who left looks like a read error, an
// upstream that closes right after finishing looks like one too, and a stream
// that produced no data at all is a candidate that can still be replaced rather
// than a response that went wrong.
//
// What the two pumps do NOT share is sawCompletion, and it is a parameter
// rather than something computed here because each protocol answers it
// differently. An OpenAI stream reports usage in a frame of its own, so that
// frame arriving is a fact about the completion separate from the terminator —
// which is what lets a stream missing only the terminator be told apart from
// one that was cut short. The other protocols carry usage inside their terminal
// event, so for them the two questions have one answer and demanding a separate
// signal would file whole streams as broken.
func (s *streamPump) end(scanErr error, sawTerminator, sawCompletion bool) error {
	// Whole means both: the end was announced AND everything the protocol puts
	// at the end arrived.
	whole := sawTerminator && sawCompletion
	if scanErr != nil {
		if s.tools.Client.CallerGone() {
			// The caller's departure cancels the upstream read too, so it
			// arrives here as a body-read failure rather than as itself.
			_, err := clientDisconnectOutcome(nil, sawTerminator)
			return err
		}
		if errors.Is(scanErr, bufio.ErrTooLong) {
			return fmt.Errorf("upstream stream line too long (max %d bytes)", maxStreamLineBytes)
		}
		// Some upstreams close non-standardly right after finishing. Once the
		// response is whole, that trailing transport error interrupts nothing,
		// and reporting it would file a fully delivered stream as failed.
		if whole && protocols.IsBenignPostDoneReadErr(scanErr) {
			return nil
		}
		return fmt.Errorf("read upstream stream: %w", scanErr)
	}
	if !s.committed {
		return errors.New("upstream stream ended before any data chunk")
	}
	if !sawTerminator {
		if sawCompletion {
			return errStreamNoDoneTerminator
		}
		return errStreamEndedUnannounced
	}
	return nil
}
