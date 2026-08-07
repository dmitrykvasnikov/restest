package web

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// apiEndpoint is an endpoint as the API renders it.
//
// It carries an id, unlike a project or a collection, because an endpoint has
// no name. What identifies it in the interface is its verb and path together,
// and a pair of those in a URL would have to be escaped to be readable.
type apiEndpoint struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Method    string    `json:"method"`
	Path      string    `json:"path"`
	Enabled   bool      `json:"enabled"`
	DelayMS   int       `json:"delay_ms"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// StatusCode and Body belong to a static endpoint, Collection to a
	// collection one, and each is absent for the other kind rather than sent as
	// a zero that means nothing.
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	// Collection is the collection's name, not its id: it is what the caller
	// created the collection under, and what the collection routes address it
	// by.
	Collection string `json:"collection,omitempty"`

	Headers core.Headers `json:"headers"`
	// MockURL is where this endpoint answers, absolute. For a collection it is
	// the root the six REST routes hang from.
	MockURL string `json:"mock_url"`
}

// endpointRequest is the body of both POST and PUT: the whole definition.
//
// PUT rather than PATCH, unlike a project or a collection, because an
// endpoint's fields are not independent. `kind` decides which of status_code,
// body and collection mean anything, so a partial update would have to invent
// an answer for "the kind changed and the body was not mentioned". Sending the
// endpoint you want is unambiguous.
//
// `enabled` is a pointer so that omitting it means enabled. A definition that
// silently arrived switched off would look exactly like a matcher that had
// stopped working.
type endpointRequest struct {
	Kind    string `json:"kind"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Enabled *bool  `json:"enabled"`
	DelayMS int    `json:"delay_ms"`

	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
	Collection string `json:"collection"`

	Headers core.Headers `json:"headers"`
}

func (s *Server) apiEndpointOf(e core.Endpoint, slug string, collections map[uuid.UUID]string) apiEndpoint {
	out := apiEndpoint{
		ID:        e.ID,
		Kind:      e.Kind,
		Method:    e.Method,
		Path:      e.Path,
		Enabled:   e.IsEnabled,
		DelayMS:   e.DelayMS,
		Headers:   e.Headers,
		MockURL:   s.baseURL + "/m/" + slug + e.Path,
		CreatedAt: e.CreatedAt.UTC(),
		UpdatedAt: e.UpdatedAt.UTC(),
	}
	if e.Headers == nil {
		out.Headers = core.Headers{}
	}

	if e.Kind == core.KindCollection {
		out.Collection = collections[e.CollectionID]
		return out
	}
	out.StatusCode = e.StatusCode
	out.Body = e.Body
	return out
}

func (s *Server) handleAPIEndpointList(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	endpoints, err := s.store.EndpointsByProject(r.Context(), user.ID, project.ID)
	if err != nil {
		s.apiServerError(w, r, fmt.Errorf("list endpoints: %w", err))
		return
	}
	names, ok := s.apiCollectionNames(w, r, user, project)
	if !ok {
		return
	}

	items := make([]apiEndpoint, len(endpoints))
	for i, endpoint := range endpoints {
		items[i] = s.apiEndpointOf(endpoint, project.Slug, names)
	}
	writeJSON(w, r, http.StatusOK, apiListOf(items))
}

func (s *Server) handleAPIEndpointCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	input, ok := s.readEndpointRequest(w, r, user, project)
	if !ok {
		return
	}

	endpoint, err := s.store.CreateEndpoint(r.Context(), user.ID, project.ID, input)
	if err != nil {
		s.rejectAPIEndpoint(w, r, err)
		return
	}

	// The whole point of the route: an endpoint defined by a script answers the
	// next request, with no restart and no deploy.
	s.reloadRoutes(r)

	names, ok := s.apiCollectionNames(w, r, user, project)
	if !ok {
		return
	}
	w.Header().Set("Location", endpointAPIPath(project.Slug, endpoint.ID))
	writeJSON(w, r, http.StatusCreated, s.apiEndpointOf(endpoint, project.Slug, names))
}

func (s *Server) handleAPIEndpointShow(w http.ResponseWriter, r *http.Request, user core.User) {
	project, endpoint, ok := s.findAPIEndpoint(w, r, user)
	if !ok {
		return
	}
	names, ok := s.apiCollectionNames(w, r, user, project)
	if !ok {
		return
	}
	writeJSON(w, r, http.StatusOK, s.apiEndpointOf(endpoint, project.Slug, names))
}

func (s *Server) handleAPIEndpointUpdate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, endpoint, ok := s.findAPIEndpoint(w, r, user)
	if !ok {
		return
	}

	input, ok := s.readEndpointRequest(w, r, user, project)
	if !ok {
		return
	}

	updated, err := s.store.UpdateEndpoint(r.Context(), user.ID, endpoint.ID, input)
	if err != nil {
		s.rejectAPIEndpoint(w, r, err)
		return
	}
	s.reloadRoutes(r)

	names, ok := s.apiCollectionNames(w, r, user, project)
	if !ok {
		return
	}
	writeJSON(w, r, http.StatusOK, s.apiEndpointOf(updated, project.Slug, names))
}

