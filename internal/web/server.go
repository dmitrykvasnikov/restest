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
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/mock"
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

	// API tokens. CreateAPIToken returns the plaintext as its second value,
	// which is the only time it exists: the row holds a SHA-256, so nothing
	// can produce it again.
	CreateAPIToken(ctx context.Context, userID uuid.UUID, in core.APITokenInput) (core.APIToken, string, error)
	APITokensByUser(ctx context.Context, userID uuid.UUID) ([]core.APIToken, error)
	DeleteAPIToken(ctx context.Context, userID, id uuid.UUID) error
	AuthenticateAPIToken(ctx context.Context, presented string) (core.User, core.APIToken, error)

	CreateProject(ctx context.Context, ownerID uuid.UUID, slug, name string, datasets []string) (core.Project, error)
	ProjectsByOwner(ctx context.Context, ownerID uuid.UUID) ([]core.Project, error)
	ProjectByOwnerAndSlug(ctx context.Context, ownerID uuid.UUID, slug string) (core.Project, error)
	UpdateProject(ctx context.Context, ownerID, id uuid.UUID, slug, name string) (core.Project, error)
	DeleteProject(ctx context.Context, ownerID, id uuid.UUID) error

	CreateEndpoint(ctx context.Context, ownerID, projectID uuid.UUID, in core.EndpointInput) (core.Endpoint, error)
	EndpointsByProject(ctx context.Context, ownerID, projectID uuid.UUID) ([]core.Endpoint, error)
	EndpointByOwnerAndID(ctx context.Context, ownerID, id uuid.UUID) (core.Endpoint, error)
	UpdateEndpoint(ctx context.Context, ownerID, id uuid.UUID, in core.EndpointInput) (core.Endpoint, error)
	DeleteEndpoint(ctx context.Context, ownerID, id uuid.UUID) error

	CreateCollection(ctx context.Context, ownerID, projectID uuid.UUID, in core.CollectionInput) (core.Collection, error)
	CollectionsByProject(ctx context.Context, ownerID, projectID uuid.UUID) ([]core.Collection, error)
	CollectionByOwnerAndID(ctx context.Context, ownerID, id uuid.UUID) (core.Collection, error)
	CollectionByOwnerAndName(ctx context.Context, ownerID uuid.UUID, slug, name string) (core.Collection, error)
	UpdateCollection(ctx context.Context, ownerID, id uuid.UUID, in core.CollectionInput) (core.Collection, error)
	DeleteCollection(ctx context.Context, ownerID, id uuid.UUID) error
	ResetCollection(ctx context.Context, id uuid.UUID) (int, error)

	// The document operations take a collection id and no owner. By the time one
	// is called the request has already been matched to an endpoint that names
	// the collection, and mock traffic has no account to scope by — the route
	// table is the authorisation, and it only ever holds collections a project
	// deliberately exposed.
	ListDocuments(ctx context.Context, collectionID uuid.UUID, q core.ListQuery) (core.DocumentPage, error)
	GetDocument(ctx context.Context, collectionID uuid.UUID, publicID string) (core.Document, error)
	CreateDocument(ctx context.Context, collectionID uuid.UUID, body []byte) (core.Document, error)
	ReplaceDocument(ctx context.Context, collectionID uuid.UUID, publicID string, body []byte) (core.Document, error)
	PatchDocument(ctx context.Context, collectionID uuid.UUID, publicID string, body []byte) (core.Document, error)
	DeleteDocument(ctx context.Context, collectionID uuid.UUID, publicID string) error

	// The request log. Like the document operations these take a project id and
	// no owner: the handler has already resolved the project through
	// ProjectByOwnerAndSlug, which is where the ownership check lives, because
	// the exchanges table carries no owner of its own (queries/exchanges.sql).
	ExchangesByProject(ctx context.Context, projectID uuid.UUID, before core.ExchangeCursor, limit int) ([]core.Exchange, error)
	ExchangesSince(ctx context.Context, projectID uuid.UUID, after core.ExchangeCursor, limit int) ([]core.Exchange, error)
	ExchangeByID(ctx context.Context, projectID, id uuid.UUID) (core.Exchange, error)
	LatestExchangeCursor(ctx context.Context, projectID uuid.UUID) (core.ExchangeCursor, error)
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
	// Routes is the matcher behind /m/{slug}/. The handlers that change an
	// endpoint ask it to rebuild, so it is a dependency of the UI as much as of
	// the mock server.
	Routes *mock.Router
	// BaseURL is the address users reach this instance on, shown in the UI as
	// the root of their mock URLs.
	BaseURL string
	// Recorder takes the exchanges the mock server produces. Optional: without
	// one nothing is recorded and the inspector says so, which is what the
	// handler tests run with.
	Recorder Recorder
	// LogBodyLimit is how much of a body the recording middleware keeps. Zero
	// falls back to defaultLogBodyLimit.
	LogBodyLimit int
	// LogRetentionMonths is how long the log is kept. The inspector says so on
	// the page, because "where did last quarter go" is a question worth
	// answering before it is asked.
	LogRetentionMonths int
	// DemoEnabled says whether this instance serves the shared demo project. It
	// changes nothing about routing — the demo is an ordinary project and the
	// matcher treats it as one — only whether the pages an anonymous visitor
	// sees offer it.
	DemoEnabled bool
}

