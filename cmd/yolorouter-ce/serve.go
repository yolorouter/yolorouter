package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter-ce/internal/router"
	"github.com/yolorouter/yolorouter-ce/internal/service"
	"github.com/yolorouter/yolorouter-ce/pkg/crypto"
	"github.com/yolorouter/yolorouter-ce/pkg/database"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
	"github.com/yolorouter/yolorouter-ce/web"
)

func runServe(ctx context.Context, args []string) error {
	_, app, err := bootstrapCommand("serve", args, 0, nil)
	if err != nil {
		return err
	}

	// serve holds the exclusive instance lock for its entire lifetime so that
	// db:reset (which must acquire the same lock) fails fast instead of racing
	// a live server for the database file (design doc §2.2 / §14 criterion 10).
	lockPath := instanceLockPath(app.Config.Database.SQLitePath)
	unlockInstance, err := database.AcquireInstanceLock(lockPath)
	if err != nil {
		_ = app.Close()
		return fmt.Errorf("cannot start: %w", err)
	}
	// Deferred in this order (defer runs LIFO) so the database connection is
	// fully closed *before* the instance lock is released — registering
	// app.Close's defer first, as an earlier version did, made unlockInstance
	// run first instead, opening a window where db:reset could acquire the
	// now-free lock and start deleting the database file while this
	// process's connection might still be mid-close.
	defer func() { _ = unlockInstance() }()
	defer func() { _ = app.Close() }()

	// router.New validates the embedded frontend build (if any) and must run
	// before any database migration: it has no side effects of its own, so
	// rejecting a broken embedded build here guarantees a bad deploy never
	// commits schema changes first. Running it after RunMigrations would let
	// a broken artifact push the database forward and then exit, leaving a
	// migrated-but-unreachable instance behind for whatever runs next to deal
	// with.
	masterKey, err := crypto.KeyFromBase64(app.Config.Security.ProviderMasterKey)
	if err != nil {
		return fmt.Errorf("decode provider master key: %w", err)
	}

	// M6.2: stream sent-SSE bodies live under data/bodies/ (sibling of the
	// sqlite file, same convention instanceLockPath already uses). Created
	// on boot so the gateway's stream capture can append without a
	// per-request MkdirAll; the absolute path is threaded through
	// router.New so the gateway package (no direct config access) can
	// resolve it via the request context (internal/gateway/stream.go).
	bodiesDir := filepath.Join(filepath.Dir(app.Config.Database.SQLitePath), "bodies")
	if err := os.MkdirAll(bodiesDir, 0o755); err != nil {
		return fmt.Errorf("create bodies dir: %w", err)
	}

	r, err := router.New(app.DB, masterKey, bodiesDir)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}
	// isReleaseBuild is only true for -tags release; router.New() alone
	// doesn't reject an empty (no frontend) distFS, since that's the
	// correct, expected state for a plain build. A release binary built
	// without -tags embed would otherwise start, report /healthz healthy,
	// and serve web/placeholder.html for every request instead of the real
	// UI — see release_flag_off.go for why this check lives here instead
	// of inside router.New() itself.
	if isReleaseBuild && !web.HasFrontend() {
		return fmt.Errorf("release build has no embedded frontend: build with `make build-release` (always pairs -tags release with -tags embed), not -tags release alone")
	}

	sqlDB, err := app.DB.DB()
	if err != nil {
		return err
	}
	migrationsFS, dir := migrationsFor(app.Config.Database.Driver)
	if err := database.RunMigrations(sqlDB, app.Config.Database.Driver, migrationsFS, dir); err != nil {
		return fmt.Errorf("startup migration failed: %w", err)
	}

	// nil ProviderClient: VerifyMasterKeyFingerprint only touches s.db/
	// s.masterKey, never s.client, so this avoids allocating a second,
	// independent HTTPProviderClient (its own semaphore + http.Transport)
	// purely for a startup DB+crypto check — router.New builds the one
	// real instance that actually serves provider-test traffic.
	fingerprintSvc := service.NewProviderService(app.DB, masterKey, nil)
	if err := fingerprintSvc.VerifyMasterKeyFingerprint(time.Now().UTC()); err != nil {
		return fmt.Errorf("startup check failed: %w", err)
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", app.Config.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout bounds the whole request (headers + body), not just
		// the headers ReadHeaderTimeout alone covers — without it, a
		// client that sends valid headers and then stalls mid-body could
		// hold a handler (and anything it acquired, e.g.
		// internal/middleware.Semaphore's login concurrency slots) open
		// indefinitely. 30s is generous for the 1MiB admin-JSON cap
		// (middleware.BodySizeLimit) even on a slow connection.
		ReadTimeout:    30 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		WriteTimeout:   0,
	}

	// M0: empty task supervisor. No real periodic task exists yet, so there is
	// nothing for taskWG to Add/Done against. When a later module (e.g. log
	// retention) introduces a real background goroutine, it should create its
	// own cancellable context at that point and register with taskWG here.
	var taskWG sync.WaitGroup

	serveErrCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErrCh <- err
		}
	}()

	// A single persistent registration for the process's whole remaining
	// lifetime (stopped only via the deferred signal.Stop below), rather than
	// two separate signal.NotifyContext calls (one here, one inside
	// gracefulShutdown): NotifyContext's context becomes Done() and
	// self-unregisters the instant it fires, which left a real gap between
	// that unregistration and the second NotifyContext call further down —
	// a SIGTERM landing in that window was silently dropped instead of
	// forcing an exit. The channel is buffered so a signal arriving before
	// gracefulShutdown starts reading it is not lost.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-serveErrCh:
		return fmt.Errorf("http server failed: %w", err)
	case <-sigCh:
		logger.Info("shutdown signal received")
	case <-ctx.Done():
		// Derived from the ctx passed in by main() (via dispatch), not
		// context.Background(), so a caller-cancelled ctx (e.g. in a test)
		// triggers shutdown the same way a real OS signal would.
		logger.Info("shutdown context cancelled")
	}

	return gracefulShutdown(srv, &taskWG, sigCh)
}

