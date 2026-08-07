//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/web"
)

// A project created from the built-in datasets answers immediately: the
// collections hold their seeds, the endpoints serving them exist, and the route
// table has been rebuilt. This is the half of M5 that a logged-in user sees.
func TestProjectCreatedWithDatasetsServesThemAtOnce(t *testing.T) {
	s := newSite(t)

	if resp, body := s.submit("/register", "/register", formValues(map[string]string{
		"email": "sam@example.com", "password": testPassword,
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d\n%s", resp.StatusCode, body)
	}

	values := formValues(map[string]string{"slug": "checkout", "name": "Checkout API"})
	values["datasets"] = []string{"users", "todos"}
	if resp, body := s.submit("/projects/new", "/projects", values); resp.StatusCode != http.StatusOK {
		t.Fatalf("create project: status = %d\n%s", resp.StatusCode, body)
	}

	// --- the collections and their endpoints are there --------------------
	if n := s.count(`select count(*) from collections`); n != 2 {
		t.Errorf("%d collections, want 2", n)
	}
	if n := s.count(`select count(*) from endpoints where kind = 'collection'`); n != 2 {
		t.Errorf("%d collection endpoints, want 2", n)
	}
	// The seed was applied at creation, not left for a reset to apply.
	if n := s.count(`select count(*) from documents`); n == 0 {
		t.Fatal("the datasets were installed with no documents")
	}

	// --- and they answer, without another edit or a restart ---------------
	users := datasetOf(t, "users")
	resp, body := s.get("/m/checkout/users")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET the users dataset: status = %d\n%s", resp.StatusCode, body)
	}
	if got := len(decodeArray(t, body)); got != users.Documents {
		t.Errorf("the users dataset returned %d documents, want %d", got, users.Documents)
	}
	if got := resp.Header.Get("X-Total-Count"); got != strconv.Itoa(users.Documents) {
		t.Errorf("X-Total-Count = %q, want %d", got, users.Documents)
	}

	// The identifiers the seed states are the ones in the URLs, which is the
	// whole reason a fixture states them.
	if resp, body := s.get("/m/checkout/users/1"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /users/1: status = %d\n%s", resp.StatusCode, body)
	} else if !strings.Contains(body, "Ada Lovelace") {
		t.Errorf("/users/1 is not the first seeded user:\n%s", body)
	}

	// Filtering works, because the documents went in as jsonb like any others.
	if _, body := s.get("/m/checkout/users?role=admin"); len(decodeArray(t, body)) == 0 {
		t.Errorf("filtering the users dataset returned nothing:\n%s", body)
	}

	// And it is a collection, not a fixture: a write lands past the seed.
	resp, body = s.send(http.MethodPost, "/m/checkout/users", `{"name":"Sam"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST to the users dataset: status = %d\n%s", resp.StatusCode, body)
	}
	if got := decodeDocument(t, body)["id"]; got != float64(users.Documents+1) {
		t.Errorf("the created document has id %v, want %d", got, users.Documents+1)
	}

	// The todos dataset came with it and is served at its own path.
	if resp, body := s.get("/m/checkout/todos"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET the todos dataset: status = %d\n%s", resp.StatusCode, body)
	} else if len(decodeArray(t, body)) != datasetOf(t, "todos").Documents {
		t.Errorf("the todos dataset is not the size it says it is:\n%s", body)
	}

	// A dataset that was not ticked was not installed.
	if resp, _ := s.get("/m/checkout/posts"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a dataset nobody chose is being served: status = %d", resp.StatusCode)
	}
}

// An unknown dataset name is a rejection, not something to skip — and because
// the project and its datasets are one transaction, the rejection leaves
// nothing behind.
func TestProjectWithAnUnknownDatasetCreatesNothing(t *testing.T) {
	s := newSite(t)

	if resp, body := s.submit("/register", "/register", formValues(map[string]string{
		"email": "sam@example.com", "password": testPassword,
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d\n%s", resp.StatusCode, body)
	}

	values := formValues(map[string]string{"slug": "checkout", "name": "Checkout API"})
	values["datasets"] = []string{"users", "invoices"}

	resp, body := s.submit("/projects/new", "/projects", values)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "invoices") {
		t.Errorf("the page does not name the dataset it refused:\n%s", body)
	}
	if n := s.count(`select count(*) from projects`); n != 0 {
		t.Errorf("%d projects were created by a rejected form, want 0", n)
	}
	if n := s.count(`select count(*) from collections`); n != 0 {
		t.Errorf("%d collections survived a rejected form, want 0", n)
	}
}

// TestTheM5Milestone is M5's "done when": `curl /m/demo/users` works from a
// logged-out browser. Everything here is real — the demo is provisioned the way
// main.go provisions it, and the client that reads it has no session at all.
func TestTheM5Milestone(t *testing.T) {
	pool := migratedPool(t, startPostgres(t))
	store := core.NewStore(pool)

	// --- provisioned at startup, before the listener opens -----------------
	demo, err := store.EnsureDemoProject(t.Context())
	if err != nil {
		t.Fatalf("EnsureDemoProject: %v", err)
	}
	if demo.Slug != core.DemoSlug || !demo.IsDemo {
		t.Fatalf("provisioned %+v, want the demo project", demo)
	}

	s := newDemoSite(t, pool)

	// --- every built-in dataset answers, with no account ------------------
	for _, d := range core.Datasets() {
		resp, body := s.get("/m/" + core.DemoSlug + d.Path())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s anonymously: status = %d\n%s", d.Path(), resp.StatusCode, body)
		}
		if got := len(decodeArray(t, body)); got != d.Documents {
			t.Errorf("%s returned %d documents, want %d", d.Name, got, d.Documents)
		}
	}
	// Nothing about the client that read it carried a session.
	if cookies := s.client.Jar.Cookies(siteURL(t, s)); len(cookies) != 0 {
		t.Errorf("the anonymous client picked up %d cookies", len(cookies))
	}

	// --- an anonymous write is a real write -------------------------------
	resp, body := s.send(http.MethodPost, "/m/demo/users", `{"name":"A visitor"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("anonymous POST: status = %d\n%s", resp.StatusCode, body)
	}
	created := decodeDocument(t, body)
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("the created document has no Location")
	}
	if resp, body := s.get(location); resp.StatusCode != http.StatusOK || !strings.Contains(body, "A visitor") {
		t.Errorf("the document just created does not come back: status = %d\n%s", resp.StatusCode, body)
	}

	// --- and the scheduled reset takes it away again ----------------------
	//
	// This is what stops one visitor spoiling the demo for the next, and it is
	// the reason anonymous writes can be allowed to be real.
	if resp, _ := s.request(http.MethodDelete, "/m/demo/todos/1"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("anonymous DELETE: status = %d", resp.StatusCode)
	}

	reset, err := store.ResetDemoProjects(t.Context())
	if err != nil {
		t.Fatalf("ResetDemoProjects: %v", err)
	}
	if reset != len(core.Datasets()) {
		t.Errorf("reset %d collections, want %d", reset, len(core.Datasets()))
	}

	if resp, _ := s.get(location); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a visitor's document survived the reset: status = %d", resp.StatusCode)
	}
	if resp, body := s.get("/m/demo/todos/1"); resp.StatusCode != http.StatusOK {
		t.Errorf("the reset did not restore what a visitor deleted: status = %d\n%s", resp.StatusCode, body)
	}
	// The counter went back with the documents, so the next visitor's POST gets
	// the same id this one did rather than counting on from it.
	resp, body = s.send(http.MethodPost, "/m/demo/users", `{"name":"The next visitor"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST after the reset: status = %d\n%s", resp.StatusCode, body)
	}
	if got, want := decodeDocument(t, body)["id"], created["id"]; got != want {
		t.Errorf("the identifier counter was not reset: id = %v, want %v", got, want)
	}

	// --- the demo belongs to nobody who can log in ------------------------
	if n := s.count(`select count(*) from users where email = $1`, core.DemoEmail); n != 1 {
		t.Errorf("%d demo accounts, want exactly 1", n)
	}
	if n := s.count(`select count(*) from projects p join users u on u.id = p.owner_id
	                 where p.is_demo and u.email = $1`, core.DemoEmail); n != 1 {
		t.Error("the demo project is not owned by the demo account")
	}

	// --- and it is nobody's to edit ---------------------------------------
	browser := newSiteSharing(t, s)
	if resp, body := browser.submit("/register", "/register", formValues(map[string]string{
		"email": "sam@example.com", "password": testPassword,
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d\n%s", resp.StatusCode, body)
	}
	if _, body := browser.get("/projects"); strings.Contains(body, "/m/demo/") {
		t.Errorf("the demo project is listed as somebody's own:\n%s", body)
	}
	if resp, _ := browser.get("/projects/demo"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an account can open the demo project: status = %d", resp.StatusCode)
	}

	// --- an anonymous visitor is told the demo is there -------------------
	if _, page := s.get("/login"); !strings.Contains(page, "curl http://restest.test/m/demo/users") {
		t.Errorf("the login page does not offer the demo:\n%s", page)
	}
}

// Provisioning runs on every start, so it has to be safe on every start but the
// first: no second project, no second set of collections, and no second account.
func TestEnsureDemoProjectIsIdempotent(t *testing.T) {
	pool := migratedPool(t, startPostgres(t))
	store := core.NewStore(pool)

	first, err := store.EnsureDemoProject(t.Context())
	if err != nil {
		t.Fatalf("first EnsureDemoProject: %v", err)
	}
	second, err := store.EnsureDemoProject(t.Context())
	if err != nil {
		t.Fatalf("second EnsureDemoProject: %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("a second start created a second demo project: %s then %s", first.ID, second.ID)
	}

	var collections, endpoints, users int
	row := pool.QueryRow(t.Context(), `
		select (select count(*) from collections),
		       (select count(*) from endpoints),
		       (select count(*) from users)`)
	if err := row.Scan(&collections, &endpoints, &users); err != nil {
		t.Fatalf("count what was provisioned: %v", err)
	}
	if want := len(core.Datasets()); collections != want || endpoints != want {
		t.Errorf("%d collections and %d endpoints, want %d of each", collections, endpoints, want)
	}
	if users != 1 {
		t.Errorf("%d accounts, want 1", users)
	}
}

// The demo slug is reserved, so nothing a user does can take it. A row put
// there by hand, or by an instance older than the reserved list, is somebody's
// project — and provisioning stops rather than adopting or overwriting it.
func TestEnsureDemoProjectRefusesAProjectThatIsNotTheDemo(t *testing.T) {
	pool := migratedPool(t, startPostgres(t))
	store := core.NewStore(pool)

	owner, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	// Past the Go validation deliberately: this is the state the check exists
	// for, and it cannot be reached through the store.
	if _, err := pool.Exec(t.Context(),
		`insert into projects (owner_id, slug, name) values ($1, 'demo', 'Mine')`, owner.ID); err != nil {
		t.Fatalf("insert a project holding the demo slug: %v", err)
	}

	if _, err := store.EnsureDemoProject(t.Context()); err == nil {
		t.Fatal("EnsureDemoProject adopted a project that is not the demo")
	}
}

// newDemoSite is newSite over a pool the caller has already provisioned, with
// the demo enabled — the order main.go runs in, where the route table is built
// after the demo exists.
func newDemoSite(t *testing.T, pool *pgxpool.Pool) *site {
	t.Helper()

	_, srv := newAppWith(t, pool, func(o *web.Options) { o.DemoEnabled = true })

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookie jar: %v", err)
	}
	return &site{t: t, client: &http.Client{Jar: jar}, url: ts.URL, pool: pool}
}

// datasetOf finds a built-in dataset by name, so a test can assert against what
// the dataset says it holds rather than against a number copied out of a file.
func datasetOf(t *testing.T, name string) core.Dataset {
	t.Helper()

	for _, d := range core.Datasets() {
		if d.Name == name {
			return d
		}
	}
	t.Fatalf("there is no built-in dataset called %q", name)
	return core.Dataset{}
}

// siteURL is the site's address as a *url.URL, for asking the cookie jar what
// it is holding for it.
func siteURL(t *testing.T, s *site) *url.URL {
	t.Helper()

	u, err := url.Parse(s.url)
	if err != nil {
		t.Fatalf("parse the site URL: %v", err)
	}
	return u
}

func decodeArray(t *testing.T, body string) []map[string]any {
	t.Helper()

	var documents []map[string]any
	if err := json.Unmarshal([]byte(body), &documents); err != nil {
		t.Fatalf("decode the listing: %v\n%s", err, body)
	}
	return documents
}
