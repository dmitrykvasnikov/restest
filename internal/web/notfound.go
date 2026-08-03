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
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if allowed := s.allowedMethods(r); len(allowed) > 0 {
		list := strings.Join(allowed, ", ")
		w.Header().Set("Allow", list)
		s.renderMessage(w, r, http.StatusMethodNotAllowed,
			"Not that way",
			fmt.Sprintf("This address answers %s, not %s.", list, r.Method))
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
