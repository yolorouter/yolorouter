package middleware

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The transport-compression middleware's contract, one arm per rule:
// compressible types compress once past the threshold, small responses and
// non-negotiating clients pass through, SSE and already-encoded bodies are
// never touched, and HEAD/Range requests are excluded up front. Each test
// runs a real gin engine so the writer's position inside the context
// (c.Writer swapping, Written()/Size() semantics) is exercised the way
// production mounts it.

func gzipTestEngine(t *testing.T, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Gzip())
	r.GET("/x", handler)
	return r
}

// bigText is a compressible payload comfortably past the threshold.
var bigText = strings.Repeat("the quick brown fox jumps over the lazy dog. ", 64)

func getWith(t *testing.T, r *gin.Engine, header string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	if header != "" {
		req.Header.Set("Accept-Encoding", header)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeGzip(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return string(out)
}

func TestGzipCompressesLargeJSON(t *testing.T) {
	r := gzipTestEngine(t, func(c *gin.Context) { c.Data(http.StatusOK, "application/json; charset=utf-8", []byte(bigText)) })
	w := getWith(t, r, "gzip")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := w.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want it to name Accept-Encoding", got)
	}
	body, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if decodeGzip(t, body) != bigText {
		t.Fatal("decompressed body does not round-trip to the payload")
	}
	if len(body) >= len(bigText) {
		t.Fatalf("compressed body %dB is not smaller than the %dB payload", len(body), len(bigText))
	}
}

func TestGzipCompressesJavaScriptAndHTML(t *testing.T) {
	for _, ct := range []string{
		"text/javascript; charset=utf-8",
		"application/javascript",
		"text/html; charset=utf-8",
		"text/css",
		"image/svg+xml",
		"application/manifest+json",
	} {
		r := gzipTestEngine(t, func(c *gin.Context) { c.Data(http.StatusOK, ct, []byte(bigText)) })
		w := getWith(t, r, "gzip, deflate, br")
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("content type %q: Content-Encoding = %q, want gzip", ct, w.Header().Get("Content-Encoding"))
		}
	}
}

func TestGzipPassesSmallResponsesThrough(t *testing.T) {
	r := gzipTestEngine(t, func(c *gin.Context) { c.Data(http.StatusOK, "application/json", []byte(`{"ok":true}`)) })
	w := getWith(t, r, "gzip")
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatal("a small response must not be compressed")
	}
	if w.Body.String() != `{"ok":true}` {
		t.Fatalf("body = %q, want it verbatim", w.Body.String())
	}
	if w.Header().Get("Content-Length") != "" && w.Body.String() != "" {
		// Content-Length stays whatever the handler declared — untouched.
		t.Logf("Content-Length preserved: %s", w.Header().Get("Content-Length"))
	}
}

func TestGzipSkipsWhenClientDoesNotAccept(t *testing.T) {
	for _, header := range []string{"", "identity", "deflate, br", "gzip;q=0"} {
		r := gzipTestEngine(t, func(c *gin.Context) { c.Data(http.StatusOK, "application/json", []byte(bigText)) })
		w := getWith(t, r, header)
		if w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("Accept-Encoding %q: response was compressed anyway", header)
		}
		if w.Body.String() != bigText {
			t.Fatalf("Accept-Encoding %q: body must be verbatim", header)
		}
	}
}

func TestGzipNeverCompressesSSE(t *testing.T) {
	// A handler that streams events: headers flushed first (the SSE
	// handshake), then frames written and flushed one by one.
	r := gzipTestEngine(t, func(c *gin.Context) {
		c.Header("Content-Type", "text/event-stream")
		c.Writer.Flush()
		for i := 0; i < 4; i++ {
			c.Data(http.StatusOK, "", []byte("data: frame-"+strconv.Itoa(i)+"\n\n"))
			c.Writer.Flush()
		}
	})
	w := getWith(t, r, "gzip")
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatal("text/event-stream must never be compressed")
	}
	if !strings.Contains(w.Body.String(), "data: frame-0") || !strings.Contains(w.Body.String(), "data: frame-3") {
		t.Fatalf("SSE frames lost: %q", w.Body.String())
	}
}

