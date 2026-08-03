package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters, following the OWASP Password Storage Cheat Sheet: 19 MiB
// of memory, two passes, one lane. Memory is the expensive dimension for an
// attacker with a GPU, which is why it is turned up rather than the iteration
// count.
//
// These are recorded in every encoded hash, so raising them later re-hashes
// nothing: old passwords keep verifying with the parameters they were made
// with, and only new hashes use the new ones.
const (
	argonMemoryKiB = 19 * 1024
	argonTime      = 2
	argonLanes     = 1
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// ErrInvalidHash means the stored string is not an encoded Argon2id hash. It is
// a corruption of our own data rather than a wrong password, and callers must
// not report it as a failed login.
var ErrInvalidHash = errors.New("password hash is malformed")

// HashPassword returns the PHC-encoded Argon2id hash of password:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash>
//
// The format carries its own parameters, which is what lets them be raised
// later without invalidating every existing password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemoryKiB, argonLanes, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoryKiB, argonTime, argonLanes,
		b64.EncodeToString(salt), b64.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password produced encoded. A wrong password is
// (false, nil); a hash we cannot read is (false, ErrInvalidHash), because those
// two need different handling — one is a user's mistake, the other is ours.
func VerifyPassword(encoded, password string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, p.time, p.memory, p.lanes, uint32(len(want)))

	// Constant time: a comparison that returns early leaks how much of the hash
	// matched, one byte at a time.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// b64 is the unpadded standard alphabet the PHC string format uses.
var b64 = base64.RawStdEncoding

type argonParams struct {
	memory uint32
	time   uint32
	lanes  uint8
}

func decodeHash(encoded string) (p argonParams, salt, key []byte, err error) {
	// Leading empty field from the initial '$', then: algorithm, version,
	// parameters, salt, hash.
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: algorithm %q is not argon2id", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: version %d is not %d", ErrInvalidHash, version, argon2.Version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.lanes); err != nil {
		return p, nil, nil, ErrInvalidHash
	}
	if p.memory == 0 || p.time == 0 || p.lanes == 0 {
		return p, nil, nil, ErrInvalidHash
	}

	if salt, err = b64.DecodeString(parts[4]); err != nil || len(salt) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) == 0 {
		return p, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
