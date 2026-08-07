package web

import (
	"fmt"
	"net/http"
	"strings"
)

// probeMethods are the verbs handleNotFound asks the router about. HEAD is
// included because Go's routing answers it wherever GET is registered, and a
// client that gets a 405 deserves to be told so.
var probeMethods = []string{
	http.MethodGet, http.MethodHead, http.MethodPost,
	http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// handleNotFound answers everything no route claimed.
//
// It exists because the catch-all route it is registered under swallows the
// 405 that net/http's router would otherwise produce: with a pattern matching
// every path, the router never concludes that nothing matched. So the question
// is asked directly — would this path answer a different method? — and the
// distinction between "no such page" and "not that way" is preserved, now with
// the application's own page rather than a line of plain text.
// A path under /api/v1/ is answered in JSON, whichever of the two it turns out
// to be. A caller that has been sending JSON has no use for a page of HTML, and
// "this address answers PATCH" is exactly the kind of thing a script gets wrong
// and needs told.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if allowed := s.allowedMethods(r); len(allowed) > 0 {
		list := strings.Join(allowed, ", ")
		w.Header().Set("Allow", list)

		if isAPIPath(r) {
			s.apiError(w, r, http.StatusMethodNotAllowed,
				"this address answers %s, not %s", list, r.Method)
			return
		}
		s.renderMessage(w, r, http.StatusMethodNotAllowed,
			"Not that way",
			fmt.Sprintf("This address answers %s, not %s.", list, r.Method))
		return
	}

	if isAPIPath(r) {
		s.apiError(w, r, http.StatusNotFound,
			"no such route — GET %s lists what there is", pathAPIPrefix)
		return
	}
	s.renderMessage(w, r, http.StatusNotFound,
		"No such page",
		"The address does not exist here. Check the link, or start again from your projects.")
}

// allowedMethods reports which other verbs the router has a route for at this
// path, in the order they are conventionally listed.
func (s *Server) allowedMethods(r *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		if method == r.Method {
			continue
		}

		probe := r.Clone(r.Context())
		probe.Method = method

		// The catch-all matches everything, so a match on it is not a route.
		if _, pattern := s.mux.Handler(probe); pattern != "" && pattern != patternCatchAll {
			allowed = append(allowed, method)
		}
	}
	return allowed
}
