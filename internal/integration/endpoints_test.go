//go:build integration

package integration

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// newStoreWithProject is where every endpoint test in this file starts: a user,
// a project they own, and a store over real Postgres.
func newStoreWithProject(t *testing.T) (*core.Store, core.User, core.Project) {
	t.Helper()

	store := newStore(t)

	user, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	project, err := store.CreateProject(t.Context(), user.ID, "checkout", "Checkout API", nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return store, user, project
}

func validInput() core.EndpointInput {
	return core.EndpointInput{
		Method:     "GET",
		Path:       "/users/{id}",
		StatusCode: 200,
		DelayMS:    0,
		IsEnabled:  true,
		Body:       `{"id":1,"name":"Sam"}`,
		Headers:    core.Headers{"Content-Type": "application/json", "X-Mock": "yes"},
	}
}

func TestEndpointRoundTrip(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	created, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput())
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("the new endpoint has no id")
	}
	if created.Kind != core.KindStatic {
		t.Errorf("kind = %q, want %q", created.Kind, core.KindStatic)
	}

	// The headers survive the jsonb round trip, which is the part a Go-only
	// test would not have checked.
	read, err := store.EndpointByOwnerAndID(t.Context(), user.ID, created.ID)
	if err != nil {
		t.Fatalf("EndpointByOwnerAndID: %v", err)
	}
	if read.Headers["X-Mock"] != "yes" || read.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers = %v, want both back", read.Headers)
	}
	if read.Body != `{"id":1,"name":"Sam"}` {
		t.Errorf("body = %q, want it stored verbatim", read.Body)
	}
}

// TestEndpointPathIsStoredNormalised is what makes the unique index mean what
// it says: /users/ and /users are the same route, so they must be the same
// stored string.
func TestEndpointPathIsStoredNormalised(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	in := validInput()
	in.Path = "//users//{id}//"

	created, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, in)
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if created.Path != "/users/{id}" {
		t.Errorf("stored path = %q, want it normalised", created.Path)
	}

	// And the index refuses the second spelling of the same route.
	in.Path = "/users/{id}"
	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, in); !errors.Is(err, core.ErrEndpointExists) {
		t.Errorf("second create: err = %v, want ErrEndpointExists", err)
	}
}

func TestEndpointDuplicateIsRefusedByTheIndex(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput()); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput()); !errors.Is(err, core.ErrEndpointExists) {
		t.Errorf("second create: err = %v, want ErrEndpointExists", err)
	}

	// The same path under a different verb is a different endpoint.
	other := validInput()
	other.Method = "DELETE"
	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, other); err != nil {
		t.Errorf("same path, different verb: %v", err)
	}
}

// TestEndpointsAreScopedToTheirOwner: every statement carries the ownership
// test, so an endpoint in somebody else's project is indistinguishable from one
// that does not exist.
func TestEndpointsAreScopedToTheirOwner(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	intruder, err := store.RegisterUser(t.Context(), "mallory@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	endpoint, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput())
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	if _, err := store.CreateEndpoint(t.Context(), intruder.ID, project.ID, validInput()); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("create into another's project: err = %v, want ErrNotFound", err)
	}
	if _, err := store.EndpointByOwnerAndID(t.Context(), intruder.ID, endpoint.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("read another's endpoint: err = %v, want ErrNotFound", err)
	}
	if _, err := store.UpdateEndpoint(t.Context(), intruder.ID, endpoint.ID, validInput()); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("update another's endpoint: err = %v, want ErrNotFound", err)
	}
	if err := store.DeleteEndpoint(t.Context(), intruder.ID, endpoint.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("delete another's endpoint: err = %v, want ErrNotFound", err)
	}

	// The owner still has it, untouched.
	if _, err := store.EndpointByOwnerAndID(t.Context(), user.ID, endpoint.ID); err != nil {
		t.Errorf("owner's own endpoint: %v", err)
	}
	endpoints, err := store.EndpointsByProject(t.Context(), intruder.ID, project.ID)
	if err != nil {
		t.Fatalf("EndpointsByProject: %v", err)
	}
	if len(endpoints) != 0 {
		t.Errorf("listed %d of another's endpoints, want none", len(endpoints))
	}
}

