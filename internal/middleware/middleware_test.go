package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIDMiddlewareSetsHeaderAndContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.GET("/x", func(c *gin.Context) {
		id, exists := c.Get(RequestIDKey)
		if !exists || id == "" {
			t.Errorf("expected request id in context")
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Header().Get("X-Request-Id") == "" {
		t.Errorf("expected X-Request-Id response header to be set")
	}
}

func TestBodySizeLimitReturns413Envelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(BodySizeLimit(10)) // 10 bytes limit for the test
	r.POST("/echo", func(c *gin.Context) {
		buf := make([]byte, 1024)
		_, err := c.Request.Body.Read(buf)
		if err != nil && err.Error() != "EOF" {
			WriteAdminError(c, http.StatusRequestEntityTooLarge, RequestEntityTooLargeCode)
			return
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/echo", strings.NewReader("this body is definitely longer than ten bytes"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d, body: %s", w.Code, w.Body.String())
	}
}

func TestRecoveryMiddlewareReturns500Envelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery())
	r.GET("/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/panic", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 after recovered panic, got %d", w.Code)
	}
}

// TestRecoveryMiddlewareDispatchesV1PanicToGatewayEnvelope guards
// WriteNamespacedError's namespace dispatch specifically for the panic path:
// a pre-existing "unconditionally call WriteAdminError" version would also
// pass TestRecoveryMiddlewareReturns500Envelope above (both return 500), so
// this asserts the actual body shape for a /v1/* panic is the OpenAI-style
// envelope, not the admin one.
func TestRecoveryMiddlewareDispatchesV1PanicToGatewayEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery())
	r.GET("/v1/panic", func(c *gin.Context) {
		panic("boom")
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/panic", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}

	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("expected OpenAI-style error envelope JSON, got unparseable body %s: %v", w.Body.Bytes(), err)
	}
	if env.Error.Type != "server_error" || env.Error.Code != "internal_error" {
		t.Fatalf("expected error.type=server_error, error.code=internal_error, got: %s", w.Body.String())
	}

	var raw map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &raw)
	if _, ok := raw["code"]; ok {
		t.Fatalf("must not leak the admin envelope's top-level code field, got: %s", w.Body.String())
	}
	if _, ok := raw["timestamp"]; ok {
		t.Fatalf("must not leak the admin envelope's timestamp field, got: %s", w.Body.String())
	}
}

// TestRecoveryReRaisesAbortHandlerAfterPartialWrite guards the post-write
// panic path: once a handler has already written bytes (e.g. a future
// SSE/streaming handler mid-flush), Recovery must not try to write a JSON
// error body on top of them — it must re-panic with http.ErrAbortHandler,
// the sentinel net/http's own per-connection recover specifically
// recognizes as "abort without logging a redundant stack trace", so the
// connection is torn down instead of completing as if the response were
// whole. Bytes already written before the panic must remain untouched.
func TestRecoveryReRaisesAbortHandlerAfterPartialWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestID())
	r.Use(Recovery())
	r.GET("/stream-panic", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write([]byte("partial-bytes-already-sent"))
		panic("boom mid-stream")
	})

	req := httptest.NewRequest(http.MethodGet, "/stream-panic", nil)
	w := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		r.ServeHTTP(w, req)
	}()

	if recovered != http.ErrAbortHandler {
		t.Fatalf("expected the panic to re-propagate as http.ErrAbortHandler, got: %v", recovered)
	}
	if w.Body.String() != "partial-bytes-already-sent" {
		t.Fatalf("expected already-written bytes to remain untouched, got: %q", w.Body.String())
	}
}
