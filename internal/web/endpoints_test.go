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

var testEndpointID = uuid.MustParse("33333333-3333-4333-8333-333333333333")

func testEndpoint() core.Endpoint {
	return core.Endpoint{
		ID:         testEndpointID,
		ProjectID:  testProjectID,
		Method:     http.MethodGet,
		Path:       "/users/{id}",
		Kind:       core.KindStatic,
		IsEnabled:  true,
		StatusCode: 200,
		DelayMS:    250,
		Body:       `{"id":1}`,
		Headers:    core.Headers{"Content-Type": "application/json", "X-Mock": "yes"},
	}
}

// endpointStore is projectStore plus one endpoint in that project.
func endpointStore() stubStore {
	store := projectStore()
	store.endpointsByProject = func(context.Context, uuid.UUID, uuid.UUID) ([]core.Endpoint, error) {
		return []core.Endpoint{testEndpoint()}, nil
	}
	store.endpointByOwner = func(_ context.Context, _ uuid.UUID, id uuid.UUID) (core.Endpoint, error) {
		if id != testEndpointID {
			return core.Endpoint{}, core.ErrNotFound
		}
		return testEndpoint(), nil
	}
	return store
}

// endpointValues is the full set of fields the form submits, so a test can change
// one and leave the rest valid.
func endpointValues() url.Values {
	return url.Values{
		"method":      {"POST"},
		"path":        {"/orders"},
		"status_code": {"201"},
		"delay_ms":    {"0"},
		"is_enabled":  {"1"},
		"headers":     {"Content-Type: application/json\nX-Mock: yes"},
		"body":        {`{"ok":true}`},
	}
}

func TestProjectPageListsEndpoints(t *testing.T) {
	b := newBrowser(t, endpointStore())
	logIn(t, b)

	_, body := b.get("/projects/checkout")

	for _, want := range []string{"GET", "/users/{id}", "200", "250 ms"} {
		if !strings.Contains(body, want) {
			t.Errorf("the endpoint list does not show %q:\n%s", want, body)
		}
	}
}

func TestProjectPageWithNoEndpoints(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	_, body := b.get("/projects/checkout")
	if !strings.Contains(body, "Nothing defined yet") {
		t.Errorf("no empty state on the project page:\n%s", body)
	}
}

// TestNewEndpointFormIsUsableAsItStands: the defaults are a working endpoint,
// because the point of the first one is to see something answer.
func TestNewEndpointFormIsUsableAsItStands(t *testing.T) {
	b := newBrowser(t, projectStore())
	logIn(t, b)

	resp, body := b.get("/projects/checkout/endpoints/new")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	for _, want := range []string{
		`value="/hello"`, `value="200"`, `data-editor="json"`,
		"vendor/codemirror/codemirror.js", // the editor is loaded on this page
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the new endpoint form does not contain %q", want)
		}
	}
}

func TestEndpointCreate(t *testing.T) {
	var got core.EndpointInput
	var gotOwner, gotProject uuid.UUID

	store := endpointStore()
	store.createEndpoint = func(_ context.Context, owner, project uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
		gotOwner, gotProject, got = owner, project, in
		return testEndpoint(), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", endpointValues())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want a redirect to the project page\nbody: %s", resp.StatusCode, body)
	}

	if gotOwner != testUser.ID || gotProject != testProjectID {
		t.Errorf("saved under owner %v project %v, want %v and %v",
			gotOwner, gotProject, testUser.ID, testProjectID)
	}
	if got.Method != "POST" || got.Path != "/orders" || got.StatusCode != 201 {
		t.Errorf("input = %+v, want POST /orders 201", got)
	}
	if got.Headers["X-Mock"] != "yes" || got.Headers["Content-Type"] != "application/json" {
		t.Errorf("headers = %v, want both lines parsed", got.Headers)
	}
	if !got.IsEnabled {
		t.Error("is_enabled was submitted and did not survive")
	}
	// The flash names the URL the endpoint answers on, which is what the user
	// came to the page to get.
	if !strings.Contains(body, "http://restest.test/m/checkout/users/{id}") {
		t.Errorf("the confirmation does not show where it answers:\n%s", body)
	}
}

