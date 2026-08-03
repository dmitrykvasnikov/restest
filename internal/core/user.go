package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// User is an account. The password hash is deliberately not a field: it is read
// inside this package and compared here, and nothing above ever holds it.
type User struct {
	ID        uuid.UUID
	Email     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// usersEmailConstraint is the unique index behind users.email. Registering a
// duplicate address is an ordinary outcome of a form, not a failure, so it is
// recognised by name and turned into ErrEmailTaken.
const usersEmailConstraint = "users_email_key"

// RegisterUser validates the input, hashes the password and creates the
// account. Validation failures come back as FieldErrors; an address that is
// already registered comes back as ErrEmailTaken.
func (s *Store) RegisterUser(ctx context.Context, email, password string) (User, error) {
	email = NormalizeEmail(email)

	var fe FieldErrors
	validateEmail(&fe, email)
	validatePassword(&fe, password)
	if err := fe.orNil(); err != nil {
		return User{}, err
	}

	hash, err := HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	// No prior "does this address exist" query: between that check and this
	// insert a second registration could land, and the unique constraint is the
	// only thing that actually settles the race.
	row, err := s.q.CreateUser(ctx, dbgen.CreateUserParams{Email: email, PasswordHash: hash})
	if err != nil {
		if uniqueViolation(err, usersEmailConstraint) {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return toUser(row), nil
}

// Authenticate returns the user when the password is right. Every failure —
// unknown address, wrong password — is ErrInvalidCredentials, so that the login
// form cannot be used to find out who has an account here.
func (s *Store) Authenticate(ctx context.Context, email, password string) (User, error) {
	row, err := s.q.UserByEmail(ctx, NormalizeEmail(email))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Verify against a throwaway hash anyway. Without this an unknown
			// address answers in a millisecond and a known one takes fifty,
			// which is the same enumeration oracle by a different route.
			_, _ = VerifyPassword(decoyHash(), password)
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("find user by email: %w", err)
	}

	ok, err := VerifyPassword(row.PasswordHash, password)
	if err != nil {
		// A hash we cannot parse is our corruption, not a wrong password. It
		// must not be reported as a failed login, or the real problem stays
		// hidden behind a user who believes they are typing it wrong.
		return User{}, fmt.Errorf("verify password for %s: %w", row.ID, err)
	}
	if !ok {
		return User{}, ErrInvalidCredentials
	}
	return toUser(row), nil
}

// UserByID resolves the id carried in the session. A session naming a user who
// has since been deleted yields ErrNotFound, which the caller treats as being
// logged out.
func (s *Store) UserByID(ctx context.Context, id uuid.UUID) (User, error) {
	row, err := s.q.UserByID(ctx, fromUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, fmt.Errorf("find user by id: %w", err)
	}
	return toUser(row), nil
}

// decoyHash is a real Argon2id hash of a password nobody has, used to spend the
// same time on an unknown address as on a known one. It is computed at most
// once per process, and only if a login ever misses.
var decoyHash = sync.OnceValue(func() string {
	h, err := HashPassword("this password belongs to no account")
	if err != nil {
		// Only reachable if the system random source fails, in which case the
		// process has larger problems; an unparseable string still costs the
		// caller nothing and fails closed.
		return "$argon2id$"
	}
	return h
})

func toUser(row dbgen.User) User {
	return User{
		ID:        toUUID(row.ID),
		Email:     row.Email,
		CreatedAt: toTime(row.CreatedAt),
		UpdatedAt: toTime(row.UpdatedAt),
	}
}
