package middleware

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// Response transport compression (gzip) for the console's own HTTP surface:
// the JSON the admin API returns and the embedded frontend's text assets are
// delivered uncompressed today, which is what makes a slow link feel broken —
// a megabyte-scale transcript page is 10× smaller on the wire once compressed.
//
// The middleware negotiates per request (Accept-Encoding), buffers the first
// minGzipResponseBytes of the body to skip small responses, and streams the
// rest through a gzip.Writer:
//
//   - Only compressible content types apply — text/*, JSON, JavaScript,
//     XML/SVG families. Binary payloads (images, audio, video, archives,
//     fonts) are already compressed or not worth recompressing, and
//     text/event-stream is excluded explicitly because flushing per SSE
//     frame is the point of that transport (buffering it would break
//     streaming; compressing it after headers went out is impossible).
//   - Responses under the threshold pass through untouched (an error
//     envelope gains nothing and would pay gzip framing overhead).
//   - HEAD requests and Range requests are never compressed.
//   - A response that already carries Content-Encoding (a relayed upstream
//     gzip) is passed through untouched — never double-encoded.
//
// Because the decision is body-driven, headers reach the wire only when the
// first body bytes do (or at the deferred close for small responses), which
// keeps Content-Length coherent: on commit the stale uncompressed length is
// dropped and the chunked encoding takes over.
//
// One surface breaks that ordering on purpose: the gateway relay handshake
// (ginClientResponse.Commit) puts the status on the wire with WriteHeaderNow
// BEFORE the body exists. net/http snapshots the header map at that moment
// (any Header() access marks the response), so an encoding decided later
// would never serialize and the relay's JSON would ship as gzip bytes
// labeled plain JSON — silent corruption for every client that negotiates.
// The wrapper therefore decides inside WriteHeaderNow too, while the header
// it needs can still reach the wire; see gzipResponseWriter.WriteHeaderNow.

const (
	// minGzipResponseBytes is the buffering threshold: a response whose
	// body never reaches this size is served verbatim.
	minGzipResponseBytes = 1 << 10 // 1 KiB
)

// Gzip returns the transport-compression middleware. Mount it once at the
// engine root so the admin JSON API, the embedded static assets, the SPA
// fallback, and the gateway's whole-body relays all pass through it. The
// content-type allowlist keeps the streaming and binary relays (SSE frames,
// images, audio) untouched by construction; whole-body JSON relays commit
// their headers before writing and are handled by the WriteHeaderNow hook.
func Gzip() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !acceptsGzip(c.Request.Header.Get("Accept-Encoding")) ||
			c.Request.Method == http.MethodHead ||
			c.Request.Header.Get("Range") != "" {
			c.Next()
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: c.Writer}
		c.Writer = gw
		defer gw.finish()
		c.Next()
	}
}

// acceptsGzip reports whether the client's Accept-Encoding header names gzip
// (or the wildcard) with a non-zero quality value. A bare "gzip" counts
// (quality defaults to 1); "gzip;q=0" is an explicit refusal.
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		pieces := strings.Split(part, ";")
		token := strings.ToLower(strings.TrimSpace(pieces[0]))
		if token != "gzip" && token != "*" {
			continue
		}
		q := 1.0
		for _, p := range pieces[1:] {
			p = strings.TrimSpace(p)
			if !strings.HasPrefix(p, "q=") {
				continue
			}
			if v, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 32); err == nil {
				q = v
			}
		}
		if q > 0 {
			return true
		}
	}
	return false
}

// gzipResponseWriter wraps gin's writer with the buffer-then-decide body
// path. Until the response is decided, body bytes accumulate in buf; the
// decision happens the moment the buffer reaches the threshold (commit to
// gzip) or at finish (pass through verbatim). Handlers that never write a
// body (204/304, early Flush with nothing written) never pay for it.
type gzipResponseWriter struct {
	gin.ResponseWriter
	buf     []byte
	gz      *gzip.Writer
	decided bool
	size    int
}

// Write buffers undecided bodies and streams decided ones. The returned
// count is the bytes accepted (len(data)) either way — the underlying writer
// is only touched once the decision is made, and errors there surface on the
// deciding write.
func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	w.size += len(data)
	if w.decided {
		if w.gz != nil {
			if _, err := w.gz.Write(data); err != nil {
				return 0, err
			}
			return len(data), nil
		}
		return w.ResponseWriter.Write(data)
	}
	w.buf = append(w.buf, data...)
	if len(w.buf) >= minGzipResponseBytes {
		w.decide()
	}
	return len(data), nil
}

