package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yolorouter/yolorouter/internal/fact"
	"github.com/yolorouter/yolorouter/internal/protocols"
)

// Spoken audio is the payload that inverts a rule the rest of the gateway
// follows. Everywhere else, response headers are staged optimistically and
// taken back if the delivery turns out badly. Here they cannot be: whether the
// upstream actually produced audio is not known from its status — a provider
// can answer 200 with a JSON error, or with a signed URL that has to be
// fetched before anything exists to send — so headers go out only once there is
// something to send them with, and there is nothing to take back.
//
// Two more properties come with it: the bytes the caller receives may not be
// the bytes the upstream sent, and neither may go in a log.

// fakeAudioPayload delivers audio in one of two shapes, chosen by the upstream.
type fakeAudioPayload struct {
	fakePayloadBase

	characters int

	// injectedAtCommit records whether headers were staged at all. It is the
	// observable form of the policy: the point is not that Inject is called, it
	// is that it is called late.
	injectedAtCommit bool
	// bytesForwarded counts what reached the caller. A progressive delivery and
	// a buffered one differ in nothing an assertion can see EXCEPT when the
	// caller first heard something, which is what the reader below records.
	bytesForwarded int
}

// audioContentType is what a served audio response is announced as. Getting it
// onto a response that turned out to be a JSON error is precisely the failure
// the late injection avoids.
const audioContentType = "audio/mpeg"

// signedURLResponse is the indirect shape: the upstream answers with a link,
// not with audio.
type signedURLResponse struct {
	AudioURL string `json:"audio_url"`
}

// Deliver forwards the audio as it arrives, committing once the first bytes
// prove there is audio to forward.
//
// Progressive, not buffered, and the two are not interchangeable here. A minute
// of speech is megabytes that arrive over seconds; reading it whole before
// writing anything means the caller waits for all of it before hearing any of
// it, and the gateway holds the whole thing in memory to achieve that. Which is
// also where the inverted header policy comes from: the commit has to happen
// after SOMETHING has been checked and before EVERYTHING has arrived, so the
// only thing available to check is the first read.
func (p *fakeAudioPayload) Deliver(tools DeliveryTools, resp *http.Response) fact.Delivery {
	src, err := p.audioStream(tools, resp)
	if err != nil {
		// Nothing has been staged and nothing committed, so this candidate can
		// still be replaced. That is only true BECAUSE the headers were not
		// staged up front: a response already announced as audio/mpeg is one
		// the caller has been told about.
		return fact.HandedOn(fact.FaultUpstream, "no_audio: "+err.Error(), err)
	}
	defer func() { _ = src.Close() }()

	// The usage is what the provider was asked to synthesise, and it is incurred
	// the moment they start producing. Built once, up front, so every exit below
	// carries it: a caller who hangs up mid-sentence still cost us the audio.
	// Count carries the counted-unit quantity (the image precedent); Source
	// says the gateway derived it from the request rather than reading it off
	// an upstream usage field.
	usage := &fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromRequest, Count: p.characters}

	buf := make([]byte, 8)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			tools.Capture.Upstream(chunk)
			if !tools.Client.Committed() {
				// The first bytes are the whole check. There is nothing else to
				// validate — no frame, no envelope, no terminator — and waiting
				// for more would be waiting for the entire response.
				if len(chunk) == 0 {
					return fact.HandedOn(fact.FaultUpstream, "empty_audio", nil)
				}
				tools.Client.Inject(http.Header{"Content-Type": {audioContentType}})
				p.injectedAtCommit = true
				if cerr := tools.Client.Commit(http.StatusOK); cerr != nil {
					return fact.Undelivered(http.StatusInternalServerError, fact.VerdictSettled,
						fact.FaultGateway, "commit_failed: "+cerr.Error(), cerr).WithUsage(usage)
				}
			}
			if _, werr := tools.Client.Write(chunk); werr != nil {
				return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_write", werr).WithUsage(usage)
			}
			if ferr := tools.Client.Flush(); ferr != nil {
				return fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_flush", ferr).WithUsage(usage)
			}
			p.bytesForwarded += n
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if !tools.Client.Committed() {
				return fact.HandedOn(fact.FaultUpstream, "audio_read: "+rerr.Error(), rerr).WithUsage(usage)
			}
			return fact.Truncated(http.StatusOK, http.StatusInternalServerError,
				fact.FaultUpstream, "audio_cut_short", rerr).WithUsage(usage)
		}
	}
	if !tools.Client.Committed() {
		return fact.HandedOn(fact.FaultUpstream, "empty_audio", nil).WithUsage(usage)
	}
	return fact.Succeeded(http.StatusOK).WithUsage(usage)
}

