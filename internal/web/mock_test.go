package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
	"github.com/dmitrykvasnikov/restest/internal/mock"
)

// mockSlug is the project every mock test in this file is served under.
const mockSlug = "shop"

// mockEndpoint is a static endpoint, described the way a test wants to read it.
type mockEndpoint struct {
	method  string
	path    string
	status  int
	body    string
	headers core.Headers
	delayMS int
}

// withMocks builds a server whose route table holds these endpoints, and
// returns a client for it. Mock traffic carries no session and no CSRF token,
// so the client is a plain one rather than the cookie-jar browser the UI tests
// use — which is itself part of what is being checked.
func withMocks(t *testing.T, endpoints ...mockEndpoint) *browser {
	t.Helper()

	project := core.MockProject{ID: uuid.New(), Slug: mockSlug}
	data := core.MockData{Projects: []core.MockProject{project}}

	for _, e := range endpoints {
		data.Endpoints = append(data.Endpoints, core.MockEndpoint{
			ProjectSlug: mockSlug,
			Endpoint: core.Endpoint{
				ID:         uuid.New(),
				ProjectID:  project.ID,
				Method:     e.method,
				Path:       core.NormalizePath(e.path),
				Kind:       core.KindStatic,
				IsEnabled:  true,
				StatusCode: e.status,
				Body:       e.body,
				Headers:    e.headers,
				DelayMS:    e.delayMS,
			},
		})
	}

	store := stubStore{
		mockData: func(context.Context) (core.MockData, error) { return data, nil },
	}
	return newBrowser(t, store)
}

// do sends a bare request, without the CSRF token or the same-origin headers a
// form submission needs. A mock client is not a browser.
func (b *browser) do(method, path string, body string) (*http.Response, string) {
	b.t.Helper()

	req, err := http.NewRequest(method, b.url+path, strings.NewReader(body))
	if err != nil {
		b.t.Fatalf("build %s %s: %v", method, path, err)
	}

	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp, readBody(b.t, resp)
}

func TestMockServesTheEndpoint(t *testing.T) {
	b := withMocks(t, mockEndpoint{
		method: "GET", path: "/users/{id}", status: 201,
		body:    `{"id":1,"name":"Sam"}`,
		headers: core.Headers{"X-Mock": "yes"},
	})

	resp, body := b.get("/m/shop/users/7")

	if resp.StatusCode != 201 {
		t.Errorf("status = %d, want 201", resp.StatusCode)
	}
	if body != `{"id":1,"name":"Sam"}` {
		t.Errorf("body = %q, want the endpoint's body verbatim", body)
	}
	if got := resp.Header.Get("X-Mock"); got != "yes" {
		t.Errorf("X-Mock = %q, want yes", got)
	}
	// No Content-Type was set, and the body parses as JSON.
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the sniffed JSON type", got)
	}
}

func TestMockContentType(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		headers core.Headers
		want    string
	}{
		{
			name: "JSON is sniffed by parsing",
			body: `{"ok":true}`,
			want: "application/json; charset=utf-8",
		},
		{
			name: "a JSON array too",
			body: `[1,2,3]`,
			want: "application/json; charset=utf-8",
		},
		{
			name: "anything else falls back to net/http's own sniffing",
			body: "hello, world",
			want: "text/plain; charset=utf-8",
		},
		{
			name: "HTML is recognised",
			body: "<!DOCTYPE html><html><body>hi</body></html>",
			want: "text/html; charset=utf-8",
		},
		{
			name:    "an endpoint's own Content-Type is never overridden",
			body:    `{"ok":true}`,
			headers: core.Headers{"Content-Type": "application/vnd.api+json"},
			want:    "application/vnd.api+json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := withMocks(t, mockEndpoint{
				method: "GET", path: "/thing", status: 200,
				body: tc.body, headers: tc.headers,
			})

			resp, _ := b.get("/m/shop/thing")
			if got := resp.Header.Get("Content-Type"); got != tc.want {
				t.Errorf("Content-Type = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMockAcceptsWritesWithoutCSRF is the property that makes the mock server
// usable at all: it is not browser traffic, and a POST from curl carries no
// token. Routing it through nosurf would answer every write with 400.
func TestMockAcceptsWritesWithoutCSRF(t *testing.T) {
	b := withMocks(t, mockEndpoint{
		method: "POST", path: "/orders", status: 202, body: `{"queued":true}`,
	})

	resp, body := b.do(http.MethodPost, "/m/shop/orders", `{"item":"x"}`)

	if resp.StatusCode != 202 {
		t.Fatalf("status = %d, want 202\nbody: %s", resp.StatusCode, body)
	}
	// A session cookie here would mean the mock server had been dragged through
	// the session middleware, which is a database round trip per mock request.
	if len(resp.Cookies()) != 0 {
		t.Errorf("cookies = %v, want none on a mock response", resp.Cookies())
	}
}

func TestMockMethodFallthrough(t *testing.T) {
	b := withMocks(t,
		mockEndpoint{method: "GET", path: "/users", status: 200, body: "[]"},
		mockEndpoint{method: "POST", path: "/users", status: 201, body: "{}"},
	)

	resp, body := b.do(http.MethodDelete, "/m/shop/users", "")

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405\nbody: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD, POST")
	}

	var answer mockError
	decodeJSON(t, body, &answer)
	if !slices.Equal(answer.Allow, []string{"GET", "HEAD", "POST"}) {
		t.Errorf("allow in the body = %v, want the same as the header", answer.Allow)
	}
	if answer.Path != "/users" {
		t.Errorf("path = %q, want /users", answer.Path)
	}
}

// TestMockOptionsIsAnswered covers the one verb where 405 would be the wrong
// answer: OPTIONS asks which verbs a path takes, and Allow already says.
func TestMockOptionsIsAnswered(t *testing.T) {
	b := withMocks(t, mockEndpoint{method: "GET", path: "/users", status: 200, body: "[]"})

	resp, body := b.do(http.MethodOptions, "/m/shop/users", "")

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204\nbody: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

func TestMockNotFoundListsNearestRoutes(t *testing.T) {
	b := withMocks(t,
		mockEndpoint{method: "GET", path: "/users", status: 200, body: "[]"},
		mockEndpoint{method: "GET", path: "/users/{id}/posts", status: 200, body: "[]"},
		mockEndpoint{method: "GET", path: "/orders", status: 200, body: "[]"},
	)

	resp, body := b.get("/m/shop/users/7/postz")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", got)
	}

	var answer mockError
	decodeJSON(t, body, &answer)

	if answer.Project != mockSlug {
		t.Errorf("project = %q, want %q", answer.Project, mockSlug)
	}
	if answer.Method != http.MethodGet || answer.Path != "/users/7/postz" {
		t.Errorf("method/path = %s %s, want GET /users/7/postz", answer.Method, answer.Path)
	}
	if len(answer.Nearest) == 0 {
		t.Fatalf("nearest is empty; a bare 404 is what this exists to avoid")
	}
	if want := (mock.Ref{Method: "GET", Path: "/users/{id}/posts"}); answer.Nearest[0] != want {
		t.Errorf("nearest[0] = %v, want %v", answer.Nearest[0], want)
	}
	if answer.Allow != nil {
		t.Errorf("allow = %v, want nothing on a 404", answer.Allow)
	}
}

func TestMockUnknownProject(t *testing.T) {
	b := withMocks(t, mockEndpoint{method: "GET", path: "/users", status: 200, body: "[]"})

	resp, body := b.get("/m/nope/users")

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}

	var answer mockError
	decodeJSON(t, body, &answer)
	if !strings.Contains(answer.Error, `"nope"`) {
		t.Errorf("error = %q, want it to name the slug", answer.Error)
	}
	if len(answer.Nearest) != 0 {
		t.Errorf("nearest = %v, want none: there is no project to suggest within", answer.Nearest)
	}
}

