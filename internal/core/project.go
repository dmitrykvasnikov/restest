package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// Project is the unit of isolation, and the name that appears in mock URLs.
type Project struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	Slug      string
	Name      string
	IsDemo    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MockPath is where this project's mock traffic is served, relative to the host
// (DESIGN.md §4). Kept here rather than assembled in a template so that the one
// place that knows the URL shape is the one place that has to change if the
// subdomain alias is ever added.
func (p Project) MockPath() string { return "/m/" + p.Slug + "/" }

// projectsSlugConstraint is the unique index behind projects.slug. Slugs are
// global, not per-owner: they address mock traffic, and mock traffic arrives
// without an account.
const projectsSlugConstraint = "projects_slug_key"

// CreateProject validates and stores a new project owned by ownerID, optionally
// pre-seeded from the built-in datasets named in datasets (DESIGN.md §6).
//
// Each dataset becomes a collection holding its seed and an endpoint of kind
// collection rooted at /{name}, so a project created with `users` answers
// /m/{slug}/users the moment the form is submitted. An empty list creates the
// empty project it always did.
func (s *Store) CreateProject(ctx context.Context, ownerID uuid.UUID, slug, name string, datasets []string) (Project, error) {
	slug, name = normalizeProject(slug, name)

	var fe FieldErrors
	validateSlug(&fe, slug)
	validateProjectName(&fe, name)
	chosen := selectDatasets(&fe, datasets)
	if err := fe.orNil(); err != nil {
		return Project{}, err
	}
	return s.createProject(ctx, ownerID, slug, name, false, chosen)
}

// createProject is the storage half, past validation.
//
// It is separate because the demo project is created with a slug the reserved
// list refuses (validate.go) and a flag no form can set. Everything below the
// validation is the same work, and having it once is what makes the demo a
// project like any other rather than a second kind of thing.
func (s *Store) createProject(ctx context.Context, ownerID uuid.UUID, slug, name string, isDemo bool, datasets []Dataset) (Project, error) {
	// One transaction for the project and everything the datasets add to it: a
	// project that exists holding two of the three datasets it was asked for is
	// a state the interface offers no way to repair.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Project{}, fmt.Errorf("begin create project: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the commit path has already reported

	q := s.q.WithTx(tx)

	row, err := q.CreateProject(ctx, dbgen.CreateProjectParams{
		OwnerID: fromUUID(ownerID),
		Slug:    slug,
		Name:    name,
		IsDemo:  isDemo,
	})
	if err != nil {
		if uniqueViolation(err, projectsSlugConstraint) {
			return Project{}, ErrSlugTaken
		}
		return Project{}, fmt.Errorf("create project: %w", err)
	}

	project := toProject(row)
	for _, d := range datasets {
		if err := installDataset(ctx, tx, q, ownerID, project.ID, d); err != nil {
			return Project{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, fmt.Errorf("commit create project: %w", err)
	}
	return project, nil
}

// ProjectsByOwner lists everything ownerID owns, newest first.
func (s *Store) ProjectsByOwner(ctx context.Context, ownerID uuid.UUID) ([]Project, error) {
	rows, err := s.q.ProjectsByOwner(ctx, fromUUID(ownerID))
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	projects := make([]Project, len(rows))
	for i, row := range rows {
		projects[i] = toProject(row)
	}
	return projects, nil
}

// ProjectByOwnerAndSlug is the lookup behind every /projects/{slug} page. A slug
// owned by somebody else is ErrNotFound, the same answer as a slug that was
// never used: anything else would report on another account's projects.
func (s *Store) ProjectByOwnerAndSlug(ctx context.Context, ownerID uuid.UUID, slug string) (Project, error) {
	row, err := s.q.ProjectByOwnerAndSlug(ctx, dbgen.ProjectByOwnerAndSlugParams{
		OwnerID: fromUUID(ownerID),
		Slug:    strings.TrimSpace(slug),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		return Project{}, fmt.Errorf("find project: %w", err)
	}
	return toProject(row), nil
}

// UpdateProject changes the slug and name of a project the caller owns.
//
// Renaming the slug moves the project's mock URL, which will break clients
// already pointed at the old one. That is the owner's call to make: this is a
// development tool, and a slug typed wrong once should not be permanent.
func (s *Store) UpdateProject(ctx context.Context, ownerID, id uuid.UUID, slug, name string) (Project, error) {
	slug, name = normalizeProject(slug, name)

	var fe FieldErrors
	validateSlug(&fe, slug)
	validateProjectName(&fe, name)
	if err := fe.orNil(); err != nil {
		return Project{}, err
	}

	row, err := s.q.UpdateProject(ctx, dbgen.UpdateProjectParams{
		ID:      fromUUID(id),
		OwnerID: fromUUID(ownerID),
		Slug:    slug,
		Name:    name,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// No row matched both the id and the owner.
			return Project{}, ErrNotFound
		}
		if uniqueViolation(err, projectsSlugConstraint) {
			return Project{}, ErrSlugTaken
		}
		return Project{}, fmt.Errorf("update project: %w", err)
	}
	return toProject(row), nil
}

// DeleteProject removes a project the caller owns, and with it — by cascade —
// its endpoints, collections and documents.
func (s *Store) DeleteProject(ctx context.Context, ownerID, id uuid.UUID) error {
	n, err := s.q.DeleteProject(ctx, dbgen.DeleteProjectParams{
		ID:      fromUUID(id),
		OwnerID: fromUUID(ownerID),
	})
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// normalizeProject trims what a form sends and lower-cases the slug, so that
// "  My-API " and "my-api" are not two different rejections of the same intent.
func normalizeProject(slug, name string) (string, string) {
	return strings.ToLower(strings.TrimSpace(slug)), strings.TrimSpace(name)
}

func toProject(row dbgen.Project) Project {
	return Project{
		ID:        toUUID(row.ID),
		OwnerID:   toUUID(row.OwnerID),
		Slug:      row.Slug,
		Name:      row.Name,
		IsDemo:    row.IsDemo,
		CreatedAt: toTime(row.CreatedAt),
		UpdatedAt: toTime(row.UpdatedAt),
	}
}