// Server carries the dependencies shared by all handlers.
type Server struct {
	logger       *slog.Logger
	store        Store
	sessions     *scs.SessionManager
	matcher      *mock.Router
	templates    templateSet
	mux          *http.ServeMux
	baseURL      string
	assetVersion string
	secure       bool
	recorder     Recorder
	logBodyLimit int

	logRetentionMonths int
	demoEnabled        bool
}

// defaultLogBodyLimit is how much of a body is recorded when the caller did not
// say. It matches the default of RESTEST_LOG_BODY_LIMIT, so a Server built by
// hand behaves like the one main.go builds.
const defaultLogBodyLimit = 64 * 1024

// New returns a Server with its routes and templates ready. Templates are
// parsed here rather than on first use, so a broken template stops the process
// at startup instead of turning into a 500 for whoever hits that page first.
func New(opts Options) (*Server, error) {
	if opts.Routes == nil {
		return nil, errors.New("web: Options.Routes is required")
	}
	templates, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	version, err := assetVersion()
	if err != nil {
		return nil, err
	}

	limit := opts.LogBodyLimit
	if limit <= 0 {
		limit = defaultLogBodyLimit
	}

	s := &Server{
		logger:       opts.Logger,
		store:        opts.Store,
		sessions:     opts.Sessions,
		matcher:      opts.Routes,
		templates:    templates,
		mux:          http.NewServeMux(),
		baseURL:      strings.TrimRight(opts.BaseURL, "/"),
		assetVersion: version,
		secure:       opts.Sessions.Cookie.Secure,
		recorder:     opts.Recorder,
		logBodyLimit: limit,

		logRetentionMonths: opts.LogRetentionMonths,
		demoEnabled:        opts.DemoEnabled,
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

	// Endpoints. Listed on the project page; edited on their own.
	s.mux.Handle("GET /projects/{slug}/endpoints/new", s.requireUser(s.handleEndpointNew))
	s.mux.Handle("POST /projects/{slug}/endpoints", s.requireUser(s.handleEndpointCreate))
	s.mux.Handle("GET /projects/{slug}/endpoints/{id}/edit", s.requireUser(s.handleEndpointEdit))
	s.mux.Handle("POST /projects/{slug}/endpoints/{id}", s.requireUser(s.handleEndpointUpdate))
	s.mux.Handle("POST /projects/{slug}/endpoints/{id}/delete", s.requireUser(s.handleEndpointDelete))

	// Collections. Listed on the project page alongside the endpoints, because
	// what a project serves is both together.
	s.mux.Handle("GET /projects/{slug}/collections/new", s.requireUser(s.handleCollectionNew))
	s.mux.Handle("POST /projects/{slug}/collections", s.requireUser(s.handleCollectionCreate))
	s.mux.Handle("GET /projects/{slug}/collections/{id}/edit", s.requireUser(s.handleCollectionEdit))
	s.mux.Handle("POST /projects/{slug}/collections/{id}", s.requireUser(s.handleCollectionUpdate))
	s.mux.Handle("POST /projects/{slug}/collections/{id}/delete", s.requireUser(s.handleCollectionDelete))

	// The request log. The stream is registered on a literal path below the
	// list, and the detail view on a parameter beside it: a literal segment
	// beats a wildcard in net/http's router too, so /log/stream cannot be read
	// as an exchange id.
	s.mux.Handle("GET "+pathLog, s.requireUser(s.handleLogList))
	s.mux.Handle("GET "+pathLogStream, s.requireUser(s.handleLogStream))
	s.mux.Handle("GET "+pathLogEntry, s.requireUser(s.handleLogEntry))

	// API tokens. In the interface and not in the API: a token that could mint
	// tokens would turn one leaked CI credential into a permanent foothold, and
	// the account that established the trust is the one that should extend it.
	s.mux.Handle("GET "+pathTokens, s.requireUser(s.handleTokenList))
	s.mux.Handle("POST "+pathTokens, s.requireUser(s.handleTokenCreate))
	s.mux.Handle("POST "+pathTokenDelete, s.requireUser(s.handleTokenDelete))

	// The management API. Every route takes either the session cookie or an
	// `Authorization: Bearer` token — see apiauth.go for why presenting a token
	// is what exempts a request from the CSRF guard.
	s.mux.Handle("GET "+pathAPIRoot, s.requireAPIUser(s.handleAPIIndex))

	s.mux.Handle("GET "+pathAPIProjects, s.requireAPIUser(s.handleAPIProjectList))
	s.mux.Handle("POST "+pathAPIProjects, s.requireAPIUser(s.handleAPIProjectCreate))
	s.mux.Handle("GET "+pathAPIProject, s.requireAPIUser(s.handleAPIProjectShow))
	s.mux.Handle("PATCH "+pathAPIProject, s.requireAPIUser(s.handleAPIProjectUpdate))
	s.mux.Handle("DELETE "+pathAPIProject, s.requireAPIUser(s.handleAPIProjectDelete))

	s.mux.Handle("GET "+pathAPIEndpoints, s.requireAPIUser(s.handleAPIEndpointList))
	s.mux.Handle("POST "+pathAPIEndpoints, s.requireAPIUser(s.handleAPIEndpointCreate))
	s.mux.Handle("GET "+pathAPIEndpoint, s.requireAPIUser(s.handleAPIEndpointShow))
	s.mux.Handle("PUT "+pathAPIEndpoint, s.requireAPIUser(s.handleAPIEndpointUpdate))
	s.mux.Handle("DELETE "+pathAPIEndpoint, s.requireAPIUser(s.handleAPIEndpointDelete))

	s.mux.Handle("GET "+pathAPICollections, s.requireAPIUser(s.handleAPICollectionList))
	s.mux.Handle("POST "+pathAPICollections, s.requireAPIUser(s.handleAPICollectionCreate))
	s.mux.Handle("GET "+pathAPICollection, s.requireAPIUser(s.handleAPICollectionShow))
	s.mux.Handle("PATCH "+pathAPICollection, s.requireAPIUser(s.handleAPICollectionUpdate))
	s.mux.Handle("DELETE "+pathAPICollection, s.requireAPIUser(s.handleAPICollectionDelete))
	// The reset route is unchanged from M3, at the URL it has always had. What
	// M6 added is a credential it can be called with (DESIGN.md §5.1).
	s.mux.Handle("POST "+pathAPIReset, s.requireAPIUser(s.handleCollectionReset))

	s.mux.Handle("GET "+pathAPILog, s.requireAPIUser(s.handleAPILogList))
	s.mux.Handle("GET "+pathAPILogEntry, s.requireAPIUser(s.handleAPILogEntry))

	// Mock traffic. Registered without a method, because which verbs answer is
	// the project's decision and not this router's, and skipped by the session
	// and CSRF middleware — see isUnsessioned.
	//
	// The recording middleware wraps this one route and nothing else: the log
	// is of the traffic a project serves, not of somebody administering it.
	s.mux.Handle(patternMock, s.recordExchanges(http.HandlerFunc(s.handleMock)))

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
//
// Mock traffic is in the list for a stronger reason than the probes and assets
// are. It is unauthenticated by design (DESIGN.md §4), so a session would buy
// nothing — but more to the point, it is not browser traffic: a test client
// POSTing to a mock endpoint carries no CSRF token and must not be asked for
// one. Sending it through nosurf would make every write to a mock 400.
func isUnsessioned(pattern string) bool {
	switch pattern {
	case "GET " + pathHealthz, "GET " + pathReadyz, "GET " + pathStatic, patternMock:
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
	// Fields carries per-field messages when what was refused was a definition
	// rather than the request itself — the same messages the forms show beside
	// the field. It is absent otherwise, so a caller that reads only `error` is
	// never surprised by it.
	Fields core.FieldErrors `json:"fields,omitempty"`
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
