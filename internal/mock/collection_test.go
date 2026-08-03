package mock

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// collectionTable builds a table holding one collection endpoint rooted at root,
// plus any static patterns alongside it.
func collectionTable(t *testing.T, collectionID uuid.UUID, root string, static ...string) *Table {
	t.Helper()

	project := core.MockProject{ID: uuid.New(), Slug: testProject}
	data := core.MockData{Projects: []core.MockProject{project}}

	data.Endpoints = append(data.Endpoints, core.MockEndpoint{
		ProjectSlug: testProject,
		Endpoint: core.Endpoint{
			ID:           uuid.New(),
			ProjectID:    project.ID,
			Method:       core.MethodAny,
			Path:         core.NormalizePath(root),
			Kind:         core.KindCollection,
			IsEnabled:    true,
			CollectionID: collectionID,
		},
	})
	for _, pattern := range static {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("route %q: want %q", pattern, "METHOD /path")
		}
		data.Endpoints = append(data.Endpoints, core.MockEndpoint{
			ProjectSlug: testProject,
			Endpoint: core.Endpoint{
				ID:         uuid.New(),
				ProjectID:  project.ID,
				Method:     core.NormalizeMethod(method),
				Path:       core.NormalizePath(path),
				Kind:       core.KindStatic,
				IsEnabled:  true,
				StatusCode: 200,
				Body:       pattern,
			},
		})
	}
	sortLikeTheQuery(data.Endpoints)

	return BuildTable(data, discardLogger())
}

// One endpoint row becomes six routes. This is the whole of what M3 asked the
// matcher for, and the table below is the specification of it.
func TestCollectionExpandsIntoSixRoutes(t *testing.T) {
	collectionID := uuid.New()
	table := collectionTable(t, collectionID, "/users")

	tests := []struct {
		method string
		path   string
		wantOp Op
		wantID string
	}{
		{method: "GET", path: "/users", wantOp: OpList},
		{method: "POST", path: "/users", wantOp: OpCreate},
		{method: "GET", path: "/users/7", wantOp: OpGet, wantID: "7"},
		{method: "PUT", path: "/users/7", wantOp: OpReplace, wantID: "7"},
		{method: "PATCH", path: "/users/7", wantOp: OpPatch, wantID: "7"},
		{method: "DELETE", path: "/users/7", wantOp: OpDelete, wantID: "7"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			result := table.Lookup(testProject, tt.method, tt.path)

			if result.Outcome != Matched {
				t.Fatalf("outcome = %v, want Matched", result.Outcome)
			}
			if result.Route.Op != tt.wantOp {
				t.Errorf("op = %d, want %d", result.Route.Op, tt.wantOp)
			}
			if result.Route.CollectionID != collectionID {
				t.Errorf("collection = %s, want %s", result.Route.CollectionID, collectionID)
			}
			if got := result.Params[DocumentParam]; got != tt.wantID {
				t.Errorf("%s = %q, want %q", DocumentParam, got, tt.wantID)
			}
		})
	}
}

// The routes are real routes, so everything the matcher already did keeps
// working over them: an undefined verb is 405 with the verbs that are defined,
// and the list is the union across both shapes rather than the first one found.
func TestCollectionMethodFallthrough(t *testing.T) {
	table := collectionTable(t, uuid.New(), "/users")

	tests := []struct {
		name      string
		method    string
		path      string
		wantAllow []string
	}{
		{
			name: "no PUT on the collection itself", method: "PUT", path: "/users",
			wantAllow: []string{"GET", "HEAD", "POST"},
		},
		{
			name: "no POST to one document", method: "POST", path: "/users/7",
			wantAllow: []string{"GET", "HEAD", "PUT", "PATCH", "DELETE"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := table.Lookup(testProject, tt.method, tt.path)

			if result.Outcome != WrongMethod {
				t.Fatalf("outcome = %v, want WrongMethod", result.Outcome)
			}
			if !slices.Equal(result.Allow, tt.wantAllow) {
				t.Errorf("Allow = %v, want %v", result.Allow, tt.wantAllow)
			}
		})
	}
}

// HEAD falls back to GET on a collection route as it does anywhere else, and
// the op that answers is the one GET would have run.
func TestCollectionHeadFallsBackToGet(t *testing.T) {
	table := collectionTable(t, uuid.New(), "/users")

	for _, tt := range []struct {
		path   string
		wantOp Op
	}{
		{path: "/users", wantOp: OpList},
		{path: "/users/7", wantOp: OpGet},
	} {
		result := table.Lookup(testProject, http.MethodHead, tt.path)
		if result.Outcome != Matched || result.Route.Op != tt.wantOp {
			t.Errorf("HEAD %s = outcome %v op %d, want Matched and op %d",
				tt.path, result.Outcome, result.Route.Op, tt.wantOp)
		}
	}
}

