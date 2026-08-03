package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/justinas/nosurf"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// templateSet holds one fully parsed template per page, keyed by the page's
// file name without its extension.
//
// Each entry is the layout, the partials and that one page parsed together.
// Parsing per page rather than once for everything means two pages may define
// the same block — "content" — without colliding.
type templateSet map[string]*template.Template

func parseTemplates() (templateSet, error) {
	pages, err := fs.Glob(templateFS, "templates/pages/*.html")
	if err != nil {
		return nil, fmt.Errorf("find page templates: %w", err)
	}
	if len(pages) == 0 {
		return nil, errors.New("no page templates were embedded")
	}

	set := make(templateSet, len(pages))
	for _, page := range pages {
		// The template is named for the layout, so executing it renders the
		// whole page rather than only the block the page file defines.
		t, err := template.New("base.html").ParseFS(templateFS,
			"templates/base.html",
			"templates/partials/*.html",
			page,
		)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", page, err)
		}
		set[strings.TrimSuffix(path.Base(page), ".html")] = t
	}
	return set, nil
}

// pageData is what every template is executed with. One struct for all pages,
// with Form carrying whatever the current page needs, so that the layout can
// count on the fields it uses always being there.
type pageData struct {
	Title        string
	Path         string
	BaseURL      string
	CSRFToken    string
	AssetVersion string

	// User is nil for an anonymous visitor, which is how the layout decides
	// between showing the navigation and showing a "log in" link.
	User    *core.User
	Flashes []Flash

	// Errors holds per-field validation messages, rendered next to the field
	// they belong to. Form holds the values the user typed, so a rejected form
	// comes back filled in rather than blank.
	Errors core.FieldErrors
	Form   any

	// Message carries the body of the standalone message page: 404, 500 and
	// the CSRF rejection all use it.
	Message string
}

// Asset returns the URL of a static file with the version stamp that lets it be
// cached forever. The stamp is a hash of the embedded assets, so a rebuild that
// changes a stylesheet changes the URL and a rebuild that does not, does not.
func (p pageData) Asset(name string) string {
	return pathStatic + strings.TrimPrefix(name, "/") + "?v=" + url.QueryEscape(p.AssetVersion)
}

// newPage builds the data every template needs. It pops the pending flash
// messages, so it must be called once per rendered response and only inside the
// session middleware.
func (s *Server) newPage(r *http.Request, title string) pageData {
	data := pageData{
		Title:        title,
		Path:         r.URL.Path,
		BaseURL:      s.baseURL,
		CSRFToken:    nosurf.Token(r),
		AssetVersion: s.assetVersion,
		Flashes:      s.popFlashes(r.Context()),
	}
	if user, ok := userFrom(r.Context()); ok {
		data.User = &user
	}
	return data
}

// render writes a page. The template is executed into a buffer first: a
// template that fails halfway through would otherwise have already sent a 200
// and half a page, leaving no way to report the failure.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, data pageData) {
	t, ok := s.templates[page]
	if !ok {
		// A page name that does not exist is a bug in this package, not
		// anything the request did.
		s.renderFailed(w, r, fmt.Errorf("no template named %q", page))
		return
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		s.renderFailed(w, r, fmt.Errorf("execute template %q: %w", page, err))
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		Logger(r.Context()).Error("write page", slog.String("error", err.Error()))
	}
}

// renderFailed is the answer when rendering itself is broken. It deliberately
// does not try to render anything: whatever went wrong would go wrong again.
func (s *Server) renderFailed(w http.ResponseWriter, r *http.Request, err error) {
	Logger(r.Context()).Error("render failed", slog.String("error", err.Error()))
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// renderMessage shows a standalone page with a heading and a sentence — the
// shape every non-page answer takes, from a 404 to a rejected CSRF token.
func (s *Server) renderMessage(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	data := s.newPage(r, title)
	data.Message = message
	s.render(w, r, status, "message", data)
}

// serverError is the end of the line for anything unexpected. The detail goes
// to the log with the request id attached; the user gets a sentence, because
// the detail is either useless to them or something they should not see.
func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	Logger(r.Context()).Error("request failed", slog.String("error", err.Error()))
	s.renderMessage(w, r, http.StatusInternalServerError,
		"Something went wrong",
		"The failure has been logged. Try again in a moment.")
}

// fieldErrors separates a validation failure from a real one. A rejected form
// is an ordinary outcome and gets re-rendered with messages; anything else is
// a 500.
func fieldErrors(err error) (core.FieldErrors, bool) {
	var fe core.FieldErrors
	ok := errors.As(err, &fe)
	return fe, ok
}
