package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. The memory size stays well inside the control plane's
// process budget (C-57): a login costs one hash, not a resident allowance.
const (
	argon2MemoryKiB   = 19 * 1024
	argon2Iterations  = 2
	argon2Parallelism = 1
	argon2KeyLength   = 32
	argon2SaltLength  = 16
)

// hashPassword returns the one-way digest stored for a password (C-63). The
// encoding carries the parameters so a future cost can be raised without
// invalidating existing accounts.
func hashPassword(password string) (string, error) {
	salt := make([]byte, argon2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("identity: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argon2Iterations, argon2MemoryKiB, argon2Parallelism, argon2KeyLength)
	encoded := fmt.Sprintf("argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2MemoryKiB, argon2Iterations, argon2Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
	return encoded, nil
}

// verifyPassword reports whether password reproduces the stored digest. A
// malformed digest never verifies.
func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 5 || parts[0] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[1], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	params := strings.Split(parts[2], ",")
	if len(params) != 3 {
		return false
	}
	var memory, iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(params[0], "m=%d", &memory); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(params[1], "t=%d", &iterations); err != nil {
		return false
	}
	parsed, err := strconv.ParseUint(strings.TrimPrefix(params[2], "p="), 10, 8)
	if err != nil || !strings.HasPrefix(params[2], "p=") {
		return false
	}
	parallelism = uint8(parsed)
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// newSessionToken returns an opaque bearer token. Only its hash is stored, so
// a database export cannot be replayed as a session.
func newSessionToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: read session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// sessionTokenHash is the stored form of a session token.
func sessionTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// newOneTimePassword returns the password an account is created with; the
// account must replace it at first login (C-63).
func newOneTimePassword() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: read one-time password: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw), nil
}

// newUserID returns the internal identity of an account. The login name stays
// the name sessions and audit carry; the ID survives a rename.
func newUserID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("identity: read account id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