func (s *Server) handleAPIEndpointDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	_, endpoint, ok := s.findAPIEndpoint(w, r, user)
	if !ok {
		return
	}

	if err := s.store.DeleteEndpoint(r.Context(), user.ID, endpoint.ID); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.apiServerError(w, r, fmt.Errorf("delete endpoint: %w", err))
		return
	}
	s.reloadRoutes(r)

	w.WriteHeader(http.StatusNoContent)
}

// readEndpointRequest decodes a definition and turns it into the input core
// takes, resolving the collection's name to its id along the way.
//
// The kind is inferred when a collection is named and no kind is given: a body
// saying `{"collection": "users", "path": "/users"}` cannot mean anything else,
// and making a caller write `"kind": "collection"` beside it is asking them to
// say the same thing twice.
func (s *Server) readEndpointRequest(
	w http.ResponseWriter, r *http.Request, user core.User, project core.Project,
) (core.EndpointInput, bool) {
	var in endpointRequest
	if !s.decodeJSON(w, r, &in) {
		return core.EndpointInput{}, false
	}

	kind := in.Kind
	if kind == "" && in.Collection != "" {
		kind = core.KindCollection
	}

	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}

	input := core.EndpointInput{
		Kind:       kind,
		Method:     in.Method,
		Path:       in.Path,
		StatusCode: in.StatusCode,
		DelayMS:    in.DelayMS,
		IsEnabled:  enabled,
		Body:       in.Body,
		Headers:    in.Headers,
	}

	if kind != core.KindCollection {
		if in.Collection != "" {
			writeJSON(w, r, http.StatusUnprocessableEntity, errorBody{
				Error: "a static endpoint cannot name a collection",
				Fields: core.FieldErrors{
					"collection": `Only an endpoint of kind "collection" is bound to one.`,
				},
			})
			return core.EndpointInput{}, false
		}
		return input, true
	}

	name := core.NormalizeCollectionName(in.Collection)
	if name == "" {
		writeJSON(w, r, http.StatusUnprocessableEntity, errorBody{
			Error: "a collection endpoint has to name the collection it serves",
			Fields: core.FieldErrors{
				"collection": "Name the collection this endpoint serves.",
			},
		})
		return core.EndpointInput{}, false
	}

	collection, err := s.store.CollectionByOwnerAndName(r.Context(), user.ID, project.Slug, name)
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeJSON(w, r, http.StatusUnprocessableEntity, errorBody{
			Error: fmt.Sprintf("no collection named %q in project %q", name, project.Slug),
			Fields: core.FieldErrors{
				"collection": fmt.Sprintf("This project has no collection named %q. Create it first.", name),
			},
		})
		return core.EndpointInput{}, false
	case err != nil:
		s.apiServerError(w, r, fmt.Errorf("find collection %q: %w", name, err))
		return core.EndpointInput{}, false
	}

	input.CollectionID = collection.ID
	return input, true
}

// findAPIEndpoint resolves the {slug} and {id} of an endpoint URL. The endpoint
// has to belong to the project in the path as well as to the caller, so that an
// id from one project cannot be reached through another's URL.
func (s *Server) findAPIEndpoint(w http.ResponseWriter, r *http.Request, user core.User) (core.Project, core.Endpoint, bool) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return core.Project{}, core.Endpoint{}, false
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.apiError(w, r, http.StatusNotFound, "%q does not name an endpoint", r.PathValue("id"))
		return core.Project{}, core.Endpoint{}, false
	}

	endpoint, err := s.store.EndpointByOwnerAndID(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, core.ErrNotFound), err == nil && endpoint.ProjectID != project.ID:
		s.apiError(w, r, http.StatusNotFound, "no endpoint %s in project %q", id, project.Slug)
		return core.Project{}, core.Endpoint{}, false
	case err != nil:
		s.apiServerError(w, r, fmt.Errorf("find endpoint %s: %w", id, err))
		return core.Project{}, core.Endpoint{}, false
	}
	return project, endpoint, true
}

// apiCollectionNames maps a project's collection ids to their names, which is
// what turns a stored binding back into the name the caller used.
func (s *Server) apiCollectionNames(
	w http.ResponseWriter, r *http.Request, user core.User, project core.Project,
) (map[uuid.UUID]string, bool) {
	collections, err := s.store.CollectionsByProject(r.Context(), user.ID, project.ID)
	if err != nil {
		s.apiServerError(w, r, fmt.Errorf("list collections: %w", err))
		return nil, false
	}

	names := make(map[uuid.UUID]string, len(collections))
	for _, collection := range collections {
		names[collection.ID] = collection.Name
	}
	return names, true
}

// rejectAPIEndpoint answers the outcomes a caller can cause.
func (s *Server) rejectAPIEndpoint(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case s.apiRejected(w, r, err):
	case errors.Is(err, core.ErrEndpointExists):
		writeJSON(w, r, http.StatusConflict, errorBody{
			Error:  "this project already answers that method at that path",
			Fields: core.FieldErrors{"path": "This project already answers that method at that path."},
		})
	case errors.Is(err, core.ErrNotFound):
		s.apiError(w, r, http.StatusNotFound, "that endpoint no longer exists")
	default:
		s.apiServerError(w, r, fmt.Errorf("save endpoint: %w", err))
	}
}

func endpointAPIPath(slug string, id uuid.UUID) string {
	return pathAPIProjects + "/" + slug + "/endpoints/" + id.String()
}
