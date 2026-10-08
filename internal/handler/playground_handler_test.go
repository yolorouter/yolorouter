package handler

// Unit pins for the aggregate channel's write path. The endpoint's
// end-to-end contract lives in internal/router/playground_e2e_test.go;
// what is pinned here is the branch only a direct call can reach: the
// marshal-failure skip in writePlaygroundEvent.

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestWritePlaygroundEventMarshalFailure: a payload json.Marshal refuses —
// here a channel, which no payload the run path passes can be — must cost
// exactly that one event: nothing is written and nothing panics, while the
// next healthy event still lands. Delete the skip branch and the failed
// marshal leaks an empty-data event onto the stream instead.
func TestWritePlaygroundEventMarshalFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	writePlaygroundEvent(c, "chunk", map[string]any{"delta": make(chan int)})
	if w.Body.Len() != 0 {
		t.Fatalf("an unmarshalable payload must write nothing, got %q", w.Body.String())
	}

	writePlaygroundEvent(c, "chunk", sseChunkEvent{Slot: 2, Model: "m", Delta: "d"})
	body := w.Body.String()
	if !strings.Contains(body, "event: chunk") || !strings.Contains(body, `"slot":2`) {
		t.Fatalf("a healthy payload after a skipped one must still land, got %q", body)
	}
}
