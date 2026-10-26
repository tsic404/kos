package identity

import (
	"strings"
	"testing"
)

// TestPasswordDigest checks that only a one-way digest is stored and that a
// wrong or malformed digest never verifies (C-63).
func TestPasswordDigest(t *testing.T) {
	password := "correct-horse-battery"
	digest, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if strings.Contains(digest, password) {
		t.Fatalf("digest %q contains the password", digest)
	}
	if !verifyPassword(digest, password) {
		t.Fatal("the digest does not verify its password")
	}
	if verifyPassword(digest, password+"x") {
		t.Fatal("a different password verified")
	}
	other, err := hashPassword(password)
	if err != nil {
		t.Fatalf("hash password again: %v", err)
	}
	if other == digest {
		t.Fatal("two hashes of one password share a salt")
	}
	for _, malformed := range []string{"", "plain-text", "argon2id$v=19$m=1,t=1,p=1$bad$bad", digest[:len(digest)-2]} {
		if verifyPassword(malformed, password) {
			t.Fatalf("malformed digest %q verified", malformed)
		}
	}
}
