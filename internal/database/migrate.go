package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/dmitrykvasnikov/restest/migrations"
)

// migrationLockKey identifies the advisory lock held while migrating. The
// value spells "restest" in ASCII; it only has to be stable and unlikely to
// collide with another application's locks in the same database.
const migrationLockKey int64 = 0x72657374657374

// Migrate applies every pending migration and returns once the schema is
// current. It runs at startup, so a deployment cannot serve traffic against a
// schema it was not built for.
//
// It opens its own single connection rather than borrowing the application
// pool: the advisory lock below is session-scoped, and it must live on the
// same connection that runs the migrations for the whole run. Holding a pool
// connection hostage for that long would also deadlock a small pool.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer func() {
		// Nothing useful to do about a failed close of a connection we are
		// finished with, but silence is worse than a line in the log.
		if err := db.Close(); err != nil {
			logger.Warn("close migration connection", slog.String("error", err.Error()))
		}
	}()

	// One connection, so the lock and the migrations share a session.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{logger})

	// Two instances starting at once would otherwise both try to apply the
	// same migration. The loser waits here and then finds nothing to do.
	if _, err := db.ExecContext(ctx, "select pg_advisory_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		// Not ctx: on a cancelled startup the unlock still has to be sent, or
		// the lock lingers until the connection is torn down.
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			"select pg_advisory_unlock($1)", migrationLockKey); err != nil {
			logger.Error("release migration lock", slog.String("error", err.Error()))
		}
	}()

	before, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if err := goose.UpContext(ctx, db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, err := goose.GetDBVersionContext(ctx, db)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	logger.Info("migrations applied",
		slog.Int64("version_before", before),
		slog.Int64("version_after", after),
	)
	return nil
}

// gooseLogger routes goose's own output into slog, so that migration output
// is structured like everything else instead of arriving as bare stderr lines.
type gooseLogger struct{ logger *slog.Logger }

func (g gooseLogger) Printf(format string, v ...any) {
	g.logger.Info(trimNewline(fmt.Sprintf(format, v...)), slog.String("source", "goose"))
}

// Fatalf does not exit. goose returns its errors as well as logging them, and
// the caller decides what is fatal.
func (g gooseLogger) Fatalf(format string, v ...any) {
	g.logger.Error(trimNewline(fmt.Sprintf(format, v...)), slog.String("source", "goose"))
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