func gracefulShutdown(srv *http.Server, taskWG *sync.WaitGroup, sigCh <-chan os.Signal) error {
	const totalBudget = 15 * time.Second
	deadline := time.Now().Add(totalBudget)
	remaining := func() time.Duration {
		d := time.Until(deadline)
		if d < 0 {
			return 0
		}
		return d
	}
	phaseTimeout := func(phase time.Duration) time.Duration {
		if r := remaining(); r < phase {
			return r
		}
		return phase
	}

	// A second signal forces immediate exit. This reuses the single
	// signal.Notify registration from runServe (still active — its deferred
	// signal.Stop only runs once this function returns), so there is no
	// registration gap between "first signal consumed" and "listening for a
	// second one". The watcher must also stop once this function returns on
	// its own (shutdown finished without a second signal) — otherwise it
	// blocks on <-sigCh forever, leaking a goroutine for the rest of the
	// process's life every time (harmless for the real CLI, which exits
	// right after, but a real leak for tests/embedders that call
	// gracefulShutdown repeatedly against a shared process).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigCh:
			os.Exit(1)
		case <-done:
		}
	}()

	// Phase 2: stop accepting new HTTP connections.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), phaseTimeout(10*time.Second))
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		logger.Error("http shutdown timed out, forcing close", zap.Error(shutdownErr))
		if closeErr := srv.Close(); closeErr != nil {
			logger.Error("forced http close also failed", zap.Error(closeErr))
		}
	}

	// Phase 3: wait for background goroutines to finish. taskWG.Wait()
	// returns immediately when no goroutine has ever called Add (the case for
	// all of M0, which has no real background tasks) — no need for a
	// separate fixed-duration "drain" phase on top of this.
	//
	// Known limitation: if taskWG.Wait() itself never returns (a future
	// module's background task hangs and never calls Done()), the goroutine
	// below outlives the phaseTimeout select and leaks for the life of the
	// process. Harmless for the real CLI (which exits shortly after this
	// function returns), but whichever module first registers a real
	// long-lived task against taskWG should give that task its own
	// cancellable context so this can be select'd on a done signal instead —
	// there's nothing to cancel yet in M0, so that plumbing doesn't exist here.
	waitCh := make(chan struct{})
	go func() {
		taskWG.Wait()
		close(waitCh)
	}()
	select {
	case <-waitCh:
	case <-time.After(phaseTimeout(3 * time.Second)):
		logger.Error("background goroutines did not exit within budget, continuing shutdown")
	}

	// Every phase above already logs its own errors as they happen (design
	// doc §9: each phase's timeout/failure is "记录警告，继续往下走", not a
	// fatal top-level failure) — the process still shut down, just not
	// entirely gracefully, so this returns nil rather than surfacing
	// shutdownErr as the CLI command's overall result.
	logger.Info("shutdown complete")
	return nil
}
