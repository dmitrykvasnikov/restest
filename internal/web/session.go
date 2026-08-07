package web

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// Session lifetime. A development tool people leave open for days should not
// log them out over lunch; IdleTimeout is what bounds an abandoned session on a
// shared machine.
const (
	sessionLifetime    = 30 * 24 * time.Hour
	sessionIdleTimeout = 7 * 24 * time.Hour
	sessionCleanup     = 15 * time.Minute
)

// Session keys. Only the user id is kept: everything else about the account is
// read from the database on the request that needs it, so a change of address
// or a deleted account takes effect immediately rather than at the next login.
// The one exception is a token's plaintext, which crosses the redirect from
// "create" to "here it is" in the session — server-side, in Postgres, and
// popped by the page that shows it — because the alternatives are a URL that
// lands in a proxy log and a page that cannot be reloaded.
const (
	sessionKeyUserID     = "user_id"
	sessionKeyFlashes    = "flashes"
	sessionKeyAfterLogin = "after_login"

	sessionKeyNewToken     = "new_token"
	sessionKeyNewTokenName = "new_token_name"
)

// NewSessionManager builds the session manager over the sessions table the
// schema already defines. The returned function stops the store's cleanup
// goroutine and must be called on shutdown.
//
// Sessions are server-side and not JWTs, so that logging out revokes access at
// once rather than waiting for a token to expire (DESIGN.md §8).
func NewSessionManager(pool *pgxpool.Pool, secure bool) (*scs.SessionManager, func()) {
	store := pgxstore.NewWithCleanupInterval(pool, sessionCleanup)

	m := newSessionManager(secure)
	m.Store = store
	return m, store.StopCleanup
}

// newSessionManager holds the cookie policy, apart from where the sessions are
// kept. Tests build one over the default in-memory store, so that what they
// exercise is the policy this function defines rather than scs's defaults.
func newSessionManager(secure bool) *scs.SessionManager {
	m := scs.New()
	m.Lifetime = sessionLifetime
	m.IdleTimeout = sessionIdleTimeout
	m.Cookie.Name = "restest_session"
	m.Cookie.Path = "/"
	m.Cookie.HttpOnly = true
	// Lax rather than Strict: a link from an email or from documentation should
	// arrive at a logged-in page, and Lax still withholds the cookie from the
	// cross-site POSTs that CSRF is about.
	m.Cookie.SameSite = http.SameSiteLaxMode
	m.Cookie.Secure = secure
	m.Cookie.Persist = true

	return m
}

type userContextKey struct{}

// userFrom returns the account making the request, if there is one.
func userFrom(ctx context.Context) (core.User, bool) {
	u, ok := ctx.Value(userContextKey{}).(core.User)
	return u, ok
}

// withUser resolves the session's user id to an account and puts it in the
// request context. A session naming a user who no longer exists is destroyed
// rather than carried around, so a deleted account cannot keep browsing.
func (s *Server) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		raw := s.sessions.GetString(ctx, sessionKeyUserID)
		if raw == "" {
			next.ServeHTTP(w, r)
			return
		}

		id, err := uuid.Parse(raw)
		if err != nil {
			// Our own value, so this is corruption rather than input: log it
			// and treat the request as logged out.
			Logger(ctx).Error("session holds an unparseable user id", slog.String("value", raw))
			s.destroySession(ctx)
			next.ServeHTTP(w, r)
			return
		}

		user, err := s.store.UserByID(ctx, id)
		switch {
		case errors.Is(err, core.ErrNotFound):
			s.destroySession(ctx)
			next.ServeHTTP(w, r)
			return
		case err != nil:
			s.serverError(w, r, fmt.Errorf("load session user: %w", err))
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, userContextKey{}, user)))
	})
}

// userHandler is a handler that only runs for a logged-in request. Taking the
// user as an argument rather than digging it back out of the context means a
// handler cannot forget to check.
type userHandler func(w http.ResponseWriter, r *http.Request, user core.User)

// requireUser sends anonymous callers to the login form, remembering where they
// were trying to go.
func (s *Server) requireUser(h userHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := userFrom(r.Context())
		if !ok {
			if r.Method == http.MethodGet {
				s.rememberDestination(r)
			}
			s.flash(r.Context(), flashInfo, "Log in to continue.")
			redirect(w, r, "/login")
			return
		}
		h(w, r, user)
	})
}

// logIn attaches the account to the session and returns where to send them
// next. The session token is renewed first: reusing the anonymous token would
// leave a session fixed before login still valid after it.
func (s *Server) logIn(ctx context.Context, user core.User) (string, error) {
	if err := s.sessions.RenewToken(ctx); err != nil {
		return "", fmt.Errorf("renew session token: %w", err)
	}
	s.sessions.Put(ctx, sessionKeyUserID, user.ID.String())

	if dest := s.sessions.PopString(ctx, sessionKeyAfterLogin); dest != "" {
		return dest, nil
	}
	return "/projects", nil
}

// logOut throws the whole session away rather than clearing the user id, so
// nothing set while logged in survives into the next visitor's session on a
// shared browser.
func (s *Server) logOut(ctx context.Context) error {
	if err := s.sessions.Destroy(ctx); err != nil {
		return fmt.Errorf("destroy session: %w", err)
	}
	return nil
}

func (s *Server) destroySession(ctx context.Context) {
	if err := s.sessions.Destroy(ctx); err != nil {
		Logger(ctx).Error("destroy stale session", slog.String("error", err.Error()))
	}
}

// rememberDestination stores where an anonymous request was heading, so that
// logging in continues the journey instead of dropping the user on the project
// list.
func (s *Server) rememberDestination(r *http.Request) {
	dest := r.URL.RequestURI()
	// Only our own paths. A value starting with "//" is a protocol-relative URL
	// and would turn the post-login redirect into an open redirect.
	if !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") {
		return
	}
	s.sessions.Put(r.Context(), sessionKeyAfterLogin, dest)
}

// Flash levels. They name the intent, not the colour, so the templates decide
// how a message looks.
const (
	flashSuccess = "success"
	flashError   = "error"
	flashInfo    = "info"
)

// Flash is a one-shot message shown on the next page the user sees. It lives in
// the session so that it survives the redirect that follows every successful
// form post.
type Flash struct {
	Level   string
	Message string
}

// scs serialises session data with gob, which needs to be told about any type
// that travels inside an interface value.
func init() {
	gob.Register(Flash{})
	gob.Register([]Flash{})
}

func (s *Server) flash(ctx context.Context, level, message string) {
	flashes, _ := s.sessions.Get(ctx, sessionKeyFlashes).([]Flash)
	s.sessions.Put(ctx, sessionKeyFlashes, append(flashes, Flash{Level: level, Message: message}))
}

// popFlashes returns the pending messages and clears them, so that a refresh
// does not show the same "Project created" a second time.
func (s *Server) popFlashes(ctx context.Context) []Flash {
	flashes, _ := s.sessions.Pop(ctx, sessionKeyFlashes).([]Flash)
	return flashes
}