// audioStream resolves whatever the upstream sent into a reader of audio.
//
// The indirect shape is the one that cannot be progressive on the upstream's
// terms: a link has to be read whole before it can be followed, and only what
// comes back from following it is audio. Both shapes end up as a reader so the
// forwarding loop above does not have to know which happened.
func (p *fakeAudioPayload) audioStream(tools DeliveryTools, resp *http.Response) (io.ReadCloser, error) {
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "audio/") {
		return resp.Body, nil
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	tools.Capture.Upstream(body)

	var indirect signedURLResponse
	if err := json.Unmarshal(body, &indirect); err != nil || indirect.AudioURL == "" {
		// A 200 carrying JSON that is not a link is the provider's error
		// envelope. Status alone would have called this a success.
		return nil, errors.New("upstream sent no audio and no link")
	}
	audio, err := tools.Fetch.Fetch(context.Background(), indirect.AudioURL, FetchPolicy{
		AllowedHosts: []string{"cdn.fake-provider.invalid"},
	})
	if err != nil {
		return nil, err
	}
	if len(audio) == 0 {
		return nil, errors.New("the link produced no audio")
	}
	return io.NopCloser(bytes.NewReader(audio)), nil
}

// FinalizeUsage counts characters, not tokens, and counts them whatever became
// of the delivery.
//
// The unit travels with the numbers, and settlement prices it when the
// settling candidate declares audio billing — that dispatch and its unit
// guard are tested in pricing_audio_test.go.
// Completeness is deliberately not consulted: the provider synthesised the
// audio and charged for it before any of this gateway's problems began, and a
// modality that withheld usage on an incomplete delivery would be deciding a
// refund policy from inside a method that only reports quantities.
func (p *fakeAudioPayload) FinalizeUsage(d fact.Delivery) *fact.UsageReported {
	if d.Usage != nil {
		return d.Usage
	}
	return &fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromRequest, Count: p.characters}
}

// LogPolicy stores the caller's request and nothing else. The request is text;
// both response bodies are audio.
func (p *fakeAudioPayload) LogPolicy() LogPolicy {
	return LogPolicy{Store: map[BodyKind]BodyStorage{BodyClientRequest: BodyStoredRaw}}
}

// SanitizeForLog renders audio as a digest.
//
// The bytes cannot be stored — they are megabytes of binary — but dropping them
// silently leaves no way to tell a response that was never recorded from one
// that never happened. A digest and a length answer "was it the same audio as
// last time" without keeping any of it.
func (p *fakeAudioPayload) SanitizeForLog(k BodyKind, _ string, body []byte) string {
	if k == BodyClientRequest {
		return string(body)
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("<audio %d bytes sha256:%s>", len(body), hex.EncodeToString(sum[:])[:16])
}

// fakeAudioModality is the stateless half.
type fakeAudioModality struct{}

func (fakeAudioModality) ID() ModalityID         { return ModalityID("fake_audio") }
func (fakeAudioModality) Limits() TransferLimits { return TransferLimits{} }

func (fakeAudioModality) Admit(_ context.Context, in Ingress) (Payload, *Rejection) {
	var req struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(in.Body, &req); err != nil || req.Input == "" {
		return nil, &Rejection{
			Status: http.StatusBadRequest, ErrorType: "invalid_request_error",
			Message: "input is required", FailReason: "no_input", Fault: fact.FaultClient,
		}
	}
	return &fakeAudioPayload{characters: len(req.Input)}, nil
}

// stubFetcher stands in for the secondary fetch. The real one refuses private
// addresses, which is what makes it worth having and also what makes it
// unusable against a test server; its own guarantees are tested where it lives.
type stubFetcher struct {
	body       []byte
	err        error
	sawURL     string
	sawPolicy  FetchPolicy
	fetchCount int
}

func (f *stubFetcher) Fetch(_ context.Context, rawURL string, p FetchPolicy) ([]byte, error) {
	f.fetchCount++
	f.sawURL = rawURL
	f.sawPolicy = p
	return f.body, f.err
}

// deliverFakeAudio runs one audio delivery and reports what the caller got.
func deliverFakeAudio(t *testing.T, callerBody string, upstream *http.Response, fetch SecondaryFetcher) (fact.Delivery, *httptest.ResponseRecorder, *fakeAudioPayload) {
	t.Helper()
	payload, rej := fakeAudioModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/audio/speech",
		ContentType: "application/json", Body: []byte(callerBody),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid request: %+v", rej)
	}

	tools, release, w := fakeDelivery{path: "/v1/audio/speech", requestID: "req-fake-audio"}.tools(t)
	defer release()
	if fetch != nil {
		tools.Fetch = fetch
	}
	d := payload.Deliver(tools, upstream)
	return d, w, payload.(*fakeAudioPayload)
}

