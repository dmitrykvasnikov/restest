//go:build integration

package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
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
