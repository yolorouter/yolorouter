package protocols

import "testing"

func TestSSEFrameEnd(t *testing.T) {
	tests := []struct {
		name    string
		buf     string
		wantIdx int
		wantSep int
	}{
		{"LF frame", "data: a\n\nrest", 7, 2},
		{"LF then CRLF", "data: a\n\r\nrest", 7, 3},
		{"CRLF throughout", "data: a\r\n\r\nrest", 8, 3},
		{"no separator yet", "data: a\n", -1, 0},
		{"partial separator held for the tail", "data: a\n\r", -1, 0},
		{"empty buffer", "", -1, 0},
		{"separator at start", "\n\ndata: a", 0, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx, sep := SSEFrameEnd(tt.buf)
			if idx != tt.wantIdx || sep != tt.wantSep {
				t.Fatalf("SSEFrameEnd(%q) = (%d, %d), want (%d, %d)", tt.buf, idx, sep, tt.wantIdx, tt.wantSep)
			}
		})
	}
}

func TestSSEDataPayload(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantPayload string
		wantOK      bool
	}{
		{"with space", `data: {"a":1}`, `{"a":1}`, true},
		{"without space", `data:{"a":1}`, `{"a":1}`, true},
		{"trailing CR from a CRLF stream", "data: {\"a\":1}\r", `{"a":1}`, true},
		{"leading whitespace on the line", "  data: x", "x", true},
		{"done marker", "data: [DONE]", "[DONE]", true},
		{"empty payload is still a data line", "data:", "", true},
		{"comment line", ": keep-alive", "", false},
		{"event line", "event: message_start", "", false},
		{"blank line", "", "", false},
		{"prefix must be exact", "database: x", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, ok := SSEDataPayload(tt.line)
			if payload != tt.wantPayload || ok != tt.wantOK {
				t.Fatalf("SSEDataPayload(%q) = (%q, %v), want (%q, %v)", tt.line, payload, ok, tt.wantPayload, tt.wantOK)
			}
		})
	}
}

func TestSSEDataPayloadStart(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantStart int
		wantOK    bool
	}{
		{"with space", `data: {"a":1}`, 6, true},
		{"without space", `data:{"a":1}`, 5, true},
		{"only the first space is framing", "data:  x", 6, true},
		{"bare prefix", "data:", 5, true},
		{"not a data line", "event: x", 0, false},
		{"prefix must be exact", "database: x", 0, false},
		{"too short", "dat", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, ok := SSEDataPayloadStart([]byte(tt.line))
			if start != tt.wantStart || ok != tt.wantOK {
				t.Fatalf("SSEDataPayloadStart(%q) = (%d, %v), want (%d, %v)", tt.line, start, ok, tt.wantStart, tt.wantOK)
			}
		})
	}
}

// TestSSEDataPayloadStartDoesNotTrim pins the one documented divergence
// between the two readings: the parsing side trims the line first, the
// byte-preserving side must not — a verbatim forwarder that silently
// reframed an indented line would alter bytes it promised to keep. The
// prefix rule itself cannot diverge: SSEDataPayload is built on
// SSEDataPayloadStart.
func TestSSEDataPayloadStartDoesNotTrim(t *testing.T) {
	const line = "  data: x"
	if payload, ok := SSEDataPayload(line); !ok || payload != "x" {
		t.Fatalf("SSEDataPayload(%q) = (%q, %v), want (\"x\", true) — the parsing side trims first", line, payload, ok)
	}
	if _, ok := SSEDataPayloadStart([]byte(line)); ok {
		t.Fatalf("SSEDataPayloadStart(%q) = ok, want not a data line — the verbatim side must not trim", line)
	}
}
