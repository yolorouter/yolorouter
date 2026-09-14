package protocols

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// This file exists only under `go test`. What it holds is a ClientWriter over a
// raw gin context, which the tests in this package need and production does
// not: the gateway supplies its own response object, and this package cannot
// import that one back without a cycle.
//
// Shipping it anyway would put an adapter in the package's surface that nothing
// in the product reaches, and the tooling cannot tell the difference — an
// exported symbol used only by tests reads as used.

// ginClientWriter is the ClientWriter over a raw gin response, for callers that
// still hold one.
type ginClientWriter struct{ c *gin.Context }

// NewGinClientWriter adapts a gin context to ClientWriter.
func NewGinClientWriter(c *gin.Context) ClientWriter { return ginClientWriter{c: c} }

func (g ginClientWriter) Inject(h http.Header) {
	for k, vv := range h {
		// Cleared first, then every value added: replacing by name while
		// keeping multi-value headers whole. Adding without clearing would put
		// a second Content-Type on a response that already had one.
		g.c.Writer.Header().Del(k)
		for _, v := range vv {
			g.c.Writer.Header().Add(k, v)
		}
	}
}

// Commit records the status the way gin does: held until the first body write,
// so the caller's own view of "has anything been sent" is unchanged by moving
// through this interface. A writer that needs the status on the wire at commit
// time implements that itself.
func (g ginClientWriter) Commit(status int) error {
	g.c.Writer.WriteHeader(status)
	return nil
}

func (g ginClientWriter) Write(p []byte) (int, error) {
	g.slideDeadline()
	return g.c.Writer.Write(p)
}

func (g ginClientWriter) Flush() error {
	g.slideDeadline()
	return FlushAndCheckError(g.c)
}

// slideDeadline bounds how long one write to the caller may take.
//
// A caller that stops reading otherwise holds the handler open for as long as
// it likes, which is a connection and a concurrency slot spent on somebody who
// left. Production bounds this from the gateway's own response object, which
// uses the window that delivery asked for; this one uses the package default,
// which is why what it pins is the mechanism rather than the policy.
//
// The error is ignored deliberately: writers that cannot take a deadline
// (a test recorder, say) still write correctly.
func (g ginClientWriter) slideDeadline() { _ = ApplyStreamWriteDeadline(g.c) }