// TestMockEncodedPathSegment is the case that r.URL.Path would get wrong: the
// encoded slash has to stay inside the parameter rather than becoming a segment
// boundary.
func TestMockEncodedPathSegment(t *testing.T) {
	b := withMocks(t, mockEndpoint{
		method: "GET", path: "/files/{name}", status: 200, body: "ok",
	})

	resp, body := b.get("/m/shop/files/a%2Fb")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\nbody: %s", resp.StatusCode, body)
	}

	// Two real segments must not match the one-segment pattern.
	resp, _ = b.get("/m/shop/files/a/b")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/files/a/b: status = %d, want 404", resp.StatusCode)
	}
}

func TestMockDelay(t *testing.T) {
	const delay = 120 * time.Millisecond

	b := withMocks(t, mockEndpoint{
		method: "GET", path: "/slow", status: 200, body: "ok",
		delayMS: int(delay / time.Millisecond),
	})

	start := time.Now()
	resp, _ := b.get("/m/shop/slow")
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed < delay {
		t.Errorf("answered in %v, want at least the configured %v", elapsed, delay)
	}
}

func TestMockStatusesWithoutABody(t *testing.T) {
	// A 204 with a body configured is a definition the user is allowed to make
	// and the server is not allowed to honour.
	b := withMocks(t, mockEndpoint{
		method: "DELETE", path: "/users/{id}", status: 204, body: `{"deleted":true}`,
	})

	resp, body := b.do(http.MethodDelete, "/m/shop/users/1", "")

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if body != "" {
		t.Errorf("body = %q, want empty on a 204", body)
	}
}

func TestMockHeadHasNoBody(t *testing.T) {
	b := withMocks(t, mockEndpoint{
		method: "GET", path: "/users", status: 200, body: `[{"id":1}]`,
	})

	resp, body := b.do(http.MethodHead, "/m/shop/users", "")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body != "" {
		t.Errorf("body = %q, want empty on a HEAD", body)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want the GET route's", got)
	}
}

// TestMockRootPath covers the project root, /m/{slug}/, which is a path like
// any other and is easy to leave unreachable.
func TestMockRootPath(t *testing.T) {
	b := withMocks(t, mockEndpoint{method: "GET", path: "/", status: 200, body: "root"})

	resp, body := b.get("/m/shop/")
	if resp.StatusCode != http.StatusOK || body != "root" {
		t.Errorf("/m/shop/: status = %d, body = %q, want 200 and \"root\"", resp.StatusCode, body)
	}

	// Without the trailing slash, net/http's own subtree redirect should bring
	// the client to the same place.
	resp, body = b.get("/m/shop")
	if resp.StatusCode != http.StatusOK || body != "root" {
		t.Errorf("/m/shop: status = %d, body = %q, want the redirect to land on the root",
			resp.StatusCode, body)
	}
}

func TestMockDoesNotShadowTheApplication(t *testing.T) {
	b := withMocks(t, mockEndpoint{method: "GET", path: "/users", status: 200, body: "[]"})

	// The mock route is a subtree of /m/, and nothing above it.
	for _, path := range []string{"/", "/login", "/healthz"} {
		resp, _ := b.get(path)
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("%s: status = 404; the mock route has swallowed an application page", path)
		}
	}
}

func decodeJSON(t *testing.T, body string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(body), v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}
