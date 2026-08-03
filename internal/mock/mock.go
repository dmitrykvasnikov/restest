// Package mock is the inbound half of restest: it decides which endpoint, if
// any, answers a request that arrived under /m/{slug}/.
//
// Nothing here speaks HTTP. The matcher takes a method and a path and returns a
// decision; writing that decision onto the wire belongs to internal/web. The
// separation is what lets the matcher be tested as a table of inputs and
// expected outcomes rather than through a server.
//
// This package depends only on internal/core, and internal/runner — phase 2,
// not built — will do the same without either importing the other
// (DESIGN.md §10).
package mock

import (
	"github.com/dmitrykvasnikov/restest/internal/core"
)

// Route is one matchable endpoint: its definition, plus the parameter names its
// pattern declares.
//
// The names live on the route rather than on the trie node they were read from,
// because two patterns may share a position and disagree about what it is
// called — /users/{id} and /users/{name} walk through the same node. The
// matcher collects the values it passed and zips them with the names of
// whichever route it finally reached.
type Route struct {
	core.MockEndpoint
	Params []string
}

// Ref names a route without carrying its definition, for the "did you mean"
// list in a 404 body.
type Ref struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Outcome is what the matcher concluded. The four cases are deliberately
// distinct: they are four different answers to give a caller, and collapsing
// any two of them would be throwing away the thing that makes the answer
// useful.
type Outcome int

const (
	// Matched — an endpoint answers this method at this path.
	Matched Outcome = iota + 1
	// NoProject — no project has this slug.
	NoProject
	// NoRoute — the project exists, but nothing is defined at this path.
	NoRoute
	// WrongMethod — the path is defined, but not for this verb.
	WrongMethod
)

// Result is a matcher decision. Which fields are set depends on Outcome:
// Matched fills Route and Params, WrongMethod fills Allow, NoRoute fills
// Nearest, and NoProject fills none of them.
type Result struct {
	Outcome Outcome
	Project core.MockProject

	Route  Route
	Params map[string]string

	// Allow is the verbs this path does answer, sorted, for the Allow header
	// that has to accompany a 405.
	Allow []string

	// Nearest is a handful of the project's routes that look most like what was
	// asked for. The common cause of a 404 here is a typo, and a bare 404 makes
	// the user go and look the route up themselves.
	Nearest []Ref
}
