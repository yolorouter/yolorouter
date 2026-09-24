package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yolorouter/yolorouter/internal/selfupdate"
	"github.com/yolorouter/yolorouter/pkg/database"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// fakeUpdate is a test double for the apply + restart dependencies: it
// records invocations and returns a scripted result, so the handler's
// gating and sequencing are testable without touching network or disk.
type fakeUpdate struct {
	result       selfupdate.Result
	err          error
	applyCalls   int
	restartCalls int
}

func (f *fakeUpdate) apply(_ context.Context) (selfupdate.Result, error) {
	f.applyCalls++
	return f.result, f.err
}

func (f *fakeUpdate) restart() { f.restartCalls++ }

func newUpdateTestRouter(mode string, fake *fakeUpdate) *gin.Engine {
	return newUpdateTestRouterWithPrecheck(mode, nil, fake)
}

// newUpdateTestRouterWithPrecheck is newUpdateTestRouter plus the pre-update
// preflight gate; the plain helper keeps the preflight-less tests reading
// short (nil skips the gate, mirroring a router assembled without one).
func newUpdateTestRouterWithPrecheck(mode string, precheck UpdatePrecheck, fake *fakeUpdate) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/admin/system/update", PostSystemUpdate(mode, precheck, fake.apply, fake.restart))
	return r
}

func postUpdate(t *testing.T, r *gin.Engine) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/system/update", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPostSystemUpdateAppliesAndRestarts(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9", BackupPath: "/x/yolorouter.bak"}}
	r := newUpdateTestRouter(selfupdate.ModeInPlace, fake)

	w := postUpdate(t, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body %s", w.Code, w.Body.String())
	}
	data := decodeEnvelopeData(t, w.Body.Bytes())
	assertField(t, data, "status", "updated")
	assertField(t, data, "target", "v9.9.9")
	if fake.applyCalls != 1 {
		t.Fatalf("apply called %d times, want 1", fake.applyCalls)
	}
	if fake.restartCalls != 1 {
		t.Fatalf("restart called %d times, want 1 — without it the new binary never starts serving", fake.restartCalls)
	}
}

func TestPostSystemUpdateUpToDateSkipsRestart(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v1.0.0", UpToDate: true}}
	r := newUpdateTestRouter(selfupdate.ModeInPlace, fake)

	w := postUpdate(t, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body %s", w.Code, w.Body.String())
	}
	data := decodeEnvelopeData(t, w.Body.Bytes())
	assertField(t, data, "status", "up_to_date")
	if fake.restartCalls != 0 {
		t.Fatalf("an up-to-date result must not restart the process (restart called %d times)", fake.restartCalls)
	}
}

