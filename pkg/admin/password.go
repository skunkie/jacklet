// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package admin

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// hashScheme names the only key-derivation scheme Jacklet writes. It is
// recorded in every hash so a future change of scheme or cost can be
// recognized rather than guessed.
const hashScheme = "pbkdf2-sha256"

// hashIterations is the PBKDF2 work factor, following OWASP's guidance for
// PBKDF2-HMAC-SHA256. A stored hash carries its own iteration count, so
// raising this does not invalidate hashes generated earlier.
const hashIterations = 600_000

// hashSaltBytes and hashKeyBytes size the random salt and the derived key.
const (
	hashKeyBytes  = 32
	hashSaltBytes = 16
)

// ErrMalformedHash is returned when ADMIN_PASSWORD_HASH is not a hash this
// build recognizes.
var ErrMalformedHash = errors.New("malformed password hash")

// HashPassword derives a storable hash of password, in the form
// "pbkdf2-sha256$<iterations>$<salt>$<key>" with the salt and key
// base64-encoded. The result is safe to keep in a config file or a
// deployment manifest: it does not reveal the password.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password is empty")
	}

	salt := make([]byte, hashSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, hashIterations, hashKeyBytes)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s$%d$%s$%s",
		hashScheme,
		hashIterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// parsedHash is a decoded password hash.
type parsedHash struct {
	iterations int
	key        []byte
	salt       []byte
}

// parseHash decodes a stored hash, rejecting anything it does not
// recognize rather than silently treating it as a password.
func parseHash(encoded string) (parsedHash, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return parsedHash{}, fmt.Errorf("%w: expected 4 fields, got %d", ErrMalformedHash, len(parts))
	}
	if parts[0] != hashScheme {
		return parsedHash{}, fmt.Errorf("%w: unsupported scheme %q", ErrMalformedHash, parts[0])
	}

	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 1 {
		return parsedHash{}, fmt.Errorf("%w: bad iteration count %q", ErrMalformedHash, parts[1])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) == 0 {
		return parsedHash{}, fmt.Errorf("%w: bad salt", ErrMalformedHash)
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) == 0 {
		return parsedHash{}, fmt.Errorf("%w: bad key", ErrMalformedHash)
	}

	return parsedHash{iterations: iterations, key: key, salt: salt}, nil
}

// Password verifies the admin password against whichever credential was
// configured: a stored hash when one is supplied, or a plaintext value
// otherwise.
type Password struct {
	hash      *parsedHash
	plaintext string
}

// NewPassword builds the credential the panel checks against. hash takes
// precedence over plaintext, so a deployment that sets both gets the
// safer one. A hash that cannot be parsed is an error rather than a
// silent fallback: quietly ignoring it would leave the panel accepting
// some other password, or none at all.
//
// With both empty the credential is unconfigured, and the panel is
// disabled.
func NewPassword(plaintext, hash string) (*Password, error) {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return &Password{plaintext: plaintext}, nil
	}

	parsed, err := parseHash(hash)
	if err != nil {
		return nil, err
	}
	return &Password{hash: &parsed}, nil
}

// Configured reports whether any credential was supplied.
func (p *Password) Configured() bool {
	return p != nil && (p.hash != nil || p.plaintext != "")
}

// Verify reports whether candidate is the configured password. The
// comparison is constant-time in both modes, so a wrong guess cannot be
// refined by timing it.
func (p *Password) Verify(candidate string) bool {
	if !p.Configured() {
		return false
	}

	if p.hash == nil {
		return subtle.ConstantTimeCompare([]byte(candidate), []byte(p.plaintext)) == 1
	}

	derived, err := pbkdf2.Key(sha256.New, candidate, p.hash.salt, p.hash.iterations, len(p.hash.key))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(derived, p.hash.key) == 1
}

// Hashed reports whether the configured credential is a stored hash rather
// than a plaintext value, for a startup log line.
func (p *Password) Hashed() bool {
	return p != nil && p.hash != nil
}