func audioUpstream(contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestAudioHeadersGoOutOnlyOnceThereIsAudioToSendThemWith is the inverted
// policy, stated as the thing that goes wrong without it.
//
// This upstream answers 200 with JSON — a provider error wearing a success
// status. Stage the audio content type up front and the caller is told they are
// receiving an MP3 before anybody has checked; the delivery then has to take
// the headers back, and the take-back is a step that can be forgotten. Staging
// late means there is nothing to forget.
func TestAudioHeadersGoOutOnlyOnceThereIsAudioToSendThemWith(t *testing.T) {
	d, w, payload := deliverFakeAudio(t,
		`{"input":"hello"}`,
		audioUpstream("application/json", `{"error":{"message":"model overloaded"}}`),
		nil)

	if d.Committed {
		t.Fatal("a response went out for an upstream that produced no audio")
	}
	if d.Verdict != fact.VerdictNextCandidate {
		t.Errorf("verdict = %v, want the chain to continue: nothing reached the caller, "+
			"so another provider can still serve this", d.Verdict)
	}
	if payload.injectedAtCommit {
		t.Error("headers were staged for a delivery that never had audio")
	}
	if got := w.Header().Get("Content-Type"); strings.HasPrefix(got, "audio/") {
		t.Errorf("Content-Type = %q, want no audio announcement on a response that carries none", got)
	}
	if payload.bytesForwarded != 0 {
		t.Errorf("%d bytes reached the caller from a response that carried no audio; "+
			"nothing is taken back once it has been written", payload.bytesForwarded)
	}
}

// TestAudioThatArrivesAsALinkIsFetchedBeforeTheCallerIsCommitted pins the
// second property: the upstream response is not always the caller's response.
//
// The provider answers with a URL. Nothing exists to send until that URL has
// been fetched, which is another chance for the delivery to fail — after a 200
// from the provider and before a single byte to the caller. A modality that
// committed on the provider's status would have announced a success it then
// could not produce.
func TestAudioThatArrivesAsALinkIsFetchedBeforeTheCallerIsCommitted(t *testing.T) {
	const audio = "ID3fake-mp3-bytes"
	fetch := &stubFetcher{body: []byte(audio)}

	d, w, _ := deliverFakeAudio(t,
		`{"input":"hello"}`,
		audioUpstream("application/json", `{"audio_url":"https://cdn.fake-provider.invalid/a.mp3"}`),
		fetch)

	if !d.Complete {
		t.Fatalf("delivery = %+v, want a complete one", d)
	}
	if fetch.fetchCount != 1 {
		t.Errorf("secondary fetches = %d, want 1", fetch.fetchCount)
	}
	if fetch.sawURL != "https://cdn.fake-provider.invalid/a.mp3" {
		t.Errorf("fetched %q, want the link the upstream gave", fetch.sawURL)
	}
	if len(fetch.sawPolicy.AllowedHosts) == 0 {
		t.Error("the fetch was made with no host allowlist, which is the policy that permits nothing " +
			"or everything depending on how the fetcher reads it — a modality must name its hosts")
	}
	if got := w.Body.String(); got != audio {
		t.Errorf("caller received %q, want the fetched audio: the bytes the caller gets are not "+
			"the bytes the upstream sent, and this is the seam that has to allow for it", got)
	}
	if got := w.Header().Get("Content-Type"); got != audioContentType {
		t.Errorf("Content-Type = %q, want %q", got, audioContentType)
	}
}

// TestALinkThatCannotBeFetchedLeavesTheChainOpen is why the fetch happening
// before the commit matters at all.
func TestALinkThatCannotBeFetchedLeavesTheChainOpen(t *testing.T) {
	fetch := &stubFetcher{err: errors.New("cdn refused")}

	d, w, _ := deliverFakeAudio(t,
		`{"input":"hello"}`,
		audioUpstream("application/json", `{"audio_url":"https://cdn.fake-provider.invalid/a.mp3"}`),
		fetch)

	if d.Committed || d.Verdict != fact.VerdictNextCandidate {
		t.Errorf("delivery = %+v, want nothing committed and the chain still open", d)
	}
	// The reason has to survive too. It is the only record of WHY the link could
	// not be fetched, and a delivery that reports the right verdict under an
	// empty reason leaves an operator with a failed request and no lead.
	if !strings.Contains(d.FailReason, "cdn refused") {
		t.Errorf("FailReason = %q, want it to carry the fetch failure", d.FailReason)
	}
	if w.Body.Len() != 0 {
		t.Errorf("caller received %d bytes for audio that was never fetched", w.Body.Len())
	}
}

// TestAudioIsRecordedAsADigestNeverAsBytes pins the third property.
//
// A logging capability cannot know that these bytes are unstorable, and it must
// not have to. The modality is asked, and what it returns is what is kept.
func TestAudioIsRecordedAsADigestNeverAsBytes(t *testing.T) {
	const audio = "ID3fake-mp3-bytes"
	payload, rej := fakeAudioModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/audio/speech",
		ContentType: "application/json", Body: []byte(`{"input":"hello"}`),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid request: %+v", rej)
	}

	if payload.LogPolicy().Storage(BodyUpstreamResponse) != BodyDropped || payload.LogPolicy().Storage(BodyClientResponse) != BodyDropped {
		t.Error("this modality's policy admits an audio body into storage")
	}
	if payload.LogPolicy().Storage(BodyClientRequest) == BodyDropped {
		t.Error("the caller's request is text and is what an operator reads first; it must be kept")
	}

	got := payload.SanitizeForLog(BodyUpstreamResponse, audioContentType, []byte(audio))
	if strings.Contains(got, audio) {
		t.Errorf("sanitized = %q, want no audio bytes in it", got)
	}
	if !strings.Contains(got, fmt.Sprintf("%d bytes", len(audio))) || !strings.Contains(got, "sha256:") {
		t.Errorf("sanitized = %q, want a length and a digest: a body reduced to nothing at all "+
			"is indistinguishable from one that never arrived", got)
	}
}

