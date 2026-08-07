package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// The shared demo project: every built-in dataset, served at /m/demo/… to
// anybody, with no account (DESIGN.md §6). It is what `task.md` asks for when
// it asks for simple sets of data to test against without authorisation.
//
// It is an ordinary project in every respect the rest of the code can see — a
// row in `projects`, collections, documents, endpoints, a request log — which
// is deliberate: a demo that took a separate path through the server would be a
// demonstration of a different program. The only two things that mark it are
// the `is_demo` flag, which is what the scheduled reset finds it by, and an
// owner nobody can log in as.
const (
	// DemoSlug is the reserved slug the demo answers on. It has been in the
	// reserved list since M1, held for exactly this.
	DemoSlug = "demo"
	// DemoName is what the project is called, which only its owner would ever
	// see — and its owner is an account nobody can log in as.
	DemoName = "Demo"
	// DemoEmail owns the demo project. `.invalid` is reserved by RFC 6761 and
	// resolves nowhere, so this address cannot be anybody's and no mail sent to
	// it can be delivered.
	DemoEmail = "demo@restest.invalid"
)

// ErrDemoSlugTaken reports that the demo slug belongs to a project that is not
// the demo — created by hand, or by an instance running before the slug was
// reserved. Provisioning stops rather than adopting or overwriting somebody
// else's project.
var ErrDemoSlugTaken = errors.New("the demo slug belongs to a project that is not the demo")

// EnsureDemoProject returns the demo project, creating it with every built-in
// dataset the first time.
//
// It is idempotent, and safe to run from more than one instance at once: two
// processes starting together race on the unique index over the slug, and the
// one that loses reads back what the winner created. An existing demo is
// returned untouched — its *contents* are the scheduled reset's business, not
// this function's, so an operator who added an endpoint to the demo does not
// lose it on the next restart.
func (s *Store) EnsureDemoProject(ctx context.Context) (Project, error) {
	project, err := s.demoProject(ctx)
	switch {
	case err == nil:
		return project, nil
	case !errors.Is(err, ErrNotFound):
		return Project{}, err
	}

	owner, err := s.ensureDemoOwner(ctx)
	if err != nil {
		return Project{}, err
	}

	project, err = s.createProject(ctx, owner, DemoSlug, DemoName, true, Datasets())
	if errors.Is(err, ErrSlugTaken) {
		// Another instance created it between the lookup and the insert.
		return s.demoProject(ctx)
	}
	if err != nil {
		return Project{}, fmt.Errorf("create the demo project: %w", err)
	}
	return project, nil
}

// demoProject reads the project holding the demo slug, refusing to hand back
// one that is not flagged as the demo.
func (s *Store) demoProject(ctx context.Context) (Project, error) {
	row, err := s.q.ProjectBySlug(ctx, DemoSlug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Project{}, ErrNotFound
		}
		return Project{}, fmt.Errorf("find the demo project: %w", err)
	}
	if !row.IsDemo {
		return Project{}, ErrDemoSlugTaken
	}
	return toProject(row), nil
}

// ensureDemoOwner returns the account the demo project belongs to, creating it
// if this is the first start.
//
// The account exists because ownership runs user → project throughout the
// schema, and inventing a nullable owner for one project would put a null check
// on every query that reads one. Its password is random bytes that are hashed
// and then dropped on the floor: the row is a valid account that nobody,
// including whoever runs the instance, can log in as.
func (s *Store) ensureDemoOwner(ctx context.Context) (uuid.UUID, error) {
	row, err := s.q.UserByEmail(ctx, DemoEmail)
	if err == nil {
		return toUUID(row.ID), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("find the demo account: %w", err)
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return uuid.Nil, fmt.Errorf("generate the demo account password: %w", err)
	}
	hash, err := HashPassword(hex.EncodeToString(secret))
	if err != nil {
		return uuid.Nil, fmt.Errorf("hash the demo account password: %w", err)
	}

	created, err := s.q.CreateUser(ctx, dbgen.CreateUserParams{Email: DemoEmail, PasswordHash: hash})
	if err != nil {
		if uniqueViolation(err, usersEmailConstraint) {
			// Another instance got there first; its row is as good as ours.
			existing, err := s.q.UserByEmail(ctx, DemoEmail)
			if err != nil {
				return uuid.Nil, fmt.Errorf("find the demo account: %w", err)
			}
			return toUUID(existing.ID), nil
		}
		return uuid.Nil, fmt.Errorf("create the demo account: %w", err)
	}
	return toUUID(created.ID), nil
}

// ResetDemoProjects restores every collection of every demo project to its
// seed, and reports how many collections it reset.
//
// This is what keeps one visitor's experiment from spoiling the demo for the
// next one. Anonymous writes are real writes — a POST to /m/demo/users creates
// a document that the next GET returns, because a demo that only pretended to
// store things would be demonstrating something restest does not do — and this
// is what makes them temporary.
//
// Each collection is reset in its own transaction, the same one the reset
// button uses. A reset that fails part way leaves the collections it had
// already restored restored, which is the harmless direction: the next run
// finishes the job.
func (s *Store) ResetDemoProjects(ctx context.Context) (int, error) {
	ids, err := s.q.DemoCollections(ctx)
	if err != nil {
		return 0, fmt.Errorf("list the demo collections: %w", err)
	}

	var reset int
	for _, id := range ids {
		if _, err := s.ResetCollection(ctx, toUUID(id)); err != nil {
			if errors.Is(err, ErrNotFound) {
				// Deleted between the listing and the reset. Nothing to restore
				// and nothing to report.
				continue
			}
			return reset, fmt.Errorf("reset a demo collection: %w", err)
		}
		reset++
	}
	return reset, nil
}

// ResetDemoProjectsLoop resets the demo now and then every `every` until ctx is
// cancelled. It is meant to run in its own goroutine for the life of the
// process.
//
// Once at startup as well as on the timer, so that a restart is also a way to
// put the demo back the way it was, and so that an instance restarted more
// often than the interval still resets. A failure is logged and retried at the
// next tick: the demo being stale for an hour is not worth stopping anything
// over.
func (s *Store) ResetDemoProjectsLoop(ctx context.Context, every time.Duration, logger *slog.Logger) {
	run := func() {
		reset, err := s.ResetDemoProjects(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("reset the demo project", slog.String("error", err.Error()))
			}
			return
		}
		logger.Info("demo project reset", slog.Int("collections", reset))
	}
	run()

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}
