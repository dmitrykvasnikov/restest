package core

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Recorder defaults. They are exported because main.go logs the configuration
// it started with, and a queue whose size nobody can see is a queue nobody can
// reason about when it starts dropping.
const (
	// DefaultRecorderBuffer is how many exchanges may wait to be written. At the
	// batch size and flush interval below, a thousand is several seconds of a
	// database that has stopped answering, which is long enough to ride out a
	// failover and short enough to bound the memory it costs.
	DefaultRecorderBuffer = 1000
	// DefaultRecorderBatch is the most rows one COPY carries.
	DefaultRecorderBatch = 200
	// DefaultRecorderFlush is how long a partial batch waits for company. It is
	// also, therefore, the delay before a request shows up in the inspector.
	DefaultRecorderFlush = 250 * time.Millisecond
	// DefaultRecorderWrite bounds one batch's write.
	//
	// Without it, a database that accepts a connection and then stops answering
	// would wedge this goroutine indefinitely: the queue behind it would fill,
	// every exchange after that would be dropped, and nothing would say so,
	// because the goroutine that reports drops is the one stuck in the write.
	// Ten seconds is far longer than a healthy COPY of a few hundred rows and
	// short enough that a stall is reported while it is still happening.
	DefaultRecorderWrite = 10 * time.Second
	// recorderDropReport is the shortest interval between two "dropping"
	// warnings. Under sustained overload the drops are continuous, and a line
	// per batch would bury the log it is trying to warn about.
	recorderDropReport = 10 * time.Second
	// recorderShutdownGrace bounds the final flush. The context that stopped the
	// recorder is already cancelled by then, so the last batch gets a deadline
	// of its own rather than no deadline at all.
	recorderShutdownGrace = 5 * time.Second
)

// ExchangeWriter is the storage a Recorder drains into. An interface because
// the recorder's own tests must be able to make a write fail on demand, which
// is not something a real database does when asked politely.
type ExchangeWriter interface {
	InsertExchanges(ctx context.Context, exchanges []Exchange) (int, error)
}

// RecorderOptions configures a Recorder. The zero value is usable: every field
// falls back to the default above.
type RecorderOptions struct {
	Buffer int
	Batch  int
	Flush  time.Duration
	// Write bounds one batch's write, so that a database which has stopped
	// answering cannot stop the recorder from reporting that it has.
	Write time.Duration
}

// Recorder takes exchanges off the request path.
//
// Record hands over to a buffered channel and returns; a single goroutine
// drains it in batches. That is the whole design, and the reason for it is that
// the alternative — writing the log inline — makes every mock response wait for
// a database round trip that the client did not ask for.
//
// **A full buffer drops, and says so.** It does not block, because blocking
// would put the database back in the request path at exactly the moment the
// database is the problem. It does not discard quietly either: drops are
// counted, reported in the log, and shown in the inspector, because a log that
// silently loses entries is worse than no log — it invites conclusions from
// evidence that is not there.
type Recorder struct {
	writer ExchangeWriter
	logger *slog.Logger
	opts   RecorderOptions

	queue chan Exchange

	// dropped counts exchanges the buffer had no room for; failed counts ones a
	// write rejected. They are separate because they mean different things: the
	// first is this process falling behind, the second is the database refusing.
	dropped atomic.Int64
	failed  atomic.Int64
	// reported is the drop count as of the last warning, so that the interval
	// between warnings is measured in drops as well as in time.
	reported int64
	lastWarn time.Time

	done     chan struct{}
	stopOnce sync.Once
}

// NewRecorder returns a recorder that is not yet running. Nothing is written
// until Run is called; until then Record fills the buffer and then drops, which
// is the same behaviour as a writer that has fallen behind.
func NewRecorder(writer ExchangeWriter, logger *slog.Logger, opts RecorderOptions) *Recorder {
	if opts.Buffer <= 0 {
		opts.Buffer = DefaultRecorderBuffer
	}
	if opts.Batch <= 0 {
		opts.Batch = DefaultRecorderBatch
	}
	if opts.Flush <= 0 {
		opts.Flush = DefaultRecorderFlush
	}
	if opts.Write <= 0 {
		opts.Write = DefaultRecorderWrite
	}

	return &Recorder{
		writer: writer,
		logger: logger,
		opts:   opts,
		queue:  make(chan Exchange, opts.Buffer),
		done:   make(chan struct{}),
	}
}

