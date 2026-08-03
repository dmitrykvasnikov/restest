package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The defaults a new collection form opens with. A seeded example rather than
// an empty array, because the point of the first collection is to see a listing
// answer, and an empty one answers `[]`.
const (
	defaultCollectionName = "users"
	defaultSeed           = `[
  {"id": 1, "name": "Ada Lovelace", "email": "ada@example.com"},
  {"id": 2, "name": "Alan Turing", "email": "alan@example.com"}
]`
)

// collectionForm is the new and edit pages.
type collectionForm struct {
	Name       string
	IDField    string
	IDStrategy string
	Seed       string

	// Strategies populates the identifier selector.
	Strategies []string
	// Project is the project being edited under, for the page's links.
	Project core.Project
	// Documents is how many documents the collection holds, shown on the edit
	// page beside the reset button so that resetting says what it will discard.
	Documents int64
	// NextSerial is the identifier the next POST will be given.
	NextSerial int64

	// Action is where the form posts. DeleteAction and ResetAction are empty on
	// the new page, which has nothing yet to delete or reset.
	Action       string
	DeleteAction string
	ResetAction  string
}

func (s *Server) handleCollectionNew(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}

	data := s.newPage(r, "New collection")
	data.Form = collectionForm{
		Name:       defaultCollectionName,
		IDField:    "id",
		IDStrategy: core.IDSerial,
		Seed:       defaultSeed,
		Strategies: core.IDStrategies,
		Project:    project,
		Action:     collectionsPath(project.Slug),
	}
	s.render(w, r, http.StatusOK, "collection_form", data)
}

func (s *Server) handleCollectionCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return
	}
	form, ok := s.readCollectionForm(w, r)
	if !ok {
		return
	}
	form.Project = project
	form.Action = collectionsPath(project.Slug)

	collection, err := s.store.CreateCollection(r.Context(), user.ID, project.ID, form.toInput())
	if err != nil {
		s.rejectCollection(w, r, "New collection", form, err)
		return
	}

	s.flash(r.Context(), flashSuccess, fmt.Sprintf(
		"Collection %q created and seeded. Add an endpoint of kind “collection” to serve it.",
		collection.Name))
	redirect(w, r, projectPath(project.Slug))
}

func (s *Server) handleCollectionEdit(w http.ResponseWriter, r *http.Request, user core.User) {
	project, collection, ok := s.findCollection(w, r, user)
	if !ok {
		return
	}

	data := s.newPage(r, "Edit "+collection.Name)
	data.Form = collectionFormFrom(project, collection)
	s.render(w, r, http.StatusOK, "collection_form", data)
}

func (s *Server) handleCollectionUpdate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, collection, ok := s.findCollection(w, r, user)
	if !ok {
		return
	}
	form, ok := s.readCollectionForm(w, r)
	if !ok {
		return
	}
	form.Project = project
	form.Action = collectionPath(project.Slug, collection.ID)
	form.DeleteAction = form.Action + "/delete"
	form.ResetAction = resetPath(project.Slug, collection.Name)

	title := "Edit " + collection.Name

	updated, err := s.store.UpdateCollection(r.Context(), user.ID, collection.ID, form.toInput())
	if err != nil {
		s.rejectCollection(w, r, title, form, err)
		return
	}

	// The name is part of the reset URL but not of any mock route — the route
	// comes from the endpoint's path — so a rename does not move anything the
	// matcher serves. The table is rebuilt anyway, because an endpoint bound to
	// this collection is cheap to refresh and expensive to have left stale.
	s.reloadRoutes(r)

	s.flash(r.Context(), flashSuccess, fmt.Sprintf(
		"Collection %q saved. The documents it holds are untouched — reset to apply the new seed.",
		updated.Name))
	redirect(w, r, projectPath(project.Slug))
}

func (s *Server) handleCollectionDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	project, collection, ok := s.findCollection(w, r, user)
	if !ok {
		return
	}

	if err := s.store.DeleteCollection(r.Context(), user.ID, collection.ID); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			// Deleted in another tab between the page loading and the button
			// being pressed. The end state is the one that was asked for.
			s.flash(r.Context(), flashInfo, "That collection was already deleted.")
			redirect(w, r, projectPath(project.Slug))
			return
		}
		s.serverError(w, r, fmt.Errorf("delete collection: %w", err))
		return
	}

	// The documents went by cascade, and so did any endpoint serving them, so
	// the table has to stop offering routes that now lead nowhere.
	s.reloadRoutes(r)

	s.flash(r.Context(), flashSuccess, fmt.Sprintf(
		"Collection %q, its documents and any endpoint serving it were deleted.", collection.Name))
	redirect(w, r, projectPath(project.Slug))
}

