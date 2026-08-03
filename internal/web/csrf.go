package web

import (
	"log/slog"
	"net/http"

	"github.com/justinas/nosurf"
)

// csrfCookieMaxAge bounds how long an unused CSRF cookie lingers. It outlives
// the time anyone leaves a form open, and is well short of the session.
const csrfCookieMaxAge = 12 * 60 * 60

// withCSRF rejects mutating requests that do not carry the token, which is the
// double-submit half of what stops a third-party page from posting to us on a
// logged-in user's behalf. SameSite=Lax on the session cookie is the other
// half; neither is relied on alone.
//
// Only unsafe methods are checked — nosurf lets GET, HEAD, OPTIONS and TRACE
// through, and those must not change anything anyway.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	h := nosurf.New(next)

	h.SetBaseCookie(http.Cookie{
		Path:     "/",
		MaxAge:   csrfCookieMaxAge,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})

	// nosurf checks Origin and Referer only for requests it believes are over
	// TLS. The Go server may itself be plain HTTP behind a proxy that
	// terminates TLS, so the answer comes from how users actually reach us —
	// the scheme of the configured base URL — not from r.TLS.
	h.SetIsTLSFunc(func(*http.Request) bool { return s.secure })

	// A request that matched no route has nothing to protect, and answering it
	// with "your form expired" would be a worse answer than the 404 or 405 it
	// has actually earned.
	h.ExemptFunc(func(r *http.Request) bool {
		_, pattern := s.mux.Handler(r)
		return pattern == patternCatchAll
	})

	h.SetFailureHandler(http.HandlerFunc(s.handleCSRFFailure))
	return h
}

// handleCSRFFailure explains the rejection instead of returning a bare 400.
// The overwhelmingly common cause is innocent — a form left open until its
// token expired, or a session that ended in another tab — and a user who is
// told that can simply try again.
func (s *Server) handleCSRFFailure(w http.ResponseWriter, r *http.Request) {
	Logger(r.Context()).Warn("csrf check failed",
		slog.String("path", r.URL.Path),
		slog.String("reason", nosurf.Reason(r).Error()),
	)

	s.renderMessage(w, r, http.StatusBadRequest,
		"That form has expired",
		"The page sat open too long, or the session ended in another tab. Go back, reload the page and try again.")
}