func TestEndpointUpdateAndDelete(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	endpoint, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput())
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	changed := validInput()
	changed.Method = "POST"
	changed.Path = "/orders"
	changed.StatusCode = 201
	changed.DelayMS = 150
	changed.IsEnabled = false
	changed.Headers = core.Headers{"X-Other": "1"}

	updated, err := store.UpdateEndpoint(t.Context(), user.ID, endpoint.ID, changed)
	if err != nil {
		t.Fatalf("UpdateEndpoint: %v", err)
	}
	if updated.Method != "POST" || updated.Path != "/orders" || updated.StatusCode != 201 || updated.DelayMS != 150 {
		t.Errorf("updated = %+v, want every field changed", updated)
	}
	if updated.IsEnabled {
		t.Error("is_enabled did not change")
	}
	// The headers were replaced, not merged: the field is one value.
	if _, ok := updated.Headers["X-Mock"]; ok {
		t.Errorf("headers = %v, want the old ones gone", updated.Headers)
	}

	if err := store.DeleteEndpoint(t.Context(), user.ID, endpoint.ID); err != nil {
		t.Fatalf("DeleteEndpoint: %v", err)
	}
	if err := store.DeleteEndpoint(t.Context(), user.ID, endpoint.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

// TestEndpointsCascadeFromTheProject pins the schema's own promise: deleting a
// project takes its endpoints with it, so nothing is left addressing a slug
// that is gone.
func TestEndpointsCascadeFromTheProject(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput()); err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if err := store.DeleteProject(t.Context(), user.ID, project.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	data, err := store.MockData(t.Context())
	if err != nil {
		t.Fatalf("MockData: %v", err)
	}
	if len(data.Endpoints) != 0 {
		t.Errorf("%d endpoints survive the project, want 0", len(data.Endpoints))
	}
}

func TestMockDataFeedsTheRouteTable(t *testing.T) {
	store, user, project := newStoreWithProject(t)

	enabled := validInput()
	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, enabled); err != nil {
		t.Fatalf("create enabled: %v", err)
	}
	disabled := validInput()
	disabled.Path = "/hidden"
	disabled.IsEnabled = false
	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, disabled); err != nil {
		t.Fatalf("create disabled: %v", err)
	}

	data, err := store.MockData(t.Context())
	if err != nil {
		t.Fatalf("MockData: %v", err)
	}

	// The project is listed even though it is not the only source of routes:
	// "nothing defined here" needs it.
	if len(data.Projects) != 1 || data.Projects[0].Slug != "checkout" {
		t.Errorf("projects = %+v, want just checkout", data.Projects)
	}
	// A disabled endpoint is invisible to the matcher rather than a case it has
	// to remember.
	if len(data.Endpoints) != 1 {
		t.Fatalf("%d endpoints, want only the enabled one: %+v", len(data.Endpoints), data.Endpoints)
	}
	if data.Endpoints[0].ProjectSlug != "checkout" {
		t.Errorf("project slug = %q, want checkout", data.Endpoints[0].ProjectSlug)
	}
	if data.Endpoints[0].Headers["X-Mock"] != "yes" {
		t.Errorf("headers = %v, want them carried into the table", data.Endpoints[0].Headers)
	}
}

// TestSchemaRefusesWhatTheGoRulesRefuse is the counterpart to the slug test in
// store_test.go: the check constraints and the Go validation have to agree, and
// the only way to know is to insert past the Go rules.
func TestSchemaRefusesWhatTheGoRulesRefuse(t *testing.T) {
	store, user, project := newStoreWithProject(t)
	pool := store.Pool()

	insert := func(method, path string, status int16, delay int32) error {
		_, err := pool.Exec(t.Context(), `
			insert into endpoints (project_id, method, path_pattern, kind, status_code, delay_ms)
			values ($1, $2, $3, 'static', $4, $5)`,
			project.ID, method, path, status, delay)
		return err
	}

	refused := []struct {
		name   string
		method string
		path   string
		status int16
		delay  int32
	}{
		{name: "an unknown verb", method: "FETCH", path: "/x", status: 200},
		{name: "a relative path", method: "GET", path: "x", status: 200},
		{name: "a negative delay", method: "GET", path: "/x", status: 200, delay: -1},
		{name: "a delay past the ceiling", method: "GET", path: "/x", status: 200, delay: 60001},
		{name: "a static endpoint with no status", method: "GET", path: "/x", status: 0},
	}

	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			var err error
			if status == 0 {
				// A null status, which the kind constraint is what refuses.
				_, err = pool.Exec(t.Context(), `
					insert into endpoints (project_id, method, path_pattern, kind)
					values ($1, $2, $3, 'static')`, project.ID, tc.method, tc.path)
			} else {
				err = insert(tc.method, tc.path, status, tc.delay)
			}
			if err == nil {
				t.Error("the database accepted it; the Go rules and the schema disagree")
			}
		})
	}

	// And what Go accepts, the schema accepts.
	if _, err := store.CreateEndpoint(t.Context(), user.ID, project.ID, validInput()); err != nil {
		t.Errorf("a valid endpoint was refused: %v", err)
	}
}

