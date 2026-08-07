package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The management API (DESIGN.md §4). It exists so that a CI job can configure a
// mock the way a person configures one in the browser — create a project,
// define endpoints and collections, reset state between runs — without driving
// a form.
//
// It is the same domain underneath. Every handler here calls the same core
// method the corresponding page calls, so a rule enforced in the interface is
// enforced here by construction rather than by a second copy of it that has to
// be kept in step.
//
// Addressing follows what a script already knows: a project by its slug, a
// collection by its name, an endpoint by its id — because an endpoint has no
// name, and a verb-and-path pair in a URL would need escaping to be readable.
const (
	pathAPIPrefix = "/api/v1/"
	// pathAPIRoot is the index. {$} matches /api/v1/ exactly rather than
	// everything below it.
	pathAPIRoot = pathAPIPrefix + "{$}"

	pathAPIProjects = pathAPIPrefix + "projects"
	pathAPIProject  = pathAPIProjects + "/{slug}"

	pathAPIEndpoints = pathAPIProject + "/endpoints"
	pathAPIEndpoint  = pathAPIEndpoints + "/{id}"

	pathAPICollections = pathAPIProject + "/collections"
	pathAPICollection  = pathAPICollections + "/{name}"
	pathAPIReset       = pathAPICollection + "/reset"

	pathAPILog      = pathAPIProject + "/log"
	pathAPILogEntry = pathAPILog + "/{id}"
)

// apiRoutes is what the index lists, in the order a script would use them. It
// is written out rather than derived from the mux, because a route table has no
// way to say which of two patterns is worth mentioning first.
var apiRoutes = []string{
	"GET    " + pathAPIProjects,
	"POST   " + pathAPIProjects,
	"GET    " + pathAPIProject,
	"PATCH  " + pathAPIProject,
	"DELETE " + pathAPIProject,
	"GET    " + pathAPIEndpoints,
	"POST   " + pathAPIEndpoints,
	"GET    " + pathAPIEndpoint,
	"PUT    " + pathAPIEndpoint,
	"DELETE " + pathAPIEndpoint,
	"GET    " + pathAPICollections,
	"POST   " + pathAPICollections,
	"GET    " + pathAPICollection,
	"PATCH  " + pathAPICollection,
	"DELETE " + pathAPICollection,
	"POST   " + pathAPIReset,
	"GET    " + pathAPILog,
	"GET    " + pathAPILogEntry,
}

// apiIndex is what GET /api/v1/ answers: who the caller turned out to be, how
// they proved it, and what there is to call.
//
// It is the cheapest possible "does this token work". A script that has just
// been handed a secret should be able to find out before it starts creating
// things, and a 401 from a route that also does something is an ambiguous
// answer.
type apiIndex struct {
	User            string   `json:"user"`
	AuthenticatedBy string   `json:"authenticated_by"`
	Routes          []string `json:"routes"`
}

func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request, user core.User) {
	method := "session"
	if _, ok := bearerToken(r); ok {
		method = "token"
	}

	writeJSON(w, r, http.StatusOK, apiIndex{
		User:            user.Email,
		AuthenticatedBy: method,
		Routes:          apiRoutes,
	})
}

// projectSlugOf is the {slug} of the route, in the form projects are stored
// under.
func projectSlugOf(r *http.Request) string {
	return strings.ToLower(strings.TrimSpace(r.PathValue("slug")))
}

// findAPIProject resolves the {slug} in an API URL to a project the caller
// owns. A project belonging to somebody else is the same 404 as one that does
// not exist, exactly as it is in the interface.
func (s *Server) findAPIProject(w http.ResponseWriter, r *http.Request, user core.User) (core.Project, bool) {
	slug := projectSlugOf(r)

	project, err := s.store.ProjectByOwnerAndSlug(r.Context(), user.ID, slug)
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.apiError(w, r, http.StatusNotFound, "no project named %q", slug)
		return core.Project{}, false
	case err != nil:
		s.apiServerError(w, r, fmt.Errorf("find project %q: %w", slug, err))
		return core.Project{}, false
	}
	return project, true
}

// isAPIPath reports whether a request is addressed to the management API, which
// is what decides the shape of a refusal that never reached a handler — a 404,
// a 405, a rejected CSRF token. A caller that has been sending JSON should not
// be handed a page of HTML telling it to reload a form.
func isAPIPath(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, pathAPIPrefix)
}

// serverErrorFor reports an unexpected failure in whichever form the caller
// asked in. It exists for the handlers that answer both — the reset route, for
// now — where a page of HTML would be no use to a script and a JSON body no use
// to a browser.
func (s *Server) serverErrorFor(w http.ResponseWriter, r *http.Request, err error) {
	if wantsHTML(r) {
		s.serverError(w, r, err)
		return
	}
	s.apiServerError(w, r, err)
}
