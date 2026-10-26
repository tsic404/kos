// Package extension implements the extension interface: package digests, the
// declaration, contribution loading, dependency activation and the visibility
// projection (C-17 to C-20, C-28).
//
// The kernel never identifies an extension by name: an install request names a
// package artifact, the artifact carries the declaration, and every visible
// contribution comes from that declaration (C-17).
package extension

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// digestPrefix is the one digest form an install request names (C-17).
const digestPrefix = "sha256:"

// DigestOf returns the sha256 digest of an artifact in the form a component
// names it.
func DigestOf(content []byte) string {
	sum := sha256.Sum256(content)
	return digestPrefix + hex.EncodeToString(sum[:])
}

// Package is one extension package artifact and the digest the install request
// binds it to (C-17). The artifact is the declaration document.
type Package struct {
	// Reference locates the artifact; it is the component spec's value.
	Reference string
	// Digest is the sha256 the artifact must match.
	Digest string
	// Content is the fetched artifact.
	Content []byte
}

// Verify reports whether the artifact matches the named digest. A missing or
// malformed digest is a mismatch: an install request has to name one (C-17).
func (p Package) Verify() error {
	named := strings.ToLower(p.Digest)
	encoded, found := strings.CutPrefix(named, digestPrefix)
	if !found || len(encoded) != 2*sha256.Size {
		return fmt.Errorf("package digest %q is not sha256:<hex>", p.Digest)
	}
	if _, err := hex.DecodeString(encoded); err != nil {
		return fmt.Errorf("package digest %q is not sha256:<hex>", p.Digest)
	}
	if actual := DigestOf(p.Content); actual != named {
		return fmt.Errorf("package artifact is %s, the request names %s", actual, p.Digest)
	}
	return nil
}

// ArtifactSource resolves the reference a component names to the artifact's
// bytes (C-17).
type ArtifactSource interface {
	// Fetch returns the artifact a reference names. A missing artifact is an
	// error: the component cannot install without it.
	Fetch(ctx context.Context, reference string) ([]byte, error)
}

// MemoryArtifacts is an in-memory artifact source: it serves the artifacts a
// process already holds, which is how an install request is resolved before a
// registry client exists.
type MemoryArtifacts map[string][]byte

// Fetch returns the artifact stored under reference.
func (m MemoryArtifacts) Fetch(_ context.Context, reference string) ([]byte, error) {
	content, ok := m[reference]
	if !ok {
		return nil, fmt.Errorf("artifact %q is not available", reference)
	}
	return content, nil
}
