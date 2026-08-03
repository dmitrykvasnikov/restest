package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// projectStore is accountStore plus a project the account owns.
func projectStore() stubStore {
	store := accountStore()
	store.projectsByOwner = func(context.Context, uuid.UUID) ([]core.Project, error) {
		return []core.Project{testProject("checkout", "Checkout API")}, nil
	}
	store.projectByOwnerAndSlug = func(_ context.Context, _ uuid.UUID, slug string) (core.Project, error) {
		if slug != "checkout" {
			return core.Project{}, core.ErrNotFound
		}
		return testProject("checkout", "Checkout API"), nil
	}
	return store
}

func TestProjectListShowsProjectsAndTheirMockURLs(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	_, body := b.get("/projects")
	if !strings.Contains(body, "Checkout API") {
		t.Errorf("the project is not listed:\n%s", body)
	}
	// The mock URL is the thing a user came to copy, so it is on the page.
	if !strings.Contains(body, "http://restest.test/m/checkout/") {
		t.Error("the project's mock URL is not shown")
	}
}

func TestProjectListIsEmptyForANewAccount(t *testing.T) {
	b := newBrowser(t, accountStore())
	logIn(t, b)

	_, body := b.get("/projects")
	if !strings.Contains(body, "No projects yet") {
		t.Errorf("no empty state on the list page:\n%s", body)
	}
}

func TestProjectCreate(t *testing.T) {
	var gotSlug, gotName string
	var gotOwner uuid.UUID

	store := projectStore()
	store.createProject = func(_ context.Context, owner uuid.UUID, slug, name string) (core.Project, error) {
		gotOwner, gotSlug, gotName = owner, slug, name
		return testProject(slug, name), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/new", "/projects", url.Values{
		"slug": {"checkout"},
		"name": {"Checkout API"},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if gotOwner != testUser.ID {
		t.Errorf("owner = %v, want the logged-in account", gotOwner)
	}
	if gotSlug != "checkout" || gotName != "Checkout API" {
		t.Errorf("store saw slug %q and name %q", gotSlug, gotName)
	}
	// It lands on the new project, not back on the list.
	if got := resp.Request.URL.Path; got != "/projects/checkout" {
		t.Errorf("landed on %q, want /projects/checkout", got)
	}
	if !strings.Contains(body, "created") {
		t.Error("no confirmation flash after creating a project")
	}
}

func TestProjectCreateRejectsAnInvalidSlug(t *testing.T) {
	store := projectStore()
	store.createProject = func(context.Context, uuid.UUID, string, string) (core.Project, error) {
		return core.Project{}, core.FieldErrors{"slug": "That slug is reserved. Pick another one."}
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/new", "/projects", url.Values{
		"slug": {"admin"},
		"name": {"Admin"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "That slug is reserved") {
		t.Errorf("the field message is not on the page:\n%s", body)
	}
	// The form comes back filled in, so the fix is one edit rather than a retype.
	if !strings.Contains(body, `value="admin"`) || !strings.Contains(body, `value="Admin"`) {
		t.Error("the rejected form was not returned with its values")
	}
}

func TestProjectCreateReportsATakenSlug(t *testing.T) {
	store := projectStore()
	store.createProject = func(context.Context, uuid.UUID, string, string) (core.Project, error) {
		return core.Project{}, core.ErrSlugTaken
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/new", "/projects", url.Values{
		"slug": {"checkout"},
		"name": {"Checkout"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "That slug is taken") {
		t.Errorf("the page does not say the slug is taken:\n%s", body)
	}
}

// A project somebody else owns is the same answer as one that does not exist.
// Anything else would report on another account's projects.
func TestProjectOfAnotherOwnerIsNotFound(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	resp, body := b.get("/projects/somebody-elses")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if !strings.Contains(body, "No such project") {
		t.Errorf("unexpected page:\n%s", body)
	}
}

// Renaming the slug moves the project's mock URL. The user is allowed to do it
// and told what it costs.
func TestProjectUpdateWarnsWhenTheSlugMoves(t *testing.T) {
	store := projectStore()
	store.updateProject = func(_ context.Context, _, _ uuid.UUID, slug, name string) (core.Project, error) {
		return testProject(slug, name), nil
	}
	// The project answers to its new slug once it has moved, so the redirect
	// after the update lands somewhere.
	store.projectByOwnerAndSlug = func(_ context.Context, _ uuid.UUID, slug string) (core.Project, error) {
		return testProject(slug, "Checkout API"), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/checkout/edit", "/projects/checkout", url.Values{
		"slug": {"payments"},
		"name": {"Payments API"},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if got := resp.Request.URL.Path; got != "/projects/payments" {
		t.Errorf("landed on %q, want the project's new address", got)
	}
	if !strings.Contains(body, "stop matching") {
		t.Errorf("no warning that the old mock URL is dead:\n%s", body)
	}
}

func TestProjectUpdateKeepsQuietWhenTheSlugIsUnchanged(t *testing.T) {
	store := projectStore()
	store.updateProject = func(_ context.Context, _, _ uuid.UUID, slug, name string) (core.Project, error) {
		return testProject(slug, name), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	_, body := b.post("/projects/checkout/edit", "/projects/checkout", url.Values{
		"slug": {"checkout"},
		"name": {"Checkout API v2"},
	})

	if strings.Contains(body, "stop matching") {
		t.Error("warned about a moved URL when only the name changed")
	}
}

func TestProjectDelete(t *testing.T) {
	var deletedID uuid.UUID

	store := projectStore()
	store.deleteProject = func(_ context.Context, _, id uuid.UUID) error {
		deletedID = id
		return nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/checkout", "/projects/checkout/delete", url.Values{})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if deletedID != testProjectID {
		t.Errorf("deleted %v, want the project on the page", deletedID)
	}
	if got := resp.Request.URL.Path; got != "/projects" {
		t.Errorf("landed on %q, want the project list", got)
	}
	if !strings.Contains(body, "deleted") {
		t.Error("no confirmation flash after deleting")
	}
}

// HTMX does the request over XHR, where a 303 would be followed transparently
// and the resulting page swapped into a fragment. It is told to navigate.
func TestProjectDeleteAnswersHTMXWithARedirectHeader(t *testing.T) {
	store := projectStore()
	store.deleteProject = func(context.Context, uuid.UUID, uuid.UUID) error { return nil }

	b := newBrowser(t, store)
	logIn(t, b)
	b.noFollow()

	_, page := b.get("/projects/checkout")

	headers := b.sameOriginHeaders("/projects/checkout")
	headers.Set("HX-Request", "true")
	headers.Set("X-CSRF-Token", csrfToken(t, page))

	resp, body := b.postRaw("/projects/checkout/delete", url.Values{}, headers)

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204:\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("HX-Redirect"); got != "/projects" {
		t.Errorf("HX-Redirect = %q, want /projects", got)
	}
	if got := resp.Header.Get("Location"); got != "" {
		t.Errorf("Location = %q, want it left to HTMX", got)
	}
}

// Deleting something already deleted in another tab ends where it was going.
func TestProjectDeleteIsIdempotentEnough(t *testing.T) {
	store := projectStore()
	store.deleteProject = func(context.Context, uuid.UUID, uuid.UUID) error {
		return core.ErrNotFound
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/checkout", "/projects/checkout/delete", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect", resp.StatusCode)
	}
	if !strings.Contains(body, "already deleted") {
		t.Errorf("no explanation of what happened:\n%s", body)
	}
}
