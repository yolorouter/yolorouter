package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// The reveal rate tests exercise the aggregated window in isolation: a
// small injected limit, a pinned clock (the handler reads time through the
// package's timeNow var, so a test can advance the window without
// sleeping), and a stub that stamps the admin identity the same way
// RequireSession does in production. The full-stack permission, audit,
// and envelope behavior lives in the router package's reveal tests.

// seedRevealKey inserts one provider_keys row with the given plaintext,
// encrypted under the test master key, and returns its id.
func seedRevealKey(t *testing.T, db *gorm.DB, providerID uint, sort int, plaintext string) uint {
	t.Helper()
	encrypted, err := testutil.ProviderSecrets().Encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	now := time.Now().UTC()
	k := &model.ProviderKey{
		ProviderID: providerID, Label: fmt.Sprintf("key-%d", sort), EncryptedKey: encrypted,
		KeyPrefix: "sk-rate-", TestModel: "gpt-4o-mini", SortOrder: sort,
		ManagementStatus: model.ProviderKeyStatusEnabled, VerificationStatus: model.VerificationStatusUntested,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(k).Error; err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return k.ID
}

// newRevealRateRouter builds a minimal engine — the identity stub (sets
// user_id from a header, standing in for RequireSession) plus the reveal
// handler with the caller's injected window — over one provider with
// three real keys. It returns the engine, the reveal-able key ids ordered
// by seed, and a pointer to the pinned clock the handler reads.
func newRevealRateRouter(t *testing.T, limit int) (*gin.Engine, []uint, *time.Time) {
	t.Helper()
	if err := RegisterValidators(); err != nil {
		t.Fatalf("RegisterValidators failed: %v", err)
	}
	db := testutil.NewSQLiteDB(t)
	svc := provider.NewProviderService(db, testutil.ProviderSecrets(), nil)

	now := time.Now().UTC()
	p := &model.Provider{Name: "rate-target", ProviderType: "openai", BaseURL: "https://api.example.com/v1",
		ManagementStatus: model.ProviderStatusEnabled, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(p).Error; err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	ids := make([]uint, 0, 3)
	for i := 0; i < 3; i++ {
		ids = append(ids, seedRevealKey(t, db, p.ID, i+1, fmt.Sprintf("sk-rate-window-%02d-abcdefgh", i)))
	}

	clock := now
	prev := timeNow
	timeNow = func() time.Time { return clock }
	t.Cleanup(func() { timeNow = prev })

	r := gin.New()
	r.Use(func(c *gin.Context) {
		if uid := c.GetHeader("X-Test-Admin"); uid != "" {
			id, err := strconv.ParseUint(uid, 10, 64)
			if err != nil {
				return
			}
			c.Set(middleware.UserIDKey, uint(id))
		}
	})
	r.POST("/api/admin/providers/:id/keys/:keyId/reveal",
		PostProviderKeyReveal(svc, middleware.NewPerClientRateWindow(limit, time.Minute)))
	return r, ids, &clock
}

func revealRequest(r *gin.Engine, admin string, keyID uint) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/providers/1/keys/%d/reveal", keyID), nil)
	req.Header.Set("X-Test-Admin", admin)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestProviderKeyRevealRateWindowAggregatesAcrossKeyIDs pins the
// anti-enumeration property: the budget belongs to the admin account, not
// to the key, so probing one request per DIFFERENT key id still exhausts
// the window; advancing the clock one full window resets it; a second
// admin account carries an independent budget (behind a reverse proxy the
// two would share an IP — the window key must still keep them apart).
func TestProviderKeyRevealRateWindowAggregatesAcrossKeyIDs(t *testing.T) {
	r, ids, clock := newRevealRateRouter(t, 2)

	// Two different keys: both fit the limit-2 window.
	for i := 0; i < 2; i++ {
		if w := revealRequest(r, "7", ids[i]); w.Code != http.StatusOK {
			t.Fatalf("reveal of key %d (all probes use different key ids) within budget must succeed, got %d: %s", i, w.Code, w.Body.String())
		}
	}
	// The third probe targets a key id this budget has never seen — the
	// enumeration shape. It must be refused all the same.
	if w := revealRequest(r, "7", ids[2]); w.Code != http.StatusTooManyRequests {
		t.Fatalf("an enumerated third key id must be refused once the caller's aggregate budget is spent, got %d: %s", w.Code, w.Body.String())
	}

	// Advance one full window: the same caller's budget starts over.
	*clock = clock.Add(time.Minute + time.Second)
	if w := revealRequest(r, "7", ids[2]); w.Code != http.StatusOK {
		t.Fatalf("after the window rolls the reveal must pass again, got %d: %s", w.Code, w.Body.String())
	}

	// A different admin account is a different window key: roll the clock
	// back inside the first admin's spent window and the second admin
	// still gets through.
	*clock = clock.Add(-(time.Minute + time.Second))
	if w := revealRequest(r, "8", ids[0]); w.Code != http.StatusOK {
		t.Fatalf("a second admin must carry an independent budget, got %d: %s", w.Code, w.Body.String())
	}
}

// TestProviderKeyRevealRateWindowConcurrentSinglePass pins the
// last-quota race: with exactly one allowance left, two concurrent
// reveals from the same admin must produce exactly one 200 and one 429 —
// never two passes. PerClientRateWindow counts under a mutex, so the
// loser is rejected rather than granted a stale slot.
func TestProviderKeyRevealRateWindowConcurrentSinglePass(t *testing.T) {
	r, ids, _ := newRevealRateRouter(t, 1)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = revealRequest(r, "7", ids[i]).Code
		}(i)
	}
	wg.Wait()

	got := map[int]int{}
	for _, c := range codes {
		got[c]++
	}
	if got[http.StatusOK] != 1 || got[http.StatusTooManyRequests] != 1 {
		t.Fatalf("two concurrent reveals against one remaining allowance must split exactly 1x200 + 1x429, got %v", codes)
	}
}

// TestProviderKeyRevealResponseContract checks the response at the handler
// boundary: 200 with the full plaintext exactly once under plaintext_key,
// and the no-store header present so no cache between server and browser
// retains the secret.
func TestProviderKeyRevealResponseContract(t *testing.T) {
	r, ids, _ := newRevealRateRouter(t, 10)
	plaintext := "sk-rate-window-00-abcdefgh"

	w := revealRequest(r, "7", ids[0])
	if w.Code != http.StatusOK {
		t.Fatalf("reveal must succeed, got %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store — the plaintext body must not be cacheable", got)
	}
	if n := strings.Count(w.Body.String(), plaintext); n != 1 {
		t.Errorf("body must carry the plaintext exactly once, found %d occurrences: %s", n, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plaintext_key"`) {
		t.Errorf("body must expose the secret under the plaintext_key field, got %s", w.Body.String())
	}
}