// Record queues an exchange, or counts a drop. It never blocks and never
// returns an error: the caller is a request handler that has already answered
// its client, and there is nothing it could usefully do about a full queue.
func (r *Recorder) Record(ex Exchange) {
	select {
	case r.queue <- ex:
	default:
		r.dropped.Add(1)
	}
}

// Dropped reports how many exchanges have been lost — to a full buffer or to a
// failed write — since the process started. The inspector shows it, so that a
// gap in the log is visible as a gap rather than as an absence of traffic.
func (r *Recorder) Dropped() int64 { return r.dropped.Load() + r.failed.Load() }

// Queued reports how many exchanges are waiting to be written, and Capacity how
// many could be. Together they are the queue depth gauge in the metrics: drops
// say the buffer has already overflowed, and a depth climbing towards its
// capacity says it is about to. One is a post-mortem, the other is a warning.
func (r *Recorder) Queued() int { return len(r.queue) }

// Capacity is the size the buffer was built with.
func (r *Recorder) Capacity() int { return cap(r.queue) }

// Run drains the queue until ctx is cancelled, then writes what is left and
// returns. It is meant to be run in its own goroutine for the lifetime of the
// process.
func (r *Recorder) Run(ctx context.Context) {
	defer r.stopOnce.Do(func() { close(r.done) })

	batch := make([]Exchange, 0, r.opts.Batch)
	ticker := time.NewTicker(r.opts.Flush)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Whatever is already queued belongs to requests that have been
			// answered. Writing it is the last useful thing this goroutine does.
			batch = r.drain(batch)
			r.flush(context.WithoutCancel(ctx), batch, recorderShutdownGrace)
			// Unconditionally, whatever the interval says: a drop count that is
			// never reported because the process stopped first is exactly the
			// silence this recorder is built not to produce.
			r.reportDrops(true)
			return

		case ex := <-r.queue:
			batch = append(batch, ex)
			if len(batch) >= r.opts.Batch {
				batch = r.flush(ctx, batch, r.opts.Write)
			}

		case <-ticker.C:
			batch = r.flush(ctx, batch, r.opts.Write)
			r.reportDrops(false)
		}
	}
}

// Wait blocks until Run has returned, so that shutdown can stop the recorder
// before it closes the pool the recorder writes through.
func (r *Recorder) Wait() { <-r.done }

// drain empties the queue without blocking, for the final flush.
func (r *Recorder) drain(batch []Exchange) []Exchange {
	for {
		select {
		case ex := <-r.queue:
			batch = append(batch, ex)
		default:
			return batch
		}
	}
}

// flush writes a batch and returns an empty one. A failure is logged and the
// rows are given up: retrying would mean holding them while new ones arrive,
// which is how a buffer that drops turns into a buffer that stalls.
func (r *Recorder) flush(ctx context.Context, batch []Exchange, timeout time.Duration) []Exchange {
	if len(batch) == 0 {
		return batch
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if _, err := r.writer.InsertExchanges(ctx, batch); err != nil {
		r.failed.Add(int64(len(batch)))
		r.logger.Error("write request log",
			slog.Int("exchanges", len(batch)),
			slog.String("error", err.Error()),
		)
	}
	return batch[:0]
}

// reportDrops warns about a buffer that is losing exchanges, at most once every
// recorderDropReport and only when the count has moved. force ignores the
// interval, for the last word before the recorder stops.
//
// reported and lastWarn are read and written only here, and this only runs on
// the Run goroutine, which is why neither needs the atomic the counters do.
func (r *Recorder) reportDrops(force bool) {
	total := r.Dropped()
	if total == r.reported {
		return
	}
	if now := time.Now(); !force && now.Sub(r.lastWarn) < recorderDropReport {
		return
	}

	r.logger.Warn("request log entries dropped",
		slog.Int64("since_last_report", total-r.reported),
		slog.Int64("total", total),
		slog.Int("buffer", r.opts.Buffer),
	)
	r.reported = total
	r.lastWarn = time.Now()
}
