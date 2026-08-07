package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The API tokens page. A token is what makes /api/v1/ usable from a script, so
// this page exists in the interface and the API has no route that mints one —
// see the note on the routes in server.go.
const (
	pathTokens      = "/tokens"
	pathTokenDelete = "/tokens/{id}/delete"
)

// tokenPage is what the page renders: the tokens that exist, the form to add
// another, and — once, immediately after creating one — the secret itself.
type tokenPage struct {
	Tokens []core.APIToken

	// Name and ExpiresInDays are the form's values, put back after a rejection.
	Name          string
	ExpiresInDays string

	// Created is the plaintext of a token just minted. It is shown on this one
	// rendering and never again: nothing stores it, and a refresh loses it,
	// which is the honest version of "copy it now".
	Created     string
	CreatedName string

	// Example is a command that uses the new token against this instance, so
	// that the first thing anyone does with it is something that works.
	Example string
}

// Any reports whether the account has tokens at all, which is what the page
// checks before rendering a table with nothing in it.
func (p tokenPage) Any() bool { return len(p.Tokens) > 0 }

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request, user core.User) {
	page, ok := s.tokenPage(w, r, user)
	if !ok {
		return
	}

	// Popped, not read: the secret is shown on this rendering and is gone from
	// the session by the time the page reaches the browser.
	page.Created = s.sessions.PopString(r.Context(), sessionKeyNewToken)
	page.CreatedName = s.sessions.PopString(r.Context(), sessionKeyNewTokenName)
	if page.Created != "" {
		page.Example = s.tokenExample(page.Created)
	}

	data := s.newPage(r, "API tokens")
	data.Form = page
	s.render(w, r, http.StatusOK, "tokens", data)
}

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return
	}

	name := r.PostFormValue("name")
	rawDays := strings.TrimSpace(r.PostFormValue("expires_in_days"))

	days, err := parseExpiryDays(rawDays)
	if err != nil {
		s.rejectToken(w, r, user, name, rawDays, err)
		return
	}

	token, plaintext, err := s.store.CreateAPIToken(r.Context(), user.ID, core.APITokenInput{
		Name:          name,
		ExpiresInDays: days,
	})
	if err != nil {
		s.rejectToken(w, r, user, name, rawDays, err)
		return
	}

	// The secret travels to the next request in the session — server-side, in
	// Postgres, and popped by the page that shows it — rather than in the URL,
	// where it would land in a proxy log and in the browser's history.
	s.sessions.Put(r.Context(), sessionKeyNewToken, plaintext)
	s.sessions.Put(r.Context(), sessionKeyNewTokenName, token.Name)

	redirect(w, r, pathTokens)
}

func (s *Server) handleTokenDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderMessage(w, r, http.StatusNotFound, "No such token",
			"The address does not name a token.")
		return
	}

	if err := s.store.DeleteAPIToken(r.Context(), user.ID, id); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// Revoked in another tab between the page loading and the button
			// being pressed. The end state is the one that was asked for.
			s.flash(r.Context(), flashInfo, "That token was already revoked.")
			redirect(w, r, pathTokens)
			return
		}
		s.serverError(w, r, fmt.Errorf("delete api token: %w", err))
		return
	}

	s.flash(r.Context(), flashSuccess, "Token revoked. Anything using it stops working now.")
	redirect(w, r, pathTokens)
}

// tokenPage reads the account's tokens, which every rendering of the page needs.
func (s *Server) tokenPage(w http.ResponseWriter, r *http.Request, user core.User) (tokenPage, bool) {
	tokens, err := s.store.APITokensByUser(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("list api tokens: %w", err))
		return tokenPage{}, false
	}
	return tokenPage{Tokens: tokens}, true
}

// rejectToken re-renders the page with the messages attached and the form as it
// was left.
func (s *Server) rejectToken(w http.ResponseWriter, r *http.Request, user core.User, name, days string, err error) {
	fe, ok := fieldErrors(err)
	if !ok {
		s.serverError(w, r, fmt.Errorf("create api token: %w", err))
		return
	}

	page, pageOK := s.tokenPage(w, r, user)
	if !pageOK {
		return
	}
	page.Name = name
	page.ExpiresInDays = days

	data := s.newPage(r, "API tokens")
	data.Errors = fe
	data.Form = page
	s.render(w, r, http.StatusUnprocessableEntity, "tokens", data)
}

// parseExpiryDays reads the expiry field. Empty means no expiry, which is the
// same thing zero means, so the field can be left alone.
func parseExpiryDays(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}

	days, err := strconv.Atoi(raw)
	if err != nil {
		var fe core.FieldErrors
		fe.Add("expires_in_days", "Enter a number of days, or leave it empty for a token that does not expire.")
		return 0, fe
	}
	return days, nil
}

// tokenExample is a command that proves the new token works. The index is the
// right thing to point it at: it changes nothing, and it answers with the
// account the token resolved to.
func (s *Server) tokenExample(plaintext string) string {
	return fmt.Sprintf("curl -H 'Authorization: Bearer %s' %s/api/v1/", plaintext, s.baseURL)
}
