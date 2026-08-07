//go:build integration

package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// newStore is the shorthand these tests start from: a migrated database and a
// store over it.
func newStore(t *testing.T) *core.Store {
	t.Helper()

	store, _ := newApp(t, migratedPool(t, startPostgres(t)))
	return store
}

const testPassword = "correct horse battery staple"

func TestRegisterAndAuthenticate(t *testing.T) {
	store := newStore(t)

	user, err := store.RegisterUser(t.Context(), "Sam@Example.COM ", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	// The address is normalised on the way in, so it is stored the way it will
	// be compared.
	if user.Email != "sam@example.com" {
		t.Errorf("stored email = %q, want it lower-cased and trimmed", user.Email)
	}
	if user.ID == uuid.Nil {
		t.Error("the new user has no id")
	}

	// citext means the login is case-insensitive in the database, not in Go.
	got, err := store.Authenticate(t.Context(), "SAM@example.com", testPassword)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != user.ID {
		t.Errorf("authenticated %v, want %v", got.ID, user.ID)
	}
}

// The password must not be recoverable from the table, which is the only reason
// the hashing exists.
func TestPasswordIsStoredAsAnArgon2idHash(t *testing.T) {
	pool := migratedPool(t, startPostgres(t))
	store, _ := newApp(t, pool)

	if _, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword); err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	var hash string
	if err := pool.QueryRow(t.Context(),
		"select password_hash from users where email = 'sam@example.com'").Scan(&hash); err != nil {
		t.Fatalf("read hash: %v", err)
	}

	if strings.Contains(hash, testPassword) {
		t.Fatal("the stored value contains the password")
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("stored hash = %q, want the tuned Argon2id parameters", hash)
	}
}

func TestAuthenticateRejectsWrongCredentials(t *testing.T) {
	store := newStore(t)

	if _, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword); err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	tests := []struct {
		name     string
		email    string
		password string
	}{
		{"wrong password", "sam@example.com", "not the password"},
		{"unknown address", "nobody@example.com", testPassword},
		{"empty password", "sam@example.com", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.Authenticate(t.Context(), tt.email, tt.password)
			// The same error for both, so the caller cannot tell an unknown
			// address from a wrong password.
			if !errors.Is(err, core.ErrInvalidCredentials) {
				t.Errorf("error = %v, want ErrInvalidCredentials", err)
			}
		})
	}
}

// The unique index settles the race, so the second registration of an address
// is an ordinary rejection rather than a constraint error escaping upwards.
func TestRegisterRejectsADuplicateAddress(t *testing.T) {
	store := newStore(t)

	if _, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword); err != nil {
		t.Fatalf("first RegisterUser: %v", err)
	}

	// Different case, because citext makes it the same address.
	_, err := store.RegisterUser(t.Context(), "SAM@example.com", "a different password")
	if !errors.Is(err, core.ErrEmailTaken) {
		t.Errorf("error = %v, want ErrEmailTaken", err)
	}
}

func TestRegisterRejectsInvalidInput(t *testing.T) {
	store := newStore(t)

	_, err := store.RegisterUser(t.Context(), "not-an-address", "short")

	var fe core.FieldErrors
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want FieldErrors", err)
	}
	if fe["email"] == "" {
		t.Error("nothing said about the address")
	}
	if fe["password"] == "" {
		t.Error("nothing said about the password")
	}
}

