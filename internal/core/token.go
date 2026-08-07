package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/dmitrykvasnikov/restest/internal/core/dbgen"
)

// APIToken is a credential for the management API, scoped to one account
// (DESIGN.md §8). Mock traffic never sees one: it is unauthenticated by design,
// and a token that also opened mock endpoints would be a second answer to a
// question already settled.
//
// The secret is not a field. It exists for the length of the call that created
// it and is never read back, because the row holds only its SHA-256.
type APIToken struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Name   string
	// Prefix is the leading characters of the plaintext, kept so that a row is
	// identifiable after the secret is gone. It is not a secret: it is the part
	// of the token a user can safely paste into a bug report.
	Prefix string

	// LastUsedAt is zero until the token authenticates something, which is what
	// makes "this one has never been used" a question the page can answer.
	LastUsedAt time.Time
	// ExpiresAt is zero for a token that does not expire.
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Used reports whether the token has ever authenticated a request.
func (t APIToken) Used() bool { return !t.LastUsedAt.IsZero() }

// Expires reports whether the token has an expiry at all.
func (t APIToken) Expires() bool { return !t.ExpiresAt.IsZero() }

// Expired reports whether that expiry has passed. The database is what actually
// refuses an expired token — AuthenticateAPIToken matches no row — so this is
// for the page, which should say "expired" rather than list it as live.
func (t APIToken) Expired() bool { return t.Expires() && !t.ExpiresAt.After(time.Now()) }

// APITokenInput is what a caller supplies to mint a token.
type APITokenInput struct {
	// Name is what the token is for — "ci", "laptop" — so that revoking the
	// right one later does not come down to guessing.
	Name string
	// ExpiresInDays is 0 for a token that never expires. A CI job's token
	// outliving the job is the ordinary case; an expiry is offered because a
	// token handed to something temporary should be able to say so.
	ExpiresInDays int
}

// Token format. The mark is there so that a token found in a shell history or a
// log file is recognisable as one, and so that a caller pasting the wrong string
// is told what is wrong rather than getting a bare 401.
const (
	// TokenMark opens every token restest issues.
	TokenMark = "rst_"
	// tokenBytes is the secret's length before encoding: 32 bytes, as
	// DESIGN.md §3 says, which is 256 bits of the system's random source.
	tokenBytes = 32
	// tokenPrefixLen is how much of the plaintext is kept in the clear. It
	// covers the mark and eight characters of the secret — enough to tell two
	// tokens apart in a list, and 48 bits out of 256, which leaves the rest
	// well past anything worth guessing at.
	tokenPrefixLen = len(TokenMark) + 8
)

// ErrInvalidToken is what a presented token that authenticates nothing comes
// back as — malformed, unknown, revoked or expired, all of them the same answer.
// Distinguishing them would tell a caller holding a guess which part of it was
// right.
var ErrInvalidToken = errors.New("invalid api token")

// CreateAPIToken mints a token for userID and returns it together with its
// plaintext, which is the only time the plaintext exists.
//
// The caller shows it once and drops it. Nothing stores it: the row holds a
// SHA-256, and a hash of 32 random bytes needs no salt and no slow KDF — those
// exist to make a low-entropy secret expensive to guess, and there is nothing
// cheap to guess here.
func (s *Store) CreateAPIToken(ctx context.Context, userID uuid.UUID, in APITokenInput) (APIToken, string, error) {
	in.Name = strings.TrimSpace(in.Name)

	var fe FieldErrors
	validateTokenName(&fe, in.Name)
	validateTokenExpiry(&fe, in.ExpiresInDays)
	if err := fe.orNil(); err != nil {
		return APIToken{}, "", err
	}

	plaintext, err := newTokenText()
	if err != nil {
		return APIToken{}, "", err
	}

	row, err := s.q.CreateAPIToken(ctx, dbgen.CreateAPITokenParams{
		UserID:    fromUUID(userID),
		Name:      in.Name,
		Prefix:    plaintext[:tokenPrefixLen],
		TokenHash: HashToken(plaintext),
		ExpiresAt: tokenExpiry(in.ExpiresInDays),
	})
	if err != nil {
		return APIToken{}, "", fmt.Errorf("create api token: %w", err)
	}
	return toAPIToken(row), plaintext, nil
}

