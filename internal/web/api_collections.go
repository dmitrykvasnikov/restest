package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// apiCollection is a collection as the API renders it: the definition, plus how
// much is currently in it.
//
// Addressed by name, not by id. The name is what appears in the reset URL and
// in an endpoint's binding, so it is what a script already has written down.
type apiCollection struct {
	Name       string `json:"name"`
	IDField    string `json:"id_field"`
	IDStrategy string `json:"id_strategy"`
	// NextSerial is the identifier the next POST to this collection will be
	// given. Meaningless under the uuid strategy, and reported anyway, because
	// it is what the column says.
	NextSerial int64 `json:"next_serial"`
	// Documents is how many the collection holds now — which is not how many
	// the seed holds, and the difference is the whole point of a reset.
	Documents int64 `json:"documents"`
	// Seed is embedded as JSON rather than as a string of JSON, so a script can
	// build one with its own encoder instead of escaping quotes into a string.
	Seed      json.RawMessage `json:"seed"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func apiCollectionOf(c core.Collection) apiCollection {
	seed := c.Seed
	if len(seed) == 0 {
		seed = json.RawMessage("[]")
	}

	return apiCollection{
		Name:       c.Name,
		IDField:    c.IDField,
		IDStrategy: c.IDStrategy,
		NextSerial: c.NextSerial,
		Documents:  c.Documents,
		Seed:       seed,
		CreatedAt:  c.CreatedAt.UTC(),
		UpdatedAt:  c.UpdatedAt.UTC(),
	}
}

// collectionCreateRequest is the body of POST. Only the name is required: a
// collection with no seed is an empty one, which is a legitimate thing to
// create and then POST into.
type collectionCreateRequest struct {
	Name       string          `json:"name"`
	IDField    string          `json:"id_field"`
	IDStrategy string          `json:"id_strategy"`
	Seed       json.RawMessage `json:"seed"`
}

// collectionUpdateRequest is the body of PATCH, with an absent field meaning
// "leave it alone".
//
// PATCH rather than PUT because a collection's fields are independent of one
// another: changing the seed says nothing about the identifier strategy, and a
// script that wants to replace only the fixture should not have to restate the
// rest of the definition to avoid resetting it. An endpoint is the other case,
// and takes PUT — see api_endpoints.go.
//
// Seed is a json.RawMessage and is nil exactly when the field was absent; an
// explicit `null` arrives as the four bytes and is refused, because a
// collection's seed is an array.
type collectionUpdateRequest struct {
	Name       *string         `json:"name"`
	IDField    *string         `json:"id_field"`
	IDStrategy *string         `json:"id_strategy"`
	Seed       json.RawMessage `json:"seed"`
}

func (s *Server) handleAPICollectionList(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	collections, err := s.store.CollectionsByProject(r.Context(), user.ID, project.ID)
	if err != nil {
		s.apiServerError(w, r, fmt.Errorf("list collections: %w", err))
		return
	}

	items := make([]apiCollection, len(collections))
	for i, collection := range collections {
		items[i] = apiCollectionOf(collection)
	}
	writeJSON(w, r, http.StatusOK, apiListOf(items))
}

func (s *Server) handleAPICollectionCreate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return
	}

	var in collectionCreateRequest
	if !s.decodeJSON(w, r, &in) {
		return
	}

	collection, err := s.store.CreateCollection(r.Context(), user.ID, project.ID, core.CollectionInput{
		Name:       in.Name,
		IDField:    in.IDField,
		IDStrategy: in.IDStrategy,
		Seed:       string(in.Seed),
	})
	if err != nil {
		s.rejectAPICollection(w, r, err)
		return
	}

	w.Header().Set("Location", collectionAPIPath(project.Slug, collection.Name))
	s.writeAPICollection(w, r, user, project, collection, http.StatusCreated)
}

func (s *Server) handleAPICollectionShow(w http.ResponseWriter, r *http.Request, user core.User) {
	project, collection, ok := s.findAPICollection(w, r, user)
	if !ok {
		return
	}
	s.writeAPICollection(w, r, user, project, collection, http.StatusOK)
}

func (s *Server) handleAPICollectionUpdate(w http.ResponseWriter, r *http.Request, user core.User) {
	project, collection, ok := s.findAPICollection(w, r, user)
	if !ok {
		return
	}

	var in collectionUpdateRequest
	if !s.decodeJSON(w, r, &in) {
		return
	}

	input := core.CollectionInput{
		Name:       collection.Name,
		IDField:    collection.IDField,
		IDStrategy: collection.IDStrategy,
		Seed:       string(collection.Seed),
	}
	if in.Name != nil {
		input.Name = *in.Name
	}
	if in.IDField != nil {
		input.IDField = *in.IDField
	}
	if in.IDStrategy != nil {
		input.IDStrategy = *in.IDStrategy
	}
	if in.Seed != nil {
		input.Seed = string(in.Seed)
	}

	updated, err := s.store.UpdateCollection(r.Context(), user.ID, collection.ID, input)
	if err != nil {
		s.rejectAPICollection(w, r, err)
		return
	}

	// Saving does not apply the seed — that is what reset is for (DESIGN.md
	// §5) — but an endpoint bound to this collection is cheap to refresh and
	// expensive to have left stale.
	s.reloadRoutes(r)

	s.writeAPICollection(w, r, user, project, updated, http.StatusOK)
}

func (s *Server) handleAPICollectionDelete(w http.ResponseWriter, r *http.Request, user core.User) {
	_, collection, ok := s.findAPICollection(w, r, user)
	if !ok {
		return
	}

	if err := s.store.DeleteCollection(r.Context(), user.ID, collection.ID); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.apiServerError(w, r, fmt.Errorf("delete collection: %w", err))
		return
	}

	// The documents went by cascade, and so did any endpoint serving them, so
	// the table has to stop offering routes that now lead nowhere.
	s.reloadRoutes(r)

	w.WriteHeader(http.StatusNoContent)
}

// resetResult is what a reset answers a programmatic caller with. The count is
// the point: a test suite that resets between runs wants to see that it got the
// fixture it expected rather than an empty collection.
type resetResult struct {
	Project    string `json:"project"`
	Collection string `json:"collection"`
	Documents  int    `json:"documents"`
}

// handleCollectionReset restores a collection to its seed.
//
// One handler answers both callers. A browser reaches it through the button on
// the collection page and is sent back where it came from with a message; a
// programmatic caller — a CI job holding a token, which is what this route was
// always for — gets JSON. Two routes doing the same thing would be two places
// for the ownership check to be got wrong.
func (s *Server) handleCollectionReset(w http.ResponseWriter, r *http.Request, user core.User) {
	slug := projectSlugOf(r)
	name := core.NormalizeCollectionName(r.PathValue("name"))

	collection, err := s.store.CollectionByOwnerAndName(r.Context(), user.ID, slug, name)
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.rejectReset(w, r, slug, http.StatusNotFound,
			fmt.Sprintf("no collection named %q in project %q", name, slug))
		return
	case err != nil:
		s.serverErrorFor(w, r, fmt.Errorf("find collection %q: %w", name, err))
		return
	}

	count, err := s.store.ResetCollection(r.Context(), collection.ID)
	if err != nil {
		if fe, ok := fieldErrors(err); ok {
			// The seed was edited into something that cannot be applied — an
			// entry with a duplicate id, most likely. It was accepted as valid
			// JSON when it was saved; this is where it stops being applicable.
			s.rejectReset(w, r, slug, http.StatusUnprocessableEntity, fe.Error())
			return
		}
		s.serverErrorFor(w, r, fmt.Errorf("reset collection %q: %w", name, err))
		return
	}

	if wantsHTML(r) {
		s.flash(r.Context(), flashSuccess, fmt.Sprintf(
			"Collection %q reset to its seed: %s.", collection.Name, documentCount(count)))
		redirect(w, r, projectPath(slug))
		return
	}
	writeJSON(w, r, http.StatusOK, resetResult{
		Project:    slug,
		Collection: collection.Name,
		Documents:  count,
	})
}

// rejectReset answers a refused reset in whichever form the caller asked in.
func (s *Server) rejectReset(w http.ResponseWriter, r *http.Request, slug string, status int, message string) {
	if wantsHTML(r) {
		s.flash(r.Context(), flashError, message)
		redirect(w, r, projectPath(slug))
		return
	}
	writeJSON(w, r, status, errorBody{Error: message})
}

// wantsHTML reports whether the caller is the browser UI rather than a script.
//
// It asks whether the request came from HTMX, not what it will accept: the
// button on the collection page is the only browser caller, and it is an HTMX
// post. Anything else is treated as programmatic and gets JSON, which is the
// safer default for a route whose whole purpose is to be called by a test
// suite — and a request carrying a bearer token is never the button, because
// the page does not have one.
func wantsHTML(r *http.Request) bool {
	if _, ok := bearerToken(r); ok {
		return false
	}
	return isHTMX(r)
}

func documentCount(n int) string {
	if n == 1 {
		return "1 document"
	}
	return fmt.Sprintf("%d documents", n)
}

// findAPICollection resolves the {slug} and {name} of a collection URL in one
// statement, so that a name in another account's project is the same 404 as a
// name that was never used.
//
// It answers with the project as well, because every caller needs both.
func (s *Server) findAPICollection(w http.ResponseWriter, r *http.Request, user core.User) (core.Project, core.Collection, bool) {
	project, ok := s.findAPIProject(w, r, user)
	if !ok {
		return core.Project{}, core.Collection{}, false
	}

	name := core.NormalizeCollectionName(r.PathValue("name"))
	collection, err := s.store.CollectionByOwnerAndName(r.Context(), user.ID, project.Slug, name)
	switch {
	case errors.Is(err, core.ErrNotFound):
		s.apiError(w, r, http.StatusNotFound, "no collection named %q in project %q", name, project.Slug)
		return core.Project{}, core.Collection{}, false
	case err != nil:
		s.apiServerError(w, r, fmt.Errorf("find collection %q: %w", name, err))
		return core.Project{}, core.Collection{}, false
	}
	return project, collection, true
}

// writeAPICollection answers with one collection, read back through the listing
// statement.
//
// The re-read is not ceremony. The single-collection statements carry no
// document count, and creating a collection applies its seed *after* the row
// comes back — so the row a write hands over is already out of date about both
// how much it holds and what the next identifier will be. Asking again costs
// one query on a route that is on no hot path, and it is what stops the API
// reporting a freshly seeded collection as empty.
func (s *Server) writeAPICollection(
	w http.ResponseWriter, r *http.Request,
	user core.User, project core.Project, collection core.Collection, status int,
) {
	collections, err := s.store.CollectionsByProject(r.Context(), user.ID, project.ID)
	if err != nil {
		s.apiServerError(w, r, fmt.Errorf("re-read collection %q: %w", collection.Name, err))
		return
	}
	for _, candidate := range collections {
		if candidate.ID == collection.ID {
			collection = candidate
			break
		}
	}

	writeJSON(w, r, status, apiCollectionOf(collection))
}

// rejectAPICollection answers the outcomes a caller can cause.
func (s *Server) rejectAPICollection(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case s.apiRejected(w, r, err):
	case errors.Is(err, core.ErrCollectionExists):
		writeJSON(w, r, http.StatusConflict, errorBody{
			Error:  "this project already has a collection with that name",
			Fields: core.FieldErrors{"name": "This project already has a collection with that name."},
		})
	case errors.Is(err, core.ErrNotFound):
		s.apiError(w, r, http.StatusNotFound, "that collection no longer exists")
	default:
		s.apiServerError(w, r, fmt.Errorf("save collection: %w", err))
	}
}

// The API's URL shapes, in one place. resetPath is what the interface links to,
// so the address shown on the collection page is the address a script uses.
func collectionAPIPath(slug, name string) string {
	return pathAPIProjects + "/" + slug + "/collections/" + name
}

func resetPath(slug, name string) string { return collectionAPIPath(slug, name) + "/reset" }
