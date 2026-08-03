package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The management API. It is one route today — reset — because that is the one
// M3 needs; the rest of /api/v1/ arrives with API tokens in M6 (PLAN.md).
//
// **Authentication is the session cookie, and the CSRF guard applies.** That
// makes this route usable from the UI and not yet from a shell script, which is
// the honest state of it: the alternative was exempting a cookie-authenticated
// mutating route from CSRF, which is the hole the guard exists to close. A
// token sent as `Authorization: Bearer` is not a cookie and needs no such
// exemption, so M6 is what makes this route scriptable, and the URL it will be
// scripted at is this one.
const (
	pathAPIPrefix = "/api/v1/"
	pathAPIReset  = pathAPIPrefix + "projects/{slug}/collections/{name}/reset"
)

// resetResult is what a reset answers a programmatic caller with. The count is
// the point: a test suite that resets between runs wants to see that it got the
// fixture it expected rather than an empty collection.
type resetResult struct {
	Project    string `json:"project"`
	Collection string `json:"collection"`
	Documents  int    `json:"documents"`
}

// handleCollectionReset restores a collection to its seed.
//
// One handler answers both callers. A browser reaches it through the button on
// the collection page and is sent back where it came from with a message; a
// programmatic caller gets JSON. Two routes doing the same thing would be two
// places for the ownership check to be got wrong.
func (s *Server) handleCollectionReset(w http.ResponseWriter, r *http.Request, user core.User) {
	slug := strings.ToLower(r.PathValue("slug"))
	name := core.NormalizeCollectionName(r.PathValue("name"))

	collection, err := s.store.CollectionByOwnerAndName(r.Context(), user.ID, slug, name)
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.rejectReset(w, r, slug, http.StatusNotFound,
			fmt.Sprintf("no collection named %q in project %q", name, slug))
		return
	case err != nil:
		s.serverError(w, r, fmt.Errorf("find collection %q: %w", name, err))
		return
	}

	count, err := s.store.ResetCollection(r.Context(), collection.ID)
	if err != nil {
		if fe, ok := fieldErrors(err); ok {
			// The seed was edited into something that cannot be applied — an
			// entry with a duplicate id, most likely. It was accepted as valid
			// JSON when it was saved; this is where it stops being applicable.
			s.rejectReset(w, r, slug, http.StatusUnprocessableEntity, fe.Error())
			return
		}
		s.serverError(w, r, fmt.Errorf("reset collection %q: %w", name, err))
		return
	}

	if wantsHTML(r) {
		s.flash(r.Context(), flashSuccess, fmt.Sprintf(
			"Collection %q reset to its seed: %s.", collection.Name, documentCount(count)))
		redirect(w, r, projectPath(slug))
		return
	}
	writeJSON(w, r, http.StatusOK, resetResult{
		Project:    slug,
		Collection: collection.Name,
		Documents:  count,
	})
}

// rejectReset answers a refused reset in whichever form the caller asked in.
func (s *Server) rejectReset(w http.ResponseWriter, r *http.Request, slug string, status int, message string) {
	if wantsHTML(r) {
		s.flash(r.Context(), flashError, message)
		redirect(w, r, projectPath(slug))
		return
	}
	writeJSON(w, r, status, errorBody{Error: message})
}

// wantsHTML reports whether the caller is the browser UI rather than a script.
//
// It asks whether the request came from HTMX, not what it will accept: the
// button on the collection page is the only browser caller, and it is an HTMX
// post. Anything else is treated as programmatic and gets JSON, which is the
// safer default for a route whose whole purpose is to be called by a test
// suite.
func wantsHTML(r *http.Request) bool { return isHTMX(r) }

func documentCount(n int) string {
	if n == 1 {
		return "1 document"
	}
	return fmt.Sprintf("%d documents", n)
}

// resetPath is the reset URL of one collection. The UI links to the same
// address a script will use, so the one shown in the interface is the one that
// gets documented.
func resetPath(slug, name string) string {
	return pathAPIPrefix + "projects/" + slug + "/collections/" + name + "/reset"
}

// requireUserAPI is requireUser for the JSON side: an anonymous caller gets 401
// and a sentence rather than a redirect to a login page it cannot fill in.
func (s *Server) requireUserAPI(h userHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := userFrom(r.Context())
		if !ok {
			writeJSON(w, r, http.StatusUnauthorized, errorBody{
				Error: "log in, or send an API token once /api/v1/ accepts them",
			})
			return
		}
		h(w, r, user)
	})
}
