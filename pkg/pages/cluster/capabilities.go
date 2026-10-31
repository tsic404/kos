package cluster

import (
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/provider"
)

// Gap is one unmet capability requirement of C-11's comparison.
type Gap struct {
	// Kind is "missing" for a required capability no provider serves, or
	// "unsatisfied" for an optional capability or a guarantee that does not
	// hold.
	Kind string `json:"kind"`
	// Requirement is the capability name, or "capability.key" for a guarantee.
	Requirement string `json:"requirement"`
}

// Compare applies C-11's comparison to one definition's requirements against the
// providers' effective capability set. An invalid requirement is the
// definition's error, never a missing capability.
func Compare(requirements []v1beta1.CapabilityRequirement, providers []v1beta1.Provider) (app.CapabilityResult, error) {
	return app.Compare(requirements, provider.Capabilities(providers))
}

// Gaps renders one comparison as page items, missing capabilities first.
func Gaps(result app.CapabilityResult) []Gap {
	gaps := make([]Gap, 0, len(result.Missing)+len(result.Unsatisfied))
	for _, name := range result.Missing {
		gaps = append(gaps, Gap{Kind: "missing", Requirement: name})
	}
	for _, item := range result.Unsatisfied {
		gaps = append(gaps, Gap{Kind: "unsatisfied", Requirement: item})
	}
	return gaps
}
