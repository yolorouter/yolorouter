package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func newAuthContext(t *testing.T, authHeader string) *gin.Context {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if authHeader != "" {
		c.Request.Header.Set("Authorization", authHeader)
	}
	return c
}

// TestExtractBearerKey locks in the RFC 7235 scheme handling: case-
// insensitive scheme, whitespace/tab separator tolerated, and the
// "bearerXYZ" typo rejected as malformed (not parsed as token "XYZ").
func TestExtractBearerKey(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"standard", "Bearer sk-yr-abc", "sk-yr-abc"},
		{"lowercase scheme", "bearer sk-yr-abc", "sk-yr-abc"},
		{"uppercase scheme", "BEARER sk-yr-abc", "sk-yr-abc"},
		{"mixed case", "BeArEr sk-yr-abc", "sk-yr-abc"},
		{"tab separator", "Bearer\tsk-yr-abc", "sk-yr-abc"},
		{"extra spaces collapsed", "Bearer   sk-yr-abc", "sk-yr-abc"},
		{"missing header", "", ""},
		{"wrong scheme", "Basic sk-yr-abc", ""},
		{"bearerXYZ typo rejected", "bearerXYZ", ""},
		{"bearerX-with-token rejected", "bearerXYZ sk-yr-abc", ""},
		{"just bearer", "Bearer", ""},
		{"bearer trailing space", "Bearer ", ""},
		{"token with internal spaces preserved", "Bearer sk yr abc", "sk yr abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newAuthContext(t, tt.input)
			if got := extractBearerKey(c); got != tt.want {
				t.Errorf("extractBearerKey(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
