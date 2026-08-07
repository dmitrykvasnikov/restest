// Command restest runs the mock REST API server.
//
// This file does one thing: wire the pieces together in the right order and
// take them down again in the reverse one. Everything it starts is stopped
// before it returns.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/config"
	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/database"
	"github.com/dmitrykvasnikov/restest/internal/logging"
	"github.com/dmitrykvasnikov/restest/internal/mock"
	"github.com/dmitrykvasnikov/restest/internal/web"
)

// Timeouts on the HTTP server itself, guarding against a client that opens a
// connection and then dawdles. M7 revisits these.
//
// WriteTimeout stays where it is now that the log tail streams: that one
// response lifts its own write deadline through http.ResponseController, which
// is the same trick a delayed mock endpoint uses, and is better than weakening
// the guard on every route to accommodate two.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

// logMaintenanceInterval is how often partitions are created and expired ones
// dropped. Daily rather than monthly: a process restarted often would otherwise
// be the only thing that ever ran it, and one never restarted would run it once.
const logMaintenanceInterval = 24 * time.Hour

func main() {
	if err := run(context.Background()); err != nil {
		// The configured logger may not exist yet — this is where a bad
		// configuration surfaces — so this last word goes out on its own.
		logging.New(os.Stderr, slog.LevelError, logging.FormatJSON).
			Error("restest exited", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// A second signal is left to the runtime: if shutdown wedges, the operator
	// can still ^C out of it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}

	logger := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)
	logger.Info("starting restest",
		slog.String("revision", revision()),
		slog.Any("config", cfg),
	)

	pool, err := database.Open(ctx, cfg.DatabaseURL, cfg.DatabaseMaxConns, logger)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer pool.Close()

	// Before the listener opens: an instance never serves traffic against a
	// schema older than the code.
	if err := database.Migrate(ctx, cfg.DatabaseURL, logger); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	store := core.NewStore(pool)

	sessions, stopSessionCleanup := web.NewSessionManager(pool, cfg.SecureCookies())
	defer stopSessionCleanup()

	// The route table is built before the listener opens. An instance that
	// started serving with an empty table would answer "no such project" to
	// every mock request until the first edit rebuilt it.
	matcher := mock.NewRouter(store, logger)
	if err := matcher.Reload(ctx); err != nil {
		return fmt.Errorf("route table: %w", err)
	}
	go matcher.Refresh(ctx, mock.RefreshInterval)

	// The request log. The recorder is started before the listener opens so
	// that no request can arrive to find a queue nobody is draining, and it is
	// stopped after the server has shut down, so the last requests served are
	// still written. Its context is separate from the one the signal cancels,
	// for exactly that reason.
	recorderCtx, stopRecorder := context.WithCancel(context.Background())
	defer stopRecorder()

	recorder := core.NewRecorder(store, logger, core.RecorderOptions{Buffer: cfg.LogBuffer})
	go recorder.Run(recorderCtx)

	// Partition maintenance: next months created, expired ones detached and
	// dropped. It runs once at startup and then daily, and a failure is logged
	// rather than fatal — the default partition means writes keep working
	// either way.
	go store.MaintainExchangeLogLoop(ctx, logMaintenanceInterval, cfg.LogRetentionMonths, logger)

	app, err := web.New(web.Options{
		Logger:             logger,
		Store:              store,
		Sessions:           sessions,
		Routes:             matcher,
		BaseURL:            cfg.BaseURL,
		Recorder:           recorder,
		LogBodyLimit:       cfg.LogBodyLimit,
		LogRetentionMonths: cfg.LogRetentionMonths,
	})
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		stop() // restore default signal handling from here on
	}

	logger.Info("shutting down", slog.Duration("timeout", cfg.ShutdownTimeout))

	// Not the cancelled ctx: shutdown needs a deadline of its own, otherwise
	// the signal that asked for it would cancel it immediately.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()

	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		// Requests still running when the deadline passed. Drop them rather
		// than hang: the process was asked to leave.
		_ = srv.Close()
	}

	// After the server, before the pool: the exchanges queued by the requests
	// that just finished are written by a goroutine that needs the pool to
	// still be open, and `defer pool.Close()` runs after this function returns.
	stopRecorder()
	recorder.Wait()

	if shutdownErr != nil {
		return fmt.Errorf("graceful shutdown: %w", shutdownErr)
	}

	logger.Info("stopped")
	return nil
}

// stampedRevision is set at link time by the container build, which has no
// .git directory to read. See the REVISION build argument in the Dockerfile.
var stampedRevision string

// revision reports the commit the binary was built from, answering "which build
// is this?" without a version number anyone has to remember to bump. A plain
// `go build` in the repository stamps it automatically.
func revision() string {
	if stampedRevision != "" {
		return stampedRevision
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	switch {
	case rev == "":
		return "unknown"
	case modified == "true":
		// Built from a working tree with uncommitted changes: the commit alone
		// would not identify what is actually running.
		return rev + "-dirty"
	default:
		return rev
	}
}
