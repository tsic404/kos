package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tsic404/kos/api/v1beta1"
)

// DigestPrefix is what a package digest starts with (C-32).
const DigestPrefix = "sha256:"

// DefinitionDigest computes the digest of a definition's package content. The
// digest covers the metadata, config structure, template, hooks, chart and
// requirements, and never itself, so the catalogue can compute it from the same
// bytes it installs (C-32).
func DefinitionDigest(spec v1beta1.ApplicationDefinitionSpec) (string, error) {
	content := spec
	content.Package.Digest = ""
	canonical, err := json.Marshal(content)
	if err != nil {
		return "", fmt.Errorf("encode package content: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// ValidateDefinition checks a definition before anything renders from it
// (C-32): the package carries a digest, the config structure is well formed,
// the hooks name a command, and the capability requirements are decidable.
func ValidateDefinition(spec v1beta1.ApplicationDefinitionSpec) error {
	if !validDigest(spec.Package.Digest) {
		return &InvalidPackage{Reason: fmt.Sprintf("the package digest %q is not %s followed by 64 hex digits", spec.Package.Digest, DigestPrefix)}
	}
	computed, err := DefinitionDigest(spec)
	if err != nil {
		return &InvalidPackage{Reason: err.Error()}
	}
	if computed != spec.Package.Digest {
		return &InvalidPackage{Reason: "the package content does not match its digest"}
	}
	if strings.TrimSpace(spec.Template) == "" {
		return &InvalidPackage{Reason: "the definition carries no translation template"}
	}
	if err := ValidateSchema(spec.Config); err != nil {
		return &InvalidPackage{Reason: "the config structure is invalid: " + err.Error()}
	}
	seen := make(map[string]bool, len(spec.Hooks))
	for _, hook := range spec.Hooks {
		switch {
		case hook.Name == "":
			return &InvalidPackage{Reason: "a hook carries no name"}
		case seen[hook.Name]:
			return &InvalidPackage{Reason: fmt.Sprintf("hook %q is declared twice", hook.Name)}
		case len(hook.Command) == 0:
			return &InvalidPackage{Reason: fmt.Sprintf("hook %q carries no command", hook.Name)}
		case hook.TimeoutSeconds < 0:
			return &InvalidPackage{Reason: fmt.Sprintf("hook %q has a negative timeout", hook.Name)}
		}
		seen[hook.Name] = true
	}
	for _, requirement := range spec.Requires {
		if err := ValidateRequirement(requirement); err != nil {
			return &InvalidPackage{Reason: err.Error()}
		}
	}
	return nil
}

// validDigest reports whether text is a well-formed package digest.
func validDigest(text string) bool {
	if !strings.HasPrefix(text, DigestPrefix) {
		return false
	}
	rest := strings.TrimPrefix(text, DigestPrefix)
	if len(rest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(rest)
	return err == nil
}

// HookSucceeded reports the result a hook expresses by its exit status: zero
// succeeds and every other value fails (C-32).
func HookSucceeded(exitCode int) bool {
	return exitCode == 0
}
