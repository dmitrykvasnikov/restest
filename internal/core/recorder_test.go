package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeWriter stands in for the store. It can be made to block or to fail, which
// is what the recorder's whole design is about: what happens when writing is
// slower than recording, or does not work at all.
type fakeWriter struct {
	mu       sync.Mutex
	written  []Exchange
	batches  []int
	err      error
	release  chan struct{}
	blocking bool
}

func (w *fakeWriter) InsertExchanges(ctx context.Context, exchanges []Exchange) (int, error) {
	if w.blocking {
		// A database that has accepted the work and stopped answering. It comes
		// back when the test releases it, or when the recorder's own write
		// deadline gives up on it.
		select {
		case <-w.release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.err != nil {
		return 0, w.err
	}
	w.written = append(w.written, exchanges...)
	w.batches = append(w.batches, len(exchanges))
	return len(exchanges), nil
}

func (w *fakeWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.written)
}

func (w *fakeWriter) batchSizes() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.batches...)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// lockedBuffer collects log output for a test to read while the goroutine under
// test is still writing to it. slog handlers do not serialise access to their
// writer for us.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func anExchange() Exchange {
	return Exchange{
		ID:        uuid.New(),
		ProjectID: uuid.New(),
		Method:    "GET",
		Path:      "/x",
		CreatedAt: time.Now(),
	}
}

// waitFor polls until cond holds or the deadline passes, so that a test about a
// background goroutine neither sleeps for a fixed time nor races it.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRecorderWritesWhatItIsGiven(t *testing.T) {
	writer := &fakeWriter{}
	r := NewRecorder(writer, discardLogger(), RecorderOptions{Flush: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)

	for range 3 {
		r.Record(anExchange())
	}

	waitFor(t, "the exchanges to be written", func() bool { return writer.count() == 3 })
	if r.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0", r.Dropped())
	}
}

// The batching is the point: one COPY for many requests rather than one write
// per request.
func TestRecorderWritesInBatches(t *testing.T) {
	writer := &fakeWriter{}
	r := NewRecorder(writer, discardLogger(), RecorderOptions{
		Buffer: 100,
		Batch:  10,
		// Long enough that the batch fills before the timer ever fires, so what
		// is being measured is the batching and not the tick.
		Flush: time.Hour,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)

	for range 20 {
		r.Record(anExchange())
	}

	waitFor(t, "two full batches", func() bool { return writer.count() == 20 })
	for _, size := range writer.batchSizes() {
		if size != 10 {
			t.Errorf("batch sizes = %v, want batches of 10", writer.batchSizes())
			break
		}
	}
}

// The rule the milestone is explicit about: a full buffer drops rather than
// blocking, and the drops are counted rather than discarded quietly.
func TestRecorderDropsRatherThanBlocking(t *testing.T) {
	writer := &fakeWriter{blocking: true, release: make(chan struct{})}
	r := NewRecorder(writer, discardLogger(), RecorderOptions{
		Buffer: 4,
		Batch:  1,
		Flush:  time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)

	// The writer is wedged on its first batch, so the buffer fills and stays
	// full. Recording has to keep returning anyway.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			r.Record(anExchange())
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Record blocked when the buffer was full")
	}

	if r.Dropped() == 0 {
		t.Error("Dropped = 0 after overwhelming a buffer of 4, want the drops to be counted")
	}
	close(writer.release)
}

// A write that fails loses those exchanges — retrying would hold them while new
// ones arrive — but it is counted and logged, which is the same promise the
// buffer makes.
func TestRecorderCountsAndLogsFailedWrites(t *testing.T) {
	var logged lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	writer := &fakeWriter{err: errors.New("the database said no")}
	r := NewRecorder(writer, logger, RecorderOptions{Flush: 5 * time.Millisecond})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Run(ctx)

	r.Record(anExchange())

	waitFor(t, "the failure to be counted", func() bool { return r.Dropped() == 1 })
	waitFor(t, "the failure to be logged", func() bool {
		return strings.Contains(logged.String(), "write request log")
	})
}

// Drops are reported in the log as well as counted, or nobody would know to go
// and look at the count.
func TestRecorderReportsDropsInTheLog(t *testing.T) {
	var logged lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	writer := &fakeWriter{blocking: true, release: make(chan struct{})}
	r := NewRecorder(writer, logger, RecorderOptions{
		Buffer: 2,
		Batch:  1,
		Flush:  time.Millisecond,
		// The write deadline is what lets the recorder notice it is dropping
		// while the database is still not answering, rather than only once it
		// starts again.
		Write: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(t.Context())
	go r.Run(ctx)

	for range 50 {
		r.Record(anExchange())
	}

	waitFor(t, "the drops to be reported", func() bool {
		return strings.Contains(logged.String(), "request log entries dropped")
	})

	cancel()
	close(writer.release)
	r.Wait()
}

// Shutdown writes what is queued. The requests those exchanges belong to have
// already been answered; losing them at the door would be losing the log of the
// last thing the process did.
func TestRecorderFlushesOnShutdown(t *testing.T) {
	writer := &fakeWriter{}
	r := NewRecorder(writer, discardLogger(), RecorderOptions{
		Batch: 1000,
		Flush: time.Hour, // never on its own
	})

	ctx, cancel := context.WithCancel(t.Context())
	go r.Run(ctx)

	for range 5 {
		r.Record(anExchange())
	}

	cancel()
	r.Wait()

	if got := writer.count(); got != 5 {
		t.Errorf("wrote %d exchanges on shutdown, want 5", got)
	}
}

// Wait returns for every caller, and returns immediately once Run has finished.
func TestRecorderWaitIsIdempotent(t *testing.T) {
	r := NewRecorder(&fakeWriter{}, discardLogger(), RecorderOptions{})

	ctx, cancel := context.WithCancel(t.Context())
	go r.Run(ctx)
	cancel()

	r.Wait()
	r.Wait()
}

// The defaults apply when a caller says nothing, so a zero RecorderOptions is a
// usable one rather than a recorder with a buffer of zero that drops
// everything.
func TestRecorderDefaults(t *testing.T) {
	r := NewRecorder(&fakeWriter{}, discardLogger(), RecorderOptions{})

	if got := cap(r.queue); got != DefaultRecorderBuffer {
		t.Errorf("buffer = %d, want %d", got, DefaultRecorderBuffer)
	}
	if r.opts.Batch != DefaultRecorderBatch || r.opts.Flush != DefaultRecorderFlush {
		t.Errorf("batch = %d, flush = %v, want the defaults", r.opts.Batch, r.opts.Flush)
	}
}
