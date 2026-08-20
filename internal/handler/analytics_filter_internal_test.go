// In-package tests for the analytics filter parsing — parseAnalyticsFilter
// is unexported, so these stay in package handler while the endpoint tests
// (which need the real router) live in the external test package.
package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/middleware"
)

// TestParseAnalyticsFilterMemberPinStripsFailovers locks the security-
// sensitive half of the member pin: a member's request arrives with
// with_failovers=1 and provider/user params of its own choosing, and the
// pin must override the user, strip the provider, and — the half that now
// lives on AnalyticsOptions rather than the filter — force WithFailovers
// off so a member cannot trigger the full failover scan. Deleting the
// options half of the pin turns this red.
func TestParseAnalyticsFilterMemberPinStripsFailovers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet,
		"/api/admin/analytics/report?with_failovers=1&provider_id=3&user_id=9", nil)
	pinned := uint(7)
	c.Set(middleware.ViewScopeKey, middleware.ViewScope{Member: true, Owner: &pinned})

	filter, opts, ok := parseAnalyticsFilter(c)
	if !ok {
		t.Fatal("parseAnalyticsFilter failed on a member request")
	}
	if filter.UserID == nil || *filter.UserID != 7 {
		t.Fatalf("member filter must pin user_id=7, got %+v", filter.UserID)
	}
	if filter.ProviderID != nil {
		t.Fatalf("member filter must strip provider_id, got %v", *filter.ProviderID)
	}
	if opts.WithFailovers {
		t.Fatal("member options must force WithFailovers=false (the failover scan is operator-only)")
	}
}