// findCollection resolves both the {slug} and the {id} in a collection URL. The
// collection has to belong to the project in the path as well as to the caller,
// so that an id from one project cannot be edited through another's URL.
func (s *Server) findCollection(w http.ResponseWriter, r *http.Request, user core.User) (core.Project, core.Collection, bool) {
	project, ok := s.findProject(w, r, user)
	if !ok {
		return core.Project{}, core.Collection{}, false
	}

	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		s.renderMessage(w, r, http.StatusNotFound, "No such collection",
			"The address does not name a collection.")
		return core.Project{}, core.Collection{}, false
	}

	collection, err := s.store.CollectionByOwnerAndID(r.Context(), user.ID, id)
	switch {
	case errors.Is(err, core.ErrNotFound), err == nil && collection.ProjectID != project.ID:
		s.renderMessage(w, r, http.StatusNotFound, "No such collection",
			"It may have been deleted, or the address may be wrong.")
		return core.Project{}, core.Collection{}, false
	case err != nil:
		s.serverError(w, r, fmt.Errorf("find collection %s: %w", id, err))
		return core.Project{}, core.Collection{}, false
	}
	return project, collection, true
}

// readCollectionForm pulls the fields out of a submission. It fails only when
// the body itself could not be decoded; everything a user can type wrong is
// caught by core, which reports it beside the field.
func (s *Server) readCollectionForm(w http.ResponseWriter, r *http.Request) (collectionForm, bool) {
	if err := r.ParseForm(); err != nil {
		s.renderMessage(w, r, http.StatusBadRequest, "That form could not be read",
			"Go back, reload the page and try again.")
		return collectionForm{}, false
	}

	return collectionForm{
		Name:       r.PostFormValue("name"),
		IDField:    r.PostFormValue("id_field"),
		IDStrategy: r.PostFormValue("id_strategy"),
		// Browsers submit textarea newlines as CRLF. The seed goes into jsonb,
		// which would not care, but the value is echoed back into the editor on
		// a rejection and would grow a carriage return per line each time.
		Seed:       normalizeNewlines(r.PostFormValue("seed")),
		Strategies: core.IDStrategies,
	}, true
}

func (f collectionForm) toInput() core.CollectionInput {
	return core.CollectionInput{
		Name:       f.Name,
		IDField:    f.IDField,
		IDStrategy: f.IDStrategy,
		Seed:       f.Seed,
	}
}

func collectionFormFrom(project core.Project, collection core.Collection) collectionForm {
	action := collectionPath(project.Slug, collection.ID)

	return collectionForm{
		Name:         collection.Name,
		IDField:      collection.IDField,
		IDStrategy:   collection.IDStrategy,
		Seed:         collection.SeedPretty(),
		Strategies:   core.IDStrategies,
		Project:      project,
		Documents:    collection.Documents,
		NextSerial:   collection.NextSerial,
		Action:       action,
		DeleteAction: action + "/delete",
		ResetAction:  resetPath(project.Slug, collection.Name),
	}
}

// rejectCollection turns the outcomes a user can cause into messages on the
// form, and anything else into a 500.
func (s *Server) rejectCollection(w http.ResponseWriter, r *http.Request, title string, form collectionForm, err error) {
	fe, ok := fieldErrors(err)
	switch {
	case ok:
	case errors.Is(err, core.ErrCollectionExists):
		fe = core.FieldErrors{"name": "This project already has a collection with that name."}
	case errors.Is(err, core.ErrNotFound):
		s.renderMessage(w, r, http.StatusNotFound, "No such collection",
			"It may have been deleted while this page was open.")
		return
	default:
		s.serverError(w, r, fmt.Errorf("save collection: %w", err))
		return
	}

	data := s.newPage(r, title)
	data.Errors = fe
	data.Form = form
	s.render(w, r, http.StatusUnprocessableEntity, "collection_form", data)
}

// The URL shapes of this section, in one place.
func collectionsPath(slug string) string { return projectPath(slug) + "/collections" }

func collectionPath(slug string, id uuid.UUID) string {
	return collectionsPath(slug) + "/" + id.String()
}
