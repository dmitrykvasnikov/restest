package mock

import (
	"slices"
	"testing"
)

func TestNearestSuggestions(t *testing.T) {
	table := newTable(t,
		"GET /users",
		"POST /users",
		"GET /users/{id}",
		"GET /users/{id}/posts",
		"GET /orders",
		"GET /orders/{id}/lines",
		"GET /health",
	)

	tests := []struct {
		name   string
		method string
		path   string
		// first is what the top suggestion must be. The rest of the list is
		// deliberately not pinned: it is a hint, and over-specifying it would
		// make the ranking impossible to improve.
		first Ref
	}{
		{
			name:   "a mistyped leaf keeps the prefix it got right",
			method: "GET", path: "/users/7/postz",
			first: Ref{Method: "GET", Path: "/users/{id}/posts"},
		},
		{
			name:   "a mistyped first segment falls back to edit distance",
			method: "GET", path: "/user",
			first: Ref{Method: "GET", Path: "/users"},
		},
		{
			name:   "a path one segment too deep points at its parent",
			method: "GET", path: "/orders/7/lines/3",
			first: Ref{Method: "GET", Path: "/orders/{id}/lines"},
		},
		{
			name:   "the verb asked for breaks a tie",
			method: "POST", path: "/user",
			first: Ref{Method: "POST", Path: "/users"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := table.Lookup(testProject, tc.method, tc.path)

			if got.Outcome != NoRoute {
				t.Fatalf("%s %s: outcome = %v, want NoRoute", tc.method, tc.path, got.Outcome)
			}
			if len(got.Nearest) == 0 {
				t.Fatalf("%s %s: no suggestions", tc.method, tc.path)
			}
			if got.Nearest[0] != tc.first {
				t.Errorf("%s %s: nearest[0] = %v, want %v", tc.method, tc.path, got.Nearest[0], tc.first)
			}
		})
	}
}

func TestNearestIsCapped(t *testing.T) {
	patterns := []string{
		"GET /a", "GET /b", "GET /c", "GET /d", "GET /e", "GET /f", "GET /g",
	}
	table := newTable(t, patterns...)

	got := table.Lookup(testProject, "GET", "/z")
	if len(got.Nearest) != nearestLimit {
		t.Fatalf("nearest = %d suggestions, want %d", len(got.Nearest), nearestLimit)
	}
}

// TestNearestIsStable guards the thing a suggestion list quietly gets wrong:
// ranking off a Go map, so the same 404 answers differently twice.
func TestNearestIsStable(t *testing.T) {
	table := newTable(t,
		"GET /alpha", "GET /beta", "GET /gamma", "GET /delta", "GET /epsilon", "GET /zeta",
	)

	first := table.Lookup(testProject, "GET", "/theta").Nearest
	for range 20 {
		if got := table.Lookup(testProject, "GET", "/theta").Nearest; !slices.Equal(got, first) {
			t.Fatalf("nearest = %v, want the same list every time: %v", got, first)
		}
	}
}

func TestEditDistance(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"/users", "/user", 1},
		{"/users", "/uzers", 1},
		{"/users", "/usres", 2},
		{"/users/1", "/users/{id}", 4},
		{"élise", "elise", 1}, // counted in runes, not bytes
	}

	for _, tc := range tests {
		if got := editDistance(tc.a, tc.b); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		if got := editDistance(tc.b, tc.a); got != tc.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d — it must be symmetric", tc.b, tc.a, got, tc.want)
		}
	}
}

func TestCommonPrefix(t *testing.T) {
	tests := []struct {
		wanted  []string
		pattern []string
		want    int
	}{
		{nil, nil, 0},
		{[]string{"users"}, []string{"users"}, 1},
		{[]string{"users"}, []string{"orders"}, 0},
		{[]string{"users", "7"}, []string{"users", "{id}"}, 2},
		{[]string{"users", "7", "postz"}, []string{"users", "{id}", "posts"}, 2},
		{[]string{"users", "7"}, []string{"users", "{id}", "posts"}, 2},
	}

	for _, tc := range tests {
		if got := commonPrefix(tc.wanted, tc.pattern); got != tc.want {
			t.Errorf("commonPrefix(%v, %v) = %d, want %d", tc.wanted, tc.pattern, got, tc.want)
		}
	}
}
