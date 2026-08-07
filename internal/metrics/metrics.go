// Package metrics is the Prometheus instrumentation of a restest process.
//
// It exists so that the questions an operator actually asks — how much traffic
// is arriving, how much of it matches an endpoint, how slow it is, and whether
// the request log is keeping up — have answers that do not require reading the
// log. Everything here is either a counter or a gauge over process state; no
// metric touches the database, and the handler serves whatever the collectors
// hold at the moment it is scraped.
//
// **Labels are kept to bounded sets on purpose.** Nothing here is labelled by
// project slug, path, client address or token: those are unbounded, and an
// unbounded label is how a metrics endpoint becomes the memory leak it was
// added to detect. The per-project view is the request log, which is stored,
// paginated and retained — not this.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The surfaces a request can arrive on. One label value each, decided by which
// route pattern matched rather than by the path, because those are the four
// things with genuinely different shapes: unauthenticated mock traffic, the
// browser interface, the token-authenticated API, and the operational
// endpoints that a container runtime and a scraper call.
const (
	SurfaceMock  = "mock"
	SurfaceApp   = "app"
	SurfaceAPI   = "api"
	SurfaceOps   = "ops"
	SurfaceOther = "other"
)

// The scopes a rate limiter refuses under, matching the limits in the
// configuration.
const (
	ScopeMockIP      = "mock_ip"
	ScopeMockProject = "mock_project"
	ScopeAPI         = "api"
)

// Metrics holds the collectors and the registry they are registered in.
//
// A nil *Metrics is a working no-op, so a Server built without instrumentation
// — every handler test — needs no branch at the call sites.
type Metrics struct {
	registry *prometheus.Registry

	requests    *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	mockOutcome *prometheus.CounterVec
	rateLimited *prometheus.CounterVec
}

// New builds the registry and registers the collectors on it.
//
// A registry of its own rather than the default one: the default is global
// state that any dependency can write to, and a metric arriving from a library
// nobody chose is a metric nobody can explain. The Go and process collectors
// are added deliberately, because heap size and file descriptors are the two
// things an operator wants beside the application's own numbers.
func New(revision string) *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,

		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "restest_http_requests_total",
			Help: "HTTP requests served, by surface, method and status.",
		}, []string{"surface", "method", "status"}),

		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "restest_http_request_duration_seconds",
			Help: "Time to serve an HTTP request, by surface.",
			// The default buckets, which run from 5 ms to 10 s. A mock response
			// is a database round trip at worst and lands in the first few; the
			// long tail is there because an endpoint may be configured with a
			// delay of up to a minute, and that is a setting rather than a fault.
			Buckets: prometheus.DefBuckets,
		}, []string{"surface"}),

		mockOutcome: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "restest_mock_requests_total",
			Help: "Mock requests by what the matcher concluded: matched, no_route, wrong_method or no_project.",
		}, []string{"outcome"}),

		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "restest_rate_limited_total",
			Help: "Requests refused by a rate limiter, by the scope that refused them.",
		}, []string{"scope"}),
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "restest_build_info",
		Help: "Always 1; the revision this process was built from is the label.",
	}, []string{"revision"})
	buildInfo.WithLabelValues(revision).Set(1)

	reg.MustRegister(
		m.requests,
		m.duration,
		m.mockOutcome,
		m.rateLimited,
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serves the exposition format. It is registered by internal/web,
// which decides who may call it.
func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		// A scrape that fails half-way is more useful as an error than as a
		// truncated body a scraper would store as fact.
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// ObserveRequest records one served request.
func (m *Metrics) ObserveRequest(surface, method string, status int, d time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(surface, method, statusLabel(status)).Inc()
	m.duration.WithLabelValues(surface).Observe(d.Seconds())
}

// ObserveMockOutcome records what the matcher decided, which is what makes a
// match rate computable: matched over the sum of all four.
func (m *Metrics) ObserveMockOutcome(outcome string) {
	if m == nil {
		return
	}
	m.mockOutcome.WithLabelValues(outcome).Inc()
}

// ObserveRateLimited records one refusal.
func (m *Metrics) ObserveRateLimited(scope string) {
	if m == nil {
		return
	}
	m.rateLimited.WithLabelValues(scope).Inc()
}

// Gauge registers a gauge whose value is read from the process at scrape time.
// It is how main.go publishes state owned by something else — the recorder's
// queue depth, the size of the route table, a limiter's key count — without
// those packages having to know that Prometheus exists.
func (m *Metrics) Gauge(name, help string, labels prometheus.Labels, read func() float64) {
	if m == nil {
		return
	}
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name:        name,
		Help:        help,
		ConstLabels: labels,
	}, read))
}

// Counter registers a counter whose value is read from the process at scrape
// time, for a total something else is already keeping. It differs from Gauge
// only in the type it declares, which is what tells a query engine that
// rate() is meaningful over it.
func (m *Metrics) Counter(name, help string, labels prometheus.Labels, read func() float64) {
	if m == nil {
		return
	}
	m.registry.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{
		Name:        name,
		Help:        help,
		ConstLabels: labels,
	}, read))
}

// statusLabel is the status code as a label value.
//
// The full code rather than a 2xx/4xx/5xx class: the set is bounded by the
// statuses this application actually sends, an endpoint's own status codes are
// a project's deliberate choice, and telling 404 from 429 is most of what the
// number is for. A code outside the range HTTP defines is bucketed rather than
// echoed, so a handler that invents one cannot invent a label with it.
func statusLabel(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return statusStrings[status-100]
}

// statusStrings is every code from 100 to 599 as a string, built once, so that
// the hot path allocates nothing to label a counter.
var statusStrings = func() [500]string {
	var out [500]string
	for i := range out {
		out[i] = itoa3(100 + i)
	}
	return out
}()

func itoa3(n int) string {
	return string([]byte{byte('0' + n/100), byte('0' + n/10%10), byte('0' + n%10)})
}
