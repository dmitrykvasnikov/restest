package web

import (
	"bytes"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/dmitrykvasnikov/restest/internal/core"
)

// fakeTokens is an in-memory stand-in for the token half of core.Store.
//
// It is a working little store rather than a set of canned answers, because
// what these tests ask is a sequence — mint a token, present it, revoke it,
// present it again — and canned answers cannot fail that. It hashes with
// core.HashToken and keeps only the hash, so a test cannot accidentally pass by
// comparing plaintext the real store would never have.
type fakeTokens struct {
	mu     sync.Mutex
	tokens []storedToken
}

type storedToken struct {
	core.APIToken
	hash []byte
}

func newFakeTokens() *fakeTokens { return &fakeTokens{} }

func (f *fakeTokens) create(userID uuid.UUID, in core.APITokenInput) (core.APIToken, string, error) {
	if in.Name == "" {
		var fe core.FieldErrors
		fe.Add("name", "Name the token — it is how you will know which one to revoke.")
		return core.APIToken{}, "", fe
	}

	// Not core's generator — that one is unexported — but the same shape, so
	// the format check in AuthenticateAPIToken sees what it would see.
	plaintext := core.TokenMark + uuid.NewString() + uuid.NewString()
	plaintext = plaintext[:len(core.TokenMark)+43]

	token := core.APIToken{
		ID:        uuid.New(),
		UserID:    userID,
		Name:      in.Name,
		Prefix:    plaintext[:len(core.TokenMark)+8],
		CreatedAt: time.Now(),
	}
	if in.ExpiresInDays > 0 {
		token.ExpiresAt = time.Now().AddDate(0, 0, in.ExpiresInDays)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, storedToken{APIToken: token, hash: core.HashToken(plaintext)})

	return token, plaintext, nil
}

func (f *fakeTokens) byUser(userID uuid.UUID) []core.APIToken {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []core.APIToken
	for _, stored := range f.tokens {
		if stored.UserID == userID {
			out = append(out, stored.APIToken)
		}
	}
	return out
}

func (f *fakeTokens) remove(userID, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i, stored := range f.tokens {
		if stored.ID == id && stored.UserID == userID {
			f.tokens = append(f.tokens[:i], f.tokens[i+1:]...)
			return nil
		}
	}
	return core.ErrNotFound
}

// authenticate matches on the hash and refuses an expired token, which is what
// the real statement does in SQL.
func (f *fakeTokens) authenticate(presented string, user core.User) (core.User, core.APIToken, error) {
	hash := core.HashToken(presented)

	f.mu.Lock()
	defer f.mu.Unlock()

	for i, stored := range f.tokens {
		if !bytes.Equal(stored.hash, hash) {
			continue
		}
		if stored.Expires() && stored.Expired() {
			return core.User{}, core.APIToken{}, core.ErrInvalidToken
		}
		f.tokens[i].LastUsedAt = time.Now()
		return user, f.tokens[i].APIToken, nil
	}
	return core.User{}, core.APIToken{}, core.ErrInvalidToken
}
