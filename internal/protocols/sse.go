package protocols

import "strings"

// This file is the single source for how this codebase recognises SSE
// framing and data lines. Every reader of a stream must share one answer:
// one reader requiring a space after "data:" while another accepts its
// absence reads the same delivered stream as two different streams, and
// the stricter side silently drops frames that were already paid for.

// SSEFrameEnd returns the offset and length of the first blank-line frame
// separator in buf, or (-1, 0) when no complete frame has arrived yet. SSE
// permits "\n", "\r\n", or "\r" line endings, so the separator is "\n\n",
// "\n\r\n", or — for a stream using CRLF throughout — "\r\n\r\n", which is
// a shape with no two consecutive newlines in it: a framer scanning for
// "\n\n" alone never finds a frame and drops the whole stream. The scan
// keys on a newline followed by an optional carriage return and another
// newline, which covers the LF and CRLF spellings (a bare-CR stream would
// need "\r\r", and nothing in this stack splits lines on bare CR); a
// partial separator at the end of buf deliberately does not match, so a
// frame split across reads is held until its tail arrives. The bytes
// before the returned offset are the frame's block, with each line's own
// trailing "\r" left for the per-line TrimSpace that already exists at
// every caller.
func SSEFrameEnd(buf string) (index, sepLen int) {
	for i := 0; i+1 < len(buf); i++ {
		if buf[i] != '\n' {
			continue
		}
		if buf[i+1] == '\n' {
			return i, 2
		}
		if i+2 < len(buf) && buf[i+1] == '\r' && buf[i+2] == '\n' {
			return i, 3
		}
	}
	return -1, 0
}

// SSEDataPayload reports whether line is an SSE data line, and hands back
// its payload with surrounding whitespace removed. The space after the
// colon is optional — the SSE grammar says so, and upstreams exist that
// send `data:{...}` — so a reader that requires it drops every frame of a
// delivered, billed stream. Leading whitespace on the line itself is
// tolerated.
//
// An empty payload returns ("", true): the line WAS a data line, and
// whether an empty one means anything is the caller's question, not this
// function's.
//
// This is the parsing-side reading; it normalises whitespace because its
// callers hand the payload to a JSON decoder. The prefix rule itself is
// SSEDataPayloadStart's — one definition, both readings. A forwarder that
// must reproduce the caller's framing byte-for-byte uses
// SSEDataPayloadStart directly.
func SSEDataPayload(line string) (payload string, ok bool) {
	trimmed := strings.TrimSpace(line)
	start, ok := SSEDataPayloadStart([]byte(trimmed))
	if !ok {
		return "", false
	}
	return strings.TrimSpace(trimmed[start:]), true
}

// SSEDataPayloadStart returns the offset where an SSE data line's payload
// starts — after "data:" and at most one optional space — or ok=false for
// a line that is not a data line. It applies the same prefix rule as
// SSEDataPayload but normalises nothing, and in particular does NOT trim
// the line: a verbatim forwarder must not silently reframe a line it did
// not recognise, so leading whitespace means "not a data line" here while
// the parsing-side reading trims it away first. The bytes from start
// onward, and the prefix before it, are exactly the caller's, so a
// forwarder can rewrite the payload and put the provider's own framing
// back untouched.
func SSEDataPayloadStart(line []byte) (start int, ok bool) {
	const prefix = "data:"
	if len(line) < len(prefix) || string(line[:len(prefix)]) != prefix {
		return 0, false
	}
	start = len(prefix)
	if start < len(line) && line[start] == ' ' {
		start++
	}
	return start, true
}
