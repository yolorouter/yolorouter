package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/pricecatalog"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/router"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/provider"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/database"
	"github.com/yolorouter/yolorouter/pkg/logger"
	"github.com/yolorouter/yolorouter/web"
)

func runServe(ctx context.Context, args []string) error {
	_, app, err := bootstrapCommand("serve", args, 0, nil)
	if err != nil {
		return err
	}

	// serve holds the exclusive instance lock for its entire lifetime so that
	// db:reset (which must acquire the same lock) fails fast instead of racing
	// a live server for the database file.
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

	// Beyond the shared instance lock above, serve holds a dedicated
	// serve-liveness lock for its entire lifetime. Only a running serve takes
	// this lock (db:reset and other maintenance commands take just the instance
	// lock), so `stop` can use it to tell "a server is running" apart from
	// "some maintenance command is holding the instance lock" — a distinction
	// the pid file alone cannot make, since a crashed server leaves a stale one.
	serveLockPath := serveInstanceLockPath(app.Config.Database.SQLitePath)
	unlockServe, err := database.AcquireInstanceLock(serveLockPath)
	if err != nil {
		return fmt.Errorf("cannot start: %w", err)
	}
	defer func() { _ = unlockServe() }()

	// Stand up the platform stop waiter (a no-op on unix, a named event on
	// windows), then record our pid so `stop` knows which process to signal.
	//
	// This order is load-bearing on windows. `stop` treats a readable pid file
	// as "a server is live, signal it" and its signalStop is a deliberate
	// silent no-op when the named event does not exist yet. Publishing the pid
	// first would therefore leave a window where a stop request is accepted,
	// delivered nowhere, and reported to the operator as a 20s timeout while
	// the server keeps running — and windows has no SIGTERM fallback to catch
	// it. Creating the event first closes that window: once the pid is visible,
	// the delivery channel already exists.
	//
	// Teardown stays correct under this order too. Both defers are registered
	// after the serve-unlock defer, so LIFO runs them first: the pid file is
	// removed and the event handle closed while the serve lock is still held,
	// which is what lets a held serve lock imply a valid pid file.
	stopCh, stopCleanup, err := newStopWaiter(app.Config.Database.SQLitePath)
	if err != nil {
		return fmt.Errorf("init stop waiter: %w", err)
	}
	defer stopCleanup()

	pidPath := instancePIDPath(app.Config.Database.SQLitePath)
	if err := writePIDFile(pidPath); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	defer func() { _ = os.Remove(pidPath) }()

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

	// Stream sent-SSE bodies live under data/bodies/, next to the
	// per-deployment data directory. Config always resolves SQLitePath to an
	// absolute path alongside the config file (and defaults it) regardless of
	// driver, so its parent doubles as the stable data dir even on Postgres —
	// the same convention instanceLockPath relies on. Created on boot so the
	// gateway's stream capture can append without a per-request MkdirAll; the
	// absolute path is threaded through router.New so the gateway package (no
	// direct config access) can resolve it via the request context
	// (internal/gateway/stream.go).
	bodiesDir := filepath.Join(filepath.Dir(app.Config.Database.SQLitePath), "bodies")
	if err := os.MkdirAll(bodiesDir, 0o755); err != nil {
		return fmt.Errorf("create bodies dir: %w", err)
	}

	// Background probe queue: bulk import stores mappings disabled+untested
	// and hands them here; each probe that passes auto-enables its mapping.
	// The queue gets its own ModelService (services are stateless; the router
	// builds its own instances the same way). Signal-driven shutdown does NOT
	// cancel serve's ctx (signals land on a channel, see below), so
	// gracefulShutdown stops the queue explicitly inside its budget; this
	// bounded deferred stop only matters for the error returns before that,
	// where the just-started queue is idle and stops instantly. Bounded rather
	// than Stop() because an unbounded wait on a stuck probe would hold the
	// exiting process past any supervisor grace.
	// Constructed here because router.New needs the handle, but NOT started
	// yet: the queue probes upstreams and writes verdicts, and none of that
	// may happen before the startup checks below (migration, master-key
	// fingerprint) have passed — a worker running with a wrong master key
	// would stamp queued rows with abandonment notes moments before the
	// fingerprint check aborts the boot, and those rows would no longer be
	// recovered once the key is fixed.
	probeSvc := modeladmin.NewModelService(app.DB, crypto.NewSecretBox(masterKey),
		providerclient.NewHTTPProviderClient(app.Config.Security.AllowPrivateUpstreams))
	probeQueue := modeladmin.NewProbeQueue(probeSvc, modeladmin.DefaultProbeWorkers)
	// Disarmed once gracefulShutdown takes over: that path stops the queue
	// inside the 15s budget itself, and letting this defer wait ANOTHER second
	// afterwards would push a worst-case shutdown past the stated budget.
	queueStopHandedOff := false
	defer func() {
		if !queueStopHandedOff {
			probeQueue.StopWithin(time.Second)
		}
	}()

	// The gateway's vision-fallback capability calls back into this server's
	// own API; the loopback base is derived from the same port the server is
	// about to listen on.
	loopbackBase := fmt.Sprintf("http://localhost:%d", app.Config.Server.Port)
	r, err := router.New(router.Deps{
		DB:                    app.DB,
		ProviderMasterKey:     masterKey,
		BodiesDir:             bodiesDir,
		Update:                app.Config.Update,
		AllowPrivateUpstreams: app.Config.Security.AllowPrivateUpstreams,
		Gateway:               app.Config.Gateway,
		LoopbackBase:          loopbackBase,
		ExternalURL:           app.Config.Server.ExternalURL,
		ProbeQueue:            probeQueue,
	})
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
	// Schema upgrades reach this point unattended (in-app update restart,
	// Docker image pull), so migrating goes through MigrateWithBackup: on
	// SQLite it snapshots the database before applying anything, and refuses
	// to migrate if that snapshot cannot be written. The instance lock held
	// above makes version check, backup, and migration one critical section.
	migrationsFS, dir := migrationsFor(app.Config.Database.Driver)
	backupPath, err := database.MigrateWithBackup(sqlDB, app.Config.Database.Driver, app.Config.Database.SQLitePath, migrationsFS, dir)
	if err != nil {
		return fmt.Errorf("startup migration failed: %w", err)
	}
	if backupPath != "" {
		logger.Info("pre-migration database backup available", zap.String("path", backupPath))
	}

	// nil ProviderClient: VerifyMasterKeyFingerprint only touches s.db/
	// s.secrets, never s.client, so this avoids allocating a second,
	// independent HTTPProviderClient (its own semaphore + http.Transport)
	// purely for a startup DB+crypto check — router.New builds the one
	// real instance that actually serves provider-test traffic.
	fingerprintSvc := provider.NewProviderService(app.DB, crypto.NewSecretBox(masterKey), nil)
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
		// WriteTimeout is non-zero so non-streaming endpoints are protected
		// against slow-read clients (a slow client could otherwise hold a
		// handler and its concurrency slot open indefinitely). Streaming
		// deliveries slide the deadline forward before each Write/Flush,
		// bounding a slow client without clearing the server WriteTimeout
		// entirely; the
		// request-level context timeout (gateway.RequestTimeout) remains as
		// the streaming backstop.
		// The value is RequestTimeout + a slack so a non-streaming handler
		// always has room to write its response even if the upstream consumed
		// the full request budget, and so a streaming request has coverage
		// during the pre-first-write gap (e.g. a long TTFT). The slack is
		// derived from protocols.StreamWriteWindow() — not a separate
		// hard-coded literal — because it MUST stay >= that window: once a
		// stream starts writing, every chunk slides this server-level
		// WriteTimeout forward, but the very
		// first slide only happens after the pre-first-write gap this slack
		// covers — a slack shorter than the write window would let the
		// global WriteTimeout fire before the first slide lands. Deriving
		// from the same source keeps the invariant true even if
		// streamWriteWindow's value ever changes.
		WriteTimeout: app.Config.Gateway.RequestTimeout + protocols.StreamWriteWindow(),
	}

	// Empty task supervisor. No real periodic task exists yet, so there is
	// nothing for taskWG to Add/Done against. When a later module (e.g. log
	// retention) introduces a real background goroutine, it should create its
	// own cancellable context at that point and register with taskWG here.
	var taskWG sync.WaitGroup

	// Background price-catalog refresh. The default endpoint (set in config
	// defaults()) is the live distribution Worker, so every instance warms and
	// re-fetches on the configured interval with zero opt-in. An operator who
	// sets the endpoint to "" disables refresh: StartRefresh returns nil, no
	// goroutine is spawned, and the instance keeps serving the embedded seed.
	// A failed fetch never wipes a warm index (only cover, never delete), so a
	// flaky endpoint cannot degrade pricing.
	//
	// The refresh runs on a context derived from serve's own ctx, so the same
	// shutdown signal (SIGTERM / external stop / ctx cancel) that stops the HTTP
	// server also stops the refresh. The returned stop is awaited via deferred
	// cancel + stop() so the goroutine exits before the process does rather than
	// leaking for its lifetime — harmless for the real CLI (which exits right
	// after) but a real leak for tests/embedders that call runServe repeatedly.
	priceCtx, priceCancel := context.WithCancel(ctx)
	stopPriceRefresh := pricecatalog.StartRefresh(
		priceCtx,
		app.Config.PriceCatalog.Endpoint,
		app.Config.PriceCatalog.RefreshInterval,
	)
	defer func() {
		priceCancel()
		// stop is nil for an empty endpoint (no goroutine to wait on); calling a
		// nil func would panic, so guard it.
		if stopPriceRefresh != nil {
			stopPriceRefresh()
		}
	}()

	// Bind the listener up front rather than letting ListenAndServe do it
	// inside the goroutine: ln.Addr().String() yields the actually-bound
	// address (e.g. [::]:8080) for the startup log, and a bind failure —
	// typically a port already in use — surfaces synchronously here instead
	// of arriving asynchronously through serveErrCh after the rest of
	// startup has already run.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	// url is a clickable localhost link for quick local access; addr is the
	// raw bound socket (e.g. [::]:8070) showing which interface the kernel
	// actually bound — useful when diagnosing dual-stack or remote access,
	// since ":port" binds every interface, not just loopback. lan (when
	// present) is a clickable URL using the host's primary LAN IPv4 so
	// other devices on the same network can reach the server; it is
	// omitted when no usable IPv4 can be determined.
	fields := []zap.Field{
		zap.String("url", fmt.Sprintf("http://localhost:%d", app.Config.Server.Port)),
		zap.String("addr", ln.Addr().String()),
	}
	if lan := lanListenURL(app.Config.Server.Port); lan != "" {
		fields = append(fields, zap.String("lan", lan))
	}
	logger.Info("http server listening", fields...)

	// Only now — schema migrated, master key verified, listener bound — may
	// the queue touch upstreams or rows: every startup failure mode that can
	// still abort the boot (a port already in use, above) has passed, so no
	// probe spends upstream calls or writes verdicts for a process that then
	// exits. The in-memory queue dies with the process; the untested-
	// unstamped ARMED rows in the database are its durable form, and
	// recovering them here turns every way a queued probe can be lost — a
	// crash, a shutdown racing an import's enqueue — into a self-healing
	// restart. Failure is non-fatal: the rows stay recoverable by re-import,
	// and the server is still fully usable.
	probeQueue.Start(ctx)
	probeQueue.RecoverPendingInBackground()

	serveErrCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
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
	case <-stopCh:
		logger.Info("external stop requested")
	case <-ctx.Done():
		// Derived from the ctx passed in by main() (via dispatch), not
		// context.Background(), so a caller-cancelled ctx (e.g. in a test)
		// triggers shutdown the same way a real OS signal would.
		logger.Info("shutdown context cancelled")
	}

	queueStopHandedOff = true
	return gracefulShutdown(srv, &taskWG, probeQueue, sigCh)
}