func TestGzipSkipsBinaryAndAlreadyEncodedTypes(t *testing.T) {
	payload := []byte(bigText)
	for _, ct := range []string{"image/png", "audio/mpeg", "video/mp4", "application/zip", "font/woff2", "application/octet-stream", "application/wasm"} {
		r := gzipTestEngine(t, func(c *gin.Context) { c.Data(http.StatusOK, ct, payload) })
		w := getWith(t, r, "gzip")
		if w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("content type %q must not be compressed", ct)
		}
	}
	// A body that arrives already gzip-encoded (a relayed upstream): the
	// middleware must never wrap it a second time.
	r := gzipTestEngine(t, func(c *gin.Context) {
		c.Header("Content-Encoding", "gzip")
		c.Data(http.StatusOK, "application/json", payload)
	})
	w := getWith(t, r, "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" || w.Body.String() != bigText {
		t.Fatal("an already-encoded response must pass through byte-for-byte")
	}
}

func TestGzipSkipsHeadAndRangeRequests(t *testing.T) {
	r := gzipTestEngine(t, func(c *gin.Context) { c.Data(http.StatusOK, "application/json", []byte(bigText)) })

	req := httptest.NewRequest(http.MethodHead, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatal("HEAD responses must not be compressed")
	}

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("Range", "bytes=0-99")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Header().Get("Content-Encoding") != "" {
		t.Fatal("Range requests must not be compressed (partial content units would break)")
	}
}

func TestGzipCompressesChunkedWritesAsOneBody(t *testing.T) {
	// A handler that writes the JSON in several small pieces (below the
	// threshold each) with a flush in between: the buffered head plus the
	// flush decision still compress the whole body as one stream.
	r := gzipTestEngine(t, func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		half := len(bigText) / 2
		_, _ = c.Writer.WriteString(bigText[:half])
		c.Writer.Flush()
		_, _ = c.Writer.WriteString(bigText[half:])
	})
	w := getWith(t, r, "gzip")
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("a flushed compressible body must still compress; headers: %v", w.Header())
	}
	body, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if decodeGzip(t, body) != bigText {
		t.Fatal("chunked body did not round-trip through one gzip stream")
	}
}

func TestGzipWriterSemanticsForCallers(t *testing.T) {
	// Written() must be true once body bytes were accepted even while they
	// are still buffered (a partial write is a started response — error
	// paths must not append an envelope after it), and Size() must keep
	// reporting the handler-produced (uncompressed) byte count.
	var captured gin.ResponseWriter
	r := gzipTestEngine(t, func(c *gin.Context) {
		c.Header("Content-Type", "application/json")
		_, _ = c.Writer.WriteString(bigText[:512]) // buffered, below threshold
		if !c.Writer.Written() {
			t.Fatal("Written() must be true while bytes are buffered")
		}
		_, _ = c.Writer.WriteString(bigText[512:])
		captured = c.Writer
	})
	w := getWith(t, r, "gzip")
	if captured == nil {
		t.Fatal("handler did not run")
	}
	if got := captured.Size(); got != len(bigText) {
		t.Fatalf("Size() = %d, want %d (uncompressed body)", got, len(bigText))
	}
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("the response should have committed to gzip")
	}
}

