package router

// Streaming-channel tests for the console Playground run endpoint: the
// aggregate channel's concurrent, per-column behaviour. Columns run at the
// same time and forward their text as it is generated, a slow column
// cannot hold a fast one back, a failing column keeps its native error
// shape on its own slot while the others finish, vision attachments reach
// the upstream in their real encoding, and a console disconnect propagates
// into the in-flight columns (observed at the upstream itself) and is
// acknowledged per slot on the way out.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// sseChunkPayload is the slice of one column's chunk events, with the
// fields the slot-routing contract carries.
type sseChunkPayload struct {
	Slot  int    `json:"slot"`
	Model string `json:"model"`
	Delta string `json:"delta"`
}

// sseDonePayload is a done event's settled reading.
type sseDonePayload struct {
	Slot       int    `json:"slot"`
	Status     int    `json:"status"`
	RequestID  string `json:"request_id"`
	CostMicros int64  `json:"cost_micros"`
}

// sseErrorPayload is an error event's native failure shape.
type sseErrorPayload struct {
	Slot   int `json:"slot"`
	Status int `json:"status"`
	Error  struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// sseCancelPayload is a cancel acknowledgement.
type sseCancelPayload struct {
	Slot      int    `json:"slot"`
	RequestID string `json:"request_id"`
}

// eventsOfKind returns the data payloads of every event with the given
// name, in stream order.
func eventsOfKind(t *testing.T, w *httptest.ResponseRecorder, kind string) []string {
	t.Helper()
	var out []string
	for _, ev := range sseEvents(t, w.Body.String()) {
		if ev.Event == kind {
			out = append(out, ev.Data)
		}
	}
	return out
}

// parseAs unmarshals one event payload into dst.
func parseAs(t *testing.T, data string, dst any) {
	t.Helper()
	if err := json.Unmarshal([]byte(data), dst); err != nil {
		t.Fatalf("event payload not JSON: %v (%s)", err, data)
	}
}

// streamTextUpstream stands in for an upstream that answers an
// OpenAI-shaped chat stream: the given text deltas as separate chunks,
// a terminal usage frame of 5 input + 3 output tokens (11 micros at the
// seeded rates), and [DONE].
func streamTextUpstream(t *testing.T, deltas ...string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range deltas {
			_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{"content":"` + d + `"}}]}` + "\n\n"))
		}
		_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// TestPlaygroundColumnsStreamConcurrentlyAndRouteBySlot: three columns run
// at the same time over a REAL console socket, read incrementally the way
// the page reads the channel. The slow column's upstream is gated by the
// test itself: it answers nothing until the test has OBSERVED both fast
// columns' done events on the wire. Those done events arriving while the
// slow upstream has still answered nothing is the no-drag proof — under
// any sequential implementation the slow column (slot 1) would hold slot 2
// back, no done for slot 2 could ever arrive first, and the test's wait
// guard would fail it. Slot routing, per-slot ordering, and multi-chunk
// deltas are asserted on the same run.
func TestPlaygroundColumnsStreamConcurrentlyAndRouteBySlot(t *testing.T) {
	db := testutil.NewSQLiteDB(t)

	fastStream := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{"content":"fast"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	fastA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fastStream(w) }))
	fastB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fastStream(w) }))
	t.Cleanup(fastA.Close)
	t.Cleanup(fastB.Close)
	slowEntered := make(chan struct{}, 4)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowEntered <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{"content":"slow-"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{"content":"mo"}}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"id":"c","model":"up-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		slow.Close()
	})

	seedPlaygroundUpstreamModel(t, db, "pg-fast-a", "m-fast-a", "m-fast-a-real", fastA.URL)
	seedPlaygroundUpstreamModel(t, db, "pg-slow", "m-slow", "m-slow-real", slow.URL)
	seedPlaygroundUpstreamModel(t, db, "pg-fast-b", "m-fast-b", "m-fast-b-real", fastB.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-stream-conc-0001", nil)

	r := newRelayTestRouter(t, db)
	console := httptest.NewServer(r)
	t.Cleanup(console.Close)

	body := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["m-fast-a","m-slow","m-fast-b"],"request":{"messages":[{"role":"user","content":"ping"}]}}`, key.ID)
	req, _ := http.NewRequest(http.MethodPost, console.URL+"/api/admin/playground/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	resp, err := console.Client().Do(req)
	if err != nil {
		t.Fatalf("console request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d", resp.StatusCode)
	}

	// Read the aggregate channel frame by frame, as the page does.
	frames := make(chan sseEvent, 64)
	go func() {
		defer close(frames)
		var event, data string
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && event != "":
				frames <- sseEvent{event, data}
				event, data = "", ""
			}
		}
	}()

	// The no-drag leg: both FAST columns must reach done — observed on the
	// wire — while the slow upstream has still answered nothing. The slow
	// upstream entered (its column is genuinely in flight) before the wait.
	fastDone := map[int]bool{}
	var all []sseEvent
	waitFast := time.After(15 * time.Second)
	for len(fastDone) < 2 {
		select {
		case ev, ok := <-frames:
			if !ok {
				t.Fatalf("channel closed before the fast columns retired; events so far: %v", all)
			}
			all = append(all, ev)
			var payload struct {
				Slot  int    `json:"slot"`
				Model string `json:"model"`
			}
			parseAs(t, ev.Data, &payload)
			if payload.Slot == 1 {
				t.Fatalf("the slow column spoke before both fast columns retired: %s %s", ev.Event, ev.Data)
			}
			if ev.Event == "done" {
				fastDone[payload.Slot] = true
			}
		case <-waitFast:
			t.Fatalf("fast columns did not retire while the slow upstream was gated; events so far: %v", all)
		}
	}
	// The slow column's upstream was entered and gated this whole time.
	select {
	case <-slowEntered:
	default:
		t.Fatal("the slow column never went in flight")
	}
	// Release the slow upstream and let the run finish.
	close(release)
	for doneSlow := false; !doneSlow; {
		ev, ok := <-frames
		if !ok {
			t.Fatalf("channel closed before the slow column retired; events so far: %v", all)
		}
		all = append(all, ev)
		if ev.Event == "done" {
			var payload struct {
				Slot int `json:"slot"`
			}
			parseAs(t, ev.Data, &payload)
			if payload.Slot == 1 {
				doneSlow = true
			}
		}
	}

	// Slot routing, per-slot ordering, and multi-chunk deltas over the
	// whole observed run.
	slotOfModel := map[string]int{"m-fast-a": 0, "m-slow": 1, "m-fast-b": 2}
	deltas := map[int]string{0: "", 1: "", 2: ""}
	terminalAt := map[int]int{}
	for i, ev := range all {
		var payload struct {
			Slot  int    `json:"slot"`
			Model string `json:"model"`
		}
		parseAs(t, ev.Data, &payload)
		if want, ok := slotOfModel[payload.Model]; !ok || payload.Slot != want {
			t.Fatalf("event %d (%s) names slot %d for model %q, want slot %d", i, ev.Event, payload.Slot, payload.Model, want)
		}
		if ev.Event == "chunk" {
			if _, terminal := terminalAt[payload.Slot]; terminal {
				t.Fatalf("chunk after the terminal event on slot %d: %s", payload.Slot, ev.Data)
			}
			var chunk sseChunkPayload
			parseAs(t, ev.Data, &chunk)
			deltas[chunk.Slot] += chunk.Delta
		}
		if ev.Event == "done" || ev.Event == "error" || ev.Event == "cancel_ack" {
			if _, terminal := terminalAt[payload.Slot]; !terminal {
				terminalAt[payload.Slot] = i
			}
		}
	}
	if len(terminalAt) != 3 {
		t.Fatalf("every column must reach exactly one terminal event, got %v over %d events", terminalAt, len(all))
	}
	if deltas[1] != "slow-mo" {
		t.Fatalf("slow column deltas concatenated to %q, want slow-mo", deltas[1])
	}
	if deltas[0] != "fast" || deltas[2] != "fast" {
		t.Fatalf("fast column deltas = %q / %q, want fast / fast", deltas[0], deltas[2])
	}
}

