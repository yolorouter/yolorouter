package providerclient

import (
	"strings"
	"testing"
)

// TestScanSSEStreamClassifiesLikeTheForwarder pins the probe's line
// classification to the strict reading the live forwarding path uses. The
// probe's answer is persisted as a capability the forwarder then has to
// honour, so the two must reject and accept the same lines: a spaceless
// `data:{...}` is a real delta, while an indented ` data: {...}` is not an
// SSE data line and must not certify an endpoint as streaming-capable.
func TestScanSSEStreamClassifiesLikeTheForwarder(t *testing.T) {
	const delta = `{"choices":[{"delta":{"content":"hi"}}]}`

	tests := []struct {
		name          string
		body          string
		wantDelta     bool
		wantTerminate bool
	}{
		{
			name:          "spaced data line with DONE",
			body:          "data: " + delta + "\n\ndata: [DONE]\n\n",
			wantDelta:     true,
			wantTerminate: true,
		},
		{
			name:          "spaceless data line still counts",
			body:          "data:" + delta + "\n\ndata:[DONE]\n\n",
			wantDelta:     true,
			wantTerminate: true,
		},
		{
			name: "indented line is not a data line",
			// The forwarder treats an indented line as preamble; certifying
			// streaming off it would persist a capability the live path
			// cannot honour.
			body:          "  data: " + delta + "\n\n",
			wantDelta:     false,
			wantTerminate: true, // clean EOF, but no delta was seen
		},
		{
			name:          "comment and event lines are skipped",
			body:          ": keep-alive\nevent: message\n\n",
			wantDelta:     false,
			wantTerminate: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sawDelta, terminate := scanSSEStream(strings.NewReader(tt.body))
			if sawDelta != tt.wantDelta || terminate != tt.wantTerminate {
				t.Fatalf("scanSSEStream(%q) = (delta=%v, terminate=%v), want (%v, %v)",
					tt.body, sawDelta, terminate, tt.wantDelta, tt.wantTerminate)
			}
		})
	}
}
