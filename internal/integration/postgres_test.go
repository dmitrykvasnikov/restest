//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/database"
	"github.com/dmitrykvasnikov/restest/internal/mock"
	"github.com/dmitrykvasnikov/restest/internal/web"
)

// postgresImage is the version the project deploys, per docker-compose.yml.
// Testing against a different one would be testing something else.
const postgresImage = "postgres:17-alpine"

// startPostgres brings up an empty database and returns its connection string.
// The container is torn down when the test ends, however it ends.
func startPostgres(t *testing.T) string {
	t.Helper()

	ctr, err := postgres.Run(t.Context(), postgresImage,
		postgres.WithDatabase("restest"),
		postgres.WithUsername("restest"),
		postgres.WithPassword("restest"),
		postgres.BasicWaitStrategies(),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}

	dsn, err := ctr.ConnectionString(t.Context(), "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}

// migratedPool brings a fresh database up to the current schema and returns a
// pool over it, which is the first half of what main.go does at startup.
func migratedPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()

	if err := database.Migrate(t.Context(), dsn, testLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	pool, err := database.Open(t.Context(), dsn, 4, testLogger())
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newApp wires the application over pool the way main.go does, so that what the
// tests drive is the real assembly rather than a convenient approximation.
func newApp(t *testing.T, pool *pgxpool.Pool) (*core.Store, *web.Server) {
	t.Helper()
	return newAppWith(t, pool, nil)
}

// newAppWith is newApp with the options a test needs to differ on — whether
// this instance serves the demo project, so far.
func newAppWith(t *testing.T, pool *pgxpool.Pool, tweak func(*web.Options)) (*core.Store, *web.Server) {
	t.Helper()

	sessions, stopCleanup := web.NewSessionManager(pool, false)
	t.Cleanup(stopCleanup)

	store := core.NewStore(pool)

	// Loaded, not left empty, exactly as main.go does it before the listener
	// opens. No background refresh: these tests want the reload that follows an
	// edit to be the reason a route starts answering.
	matcher := mock.NewRouter(store, testLogger())
	if err := matcher.Reload(t.Context()); err != nil {
		t.Fatalf("load route table: %v", err)
	}

	// The request log, wired the way main.go wires it: a recorder draining into
	// this store. Its flush interval is short so that a test which sends a
	// request and then looks for it in the database is waiting on milliseconds
	// rather than on the quarter second a deployment batches over.
	recorderCtx, stopRecorder := context.WithCancel(context.WithoutCancel(t.Context()))
	recorder := core.NewRecorder(store, testLogger(), core.RecorderOptions{
		Flush: 25 * time.Millisecond,
	})
	go recorder.Run(recorderCtx)
	t.Cleanup(func() {
		stopRecorder()
		recorder.Wait()
	})

	opts := web.Options{
		Logger:             testLogger(),
		Store:              store,
		Sessions:           sessions,
		Routes:             matcher,
		BaseURL:            "http://restest.test",
		Recorder:           recorder,
		LogRetentionMonths: core.DefaultRetentionMonths,
	}
	if tweak != nil {
		tweak(&opts)
	}

	srv, err := web.New(opts)
	if err != nil {
		t.Fatalf("build web server: %v", err)
	}
	return store, srv
}

// connect opens a single connection for a test to inspect the database with,
// separate from anything the code under test is using.
func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		// t.Context is already cancelled during cleanup, so this close gets a
		// context of its own.
		if err := conn.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("close inspection connection: %v", err)
		}
	})
	return conn
}

// tableNames lists the tables in the public schema, which is how these tests
// tell an applied migration from a rolled-back one.
func tableNames(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()

	rows, err := conn.Query(t.Context(), `
		select table_name
		from information_schema.tables
		where table_schema = 'public' and table_type = 'BASE TABLE'
		order by table_name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}

	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("collect table names: %v", err)
	}
	return names
}

// testLogger keeps container and migration output out of the test log unless
// something asks for it.
func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
