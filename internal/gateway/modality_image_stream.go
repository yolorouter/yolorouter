package gateway

// The streaming half of the image modality. A stream=true ask on the Images
// API is answered with named-event SSE, and this file is the pump that
// forwards it verbatim while reading it for the two facts settlement needs:
// how many images completed, and what usage the last completed event
// reported. Everything else about the transfer — the preamble buffering,
// the commit-on-first-data-frame discipline, the per-line flush — is the
// shared stream machinery the text modality's pumps use.

import (
	"bufio"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/protocols/images"
)

// gptImagePrefix names the only model family whose upstreams stream image
// generation and edits today. The door checks it against the model the
// caller asked for, and Supports checks it again against the provider's own
// name for the model — the alias between the two can change the answer.
const gptImagePrefix = "gpt-image-"

// imageStreamLineBytes caps one SSE line of an image stream. A partial-image
// event embeds a whole base64 image, and a 1536x1024 high-quality one runs
// to several megabytes — an order larger than a chat line, which is why the
// images half does not borrow the text scanner's 1 MiB cap. Past this the
// scanner errors rather than truncating into a frame that would parse as
// something it is not.
const imageStreamLineBytes = 8 * 1024 * 1024

func newImageStreamScanner(resp *http.Response) *bufio.Scanner {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), imageStreamLineBytes)
	return scanner
}

// streamAsked reports whether the caller asked for the streaming delivery,
// whichever half of the API the request came in on.
func (p *imagePayload) streamAsked() bool {
	if p.isEdit() {
		return p.edit.Stream
	}
	return p.req.Stream
}

// completedEventName is the event that carries one finished image on the
// half this payload serves.
func (p *imagePayload) completedEventName() string {
	if p.isEdit() {
		return images.EventEditCompleted
	}
	return images.EventGenerationCompleted
}

// imageStreamState is what the pump learned from the stream it forwarded:
// the count that bills, the usage the last completed event reported, and
// the two endings an operator can be asked about.
type imageStreamState struct {
	completed  int
	usage      *images.StreamUsage
	errorEvent bool
	doneSeen   bool
}

// consume reads one already-forwarded line for the facts settlement needs.
// Run on every line, including one the caller never received: an error
// event lost to a hung-up client must still mark the stream as broken, or
// the provider reads as blameless and the request bills as complete.
func (s *imageStreamState) consume(line []byte, completedEvent string) {
	data, ok := strings.CutPrefix(string(line), "data:")
	if !ok {
		return
	}
	// SSE allows one optional space after the colon; both spellings are in
	// the wild.
	data = strings.TrimPrefix(data, " ")
	if data == "[DONE]" {
		s.doneSeen = true
		return
	}
	ev, err := images.ParseStreamEvent(data)
	if err != nil {
		// A data line that is not this vocabulary's JSON: forwarded
		// verbatim all the same, and not ours to judge here.
		return
	}
	switch ev.Type {
	case images.EventError:
		s.errorEvent = true
	case completedEvent:
		s.completed++
		s.usage = ev.Usage
	}
}

// report states the billable quantities of a whole stream: the images that
// completed (the count that bills), what was asked for, the pricing axes,
// and the token sub-counts when the events carried them. Nil when nothing
// completed — an empty stream is not a delivery, and must not bill as one.
func (p *imagePayload) streamReport(s *imageStreamState) *fact.UsageReported {
	if s.completed == 0 {
		return nil
	}
	requested, quality, size := p.requestAxes()
	report := &fact.UsageReported{
		Unit:      fact.UnitImage,
		Source:    fact.UsageFromUpstream,
		Count:     s.completed,
		Requested: requested,
		Quality:   quality,
		Size:      size,
	}
	if s.usage != nil {
		report.Prompt = s.usage.InputTokens
		report.Completion = s.usage.OutputTokens
		report.Total = s.usage.TotalTokens
	}
	return report
}

