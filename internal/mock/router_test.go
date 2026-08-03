package mock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// stubSource stands in for the store. It counts reads so a test can tell a
// reload that happened from one that did not.
type stubSource struct {
	mu    sync.Mutex
	data  core.MockData
	err   error
	reads int
}

func (s *stubSource) MockData(context.Context) (core.MockData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reads++
	return s.data, s.err
}

func (s *stubSource) set(data core.MockData, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data, s.err = data, err
}

func (s *stubSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.reads
}

func oneRoute(slug, method, path string) core.MockData {
	project := core.MockProject{ID: uuid.New(), Slug: slug}
	return core.MockData{
		Projects: []core.MockProject{project},
		Endpoints: []core.MockEndpoint{{
			ProjectSlug: slug,
			Endpoint: core.Endpoint{
				ID: uuid.New(), ProjectID: project.ID,
				Method: method, Path: path, Kind: core.KindStatic,
				IsEnabled: true, StatusCode: 200,
			},
		}},
	}
}

func TestRouterServesNothingBeforeItsFirstReload(t *testing.T) {
	r := NewRouter(&stubSource{}, discardLogger())

	if got := r.Lookup("shop", "GET", "/users"); got.Outcome != NoProject {
		t.Errorf("outcome = %v, want NoProject", got.Outcome)
	}
}

func TestRouterReload(t *testing.T) {
	source := &stubSource{data: oneRoute("shop", "GET", "/users")}
	r := NewRouter(source, discardLogger())

	if err := r.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := r.Lookup("shop", "GET", "/users"); got.Outcome != Matched {
		t.Fatalf("after reload: outcome = %v, want Matched", got.Outcome)
	}

	// The route table is rebuilt, not patched: what the source no longer
	// returns must stop being served.
	source.set(oneRoute("shop", "GET", "/orders"), nil)
	if err := r.Reload(t.Context()); err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if got := r.Lookup("shop", "GET", "/users"); got.Outcome != NoRoute {
		t.Errorf("removed route: outcome = %v, want NoRoute", got.Outcome)
	}
	if got := r.Lookup("shop", "GET", "/orders"); got.Outcome != Matched {
		t.Errorf("added route: outcome = %v, want Matched", got.Outcome)
	}
}

// TestReloadFailureKeepsTheOldTable is the behaviour that matters when the
// database blinks: a stale answer beats answering "no such project" to every
// mock request until it comes back.
func TestReloadFailureKeepsTheOldTable(t *testing.T) {
	source := &stubSource{data: oneRoute("shop", "GET", "/users")}
	r := NewRouter(source, discardLogger())

	if err := r.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	source.set(core.MockData{}, errors.New("database is not answering"))
	if err := r.Reload(t.Context()); err == nil {
		t.Fatal("reload: want an error")
	}

	if got := r.Lookup("shop", "GET", "/users"); got.Outcome != Matched {
		t.Errorf("outcome = %v, want the previous table to still be serving", got.Outcome)
	}
}

func TestRefreshReloadsUntilCancelled(t *testing.T) {
	source := &stubSource{data: oneRoute("shop", "GET", "/users")}
	r := NewRouter(source, discardLogger())

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Refresh(ctx, time.Millisecond)
	}()

	deadline := time.After(2 * time.Second)
	for source.readCount() < 3 {
		select {
		case <-deadline:
			t.Fatalf("reads = %d after two seconds, want at least 3", source.readCount())
		default:
			time.Sleep(time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh did not return when its context was cancelled")
	}
}

// TestLookupDuringReload is here for the race detector: a reader and the writer
// have to be able to run at once, which is the entire reason for the RWMutex.
func TestLookupDuringReload(t *testing.T) {
	source := &stubSource{data: oneRoute("shop", "GET", "/users")}
	r := NewRouter(source, discardLogger())

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				r.Lookup("shop", "GET", "/users")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 200 {
			if err := r.Reload(t.Context()); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
