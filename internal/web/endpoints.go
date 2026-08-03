package web

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The defaults a new endpoint form opens with. A blank form would be correct
// and useless: the point of the first endpoint is to see one answer, and these
// values do that the moment they are saved.
const (
	defaultEndpointMethod  = http.MethodGet
	defaultEndpointPath    = "/hello"
	defaultEndpointStatus  = "200"
	defaultEndpointHeaders = "Content-Type: application/json"
	defaultEndpointBody    = "{\n  \"message\": \"hello\"\n}"
)

// endpointForm is the new and edit pages.
//
// The numeric fields are strings because this is what the user typed: a status
// code of "20O" has to come back in the field the way it was entered, next to a
// message saying why it was refused, and an int cannot hold that.
type endpointForm struct {
	Method     string
	Path       string
	StatusCode string
	DelayMS    string
	IsEnabled  bool
	Headers    string
	Body       string

	// Methods populates the verb selector.
	Methods []string
	// Project is the project being edited under, for the page's links and for
	// showing the mock URL this endpoint will answer on.
	Project core.Project
	// Action is where the form posts. DeleteAction is empty on the new page,
	// which has nothing to delete.
	Action       string
	DeleteAction string
}

// projectShow is the project page: the project itself and what it serves.
type projectShow struct {
	Project   core.Project
	Endpoints []core.Endpoint
}

func (s *Server) handleEndpointNew(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	data := s.newPage(r, "New endpoint")
	data.Form = endpointForm{
		Method:     defaultEndpointMethod,
		Path:       defaultEndpointPath,
		StatusCode: defaultEndpointStatus,
		DelayMS:    "0",
		IsEnabled:  true,
		Headers:    defaultEndpointHeaders,
		Body:       defaultEndpointBody,
		Methods:    core.Methods,
		Project:    project,
		Action:     endpointsPath(project.Slug),
	}
	s.render(w, r, http.StatusOK, "endpoint_form", data)
}

func (s *Server) handleEndpointCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}
	form, ok := s.readEndpointForm(w, r)
	if !ok {
		return
	}
	form.Project = project
	form.Action = endpointsPath(project.Slug)

	input, fe := form.toInput()
	if fe != nil {
		s.renderEndpointForm(w, r, "New endpoint", form, fe)
		return
	}

	endpoint, err := s.store.CreateEndpoint(r.Context(), user.ID, project.ID, input)
	if err != nil {
		s.rejectEndpoint(w, r, "New endpoint", form, err)
		return
	}
	s.endpointSaved(w, r, project, endpoint, "Endpoint created.")
}

func (s *Server) handleEndpointEdit(w http.ResponseWriter, r *http.Request, user core.User) {
	project, endpoint, ok := s.findEndpoint(w, r, user)
	if !ok {
		return
	}

	data := s.newPage(r, "Edit "+endpoint.Method+" "+endpoint.Path)
	data.Form = endpointFormFrom(project, endpoint)
	s.render(w, r, http.StatusOK, "endpoint_form", data)
}

func (s *Server) handleEndpointUpdate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, endpoint, ok := s.findEndpoint(w, r, user)
	if !ok {
		return
	}
	form, ok := s.readEndpointForm(w, r)
	if !ok {
		return
	}
	form.Project = project
	form.Action = endpointPath(project.Slug, endpoint.ID)
	form.DeleteAction = form.Action + "/delete"

	title := "Edit " + endpoint.Method + " " + endpoint.Path

	input, fe := form.toInput()
	if fe != nil {
		s.renderEndpointForm(w, r, title, form, fe)
		return
	}

	updated, err := s.store.UpdateEndpoint(r.Context(), user.ID, endpoint.ID, input)
	if err != nil {
		s.rejectEndpoint(w, r, title, form, err)
		return
	}
	s.endpointSaved(w, r, project, updated, "Endpoint saved.")
}

func (s *Server) handleEndpointDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	project, endpoint, ok := s.findEndpoint(w, r, user)
	if !ok {
		return
	}

	if err := s.store.DeleteEndpoint(r.Context(), user.ID, endpoint.ID); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// Deleted in another tab between the page loading and the button
			// being pressed. The end state is the one that was asked for.
			s.flash(r.Context(), flashInfo, "That endpoint was already deleted.")
			redirect(w, r, projectPath(project.Slug))
			return
		}
		s.serverError(w, r, fmt.Errorf("delete endpoint: %w", err))
		return
	}

	s.reloadRoutes(r)
	s.flash(r.Context(), flashSuccess,
		fmt.Sprintf("%s %s no longer answers.", endpoint.Method, endpoint.Path))
	redirect(w, r, projectPath(project.Slug))
}

// endpointSaved is the tail of a successful create or update: rebuild the route
// table, say where the endpoint answers, and go back to the project.
func (s *Server) endpointSaved(w http.ResponseWriter, r *http.Request, project core.Project, endpoint core.Endpoint, message string) {
	s.reloadRoutes(r)

	if endpoint.IsEnabled {
		s.flash(r.Context(), flashSuccess, fmt.Sprintf("%s Try it: %s %s%s",
			message, endpoint.Method, s.baseURL, mockURLPath(project, endpoint)))
	} else {
		s.flash(r.Context(), flashSuccess, message+" It is disabled, so it does not answer yet.")
	}
	redirect(w, r, projectPath(project.Slug))
}