// slowAudio hands out its bytes a chunk at a time and records what the caller
// had received by the time each chunk was read.
//
// That record is the only thing that tells a progressive delivery from a
// buffered one. Both end with the same bytes in the same recorder; they differ
// in WHEN, and a test that only looks at the end cannot see it.
type slowAudio struct {
	chunks    []string
	sent      int
	forwarded func() int
	// forwardedAtRead[i] is how much the caller held when chunk i was handed
	// over. A buffered delivery leaves every entry at zero.
	forwardedAtRead []int
}

func (s *slowAudio) Read(p []byte) (int, error) {
	if s.sent >= len(s.chunks) {
		return 0, io.EOF
	}
	s.forwardedAtRead = append(s.forwardedAtRead, s.forwarded())
	n := copy(p, s.chunks[s.sent])
	s.sent++
	return n, nil
}

func (s *slowAudio) Close() error { return nil }

// TestAudioIsForwardedAsItArrivesRatherThanAfterItAllHas is the property the
// inverted header policy exists to serve, and the one an assertion about the
// final bytes cannot see.
//
// A minute of speech is megabytes arriving over seconds. Buffered, the caller
// hears nothing until the last byte lands and the gateway holds all of it in
// memory to arrange that. It also removes the only thing the commit can be
// based on: if everything has already arrived, the "commit after the first byte
// is validated" rule has nothing left to mean.
func TestAudioIsForwardedAsItArrivesRatherThanAfterItAllHas(t *testing.T) {
	payload, rej := fakeAudioModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/audio/speech",
		ContentType: "application/json", Body: []byte(`{"input":"hello"}`),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid request: %+v", rej)
	}
	fake := payload.(*fakeAudioPayload)

	src := &slowAudio{chunks: []string{"ID3first", "second", "third"}}
	src.forwarded = func() int { return fake.bytesForwarded }

	tools, release, w := fakeDelivery{
		path: "/v1/audio/speech", requestID: "req-progressive-audio", isStream: true,
	}.tools(t)
	d := payload.Deliver(tools, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {audioContentType}},
		Body:       src,
	})
	release()

	if !d.Complete {
		t.Fatalf("delivery = %+v, want a complete one", d)
	}
	if len(src.forwardedAtRead) < 3 {
		t.Fatalf("the upstream was read %d times, want one read per chunk", len(src.forwardedAtRead))
	}
	if src.forwardedAtRead[0] != 0 {
		t.Errorf("the caller already had %d bytes before the first chunk was read", src.forwardedAtRead[0])
	}
	if src.forwardedAtRead[len(src.forwardedAtRead)-1] == 0 {
		t.Error("the caller had received nothing by the time the last chunk was read: this response " +
			"was buffered whole before any of it went out, which is the thing progressive delivery is not")
	}
	if got := w.Body.String(); got != "ID3firstsecondthird" {
		t.Errorf("caller received %q, want every chunk in order", got)
	}
}

