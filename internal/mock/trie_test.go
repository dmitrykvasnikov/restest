package mock

import (
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// The route set every matching test below is run against. It is one set rather
// than one per case on purpose: precedence is a property of a table, and a
// pattern that wins against nothing has not been shown to win.
var routes = []string{
	"GET /",
	"GET /users",
	"POST /users",
	"GET /users/me",
	"GET /users/{id}",
	"DELETE /users/{id}",
	"GET /users/{id}/posts",
	"GET /users/{id}/posts/{postID}",
	"GET /a/b/c",
	"GET /{x}/b/d",
	"GET /orders/{id}",
	"PUT /orders/{id}",
	"* /any",
	"GET /any/exact",
	"* /any/exact",
	"POST /webhooks/{provider}/events",
}

func TestLookupMatches(t *testing.T) {
	table := newTable(t, routes...)

	tests := []struct {
		name   string
		method string
		path   string
		// want is the pattern expected to answer, written as it is in `routes`.
		want   string
		params map[string]string
	}{
		{
			name:   "root",
			method: "GET", path: "/",
			want: "GET /",
		},
		{
			name:   "empty path is the root",
			method: "GET", path: "",
			want: "GET /",
		},
		{
			name:   "literal",
			method: "GET", path: "/users",
			want: "GET /users",
		},
		{
			name:   "method selects among routes at one path",
			method: "POST", path: "/users",
			want: "POST /users",
		},
		{
			// The rule PLAN.md names: /users/me beats /users/{id}.
			name:   "a literal segment outranks a parameter",
			method: "GET", path: "/users/me",
			want: "GET /users/me",
		},
		{
			name:   "parameter matches what the literal did not",
			method: "GET", path: "/users/42",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "42"},
		},
		{
			name:   "parameter value that happens to look like a literal elsewhere",
			method: "GET", path: "/users/orders",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "orders"},
		},
		{
			name:   "two parameters",
			method: "GET", path: "/users/7/posts/99",
			want:   "GET /users/{id}/posts/{postID}",
			params: map[string]string{"id": "7", "postID": "99"},
		},
		{
			name:   "literal after a parameter",
			method: "GET", path: "/users/7/posts",
			want:   "GET /users/{id}/posts",
			params: map[string]string{"id": "7"},
		},
		{
			// The case a matcher without backtracking gets wrong: the literal
			// branch matches two segments and then dies, and the answer is up
			// the parameter branch.
			name:   "the search backtracks out of a dead literal branch",
			method: "GET", path: "/a/b/d",
			want:   "GET /{x}/b/d",
			params: map[string]string{"x": "a"},
		},
		{
			name:   "the literal branch still wins when it does reach the end",
			method: "GET", path: "/a/b/c",
			want: "GET /a/b/c",
		},
		{
			name:   "trailing slash matches the pattern without one",
			method: "GET", path: "/users/",
			want: "GET /users",
		},
		{
			name:   "trailing slash on a parameter route",
			method: "GET", path: "/users/42/",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "42"},
		},
		{
			name:   "repeated slashes collapse",
			method: "GET", path: "//users//me//",
			want: "GET /users/me",
		},
		{
			// The consequence of collapsing, worth pinning: an empty segment
			// disappears rather than matching a parameter. This is /users/posts,
			// so /users/{id} answers it — not /users/{id}/posts.
			name:   "an empty segment is dropped, not treated as a parameter",
			method: "GET", path: "/users//posts/",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "posts"},
		},
		{
			name:   "a wildcard method answers a verb of its own",
			method: "PATCH", path: "/any",
			want: "* /any",
		},
		{
			name:   "an exact verb outranks the wildcard at the same path",
			method: "GET", path: "/any/exact",
			want: "GET /any/exact",
		},
		{
			name:   "the wildcard covers the verbs the exact route does not",
			method: "DELETE", path: "/any/exact",
			want: "* /any/exact",
		},
		{
			// net/http discards the body of a HEAD response, so answering from
			// the GET route gives the right headers and no body.
			name:   "HEAD falls back to GET",
			method: "HEAD", path: "/users",
			want: "GET /users",
		},
		{
			name:   "HEAD prefers the wildcard over the GET fallback",
			method: "HEAD", path: "/any",
			want: "* /any",
		},
		{
			// r.URL.Path would have turned this into two segments. Matching on
			// the escaped path is what keeps it one.
			name:   "an encoded slash stays inside its segment",
			method: "GET", path: "/users/a%2Fb",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "a/b"},
		},
		{
			name:   "a percent-encoded literal matches the literal it encodes",
			method: "GET", path: "/users/%6De",
			want: "GET /users/me",
		},
		{
			name:   "non-ASCII in a parameter is decoded",
			method: "GET", path: "/users/%C3%A9lise",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "élise"},
		},
		{
			name:   "an encoded space in a parameter",
			method: "GET", path: "/users/a%20b",
			want:   "GET /users/{id}",
			params: map[string]string{"id": "a b"},
		},
		{
			name:   "parameter in the middle of literals",
			method: "POST", path: "/webhooks/stripe/events",
			want:   "POST /webhooks/{provider}/events",
			params: map[string]string{"provider": "stripe"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := table.Lookup(testProject, tc.method, tc.path)

			if got.Outcome != Matched {
				t.Fatalf("%s %s: outcome = %v, want Matched", tc.method, tc.path, got.Outcome)
			}
			// newTable puts the pattern in the body, so a failure names the
			// route that answered rather than only the one that should have.
			if got.Route.Body != tc.want {
				t.Errorf("%s %s: matched %q, want %q", tc.method, tc.path, got.Route.Body, tc.want)
			}
			if !maps.Equal(got.Params, tc.params) {
				t.Errorf("%s %s: params = %v, want %v", tc.method, tc.path, got.Params, tc.params)
			}
		})
	}
}