// findEndpoint resolves both the {slug} and the {id} in an endpoint URL. The
// endpoint has to belong to the project in the path as well as to the caller,
// so that an id from one project cannot be edited through another's URL.
func (s *Server) findEndpoint(w http.ResponseWriter, r *http.Request, user core.User) (core.Project, core.Endpoint, bool) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return core.Project{}, core.Endpoint{}, false
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderMessage(w, r, http.StatusNotFound, "No such endpoint",
			"The address does not name an endpoint.")
		return core.Project{}, core.Endpoint{}, false
	}

	endpoint, err := s.store.EndpointByOwnerAndID(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, core.ErrNotFound), err == nil && endpoint.ProjectID != project.ID:
		s.renderMessage(w, r, http.StatusNotFound, "No such endpoint",
			"It may have been deleted, or the address may be wrong.")
		return core.Project{}, core.Endpoint{}, false
	case err != nil:
		s.serverError(w, r, fmt.Errorf("find endpoint %s: %w", id, err))
		return core.Project{}, core.Endpoint{}, false
	}
	return project, endpoint, true
}

// readEndpointForm pulls the fields out of a submission. It fails only when the
// body itself could not be decoded; everything a user can type wrong is caught
// by toInput, which reports it beside the field.
func (s *Server) readEndpointForm(w http.ResponseWriter, r *http.Request) (endpointForm, bool) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return endpointForm{}, false
	}

	return endpointForm{
		Method:     r.PostFormValue("method"),
		Path:       r.PostFormValue("path"),
		StatusCode: r.PostFormValue("status_code"),
		DelayMS:    r.PostFormValue("delay_ms"),
		IsEnabled:  r.PostFormValue("is_enabled") != "",
		Headers:    r.PostFormValue("headers"),
		// Browsers submit textarea newlines as CRLF. Left alone, every saved
		// body would grow a carriage return per line and be served with them.
		Body:    strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n"),
		Methods: core.Methods,
	}, true
}

// toInput converts what was typed into what the store takes, reporting the two
// failures that belong to the form rather than to the domain: a status code and
// a delay that are not numbers at all.
//
// Everything else — the range of the status, the shape of the path, the header
// names — is core's to judge, so that the management API in M6 enforces exactly
// the same rules rather than a second copy of them.
func (f endpointForm) toInput() (core.EndpointInput, core.FieldErrors) {
	var fe core.FieldErrors

	status, err := strconv.Atoi(strings.TrimSpace(f.StatusCode))
	if err != nil {
		fe.Add("status_code", "Enter a status code, such as 200.")
	}
	// An untouched number field arrives as "", which means "no delay" rather
	// than "not a number".
	delay, err := strconv.Atoi(strings.TrimSpace(cmp.Or(f.DelayMS, "0")))
	if err != nil {
		fe.Add("delay_ms", "Enter a delay in milliseconds, or 0.")
	}
	if fe != nil {
		return core.EndpointInput{}, fe
	}

	return core.EndpointInput{
		Method:     f.Method,
		Path:       f.Path,
		StatusCode: status,
		DelayMS:    delay,
		IsEnabled:  f.IsEnabled,
		Body:       f.Body,
		Headers:    core.ParseHeaderLines(f.Headers),
	}, nil
}

// endpointFormFrom fills the edit page from a stored endpoint.
func endpointFormFrom(project core.Project, endpoint core.Endpoint) endpointForm {
	action := endpointPath(project.Slug, endpoint.ID)

	return endpointForm{
		Method:       endpoint.Method,
		Path:         endpoint.Path,
		StatusCode:   strconv.Itoa(endpoint.StatusCode),
		DelayMS:      strconv.Itoa(endpoint.DelayMS),
		IsEnabled:    endpoint.IsEnabled,
		Headers:      endpoint.Headers.Lines(),
		Body:         endpoint.Body,
		Methods:      core.Methods,
		Project:      project,
		Action:       action,
		DeleteAction: action + "/delete",
	}
}

// rejectEndpoint turns the outcomes a user can cause into messages on the form,
// and anything else into a 500.
func (s *Server) rejectEndpoint(w http.ResponseWriter, r *http.Request, title string, form endpointForm, err error) {
	fe, ok := fieldErrors(err)
	switch {
	case ok:
	case errors.Is(err, core.ErrEndpointExists):
		fe = core.FieldErrors{"path": "This project already answers that method at that path."}
	case errors.Is(err, core.ErrNotFound):
		s.renderMessage(w, r, http.StatusNotFound, "No such endpoint",
			"It may have been deleted while this page was open.")
		return
	default:
		s.serverError(w, r, fmt.Errorf("save endpoint: %w", err))
		return
	}
	s.renderEndpointForm(w, r, title, form, fe)
}

func (s *Server) renderEndpointForm(w http.ResponseWriter, r *http.Request, title string, form endpointForm, fe core.FieldErrors) {
	data := s.newPage(r, title)
	data.Errors = fe
	data.Form = form
	s.render(w, r, http.StatusUnprocessableEntity, "endpoint_form", data)
}

// The URL shapes of this section, in one place, so a renamed route is one edit
// rather than a search through format strings.
func projectPath(slug string) string   { return "/projects/" + slug }
func endpointsPath(slug string) string { return projectPath(slug) + "/endpoints" }

func endpointPath(slug string, id uuid.UUID) string {
	return endpointsPath(slug) + "/" + id.String()
}

// mockURLPath is where an endpoint answers, relative to the host: the project's
// mock prefix with the endpoint's own path below it.
func mockURLPath(project core.Project, endpoint core.Endpoint) string {
	return strings.TrimSuffix(project.MockPath(), "/") + endpoint.Path
}
