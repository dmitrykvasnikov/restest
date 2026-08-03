// Package web holds everything that speaks HTTP: routing, middleware and
// handlers.
//
// Handlers stay thin on purpose. Business logic lives below them, so that the
// phase 2 runner can drive the same logic from a background worker with no
// HTTP request in sight (DESIGN.md §10).
package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// Pinger is the narrowest view of the database: a way to ask whether it is
// answering. The health handlers need nothing more, and keeping it separate
// lets them be tested without a store at all.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Store is what this package needs from core. It is an interface rather than
// *core.Store so that handler tests can run without Docker; the integration
// tests exercise the real implementation against real Postgres.
type Store interface {
	Pinger

	RegisterUser(ctx context.Context, email, password string) (core.User, error)
	Authenticate(ctx context.Context, email, password string) (core.User, error)
	UserByID(ctx context.Context, id uuid.UUID) (core.User, error)

	CreateProject(ctx context.Context, ownerID uuid.UUID, slug, name string) (core.Project, error)
	ProjectsByOwner(ctx context.Context, ownerID uuid.UUID) ([]core.Project, error)
	ProjectByOwnerAndSlug(ctx context.Context, ownerID uuid.UUID, slug string) (core.Project, error)
	UpdateProject(ctx context.Context, ownerID, id uuid.UUID, slug, name string) (core.Project, error)
	DeleteProject(ctx context.Context, ownerID, id uuid.UUID) error
}

// Options are the dependencies of a Server. A struct rather than a parameter
// list because this will keep growing, and a growing parameter list is how
// arguments end up silently swapped.
type Options struct {
	Logger *slog.Logger
	Store  Store
	// Sessions also settles the cookie policy: the CSRF cookie follows the
	// session cookie's Secure attribute rather than taking a setting of its
	// own, because two settings for one question can disagree.
	Sessions *scs.SessionManager
	// BaseURL is the address users reach this instance on, shown in the UI as
	// the root of their mock URLs.
	BaseURL string
}

// Server carries the dependencies shared by all handlers.
type Server struct {
	logger       *slog.Logger
	store        Store
	sessions     *scs.SessionManager
	templates    templateSet
	mux          *http.ServeMux
	baseURL      string
	assetVersion string
	secure       bool
}

// New returns a Server with its routes and templates ready. Templates are
// parsed here rather than on first use, so a broken template stops the process
// at startup instead of turning into a 500 for whoever hits that page first.
func New(opts Options) (*Server, error) {
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	version, err := assetVersion()
	if err != nil {
		return nil, err
	}

	s := &Server{
		logger:       opts.Logger,
		store:        opts.Store,
		sessions:     opts.Sessions,
		templates:    templates,
		mux:          http.NewServeMux(),
		baseURL:      strings.TrimRight(opts.BaseURL, "/"),
		assetVersion: version,
		secure:       opts.Sessions.Cookie.Secure,
	}
	s.routes()
	return s, nil
}

// Handler returns the fully wrapped HTTP handler for the application.
//
// Order matters: the id is assigned first so everything below can log it, the
// access log wraps the panic guard so that a recovered panic is still reported
// as the 500 it became, and the routes run innermost.
func (s *Server) Handler() http.Handler {
	return withRequestID(s.logger, logRequests(recoverPanic(s.withSession(s.mux))))
}

func (s *Server) routes() {
	// Probes and assets. Neither needs a session or a CSRF token; see
	// withSession, which skips the session machinery for exactly these paths.
	s.mux.HandleFunc("GET "+pathHealthz, s.handleHealthz)
	s.mux.HandleFunc("GET "+pathReadyz, s.handleReadyz)
	s.mux.Handle("GET "+pathStatic, s.staticHandler())

	// Public pages.
	s.mux.HandleFunc("GET /{$}", s.handleHome)
	s.mux.HandleFunc("GET /register", s.handleRegisterForm)
	s.mux.HandleFunc("POST /register", s.handleRegister)
	s.mux.HandleFunc("GET /login", s.handleLoginForm)
	s.mux.HandleFunc("POST /login", s.handleLogin)
	s.mux.HandleFunc("POST /logout", s.handleLogout)

	// Projects. Everything below requires an account.
	s.mux.Handle("GET /projects", s.requireUser(s.handleProjectList))
	s.mux.Handle("GET /projects/new", s.requireUser(s.handleProjectNew))
	s.mux.Handle("POST /projects", s.requireUser(s.handleProjectCreate))
	s.mux.Handle("GET /projects/{slug}", s.requireUser(s.handleProjectShow))
	s.mux.Handle("GET /projects/{slug}/edit", s.requireUser(s.handleProjectEdit))
	s.mux.Handle("POST /projects/{slug}", s.requireUser(s.handleProjectUpdate))
	s.mux.Handle("POST /projects/{slug}/delete", s.requireUser(s.handleProjectDelete))

	// Anything else. Registered last and matched last: every pattern above is
	// more specific, so this only sees paths nothing claimed.
	s.mux.HandleFunc(patternCatchAll, s.handleNotFound)
}

// patternCatchAll is the fallback route. It exists so that a mistyped URL gets
// the application's own page rather than net/http's plain-text 404 — and,
// because it swallows the method mismatch that ServeMux would otherwise turn
// into a 405, handleNotFound works that answer out for itself.
const patternCatchAll = "/"

// withSession wraps h in the session manager, the CSRF guard and the current
// user lookup — but only for the routes that need them.
//
// Probes and static assets are skipped deliberately: a browser sends the
// session cookie with every stylesheet request, and loading and re-saving the
// session for each one is a database round trip that buys nothing.
//
// The decision is made on the route the request actually resolves to, not on
// its path. A POST to /healthz matches no route and has to reach the session
// chain, because the page that answers it renders like any other.
func (s *Server) withSession(h http.Handler) http.Handler {
	sessioned := s.sessions.LoadAndSave(s.withCSRF(s.withUser(h)))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := s.mux.Handler(r); isUnsessioned(pattern) {
			h.ServeHTTP(w, r)
			return
		}
		sessioned.ServeHTTP(w, r)
	})
}

// isUnsessioned takes a matched route pattern, not a path.
func isUnsessioned(pattern string) bool {
	switch pattern {
	case "GET " + pathHealthz, "GET " + pathReadyz, "GET " + pathStatic:
		return true
	default:
		return false
	}
}

// errorBody is the shape of every error this package returns as JSON. Mock
// traffic answers JSON, so the API side of the application answers JSON too
// rather than making a caller parse two different error formats.
type errorBody struct {
	Error string `json:"error"`
}

// writeJSON sends v with the given status. A body that fails to encode is
// logged rather than returned: the status line has already gone out, so there
// is no way left to report the failure to the client.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		Logger(r.Context()).Error("write response body", slog.String("error", err.Error()))
	}
}
