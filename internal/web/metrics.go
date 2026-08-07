package web

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/metrics"
	"github.com/dmitrykvasnikov/restest/internal/mock"
)

// pathMetrics is where the Prometheus exposition is served. Registered only
// when a Server was built with instrumentation, so an instance with metrics off
// answers 404 there rather than 200 with nothing in it.
const pathMetrics = "/metrics"

// observe counts a request and times it.
//
// The surface a request is counted under comes from the route pattern it
// matched rather than from its path, for the same reason withSession and
// withSecurityHeaders make their decisions there: the pattern is what the
// application concluded the request was, and the path is what somebody typed.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := s.mux.Handler(r)
		surface := surfaceOf(pattern)

		// The access log above has already wrapped the writer to learn the
		// status, and one wrapper answering both questions is one place for the
		// status to be wrong rather than two.
		rec, ok := w.(*recorder)
		if !ok {
			rec = &recorder{ResponseWriter: w, status: http.StatusOK}
		}

		started := time.Now()
		next.ServeHTTP(rec, r)

		s.metrics.ObserveRequest(surface, methodLabel(r.Method), rec.status, time.Since(started))
	})
}

// surfaceOf maps a matched route pattern to one of the four label values.
func surfaceOf(pattern string) string {
	switch {
	case pattern == patternMock:
		return metrics.SurfaceMock
	case strings.Contains(pattern, pathAPIPrefix):
		return metrics.SurfaceAPI
	case pattern == "GET "+pathHealthz, pattern == "GET "+pathReadyz,
		pattern == "GET "+pathStatic, pattern == "GET "+pathMetrics:
		return metrics.SurfaceOps
	case pattern == "":
		// No route claimed it at all, which net/http reports as an empty
		// pattern. The catch-all handler answers, but there is nothing here to
		// call it.
		return metrics.SurfaceOther
	default:
		return metrics.SurfaceApp
	}
}

// knownMethods is the verb set this application itself defines. A mock endpoint
// may answer any of them, and a client may send something else entirely — which
// is why anything unrecognised becomes one label value rather than its own.
// Without that, "method" is an unbounded label written by the caller.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

func methodLabel(method string) string {
	if knownMethods[method] {
		return method
	}
	return "other"
}

// mockOutcomeLabel names what the matcher concluded, so that the match rate —
// matched over the sum of the four — is computable from the scrape.
func mockOutcomeLabel(outcome mock.Outcome) string {
	switch outcome {
	case mock.Matched:
		return "matched"
	case mock.WrongMethod:
		return "wrong_method"
	case mock.NoRoute:
		return "no_route"
	case mock.NoProject:
		return "no_project"
	default:
		return "unknown"
	}
}

// handleMetrics serves the exposition, behind the token if there is one.
//
// The guard is optional because the right answer depends on the deployment: on
// an instance behind a proxy that does not route /metrics, a token is one more
// secret to manage for no gain, and on one published as it stands, the scrape
// names every project's traffic volume and this process's memory layout. The
// default is no token, which is why the README says to block the path at the
// proxy or set one.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.metricsToken != "" && !s.metricsTokenMatches(r) {
		w.Header().Set(headerWWWAuth, `Bearer realm="restest metrics"`)
		writeJSON(w, r, http.StatusUnauthorized, errorBody{
			Error: "this endpoint requires the metrics token as `Authorization: Bearer …`",
		})
		return
	}
	s.metrics.Handler().ServeHTTP(w, r)
}

// metricsTokenMatches compares in constant time, so that a wrong token cannot
// be improved one character at a time by measuring how long the refusal took.
func (s *Server) metricsTokenMatches(r *http.Request) bool {
	presented, ok := bearerToken(r)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.metricsToken)) == 1
}
