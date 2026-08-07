package web

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// fakeLog is an in-memory stand-in for the exchange half of core.Store, and for
// the recorder that fills it.
//
// It is both because the two are one thing from a handler test's point of view:
// what these tests ask is whether a request that went through the mock server
// comes back out of the inspector, and a fake that recorded into one place and
// served from another could answer yes without that being true. The real
// storage — COPY into a partitioned table, the cursor comparison, retention —
// is exercised against real Postgres in internal/integration.
type fakeLog struct {
	mu        sync.Mutex
	exchanges []core.Exchange

	// full makes Record drop, so that a test can ask what the inspector says
	// about a gap without having to overwhelm a real queue.
	full    bool
	dropped atomic.Int64
}

func newFakeLog() *fakeLog { return &fakeLog{} }

// Record is the Recorder half. It stores immediately rather than queueing:
// these tests are about what the inspector shows, and a background goroutine
// between the request and the assertion would only add a poll loop.
func (f *fakeLog) Record(ex core.Exchange) {
	if f.full {
		f.dropped.Add(1)
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if ex.ID == uuid.Nil {
		ex.ID = uuid.New()
	}
	f.exchanges = append(f.exchanges, ex)
}

func (f *fakeLog) Dropped() int64 { return f.dropped.Load() }

// recorded returns what has been recorded, oldest first.
func (f *fakeLog) recorded() []core.Exchange {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.exchanges)
}

// last is the most recent exchange, for a test that recorded exactly one.
func (f *fakeLog) last() (core.Exchange, bool) {
	all := f.recorded()
	if len(all) == 0 {
		return core.Exchange{}, false
	}
	return all[len(all)-1], true
}

// The store half. Newest first for the page, oldest first for the tail, both
// keyed on the same (created_at, id) ordering the SQL uses.

func (f *fakeLog) byProject(projectID uuid.UUID, before core.ExchangeCursor, limit int) ([]core.Exchange, error) {
	ordered := f.ordered(projectID)
	slices.Reverse(ordered)

	page := make([]core.Exchange, 0, limit)
	for _, ex := range ordered {
		if !before.IsZero() && !isBefore(ex.Cursor(), before) {
			continue
		}
		if len(page) == limit {
			break
		}
		page = append(page, ex)
	}
	return page, nil
}

func (f *fakeLog) since(projectID uuid.UUID, after core.ExchangeCursor, limit int) ([]core.Exchange, error) {
	if after.IsZero() {
		return nil, nil
	}

	tail := make([]core.Exchange, 0, limit)
	for _, ex := range f.ordered(projectID) {
		if isBefore(ex.Cursor(), after) || ex.ID == after.ID {
			continue
		}
		if len(tail) == limit {
			break
		}
		tail = append(tail, ex)
	}
	return tail, nil
}

func (f *fakeLog) byID(projectID, id uuid.UUID) (core.Exchange, error) {
	for _, ex := range f.ordered(projectID) {
		if ex.ID == id {
			return ex, nil
		}
	}
	return core.Exchange{}, core.ErrNotFound
}

func (f *fakeLog) latest(projectID uuid.UUID) (core.ExchangeCursor, error) {
	ordered := f.ordered(projectID)
	if len(ordered) == 0 {
		return core.ExchangeCursor{}, nil
	}
	return ordered[len(ordered)-1].Cursor(), nil
}

// ordered returns one project's exchanges oldest first, in the order the
// database would: by timestamp, then by id.
func (f *fakeLog) ordered(projectID uuid.UUID) []core.Exchange {
	f.mu.Lock()
	defer f.mu.Unlock()

	var mine []core.Exchange
	for _, ex := range f.exchanges {
		if ex.ProjectID == projectID {
			mine = append(mine, ex)
		}
	}
	slices.SortStableFunc(mine, func(a, b core.Exchange) int {
		if a.CreatedAt.Equal(b.CreatedAt) {
			return slices.Compare(a.ID[:], b.ID[:])
		}
		if a.CreatedAt.Before(b.CreatedAt) {
			return -1
		}
		return 1
	})
	return mine
}

func isBefore(a, b core.ExchangeCursor) bool {
	if a.At.Equal(b.At) {
		return slices.Compare(a.ID[:], b.ID[:]) < 0
	}
	return a.At.Before(b.At)
}
