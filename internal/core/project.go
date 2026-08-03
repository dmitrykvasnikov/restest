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

// CreateProject validates and stores a new project owned by ownerID.
func (s *Store) CreateProject(ctx context.Context, ownerID uuid.UUID, slug, name string) (Project, error) {
	slug, name = normalizeProject(slug, name)

	var fe FieldErrors
	validateSlug(&fe, slug)
	validateProjectName(&fe, name)
	if err := fe.orNil(); err != nil {
		return Project{}, err
	}

	row, err := s.q.CreateProject(ctx, dbgen.CreateProjectParams{
		OwnerID: fromUUID(ownerID),
		Slug:    slug,
		Name:    name,
	})
	if err != nil {
		if uniqueViolation(err, projectsSlugConstraint) {
			return Project{}, ErrSlugTaken
		}
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return toProject(row), nil
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
