package testutil

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// Envelope is the admin API's unified response envelope, decoded far enough
// for a test to route on code/message and unmarshal Data itself.
type Envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
	Timestamp int64           `json:"timestamp"`
}

// DoJSON performs one JSON request against the engine and decodes the
// envelope. A nil cookie sends the request unauthenticated.
func DoJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}, cookie *http.Cookie) (*httptest.ResponseRecorder, Envelope) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var env Envelope
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal response body %q: %v", w.Body.String(), err)
		}
	}
	return w, env
}