// TestPlaygroundStreamingFailureIsolation: a column whose upstream refuses
// keeps the refusal's native shape on its own slot while the other columns
// stream and complete, and the failed exchange lands in the audit trail
// like any other playground run.
func TestPlaygroundStreamingFailureIsolation(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	healthy := streamTextUpstream(t, "pong")
	seedPlaygroundUpstreamModel(t, db, "pg-h1", "m-h1", "m-h1-real", healthy.URL)
	seedPlaygroundUpstreamModel(t, db, "pg-h2", "m-h2", "m-h2-real", healthy.URL)
	seedPlaygroundUpstreamModel(t, db, "pg-quota", "m-quota", "m-quota-real", newQuotaFailingUpstream(t).URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-stream-fail-001", nil)

	body := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["m-h1","m-quota","m-h2"],"request":{"messages":[{"role":"user","content":"ping"}]}}`, key.ID)
	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}

	// The two healthy columns completed with their streamed text; the
	// failed column's own slot carries the relay's native verdict for a
	// refusing upstream (429, rate_limit_error) — the exact shape a real
	// caller of the same ingress would receive.
	dones := eventsOfKind(t, w, "done")
	if len(dones) != 2 {
		t.Fatalf("expected the two healthy columns to complete, got %d done events: %s", len(dones), w.Body.String())
	}
	for _, data := range dones {
		var done sseDonePayload
		parseAs(t, data, &done)
		if done.Slot != 0 && done.Slot != 2 {
			t.Fatalf("healthy done on slot %d, want 0 or 2: %s", done.Slot, data)
		}
	}
	errors := eventsOfKind(t, w, "error")
	if len(errors) != 1 {
		t.Fatalf("expected exactly one error event, got %d: %s", len(errors), w.Body.String())
	}
	var failed sseErrorPayload
	parseAs(t, errors[0], &failed)
	if failed.Slot != 1 || failed.Status != http.StatusTooManyRequests || failed.Error.Type != "rate_limit_error" {
		t.Fatalf("failed column must be slot 1 with the native 429 rate_limit_error, got slot %d status %d type %q",
			failed.Slot, failed.Status, failed.Error.Type)
	}
	if failed.Error.Message == "" {
		t.Fatalf("the native error message must survive to the column: %s", errors[0])
	}

	// The failed exchange is in the audit trail under the playground
	// source — a failed comparison is as visible as a successful one.
	var failedRow model.RequestLog
	if err := db.Where("source = ? AND status_code = ?", "playground", http.StatusTooManyRequests).
		Order("id DESC").First(&failedRow).Error; err != nil {
		t.Fatalf("expected a tagged 429 playground row for the refused column: %v", err)
	}
}

// visionImagePart builds one OpenAI vision content array: a text turn plus
// one attached image as a base64 data URL — the real encoding the page
// produces.
func visionImagePart(dataURL string) string {
	return fmt.Sprintf(`[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":%q}}]`, dataURL)
}

// TestPlaygroundVisionAttachmentReachesUpstream: an attached image travels
// to the upstream in its real encoding — a data URL inside an image_url
// content part, bytes intact — alongside the text turn, and the column
// still streams and settles through the whole chain.
func TestPlaygroundVisionAttachmentReachesUpstream(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, bodies := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-vision-key-0001", nil)

	const dataURL = "data:image/png;base64,aGVsbG8gdmlzaW9uIGF0dGFjaG1lbnQ="
	body := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["gpt-4o"],"request":{"messages":[{"role":"user","content":%s}]}}`,
		key.ID, visionImagePart(dataURL))
	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", body)
	if w.Code != http.StatusOK {
		t.Fatalf("expected the stream to start, got %d body %s", w.Code, w.Body.String())
	}
	if dones := eventsOfKind(t, w, "done"); len(dones) != 1 {
		t.Fatalf("the vision column must complete: %s", w.Body.String())
	}
	if len(*bodies) != 1 {
		t.Fatalf("expected one upstream request, got %d", len(*bodies))
	}
	saw := (*bodies)[0]
	for _, want := range []string{
		`"type":"text","text":"what is this"`,
		`"type":"image_url"`,
		`"image_url":{"url":"` + dataURL + `"}`, // the attachment, byte for byte
		`"stream":true`,                         // the channel's own transport mode
	} {
		if !strings.Contains(saw, want) {
			t.Errorf("upstream body missing %q; body: %s", want, saw)
		}
	}
}

// TestPlaygroundVisionAttachmentWhitelist: the attachment whitelist refuses
// what the page cannot produce — a remote URL (the provider would be asked
// to fetch an address the console never vouched for), an oversized data
// URL, and every smuggled content shape decodePlaygroundContent rejects —
// with the console envelope BEFORE the stream starts, so no column ever
// runs on a smuggled shape. Every arm pins the exact refusal message, so
// each 400 is proven to come from its own branch, not from an accidental
// bind failure that happens to look like one.
func TestPlaygroundVisionAttachmentWhitelist(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, bodies := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-visi-wl-key-001", nil)
	r := newRelayTestRouter(t, db)

	const tinyImage = "data:image/png;base64,aGVsbG8="
	fiveImages := "[" + strings.TrimSuffix(strings.Repeat(`{"type":"image_url","image_url":{"url":"`+tinyImage+`"}},`, 5), ",") + "]"
	oversized := "data:image/png;base64," + strings.Repeat("A", 8<<20+16)
	for _, arm := range []struct {
		label   string
		content string
		wantMsg string
	}{
		{"remote URL", `[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]`, "an attachment must be a base64 data:image/ URL"},
		{"plain data URL without base64", `[{"type":"image_url","image_url":{"url":"data:image/png,rawbytes"}}]`, "an attachment must be a base64 data:image/ URL"},
		{"oversized data URL", visionImagePart(oversized), "an attachment exceeds the 8388608-byte data URL cap"},
		{"content that is neither a string nor an array", `123`, "message content must be a string or an array of text/image_url parts"},
		{"empty string content", `""`, "message content must not be empty"},
		{"empty parts array", `[]`, "message content parts must not be empty"},
		{"text part without text", `[{"type":"text","text":""}]`, "a text part must carry text"},
		{"text part smuggling an image_url", `[{"type":"text","text":"what is this","image_url":{"url":"` + tinyImage + `"}}]`, "a text part cannot carry an image_url"},
		{"image_url part smuggling text", `[{"type":"image_url","image_url":{"url":"` + tinyImage + `"},"text":"what is this"}]`, "an image_url part cannot carry text"},
		{"image_url part without a url", `[{"type":"image_url"}]`, "an image_url part must carry image_url.url"},
		{"part with an unknown type", `[{"type":"video_url","video_url":{"url":"https://example.com/v.mp4"}}]`, `a content part must be of type text or image_url, got "video_url"`},
		{"part without a type", `[{"text":"what is this"}]`, `a content part must be of type text or image_url, got ""`},
		{"fifth attachment in one message", fiveImages, "at most 4 attachments per message"},
	} {
		body := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["gpt-4o"],"request":{"messages":[{"role":"user","content":%s}]}}`, key.ID, arm.content)
		w := postPlaygroundRun(r, "tok-pg-member", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected a 400 console envelope, got %d body %s", arm.label, w.Code, w.Body.String())
		}
		if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("%s: refusal must not open the stream", arm.label)
		}
		var envelope struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("%s: refusal body is not the console envelope: %v (%s)", arm.label, err, w.Body.String())
		}
		if envelope.Message != arm.wantMsg {
			t.Fatalf("%s: refusal message = %q, want %q — the arm must be refused by its own branch", arm.label, envelope.Message, arm.wantMsg)
		}
	}
	// The closedness leg: not one refused shape ever reached the upstream —
	// every refusal happened before any column ran.
	if len(*bodies) != 0 {
		t.Fatalf("a refused shape reached the upstream (%d requests); the whitelist must refuse before any column runs", len(*bodies))
	}
}