// TestEndpointCreateRebuildsTheRouteTable is the join between the two halves of
// this milestone: an endpoint defined in the UI has to answer without a
// restart. It is the milestone's "done when", short of a real database.
func TestEndpointCreateRebuildsTheRouteTable(t *testing.T) {
	project := core.MockProject{ID: testProjectID, Slug: "checkout"}
	data := core.MockData{Projects: []core.MockProject{project}}

	store := endpointStore()
	store.mockData = func(context.Context) (core.MockData, error) { return data, nil }
	store.createEndpoint = func(_ context.Context, _, _ uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
		endpoint := core.Endpoint{
			ID: uuid.New(), ProjectID: testProjectID,
			Method: in.Method, Path: in.Path, Kind: core.KindStatic,
			IsEnabled: in.IsEnabled, StatusCode: in.StatusCode,
			Body: in.Body, Headers: in.Headers, DelayMS: in.DelayMS,
		}
		data.Endpoints = append(data.Endpoints, core.MockEndpoint{
			Endpoint: endpoint, ProjectSlug: "checkout",
		})
		return endpoint, nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	// Before: the project exists and answers nothing.
	resp, _ := b.get("/m/checkout/orders")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("before defining it: status = %d, want 404", resp.StatusCode)
	}

	if resp, body := b.post("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", endpointValues()); resp.StatusCode != http.StatusOK {
		t.Fatalf("create: status = %d\nbody: %s", resp.StatusCode, body)
	}

	// After: no restart, no reload of anything but the table.
	resp, body := b.do(http.MethodPost, "/m/checkout/orders", "")
	if resp.StatusCode != 201 {
		t.Fatalf("after defining it: status = %d, want 201\nbody: %s", resp.StatusCode, body)
	}
	if body != `{"ok":true}` {
		t.Errorf("body = %q, want the body that was typed into the form", body)
	}
}

func TestEndpointEditFormIsFilledIn(t *testing.T) {
	b := newBrowser(t, endpointStore())
	logIn(t, b)

	resp, body := b.get("/projects/checkout/endpoints/" + testEndpointID.String() + "/edit")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	for _, want := range []string{
		`value="/users/{id}"`, `value="200"`, `value="250"`, `{&#34;id&#34;:1}`,
		// Headers come back sorted and one per line, so editing twice without
		// touching the field does not reorder it.
		"Content-Type: application/json\nX-Mock: yes",
		"/delete", // the edit page is where an endpoint is removed
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the edit form does not contain %q:\n%s", want, body)
		}
	}
}

func TestEndpointUpdate(t *testing.T) {
	var got core.EndpointInput

	store := endpointStore()
	store.updateEndpoint = func(_ context.Context, _, id uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
		if id != testEndpointID {
			t.Errorf("updated %v, want %v", id, testEndpointID)
		}
		got = in
		return testEndpoint(), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	action := "/projects/checkout/endpoints/" + testEndpointID.String()
	values := endpointValues()
	values.Del("is_enabled") // unchecking the box

	resp, body := b.post(action+"/edit", action, values)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\nbody: %s", resp.StatusCode, body)
	}
	if got.IsEnabled {
		t.Error("is_enabled = true, want the unchecked box to disable the endpoint")
	}
}

