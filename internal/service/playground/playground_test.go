package playground

// Unit pins for the body builder. The endpoint-level behaviour (the
// whitelist a real run sends upstream) is covered by the router e2e tests;
// what is pinned here is the marshal-failure contract of buildChatBody.

import (
	"math"
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
