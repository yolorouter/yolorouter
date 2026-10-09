package playground

// Unit pins for the tap and the body builder. The endpoint-level behaviour
// (the whitelist a real run sends upstream) is covered by the router e2e
// tests; what is pinned here are the branches only a direct call can
// reach: the frame-draining of a CRLF exchange, and the marshal-failure
// contract of buildChatBody.

import (
	"math"
	"reflect"
	"testing"
)

// TestBuildChatBodyMarshalFailure: the error return is not decoration. A
// NaN temperature — a value JSON cannot encode — must surface as the
// builder's error, not as invalid JSON smuggled to an upstream. The HTTP
// endpoint cannot produce one (JSON has no NaN literal and the param
// binding would refuse it), but TextRun is an exported contract and a
// direct caller can; this pin keeps the error path live. Make the builder
// swallow the marshal error and this goes red.
func TestBuildChatBodyMarshalFailure(t *testing.T) {
	nan := math.NaN()
	_, err := buildChatBody(TextRun{
		Model:       "m",
		Messages:    []Message{{Role: "user", Content: "x"}},
		Temperature: &nan,
	})
	if err == nil {
		t.Fatal("a NaN temperature must fail the body marshal, not emit invalid JSON")
	}
}

// TestColumnTapDrainsCRLFFrames: the tap must recognise frames the way the
// whole codebase does — protocols.SSEFrameEnd, not a private "\n\n" scan.
// A CRLF-throughout exchange is the exact shape sse.go warns about: its
// "\r\n\r\n" separator contains no "\n\n", so a "\n\n"-only framer never
// finds a frame and silently drops the whole stream. Revert drainFrames to
// bytes.Index(frame, "\n\n") and the first two legs of this test go red
// while every LF-framed test stays green — the drift this pin exists to
// catch.
func TestColumnTapDrainsCRLFFrames(t *testing.T) {
	var got []string
	tap := newColumnTap(func(p string) { got = append(got, p) }, columnBodyCap)

	// CRLF spelling: each complete frame's data payloads reach the sink as
	// they land, non-data lines carrying nothing.
	_, _ = tap.Write([]byte("event: chunk\r\ndata: {\"a\":1}\r\n\r\n"))
	if !reflect.DeepEqual(got, []string{`{"a":1}`}) {
		t.Fatalf("CRLF frame payloads = %v, want [{\"a\":1}]", got)
	}

	// The LF spelling still drains exactly as before.
	_, _ = tap.Write([]byte("data: {\"b\":2}\n\n"))
	if !reflect.DeepEqual(got, []string{`{"a":1}`, `{"b":2}`}) {
		t.Fatalf("payloads after the LF frame = %v", got)
	}

	// A separator split across two writes holds its frame until the tail
	// arrives — a partial "\r\n" at the end of a write is not a match.
	_, _ = tap.Write([]byte("data: {\"c\":3}\r\n"))
	if !reflect.DeepEqual(got, []string{`{"a":1}`, `{"b":2}`}) {
		t.Fatalf("a frame whose separator has not fully arrived must be held, got %v", got)
	}
	_, _ = tap.Write([]byte("\r\n"))
	if !reflect.DeepEqual(got, []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}) {
		t.Fatalf("payloads after the split separator completed = %v", got)
	}
}