// TestTheM2Milestone is M2's "done when": an endpoint defined through the UI
// answers correctly to a plain HTTP client, with nothing restarted in between.
func TestTheM2Milestone(t *testing.T) {
	s := newSite(t)
	registerAndCreateProject(t, s)

	// --- define an endpoint through the form ----------------------------
	resp, body := s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", formValues(map[string]string{
		"method":      "GET",
		"path":        "/users/{id}",
		"status_code": "200",
		"delay_ms":    "0",
		"is_enabled":  "1",
		"headers":     "Content-Type: application/json\nX-Mock: yes",
		"body":        `{"id":1,"name":"Sam"}`,
	}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create endpoint: status = %d\n%s", resp.StatusCode, body)
	}
	if got := resp.Request.URL.Path; got != "/projects/checkout" {
		t.Fatalf("landed on %q, want the project page", got)
	}
	if n := s.count(`select count(*) from endpoints where path_pattern = '/users/{id}'`); n != 1 {
		t.Fatalf("%d endpoints stored, want 1", n)
	}

	// --- and it answers, without a restart ------------------------------
	resp, body = s.get("/m/checkout/users/7")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mock request: status = %d\n%s", resp.StatusCode, body)
	}
	if body != `{"id":1,"name":"Sam"}` {
		t.Errorf("body = %q, want the body defined in the form", body)
	}
	if got := resp.Header.Get("X-Mock"); got != "yes" {
		t.Errorf("X-Mock = %q, want yes", got)
	}

	// --- the path exists under one verb only ----------------------------
	resp, body = s.request(http.MethodDelete, "/m/checkout/users/7")
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("DELETE: status = %d, want 405\n%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}

	// --- an unmatched path says what is nearby --------------------------
	resp, body = s.get("/m/checkout/userz/7")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("typo: status = %d, want 404\n%s", resp.StatusCode, body)
	}
	var answer struct {
		Error   string `json:"error"`
		Nearest []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		} `json:"nearest"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("the 404 body is not JSON: %v\n%s", err, body)
	}
	if len(answer.Nearest) == 0 || answer.Nearest[0].Path != "/users/{id}" {
		t.Errorf("nearest = %+v, want the route that was mistyped", answer.Nearest)
	}

	// --- a literal segment outranks the parameter -----------------------
	resp, body = s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", formValues(map[string]string{
		"method": "GET", "path": "/users/me", "status_code": "200",
		"delay_ms": "0", "is_enabled": "1", "headers": "", "body": `{"id":0,"name":"You"}`,
	}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create /users/me: status = %d\n%s", resp.StatusCode, body)
	}

	if _, body := s.get("/m/checkout/users/me"); body != `{"id":0,"name":"You"}` {
		t.Errorf("/users/me returned %q, want the literal route to win", body)
	}
	if _, body := s.get("/m/checkout/users/7"); body != `{"id":1,"name":"Sam"}` {
		t.Errorf("/users/7 returned %q, want the parameter route", body)
	}

	// --- deleting it takes it off the air -------------------------------
	var id string
	if err := s.pool.QueryRow(t.Context(),
		`select id::text from endpoints where path_pattern = '/users/me'`).Scan(&id); err != nil {
		t.Fatalf("find the endpoint id: %v", err)
	}
	action := "/projects/checkout/endpoints/" + id + "/delete"
	if resp, body := s.submit("/projects/checkout/endpoints/"+id+"/edit", action, formValues(nil)); resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: status = %d\n%s", resp.StatusCode, body)
	}

	// It falls through to the parameter route now, rather than 404ing: /users/me
	// is still a path this project answers.
	if _, body := s.get("/m/checkout/users/me"); body != `{"id":1,"name":"Sam"}` {
		t.Errorf("after the delete /users/me returned %q, want the parameter route", body)
	}
}

// TestRenamingAProjectMovesItsMockURL is the other half of the rebuild: the
// table is keyed by slug, so a rename has to move every route with it.
func TestRenamingAProjectMovesItsMockURL(t *testing.T) {
	s := newSite(t)
	registerAndCreateProject(t, s)

	if resp, body := s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", formValues(map[string]string{
		"method": "GET", "path": "/ping", "status_code": "200",
		"delay_ms": "0", "is_enabled": "1", "headers": "", "body": "pong",
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("create endpoint: status = %d\n%s", resp.StatusCode, body)
	}

	if resp, body := s.submit("/projects/checkout/edit", "/projects/checkout", formValues(map[string]string{
		"slug": "billing", "name": "Billing API",
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("rename: status = %d\n%s", resp.StatusCode, body)
	}

	if resp, body := s.get("/m/billing/ping"); resp.StatusCode != http.StatusOK || body != "pong" {
		t.Errorf("new slug: status = %d, body = %q, want 200 and pong", resp.StatusCode, body)
	}
	if resp, _ := s.get("/m/checkout/ping"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("old slug: status = %d, want 404", resp.StatusCode)
	}
}

// TestDisabledEndpointDoesNotAnswer: disabling is not deleting, and the route
// has to leave the table without the row leaving the database.
func TestDisabledEndpointDoesNotAnswer(t *testing.T) {
	s := newSite(t)
	registerAndCreateProject(t, s)

	fields := map[string]string{
		"method": "GET", "path": "/ping", "status_code": "200",
		"delay_ms": "0", "is_enabled": "1", "headers": "", "body": "pong",
	}
	if resp, body := s.submit("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", formValues(fields)); resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d\n%s", resp.StatusCode, body)
	}
	if resp, _ := s.get("/m/checkout/ping"); resp.StatusCode != http.StatusOK {
		t.Fatalf("before disabling: status = %d, want 200", resp.StatusCode)
	}

	var id string
	if err := s.pool.QueryRow(t.Context(),
		`select id::text from endpoints where path_pattern = '/ping'`).Scan(&id); err != nil {
		t.Fatalf("find the endpoint id: %v", err)
	}

	delete(fields, "is_enabled")
	action := "/projects/checkout/endpoints/" + id
	if resp, body := s.submit(action+"/edit", action, formValues(fields)); resp.StatusCode != http.StatusOK {
		t.Fatalf("disable: status = %d\n%s", resp.StatusCode, body)
	}

	if resp, _ := s.get("/m/checkout/ping"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after disabling: status = %d, want 404", resp.StatusCode)
	}
	if n := s.count(`select count(*) from endpoints where path_pattern = '/ping'`); n != 1 {
		t.Errorf("%d rows, want the endpoint still stored", n)
	}
}

// registerAndCreateProject gets a site to the point every endpoint test starts
// from: logged in, with one project.
func registerAndCreateProject(t *testing.T, s *site) {
	t.Helper()

	if resp, body := s.submit("/register", "/register", formValues(map[string]string{
		"email": "sam@example.com", "password": testPassword,
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("register: status = %d\n%s", resp.StatusCode, body)
	}
	if resp, body := s.submit("/projects/new", "/projects", formValues(map[string]string{
		"slug": "checkout", "name": "Checkout API",
	})); resp.StatusCode != http.StatusOK {
		t.Fatalf("create project: status = %d\n%s", resp.StatusCode, body)
	}
}

// request sends a verb the browser helpers do not cover — mock traffic, which
// needs neither a CSRF token nor a form body.
func (s *site) request(method, path string) (*http.Response, string) {
	s.t.Helper()

	req, err := http.NewRequest(method, s.url+path, nil)
	if err != nil {
		s.t.Fatalf("build %s %s: %v", method, path, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp, readBody(s.t, resp)
}

func formValues(fields map[string]string) url.Values {
	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	return values
}
