package core

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Partition maintenance, which is how the request log is kept from growing
// without end.
//
// `exchanges` is range-partitioned by month (migration 00001). Expiry is
// therefore a partition detached and dropped — a metadata change and a file
// unlink — rather than a DELETE that has to visit every expired row and leave
// the space behind for autovacuum to reclaim. On the write-heavy table that is
// the difference between retention being free and retention being an incident.
const (
	// PartitionsAhead is how many future months are created each time
	// maintenance runs. One would be enough if the job never failed; three means
	// a month can only reach the default partition after the job has been
	// failing, unnoticed, for a quarter.
	PartitionsAhead = 3
	// DefaultRetentionMonths is how many months of log are kept, counting the
	// current one. Long enough to answer "what did that client send last week",
	// short enough that a mock server does not quietly become an archive.
	DefaultRetentionMonths = 3
	// MinRetentionMonths keeps the current month whatever a caller asks for.
	// Detaching the partition being written into would send live traffic to the
	// default partition, which is a safety net and not a home.
	MinRetentionMonths = 1

	// exchangesTable is the partitioned parent. Its default partition is named
	// after it and is never dropped.
	exchangesTable   = "exchanges"
	defaultPartition = "exchanges_default"

	// partitionLockTimeout bounds how long maintenance waits for the
	// ACCESS EXCLUSIVE lock that attaching or detaching a partition needs. A
	// long-running query over the log would otherwise let a background job
	// block every mock request behind it. Failing and trying again tomorrow is
	// the right answer; queueing in front of the request path is not.
	partitionLockTimeout = "5s"
)

// MaintainExchangeLog creates the partitions the coming months will need and
// drops the ones that have aged out, returning what it did.
//
// It is safe to run repeatedly and from more than one instance: creation is
// `if not exists`, and a drop of a partition another instance has already
// detached fails the whole transaction rather than half of it.
func (s *Store) MaintainExchangeLog(ctx context.Context, now time.Time, keepMonths int) error {
	if err := s.CreateExchangePartitions(ctx, now, PartitionsAhead); err != nil {
		return err
	}
	if _, err := s.DropExpiredExchangePartitions(ctx, now, keepMonths); err != nil {
		return err
	}
	return nil
}

// CreateExchangePartitions makes sure the current month and the next `ahead`
// months each have a partition.
//
// Creating them in advance is the point. A month with no partition still
// accepts writes — they land in the default partition, which exists so that
// logging can never be the reason a mock request fails — but rows sitting in
// the default cannot be dropped by detaching, and their presence is what makes
// creating that month's partition later fail. So the job runs ahead of the
// traffic rather than alongside it.
func (s *Store) CreateExchangePartitions(ctx context.Context, now time.Time, ahead int) error {
	month := monthStart(now)

	for range ahead + 1 {
		next := month.AddDate(0, 1, 0)

		create := fmt.Sprintf(
			`create table if not exists %s partition of %s for values from ('%s') to ('%s')`,
			quoteIdentifier(partitionName(month)),
			quoteIdentifier(exchangesTable),
			month.Format(time.RFC3339),
			next.Format(time.RFC3339),
		)
		if _, err := s.pool.Exec(ctx, create); err != nil {
			return fmt.Errorf("create the %s partition of the request log: %w", partitionName(month), err)
		}
		month = next
	}
	return nil
}