// APITokensByUser lists an account's tokens, newest first.
func (s *Store) APITokensByUser(ctx context.Context, userID uuid.UUID) ([]APIToken, error) {
	rows, err := s.q.APITokensByUser(ctx, fromUUID(userID))
	if err != nil {
		return nil, fmt.Errorf("list api tokens: %w", err)
	}

	tokens := make([]APIToken, len(rows))
	for i, row := range rows {
		tokens[i] = toAPIToken(row)
	}
	return tokens, nil
}

// DeleteAPIToken revokes a token the caller owns. Revocation is immediate and
// total: the row is gone, so the next request carrying that secret hashes to
// nothing.
func (s *Store) DeleteAPIToken(ctx context.Context, userID, id uuid.UUID) error {
	n, err := s.q.DeleteAPIToken(ctx, dbgen.DeleteAPITokenParams{
		ID:     fromUUID(id),
		UserID: fromUUID(userID),
	})
	if err != nil {
		return fmt.Errorf("delete api token: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthenticateAPIToken resolves a presented token to the account that owns it,
// and records that it was used.
//
// A malformed string is refused here rather than in the database: a caller who
// sent a password, a session cookie or an empty header is not a lookup worth
// making, and the answer is the same either way.
func (s *Store) AuthenticateAPIToken(ctx context.Context, presented string) (User, APIToken, error) {
	if !looksLikeToken(presented) {
		return User{}, APIToken{}, ErrInvalidToken
	}

	row, err := s.q.AuthenticateAPIToken(ctx, HashToken(presented))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Unknown, revoked or expired. One answer for all three.
			return User{}, APIToken{}, ErrInvalidToken
		}
		return User{}, APIToken{}, fmt.Errorf("authenticate api token: %w", err)
	}

	user := User{
		ID:        toUUID(row.UserID),
		Email:     row.UserEmail,
		CreatedAt: toTime(row.UserCreatedAt),
		UpdatedAt: toTime(row.UserUpdatedAt),
	}
	token := APIToken{
		ID:         toUUID(row.ID),
		UserID:     toUUID(row.UserID),
		Name:       row.Name,
		Prefix:     row.Prefix,
		LastUsedAt: toTime(row.LastUsedAt),
		ExpiresAt:  toTime(row.ExpiresAt),
		CreatedAt:  toTime(row.CreatedAt),
	}
	return user, token, nil
}

// HashToken is what is stored and what is compared: the SHA-256 of the
// plaintext, as raw bytes. Exported because the integration tests hash a token
// to look its row up, and a second copy of this rule in a test is a second copy
// that can drift.
func HashToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(plaintext))
	return sum[:]
}

// newTokenText returns a fresh token: the mark, then 32 random bytes in
// URL-safe base64 with no padding, so the whole thing survives a query string,
// a shell and a YAML file unquoted.
func newTokenText() (string, error) {
	secret := make([]byte, tokenBytes)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("read random bytes for an api token: %w", err)
	}
	return TokenMark + base64.RawURLEncoding.EncodeToString(secret), nil
}

// tokenTextLen is how long a well-formed token is, so that a string of the
// wrong length is rejected before it reaches the database.
var tokenTextLen = len(TokenMark) + base64.RawURLEncoding.EncodedLen(tokenBytes)

func looksLikeToken(s string) bool {
	return len(s) == tokenTextLen && strings.HasPrefix(s, TokenMark)
}

// tokenExpiry turns the form's day count into the column's value. Zero days is
// SQL null, which is how the schema says "does not expire".
func tokenExpiry(days int) pgtype.Timestamptz {
	if days <= 0 {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, days), Valid: true}
}

func toAPIToken(row dbgen.ApiToken) APIToken {
	return APIToken{
		ID:         toUUID(row.ID),
		UserID:     toUUID(row.UserID),
		Name:       row.Name,
		Prefix:     row.Prefix,
		LastUsedAt: toTime(row.LastUsedAt),
		ExpiresAt:  toTime(row.ExpiresAt),
		CreatedAt:  toTime(row.CreatedAt),
	}
}
