package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// credentialsForm is what the login and registration pages echo back after a
// rejection. The password is deliberately absent: it is not put back into the
// HTML, so it cannot end up in a proxy log or a browser cache.
type credentialsForm struct {
	Email string
}

// handleHome is the front door. There is no marketing page to show, so a
// visitor goes to whichever place is useful to them.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r.Context()); ok {
		redirect(w, r, "/projects")
		return
	}
	redirect(w, r, "/login")
}

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r.Context()); ok {
		redirect(w, r, "/projects")
		return
	}
	s.render(w, r, http.StatusOK, "register", s.newPage(r, "Create an account"))
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return
	}

	email := r.PostFormValue("email")
	password := r.PostFormValue("password")

	user, err := s.store.RegisterUser(r.Context(), email, password)
	switch {
	case err == nil:
		// Registering logs the new account in: asking someone to type the
		// credentials they just chose, immediately, achieves nothing.
	case errors.Is(err, core.ErrEmailTaken):
		// Not a leak of who has an account: this address is being offered to
		// us by someone who would find out anyway on the next login attempt.
		s.rejectCredentials(w, r, "register", "Create an account", email,
			core.FieldErrors{"email": "That address is already registered. Log in instead."})
		return
	default:
		if fe, ok := fieldErrors(err); ok {
			s.rejectCredentials(w, r, "register", "Create an account", email, fe)
			return
		}
		s.serverError(w, r, fmt.Errorf("register user: %w", err))
		return
	}

	dest, err := s.logIn(r.Context(), user)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.flash(r.Context(), flashSuccess, "Welcome. Create your first project to get started.")
	redirect(w, r, dest)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFrom(r.Context()); ok {
		redirect(w, r, "/projects")
		return
	}
	s.render(w, r, http.StatusOK, "login", s.newPage(r, "Log in"))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return
	}

	email := r.PostFormValue("email")
	password := r.PostFormValue("password")

	user, err := s.store.Authenticate(r.Context(), email, password)
	if err != nil {
		if errors.Is(err, core.ErrInvalidCredentials) {
			// One message for a wrong address and a wrong password alike. The
			// form is not a way to find out who has an account here.
			s.rejectCredentials(w, r, "login", "Log in", email,
				core.FieldErrors{"password": "That email and password do not match an account."})
			return
		}
		s.serverError(w, r, fmt.Errorf("authenticate: %w", err))
		return
	}

	dest, err := s.logIn(r.Context(), user)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	redirect(w, r, dest)
}

// handleLogout is a POST, not a link: a GET that ends a session can be
// triggered by any image tag on any page the user visits.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.logOut(r.Context()); err != nil {
		s.serverError(w, r, err)
		return
	}
	// Destroy took the flash with it, so this one is put on the fresh session
	// that the next request will carry.
	s.flash(r.Context(), flashInfo, "You are logged out.")
	redirect(w, r, "/login")
}

// rejectCredentials re-renders a credentials form with the address filled in
// and the messages attached, and answers 422 rather than 200 so that the status
// line says what happened.
func (s *Server) rejectCredentials(w http.ResponseWriter, r *http.Request, page, title, email string, fe core.FieldErrors) {
	data := s.newPage(r, title)
	data.Errors = fe
	data.Form = credentialsForm{Email: email}
	s.render(w, r, http.StatusUnprocessableEntity, page, data)
}