// DropExpiredExchangePartitions removes the months that have fallen outside the
// retention window, returning the names it dropped.
//
// Detach then drop, in one transaction, rather than a bare DROP: the detach is
// what takes the partition out of the parent's plan, and doing both together
// means a failure between them cannot leave an orphan table holding the log's
// data with nothing pointing at it.
func (s *Store) DropExpiredExchangePartitions(ctx context.Context, now time.Time, keepMonths int) ([]string, error) {
	if keepMonths < MinRetentionMonths {
		keepMonths = MinRetentionMonths
	}
	// The oldest month that survives: keepMonths counting back from the current
	// one, so keepMonths = 1 keeps only the month being written into.
	cutoff := monthStart(now).AddDate(0, -(keepMonths - 1), 0)

	names, err := s.exchangePartitions(ctx)
	if err != nil {
		return nil, err
	}

	var dropped []string
	for _, name := range names {
		month, ok := partitionMonth(name)
		if !ok || !month.Before(cutoff) {
			// Not one of ours — the default partition, or a table somebody
			// attached by hand — or still inside the window.
			continue
		}
		if err := s.dropPartition(ctx, name); err != nil {
			return dropped, err
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

func (s *Store) dropPartition(ctx context.Context, name string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin partition drop: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the commit path has already reported

	if _, err := tx.Exec(ctx, "set local lock_timeout = '"+partitionLockTimeout+"'"); err != nil {
		return fmt.Errorf("set the partition lock timeout: %w", err)
	}
	detach := fmt.Sprintf("alter table %s detach partition %s",
		quoteIdentifier(exchangesTable), quoteIdentifier(name))
	if _, err := tx.Exec(ctx, detach); err != nil {
		return fmt.Errorf("detach the %s partition of the request log: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "drop table "+quoteIdentifier(name)); err != nil {
		return fmt.Errorf("drop the %s partition of the request log: %w", name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the drop of partition %s: %w", name, err)
	}
	return nil
}

// exchangePartitions lists the partitions currently attached to the log,
// including the default one.
func (s *Store) exchangePartitions(ctx context.Context) ([]string, error) {
	const list = `
		select child.relname
		from pg_inherits
		join pg_class parent on parent.oid = pg_inherits.inhparent
		join pg_class child  on child.oid  = pg_inherits.inhrelid
		join pg_namespace ns on ns.oid     = parent.relnamespace
		where parent.relname = $1 and ns.nspname = current_schema()
		order by child.relname`

	rows, err := s.pool.Query(ctx, list, exchangesTable)
	if err != nil {
		return nil, fmt.Errorf("list request log partitions: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("collect request log partitions: %w", err)
	}
	return names, nil
}

// MaintainExchangeLogLoop runs maintenance now and then once a day until ctx is
// cancelled. It is meant to be run in its own goroutine for the lifetime of the
// process.
//
// Daily rather than monthly because a process that is restarted often would
// otherwise be the only thing that ever ran it, and a process that is never
// restarted would run it once. A failure is logged and retried tomorrow: the
// partitions for the next three months already exist, so a day without
// maintenance costs nothing.
func (s *Store) MaintainExchangeLogLoop(ctx context.Context, every time.Duration, keepMonths int, logger *slog.Logger) {
	run := func() {
		if err := s.MaintainExchangeLog(ctx, time.Now(), keepMonths); err != nil && ctx.Err() == nil {
			logger.Error("maintain the request log", slog.String("error", err.Error()))
		}
	}
	run()

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// partitionName is the table name of one month's partition: exchanges_2026_08.
// The month is in the name so that retention can read it back without asking
// the database to parse a partition bound expression.
func partitionName(month time.Time) string {
	return fmt.Sprintf("%s_%04d_%02d", exchangesTable, month.Year(), int(month.Month()))
}

// partitionMonth is partitionName read backwards. Anything that does not have
// the shape — the default partition above all — is reported as not ours, and
// left alone.
func partitionMonth(name string) (time.Time, bool) {
	rest, found := strings.CutPrefix(name, exchangesTable+"_")
	if !found {
		return time.Time{}, false
	}
	year, monthText, found := strings.Cut(rest, "_")
	if !found {
		return time.Time{}, false
	}

	y, err := strconv.Atoi(year)
	if err != nil || len(year) != 4 {
		return time.Time{}, false
	}
	m, err := strconv.Atoi(monthText)
	if err != nil || len(monthText) != 2 || m < 1 || m > 12 {
		return time.Time{}, false
	}
	return time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC), true
}

// monthStart is the first instant of the month containing t, in UTC.
//
// UTC and not local time: the partition bounds are written as absolute
// timestamps, and a server whose timezone changes — or a second instance in
// another one — must not disagree with them about where a month begins.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// quoteIdentifier renders a table name for the DDL above.
//
// Every name it is given is one this file composed from a number, so nothing
// user-supplied reaches it. It exists because DDL cannot take bind parameters,
// and a statement built by concatenation should be visibly quoted rather than
// visibly trusting.
func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
