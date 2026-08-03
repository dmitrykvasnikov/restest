package mock

import (
	"cmp"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// testProject is the slug every table in these tests is built under.
const testProject = "shop"

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

// newTable builds a route table from patterns written the way they would be
// read out of a route list: "GET /users/{id}".
//
// Endpoints arrive from the database ordered by path and then method, and the
// order decides which of two same-shape routes wins, so the helper sorts the
// same way rather than trusting the order a test happened to list them in.
func newTable(t *testing.T, patterns ...string) *Table {
	t.Helper()
	return newTableFor(t, testProject, patterns...)
}

func newTableFor(t *testing.T, slug string, patterns ...string) *Table {
	t.Helper()

	project := core.MockProject{ID: uuid.New(), Slug: slug}
	data := core.MockData{Projects: []core.MockProject{project}}

	for _, pattern := range patterns {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Fatalf("route %q: want %q", pattern, "METHOD /path")
		}
		data.Endpoints = append(data.Endpoints, core.MockEndpoint{
			ProjectSlug: slug,
			Endpoint: core.Endpoint{
				ID:         uuid.New(),
				ProjectID:  project.ID,
				Method:     core.NormalizeMethod(method),
				Path:       core.NormalizePath(path),
				Kind:       core.KindStatic,
				IsEnabled:  true,
				StatusCode: 200,
				Body:       pattern, // so a test can tell which route answered
			},
		})
	}
	sortLikeTheQuery(data.Endpoints)

	return BuildTable(data, discardLogger())
}

// sortLikeTheQuery mirrors `order by path_pattern, method` in MockEndpoints.
func sortLikeTheQuery(endpoints []core.MockEndpoint) {
	slices.SortFunc(endpoints, func(a, b core.MockEndpoint) int {
		return cmp.Or(
			strings.Compare(a.Path, b.Path),
			strings.Compare(a.Method, b.Method),
		)
	})
}