func gracefulShutdown(srv *http.Server, taskWG *sync.WaitGroup, probeQueue *modeladmin.ProbeQueue, sigCh <-chan os.Signal) error {
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

	// Phase 1b: cancel in-flight probes NOW and collect the workers later,
	// inside the budget. Signals land on a channel rather than cancelling
	// serve's ctx, so nothing else stops the queue during shutdown — without
	// this, probes would keep issuing upstream calls through the HTTP drain
	// and the deferred stop would wait for them only after the budget was
	// already spent. Stopping runs concurrently with the drain: probes are
	// outbound work with no relation to in-flight HTTP requests.
	queueStopped := make(chan struct{})
	go func() {
		probeQueue.Stop()
		close(queueStopped)
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
	// all current builds, which have no real background tasks) — no need for a
	// separate fixed-duration "drain" phase on top of this.
	//
	// Known limitation: if taskWG.Wait() itself never returns (a future
	// module's background task hangs and never calls Done()), the goroutine
	// below outlives the phaseTimeout select and leaks for the life of the
	// process. Harmless for the real CLI (which exits shortly after this
	// function returns), but whichever module first registers a real
	// long-lived task against taskWG should give that task its own
	// cancellable context so this can be select'd on a done signal instead —
	// there's nothing to cancel yet, so that plumbing doesn't exist here.
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

	// Phase 3b: collect the probe workers cancelled in phase 1b. Usually
	// instant — cancellation aborts their upstream calls — but a probe stuck
	// in a call that ignores it must not hold the process past the budget:
	// the leaked worker dies with the process moments later.
	select {
	case <-queueStopped:
	case <-time.After(phaseTimeout(2 * time.Second)):
		logger.Error("probe workers did not exit within budget, continuing shutdown")
	}

	// Every phase above already logs its own errors as they happen (each
	// phase's timeout/failure is "log a warning and keep going", not a
	// fatal top-level failure) — the process still shut down, just not
	// entirely gracefully, so this returns nil rather than surfacing
	// shutdownErr as the CLI command's overall result.
	logger.Info("shutdown complete")
	return nil
}

// lanListenURL returns an http URL rooted at the host's primary LAN IPv4
// (e.g. "http://192.168.1.5:8080") so other devices on the same network
// can reach the server. It returns "" when no usable IPv4 is found, in
// which case the caller omits the field rather than logging a misleading
// address.
//
// The address is obtained by probing the default-route egress interface:
// opening a UDP "connection" to a public address sends no packets — the
// kernel only resolves the route and reports the source IP it would use.
// The default route points at the real network adapter (Wi-Fi/Ethernet),
// so virtual bridges such as Docker/VM interfaces are naturally skipped
// because they are never the default egress. If there is no default route
// (offline host, no gateway), the probe fails and "" is returned.
func lanListenURL(port int) string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	a, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	v4 := a.IP.To4()
	if v4 == nil {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", v4.String(), port)
}
