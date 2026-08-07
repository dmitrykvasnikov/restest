package web

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// apiProject is a project as the API renders it.
//
// There is no id. A project is addressed by its slug everywhere else — in the
// mock URL, in the interface, in the reset route — and offering a second
// identifier would invite scripts to hold the one that is harder to read and
// impossible to guess from a mock URL.
type apiProject struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	// MockURL is where this project's mock traffic is served, absolute, so a
	// script can hand it straight to whatever it is about to test.
	MockURL   string    `json:"mock_url"`
	IsDemo    bool      `json:"is_demo"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) apiProjectOf(p core.Project) apiProject {
	return apiProject{
		Slug:      p.Slug,
		Name:      p.Name,
		MockURL:   s.baseURL + p.MockPath(),
		IsDemo:    p.IsDemo,
		CreatedAt: p.CreatedAt.UTC(),
		UpdatedAt: p.UpdatedAt.UTC(),
	}
}

// projectCreateRequest is the body of POST /api/v1/projects. Datasets are the
// built-in seeds to create it with, by name, which is the same choice the form
// offers as checkboxes (DESIGN.md §6.1).
type projectCreateRequest struct {
	Slug     string   `json:"slug"`
	Name     string   `json:"name"`
	Datasets []string `json:"datasets"`
}

// projectUpdateRequest is the body of PATCH. The fields are pointers because
// the verb is PATCH: an absent field keeps what the project has, and there is
// no way to express that with a string whose zero value is also a rejection.
type projectUpdateRequest struct {
	Slug *string `json:"slug"`
	Name *string `json:"name"`
}

// projectList is what a listing answers. An object rather than a bare array, so
// that a count — or anything else worth adding later — has somewhere to go
// without changing the shape of every response.
type apiList[T any] struct {
	Count int `json:"count"`
	Items []T `json:"items"`
}

func apiListOf[T any](items []T) apiList[T] {
	if items == nil {
		items = []T{}
	}
	return apiList[T]{Count: len(items), Items: items}
}

func (s *Server) handleAPIProjectList(w http.ResponseWriter, r *http.Request, user core.User) {
	projects, err := s.store.ProjectsByOwner(r.Context(), user.ID)
	if err != nil {
		s.apiServerError(w, r, fmt.Errorf("list projects: %w", err))
		return
	}

	items := make([]apiProject, len(projects))
	for i, project := range projects {
		items[i] = s.apiProjectOf(project)
	}
	writeJSON(w, r, http.StatusOK, apiListOf(items))
}

func (s *Server) handleAPIProjectCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	var in projectCreateRequest
	if !s.decodeJSON(w, r, &in) {
		return
	}

	project, err := s.store.CreateProject(r.Context(), user.ID, in.Slug, in.Name, in.Datasets)
	if err != nil {
		s.rejectAPIProject(w, r, err)
		return
	}

	// The route table is keyed by slug and has to know the project exists
	// before /m/{slug}/ can answer "nothing is defined here" rather than "no
	// such project" — which is why a project with no endpoints still triggers a
	// rebuild.
	s.reloadRoutes(r)

	w.Header().Set("Location", pathAPIProjects+"/"+project.Slug)
	writeJSON(w, r, http.StatusCreated, s.apiProjectOf(project))
}

func (s *Server) handleAPIProjectShow(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}
	writeJSON(w, r, http.StatusOK, s.apiProjectOf(project))
}

func (s *Server) handleAPIProjectUpdate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	var in projectUpdateRequest
	if !s.decodeJSON(w, r, &in) {
		return
	}

	slug, name := project.Slug, project.Name
	if in.Slug != nil {
		slug = *in.Slug
	}
	if in.Name != nil {
		name = *in.Name
	}

	updated, err := s.store.UpdateProject(r.Context(), user.ID, project.ID, slug, name)
	if err != nil {
		s.rejectAPIProject(w, r, err)
		return
	}

	// A renamed slug moves every one of this project's routes, so the table has
	// to be rebuilt before the new URL answers and the old one stops.
	s.reloadRoutes(r)

	writeJSON(w, r, http.StatusOK, s.apiProjectOf(updated))
}

func (s *Server) handleAPIProjectDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	if err := s.store.DeleteProject(r.Context(), user.ID, project.ID); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// Deleted between the lookup and here. The end state is the one that
			// was asked for.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.apiServerError(w, r, fmt.Errorf("delete project: %w", err))
		return
	}

	// The endpoints and collections went with it through `on delete cascade`,
	// so the table has to stop serving them.
	s.reloadRoutes(r)

	w.WriteHeader(http.StatusNoContent)
}

// rejectAPIProject answers the outcomes a caller can cause, and sends anything
// else to the log as a 500.
func (s *Server) rejectAPIProject(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case s.apiRejected(w, r, err):
	case errors.Is(err, core.ErrSlugTaken):
		// Slugs are global, because they address mock traffic, so this can be a
		// collision with a project the caller cannot see. "Taken" is the most
		// that can be said.
		writeJSON(w, r, http.StatusConflict, errorBody{
			Error:  "that slug is taken",
			Fields: core.FieldErrors{"slug": "That slug is taken. Pick another one."},
		})
	case errors.Is(err, core.ErrNotFound):
		s.apiError(w, r, http.StatusNotFound, "that project no longer exists")
	default:
		s.apiServerError(w, r, fmt.Errorf("save project: %w", err))
	}
}
