package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// streamRequestID is the request id every stream below is delivered under. It
// reaches caller bytes only through an error frame, so it is fixed rather than
// generated: the expected bytes for the mid-stream failure cases quote it.
const streamRequestID = "stream-under-test"

// streamOutcome is what a streaming delivery is answerable for. It differs from
// deliveryOutcome in where the audit bytes live: a stream's account is the
// capture file, not a field on the exchange.
type streamOutcome struct {
	delivery       fact.Delivery
	clientStatus   int
	clientBody     string
	captured       string
	captureExists  bool
	firstByteSent  bool
	completionSeen int
	promptSeen     int
}

// streamWant is that same account, written down.
type streamWant struct {
	clientBody    string
	captureExists bool
	firstByteSent bool
	promptSeen    int
	completion    int
	clientStatus  int
	verdict       fact.Verdict
	committed     bool
	complete      bool
}

func upstreamStream(t *testing.T, body string) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// runStream delivers one upstream stream through the modality.
func runStream(t *testing.T, ingress, egress protocols.ProtocolID, passthrough bool, callerPath, callerBody string, resp *http.Response) streamOutcome {
	t.Helper()
	dir := t.TempDir()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).
		WithContext(context.Background())
	c.Set(BodiesDirContextKey, dir)

	payload, rej := NewTextModality().Admit(context.Background(), Ingress{
		Protocol: ingress, Path: callerPath, Body: []byte(callerBody),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid body: %+v", rej)
	}
	if _, err := payload.PrepareUpstream(Candidate{
		ProviderModelName: "provider-model", EgressProtocol: egress, Passthrough: passthrough,
	}); err != nil {
		t.Fatalf("PrepareUpstream = %v", err)
	}

	// originalModel comes from the payload's routing intent, the way Handle
	// sets it; the codec wrappers the router registers read it from there.
	rc := &Exchange{requestID: streamRequestID, ingress: ingress, isStream: true,
		originalModel: payload.Routing().Model}
	tools, release := serviceAsAssembled().newDeliveryTools(c, rc, TransferLimits{}, true)
	d := payload.Deliver(tools, resp)
	// The kernel takes the toolbox back before anything reads the audit trail:
	// that is when an empty capture is removed and the file is closed, and a test
	// that read the file first would be reading a record nobody had finished.
	release()

	captured, exists := readCapture(t, dir, rc.requestID)
	out := streamOutcome{
		delivery: d, clientStatus: w.Code, clientBody: w.Body.String(),
		captured:      captured,
		captureExists: exists,
		firstByteSent: rc.firstByteSent,
	}
	if d.Usage != nil {
		out.completionSeen = d.Usage.Completion
		out.promptSeen = d.Usage.Prompt
	}
	return out
}

// readCapture returns the capture's content and whether the file is there at
// all.
//
// The two are different answers. A stream that sent nothing should leave NO
// file, and reading both as "" makes that assertion pass just as happily when
// an empty one was left behind.
func readCapture(t *testing.T, dir, requestID string) (content string, exists bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, requestID+".stream"))
	if os.IsNotExist(err) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read capture file: %v", err)
	}
	return string(b), true
}

var (
	// generatedID matches the identifier the gateway mints for a response whose
	// provider did not supply one.
	generatedID = regexp.MustCompile(`gen-[A-Za-z0-9]+`)
	// createdAt matches the timestamp the chat encoder stamps on every frame it
	// builds, taken from the clock when the stream opened.
	createdAt = regexp.MustCompile(`"created":\d+`)
)

// steady replaces what is minted fresh on every run with a fixed token, so the
// expected bytes can be written down at all.
//
// The identifier check earns its keep: it is a fresh random string per call, so
// frames disagreeing about which response they belong to would differ and be
// caught before the substitution hides them.
//
// The timestamp check proves much less, and is not claimed to prove more. The
// clock has one-second resolution and a test stream is written well inside one
// second, so an encoder re-reading it per frame would still produce identical
// values here; only a per-frame value drawn from something other than a clock
// would differ enough to be caught. Nor does it notice a frame missing the
// field — no match is not a disagreeing match. That case is covered by the
// expected bytes themselves, which spell out `"created":*` in every frame.
func steady(t *testing.T, body string) string {
	t.Helper()
	requireOneValue(t, generatedID.FindAllString(body, -1), "identifier")
	requireOneValue(t, createdAt.FindAllString(body, -1), "timestamp")
	body = generatedID.ReplaceAllString(body, "gen-*")
	return createdAt.ReplaceAllString(body, `"created":*`)
}

