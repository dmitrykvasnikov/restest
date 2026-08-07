package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// The format is part of the contract: it is pasted into shells, CI settings and
// YAML files, and it has to survive all three unquoted.
func TestTokenTextIsRecognisableAndURLSafe(t *testing.T) {
	token, err := newTokenText()
	if err != nil {
		t.Fatalf("newTokenText: %v", err)
	}

	if !strings.HasPrefix(token, TokenMark) {
		t.Errorf("token %q does not open with %q", token, TokenMark)
	}
	if len(token) != tokenTextLen {
		t.Errorf("length = %d, want %d", len(token), tokenTextLen)
	}

	secret := strings.TrimPrefix(token, TokenMark)
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		t.Fatalf("the secret is not raw URL-safe base64: %v", err)
	}
	if len(decoded) != tokenBytes {
		t.Errorf("secret = %d bytes, want %d", len(decoded), tokenBytes)
	}
	if strings.ContainsAny(token, "+/= '\"\\") {
		t.Errorf("token %q holds a character that needs quoting somewhere", token)
	}
}

func TestTokensDoNotRepeat(t *testing.T) {
	seen := make(map[string]bool, 128)
	for range 128 {
		token, err := newTokenText()
		if err != nil {
			t.Fatalf("newTokenText: %v", err)
		}
		if seen[token] {
			t.Fatalf("the generator repeated itself: %q", token)
		}
		seen[token] = true
	}
}

// The prefix is what identifies a row after the secret is gone, and it must be
// a prefix of the plaintext — a row nobody can match to the token they hold is
// a row nobody dares revoke.
func TestPrefixIdentifiesTheToken(t *testing.T) {
	token, err := newTokenText()
	if err != nil {
		t.Fatalf("newTokenText: %v", err)
	}

	prefix := token[:tokenPrefixLen]
	if !strings.HasPrefix(token, prefix) {
		t.Errorf("prefix %q is not the start of %q", prefix, token)
	}
	// Short enough that the rest is still a secret worth having.
	if len(prefix) >= len(token)/2 {
		t.Errorf("prefix %q keeps too much of the token", prefix)
	}
}

func TestHashTokenIsStableAndDistinct(t *testing.T) {
	first := HashToken("rst_one")
	again := HashToken("rst_one")
	other := HashToken("rst_two")

	if !bytes.Equal(first, again) {
		t.Error("the same token hashed to two different values")
	}
	if bytes.Equal(first, other) {
		t.Error("two tokens hashed to the same value")
	}
	if len(first) != 32 {
		t.Errorf("hash = %d bytes, want 32", len(first))
	}
	if strings.Contains(string(first), "rst_one") {
		t.Error("the hash carries the plaintext")
	}
}

func TestLooksLikeToken(t *testing.T) {
	good, err := newTokenText()
	if err != nil {
		t.Fatalf("newTokenText: %v", err)
	}

	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"a real one", good, true},
		{"empty", "", false},
		{"no mark", strings.TrimPrefix(good, TokenMark), false},
		{"truncated", good[:len(good)-1], false},
		{"with something appended", good + "x", false},
		{"a session cookie by mistake", "s%3AabcdefghijklmnopqrstuvwxyzABCDEF", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeToken(tt.in); got != tt.want {
				t.Errorf("looksLikeToken(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// A malformed credential is refused before the database is asked, which is what
// keeps a caller sending a password or a cookie from costing a query. The zero
// Store has no connection at all, so reaching one would panic rather than fail
// quietly.
func TestAMalformedTokenNeverReachesTheDatabase(t *testing.T) {
	var store Store

	_, _, err := store.AuthenticateAPIToken(context.Background(), "not-a-token")
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	if got := tokenExpiry(0); got.Valid {
		t.Error("0 days should be no expiry at all, which is SQL null")
	}
	if got := tokenExpiry(-1); got.Valid {
		t.Error("a negative count should be no expiry rather than one in the past")
	}

	got := tokenExpiry(30)
	if !got.Valid {
		t.Fatal("30 days produced no expiry")
	}
	if days := time.Until(got.Time).Hours() / 24; days < 29 || days > 31 {
		t.Errorf("expiry is %.1f days away, want about 30", days)
	}
}

func TestAPITokenReportsItsState(t *testing.T) {
	var never APIToken
	if never.Used() || never.Expires() || never.Expired() {
		t.Error("a fresh token should be unused and eternal")
	}

	live := APIToken{LastUsedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if !live.Used() || !live.Expires() || live.Expired() {
		t.Errorf("a used token expiring in an hour reported %+v", live)
	}

	dead := APIToken{ExpiresAt: time.Now().Add(-time.Hour)}
	if !dead.Expired() {
		t.Error("an expiry an hour ago is expired")
	}
}

func TestTokenValidation(t *testing.T) {
	tests := []struct {
		name  string
		in    APITokenInput
		field string
	}{
		{name: "the ordinary one", in: APITokenInput{Name: "ci"}},
		{name: "with an expiry", in: APITokenInput{Name: "ci", ExpiresInDays: 30}},
		{name: "a name is required", in: APITokenInput{}, field: "name"},
		{
			name:  "a name has a ceiling",
			in:    APITokenInput{Name: strings.Repeat("a", maxTokenNameLen+1)},
			field: "name",
		},
		{name: "no control characters", in: APITokenInput{Name: "ci\nrunner"}, field: "name"},
		{name: "no expiry in the past", in: APITokenInput{Name: "ci", ExpiresInDays: -1}, field: "expires_in_days"},
		{
			name:  "no expiry past the ceiling",
			in:    APITokenInput{Name: "ci", ExpiresInDays: maxTokenDays + 1},
			field: "expires_in_days",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fe FieldErrors
			validateTokenName(&fe, tt.in.Name)
			validateTokenExpiry(&fe, tt.in.ExpiresInDays)

			if tt.field == "" {
				if err := fe.orNil(); err != nil {
					t.Fatalf("rejected a valid input: %v", err)
				}
				return
			}
			if fe[tt.field] == "" {
				t.Fatalf("no message against %q, got %v", tt.field, fe)
			}
		})
	}
}