func TestEndpointDelete(t *testing.T) {
	var deleted uuid.UUID

	store := endpointStore()
	store.deleteEndpoint = func(_ context.Context, _, id uuid.UUID) error {
		deleted = id
		return nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	action := "/projects/checkout/endpoints/" + testEndpointID.String()
	resp, body := b.post(action+"/edit", action+"/delete", url.Values{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\nbody: %s", resp.StatusCode, body)
	}
	if deleted != testEndpointID {
		t.Errorf("deleted %v, want %v", deleted, testEndpointID)
	}
}

// TestEndpointRejections walks the messages a user can actually cause. The
// rules themselves are core's and tested there; what is checked here is that
// they arrive back on the form rather than as a 500.
func TestEndpointRejections(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
		want  string
	}{
		{name: "status out of range", field: "status_code", value: "700", want: "status between"},
		{name: "status not a number", field: "status_code", value: "20O", want: "status code"},
		{name: "delay not a number", field: "delay_ms", value: "soon", want: "milliseconds"},
		{name: "delay too long", field: "delay_ms", value: "999999", want: "milliseconds"},
		{name: "partial parameter", field: "path", value: "/v{n}/users", want: "whole segment"},
		{name: "repeated parameter", field: "path", value: "/users/{id}/x/{id}", want: "twice"},
		{name: "query string in the path", field: "path", value: "/users?page=1", want: "query string"},
		{name: "framing header", field: "headers", value: "Content-Length: 12", want: "cannot be overridden"},
		{name: "malformed header", field: "headers", value: "not a header name", want: "one header per line"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := endpointStore()
			store.createEndpoint = func(_ context.Context, _, _ uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
				// core does the validating, so the handler must be calling it.
				return core.Endpoint{}, in.Validate()
			}

			b := newBrowser(t, store)
			logIn(t, b)

			values := endpointValues()
			values.Set(tc.field, tc.value)

			resp, body := b.post("/projects/checkout/endpoints/new",
				"/projects/checkout/endpoints", values)

			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422\nbody: %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("the page does not explain the rejection (%q):\n%s", tc.want, body)
			}
			// A rejected form comes back filled in, not blank.
			if !strings.Contains(body, "&#34;ok&#34;:true") {
				t.Error("the body the user typed was lost on the way back")
			}
		})
	}
}

func TestEndpointDuplicatePath(t *testing.T) {
	store := endpointStore()
	store.createEndpoint = func(context.Context, uuid.UUID, uuid.UUID, core.EndpointInput) (core.Endpoint, error) {
		return core.Endpoint{}, core.ErrEndpointExists
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, body := b.post("/projects/checkout/endpoints/new",
		"/projects/checkout/endpoints", endpointValues())

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", resp.StatusCode)
	}
	if !strings.Contains(body, "already answers") {
		t.Errorf("the collision is not explained:\n%s", body)
	}
}

// TestEndpointOfAnotherProjectIs404 closes the gap between the two identifiers
// in the URL: a valid endpoint id reached through the wrong project's slug is
// not an endpoint of that project.
func TestEndpointOfAnotherProjectIs404(t *testing.T) {
	store := endpointStore()
	store.endpointByOwner = func(context.Context, uuid.UUID, uuid.UUID) (core.Endpoint, error) {
		endpoint := testEndpoint()
		endpoint.ProjectID = uuid.New() // somebody else's project
		return endpoint, nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	resp, _ := b.get("/projects/checkout/endpoints/" + testEndpointID.String() + "/edit")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestEndpointPagesRequireAnAccount(t *testing.T) {
	b := newBrowser(t, endpointStore()).noFollow()

	for _, path := range []string{
		"/projects/checkout/endpoints/new",
		"/projects/checkout/endpoints/" + testEndpointID.String() + "/edit",
	} {
		resp, _ := b.get(path)
		if resp.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want a redirect to the login form", path, resp.StatusCode)
		}
	}
}

// TestEndpointBodyLoosesCarriageReturns: browsers submit textarea newlines as
// CRLF, and a body served with a stray \r on every line is not the body that
// was typed.
func TestEndpointBodyLoosesCarriageReturns(t *testing.T) {
	var got string

	store := endpointStore()
	store.createEndpoint = func(_ context.Context, _, _ uuid.UUID, in core.EndpointInput) (core.Endpoint, error) {
		got = in.Body
		return testEndpoint(), nil
	}

	b := newBrowser(t, store)
	logIn(t, b)

	values := endpointValues()
	values.Set("body", "{\r\n  \"a\": 1\r\n}")
	b.post("/projects/checkout/endpoints/new", "/projects/checkout/endpoints", values)

	if strings.Contains(got, "\r") {
		t.Errorf("body = %q, want the carriage returns gone", got)
	}
	if got != "{\n  \"a\": 1\n}" {
		t.Errorf("body = %q, want it otherwise unchanged", got)
	}
}
