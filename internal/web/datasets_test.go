package web

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The built-in datasets are what a new project can be created pre-seeded from
// (DESIGN.md §6), so the form has to offer every one of them by the name the
// store resolves — a checkbox whose value the store does not recognise would be
// a 422 with no way out of it.
func TestProjectFormOffersEveryDataset(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	_, body := b.get("/projects/new")
	for _, d := range core.Datasets() {
		if !strings.Contains(body, `name="datasets" value="`+d.Name+`"`) {
			t.Errorf("the %s dataset has no checkbox:\n%s", d.Name, body)
		}
		if !strings.Contains(body, d.Summary) {
			t.Errorf("the %s dataset is offered without saying what is in it", d.Name)
		}
	}
}

// The edit form does not offer them. A dataset is something a project is
// created with; adding one to a project that exists is creating a collection,
// which has its own form and would not be what a checkbox implied.
func TestProjectEditFormDoesNotOfferDatasets(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	_, body := b.get("/projects/checkout/edit")
	if strings.Contains(body, `name="datasets"`) {
		t.Errorf("the edit form offers datasets:\n%s", body)
	}
}

func TestProjectCreatePassesTheChosenDatasets(t *testing.T) {
	var got []string

	store := projectStore()
	store.createProject = func(_ context.Context, _ uuid.UUID, slug, name string, datasets []string) (core.Project, error) {
		got = datasets
		return testProject(slug, name), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/new", "/projects", url.Values{
		"slug":     {"checkout"},
		"name":     {"Checkout API"},
		"datasets": {"users", "todos"},
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 after the redirect:\n%s", resp.StatusCode, body)
	}
	if !slices.Equal(got, []string{"users", "todos"}) {
		t.Errorf("the store was given %v, want [users todos]", got)
	}
	// The point of the datasets is that the project answers immediately, and
	// the only place that says so is the flash.
	if !strings.Contains(body, "http://restest.test/m/checkout/users") {
		t.Errorf("nothing on the page says where the datasets are being served:\n%s", body)
	}
}

// Creating an empty project is still the default. An unticked checkbox sends
// nothing, so the field is absent rather than empty.
func TestProjectCreateWithoutDatasetsSendsNone(t *testing.T) {
	got := []string{"not called"}

	store := projectStore()
	store.createProject = func(_ context.Context, _ uuid.UUID, slug, name string, datasets []string) (core.Project, error) {
		got = datasets
		return testProject(slug, name), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	b.post("/projects/new", "/projects", url.Values{
		"slug": {"checkout"},
		"name": {"Checkout API"},
	})

	if len(got) != 0 {
		t.Errorf("the store was given %v, want nothing", got)
	}
}

// A rejected form comes back with the boxes as they were left. Retyping the
// name is one thing; re-choosing the datasets after a slug collision would be
// the form losing work it was already holding.
func TestRejectedProjectFormKeepsTheTickedDatasets(t *testing.T) {
	store := projectStore()
	store.createProject = func(context.Context, uuid.UUID, string, string, []string) (core.Project, error) {
		return core.Project{}, core.ErrSlugTaken
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/new", "/projects", url.Values{
		"slug":     {"checkout"},
		"name":     {"Checkout API"},
		"datasets": {"posts"},
	})

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}

	posts := `name="datasets" value="posts"`
	users := `name="datasets" value="users"`
	if !strings.Contains(body, posts) {
		t.Fatalf("the posts checkbox is missing entirely:\n%s", body)
	}
	if !checkedAfter(body, posts) {
		t.Errorf("the ticked dataset came back unticked:\n%s", body)
	}
	if checkedAfter(body, users) {
		t.Error("a dataset that was not ticked came back ticked")
	}
}

// checkedAfter reports whether the input tag beginning at marker carries
// `checked`, by looking only as far as the end of that tag.
func checkedAfter(body, marker string) bool {
	i := strings.Index(body, marker)
	if i < 0 {
		return false
	}
	tag := body[i:]
	if end := strings.Index(tag, ">"); end >= 0 {
		tag = tag[:end]
	}
	return strings.Contains(tag, "checked")
}

// The demo project is the answer to "can I try this without registering", so
// the login page — the first page an anonymous visitor sees — is where it is
// offered, with a command that can be pasted rather than an invitation.
func TestLoginPageOffersTheDemoWhenItIsEnabled(t *testing.T) {
	b := newBrowser(t, accountStore())
	_, body := b.get("/login")
	if strings.Contains(body, "/m/"+core.DemoSlug) {
		t.Fatalf("the demo is offered by a server that does not serve one:\n%s", body)
	}

	b = newBrowserWith(t, accountStore(), func(o *Options) { o.DemoEnabled = true })
	_, body = b.get("/login")

	if !strings.Contains(body, "curl http://restest.test/m/demo/users") {
		t.Errorf("the login page does not show how to reach the demo:\n%s", body)
	}
	for _, d := range core.Datasets() {
		if !strings.Contains(body, ">"+d.Name+"<") {
			t.Errorf("the demo is offered without mentioning %s", d.Name)
		}
	}
}