// A static route on a path the collection also claims is a real conflict, and
// the existing rule settles it: the first by (path, method) wins and the other
// is reported as shadowed. What matters here is that it is deterministic and
// that the collection has not quietly swallowed a literal that sits below it.
func TestCollectionAlongsideStaticRoutes(t *testing.T) {
	table := collectionTable(t, uuid.New(), "/users", "GET /users/me", "GET /health")

	// A literal segment still outranks the parameter route the expansion added.
	result := table.Lookup(testProject, "GET", "/users/me")
	if result.Outcome != Matched {
		t.Fatalf("GET /users/me: outcome = %v, want Matched", result.Outcome)
	}
	if result.Route.Op != OpRespond {
		t.Errorf("GET /users/me answered with op %d, want the static route", result.Route.Op)
	}

	// And a route somewhere else entirely is untouched.
	if result := table.Lookup(testProject, "GET", "/health"); result.Route.Op != OpRespond {
		t.Errorf("GET /health answered with op %d, want the static route", result.Route.Op)
	}

	// The parameter route is still there for anything that is not the literal.
	if result := table.Lookup(testProject, "GET", "/users/7"); result.Route.Op != OpGet {
		t.Errorf("GET /users/7 answered with op %d, want OpGet", result.Route.Op)
	}
}

// A collection rooted below a parameter is allowed: /tenants/{tenant}/users is
// a legitimate place to put one. The parameter is matched and collected, and it
// names nothing the collection reads — state is per collection, not per
// parameter value (DESIGN.md §12.1).
func TestCollectionUnderAParameter(t *testing.T) {
	table := collectionTable(t, uuid.New(), "/tenants/{tenant}/users")

	result := table.Lookup(testProject, "GET", "/tenants/acme/users/7")
	if result.Outcome != Matched || result.Route.Op != OpGet {
		t.Fatalf("outcome = %v op = %d, want Matched and OpGet", result.Outcome, result.Route.Op)
	}
	if result.Params["tenant"] != "acme" {
		t.Errorf("tenant = %q, want acme", result.Params["tenant"])
	}
	if result.Params[DocumentParam] != "7" {
		t.Errorf("%s = %q, want 7", DocumentParam, result.Params[DocumentParam])
	}
}

// A collection rooted at the project root serves /m/{slug}/ and /m/{slug}/{id},
// which is the one case where the path juggling could produce a doubled slash.
func TestCollectionAtTheRoot(t *testing.T) {
	table := collectionTable(t, uuid.New(), "/")

	if result := table.Lookup(testProject, "GET", "/"); result.Route.Op != OpList {
		t.Errorf("GET / answered with op %d, want OpList", result.Route.Op)
	}
	result := table.Lookup(testProject, "GET", "/7")
	if result.Route.Op != OpGet || result.Params[DocumentParam] != "7" {
		t.Errorf("GET /7 = op %d params %v, want OpGet with id 7", result.Route.Op, result.Params)
	}
}

// All six routes appear in the suggestion list, because a client that mistyped
// /userz should be told about every verb /users answers.
func TestCollectionRoutesAreSuggested(t *testing.T) {
	table := collectionTable(t, uuid.New(), "/users")

	result := table.Lookup(testProject, "GET", "/userz")
	if result.Outcome != NoRoute {
		t.Fatalf("outcome = %v, want NoRoute", result.Outcome)
	}
	if len(result.Nearest) == 0 {
		t.Fatal("nothing was suggested for a one-character typo")
	}

	var sawRoot bool
	for _, ref := range result.Nearest {
		if ref.Path == "/users" {
			sawRoot = true
		}
		if ref.Method == core.MethodAny {
			t.Errorf("suggestion %v names the wildcard the row is stored under, not a route", ref)
		}
	}
	if !sawRoot {
		t.Errorf("suggestions %v do not include /users", result.Nearest)
	}
}

// Disabled endpoints are left out of the query that feeds the table, so a
// disabled collection endpoint contributes none of its six routes. The table is
// built from what MockData returned, and this is the shape of that promise.
func TestExpandStaticEndpointIsOneRoute(t *testing.T) {
	routes := expand(core.MockEndpoint{
		Endpoint: core.Endpoint{
			Method: "GET", Path: "/users/{id}", Kind: core.KindStatic, StatusCode: 200,
		},
	})

	if len(routes) != 1 {
		t.Fatalf("a static endpoint expanded into %d routes, want 1", len(routes))
	}
	if routes[0].Op != OpRespond {
		t.Errorf("op = %d, want OpRespond", routes[0].Op)
	}
	if !slices.Equal(routes[0].Params, []string{"id"}) {
		t.Errorf("params = %v, want [id]", routes[0].Params)
	}
}

// Each expanded route carries its own method and path rather than the wildcard
// and root of the row it came from. A log line or a 404 body naming "* /users"
// would describe something no client can send.
func TestExpandRewritesMethodAndPath(t *testing.T) {
	routes := expand(core.MockEndpoint{
		Endpoint: core.Endpoint{
			Method: core.MethodAny, Path: "/users", Kind: core.KindCollection,
			CollectionID: uuid.New(),
		},
	})

	if len(routes) != 6 {
		t.Fatalf("expanded into %d routes, want 6", len(routes))
	}
	for _, route := range routes {
		if route.Method == core.MethodAny {
			t.Errorf("route %+v kept the wildcard verb", route)
		}
		if route.Path != "/users" && route.Path != "/users/{id}" {
			t.Errorf("route path = %q, want /users or /users/{id}", route.Path)
		}
	}
}
