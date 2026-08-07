package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// Authentication for /api/v1/. Two credentials reach the same handlers: the
// session cookie, so the interface can call its own API, and a bearer token, so
// a shell script can (DESIGN.md §4, §8).
//
// **A request that presents a bearer token is authenticated by that token
// alone.** It never falls back to the session, and that is what makes it safe
// to exempt from the CSRF guard: a cross-site page cannot set an Authorization
// header without a preflight it will not be granted, so a forged request either
// carries no token — and is refused — or carries one the attacker already had,
// in which case CSRF was never the problem. The alternative, exempting a
// cookie-authenticated mutating route, is the hole the guard exists to close
// (DESIGN.md §5.1).
const (
	headerAuthorization = "Authorization"
	headerWWWAuth       = "WWW-Authenticate"
	// bearerScheme is compared case-insensitively, as RFC 9110 §11.1 requires.
	bearerScheme = "bearer"
)

// bearerToken returns the credential from an `Authorization: Bearer …` header.
// Anything else — no header, another scheme, an empty credential — is not a
// bearer attempt, and the request is left to the session.
func bearerToken(r *http.Request) (string, bool) {
	scheme, credential, found := strings.Cut(r.Header.Get(headerAuthorization), " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}

	credential = strings.TrimSpace(credential)
	return credential, credential != ""
}

// requireAPIUser resolves who is calling the management API, or refuses.
//
// It is the JSON counterpart of requireUser: an anonymous caller gets 401 and a
// sentence rather than a redirect to a login form it cannot fill in.
func (s *Server) requireAPIUser(h userHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, isBearer := bearerToken(r)

		// The rate limit is applied here, and here rather than anywhere else
		// for two reasons. It is before AuthenticateAPIToken, so a flood of
		// wrong tokens is refused without a database round trip each; and every
		// /api/v1/ route goes through this function, so a route added later is
		// limited by construction rather than by remembering to wrap it.
		if !s.allowAPI(w, r, presented, isBearer) {
			return
		}

		if !isBearer {
			user, ok := userFrom(r.Context())
			if !ok {
				s.rejectAPIAuth(w, r, "log in, or send an API token as `Authorization: Bearer …`")
				return
			}
			h(w, r, user)
			return
		}

		user, token, err := s.store.AuthenticateAPIToken(r.Context(), presented)
		switch {
		case errors.Is(err, core.ErrInvalidToken):
			// Unknown, revoked, expired or misspelt: one answer for all of them,
			// because saying which would tell a caller holding a guess how close
			// it was.
			s.rejectAPIAuth(w, r, "that API token is not valid — it may have been revoked, or it may have expired")
			return
		case err != nil:
			s.apiServerError(w, r, fmt.Errorf("authenticate api token: %w", err))
			return
		}

		// The token's name, never its secret. It is what makes a log line
		// attributable to one CI job rather than to an account.
		Logger(r.Context()).Debug("api token accepted",
			slog.String("token", token.Prefix),
			slog.String("token_name", token.Name),
		)
		h(w, r, user)
	})
}

// rejectAPIAuth answers 401. The challenge header is sent even to a caller who
// presented nothing, because that is the header's job: it says what this address
// accepts.
func (s *Server) rejectAPIAuth(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set(headerWWWAuth, `Bearer realm="restest"`)
	writeJSON(w, r, http.StatusUnauthorized, errorBody{Error: message})
}

// maxAPIBody bounds a request to the management API. It is above the 1 MiB cap
// on a collection seed and on a static response body, because a definition
// carrying one of those at its full size is still a definition we accept.
const maxAPIBody = 2 << 20

// decodeJSON reads the request body into dst, reporting whether it succeeded.
// It has already answered the caller when it returns false.
//
// Unknown fields are refused rather than ignored, for the same reason an
// unknown `_`-prefixed query parameter is (DESIGN.md §5): a misspelt field that
// is silently dropped looks exactly like a field that was applied, and the
// caller finds out later, from behaviour they cannot explain.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		s.rejectAPIBody(w, r, err)
		return false
	}
	// One JSON value per request. Two concatenated objects is a caller who
	// believes both were applied, and only the first would have been.
	if decoder.More() {
		s.apiError(w, r, http.StatusBadRequest, "the body holds more than one JSON value")
		return false
	}
	return true
}

// rejectAPIBody turns a decoding failure into a sentence naming what is wrong
// and, where the decoder knows it, where.
func (s *Server) rejectAPIBody(w http.ResponseWriter, r *http.Request, err error) {
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
		tooLarge  *http.MaxBytesError
	)

	switch {
	case errors.Is(err, io.EOF):
		s.apiError(w, r, http.StatusBadRequest, "send a JSON object as the request body")
	case errors.As(err, &syntaxErr):
		s.apiError(w, r, http.StatusBadRequest,
			"that is not valid JSON: %s, at byte %d", syntaxErr.Error(), syntaxErr.Offset)
	case errors.As(err, &typeErr):
		s.apiError(w, r, http.StatusBadRequest,
			"the field %q wants a %s, not a %s", typeErr.Field, typeErr.Type, typeErr.Value)
	case errors.As(err, &tooLarge):
		s.apiError(w, r, http.StatusRequestEntityTooLarge,
			"that body is larger than the %d byte limit", tooLarge.Limit)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json reports this as a bare error with no type of its own.
		// The message it carries is already the useful part.
		s.apiError(w, r, http.StatusBadRequest,
			"%s — check the spelling against the documented fields", err.Error())
	default:
		s.apiError(w, r, http.StatusBadRequest, "that body could not be read: %s", err.Error())
	}
}

// apiError answers a refusal the caller caused.
func (s *Server) apiError(w http.ResponseWriter, r *http.Request, status int, format string, args ...any) {
	writeJSON(w, r, status, errorBody{Error: fmt.Sprintf(format, args...)})
}

// apiServerError is the end of the line for anything unexpected on the API
// side: the detail goes to the log with the request id, the caller gets a
// sentence. It is serverError for a client that wants JSON.
func (s *Server) apiServerError(w http.ResponseWriter, r *http.Request, err error) {
	Logger(r.Context()).Error("api request failed", slog.String("error", err.Error()))
	writeJSON(w, r, http.StatusInternalServerError, errorBody{
		Error: "internal server error — the failure has been logged",
	})
}

// apiRejected turns the outcomes a caller can cause into the answer they
// deserve, reporting whether it handled the error. Anything it does not
// recognise is left to the handler, which has the context to describe it.
//
// The rules it reports are core's own: the API and the browser forms are
// refused by the same validation, so a definition that the interface would not
// accept is not accepted here either.
func (s *Server) apiRejected(w http.ResponseWriter, r *http.Request, err error) bool {
	if fe, ok := fieldErrors(err); ok {
		writeJSON(w, r, http.StatusUnprocessableEntity, errorBody{
			Error:  fe.Error(),
			Fields: fe,
		})
		return true
	}
	return false
}
