package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter-ce/internal/router"
	"github.com/yolorouter/yolorouter-ce/pkg/database"
	"github.com/yolorouter/yolorouter-ce/pkg/logger"
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

	sqlDB, err := app.DB.DB()
	if err != nil {
		return err
	}
	migrationsFS, dir := migrationsFor(app.Config.Database.Driver)
	if err := database.RunMigrations(sqlDB, app.Config.Database.Driver, migrationsFS, dir); err != nil {
		return fmt.Errorf("startup migration failed: %w", err)
	}

	r := router.New()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", app.Config.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		WriteTimeout:      0,
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
