package mock

import (
	"log/slog"
	"strings"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// Table is an immutable snapshot of every route the mock server serves.
//
// Immutable is the whole design. Building a new one and swapping it in means a
// request in flight keeps reading the table it started with, and the lock is
// held only for the pointer swap rather than for the length of a match.
type Table struct {
	projects map[string]*projectRoutes
}

type projectRoutes struct {
	project core.MockProject
	trie    trie
	// refs is every route this project defines, in the order the query returned
	// them, for the suggestions in a 404 body.
	refs []Ref
}

// BuildTable assembles the route table from a snapshot of the database.
//
// Endpoints whose project is missing are dropped: MockData reads projects and
// endpoints in two statements, so a project deleted between them leaves rows
// with nowhere to go. The next rebuild settles it; in the meantime the routes
// are simply not served, which is what the deletion asked for anyway.
func BuildTable(data core.MockData, logger *slog.Logger) *Table {
	t := &Table{projects: make(map[string]*projectRoutes, len(data.Projects))}
	for _, p := range data.Projects {
		t.projects[p.Slug] = &projectRoutes{project: p}
	}

	for _, endpoint := range data.Endpoints {
		routes, ok := t.projects[endpoint.ProjectSlug]
		if !ok {
			continue
		}

		// One row is not one route: a collection endpoint expands into the six
		// that make up REST semantics over its documents. The trie is inserted
		// into one route at a time either way, which is why the expansion lives
		// here and not inside it.
		for _, route := range expand(endpoint) {
			if shadowed := routes.trie.insert(route); shadowed != nil {
				// Two patterns of the same shape and verb — /users/{id} and
				// /users/{name}. The database compares the text and sees two
				// different rows; the matcher sees one route and would have to
				// pick. The first one in wins, and the other is named here
				// rather than disappearing without a word.
				logger.Warn("endpoint is shadowed by another of the same shape",
					slog.String("project", endpoint.ProjectSlug),
					slog.String("method", route.Method),
					slog.String("shadowed", route.Path),
					slog.String("serving", shadowed.Path),
				)
			}
			routes.refs = append(routes.refs, Ref{Method: route.Method, Path: route.Path})
		}
	}
	return t
}

// Lookup answers what should happen to a request for path, addressed to the
// project with this slug.
//
// path is the *escaped* path below the project prefix — everything after
// /m/{slug} — because %2F has to stay inside the segment it was written in
// (see splitRequestPath).
func (t *Table) Lookup(slug, method, path string) Result {
	routes, ok := t.projects[slug]
	if !ok {
		return Result{Outcome: NoProject}
	}

	segments := splitRequestPath(path)
	route, params, allow := routes.trie.lookup(method, segments)

	switch {
	case route != nil:
		return Result{
			Outcome: Matched,
			Project: routes.project,
			Route:   *route,
			Params:  params,
		}
	case len(allow) > 0:
		return Result{
			Outcome: WrongMethod,
			Project: routes.project,
			Allow:   allow,
		}
	default:
		// The suggestions are ranked against the decoded path, which is the
		// form the patterns are written in — comparing %7Bid%7D to {id} would
		// score a route the user is looking at as unlike what they typed.
		return Result{
			Outcome: NoRoute,
			Project: routes.project,
			Nearest: nearest(routes.refs, method, "/"+strings.Join(segments, "/"), nearestLimit),
		}
	}
}

// Routes reports how many routes the table holds, for the line logged after a
// rebuild. A table that suddenly holds none is worth being able to see.
func (t *Table) Routes() int {
	var n int
	for _, routes := range t.projects {
		n += len(routes.refs)
	}
	return n
}

// Projects reports how many projects the table knows about.
func (t *Table) Projects() int { return len(t.projects) }
