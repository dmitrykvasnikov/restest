package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// scrape returns the exposition body the handler produces.
func scrape(t *testing.T, m *Metrics) string {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("scrape returned %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func mustContain(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(body, line) {
			t.Errorf("the scrape does not contain %q", line)
		}
	}
}

func TestANilMetricsRecordsNothingAndDoesNotPanic(t *testing.T) {
	var m *Metrics

	m.ObserveRequest(SurfaceMock, http.MethodGet, http.StatusOK, time.Millisecond)
	m.ObserveMockOutcome("matched")
	m.ObserveRateLimited(ScopeMockIP)
	m.Gauge("restest_never_registered", "help", nil, func() float64 { return 1 })
	m.Counter("restest_never_registered_total", "help", nil, func() float64 { return 1 })

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a nil Metrics served %d, want 404", rec.Code)
	}
}

func TestTheScrapeCarriesWhatWasObserved(t *testing.T) {
	m := New("abc1234")

	m.ObserveRequest(SurfaceMock, http.MethodGet, http.StatusOK, 12*time.Millisecond)
	m.ObserveRequest(SurfaceMock, http.MethodGet, http.StatusOK, 8*time.Millisecond)
	m.ObserveRequest(SurfaceAPI, http.MethodPost, http.StatusCreated, 30*time.Millisecond)
	m.ObserveMockOutcome("matched")
	m.ObserveMockOutcome("no_route")
	m.ObserveRateLimited(ScopeMockProject)

	body := scrape(t, m)

	mustContain(t, body,
		`restest_http_requests_total{method="GET",status="200",surface="mock"} 2`,
		`restest_http_requests_total{method="POST",status="201",surface="api"} 1`,
		`restest_mock_requests_total{outcome="matched"} 1`,
		`restest_mock_requests_total{outcome="no_route"} 1`,
		`restest_rate_limited_total{scope="mock_project"} 1`,
		`restest_build_info{revision="abc1234"} 1`,
		`restest_http_request_duration_seconds_count{surface="mock"} 2`,
	)
}

func TestTheGoAndProcessCollectorsAreThere(t *testing.T) {
	// They are registered deliberately, and an operator watching heap size or
	// file descriptors would find their absence only in production.
	mustContain(t, scrape(t, New("dev")), "go_goroutines", "go_memstats_alloc_bytes")
}

func TestAGaugeIsReadAtScrapeTime(t *testing.T) {
	m := New("dev")

	depth := 0.0
	m.Gauge("restest_test_depth", "How deep.", prometheus.Labels{"queue": "exchanges"}, func() float64 {
		return depth
	})

	mustContain(t, scrape(t, m), `restest_test_depth{queue="exchanges"} 0`)

	depth = 7
	mustContain(t, scrape(t, m), `restest_test_depth{queue="exchanges"} 7`)
}

func TestACounterIsDeclaredAsACounter(t *testing.T) {
	m := New("dev")
	m.Counter("restest_test_dropped_total", "How many were lost.", nil, func() float64 { return 3 })

	mustContain(t, scrape(t, m),
		"# TYPE restest_test_dropped_total counter",
		"restest_test_dropped_total 3",
	)
}

func TestStatusLabelsAreBoundedToWhatHTTPDefines(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{100, "100"},
		{200, "200"},
		{204, "204"},
		{429, "429"},
		{599, "599"},
		// A handler that invents a status must not be able to invent a label
		// value with it, since every distinct value is a new time series.
		{0, "unknown"},
		{99, "unknown"},
		{600, "unknown"},
		{-1, "unknown"},
	}
	for _, tc := range cases {
		if got := statusLabel(tc.status); got != tc.want {
			t.Errorf("statusLabel(%d) = %q, want %q", tc.status, got, tc.want)
		}
	}
}
