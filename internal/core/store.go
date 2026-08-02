// Package core holds the domain — the things restest is about — and the
// storage behind them.
//
// It is the shared floor of the application: internal/mock and, in phase 2,
// internal/runner both depend on it, and neither depends on the other
// (DESIGN.md §10). Nothing here knows about HTTP.
package core

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// Store is the entry point to persistent state. Queries are the ones generated
// by sqlc from internal/core/queries; this type gives them a home and a place
// for the ones that need more than a single statement.
type Store struct {
	pool *pgxpool.Pool
	q    *dbgen.Queries
}

// NewStore wraps an already-open pool. The pool's lifetime belongs to the
// caller, which opened it and will close it.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: dbgen.New(pool)}
}

// Ping reports whether the database is answering. It runs a real query rather
// than a protocol-level ping, so it fails if the pool is exhausted or the
// server has stopped planning statements — the states a readiness probe exists
// to catch.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.q.CheckDatabase(ctx); err != nil {
		return fmt.Errorf("check database: %w", err)
	}
	return nil
}