// TestAudioAlreadySynthesisedIsBilledEvenIfTheCallerNeverHearsIt pins billing
// against completeness.
//
// The provider produced the audio and charged for it before anything went wrong
// on our side of the wire. Dropping the usage because the delivery came out
// incomplete is a refund — decided inside a method that is supposed to report
// quantities, for a case the design deliberately leaves out of the billing
// condition until there is data to decide it on.
func TestAudioAlreadySynthesisedIsBilledEvenIfTheCallerNeverHearsIt(t *testing.T) {
	payload, rej := fakeAudioModality{}.Admit(context.Background(), Ingress{
		Protocol: protocols.ProtocolOpenAI, Path: "/v1/audio/speech",
		ContentType: "application/json", Body: []byte(`{"input":"hello"}`),
	})
	if rej != nil {
		t.Fatalf("Admit refused a valid request: %+v", rej)
	}

	// A caller who left: the delivery is incomplete and blamed on them.
	aborted := fact.Truncated(http.StatusOK, 499, fact.FaultClient, "client_write", errors.New("broken pipe")).
		WithUsage(&fact.UsageReported{Unit: fact.UnitCharacter, Source: fact.UsageFromRequest, Count: len("hello")})

	got := payload.FinalizeUsage(aborted)

	if got == nil {
		t.Fatal("no usage reported for audio the provider had already synthesised and charged for")
	}
	if got.Count != len("hello") || got.Unit != fact.UnitCharacter {
		t.Errorf("usage = %+v, want the characters that were synthesised", got)
	}
}

// TestAudioReportsItsUnitAlongsideItsCount pins what a modality is answerable
// for: stating what it counted, in the unit it counted it in.
//
// The name says "reports", not "is billed", and the difference is the subject
// of the test below it. Everything asserted here happens inside the delivery.
func TestAudioReportsItsUnitAlongsideItsCount(t *testing.T) {
	d, _, _ := deliverFakeAudio(t,
		`{"input":"hello"}`,
		audioUpstream(audioContentType, "ID3fake-mp3-bytes"),
		nil)

	if d.Usage == nil {
		t.Fatal("a served audio response reported no usage at all")
	}
	if d.Usage.Unit != fact.UnitCharacter {
		t.Errorf("unit = %v, want characters", d.Usage.Unit)
	}
	if d.Usage.Count != len("hello") {
		t.Errorf("counted %d, want %d characters of input", d.Usage.Count, len("hello"))
	}
}