func requireOneValue(t *testing.T, found []string, what string) {
	t.Helper()
	if len(found) == 0 {
		return
	}
	for _, v := range found[1:] {
		if v != found[0] {
			t.Errorf("one response carries two values for its %s, %q and %q", what, found[0], v)
			return
		}
	}
}

// checkStream holds a delivery to the account written down for it.
//
// The expected bytes are the ones this path delivers. They are spelled out
// rather than derived so that a change to any of it — a frame reordered, a
// rewrite dropped, a terminator no longer sent — shows up here as a diff a
// reader can judge, instead of passing because both sides of a comparison moved
// together.
func checkStream(t *testing.T, want streamWant, got streamOutcome) {
	t.Helper()
	wantBody := steady(t, want.clientBody)
	gotBody, gotCaptured := steady(t, got.clientBody), steady(t, got.captured)

	if got.clientStatus != want.clientStatus {
		t.Errorf("caller received status %d, want %d", got.clientStatus, want.clientStatus)
	}
	if gotBody != wantBody {
		t.Errorf("caller received\n got: %q\nwant: %q", gotBody, wantBody)
	}
	if gotCaptured != gotBody {
		t.Errorf("capture file and caller disagree; the file promises exactly what the caller received\n file: %q\ncaller: %q", gotCaptured, gotBody)
	}
	if got.captureExists != want.captureExists {
		t.Errorf("capture file exists = %v, want %v", got.captureExists, want.captureExists)
	}
	if got.firstByteSent != want.firstByteSent {
		t.Errorf("first byte recorded as sent = %v, want %v", got.firstByteSent, want.firstByteSent)
	}
	if got.completionSeen != want.completion {
		t.Errorf("completion tokens = %d, want %d", got.completionSeen, want.completion)
	}
	if got.promptSeen != want.promptSeen {
		t.Errorf("prompt tokens = %d, want %d", got.promptSeen, want.promptSeen)
	}
	if got.delivery.Verdict != want.verdict {
		t.Errorf("verdict = %v, want %v", got.delivery.Verdict, want.verdict)
	}
	if got.delivery.Committed != want.committed {
		t.Errorf("committed = %v, want %v", got.delivery.Committed, want.committed)
	}
	if got.delivery.Complete != want.complete {
		t.Errorf("complete = %v, want %v", got.delivery.Complete, want.complete)
	}
}

// requireDelivered fails a case that delivered nothing.
//
// A case whose expected body is empty asserts nothing about the pump: every
// byte-level check passes on empty strings, and the row reads as covered while
// exercising none of the path it was added for. The table below is for streams
// that arrive; the ones that do not have their own tests.
func requireDelivered(t *testing.T, got streamOutcome) {
	t.Helper()
	if got.clientBody == "" {
		t.Fatalf("this case delivered nothing (verdict %v); it exercises none of the pump", got.delivery.Verdict)
	}
	if !got.delivery.Complete {
		t.Fatalf("this case did not complete (fail reason %q)", got.delivery.FailReason)
	}
}

