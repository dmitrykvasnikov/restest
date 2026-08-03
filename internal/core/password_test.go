package core

import (
	"errors"
	"strings"
	"testing"
)

const goodPassword = "correct horse battery staple"

func TestHashAndVerify(t *testing.T) {
	encoded, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	ok, err := VerifyPassword(encoded, goodPassword)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("the password that produced the hash did not verify against it")
	}
}

func TestVerifyRejectsWrongPassword(t *testing.T) {
	encoded, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	for _, wrong := range []string{
		"",
		"Correct horse battery staple", // one letter's case
		goodPassword + " ",             // one trailing space
		"totally different",
	} {
		ok, err := VerifyPassword(encoded, wrong)
		if err != nil {
			t.Fatalf("VerifyPassword(%q): %v", wrong, err)
		}
		if ok {
			t.Errorf("password %q was accepted", wrong)
		}
	}
}

// The salt is what stops one leaked table from being cracked in a single pass,
// so two accounts choosing the same password must not share a hash.
func TestHashesDifferPerCall(t *testing.T) {
	first, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if first == second {
		t.Error("the same password hashed to the same string twice: the salt is not random")
	}
}

// The parameters travel with the hash, which is what will let them be raised
// later without invalidating every password already stored.
func TestEncodedFormat(t *testing.T) {
	encoded, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	const wantPrefix = "$argon2id$v=19$m=19456,t=2,p=1$"
	if !strings.HasPrefix(encoded, wantPrefix) {
		t.Errorf("encoded hash = %q, want it to start with %q", encoded, wantPrefix)
	}
	if n := strings.Count(encoded, "$"); n != 5 {
		t.Errorf("encoded hash has %d separators, want 5: %q", n, encoded)
	}
}

// A hash written with different parameters must still verify: the stored
// string, not the current constants, decides how it is checked.
func TestVerifyUsesTheHashesOwnParameters(t *testing.T) {
	// Produced by this package with m=8192,t=1,p=1 — deliberately not today's
	// parameters — for the password "old-parameters".
	const encoded = "$argon2id$v=19$m=8192,t=1,p=1$" +
		"YWJjZGVmZ2hpamtsbW5vcA$" +
		"yPwdoJ4JbOMAhj7Oi3cvZ/bAdW1K5JO8OiOcGrGJaYw"

	ok, err := VerifyPassword(encoded, "old-parameters")
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("a hash with older parameters did not verify")
	}
}

// A hash we cannot read is our own corruption. It must be distinguishable from
// a wrong password, or the real fault hides behind a user who believes they are
// typing it wrong.
func TestVerifyRejectsMalformedHash(t *testing.T) {
	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"not a phc string", "plaintext-password"},
		{"too few fields", "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA"},
		{"wrong algorithm", "$argon2i$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA"},
		{"wrong version", "$argon2id$v=16$m=19456,t=2,p=1$c2FsdA$aGFzaA"},
		{"unparseable parameters", "$argon2id$v=19$m=lots,t=2,p=1$c2FsdA$aGFzaA"},
		{"zero memory", "$argon2id$v=19$m=0,t=2,p=1$c2FsdA$aGFzaA"},
		{"salt is not base64", "$argon2id$v=19$m=19456,t=2,p=1$!!!!$aGFzaA"},
		{"empty hash", "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, err := VerifyPassword(tt.encoded, goodPassword)
			if ok {
				t.Fatal("a malformed hash reported a match")
			}
			if !errors.Is(err, ErrInvalidHash) {
				t.Errorf("error = %v, want ErrInvalidHash", err)
			}
		})
	}
}
