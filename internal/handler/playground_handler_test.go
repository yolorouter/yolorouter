package handler

// Unit pins for the aggregate channel's write path. The endpoint's
// end-to-end contract lives in internal/router/playground_e2e_test.go;
// what is pinned here are the branches only a direct call can reach: the
// marshal-failure skip in writePlaygroundEvent, and the response shaper's
// whole truth table (the router tests drive the live image dialect, whose
// kernel hands non-parsing answers on before a done event exists).

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/service/playground"
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

// TestInlineColumnResponseShapes: the done event's Response field speaks a
// strict two-shape contract the demux folds by shape alone — an object
// means the whole answer's own JSON, a plain string always means a cut
// prefix. The shaper must hold that invariant for every body it is handed:
// a whole object-shaped body embeds raw, an over-cap body embeds the
// prefix string with the flag set, and a body with no honest form — not
// JSON at all (an empty body, an HTML proxy page), or JSON that is not
// object-shaped (a bare string would arrive indistinguishable from a cut
// prefix) — is not inlined at all, so no consumer ever reads a whole body
// as a cut one or vice versa.
func TestInlineColumnResponseShapes(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		truncated bool
		wantBody  string // the exact embedded JSON; "" means not inlined
		wantCut   bool
	}{
		{
			name:     "whole object JSON embeds raw",
			body:     `{"created":1,"data":[{"url":"https://img.example/one.png"}]}`,
			wantBody: `{"created":1,"data":[{"url":"https://img.example/one.png"}]}`,
		},
		{
			name:     "whole array JSON embeds raw",
			body:     `["frame one"]`,
			wantBody: `["frame one"]`,
		},
		{
			name:     "whitespace-padded object JSON embeds raw",
			body:     "\t{\"created\":1}\n",
			wantBody: "\t{\"created\":1}\n",
		},
		{
			name:      "over-cap body embeds the prefix as a string with the flag",
			body:      `{"created":1,`,
			truncated: true,
			wantBody:  `"{\"created\":1,"`,
			wantCut:   true,
		},
		{
			name: "empty body is not inlined",
		},
		{
			name: "non-JSON body is not inlined",
			body: `<html>upstream broke</html>`,
		},
		{
			name: "bare JSON string is not inlined",
			body: `"an answer that is itself a string"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := inlineColumnResponse(&playground.ColumnResult{
				Status:    200,
				Body:      []byte(tc.body),
				Truncated: tc.truncated,
			})
			if cut != tc.wantCut {
				t.Fatalf("truncated flag = %v, want %v", cut, tc.wantCut)
			}
			if string(got) != tc.wantBody {
				t.Fatalf("inlined payload = %q, want %q", string(got), tc.wantBody)
			}
		})
	}
}