func TestLookupWrongMethod(t *testing.T) {
	table := newTable(t, routes...)

	tests := []struct {
		name   string
		method string
		path   string
		allow  []string
	}{
		{
			name:   "a path defined for another verb is 405, not 404",
			method: "DELETE", path: "/users",
			allow: []string{"GET", "HEAD", "POST"},
		},
		{
			name:   "on a parameter route",
			method: "PATCH", path: "/users/42",
			allow: []string{"GET", "HEAD", "DELETE"},
		},
		{
			name:   "the union of the verbs the path answers",
			method: "POST", path: "/orders/1",
			allow: []string{"GET", "HEAD", "PUT"},
		},
		{
			// /users/me matches both the literal route, which answers GET, and
			// /users/{id}, which answers GET and DELETE. Allow has to cover
			// every pattern the path matches, not the first branch searched.
			name:   "the union spans every pattern the path matches",
			method: "PUT", path: "/users/me",
			allow: []string{"GET", "HEAD", "DELETE"},
		},
		{
			name:   "trailing slash does not turn a 405 into a 404",
			method: "DELETE", path: "/users/",
			allow: []string{"GET", "HEAD", "POST"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := table.Lookup(testProject, tc.method, tc.path)

			if tc.allow == nil {
				if got.Outcome != Matched {
					t.Fatalf("%s %s: outcome = %v, want Matched", tc.method, tc.path, got.Outcome)
				}
				return
			}
			if got.Outcome != WrongMethod {
				t.Fatalf("%s %s: outcome = %v, want WrongMethod", tc.method, tc.path, got.Outcome)
			}
			if !slices.Equal(got.Allow, tc.allow) {
				t.Errorf("%s %s: allow = %v, want %v", tc.method, tc.path, got.Allow, tc.allow)
			}
		})
	}
}

func TestLookupNoRoute(t *testing.T) {
	table := newTable(t, routes...)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "unknown first segment", method: "GET", path: "/nope"},
		{name: "too deep", method: "GET", path: "/users/42/posts/1/comments"},
		{name: "too shallow", method: "GET", path: "/webhooks/stripe"},
		{
			// The literal branch matches and dies, and the parameter branch
			// does not exist at that depth either.
			name: "neither branch reaches the end", method: "GET", path: "/a/b/e",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := table.Lookup(testProject, tc.method, tc.path)
			if got.Outcome != NoRoute {
				t.Fatalf("%s %s: outcome = %v, want NoRoute (matched %q)",
					tc.method, tc.path, got.Outcome, got.Route.Body)
			}
		})
	}
}

func TestLookupUnknownProject(t *testing.T) {
	table := newTable(t, routes...)

	got := table.Lookup("no-such-project", "GET", "/users")
	if got.Outcome != NoProject {
		t.Fatalf("outcome = %v, want NoProject", got.Outcome)
	}
}