// relayTestServer runs a real net/http server (not a recorder) because the
// bug class under test only exists on the wire path: net/http snapshots the
// header map at WriteHeader time, so an encoding added afterwards never
// serializes — httptest.ResponseRecorder keeps headers live and would hide
// exactly that. The client sends Accept-Encoding explicitly so Go's
// transport does neither transparent decompression nor header stripping.
func relayTestServer(t *testing.T, handler gin.HandlerFunc) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Gzip())
	r.GET("/relay", handler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func relayDo(t *testing.T, srv *httptest.Server) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/relay", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("client Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// TestGzipHandlesExplicitHeaderCommitRelay pins the gateway relay
// handshake: the relay commits its status to the wire (WriteHeader +
// WriteHeaderNow) BEFORE writing the body, unlike the c.JSON/c.Data paths
// every other test uses. Once those headers serialize, adding
// Content-Encoding later is impossible, so the wrapper must decide at the
// commit — a JSON answer otherwise ships as gzip bytes labeled as plain
// JSON and the client's parser explodes on the 0x1f 0x8b magic.
func TestGzipHandlesExplicitHeaderCommitRelay(t *testing.T) {
	srv := relayTestServer(t, func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.WriteHeader(http.StatusOK)
		c.Writer.WriteHeaderNow()
		_, _ = c.Writer.Write([]byte(bigText))
		c.Writer.Flush()
	})
	resp := relayDo(t, srv)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip — the relay's JSON is being gzip-encoded without the header (silent corruption)", got)
	}
	if got := resp.Header.Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Fatalf("Vary = %q, want it to name Accept-Encoding", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if decodeGzip(t, body) != bigText {
		t.Fatal("relay body does not round-trip through the gzip stream")
	}

	// The relay forwards upstream status codes verbatim (Commit is called
	// with the upstream's code); the decision at commit time must not
	// disturb the status the wire ends up carrying.
	srv = relayTestServer(t, func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.WriteHeader(http.StatusCreated)
		c.Writer.WriteHeaderNow()
		_, _ = c.Writer.Write([]byte(bigText))
		c.Writer.Flush()
	})
	resp = relayDo(t, srv)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — the commit must not disturb the status", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("non-200 relay: Content-Encoding = %q, want gzip", got)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read non-200 relay body: %v", err)
	}
	if decodeGzip(t, body) != bigText {
		t.Fatal("non-200 relay body does not round-trip")
	}
}

// TestGzipExplicitCommitKeepsSSEAndEncodedRelaysUntouched: the same
// commit-then-write handshake with a text/event-stream content type (the
// streaming relay) must stay byte-for-byte uncompressed, and a relay that
// already carries Content-Encoding (an upstream gzip passed through) must
// not be double-encoded even though its type is compressible JSON.
func TestGzipExplicitCommitKeepsSSEAndEncodedRelaysUntouched(t *testing.T) {
	sseFrames := "data: frame-0\n\ndata: frame-1\n\n"
	srv := relayTestServer(t, func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.WriteHeader(http.StatusOK)
		c.Writer.WriteHeaderNow()
		_, _ = c.Writer.Write([]byte(sseFrames))
		c.Writer.Flush()
	})
	resp := relayDo(t, srv)
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("SSE relay: Content-Encoding = %q, want none", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	if string(body) != sseFrames {
		t.Fatalf("SSE relay body = %q, want frames verbatim", string(body))
	}

	// Already-encoded body: compressible type, but Content-Encoding is set
	// by the handler before the commit — the wrapper must pass it through.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(bigText)); err != nil {
		t.Fatalf("seed gzip body: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close seed gzip body: %v", err)
	}
	encoded := buf.Bytes()
	srv = relayTestServer(t, func(c *gin.Context) {
		c.Writer.Header().Set("Content-Type", "application/json")
		c.Writer.Header().Set("Content-Encoding", "gzip")
		c.Writer.WriteHeader(http.StatusOK)
		c.Writer.WriteHeaderNow()
		_, _ = c.Writer.Write(encoded)
		c.Writer.Flush()
	})
	resp = relayDo(t, srv)
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("pre-encoded relay: Content-Encoding = %q, want the handler's own gzip", got)
	}
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read pre-encoded body: %v", err)
	}
	if !bytes.Equal(body, encoded) {
		t.Fatal("a pre-encoded relay body must pass through byte-for-byte, not be re-wrapped")
	}
}

func TestAcceptsGzip(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip", true},
		{"gzip, deflate, br", true},
		{"deflate, gzip;q=1.0", true},
		{"*", true},
		{"*;q=0.5, deflate", true},
		{"gzip;q=0", false},
		{"gzip; q=0.000", false},
		{"deflate, br", false},
		{"", false},
		{"identity", false},
	}
	for _, tc := range cases {
		if got := acceptsGzip(tc.header); got != tc.want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", tc.header, got, tc.want)
		}
	}
}
