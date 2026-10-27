// Package conformance holds the capability case sets a distribution ships and
// the digest that makes a run comparable (C-09, C-10).
//
// A case set is the distribution's definition of what a capability version
// promises; a provider's own result is a declaration, so only a run over a set
// whose digest matches the distribution's can verify a capability.
package conformance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Case is one check of a capability promise (C-09).
type Case struct {
	// ID identifies the case inside its set.
	ID string `json:"id"`
	// Description states the promise the case judges.
	Description string `json:"description"`
}

// CaseSet is the distribution's conformance case set for one capability
// version (C-09).
type CaseSet struct {
	// ID names the set; a provider registration names the same identifier.
	ID string `json:"id"`
	// Capability is the capability the set covers.
	Capability string `json:"capability"`
	// Version is the capability version the set covers.
	Version string `json:"version"`
	// Cases are the checks a run executes, in declared order.
	Cases []Case `json:"cases"`
}

// Distribution is the case-set collection a distribution ships (C-09). Its
// digests are what every registration is compared against (C-10).
type Distribution struct {
	sets    map[string]CaseSet
	digests map[string]string
}

// NewDistribution validates the sets and computes their digests. A duplicate
// identifier or an empty set is refused rather than silently ignored: a set
// that cannot be compared would verify nothing.
func NewDistribution(sets ...CaseSet) (*Distribution, error) {
	distribution := &Distribution{
		sets:    make(map[string]CaseSet, len(sets)),
		digests: make(map[string]string, len(sets)),
	}
	for _, set := range sets {
		if err := validateSet(set); err != nil {
			return nil, err
		}
		if _, taken := distribution.sets[set.ID]; taken {
			return nil, fmt.Errorf("conformance case set %q is declared twice", set.ID)
		}
		digest, err := digestOf(set)
		if err != nil {
			return nil, err
		}
		distribution.sets[set.ID] = set
		distribution.digests[set.ID] = digest
	}
	return distribution, nil
}

// Standard returns the case sets the distribution ships: the volume, snapshot
// and HTTP endpoint capabilities of C-09's baseline (AC-19).
func Standard() *Distribution {
	distribution, err := NewDistribution(standardSets()...)
	if err != nil {
		panic(err)
	}
	return distribution
}

// CaseSet returns one set by identifier.
func (d *Distribution) CaseSet(id string) (CaseSet, bool) {
	if d == nil {
		return CaseSet{}, false
	}
	set, ok := d.sets[id]
	return set, ok
}

// Digest returns the canonical digest of one set.
func (d *Distribution) Digest(id string) (string, bool) {
	if d == nil {
		return "", false
	}
	digest, ok := d.digests[id]
	return digest, ok
}

// validateSet rejects a set a run could not compare: it needs an identifier, a
// capability version and at least one uniquely named case.
func validateSet(set CaseSet) error {
	if strings.TrimSpace(set.ID) == "" {
		return fmt.Errorf("a conformance case set names no identifier")
	}
	if strings.TrimSpace(set.Capability) == "" || strings.TrimSpace(set.Version) == "" {
		return fmt.Errorf("case set %q names no capability version", set.ID)
	}
	if len(set.Cases) == 0 {
		return fmt.Errorf("case set %q has no case", set.ID)
	}
	seen := make(map[string]bool, len(set.Cases))
	for _, entry := range set.Cases {
		if strings.TrimSpace(entry.ID) == "" {
			return fmt.Errorf("case set %q has a case without an identifier", set.ID)
		}
		if seen[entry.ID] {
			return fmt.Errorf("case set %q declares case %q twice", set.ID, entry.ID)
		}
		seen[entry.ID] = true
	}
	return nil
}

// digestOf renders the canonical digest of one set: the capability version and
// the case identifiers in declared order.
func digestOf(set CaseSet) (string, error) {
	cases := make([]string, 0, len(set.Cases))
	for _, entry := range set.Cases {
		cases = append(cases, entry.ID)
	}
	canonical, err := json.Marshal(struct {
		Capability string   `json:"capability"`
		Version    string   `json:"version"`
		Cases      []string `json:"cases"`
	}{Capability: set.Capability, Version: set.Version, Cases: cases})
	if err != nil {
		return "", fmt.Errorf("encode case set %q: %w", set.ID, err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// standardSets is what the distribution ships. Each case names a promise the
// capability version makes, so a provider implementation answers it and the
// kernel never branches on a provider name (C-10, F11).
func standardSets() []CaseSet {
	return []CaseSet{
		{
			ID:         "kos/conformance/storage-volume-v1",
			Capability: "storage.volume",
			Version:    "v1",
			Cases: []Case{
				{ID: "volume.provision", Description: "a claim is provisioned as a backend volume"},
				{ID: "volume.bind", Description: "the claim binds to the provisioned volume"},
				{ID: "volume.expand", Description: "the volume expands online without rebinding"},
				{ID: "volume.delete", Description: "deleting the claim releases the backend volume"},
			},
		},
		{
			ID:         "kos/conformance/storage-snapshot-v1",
			Capability: "storage.snapshot",
			Version:    "v1",
			Cases: []Case{
				{ID: "snapshot.create", Description: "a snapshot captures the volume contents"},
				{ID: "snapshot.restore", Description: "a snapshot restores into a new volume"},
			},
		},
		{
			ID:         "kos/conformance/network-http-endpoint-v1",
			Capability: "network.http-endpoint",
			Version:    "v1",
			Cases: []Case{
				{ID: "endpoint.host", Description: "a host name reaches the target application"},
				{ID: "endpoint.path", Description: "a path prefix reaches the target application"},
				{ID: "endpoint.tls", Description: "the served endpoint presents the issued certificate"},
			},
		},
	}
}
