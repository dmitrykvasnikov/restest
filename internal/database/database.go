// Package database owns the connection pool and the migration run.
//
// It deliberately knows nothing about the rest of the application: it takes a
// connection string and hands back a pool. Everything above it works with
// *pgxpool.Pool, so no other package needs to care how the connection was set
// up.
package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Timings for the startup connectivity check. Docker compose already gates the
// application on the database's healthcheck; this retry covers the cases that
// compose does not, such as a database restarting underneath a running stack.
const (
	connectAttempts    = 10
	connectBackoffBase = 250 * time.Millisecond
	connectBackoffMax  = 3 * time.Second
	pingTimeout        = 3 * time.Second
)

// Open creates the connection pool and does not return until the database has
// answered a ping, so a process that starts is a process that can serve.
func Open(ctx context.Context, dsn string, maxConns int32, logger *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	// A connection that has been idle for half an hour is more likely to have
	// been silently dropped by something in the middle than to be useful.
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := waitForDatabase(ctx, pool, logger); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// waitForDatabase pings until the database answers, the attempts run out, or
// the context is cancelled.
func waitForDatabase(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	backoff := connectBackoffBase

	for attempt := 1; ; attempt++ {
		err := ping(ctx, pool)
		if err == nil {
			return nil
		}
		if attempt == connectAttempts {
			return fmt.Errorf("database unreachable after %d attempts: %w", attempt, err)
		}

		logger.Warn("database not ready, retrying",
			slog.Int("attempt", attempt),
			slog.Duration("retry_in", backoff),
			slog.String("error", err.Error()),
		)

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return fmt.Errorf("waiting for database: %w", ctx.Err())
		}

		if backoff *= 2; backoff > connectBackoffMax {
			backoff = connectBackoffMax
		}
	}
}

// ping bounds its own wait, so a database that accepts connections but never
// answers fails the check rather than hanging startup.
func ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return pool.Ping(ctx)
}