// deliverStream forwards an image SSE stream to the caller, verbatim, and
// settles what the stream amounted to. An image response carries no model
// field, so unlike the text pumps there is nothing to rewrite per line: the
// bytes the upstream sent are the bytes the caller gets.
func (p *imagePayload) deliverStream(tools DeliveryTools, resp *http.Response) fact.Delivery {
	defer func() { _ = resp.Body.Close() }()

	pump := &streamPump{tools: tools}
	state := &imageStreamState{}
	completedEvent := p.completedEventName()

	scanner := newImageStreamScanner(resp)
	for scanner.Scan() {
		if pump.callerGone() {
			return p.settleImageStream(tools, state, errCallerDisconnected)
		}
		// ScanLines strips the newline; put it back so the caller's stream
		// keeps the framing the upstream sent.
		line := append(scanner.Bytes(), '\n')

		// Parsed before it is forwarded, so the facts survive a caller who
		// hangs up mid-event.
		state.consume(line, completedEvent)

		forward, err := pump.admit(line)
		if err != nil {
			return p.settleImageStream(tools, state, err)
		}
		if !forward {
			continue
		}
		if _, werr := tools.Client.Write(line); werr != nil {
			return p.settleImageStream(tools, state, werr)
		}
		if isDataLine(line) {
			if ferr := pump.flush(); ferr != nil {
				return p.settleImageStream(tools, state, ferr)
			}
		}
		// The terminator is forwarded and then the reading stops: upstreams
		// that close sloppily right after it cannot turn a whole stream
		// into a failed read.
		if state.doneSeen {
			return p.settleImageStream(tools, state, nil)
		}
	}
	return p.settleImageStream(tools, state, scanner.Err())
}

// errCallerDisconnected is the pump's sentinel for a caller who stopped
// waiting, distinct from the write errors the client writer already wraps
// in protocols.ErrClientWrite.
var errCallerDisconnected = errors.New("image stream caller gone")

// settleImageStream turns the pump's outcome into the delivery it amounts
// to. One rule runs through every failure branch: an incomplete delivery
// bills nothing — no usage is attached, so settlement has no count to price
// — because the caller cannot be charged per image for images they were
// still waiting on when the stream broke.
func (p *imagePayload) settleImageStream(tools DeliveryTools, state *imageStreamState, err error) fact.Delivery {
	switch {
	case state.errorEvent:
		// The upstream said so itself, in the error event the caller
		// already received verbatim. The status is spent (the stream was
		// committed by its first frame), so what remains is the record:
		// provider's fault, nothing delivered whole, nothing billed. Read
		// before the err==nil cases on purpose: an error event is usually
		// the stream's clean ending, and nothing about a clean ending
		// repairs it.
		return fact.Truncated(http.StatusOK, http.StatusOK, fact.FaultUpstream,
			"image_stream_error_event", err)

	case err == nil && state.completed > 0:
		return fact.Succeeded(http.StatusOK).WithUsage(p.streamReport(state))

	case err == nil:
		// A clean end with no completed image. If nothing was ever sent,
		// this candidate can still be replaced; if frames went out, the
		// caller holds a stream that promised images and delivered none.
		if !tools.Client.Committed() {
			return fact.HandedOn(fact.FaultUpstream, "image_stream_no_images", nil)
		}
		return fact.Truncated(http.StatusOK, http.StatusOK, fact.FaultUpstream,
			"image_stream_no_images", nil)

	case errors.Is(err, errCallerDisconnected):
		if tools.Client.Committed() {
			return fact.Truncated(http.StatusOK, 499, fact.FaultClient,
				"client_disconnected", err)
		}
		return fact.Undelivered(499, fact.VerdictSettled, fact.FaultClient,
			"client_disconnected", err)

	case errors.Is(err, protocols.ErrClientWrite):
		if !tools.Client.Committed() {
			// The write that failed was the preamble flush inside the
			// commit itself: nothing went out under any status this pump
			// chose.
			return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled, fact.FaultGateway,
				"commit_failed: "+err.Error(), err)
		}
		return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_write_timeout", err)

	case errors.Is(err, bufio.ErrTooLong):
		return fact.Truncated(tools.Client.CommittedStatus(), http.StatusOK, fact.FaultUpstream,
			fmt.Sprintf("image_stream_line_too_long (max %d bytes)", imageStreamLineBytes), err)

	case tools.Client.Committed():
		// A read failure with the caller still reading: they hold a partial
		// stream and the upstream holds the blame.
		return fact.Truncated(http.StatusOK, http.StatusOK, fact.FaultUpstream,
			"image_stream_read: "+err.Error(), err)
	}
	// The stream broke before anything was committed: another candidate can
	// still try.
	return fact.HandedOn(fact.FaultUpstream, "image_stream_start: "+err.Error(), err)
}
