package web

import (
	"net/http"
)

// Response headers that constrain what a browser will do with what we send.
//
// There are two sets, because this application serves two things from one
// origin and they need opposite policies.
//
// The interface is our own HTML, and it gets a strict Content-Security-Policy:
// scripts and styles only from this origin, nothing embedded from anywhere,
// nothing framed, no base tag. That policy is affordable because of a decision
// made much earlier — templates carry no inline script and no inline style
// (DESIGN.md §9.2, §9.3), so `'unsafe-inline'` is not needed to make the
// application work, and a policy that needs it protects against very little.
//
// **The mock server is not our HTML, and that is the point of the second set.**
// A project's response body is written by whoever owns the project, and it is
// served from this origin. Without a policy, a user could define an endpoint
// returning an HTML page with a script in it, send the URL to somebody who has
// a session here, and have that script run with our origin's privileges —
// stored cross-site scripting, arriving through the front door as a feature.
// `sandbox` is the answer: a mock response rendered as a document lands in an
// opaque origin with scripts disabled, so it can reach nothing. It costs
// nothing for the traffic mocks are actually for, because CSP applies to
// documents a browser renders and not to a response a program fetched.
const (
	headerCSP        = "Content-Security-Policy"
	headerNoSniff    = "X-Content-Type-Options"
	headerFrameOpts  = "X-Frame-Options"
	headerReferrer   = "Referrer-Policy"
	headerHSTS       = "Strict-Transport-Security"
	headerCOOP       = "Cross-Origin-Opener-Policy"
	headerPermission = "Permissions-Policy"
)

// appCSP is the policy for pages this application renders.
//
// `default-src 'none'` and then only what is actually used, so that a future
// template pulling in a font, an image or an API from somewhere else fails
// visibly during development rather than silently widening the policy.
// `connect-src 'self'` is what the live log tail needs; `img-src` allows data:
// for the inline SVG favicon in base.html.
const appCSP = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'none'"

// mockCSP applies to everything under /m/{slug}/. The empty sandbox is the
// strictest form: an opaque origin, no scripts, no forms, no plugins.
const mockCSP = "default-src 'none'; sandbox"

// hstsValue is sent only when this instance is reached over https, which is
// what BaseURL's scheme says. Sending it over plain HTTP is meaningless at
// best, and on a shared hostname it is a promise made on somebody else's
// behalf. `preload` is deliberately absent: it is a submission to a list
// browsers ship, and undoing it takes months.
const hstsValue = "max-age=31536000; includeSubDomains"

// permissionsPolicy switches off the device APIs this application has no use
// for. It costs one header and removes a whole class of "what could an injected
// script reach" from the answer.
const permissionsPolicy = "accelerometer=(), camera=(), geolocation=(), gyroscope=(), " +
	"magnetometer=(), microphone=(), payment=(), usb=()"

// withSecurityHeaders puts the right set on every response.
//
// The choice is made on the matched route pattern rather than on the path, for
// the same reason withSession makes it there: a request that resolves to the
// catch-all is one of ours, whatever it was addressed to.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := s.mux.Handler(r); pattern == patternMock {
			next.ServeHTTP(&mockHeaderWriter{ResponseWriter: w}, r)
			return
		}

		h := w.Header()
		h.Set(headerCSP, appCSP)
		h.Set(headerNoSniff, "nosniff")
		h.Set(headerFrameOpts, "DENY")
		h.Set(headerReferrer, "same-origin")
		h.Set(headerCOOP, "same-origin")
		h.Set(headerPermission, permissionsPolicy)
		if s.secure {
			h.Set(headerHSTS, hstsValue)
		}

		next.ServeHTTP(w, r)
	})
}

// mockHeaderWriter stamps the mock policy on the way out, just before the
// status line.
//
// Setting the headers up front would not do. An endpoint carries a header map
// of its own — that is how somebody adds the CORS header their browser client
// needs — and those are applied inside the handler, so a project could set
// `Content-Security-Policy` to nothing and re-open the hole the policy closes
// for every other user of the instance. Writing them here, at WriteHeader, is
// the one place a handler cannot get after.
type mockHeaderWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *mockHeaderWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true

	h := w.Header()
	h.Set(headerCSP, mockCSP)
	h.Set(headerNoSniff, "nosniff")
	h.Set(headerFrameOpts, "DENY")
	// Deliberately no Cross-Origin-Resource-Policy: a browser client fetching a
	// mock from a page on another origin is exactly what this is for, and
	// same-origin there would refuse the traffic rather than protect it. What
	// an endpoint may answer such a request with is still its own decision,
	// made with its own CORS headers.

	w.ResponseWriter.WriteHeader(status)
}

func (w *mockHeaderWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap keeps http.ResponseController working through this wrapper: a delayed
// mock response extends its own write deadline.
func (w *mockHeaderWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