func TestProjectLifecycle(t *testing.T) {
	store := newStore(t)

	owner, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	project, err := store.CreateProject(t.Context(), owner.ID, " Checkout ", "  Checkout API  ", nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Both fields are trimmed and the slug lower-cased, so "  Checkout " and
	// "checkout" are not two different rejections of the same intent.
	if project.Slug != "checkout" || project.Name != "Checkout API" {
		t.Errorf("stored slug %q and name %q", project.Slug, project.Name)
	}
	if project.MockPath() != "/m/checkout/" {
		t.Errorf("MockPath = %q, want /m/checkout/", project.MockPath())
	}

	found, err := store.ProjectByOwnerAndSlug(t.Context(), owner.ID, "checkout")
	if err != nil {
		t.Fatalf("ProjectByOwnerAndSlug: %v", err)
	}
	if found.ID != project.ID {
		t.Errorf("found %v, want %v", found.ID, project.ID)
	}

	updated, err := store.UpdateProject(t.Context(), owner.ID, project.ID, "payments", "Payments API")
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.Slug != "payments" || updated.Name != "Payments API" {
		t.Errorf("after update: slug %q, name %q", updated.Slug, updated.Name)
	}
	if _, err := store.ProjectByOwnerAndSlug(t.Context(), owner.ID, "checkout"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("the old slug still resolves: %v", err)
	}

	if err := store.DeleteProject(t.Context(), owner.ID, project.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if err := store.DeleteProject(t.Context(), owner.ID, project.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("deleting twice: %v, want ErrNotFound", err)
	}

	list, err := store.ProjectsByOwner(t.Context(), owner.ID)
	if err != nil {
		t.Fatalf("ProjectsByOwner: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("owner still has %d projects", len(list))
	}
}

// Slugs are global because they address mock traffic, which arrives without an
// account. Two owners cannot share one.
func TestSlugsAreGlobal(t *testing.T) {
	store := newStore(t)

	first, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	second, err := store.RegisterUser(t.Context(), "alex@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	if _, err := store.CreateProject(t.Context(), first.ID, "checkout", "Checkout", nil); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	_, err = store.CreateProject(t.Context(), second.ID, "checkout", "Also checkout", nil)
	if !errors.Is(err, core.ErrSlugTaken) {
		t.Errorf("error = %v, want ErrSlugTaken", err)
	}
}

// A project belonging to somebody else must be indistinguishable from one that
// does not exist, and the ownership test is part of the query rather than a
// check a caller could forget.
func TestAnotherOwnersProjectIsNotFound(t *testing.T) {
	store := newStore(t)

	owner, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	stranger, err := store.RegisterUser(t.Context(), "alex@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	project, err := store.CreateProject(t.Context(), owner.ID, "checkout", "Checkout", nil)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := store.ProjectByOwnerAndSlug(t.Context(), stranger.ID, "checkout"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("read: %v, want ErrNotFound", err)
	}
	if _, err := store.UpdateProject(t.Context(), stranger.ID, project.ID, "hijacked", "Hijacked"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("update: %v, want ErrNotFound", err)
	}
	if err := store.DeleteProject(t.Context(), stranger.ID, project.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("delete: %v, want ErrNotFound", err)
	}

	// And the project is still there afterwards.
	if _, err := store.ProjectByOwnerAndSlug(t.Context(), owner.ID, "checkout"); err != nil {
		t.Errorf("the owner lost their project: %v", err)
	}
}

// CONTEXT.md §7: the constraints are exercised with deliberately invalid
// inserts. These go straight to the database, past the Go validation, to prove
// that the two agree — a value the Go rule rejects must not be one the schema
// would have accepted, and vice versa.
func TestSchemaConstraintsMatchTheGoRules(t *testing.T) {
	pool := migratedPool(t, startPostgres(t))
	store, _ := newApp(t, pool)

	owner, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}

	badSlugs := []string{"", "-mock", "mock-", "my_mock", "my mock", "my.mock", "макет", strings.Repeat("a", 41)}
	for _, slug := range badSlugs {
		t.Run("slug "+slug, func(t *testing.T) {
			// The database refuses it...
			_, err := pool.Exec(t.Context(),
				"insert into projects (owner_id, slug, name) values ($1, $2, 'Direct')",
				owner.ID, slug)
			if err == nil {
				t.Errorf("the schema accepted slug %q that validateSlug rejects", slug)
			}

			// ...and so does the store, before it ever gets there.
			if _, err := store.CreateProject(t.Context(), owner.ID, slug, "Through the store", nil); err == nil {
				t.Errorf("CreateProject accepted slug %q", slug)
			}
		})
	}

	// The other direction: a slug at the limit of the Go rule is one the schema
	// takes, so the two are not merely both strict but strict about the same
	// thing.
	longest := strings.Repeat("a", 40)
	if _, err := store.CreateProject(t.Context(), owner.ID, longest, "Longest allowed", nil); err != nil {
		t.Errorf("a 40-character slug was rejected: %v", err)
	}

	// Case and surrounding whitespace are normalised rather than rejected: the
	// constraint applies to what is stored, and what a user types with the
	// shift key held down still means the slug they meant.
	project, err := store.CreateProject(t.Context(), owner.ID, "  MyAPI ", "Mixed case", nil)
	if err != nil {
		t.Fatalf("CreateProject with a mixed-case slug: %v", err)
	}
	if project.Slug != "myapi" {
		t.Errorf("stored slug = %q, want it normalised to myapi", project.Slug)
	}
}

// Deleting an account takes its projects with it, which is what the on delete
// cascade in the schema is for.
func TestDeletingAUserCascadesToProjects(t *testing.T) {
	pool := migratedPool(t, startPostgres(t))
	store, _ := newApp(t, pool)

	owner, err := store.RegisterUser(t.Context(), "sam@example.com", testPassword)
	if err != nil {
		t.Fatalf("RegisterUser: %v", err)
	}
	if _, err := store.CreateProject(t.Context(), owner.ID, "checkout", "Checkout", nil); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	if _, err := pool.Exec(t.Context(), "delete from users where id = $1", owner.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	var projects int
	if err := pool.QueryRow(t.Context(), "select count(*) from projects").Scan(&projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projects != 0 {
		t.Errorf("%d projects outlived their owner", projects)
	}
}
