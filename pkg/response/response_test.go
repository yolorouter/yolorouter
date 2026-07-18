package response

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
)

// TestErrorMapsInvalidParamTo400 guards the /simplify altitude-review fix:
// errcode.InvalidParam (50003) numerically falls in the "50xxx = system
// error" bucket httpStatusForCode otherwise maps to 500, even though its
// meaning is a 400 client error. Two handlers (auth_handler.go's bindJSON,
// provider_handler.go's parseUintParam) had already independently worked
// around this by calling ErrorStatus(400, ...) directly instead of
// Error/ParamError — this test locks the root-cause fix in place so a
// future caller of Error(c, errcode.InvalidParam, ...) or ParamError gets
// the correct status without needing its own workaround.
func TestErrorMapsInvalidParamTo400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	Error(c, errcode.InvalidParam, "bad id")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestParamErrorReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	ParamError(c, "invalid field")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var env Response
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if env.Code != errcode.InvalidParam {
		t.Fatalf("expected code %d, got %d", errcode.InvalidParam, env.Code)
	}
}

// TestErrorStillMaps500ForOtherSystemCodes proves the InvalidParam special
// case didn't accidentally widen to the rest of the 50xxx system-error
// range — only InvalidParam itself is a 400, everything else in that
// bucket (e.g. errcode.InternalError) is still a genuine 500.
func TestErrorStillMaps500ForOtherSystemCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	Error(c, errcode.InternalError, "boom")

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
	}
}
