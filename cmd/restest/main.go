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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/dmitrykvasnikov/restest/internal/config"
	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/database"
	"github.com/dmitrykvasnikov/restest/internal/logging"
	"github.com/dmitrykvasnikov/restest/internal/metrics"
	"github.com/dmitrykvasnikov/restest/internal/mock"
	"github.com/dmitrykvasnikov/restest/internal/web"
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

	// The shared demo project, provisioned before the listener opens so that
	// /m/demo/ answers the first request rather than the second. A failure here
	// is logged and not fatal: the demo is a convenience, and refusing to serve
	// anybody's mocks because it could not be created would be the wrong trade.
	if cfg.DemoEnabled {
		if demo, err := store.EnsureDemoProject(ctx); err != nil {
			logger.Error("provision the demo project", slog.String("error", err.Error()))
		} else {
			logger.Info("demo project ready",
				slog.String("slug", demo.Slug),
				slog.String("url", cfg.BaseURL+demo.MockPath()),
				slog.Duration("reset_interval", cfg.DemoResetInterval),
			)
			// The route table was built before this, so it does not know about a
			// demo that has just been created.
			if err := matcher.Reload(ctx); err != nil {
				return fmt.Errorf("route table after provisioning the demo: %w", err)
			}
			go store.ResetDemoProjectsLoop(ctx, cfg.DemoResetInterval, logger)
		}
	}

	// Instrumentation. Built before the server, because the server's options
	// carry it, and left nil when metrics are off — which is what makes
	// /metrics unregistered rather than registered and empty.
	var instruments *metrics.Metrics
	if cfg.MetricsEnabled {
		instruments = metrics.New(revision())
	}

	app, err := web.New(web.Options{
		Logger:             logger,
		Store:              store,
		Sessions:           sessions,
		Routes:             matcher,
		BaseURL:            cfg.BaseURL,
		Recorder:           recorder,
		LogBodyLimit:       cfg.LogBodyLimit,
		LogRetentionMonths: cfg.LogRetentionMonths,
		DemoEnabled:        cfg.DemoEnabled,

		Metrics:      metricsOption(instruments),
		MetricsToken: cfg.MetricsToken,

		RateLimitIP:      cfg.RateLimitIP,
		RateLimitProject: cfg.RateLimitProject,
		RateLimitAPI:     cfg.RateLimitAPI,
		TrustedProxies:   cfg.TrustedProxies,
		MaxRequestBody:   cfg.MaxRequestBody,
	})
	if err != nil {
		return fmt.Errorf("web: %w", err)
	}

	// The gauges over state this file can reach and internal/metrics cannot:
	// the recorder's queue, the route table, and the limiters' own tables. They
	// are read at scrape time, so nothing here has to be pushed or kept fresh.
	publishGauges(instruments, recorder, matcher, app)

	// Expired buckets are swept off the limiters' tables for as long as the
	// process runs. Without this they stay bounded — by the cap, which empties
	// the table wholesale — but a sweep is the cheaper of the two mechanisms
	// and the one that should normally be doing the work.
	for _, limiter := range app.Limiters() {
		go limiter.Run(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    cfg.MaxHeaderBytes,
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

// metricsOption turns a possibly-nil *metrics.Metrics into the interface
// web.Options wants.
//
// The dance is the well-known typed-nil trap: assigning a nil *metrics.Metrics
// straight into an interface field produces an interface that is not nil, and
// web.New's "was I given instrumentation?" check would then answer yes and
// register /metrics against a registry that does not exist.
func metricsOption(m *metrics.Metrics) web.Metrics {
	if m == nil {
		return nil
	}
	return m
}

// publishGauges registers the metrics whose values live in other components.
//
// They are gauge and counter functions rather than numbers pushed from those
// components, so that nothing in core, mock or web has to know that Prometheus
// exists — and so that a scrape reports the state at the moment it was taken
// rather than the last time somebody remembered to update it.
func publishGauges(m *metrics.Metrics, recorder *core.Recorder, matcher *mock.Router, app *web.Server) {
	if m == nil {
		return
	}

	m.Gauge("restest_exchange_queue_depth",
		"Exchanges waiting to be written to the request log.", nil,
		func() float64 { return float64(recorder.Queued()) })
	m.Gauge("restest_exchange_queue_capacity",
		"How many exchanges the request log buffer holds before it drops.", nil,
		func() float64 { return float64(recorder.Capacity()) })
	m.Counter("restest_exchanges_dropped_total",
		"Exchanges lost to a full buffer or a failed write since this process started.", nil,
		func() float64 { return float64(recorder.Dropped()) })

	m.Gauge("restest_route_table_projects",
		"Projects in the mock route table.", nil,
		func() float64 {
			projects, _ := matcher.Size()
			return float64(projects)
		})
	m.Gauge("restest_route_table_routes",
		"Routes in the mock route table, counting the six a collection expands into.", nil,
		func() float64 {
			_, routes := matcher.Size()
			return float64(routes)
		})

	for scope, limiter := range app.Limiters() {
		m.Gauge("restest_rate_limiter_keys",
			"Buckets a rate limiter is currently holding.",
			prometheus.Labels{"scope": scope},
			func() float64 { return float64(limiter.Len()) })
		m.Counter("restest_rate_limiter_cleared_total",
			"Times a rate limiter's table was emptied for reaching its key ceiling.",
			prometheus.Labels{"scope": scope},
			func() float64 { return float64(limiter.Cleared()) })
	}
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
