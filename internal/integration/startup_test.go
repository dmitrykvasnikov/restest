//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/dmitrykvasnikov/restest/internal/database"
	"github.com/dmitrykvasnikov/restest/migrations"
)

// The tables migration 00001 is expected to create. Listed explicitly so that a
// migration that half-applies is a failure rather than a surprise later.
var wantTables = []string{
	"api_tokens",
	"collections",
	"documents",
	"endpoints",
	"exchanges",
	"projects",
	"sessions",
	"users",
}

// TestMigrateFromEmpty is the startup path: an empty database becomes a
// migrated one, without anyone running a command.
func TestMigrateFromEmpty(t *testing.T) {
	dsn := startPostgres(t)
	conn := connect(t, dsn)

	if got := tableNames(t, conn); len(got) != 0 {
		t.Fatalf("fresh database is not empty: %v", got)
	}

	if err := database.Migrate(t.Context(), dsn, testLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got := tableNames(t, conn)
	for _, want := range wantTables {
		if !contains(got, want) {
			t.Errorf("table %q missing after migration; have %v", want, got)
		}
	}
	if !contains(got, "goose_db_version") {
		t.Error("goose did not record the schema version")
	}
	// The exchanges partition has to exist too, or the first write to the
	// request log fails for want of a partition to land in.
	if !contains(got, "exchanges_default") {
		t.Errorf("default partition missing; have %v", got)
	}
}

// Migrate runs on every start, so running it against an already-current schema
// must be a no-op rather than an error.
func TestMigrateIsIdempotent(t *testing.T) {
	dsn := startPostgres(t)

	for i := range 3 {
		if err := database.Migrate(t.Context(), dsn, testLogger()); err != nil {
			t.Fatalf("Migrate run %d: %v", i+1, err)
		}
	}

	conn := connect(t, dsn)
	var version int64
	if err := conn.QueryRow(t.Context(),
		"select max(version_id) from goose_db_version where is_applied").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 1 {
		t.Errorf("schema version = %d, want 1", version)
	}
}

// CONTEXT.md §7: a migration is not finished until it has been run up and down
// against a real Postgres. Down has to leave the database as it was found.
func TestMigrationDownRemovesEverything(t *testing.T) {
	dsn := startPostgres(t)

	if err := database.Migrate(t.Context(), dsn, testLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatalf("set dialect: %v", err)
	}
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(goose.NopLogger())

	if err := goose.DownToContext(t.Context(), db, ".", 0); err != nil {
		t.Fatalf("goose down: %v", err)
	}

	conn := connect(t, dsn)
	for _, name := range tableNames(t, conn) {
		// goose keeps its own bookkeeping table; everything else should be gone.
		if name != "goose_db_version" {
			t.Errorf("table %q survived the down migration", name)
		}
	}
}

// The whole startup sequence, in the order main.go runs it, ending at the
// readiness probe answering over HTTP — the milestone's "done when" in the
// small.
func TestServerIsReadyAgainstRealDatabase(t *testing.T) {
	dsn := startPostgres(t)
	logger := testLogger()

	if err := database.Migrate(t.Context(), dsn, logger); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	pool, err := database.Open(t.Context(), dsn, 4, logger)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	_, srv := newApp(t, pool)
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d %s, want 200", rec.Code, rec.Body)
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok", body.Status)
	}
}

// Readiness has to notice when the database goes away, or a load balancer keeps
// sending traffic to an instance that cannot serve it.
func TestReadyzFailsAfterDatabaseIsGone(t *testing.T) {
	// Migrated, because newApp builds the route table the way main.go does and
	// that reads two tables. What is being tested here is the pool going away
	// afterwards, not what happens before the schema exists.
	pool := migratedPool(t, startPostgres(t))
	_, srv := newApp(t, pool)
	handler := srv.Handler()

	// Close the pool rather than the container: same observable state from the
	// handler's point of view, and it does not race with test cleanup.
	pool.Close()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d, want 503 once the database is unreachable", rec.Code)
	}
}

// Open must not return until the database can actually answer, so a process
// that has started is a process that can serve.
func TestOpenWaitsForAWorkingDatabase(t *testing.T) {
	dsn := startPostgres(t)

	pool, err := database.Open(t.Context(), dsn, 2, testLogger())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(context.WithoutCancel(t.Context())); err != nil {
		t.Errorf("pool returned by Open cannot ping: %v", err)
	}
}
