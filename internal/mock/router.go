package mock

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// RefreshInterval is how often the router rebuilds its table without being
// asked.
//
// Every change made through this process reloads the table itself, so this is
// not how a new endpoint normally goes live. It is the answer to the two cases
// that reload cannot cover: a rebuild that failed and would otherwise leave the
// table stale until the next edit, and a second instance of the process whose
// edits this one never saw. Thirty seconds is short enough that neither is a
// mystery and long enough to be invisible.
const RefreshInterval = 30 * time.Second

// Source is where a Router reads its routes from. An interface rather than
// *core.Store so that the router can be tested without a database — the store's
// own query is exercised against real Postgres in internal/integration.
type Source interface {
	MockData(ctx context.Context) (core.MockData, error)
}

// Router holds the current route table and hands out lookups against it.
//
// The lock is held for a pointer read, not for a match: the table it points at
// never changes once built, so a request that starts matching finishes against
// the snapshot it began with even if a rebuild lands mid-way.
type Router struct {
	source Source
	logger *slog.Logger

	mu    sync.RWMutex
	table *Table
}

// NewRouter returns a router with an empty table. Nothing is served until
// Reload has succeeded, which is deliberate: an empty table answers "no such
// project", and that is a better answer than a table half-built from a database
// that was not ready.
func NewRouter(source Source, logger *slog.Logger) *Router {
	return &Router{
		source: source,
		logger: logger,
		table:  BuildTable(core.MockData{}, logger),
	}
}

// Reload rebuilds the table from the database.
//
// On failure the existing table is left in place. A rebuild fails because the
// database is unreachable, and answering every mock request with "no such
// project" until it comes back would turn a database blip into a wrong answer
// where a slightly stale one was available.
func (r *Router) Reload(ctx context.Context) error {
	data, err := r.source.MockData(ctx)
	if err != nil {
		return fmt.Errorf("reload route table: %w", err)
	}
	table := BuildTable(data, r.logger)

	r.mu.Lock()
	r.table = table
	r.mu.Unlock()

	r.logger.Debug("route table rebuilt",
		slog.Int("projects", table.Projects()),
		slog.Int("routes", table.Routes()),
	)
	return nil
}

// Refresh reloads on a fixed interval until ctx is cancelled. It is meant to be
// run in its own goroutine for the lifetime of the process.
func (r *Router) Refresh(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Reload(ctx); err != nil && ctx.Err() == nil {
				r.logger.Error("scheduled route table refresh failed",
					slog.String("error", err.Error()))
			}
		}
	}
}

// Lookup answers a request against the current table.
func (r *Router) Lookup(slug, method, path string) Result {
	r.mu.RLock()
	table := r.table
	r.mu.RUnlock()

	return table.Lookup(slug, method, path)
}
