package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// projectForm carries what the new and edit pages need: the values to put back
// in the fields, and enough to know where the form posts to.
type projectForm struct {
	Slug string
	Name string
	// Action is the URL the form submits to — empty on the create page, which
	// posts to /projects.
	Action string
	// Existing is the slug the project had when the page was rendered, so a
	// rejected rename still knows which project it was editing.
	Existing string
}

// projectList is the data behind the list page.
type projectList struct {
	Projects []core.Project
}

func (s *Server) handleProjectList(w http.ResponseWriter, r *http.Request, user core.User) {
	projects, err := s.store.ProjectsByOwner(r.Context(), user.ID)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("list projects: %w", err))
		return
	}

	data := s.newPage(r, "Projects")
	data.Form = projectList{Projects: projects}
	s.render(w, r, http.StatusOK, "project_list", data)
}

func (s *Server) handleProjectNew(w http.ResponseWriter, r *http.Request, _ core.User) {
	data := s.newPage(r, "New project")
	data.Form = projectForm{Action: "/projects"}
	s.render(w, r, http.StatusOK, "project_form", data)
}

func (s *Server) handleProjectCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return
	}

	slug := r.PostFormValue("slug")
	name := r.PostFormValue("name")

	project, err := s.store.CreateProject(r.Context(), user.ID, slug, name)
	if err != nil {
		s.rejectProject(w, r, "New project", projectForm{
			Slug:   slug,
			Name:   name,
			Action: "/projects",
		}, err)
		return
	}

	// The route table is keyed by slug, and it has to know the project exists
	// before it can answer "nothing is defined here" rather than "no such
	// project" — which is why a project with no endpoints still triggers a
	// rebuild.
	s.reloadRoutes(r)

	s.flash(r.Context(), flashSuccess, fmt.Sprintf("Project %q created.", project.Slug))
	redirect(w, r, projectPath(project.Slug))
}

func (s *Server) handleProjectShow(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	endpoints, err := s.store.EndpointsByProject(r.Context(), user.ID, project.ID)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("list endpoints: %w", err))
		return
	}
	collections, err := s.store.CollectionsByProject(r.Context(), user.ID, project.ID)
	if err != nil {
		s.serverError(w, r, fmt.Errorf("list collections: %w", err))
		return
	}

	names := make(map[string]string, len(collections))
	rows := make([]collectionRow, len(collections))
	for i, collection := range collections {
		names[collection.ID.String()] = collection.Name
		rows[i] = collectionRow{
			Collection: collection,
			EditPath:   collectionPath(project.Slug, collection.ID) + "/edit",
			ResetPath:  resetPath(project.Slug, collection.Name),
		}
	}

	data := s.newPage(r, project.Name)
	data.Form = projectShow{
		Project:         project,
		Endpoints:       endpoints,
		Collections:     rows,
		CollectionNames: names,
	}
	s.render(w, r, http.StatusOK, "project_show", data)
}

func (s *Server) handleProjectEdit(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	data := s.newPage(r, "Edit "+project.Name)
	data.Form = projectForm{
		Slug:     project.Slug,
		Name:     project.Name,
		Action:   projectPath(project.Slug),
		Existing: project.Slug,
	}
	s.render(w, r, http.StatusOK, "project_form", data)
}

func (s *Server) handleProjectUpdate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return
	}

	slug := r.PostFormValue("slug")
	name := r.PostFormValue("name")

	updated, err := s.store.UpdateProject(r.Context(), user.ID, project.ID, slug, name)
	if err != nil {
		s.rejectProject(w, r, "Edit "+project.Name, projectForm{
			Slug:     slug,
			Name:     name,
			Action:   projectPath(project.Slug),
			Existing: project.Slug,
		}, err)
		return
	}

	// A renamed slug moves every one of this project's routes, so the table has
	// to be rebuilt before the new URL answers and the old one stops.
	s.reloadRoutes(r)

	if updated.Slug != project.Slug {
		s.flash(r.Context(), flashInfo,
			fmt.Sprintf("The mock URL is now %s%s — anything pointed at the old one will stop matching.",
				s.baseURL, updated.MockPath()))
	}
	s.flash(r.Context(), flashSuccess, "Project saved.")
	redirect(w, r, projectPath(updated.Slug))
}

func (s *Server) handleProjectDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	if err := s.store.DeleteProject(r.Context(), user.ID, project.ID); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// Deleted in another tab between the page loading and the button
			// being pressed. The end state is the one that was asked for.
			s.flash(r.Context(), flashInfo, "That project was already deleted.")
			redirect(w, r, "/projects")
			return
		}
		s.serverError(w, r, fmt.Errorf("delete project: %w", err))
		return
	}

	// The endpoints went with it through `on delete cascade`, so the table has
	// to stop serving them.
	s.reloadRoutes(r)

	s.flash(r.Context(), flashSuccess,
		fmt.Sprintf("Project %q and everything in it was deleted.", project.Slug))
	redirect(w, r, "/projects")
}

// findProject resolves the {slug} in the route to a project the caller owns,
// answering 404 when there is none. A project belonging to somebody else is the
// same 404 as one that does not exist.
func (s *Server) findProject(w http.ResponseWriter, r *http.Request, user core.User) (core.Project, bool) {
	slug := strings.ToLower(r.PathValue("slug"))

	project, err := s.store.ProjectByOwnerAndSlug(r.Context(), user.ID, slug)
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.renderMessage(w, r, http.StatusNotFound, "No such project",
			"It may have been deleted, or the address may be wrong.")
		return core.Project{}, false
	case err != nil:
		s.serverError(w, r, fmt.Errorf("find project %q: %w", slug, err))
		return core.Project{}, false
	}
	return project, true
}

// rejectProject re-renders the project form with the messages attached, turning
// the two outcomes a user can actually cause — invalid input and a slug someone
// else already has — into field errors, and anything else into a 500.
func (s *Server) rejectProject(w http.ResponseWriter, r *http.Request, title string, form projectForm, err error) {
	fe, ok := fieldErrors(err)
	switch {
	case ok:
	case errors.Is(err, core.ErrSlugTaken):
		// Slugs are global because they address mock traffic, so this can be a
		// collision with a project the user cannot see. Saying "taken" is the
		// most that can be said.
		fe = core.FieldErrors{"slug": "That slug is taken. Pick another one."}
	case errors.Is(err, core.ErrNotFound):
		s.renderMessage(w, r, http.StatusNotFound, "No such project",
			"It may have been deleted while this page was open.")
		return
	default:
		s.serverError(w, r, fmt.Errorf("save project: %w", err))
		return
	}

	data := s.newPage(r, title)
	data.Errors = fe
	data.Form = form
	s.render(w, r, http.StatusUnprocessableEntity, "project_form", data)
}
