package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubPinger stands in for the database.
type stubPinger struct {
	err    error
	blocks bool // ignore the ping, wait for the context to expire
}

func (p stubPinger) Ping(ctx context.Context) error {
	if p.blocks {
		<-ctx.Done()
		return ctx.Err()
	}
	return p.err
}

// discardLogger keeps test output clean while still exercising the log path.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func serve(t *testing.T, db Pinger, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	New(discardLogger(), db).Handler().ServeHTTP(rec, req)
	return rec
}

func decodeHealth(t *testing.T, rec *httptest.ResponseRecorder) healthBody {
	t.Helper()
	var body healthBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return body
}

// Liveness must not depend on the database: restarting the process cannot fix
// a database that is down, and doing so would drop live requests for nothing.
func TestHealthzIgnoresDatabase(t *testing.T) {
	rec := serve(t, stubPinger{err: errors.New("connection refused")},
		httptest.NewRequest(http.MethodGet, pathHealthz, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeHealth(t, rec).Status; got != "ok" {
		t.Errorf("status field = %q, want ok", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

func TestReadyzOK(t *testing.T) {
	rec := serve(t, stubPinger{}, httptest.NewRequest(http.MethodGet, pathReadyz, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeHealth(t, rec).Status; got != "ok" {
		t.Errorf("status field = %q, want ok", got)
	}
}

func TestReadyzUnavailableWhenDatabaseIsDown(t *testing.T) {
	rec := serve(t, stubPinger{err: errors.New(`dial tcp 10.0.0.1:5432: connect: refused, user "restest"`)},
		httptest.NewRequest(http.MethodGet, pathReadyz, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}

	body := decodeHealth(t, rec)
	if body.Status != "unavailable" {
		t.Errorf("status field = %q, want unavailable", body.Status)
	}
	// The probe is unauthenticated: it says that the database is unreachable,
	// not where it lives or who tried to log in.
	if strings.Contains(rec.Body.String(), "10.0.0.1") || strings.Contains(rec.Body.String(), "restest") {
		t.Errorf("response leaks connection details: %s", rec.Body.String())
	}
}

// A ping that never comes back must still produce an answer, or the probe
// tells the orchestrator nothing at all.
func TestReadyzTimesOut(t *testing.T) {
	rec := serve(t, stubPinger{blocks: true}, httptest.NewRequest(http.MethodGet, pathReadyz, nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   int
	}{
		{"unknown path", http.MethodGet, "/nope", http.StatusNotFound},
		{"wrong method on healthz", http.MethodPost, pathHealthz, http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, stubPinger{}, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
