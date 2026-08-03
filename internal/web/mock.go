package web

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/mock"
)

// The mock URL space. Everything a project defines answers below
// /m/{slug}/ — a path prefix rather than a subdomain, so that the same URL
// works on localhost, on a self-hosted instance and in a public deployment
// (DESIGN.md §4).
const (
	mockPrefix  = "/m/"
	patternMock = mockPrefix + "{slug}/"
)

// mockWriteGrace is how much longer than its own delay a delayed response is
// given to reach the client. The server's WriteTimeout is shorter than the
// longest delay an endpoint may be configured with, so a slow endpoint would
// otherwise time out on the way out rather than answering slowly, which is the
// whole point of the setting.
const mockWriteGrace = 30 * time.Second

// mockError is the body of every mock request that did not produce a user's
// own response. It answers JSON because that is what the mock server itself
// answers, and a client that has to parse two error formats has been given a
// second thing to get wrong.
//
// The fields beyond the message are what make it worth reading: Allow says
// which verbs this path does answer, and Nearest names the routes that look
// most like the one asked for — the common cause of a 404 here is a typo, and
// a bare 404 sends the user back to the UI to look the path up.
type mockError struct {
	Error   string     `json:"error"`
	Project string     `json:"project,omitempty"`
	Method  string     `json:"method"`
	Path    string     `json:"path"`
	Allow   []string   `json:"allow,omitempty"`
	Nearest []mock.Ref `json:"nearest,omitempty"`
}

// handleMock serves everything under /m/{slug}/.
//
// It is registered without a method, so it sees every verb, and it is skipped
// by the session and CSRF middleware: mock traffic is unauthenticated by
// design, and a POST from a test client carries no CSRF token and should not
// need one.
func (s *Server) handleMock(w http.ResponseWriter, r *http.Request) {
	slug := strings.ToLower(r.PathValue("slug"))
	path := mockSubPath(r)

	result := s.matcher.Lookup(slug, r.Method, path)

	switch result.Outcome {
	case mock.Matched:
		s.serveMockResponse(w, r, result)

	case mock.WrongMethod:
		allow := strings.Join(result.Allow, ", ")
		w.Header().Set("Allow", allow)

		// An OPTIONS request that no endpoint claims is answered rather than
		// refused. "Which verbs does this path take" is exactly the question
		// OPTIONS asks, and the Allow header is already the answer.
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, r, http.StatusMethodNotAllowed, mockError{
			Error:   fmt.Sprintf("%s is defined for %s, not %s", path, allow, r.Method),
			Project: slug,
			Method:  r.Method,
			Path:    path,
			Allow:   result.Allow,
		})

	case mock.NoRoute:
		writeJSON(w, r, http.StatusNotFound, mockError{
			Error:   fmt.Sprintf("no endpoint matches %s %s in project %q", r.Method, path, slug),
			Project: slug,
			Method:  r.Method,
			Path:    path,
			Nearest: result.Nearest,
		})

	default: // mock.NoProject
		writeJSON(w, r, http.StatusNotFound, mockError{
			Error:  fmt.Sprintf("no project has the slug %q", slug),
			Method: r.Method,
			Path:   path,
		})
	}
}

// serveMockResponse writes the endpoint's own answer: its delay, then its
// status, headers and body.
func (s *Server) serveMockResponse(w http.ResponseWriter, r *http.Request, result mock.Result) {
	route := result.Route

	if !s.mockDelay(w, r, route.DelayMS) {
		// The client gave up while we were waiting. Nothing has been written,
		// so there is nothing to finish.
		return
	}

	header := w.Header()
	for name, value := range route.Headers {
		header.Set(name, value)
	}
	if header.Get("Content-Type") == "" && route.Body != "" {
		header.Set("Content-Type", sniffContentType(route.Body))
	}

	w.WriteHeader(route.StatusCode)

	// A 204 or a 304 must not carry a body, and net/http would log a complaint
	// rather than send one. Refusing to write it here keeps a status the user
	// chose from looking like a fault in the server.
	if !bodyAllowed(route.StatusCode) || route.Body == "" {
		return
	}
	if _, err := io.WriteString(w, route.Body); err != nil {
		Logger(r.Context()).Warn("write mock response body",
			slog.String("endpoint", route.ID.String()),
			slog.String("error", err.Error()),
		)
	}
}

// mockDelay waits out the endpoint's configured delay, reporting whether the
// request is still worth answering.
func (s *Server) mockDelay(w http.ResponseWriter, r *http.Request, delayMS int) bool {
	if delayMS <= 0 {
		return true
	}
	delay := time.Duration(delayMS) * time.Millisecond

	// Push this one response's write deadline past its own delay. The
	// alternative would be raising the server's WriteTimeout for every route,
	// which would weaken the guard everywhere to accommodate a setting used on
	// a handful of endpoints.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(delay + mockWriteGrace)); err != nil {
		Logger(r.Context()).Debug("extend write deadline for a delayed response",
			slog.String("error", err.Error()))
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

// mockSubPath is the path below the project prefix, still percent-encoded.
//
// Encoded is the point: r.URL.Path has already turned %2F into a slash, so
// matching on it would split /users/a%2Fb into three segments and hand the
// endpoint a parameter the client never sent. The matcher splits first and
// decodes each segment afterwards, and needs the escaped form to do it.
func mockSubPath(r *http.Request) string {
	escaped := r.URL.EscapedPath()
	if !strings.HasPrefix(escaped, mockPrefix) {
		// Unreachable through the route this handler is registered on.
		return "/"
	}

	rest := escaped[len(mockPrefix):]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i:]
	}
	return "/"
}

// sniffContentType decides what an endpoint that set no Content-Type of its own
// is serving.
//
// JSON is checked first and by parsing rather than by guessing, because
// net/http's own sniffing calls a JSON object text/plain — which is true and
// unhelpful for a mock REST API, where JSON is the overwhelmingly common case.
func sniffContentType(body string) string {
	if json.Valid([]byte(body)) {
		return "application/json; charset=utf-8"
	}
	return http.DetectContentType([]byte(body))
}

// bodyAllowed reports whether a response with this status may carry a body,
// following the same rule net/http applies.
func bodyAllowed(status int) bool {
	switch {
	case status >= 100 && status < 200:
		return false
	case status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	default:
		return true
	}
}

// reloadRoutes rebuilds the matcher's table after a change that affects it.
//
// A failure is logged rather than returned. The change the user asked for has
// already been committed, so reporting it as a failed save would be a lie; the
// scheduled refresh in mock.Router picks the change up within the interval
// even if this call did not.
func (s *Server) reloadRoutes(r *http.Request) {
	if err := s.matcher.Reload(r.Context()); err != nil {
		Logger(r.Context()).Error("rebuild the route table", slog.String("error", err.Error()))
	}
}
