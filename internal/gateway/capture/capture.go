// Package capture owns every byte the gateway records about one exchange for
// the audit row: the caller's request, its compressed variant, what was sent
// upstream, what came back, what the caller actually received, and the
// stream capture file a streamed response is appended to.
//
// The fields are unexported and every write goes through a method, so the
// compiler holds what used to be a convention: one owner for the audit
// record. The package is pure state — no logging, no web framework, no
// request context. Callers resolve paths and log failures; this type only
// records.
package capture

import (
	"os"
	"path/filepath"
)

// MaxStreamFileBytes is a HARD anti-OOM disk backstop for the stream capture
// file, NOT a content truncation (the capture must record every sent SSE
// fragment, never cut short for length) — a normal chat stream never comes
// close to it; only a hostile or buggy upstream maxing bandwidth within the
// upstream timeout could. Hitting it marks the capture truncated (marked,
// never silent) and stops appending; the caller's own stream is entirely
// unaffected. A package var (not const) so tests can inject a small value
// instead of actually writing 1 GiB.
var MaxStreamFileBytes int64 = 1 << 30 // 1 GiB

// Bodies is the audit record of one exchange's bodies. The zero value is
// ready to use.
//
// The request/response pairs follow "last attempt wins": each upstream
// attempt overwrites what the previous one recorded, so the row ends up
// describing the attempt that produced the response the caller saw.
type Bodies struct {
	// request is the caller's body, verbatim. compressedRequest is what a
	// successful compression pass produced — what admission is handed and the
	// payload encodes from — while request stays the verbatim caller body for
	// the audit row.
	request           []byte
	compressedRequest []byte
	// upstreamRequest is what the last attempt sent; upstreamResponse is what
	// the upstream returned, unaltered (bounded-read). response is the
	// caller-FACING body (post-rewrite, post-usage-strip, including local
	// error JSON).
	upstreamRequest  []byte
	response         []byte
	upstreamResponse []byte
	// streamFile is the open capture file a streamed response's sent SSE
	// lines are appended to as they go out, instead of being buffered in
	// memory. streamCaptured reports a file was opened (and survived the
	// empty-file cleanup); streamTruncated flips true only if the backstop
	// fired. streamBytes mirrors the file's current size so AppendStream can
	// check the backstop with a plain integer instead of a Stat per line.
	streamFile      *os.File
	streamCaptured  bool
	streamTruncated bool
	streamBytes     int64
	// streamDir / streamName locate the capture file for the discard path
	// and the persisted stream_body_path — set once by OpenStream, the only
	// place the name is ever built.
	streamDir  string
	streamName string
}

// SetRequest records the caller's body, verbatim.
func (b *Bodies) SetRequest(body []byte) { b.request = body }

// Request is the caller's body, verbatim.
func (b *Bodies) Request() []byte { return b.request }

// SetCompressedRequest records the body a successful compression pass
// produced.
func (b *Bodies) SetCompressedRequest(body []byte) { b.compressedRequest = body }

// CompressedRequest is the compressed caller body, nil when compression did
// not run or was skipped.
func (b *Bodies) CompressedRequest() []byte { return b.compressedRequest }

// SetUpstreamRequest records what this attempt sent upstream. Overwritten on
// every attempt — the last write wins.
func (b *Bodies) SetUpstreamRequest(body []byte) { b.upstreamRequest = body }

// UpstreamRequest is what the last attempt sent.
func (b *Bodies) UpstreamRequest() []byte { return b.upstreamRequest }

// BeginUpstreamAttempt clears the request/response bodies the previous
// attempt recorded. The request and the response it produced are one pair:
// clearing only the response would leave the audit row showing an earlier
// attempt's request body with no response beside it — a request that was
// never the one sent.
func (b *Bodies) BeginUpstreamAttempt() {
	b.upstreamRequest = nil
	b.ClearResponses()
}

// SetResponse records the caller-facing body actually written to the client.
// Assigned, not appended: this is what THIS attempt delivered, and an earlier
// attempt's bytes are a different response that must not be concatenated onto
// it.
func (b *Bodies) SetResponse(body []byte) { b.response = body }

// Response is what the caller received.
func (b *Bodies) Response() []byte { return b.response }

// SetUpstreamResponse records the upstream's own bytes, unaltered.
func (b *Bodies) SetUpstreamResponse(body []byte) { b.upstreamResponse = body }

// UpstreamResponse is what the upstream returned, unaltered.
func (b *Bodies) UpstreamResponse() []byte { return b.upstreamResponse }

// ClearResponses drops both response bodies before an attempt commits to
// writing a 2xx response to the client. A prior failed candidate may have
// stashed a non-2xx error body in these fields ("last attempt wins");
// without this clear, a stale earlier-candidate error body would be
// persisted as this successful request's upstream/response body.
func (b *Bodies) ClearResponses() {
	b.upstreamResponse = nil
	b.response = nil
}

