//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"
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
	var applied int64
	if err := conn.QueryRow(t.Context(),
		"select count(*) from goose_db_version where is_applied and version_id > 0").Scan(&applied); err != nil {
		t.Fatalf("read schema version: %v", err)
	}

	// Counted against the embedded files rather than written down, so that
	// adding a migration does not also mean editing this test.
	want := int64(len(migrationFiles(t)))
	if applied != want {
		t.Errorf("%d migrations applied, want %d — the embedded set", applied, want)
	}
}

// migrationFiles lists the migrations that ship in the binary.
func migrationFiles(t *testing.T) []string {
	t.Helper()

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("no migrations are embedded")
	}
	return names
}

// Migration 00002 exists so that a listing has a stable order. Both halves of
// it are checked here — the column and the index that follows it — because a
// column that arrived without its index would be a sequential scan on every
// listing and nothing would fail to say so.
func TestDocumentOrderingColumn(t *testing.T) {
	dsn := startPostgres(t)
	if err := database.Migrate(t.Context(), dsn, testLogger()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	conn := connect(t, dsn)

	var generated string
	if err := conn.QueryRow(t.Context(), `
		select is_identity from information_schema.columns
		where table_name = 'documents' and column_name = 'seq'`).Scan(&generated); err != nil {
		t.Fatalf("documents.seq is missing: %v", err)
	}
	if generated != "YES" {
		t.Errorf("documents.seq is_identity = %q, want YES so nothing can supply its own", generated)
	}

	var indexes []string
	rows, err := conn.Query(t.Context(),
		`select indexname from pg_indexes where tablename = 'documents' order by indexname`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	indexes, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect indexes: %v", err)
	}
	if !contains(indexes, "documents_order_idx") {
		t.Errorf("the ordering index is missing; have %v", indexes)
	}
	if contains(indexes, "documents_listing_idx") {
		t.Errorf("the superseded index survived; have %v", indexes)
	}
	if !contains(indexes, "documents_body_gin_idx") {
		t.Errorf("the GIN index that filters are served by is missing; have %v", indexes)
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