// TestTheModalityStreamsWhatItPromises delivers one upstream stream per shape
// and holds each to the full account.
//
// Equal caller bytes is the loudest of these but not the only one that matters:
// the capture file has to hold the same thing, the usage has to be read off the
// frames, and a failure has to leave the right verdict behind. A change that got
// the bytes right and any of those wrong would look correct from the caller's
// side and be wrong everywhere the request is later answered for.
func TestTheModalityStreamsWhatItPromises(t *testing.T) {
	// The provider's own name for the model, deliberately not the caller's: a
	// rewrite to the same string proves nothing, and that is exactly the fixture
	// that let a dropped rewrite pass.
	const claudeUpstream = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	// What a caller sees when a claude stream is forwarded to them, up to but
	// not including the blank line that closes the last frame — the two cases
	// below differ in whether that frame is the end of the stream. Only
	// message_start is rewritten (to the caller's name for the model, which
	// re-serialises its keys); the rest is passed on byte for byte.
	const claudeForwarded = "event: message_start\n" +
		`data: {"message":{"id":"msg_1","model":"claude-3-5-sonnet","usage":{"input_tokens":7,"output_tokens":0}},"type":"message_start"}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n"

	cases := []struct {
		name        string
		ingress     protocols.ProtocolID
		egress      protocols.ProtocolID
		passthrough bool
		// callerPath matters for protocols that carry the model in the URL
		// rather than the body.
		callerPath string
		caller     string
		upstream   string
		want       streamWant
	}{
		{
			name:    "openai caller, claude provider",
			ingress: protocols.ProtocolOpenAI,
			egress:  protocols.ProtocolClaude,
			caller:  `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			// Re-encoded frame by frame into the caller's protocol, under the
			// name the caller asked for and the provider's own message id.
			upstream: claudeUpstream,
			want: streamWant{
				clientBody: `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"created":*,"id":"msg_1","model":"gpt-4o","object":"chat.completion.chunk"}` + "\n\n" +
					`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":*,"id":"msg_1","model":"gpt-4o","object":"chat.completion.chunk"}` + "\n\n" +
					"data: [DONE]\n\n",
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
		{
			name:    "claude caller, openai provider",
			ingress: protocols.ProtocolClaude,
			egress:  protocols.ProtocolOpenAI,
			caller:  `{"model":"claude-3-5-sonnet","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream: `data: {"id":"c1","model":"gpt-4o-real","choices":[{"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
				`data: {"id":"c1","model":"gpt-4o-real","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n" +
				"data: [DONE]\n\n",
			// Claude's event sequence has frames OpenAI's does not, so the two
			// content deltas above become six events here: the block open and
			// close are synthesised, not forwarded.
			want: streamWant{
				clientBody: "event: message_start\n" +
					`data: {"message":{"content":[],"id":"c1","model":"claude-3-5-sonnet","role":"assistant","stop_reason":null,"stop_sequence":null,"type":"message","usage":{"input_tokens":0,"output_tokens":0}},"type":"message_start"}` + "\n\n" +
					"event: content_block_start\n" +
					`data: {"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}` + "\n\n" +
					"event: content_block_delta\n" +
					`data: {"delta":{"text":"hi","type":"text_delta"},"index":0,"type":"content_block_delta"}` + "\n\n" +
					"event: content_block_stop\n" +
					`data: {"index":0,"type":"content_block_stop"}` + "\n\n" +
					"event: message_delta\n" +
					`data: {"delta":{"stop_reason":"end_turn","stop_sequence":null},"type":"message_delta","usage":{"input_tokens":7,"output_tokens":3}}` + "\n\n" +
					"event: message_stop\n" +
					`data: {"type":"message_stop"}` + "\n\n",
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
		{
			name:        "openai caller, openai provider, forwarded as-is",
			ingress:     protocols.ProtocolOpenAI,
			egress:      protocols.ProtocolOpenAI,
			passthrough: true,
			caller:      `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			// Opens with a comment heartbeat and a retry directive: lines that
			// arrive before any data and must reach the caller in order once the
			// first data frame commits the response, not be dropped.
			upstream: ": ping\n\nretry: 1000\n\n" +
				`data: {"id":"c1","model":"gpt-4o-real","choices":[{"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
				`data: {"id":"c1","model":"gpt-4o-real","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}` + "\n\n" +
				"data: [DONE]\n\n",
			// Forwarded frames keep the provider's own field order; only the
			// model is rewritten. The usage block is gone because this caller
			// never asked for it — the tokens are still read off the frame and
			// reported below, they are just not passed on.
			want: streamWant{
				clientBody: ": ping\n\nretry: 1000\n\n" +
					`data: {"choices":[{"delta":{"role":"assistant","content":"hi"}}],"id":"c1","model":"gpt-4o"}` + "\n\n" +
					`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"id":"c1","model":"gpt-4o"}` + "\n\n" +
					"data: [DONE]\n\n",
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
		{
			// Gemini is delivered by the pump that splits on newlines rather
			// than scanning SSE frames. Nothing else in this table goes through
			// it.
			name:    "openai caller, gemini provider",
			ingress: protocols.ProtocolOpenAI,
			egress:  protocols.ProtocolGemini,
			caller:  `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.0-flash-real"}` + "\n\n" +
				`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10},"modelVersion":"gemini-2.0-flash-real"}` + "\n\n",
			// This provider supplies no response id, so the gateway mints one —
			// the same one on every frame.
			want: streamWant{
				clientBody: `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":null,"index":0}],"created":*,"id":"gen-*","model":"gpt-4o","object":"chat.completion.chunk"}` + "\n\n" +
					`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":*,"id":"gen-*","model":"gpt-4o","object":"chat.completion.chunk"}` + "\n\n" +
					"data: [DONE]\n\n",
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
		{
			// The completion signal is the LAST event and it has no trailing
			// blank line, so the decoder holds it until asked to finish. Any
			// earlier terminated signal would make that flush unnecessary and
			// the case would pass without it. The expected bytes end in a single
			// newline for the same reason: that frame is closed by the flush.
			name:        "claude caller, claude provider, completion held back",
			ingress:     protocols.ProtocolClaude,
			egress:      protocols.ProtocolClaude,
			passthrough: true,
			caller:      `{"model":"claude-3-5-sonnet","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream: "event: message_start\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
			want: streamWant{
				clientBody:    claudeForwarded,
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
		{
			// Gemini's decoder holds a frame until a blank line closes it, so a
			// stream whose last frame arrives without one is only completed by
			// the flush at the end. Claude's decodes per line and would pass
			// without it.
			name:        "gemini caller, gemini provider, completion held back",
			ingress:     protocols.ProtocolGemini,
			egress:      protocols.ProtocolGemini,
			passthrough: true,
			callerPath:  "/v1beta/models/gemini-2.0-flash:streamGenerateContent",
			caller:      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			upstream: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.0-flash-real"}` + "\n\n" +
				`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10},"modelVersion":"gemini-2.0-flash-real"}`,
			want: streamWant{
				clientBody: `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]}}],"modelVersion":"gemini-2.0-flash"}` + "\n\n" +
					`data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"modelVersion":"gemini-2.0-flash","usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":3,"totalTokenCount":10}}` + "\n",
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
		{
			name:        "claude caller, claude provider, forwarded as-is",
			ingress:     protocols.ProtocolClaude,
			egress:      protocols.ProtocolClaude,
			passthrough: true,
			caller:      `{"model":"claude-3-5-sonnet","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			upstream:    claudeUpstream,
			want: streamWant{
				clientBody: claudeForwarded + "\nevent: message_stop\n" +
					`data: {"type":"message_stop"}` + "\n\n",
				captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
				clientStatus: 200, verdict: fact.VerdictSettled, committed: true, complete: true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.callerPath
			if path == "" {
				path = "/v1/chat/completions"
			}
			got := runStream(t, tc.ingress, tc.egress, tc.passthrough, path, tc.caller, upstreamStream(t, tc.upstream))
			requireDelivered(t, got)
			checkStream(t, tc.want, got)
		})
	}
}

// TestAStreamThatEndsWithoutItsTerminatorIsNotCalledComplete pins the case an
// equal-bytes assertion alone would let through.
//
// Neither of these streams sends its terminator, so neither can be called
// complete: what cannot be said is that the end of the response was seen, and a
// delivery reported complete here is one the audit trail would later claim
// finished when nobody knows that it did. Nothing is injected into the caller's
// stream and the status stays 200 either way — a caller who turned out to hold
// the whole answer must not have it broken to make a point.
//
// The two carry different reasons because they are different facts, and the
// reason is the only field that differs. The first still produced its final
// usage, which only a stream that reached its end produces; the second said
// nothing at all about having finished.
func TestAStreamThatEndsWithoutItsTerminatorIsNotCalledComplete(t *testing.T) {
	const delta = `data: {"id":"c1","model":"gpt-4o-real","choices":[{"delta":{"content":"hi"}}]}` + "\n\n"
	const usageFrame = `data: {"id":"c1","model":"gpt-4o-real","choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}` + "\n\n"
	const caller = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	const forwarded = `data: {"choices":[{"delta":{"content":"hi"}}],"id":"c1","model":"gpt-4o"}` + "\n\n"

	cases := []struct {
		name       string
		upstream   string
		clientBody string
		promptSeen int
		completion int
		wantReason string
	}{
		{
			name:     "a provider that never sends the terminator",
			upstream: delta + usageFrame,
			// The caller did not ask for usage, so the frame the gateway
			// requested on its own behalf is stripped back out of it.
			clientBody: forwarded + `data: {"choices":[],"id":"c1","model":"gpt-4o"}` + "\n\n",
			promptSeen: 3, completion: 4,
			wantReason: "stream_no_done",
		},
		{
			name:       "a completion that stopped in the middle",
			upstream:   delta,
			clientBody: forwarded,
			wantReason: "stream_ended_unannounced",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runStream(t, protocols.ProtocolOpenAI, protocols.ProtocolOpenAI, true, "/v1/chat/completions", caller, upstreamStream(t, tc.upstream))
			checkStream(t, streamWant{
				clientBody:    tc.clientBody,
				captureExists: true, firstByteSent: true,
				promptSeen: tc.promptSeen, completion: tc.completion,
				clientStatus: http.StatusOK, verdict: fact.VerdictSettled, committed: true, complete: false,
			}, got)

			if got.delivery.Fault != fact.FaultUpstream {
				t.Errorf("fault = %v, want upstream; the provider is the one that stopped short", got.delivery.Fault)
			}
			if got.delivery.FailReason != tc.wantReason {
				t.Errorf("fail reason = %q, want %q", got.delivery.FailReason, tc.wantReason)
			}
		})
	}
}

// TestASpacelessSSEUpstreamIsStillReadAsComplete pins the framing an Aliyun
// Anthropic-compatible upstream actually sends: `event:message_start` and
// `data:{...}` with no space after either colon. SSE makes that space optional,
// and isDataLine already honours that — so the frames commit the response and
// are forwarded byte for byte. The decoder owes the same answer: a stream whose
// every frame it quietly dropped would settle as stream_ended_unannounced with
// no usage, which is a completed, billed delivery filed as a partial failure.
func TestASpacelessSSEUpstreamIsStillReadAsComplete(t *testing.T) {
	const upstream = "event:ping\n" +
		`data:{"type":"ping"}` + "\n\n" +
		"event:message_start\n" +
		`data:{"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":7,"output_tokens":0}}}` + "\n\n" +
		"event:content_block_delta\n" +
		`data:{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event:message_delta\n" +
		`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event:message_stop\n" +
		`data:{"type":"message_stop"}` + "\n\n"

	// The framing the caller received is the provider's own, spaceless; only
	// message_start is rewritten (and re-serialised with sorted keys).
	const forwarded = "event:ping\n" +
		`data:{"type":"ping"}` + "\n\n" +
		"event:message_start\n" +
		`data:{"message":{"id":"msg_1","model":"claude-3-5-sonnet","usage":{"input_tokens":7,"output_tokens":0}},"type":"message_start"}` + "\n\n" +
		"event:content_block_delta\n" +
		`data:{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event:message_delta\n" +
		`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
		"event:message_stop\n" +
		`data:{"type":"message_stop"}` + "\n\n"

	got := runStream(t, protocols.ProtocolClaude, protocols.ProtocolClaude, true, "/v1/messages",
		`{"model":"claude-3-5-sonnet","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		upstreamStream(t, upstream))
	requireDelivered(t, got)
	checkStream(t, streamWant{
		clientBody:    forwarded,
		captureExists: true, firstByteSent: true, promptSeen: 7, completion: 3,
		clientStatus: http.StatusOK, verdict: fact.VerdictSettled, committed: true, complete: true,
	}, got)
}

// TestAStreamThatNeverSendsDataLeavesTheChainOpen pins the failover window.
//
// An upstream can answer 2xx and then die before its first data frame. Nothing
// has reached the caller at that point, which is the whole reason the response
// is not committed up front — and it means this candidate can still be replaced
// by a healthy one. Reporting it as settled would turn a recoverable provider
// failure into a failed request.
func TestAStreamThatNeverSendsDataLeavesTheChainOpen(t *testing.T) {
	const caller = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`

	got := runStream(t, protocols.ProtocolOpenAI, protocols.ProtocolOpenAI, true, "/v1/chat/completions", caller, upstreamStream(t, ""))
	checkStream(t, streamWant{
		clientStatus: http.StatusOK, verdict: fact.VerdictNextCandidate,
	}, got)

	if got.captureExists {
		t.Errorf("a capture file was left behind holding %q; nothing was ever sent, and an empty one renders as a capture worth opening", got.captured)
	}
}

// brokenAfter hands over its bytes and then fails, the way a provider that
// closes its connection non-standardly does. A buffer cannot do this, and a
// buffer is what every other fixture here is — which is why the branch below
// went unguarded: it is only read when the read itself failed.
type brokenAfter struct {
	data []byte
	err  error
}

func (r *brokenAfter) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func upstreamStreamThatBreaks(t *testing.T, body string, err error) *http.Response {
	t.Helper()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(&brokenAfter{data: []byte(body), err: err}),
	}
}

// TestWhatCountsAsAWholeStreamDiffersByProtocol pins the one thing the two
// pumps deliberately disagree about.
//
// Both forgive a transport error that arrives after the response was already
// whole — an upstream closing badly on its way out interrupts nothing. They do
// not agree on what whole means. OpenAI reports usage in a frame of its own, so
// a stream that reached the terminator without one stopped short; the other
// protocols carry usage inside their terminal event, and demanding it
// separately would file finished streams as broken.
//
// Both fixtures deliberately omit usage. That is the case where one condition
// for both would quietly change an answer.
func TestWhatCountsAsAWholeStreamDiffersByProtocol(t *testing.T) {
	const openAIStream = `data: {"id":"c1","model":"gpt-4o-real","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	const openAICaller = `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	const claudeCaller = `{"model":"claude-3-5-sonnet","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	claudeStream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude-3-5-sonnet-20241022"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		// The completion signal, carrying no usage: that omission is the whole
		// point of this fixture.
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	t.Run("openai wants its usage frame", func(t *testing.T) {
		got := runStream(t, protocols.ProtocolOpenAI, protocols.ProtocolOpenAI, true, "/v1/chat/completions", openAICaller,
			upstreamStreamThatBreaks(t, openAIStream, io.ErrUnexpectedEOF))
		// The caller already holds a terminator, and then gets a second one
		// after the error frame: the stream is closed off the only way SSE
		// allows once bytes are out.
		checkStream(t, streamWant{
			clientBody: `data: {"choices":[{"delta":{"content":"hi"}}],"id":"c1","model":"gpt-4o"}` + "\n\n" +
				"data: [DONE]\n\n" +
				`data: {"error":{"message":"upstream stream interrupted (request: ` + streamRequestID + `)","type":"upstream_error"}}` + "\n\n" +
				"data: [DONE]\n\n",
			captureExists: true, firstByteSent: true,
			clientStatus: http.StatusOK, verdict: fact.VerdictSettled, committed: true, complete: false,
		}, got)

		if got.delivery.Fault != fact.FaultUpstream {
			t.Errorf("fault = %v, want upstream", got.delivery.Fault)
		}
	})

	t.Run("claude does not", func(t *testing.T) {
		got := runStream(t, protocols.ProtocolClaude, protocols.ProtocolClaude, true, "/v1/messages", claudeCaller,
			upstreamStreamThatBreaks(t, claudeStream, io.ErrUnexpectedEOF))
		// No error frame: this stream reached its terminal event, so the read
		// failure that followed interrupted nothing.
		checkStream(t, streamWant{
			clientBody: "event: message_start\n" +
				`data: {"message":{"id":"msg_1","model":"claude-3-5-sonnet"},"type":"message_start"}` + "\n\n" +
				"event: content_block_delta\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
				"event: message_delta\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
				"event: message_stop\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			captureExists: true, firstByteSent: true,
			clientStatus: http.StatusOK, verdict: fact.VerdictSettled, committed: true, complete: true,
		}, got)
	})
}