// WriteString routes through Write so both of gin's body paths share one
// buffering/decision path.
func (w *gzipResponseWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

// Flush handles the streaming handshake: a flush with nothing buffered sends
// the headers uncompressed right away, which commits the response to
// passthrough (Content-Encoding can no longer be added); a flush with a
// buffered body decides first so a compressible JSON that was written in
// pieces still compresses.
func (w *gzipResponseWriter) Flush() {
	if !w.decided {
		if len(w.buf) == 0 {
			w.decided = true
		} else {
			w.decide()
		}
	}
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	w.ResponseWriter.Flush()
}

// Written reports whether body bytes were accepted, buffered ones included —
// handlers ask this to know "the response has started", and a buffered
// partial JSON is as started as a flushed one: writing an error envelope
// after either corrupts the response equally. A decision forced by
// WriteHeaderNow counts too: the headers are on the wire, which is as
// started as a response gets.
func (w *gzipResponseWriter) Written() bool {
	return w.decided || len(w.buf) > 0
}

// WriteHeaderNow handles the explicit header commit. The gateway relay's
// commit-then-write handshake calls this BEFORE any body byte exists, and
// net/http snapshots the header map the moment WriteHeader runs (any
// Header() access marks the response) — a Content-Encoding added later in
// decide() would never serialize, and the body would go out as unlabeled
// gzip. Deciding before delegating keeps the encoding header on the wire;
// the status still precedes any buffered bytes, because decide()'s
// passthrough arm drains through gin's own Write, which writes the
// recorded status first.
//
// With an empty buffer the decision rests on the declared content type
// alone (relays stage Content-Type before committing), so a committed
// relay response compresses whenever its type allows regardless of the
// size threshold: the size is unknowable at commit time, and correctness
// outranks the small-response optimization.
func (w *gzipResponseWriter) WriteHeaderNow() {
	if !w.decided {
		w.decide()
	}
	w.ResponseWriter.WriteHeaderNow()
}

// Size reports the bytes the handler produced (the uncompressed body size),
// preserving the access log's response-size semantics.
func (w *gzipResponseWriter) Size() int {
	return w.size
}

// decide commits the response one way or the other. The gzip arm sets the
// content encoding, drops the now-wrong uncompressed Content-Length, and
// replays the buffer through the compressor; the passthrough arm writes the
// buffer verbatim. Both set Vary so a cache between client and server can
// tell the compressed and uncompressed variants apart.
func (w *gzipResponseWriter) decide() {
	w.decided = true
	if !responseCompressible(w.Header()) {
		w.passthrough()
		return
	}
	h := w.Header()
	addVaryAcceptEncoding(h)
	if h.Get("Content-Encoding") != "" {
		// Somebody already encoded this body (a relayed upstream gzip);
		// wrapping it again would corrupt it.
		w.passthrough()
		return
	}
	h.Set("Content-Encoding", "gzip")
	h.Del("Content-Length")
	w.gz = gzip.NewWriter(w.ResponseWriter)
	if len(w.buf) > 0 {
		_, _ = w.gz.Write(w.buf)
		w.buf = nil
	}
}

// passthrough commits to serving the buffered bytes verbatim.
func (w *gzipResponseWriter) passthrough() {
	w.decided = true
	if len(w.buf) > 0 {
		_, _ = w.ResponseWriter.Write(w.buf)
		w.buf = nil
	}
}

// finish closes the response: an undecided body (smaller than the
// threshold, or never decided because nothing was written) passes through
// verbatim; a gzip body flushes the compressor's tail.
func (w *gzipResponseWriter) finish() {
	if !w.decided {
		w.passthrough()
		return
	}
	if w.gz != nil {
		_ = w.gz.Close()
	}
}

// responseCompressible reports whether the response's declared content type
// is worth compressing: text families, JSON, JavaScript, and XML (SVG
// included via the +xml suffix). Everything else — images, audio, video,
// archives, fonts, wasm, octet-stream — is binary or already compressed and
// passes through. An absent content type is treated as not compressible
// (the writer cannot judge bytes it has not seen, and no handler on this
// server leaves the type unset by accident).
func responseCompressible(h http.Header) bool {
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	if mediaType == "text/event-stream" {
		// SSE must never be buffered or encoded here: per-frame flushing is
		// the transport's whole point.
		return false
	}
	switch {
	case strings.HasPrefix(mediaType, "text/"):
		return true
	case mediaType == "application/json", strings.HasSuffix(mediaType, "+json"):
		return true
	case mediaType == "application/javascript",
		mediaType == "application/x-javascript",
		mediaType == "text/javascript",
		mediaType == "application/ecmascript",
		mediaType == "text/ecmascript":
		return true
	case mediaType == "application/xml", mediaType == "text/xml", strings.HasSuffix(mediaType, "+xml"):
		return true
	default:
		return false
	}
}

// addVaryAcceptEncoding appends Accept-Encoding to the response's Vary
// header (creating it when absent) so shared caches key the variant on the
// encoding negotiation.
func addVaryAcceptEncoding(h http.Header) {
	vary := h.Get("Vary")
	if vary == "" {
		h.Set("Vary", "Accept-Encoding")
		return
	}
	for _, v := range strings.Split(vary, ",") {
		if strings.EqualFold(strings.TrimSpace(v), "Accept-Encoding") {
			return
		}
	}
	h.Set("Vary", vary+", Accept-Encoding")
}
