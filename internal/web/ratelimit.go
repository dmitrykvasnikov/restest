package web

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/metrics"
	"github.com/dmitrykvasnikov/restest/internal/ratelimit"
)

// Rate limiting (PLAN.md M7).
//
// Two surfaces are limited, for two different reasons.
//
// **Mock traffic** is unauthenticated by design, so the only things a request
// can be counted under are where it came from and what it was addressed to.
// Both are limited: per client address, because one runaway loop should not be
// able to use the whole instance; and per project, because a project shared by
// a team is also a project a whole CI fleet can point at, and the per-address
// limit would never notice.
//
// **The management API** is the credentialed surface, and until now it was
// bounded by nothing at all — a token in a script with a mistake in it can
// create projects until the disk fills. It is counted per credential, so one
// misbehaving CI job is refused without touching anybody else's token.
//
// The interface is not limited. It is behind a session, every mutating request
// costs a form submission, and a limit there would mostly be a way to lock a
// person out of their own project while they are looking at it.

// retryAfter is the value of the Retry-After header on a refusal. Every rate
// this can be configured with is at least one per second, so a second is always
// long enough for a token to be back — and a caller that waits exactly that
// long and tries again is the behaviour the header is asking for.
const retryAfter = "1"

// limitMock refuses mock traffic that is over either of its two limits.
//
// It wraps the recording middleware rather than sitting inside it, so a refused
// request is not written to the project's inspector. That is deliberate: the
// limiter exists to shed load, and one that still pays for a captured body, a
// queued exchange and a row sheds a good deal less of it. The refusal is not
// invisible — it is the response the client is holding, it is counted in
// restest_rate_limited_total, and the 429 body says which limit was hit — but
// the request log stays a record of the traffic a project served.
func (s *Server) limitMock(next http.Handler) http.Handler {
	if s.mockIPLimiter == nil && s.mockProjectLimiter == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.mockIPLimiter.Allow(remoteIP(r)) {
			s.refuseMock(w, r, metrics.ScopeMockIP,
				"too many requests from this address; this instance serves %d mock requests a second per client",
				s.rateLimitIP)
			return
		}

		// Keyed on the project only once the project is known to exist. A slug
		// nothing answers to would otherwise be a way to fill the limiter's own
		// table — one bucket per made-up name — and the request is about to be
		// a 404 from the route table anyway, which costs a map lookup.
		if slug := mockProjectSlug(r); slug != "" && s.matcher.HasProject(slug) {
			if !s.mockProjectLimiter.Allow(slug) {
				s.refuseMock(w, r, metrics.ScopeMockProject,
					"too many requests for this project; it serves %d mock requests a second in total",
					s.rateLimitProject)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// refuseMock answers 429 in the shape the rest of the mock server answers in,
// so a client that has been parsing mock errors all along can parse this one.
func (s *Server) refuseMock(w http.ResponseWriter, r *http.Request, scope, format string, args ...any) {
	s.metrics.ObserveRateLimited(scope)

	w.Header().Set("Retry-After", retryAfter)
	writeJSON(w, r, http.StatusTooManyRequests, mockError{
		Error:   fmt.Sprintf(format, args...),
		Project: mockProjectSlug(r),
		Method:  r.Method,
		Path:    mockSubPath(r),
	})
}

// apiLimitKey is what a management API request is counted under.
//
// The key for a bearer request is the **hash** of the presented token, never
// the token: it is the same value the database stores, it identifies the
// credential exactly, and it keeps a secret out of a map this process holds for
// a minute after the request that carried it.
func apiLimitKey(r *http.Request, presented string, isBearer bool) string {
	if isBearer {
		return "token:" + string(core.HashToken(presented))
	}
	// A browser calling the API it is looking at. The session's user is the
	// credential; without one the request is about to be a 401, and the address
	// is all there is to count it under — which is what stops a flood of
	// guesses from being free.
	if user, ok := userFrom(r.Context()); ok {
		return "user:" + user.ID.String()
	}
	return "addr:" + remoteIP(r)
}

// allowAPI counts a management API request against its credential, reporting
// whether it may proceed. It has already answered the caller when it returns
// false.
//
// It runs before the token is authenticated, which is the point: authenticating
// costs a database round trip, and a limiter that only counts requests it has
// already paid for is not a limiter.
func (s *Server) allowAPI(w http.ResponseWriter, r *http.Request, presented string, isBearer bool) bool {
	if s.apiLimiter == nil {
		return true
	}
	if s.apiLimiter.Allow(apiLimitKey(r, presented, isBearer)) {
		return true
	}

	s.metrics.ObserveRateLimited(metrics.ScopeAPI)
	w.Header().Set("Retry-After", retryAfter)
	s.apiError(w, r, http.StatusTooManyRequests,
		"too many requests; this instance serves %d management API requests a second per credential",
		s.rateLimitAPI)
	return false
}

// mockProjectSlug is the project slug of a mock request, in the form projects are
// stored under.
//
// It reads the path rather than r.PathValue("slug"): this middleware is also
// what the 429 body is built from, and a refusal composed from the path works
// the same whether the mux has matched the route yet or not.
func mockProjectSlug(r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), mockPrefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return strings.ToLower(rest)
}

// newLimiters builds the three limiters a Server runs with. A limit of zero
// yields a nil *ratelimit.Limiter, which allows everything.
func newLimiters(opts Options) (mockIP, mockProject, api *ratelimit.Limiter) {
	return ratelimit.New(opts.RateLimitIP, ratelimit.Options{}),
		ratelimit.New(opts.RateLimitProject, ratelimit.Options{}),
		ratelimit.New(opts.RateLimitAPI, ratelimit.Options{})
}
