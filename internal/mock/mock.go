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
	"strings"

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
//
// One endpoint row is not one route. A collection endpoint expands into six of
// them, each carrying the same definition and a different Op; Method and Path on
// the embedded endpoint are rewritten to the expanded route's own, so that a
// suggestion list and a log line name the route that answered rather than the
// row it came from.
type Route struct {
	core.MockEndpoint
	Params []string
	// Op is what a matched request should do. It is OpRespond for a static
	// endpoint and one of the five collection operations otherwise.
	Op Op
}

// Op is the operation a matched route performs.
//
// It is decided here, at match time, rather than by the handler looking at the
// method again. The mapping from verb and shape to operation is part of what the
// route table knows — POST /users and PUT /users/{id} are different routes, not
// one route with a switch after it.
type Op int

const (
	// OpRespond writes the endpoint's stored response. It is the only operation
	// a static endpoint has, and the zero value, so a route built without
	// thinking about collections behaves as it always did.
	OpRespond Op = iota
	// OpList answers GET on the collection root.
	OpList
	// OpCreate answers POST on the collection root.
	OpCreate
	// OpGet answers GET on a single document.
	OpGet
	// OpReplace answers PUT on a single document.
	OpReplace
	// OpPatch answers PATCH on a single document.
	OpPatch
	// OpDelete answers DELETE on a single document.
	OpDelete
)

// DocumentParam is the parameter the expansion of a collection endpoint uses
// for the identifier of a single document. A collection path is refused at
// definition time if it already declares one (core.validateCollectionPath), so
// the name is always the expansion's.
const DocumentParam = "id"

// collectionOps is the route set one collection endpoint expands into: the
// four verbs of a single document, and the two of the collection itself.
//
// Six routes rather than one route with six behaviours, because that is what
// makes the rest of the matcher work unchanged — /users/me still beats
// /users/{id} whether the parameter route came from a static endpoint or from
// this table, and a DELETE on the root still answers 405 with the verbs that
// are defined, because no route claims it.
var collectionOps = []struct {
	method   string
	document bool
	op       Op
}{
	{method: "GET", op: OpList},
	{method: "POST", op: OpCreate},
	{method: "GET", document: true, op: OpGet},
	{method: "PUT", document: true, op: OpReplace},
	{method: "PATCH", document: true, op: OpPatch},
	{method: "DELETE", document: true, op: OpDelete},
}

// expand turns one endpoint row into the routes it serves.
func expand(endpoint core.MockEndpoint) []*Route {
	if endpoint.Kind != core.KindCollection {
		return []*Route{{
			MockEndpoint: endpoint,
			Params:       core.PathParams(endpoint.Path),
			Op:           OpRespond,
		}}
	}

	root := endpoint.Path
	document := strings.TrimSuffix(root, "/") + "/{" + DocumentParam + "}"

	routes := make([]*Route, 0, len(collectionOps))
	for _, spec := range collectionOps {
		route := endpoint
		route.Method = spec.method
		route.Path = root
		if spec.document {
			route.Path = document
		}
		routes = append(routes, &Route{
			MockEndpoint: route,
			Params:       core.PathParams(route.Path),
			Op:           spec.op,
		})
	}
	return routes
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