// BeginDeliveryCapture starts an attempt's upstream capture and clears what
// the last one left. Clearing here rather than on the first write is the
// difference between an audit row that says nothing and one that says
// something false: an attempt that never produces a body would otherwise
// leave the previous provider's bytes standing under this attempt's heading.
func (b *Bodies) BeginDeliveryCapture() {
	b.upstreamResponse = nil
}

// StreamFileName is the naming rule for a request's stream capture file.
// Declared here, where the file is opened, deleted and reported, so the name
// exists in exactly one place: the persisted stream_body_path, the open and
// the discard all go through it.
func StreamFileName(requestID string) string { return requestID + ".stream" }

// OpenStream opens (or re-opens) the stream capture file for requestID under
// dir. The capture owns the file's name from here on: StreamName reports it
// and DiscardEmptyStream deletes it without the caller respelling anything.
//
// Calling it twice for one attempt keeps the first file rather than opening a
// second one — overwriting the handle would drop the only reference to a
// descriptor nothing will ever close. Re-entry BETWEEN attempts is different
// and still appends: the previous attempt's file was closed on its way out,
// and O_APPEND is what lets a failover keep writing into the bytes an earlier
// attempt already captured. One Stat here (not per appended line) seeds the
// in-memory backstop counter with the file's real starting size so it never
// drifts out of sync with what prior attempts already wrote.
//
// The returned error is an OS-level open failure the caller should log; the
// capture is skipped but the caller's own stream is unaffected.
func (b *Bodies) OpenStream(dir, requestID string) error {
	if b.streamFile != nil {
		return nil
	}
	name := StreamFileName(requestID)
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	b.streamFile = f
	b.streamName = name
	b.streamDir = dir
	b.streamCaptured = true
	if info, statErr := f.Stat(); statErr == nil {
		b.streamBytes = info.Size()
	}
	return nil
}

// StreamName is the base name of the captured stream file — the value the
// audit row persists as stream_body_path — or "" when no capture happened
// (or an empty one was discarded).
func (b *Bodies) StreamName() string {
	if !b.streamCaptured {
		return ""
	}
	return b.streamName
}

// AppendStream appends one already-caller-facing SSE line to the stream
// capture file, verbatim. A no-op when the file was never opened or the
// backstop already fired. The returned error is a write failure the caller
// should log; the bytes counter still advances by what was written.
func (b *Bodies) AppendStream(line []byte) error {
	if b.streamFile == nil || b.streamTruncated || len(line) == 0 {
		return nil
	}
	if b.streamBytes+int64(len(line)) > MaxStreamFileBytes {
		b.streamTruncated = true
		return nil
	}
	n, err := b.streamFile.Write(line)
	b.streamBytes += int64(n)
	return err
}

// CloseStream closes the capture file, if any. Safe to call when open never
// succeeded.
func (b *Bodies) CloseStream() {
	if b.streamFile != nil {
		_ = b.streamFile.Close()
		b.streamFile = nil
	}
}

// DiscardEmptyStream deletes the capture file if it ended up completely
// empty — the stream failed before any data reached the caller. Left in
// place, an empty capture would show as an empty, useless "stream body" link
// on the request-log detail page. A no-op when nothing was ever captured or
// the file has content — a truncated capture in particular is never
// discarded.
//
// The descriptor is closed BEFORE unlinking: leaving it open past the remove
// would let a later append write to an unlinked inode (bytes silently lost),
// and on Windows removing an open file fails outright, leaving a stale empty
// capture path behind.
func (b *Bodies) DiscardEmptyStream() {
	if !b.streamCaptured || b.streamTruncated {
		return
	}
	path := filepath.Join(b.streamDir, b.streamName)
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		return
	}
	b.CloseStream()
	_ = os.Remove(path)
	b.streamCaptured = false
}

// DropStream closes and deletes the capture file, if any, and unmarks it —
// the caller-facing body it holds is not to be persisted.
//
// Distinct from DiscardEmptyStream, which keeps a capture that has content and
// only removes an empty one: this drops whatever is there. Unlike that path it
// also discards a truncated capture, since the decision to keep nothing is not
// about what the file managed to record.
func (b *Bodies) DropStream() {
	if !b.streamCaptured {
		return
	}
	path := filepath.Join(b.streamDir, b.streamName)
	b.CloseStream()
	_ = os.Remove(path)
	b.streamCaptured = false
}

// StreamCaptured reports whether a capture file was opened and kept.
func (b *Bodies) StreamCaptured() bool { return b.streamCaptured }

// StreamTruncated reports whether the stream capture hit its backstop.
func (b *Bodies) StreamTruncated() bool { return b.streamTruncated }