func TestPostSystemUpdateRefusesEveryNonInPlaceMode(t *testing.T) {
	for _, mode := range []string{
		selfupdate.ModeContainer,
		selfupdate.ModeWindows,
		selfupdate.ModeDisabled,
		selfupdate.ModeDevBuild,
		selfupdate.ModeCapabilities,
	} {
		t.Run(mode, func(t *testing.T) {
			fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
			r := newUpdateTestRouter(mode, fake)

			w := postUpdate(t, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("mode %q: expected 400, got %d, body %s", mode, w.Code, w.Body.String())
			}
			var env struct {
				Code int `json:"code"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if env.Code != errcode.SystemUpdateUnsupported {
				t.Fatalf("mode %q: envelope code = %d, want %d", mode, env.Code, errcode.SystemUpdateUnsupported)
			}
			// The refusal must happen before any update work: a container or
			// windows process must never even attempt a binary replacement.
			if fake.applyCalls != 0 || fake.restartCalls != 0 {
				t.Fatalf("mode %q: apply/restart ran (%d/%d), must both be 0", mode, fake.applyCalls, fake.restartCalls)
			}
		})
	}
}

// TestPostSystemUpdateSurvivesClientDisconnect pins the commitment-point
// contract: a caller whose connection is already gone (cancelled request
// context) must not abort the in-flight update — the apply must receive a
// context that is still alive.
func TestPostSystemUpdateSurvivesClientDisconnect(t *testing.T) {
	var applyCtxErr error
	gin.SetMode(gin.TestMode)
	r := gin.New()
	restarted := 0
	r.POST("/api/admin/system/update", PostSystemUpdate(selfupdate.ModeInPlace, nil,
		func(ctx context.Context) (selfupdate.Result, error) {
			applyCtxErr = ctx.Err()
			return selfupdate.Result{Target: "v9.9.9"}, nil
		},
		func() { restarted++ },
	))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/system/update", nil).WithContext(cancelled)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if applyCtxErr != nil {
		t.Fatalf("apply saw a dead context (%v); a dropped connection must not abort the update", applyCtxErr)
	}
	if restarted != 1 {
		t.Fatalf("restart called %d times, want 1 — the update completed and must still restart", restarted)
	}
}

// TestPostSystemUpdateGateBlocksSecondRunAfterSuccess pins the rollback-
// backup protection: once one update applied, every later POST (until the
// process restarts) must be refused without running apply — a second apply
// would overwrite <exe>.bak with the already-new binary.
func TestPostSystemUpdateGateBlocksSecondRunAfterSuccess(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouter(selfupdate.ModeInPlace, fake)

	if w := postUpdate(t, r); w.Code != http.StatusOK {
		t.Fatalf("first update: expected 200, got %d", w.Code)
	}

	w := postUpdate(t, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("second update before restart: expected 409, got %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Code != errcode.SystemUpdateInProgress {
		t.Fatalf("envelope code = %d, want %d", env.Code, errcode.SystemUpdateInProgress)
	}
	if fake.applyCalls != 1 {
		t.Fatalf("apply ran %d times, want 1 — the second run would destroy the rollback backup", fake.applyCalls)
	}
}

// TestPostSystemUpdateGateReopensAfterFailure: a failed run replaced
// nothing, so the operator must be able to retry.
func TestPostSystemUpdateGateReopensAfterFailure(t *testing.T) {
	fake := &fakeUpdate{err: errors.New("download blew up")}
	r := newUpdateTestRouter(selfupdate.ModeInPlace, fake)

	if w := postUpdate(t, r); w.Code != http.StatusInternalServerError {
		t.Fatalf("failed update: expected 500, got %d", w.Code)
	}

	fake.err = nil
	fake.result = selfupdate.Result{Target: "v9.9.9"}
	if w := postUpdate(t, r); w.Code != http.StatusOK {
		t.Fatalf("retry after failure must be allowed, got %d, body %s", postUpdate(t, r).Code, w.Body.String())
	}
	if fake.applyCalls != 2 {
		t.Fatalf("apply ran %d times, want 2 (initial failure + successful retry)", fake.applyCalls)
	}
}

// TestPostSystemUpdateUnsupportedMessageCarriesGuidance pins the endpoint
// contract for bare API clients: the refusal message itself must say what
// to do in this runtime, not just that the runtime is unsupported.
func TestPostSystemUpdateUnsupportedMessageCarriesGuidance(t *testing.T) {
	cases := map[string]string{
		selfupdate.ModeContainer:    "pull the newer image",
		selfupdate.ModeWindows:      "download the latest release",
		selfupdate.ModeCapabilities: "file capabilities",
		selfupdate.ModeDisabled:     "disabled by configuration",
		selfupdate.ModeDevBuild:     "not a release build",
	}
	for mode, want := range cases {
		t.Run(mode, func(t *testing.T) {
			fake := &fakeUpdate{}
			r := newUpdateTestRouter(mode, fake)
			w := postUpdate(t, r)
			var env struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if !strings.Contains(env.Message, want) {
				t.Fatalf("mode %q refusal message must carry guidance containing %q, got %q", mode, want, env.Message)
			}
		})
	}
}

func TestPostSystemUpdateFailureReturns500AndNoRestart(t *testing.T) {
	fake := &fakeUpdate{err: errors.New("checksum verification failed, not replacing")}
	r := newUpdateTestRouter(selfupdate.ModeInPlace, fake)

	w := postUpdate(t, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Code != errcode.SystemUpdateFailed {
		t.Fatalf("envelope code = %d, want %d", env.Code, errcode.SystemUpdateFailed)
	}
	// A failed apply must not restart: the old binary is still the one on
	// disk (or the replacement never completed), and bouncing the process
	// would just cause an outage without an upgrade.
	if fake.restartCalls != 0 {
		t.Fatalf("restart ran after a failed apply (%d times)", fake.restartCalls)
	}
}

// --- pre-update disk-space preflight (button-layer interception) ---

// countingProbe is a FreeSpaceProbe double that records consultations and
// reports a scripted free-byte value, so the handler tests drive the
// production preflight (database.NewUpdatePreflight) against fake space
// numbers — and can prove the postgres wiring never consults it at all.
type countingProbe struct {
	consults int
	free     int64
}

func (p *countingProbe) probe(string) (int64, error) {
	p.consults++
	return p.free, nil
}

// precheckRefusalEnvelope decodes the refusal response's full contract:
// status 400, the dedicated code, the registry message, and the structured
// required/free/backup_dir numbers in data.
func precheckRefusalEnvelope(t *testing.T, w *httptest.ResponseRecorder) (requiredBytes, freeBytes float64, backupDir, message string) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("precheck refusal: expected 400, got %d, body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			RequiredBytes float64 `json:"required_bytes"`
			FreeBytes     float64 `json:"free_bytes"`
			BackupDir     string  `json:"backup_dir"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal refusal envelope: %v, body %s", err, w.Body.String())
	}
	if env.Code != errcode.SystemUpdateInsufficientDisk {
		t.Fatalf("refusal envelope code = %d, want %d (distinct from SystemUpdateFailed=%d)",
			env.Code, errcode.SystemUpdateInsufficientDisk, errcode.SystemUpdateFailed)
	}
	if env.Message != errcode.GetMessage(errcode.SystemUpdateInsufficientDisk) {
		t.Fatalf("refusal message = %q, want the registry text %q", env.Message, errcode.GetMessage(errcode.SystemUpdateInsufficientDisk))
	}
	if env.Data.RequiredBytes <= 0 || env.Data.FreeBytes < 0 {
		t.Fatalf("refusal data must carry the numbers, got %+v", env.Data)
	}
	return env.Data.RequiredBytes, env.Data.FreeBytes, env.Data.BackupDir, env.Message
}

// TestPostSystemUpdatePrecheckRefusalContract pins the refusal response
// contract: a preflight rejection must answer with the dedicated error
// code, the exact required/free numbers in data, and must not have run
// apply — the operator learns the space problem before anything downloads.
func TestPostSystemUpdatePrecheckRefusalContract(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouterWithPrecheck(selfupdate.ModeInPlace, func() error {
		return &database.PrecheckRejectedError{
			RequiredBytes: 10,
			FreeBytes:     9,
			BackupDir:     "/data/backups/pre-migration",
		}
	}, fake)

	w := postUpdate(t, r)
	required, free, dir, _ := precheckRefusalEnvelope(t, w)
	if required != 10 || free != 9 {
		t.Fatalf("refusal numbers = required %v / free %v, want 10 / 9", required, free)
	}
	if dir != "/data/backups/pre-migration" {
		t.Fatalf("refusal backup_dir = %q, want the precheck's directory", dir)
	}
	if fake.applyCalls != 0 || fake.restartCalls != 0 {
		t.Fatalf("a refused update must not run apply/restart (ran %d/%d)", fake.applyCalls, fake.restartCalls)
	}
}

// TestPostSystemUpdatePrecheckPassApplies: sufficient space flows straight
// through to the update itself.
func TestPostSystemUpdatePrecheckPassApplies(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouterWithPrecheck(selfupdate.ModeInPlace, func() error { return nil }, fake)

	w := postUpdate(t, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body %s", w.Code, w.Body.String())
	}
	data := decodeEnvelopeData(t, w.Body.Bytes())
	assertField(t, data, "status", "updated")
	if fake.applyCalls != 1 {
		t.Fatalf("apply called %d times, want 1 — enough space must not hold the update back", fake.applyCalls)
	}
}

// TestPostSystemUpdateGateReopensAfterPrecheckRefusal: a refusal replaced
// nothing and downloaded nothing, so freeing space and clicking again must
// work against the same process.
func TestPostSystemUpdateGateReopensAfterPrecheckRefusal(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	rejected := true
	r := newUpdateTestRouterWithPrecheck(selfupdate.ModeInPlace, func() error {
		if rejected {
			return &database.PrecheckRejectedError{RequiredBytes: 10, FreeBytes: 9, BackupDir: "/data/backups/pre-migration"}
		}
		return nil
	}, fake)

	if w := postUpdate(t, r); w.Code != http.StatusBadRequest {
		t.Fatalf("first (refused) update: expected 400, got %d, body %s", w.Code, w.Body.String())
	}
	rejected = false
	if w := postUpdate(t, r); w.Code != http.StatusOK {
		t.Fatalf("retry after freeing space: expected 200, got %d, body %s", w.Code, w.Body.String())
	}
	if fake.applyCalls != 1 {
		t.Fatalf("apply ran %d times, want 1 (only the successful retry)", fake.applyCalls)
	}
}

// TestPostSystemUpdatePrecheckUnexpectedErrorContinues: the preflight's
// only refusal is the disk-space rejection; any other error out of the
// check itself must not become a new way to fail updates — the startup
// precheck still guards the actual backup.
func TestPostSystemUpdatePrecheckUnexpectedErrorContinues(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouterWithPrecheck(selfupdate.ModeInPlace, func() error {
		return errors.New("preflight bookkeeping exploded")
	}, fake)

	w := postUpdate(t, r)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (an informative precheck is not a failure source), got %d, body %s", w.Code, w.Body.String())
	}
	if fake.applyCalls != 1 {
		t.Fatalf("apply called %d times, want 1", fake.applyCalls)
	}
}

// TestPostSystemUpdateNilPrecheckSkipsGate: a router assembled without a
// preflight (router tests, future assembly variants) simply skips the gate.
func TestPostSystemUpdateNilPrecheckSkipsGate(t *testing.T) {
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouter(selfupdate.ModeInPlace, fake)

	if w := postUpdate(t, r); w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body %s", w.Code, w.Body.String())
	}
	if fake.applyCalls != 1 {
		t.Fatalf("apply called %d times, want 1", fake.applyCalls)
	}
}

// TestPostSystemUpdateRotatesOldBackupsBeforeRefusing pins the
// "rotate first, then size" order at the real entry point: five preset
// old backups plus a post-rotation-still-insufficient probe must leave
// exactly the newest three behind AND refuse — proving the refusal came
// after the program tidied up after itself, not instead of it.
func TestPostSystemUpdateRotatesOldBackupsBeforeRefusing(t *testing.T) {
	dataDir := t.TempDir()
	sqlitePath := filepath.Join(dataDir, "yolorouter.db")
	// 4 bytes of database: the estimate is (4*5+1)/2 = 10 bytes, so a probe
	// reporting 9 free is one byte short even after rotation.
	if err := os.WriteFile(sqlitePath, []byte("abcd"), 0o600); err != nil {
		t.Fatalf("stage sqlite file: %v", err)
	}
	backupDir := filepath.Join(dataDir, "backups", "pre-migration")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatalf("stage backup dir: %v", err)
	}
	// The rotation ranks by filename version alone, so plain payload bytes
	// are enough to stage history (no real gzip needed here).
	for v := 1; v <= 5; v++ {
		if err := os.WriteFile(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v)),
			[]byte(fmt.Sprintf("old snapshot v%d", v)), 0o600); err != nil {
			t.Fatalf("stage old backup v%d: %v", v, err)
		}
	}

	probe := &countingProbe{free: 9}
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouterWithPrecheck(selfupdate.ModeInPlace,
		database.NewUpdatePreflight("sqlite", nil, sqlitePath, probe.probe), fake)

	w := postUpdate(t, r)
	required, free, dir, _ := precheckRefusalEnvelope(t, w)
	if required != 10 || free != 9 {
		t.Fatalf("refusal numbers = required %v / free %v, want 10 / 9", required, free)
	}
	if dir != backupDir {
		t.Fatalf("refusal backup_dir = %q, want %q", dir, backupDir)
	}
	if probe.consults != 1 {
		t.Fatalf("probe consulted %d time(s), want 1 (single estimate, single probe)", probe.consults)
	}
	if fake.applyCalls != 0 {
		t.Fatalf("a refused update must not run apply (ran %d times)", fake.applyCalls)
	}
	for _, v := range []int{1, 2} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); !os.IsNotExist(err) {
			t.Fatalf("expected sqlite_v%d.db.gz to be rotated away before the refusal, stat err = %v", v, err)
		}
	}
	for _, v := range []int{3, 4, 5} {
		if _, err := os.Stat(filepath.Join(backupDir, fmt.Sprintf("sqlite_v%d.db.gz", v))); err != nil {
			t.Fatalf("expected sqlite_v%d.db.gz to survive the rotation: %v", v, err)
		}
	}
}