func TestLookupProjectWithNothingDefined(t *testing.T) {
	// A project that exists and has no endpoints is not the same as a project
	// that does not exist, and the table has to be able to say so.
	table := newTableFor(t, "empty")

	got := table.Lookup("empty", "GET", "/users")
	if got.Outcome != NoRoute {
		t.Fatalf("outcome = %v, want NoRoute", got.Outcome)
	}
	if len(got.Nearest) != 0 {
		t.Errorf("nearest = %v, want none", got.Nearest)
	}
}

// TestProjectsAreIsolated is the property the whole mock URL scheme rests on:
// a route defined in one project is not reachable through another's slug.
func TestProjectsAreIsolated(t *testing.T) {
	one := core.MockProject{ID: uuid.New(), Slug: "one"}
	two := core.MockProject{ID: uuid.New(), Slug: "two"}

	table := BuildTable(core.MockData{
		Projects: []core.MockProject{one, two},
		Endpoints: []core.MockEndpoint{
			{ProjectSlug: "one", Endpoint: core.Endpoint{Method: "GET", Path: "/only-in-one", StatusCode: 200}},
			{ProjectSlug: "two", Endpoint: core.Endpoint{Method: "GET", Path: "/only-in-two", StatusCode: 200}},
		},
	}, discardLogger())

	if got := table.Lookup("one", "GET", "/only-in-one"); got.Outcome != Matched {
		t.Errorf("/only-in-one under one: outcome = %v, want Matched", got.Outcome)
	}
	if got := table.Lookup("two", "GET", "/only-in-one"); got.Outcome != NoRoute {
		t.Errorf("/only-in-one under two: outcome = %v, want NoRoute", got.Outcome)
	}
	if got := table.Lookup("one", "GET", "/only-in-one"); got.Project.ID != one.ID {
		t.Errorf("matched project = %v, want %v", got.Project.ID, one.ID)
	}
}

// TestEndpointWithNoProjectIsDropped covers the torn read MockData allows: the
// project list and the endpoint list are two statements, so an endpoint can
// arrive naming a project that has just been deleted.
func TestEndpointWithNoProjectIsDropped(t *testing.T) {
	table := BuildTable(core.MockData{
		Endpoints: []core.MockEndpoint{
			{ProjectSlug: "gone", Endpoint: core.Endpoint{Method: "GET", Path: "/users", StatusCode: 200}},
		},
	}, discardLogger())

	if got := table.Lookup("gone", "GET", "/users"); got.Outcome != NoProject {
		t.Errorf("outcome = %v, want NoProject", got.Outcome)
	}
	if got := table.Routes(); got != 0 {
		t.Errorf("routes = %d, want 0", got)
	}
}

// TestSameShapeRouteIsShadowed pins the tie-break for two patterns the database
// accepts as different rows and the matcher cannot tell apart. The first by
// path order wins; the other is logged, not served.
func TestSameShapeRouteIsShadowed(t *testing.T) {
	table := newTable(t, "GET /users/{name}", "GET /users/{id}")

	got := table.Lookup(testProject, "GET", "/users/42")
	if got.Outcome != Matched {
		t.Fatalf("outcome = %v, want Matched", got.Outcome)
	}
	if got.Route.Body != "GET /users/{id}" {
		t.Errorf("matched %q, want the first by path order, %q", got.Route.Body, "GET /users/{id}")
	}
	if got.Params["id"] != "42" {
		t.Errorf("params = %v, want the winning route's name", got.Params)
	}
}

func TestParameterNamesFollowTheRouteNotTheNode(t *testing.T) {
	// Both patterns walk through the same parameter node and disagree about
	// what it is called. The name has to come from the route that answered.
	table := newTable(t, "GET /users/{id}/posts", "GET /users/{login}/friends")

	byID := table.Lookup(testProject, "GET", "/users/7/posts")
	if got := byID.Params["id"]; got != "7" {
		t.Errorf("posts: params = %v, want id=7", byID.Params)
	}
	byLogin := table.Lookup(testProject, "GET", "/users/sam/friends")
	if got := byLogin.Params["login"]; got != "sam" {
		t.Errorf("friends: params = %v, want login=sam", byLogin.Params)
	}
}
