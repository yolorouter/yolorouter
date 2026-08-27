package compress

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// stubView answers exactly the three questions the capability asks, which is
// the whole reason the view is declared here rather than taken from the kernel.
type stubView struct {
	enabled bool
	chat    bool
	proto   protocols.ProtocolID
}

func (v stubView) CompressEnabled() bool                 { return v.enabled }
func (v stubView) IsChatEndpoint() bool                  { return v.chat }
func (v stubView) IngressProtocol() protocols.ProtocolID { return v.proto }

// recordingSink captures what was reported, so a test can check the capability
// never claims a saving it did not make.
type recordingSink struct {
	facts   []fact.Fact
	records []fact.Record
}

func (s *recordingSink) Report(f ...fact.Fact) { s.facts = append(s.facts, f...) }
func (s *recordingSink) Note(r ...fact.Record) { s.records = append(s.records, r...) }

func (s *recordingSink) saved() (fact.TokensSaved, bool) {
	for _, r := range s.records {
		if rec, ok := r.(fact.TokensSaved); ok {
			return rec, true
		}
	}
	return fact.TokensSaved{}, false
}

func (s *recordingSink) skipped() (fact.CompressionSkipped, bool) {
	for _, r := range s.records {
		if rec, ok := r.(fact.CompressionSkipped); ok {
			return rec, true
		}
	}
	return fact.CompressionSkipped{}, false
}

// chatBody wraps content as the one message of an OpenAI chat request, which
// is where the engine looks for a rewritable zone.
func chatBody(content string) []byte {
	encoded, err := json.Marshal(content)
	if err != nil {
		panic(err)
	}
	return []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":` + string(encoded) + `}]}`)
}

func run(t *testing.T, v stubView, body []byte) ([]byte, *recordingSink) {
	t.Helper()
	sink := &recordingSink{}
	out, err := New().RewriteIngress(context.Background(), v, body, sink)
	if err != nil {
		t.Fatalf("RewriteIngress returned an error, but this capability must never refuse a body: %v", err)
	}
	return out, sink
}

// A body with enough compressible slack must come back smaller, and the saving
// must be reported — the report is the only channel the audit row and the
// priced saving both read, so an unreported pass is an invisible one.
func TestCompressesAndReportsTheSaving(t *testing.T) {
	body := chatBody(strings.Repeat("=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\n", 30) + "PASS\nok  \tpkg\t0.1s\n")

	out, sink := run(t, stubView{enabled: true, chat: true, proto: protocols.ProtocolOpenAI}, body)

	if len(out) >= len(body) {
		t.Fatalf("compressed body is %d bytes, not shorter than the original %d", len(out), len(body))
	}
	rec, ok := sink.saved()
	if !ok {
		t.Fatal("a pass that shrank the body reported no saving")
	}
	if rec.EstimatedTokens <= 0 {
		t.Errorf("EstimatedTokens = %d, want positive", rec.EstimatedTokens)
	}
	if len(rec.Compressors) == 0 {
		t.Error("a reported saving must name what produced it")
	}
	if _, skipped := sink.skipped(); skipped {
		t.Error("a successful pass also reported a skip")
	}
}

// Declining through Applies is what keeps the kernel from copying a body for
// nothing, and it is also why these requests report no skip reason: not being
// asked is not the same as trying and failing.
func TestAppliesOnlyToEnabledChatRequests(t *testing.T) {
	for _, tc := range []struct {
		name string
		view stubView
		want bool
	}{
		{"enabled chat", stubView{enabled: true, chat: true}, true},
		{"switched off", stubView{enabled: false, chat: true}, false},
		{"not a chat endpoint", stubView{enabled: true, chat: false}, false},
		{"off and not chat", stubView{enabled: false, chat: false}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := New().Applies(tc.view); got != tc.want {
				t.Fatalf("Applies = %v, want %v", got, tc.want)
			}
		})
	}
}

// A body the engine will not touch must come back byte-identical WITH a reason.
// This is the fail-open contract: the capability's failure is the caller's
// request going through unchanged, never an error, because a body this
// capability cannot shrink is one the upstream may still answer.
func TestUnshrinkableBodyPassesThroughWithAReason(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	out, sink := run(t, stubView{enabled: true, chat: true, proto: protocols.ProtocolOpenAI}, body)

	if !bytes.Equal(out, body) {
		t.Fatal("a body the engine skipped was still modified")
	}
	rec, ok := sink.skipped()
	if !ok {
		t.Fatal("a skipped pass reported no reason, so the audit row cannot say why")
	}
	if rec.Reason == "" {
		t.Error("the reported skip carries an empty reason")
	}
	if _, saved := sink.saved(); saved {
		t.Error("a skipped pass also claimed a saving")
	}
}

// Malformed JSON is the case most likely to arrive from a real client, and it
// must reach the upstream untouched so the upstream's own error — not ours —
// is what the caller sees.
func TestUnparseableBodyIsPassedThroughUntouched(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user",`)

	out, sink := run(t, stubView{enabled: true, chat: true, proto: protocols.ProtocolOpenAI}, body)

	if !bytes.Equal(out, body) {
		t.Fatal("an unparseable body was modified; the upstream would judge bytes the caller never sent")
	}
	if _, ok := sink.skipped(); !ok {
		t.Error("an unparseable body produced no skip record")
	}
}

// The bytes this capability is handed are the same ones the audit row keeps as
// the caller's verbatim request — the kernel hands over the live body rather
// than a copy, because copying one per request would cost more than it
// protects. So the contract falls to the capability: produce a new slice, never
// edit the one you were shown. Checked on both outcomes, since the shrinking
// path and the declining path reach the engine by different routes.
func TestNeverEditsTheBodyItWasShown(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"shrinkable", chatBody(strings.Repeat("=== RUN   TestFoo\n--- PASS: TestFoo (0.00s)\n", 30) + "PASS\nok  \tpkg\t0.1s\n")},
		{"nothing to shrink", []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)},
		{"unparseable", []byte(`{"model":"gpt-4o","messages":[{"role":"user",`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			untouched := bytes.Clone(tc.body)

			out, _ := run(t, stubView{enabled: true, chat: true, proto: protocols.ProtocolOpenAI}, tc.body)

			if !bytes.Equal(tc.body, untouched) {
				t.Fatalf("the input was edited in place; the audit row now describes bytes the caller never sent:\n got  %s\n want %s", tc.body, untouched)
			}
			if len(out) > 0 && len(tc.body) > 0 && &out[0] == &tc.body[0] && !bytes.Equal(out, tc.body) {
				t.Error("the returned body aliases the input yet differs from it, which is only possible by editing in place")
			}
		})
	}
}

// The capability's comments promise that the engine bounds its own pass with
// opts.Timeout, and New wiring DefaultOptions is the only thing making that
// budget positive — runCompress arms no deadline at zero. This pins the wire:
// a constructor that stops passing the defaults goes red here, not silently
// unbounded in production.
func TestNewCarriesAPositiveEngineBudget(t *testing.T) {
	if got := New().opts.Timeout; got <= 0 {
		t.Fatalf("New().opts.Timeout = %v, want > 0: the engine would run with no deadline", got)
	}
}