// TestPostSystemUpdatePostgresPreflightSkipsPrecheck guards the postgres
// exemption at the entry point itself: wired with the production preflight
// for a non-sqlite driver (and a real leftover sqlite file a mistaken
// wiring would size), the update must go through untouched — the armed
// probe never consulted, apply running exactly once.
func TestPostSystemUpdatePostgresPreflightSkipsPrecheck(t *testing.T) {
	// A leftover sqlite database from before a postgres switch, kept away
	// from any watched directory: a precheck mistakenly wired into the
	// non-sqlite path must size this file and consult the armed probe.
	leftoverDir := t.TempDir()
	leftoverPath := filepath.Join(leftoverDir, "yolorouter.db")
	if err := os.WriteFile(leftoverPath, []byte("leftover sqlite bytes"), 0o600); err != nil {
		t.Fatalf("stage leftover sqlite file: %v", err)
	}

	probe := &countingProbe{free: 0}
	fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9"}}
	r := newUpdateTestRouterWithPrecheck(selfupdate.ModeInPlace,
		database.NewUpdatePreflight("postgres", nil, leftoverPath, probe.probe), fake)

	w := postUpdate(t, r)
	if w.Code != http.StatusOK {
		t.Fatalf("postgres deployment: expected the update to proceed, got %d, body %s", w.Code, w.Body.String())
	}
	if fake.applyCalls != 1 {
		t.Fatalf("apply called %d times, want 1", fake.applyCalls)
	}
	if probe.consults != 0 {
		t.Fatalf("the space probe was consulted %d time(s) on a postgres deployment, want 0", probe.consults)
	}
}