// TestPlaygroundRunAcceptsAttachmentBodies: the run endpoint's body cap is
// its own (16 MiB), wider than the console-wide 1 MiB — a run carrying a
// megabyte-scale attachment is accepted, not 413'd. This is the pin for
// the endpoint's dedicated route group; moving the registration back under
// the 1 MiB admin group turns this red.
func TestPlaygroundRunAcceptsAttachmentBodies(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstream, _ := newCapturingUpstream(t)
	seedRelayableModel(t, db, upstream.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-bodycap-key-001", nil)

	// ~1.2 MiB of attachment: over the console-wide 1 MiB cap, well under
	// the per-image validation cap.
	bigURL := "data:image/png;base64," + strings.Repeat("Q", 1<<20+200*1024)
	body := fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["gpt-4o"],"request":{"messages":[{"role":"user","content":%s}]}}`,
		key.ID, visionImagePart(bigURL))
	r := newRelayTestRouter(t, db)
	w := postPlaygroundRun(r, "tok-pg-member", body)
	if w.Code != http.StatusOK {
		t.Fatalf("a run with a megabyte-scale attachment must be accepted, got %d body %s", w.Code, w.Body.String())
	}
	if dones := eventsOfKind(t, w, "done"); len(dones) != 1 {
		t.Fatalf("the attachment run must complete: %s", w.Body.String())
	}
}

// cancelObservingUpstream stands in for an upstream that never answers and
// records whether the request it received was aborted underneath it — the
// direct observation that a console disconnect propagated through the relay
// to the in-flight upstream connection.
func cancelObservingUpstream(t *testing.T) (*httptest.Server, *atomic.Int32, chan struct{}) {
	t.Helper()
	entered := make(chan struct{}, 8)
	aborted := &atomic.Int32{}
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		// Read the body, as a real provider does: net/http only begins
		// propagating a client disconnect to the server's request context
		// once the request body has been consumed, so a probe that never
		// reads would never observe the abort this test exists to catch.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			aborted.Add(1)
		case <-release:
		}
	}))
	// The release arm exists so cleanup never waits on the very thing the
	// test measures: whether the caller's disconnect reached the upstream
	// connection. A column that never aborts would otherwise hold the
	// handler open and Server.Close would hang the whole suite on a test
	// that is already failing.
	t.Cleanup(func() { close(release); upstream.Close() })
	return upstream, aborted, entered
}

// TestPlaygroundDisconnectCancelsColumns: while two columns are in flight,
// the console caller disconnects — the aggregate channel's context cancel
// must propagate into both in-flight exchanges (each upstream observes its
// connection abort), each column answers with a best-effort cancel
// acknowledgement carrying its audit row's request id, and both aborts land
// in the audit trail as 499/client_disconnected under the playground
// source.
func TestPlaygroundDisconnectCancelsColumns(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	upstreamA, abortedA, enteredA := cancelObservingUpstream(t)
	upstreamB, abortedB, enteredB := cancelObservingUpstream(t)
	seedPlaygroundUpstreamModel(t, db, "pg-cx-a", "m-cx-a", "m-cx-a-real", upstreamA.URL)
	seedPlaygroundUpstreamModel(t, db, "pg-cx-b", "m-cx-b", "m-cx-b-real", upstreamB.URL)
	member := seedConsoleUser(t, db, "pg-member", model.RoleMember, "tok-pg-member")
	key, _ := seedPlaygroundKey(t, db, member.ID, "sk-yr-pg-cancel-key-0001", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/playground/run",
		strings.NewReader(fmt.Sprintf(`{"key_id":%d,"modality":"text","models":["m-cx-a","m-cx-b"],"request":{"messages":[{"role":"user","content":"ping"}]}}`, key.ID)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session_id", Value: "tok-pg-member"})
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	r := newRelayTestRouter(t, db)
	served := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(served)
	}()

	// Both columns genuinely in flight before the disconnect.
	<-enteredA
	<-enteredB
	cancel()
	select {
	case <-served:
	case <-time.After(15 * time.Second):
		t.Fatal("the run did not settle after the caller disconnected")
	}

	// The disconnect propagated all the way to the upstream connections.
	// The relay settles as soon as its own attempt aborts; the upstream
	// observes the teardown when the TCP close lands on its side, which can
	// trail the client by a moment — so the observation is awaited, not
	// sampled.
	deadline := time.Now().Add(5 * time.Second)
	for (abortedA.Load() < 1 || abortedB.Load() < 1) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a, b := abortedA.Load(), abortedB.Load(); a != 1 || b != 1 {
		t.Fatalf("upstream aborts observed = %d / %d, want 1 / 1", a, b)
	}
	// Each in-flight column answered with a cancel acknowledgement naming
	// its own slot and audit row — best-effort on a dying channel, which a
	// recorder still captures.
	acks := eventsOfKind(t, w, "cancel_ack")
	if len(acks) != 2 {
		t.Fatalf("expected one cancel_ack per in-flight column, got %d: %s", len(acks), w.Body.String())
	}
	slots := map[int]bool{}
	for _, data := range acks {
		var ack sseCancelPayload
		parseAs(t, data, &ack)
		if ack.RequestID == "" {
			t.Fatalf("cancel_ack must carry the audit row's request id: %s", data)
		}
		slots[ack.Slot] = true
	}
	if !slots[0] || !slots[1] {
		t.Fatalf("cancel acknowledgements must cover both slots, got %v: %s", slots, w.Body.String())
	}
	if got := eventsOfKind(t, w, "done"); len(got) != 0 {
		t.Fatalf("an aborted column must not report done, got %d done events: %s", len(got), w.Body.String())
	}
	// The durable record: both aborts are in the audit trail, attributed
	// to the playground source with the caller-gone status.
	var aborted []model.RequestLog
	if err := db.Where("api_key_id = ? AND fail_reason = ?", key.ID, "client_disconnected").Find(&aborted).Error; err != nil {
		t.Fatalf("read aborted rows: %v", err)
	}
	if len(aborted) != 2 {
		t.Fatalf("expected one client_disconnected row per aborted column, got %d", len(aborted))
	}
	for _, row := range aborted {
		if row.StatusCode != 499 || row.Source != "playground" {
			t.Fatalf("aborted row = status %d source %q, want 499 playground", row.StatusCode, row.Source)
		}
	}
}
