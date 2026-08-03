package web

import "net/http"

// The HTMX request and response headers this application uses. HTMX drives the
// small interactions — a confirmed delete, for now — while the pages themselves
// remain ordinary server-rendered HTML (DESIGN.md §9.2).
const (
	headerHXRequest  = "HX-Request"
	headerHXRedirect = "HX-Redirect"
)

// isHTMX reports whether the request came from HTMX rather than from a plain
// browser navigation.
func isHTMX(r *http.Request) bool { return r.Header.Get(headerHXRequest) == "true" }

// redirect sends the caller to url, in whichever way the caller understands.
//
// An ordinary form post gets 303, which turns the POST into a GET and makes the
// result safe to reload. HTMX is doing the request over XHR, where a 303 would
// be followed transparently and the resulting page swapped into a fragment with
// the address bar left behind — so it is told to navigate instead.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	if isHTMX(r) {
		w.Header().Set(headerHXRedirect, url)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}
