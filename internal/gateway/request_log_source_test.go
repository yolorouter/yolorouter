package gateway

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/gateway/rows"
	"github.com/yolorouter/yolorouter/internal/model"
)

// TestRequestLogSourceConstantsAligned pins the request_logs source markers'
// four-place invariant: the model constant (what the row column stores), the
// rows adapter constant (what gateway row writes carry), and this package's
// ports re-export (what callers of the kernel spell) must agree byte-for-
// byte, or a source written by one layer becomes unreadable by another. The
// handler query whitelist is validated separately in the handler tests.
func TestRequestLogSourceConstantsAligned(t *testing.T) {
	cases := []struct {
		name              string
		model, rows, port string
	}{
		{"vision_fallback", model.RequestLogSourceVisionFallback, rows.RequestLogSourceVisionFallback, RequestLogSourceVisionFallback},
		{"playground", model.RequestLogSourcePlayground, rows.RequestLogSourcePlayground, RequestLogSourcePlayground},
	}
	for _, tc := range cases {
		if tc.rows != tc.model {
			t.Fatalf("%s: rows constant %q != model constant %q", tc.name, tc.rows, tc.model)
		}
		if tc.port != tc.model {
			t.Fatalf("%s: ports re-export %q != model constant %q", tc.name, tc.port, tc.model)
		}
	}
	if model.RequestLogSourcePlayground != "playground" {
		t.Fatalf("playground marker changed value: %q", model.RequestLogSourcePlayground)
	}
}
