package provider

import (
	"context"
	"sort"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/write"
)

// Catalogue is the capability view an application compares its requirements
// against: the effective capabilities of every provider, each carrying the
// verification state the validator wrote (C-09, C-10, C-48).
type Catalogue struct {
	writer *write.Writer
}

// NewCatalogue builds the view over one write path.
func NewCatalogue(writer *write.Writer) *Catalogue {
	return &Catalogue{writer: writer}
}

// Capabilities implements app.CapabilitySource. Only the effective set counts:
// a blocked withdrawal keeps the old set in service, and a capability no
// provider registered is simply absent (C-48).
func (c *Catalogue) Capabilities(ctx context.Context) ([]app.Capability, error) {
	if c == nil || c.writer == nil {
		return nil, nil
	}
	providers, err := listObjects(ctx, c.writer, api.Group, "Provider")
	if err != nil {
		return nil, err
	}
	var capabilities []app.Capability
	for _, object := range providers {
		status, err := providerStatusOf(object)
		if err != nil {
			return nil, err
		}
		capabilities = append(capabilities, StatusCapabilities(status)...)
	}
	sortCapabilities(capabilities)
	return capabilities, nil
}

// Capabilities projects the effective capability set of the given providers
// into the comparison input of C-11, each carrying the verification state the
// validator wrote (C-09, C-10, C-48).
func Capabilities(providers []v1beta1.Provider) []app.Capability {
	var capabilities []app.Capability
	for _, provider := range providers {
		capabilities = append(capabilities, StatusCapabilities(provider.Status)...)
	}
	sortCapabilities(capabilities)
	return capabilities
}

// StatusCapabilities projects one provider's status into the comparison input
// of C-11. Only the effective set counts: a blocked withdrawal keeps the old set
// in service (C-48).
func StatusCapabilities(status v1beta1.ProviderStatus) []app.Capability {
	results := make(map[string]v1beta1.ProviderCapabilityResult, len(status.Capabilities))
	for _, result := range status.Capabilities {
		results[result.Name+"@"+result.Version] = result
	}
	capabilities := make([]app.Capability, 0, len(status.EffectiveCapabilities))
	for _, capability := range status.EffectiveCapabilities {
		result, found := results[capability.Name+"@"+capability.Version]
		capabilities = append(capabilities, app.Capability{
			Name:       capability.Name,
			Version:    capability.Version,
			Scope:      capability.Scope,
			Guarantees: guaranteesOf(capability),
			Verified:   found && result.State == conformance.StateVerified,
		})
	}
	return capabilities
}

// sortCapabilities orders the comparison input by name and version.
func sortCapabilities(capabilities []app.Capability) {
	sort.SliceStable(capabilities, func(left, right int) bool {
		if capabilities[left].Name != capabilities[right].Name {
			return capabilities[left].Name < capabilities[right].Name
		}
		return capabilities[left].Version < capabilities[right].Version
	})
}

// guaranteesOf converts the registered guarantee values into the comparison
// shape (C-09).
func guaranteesOf(capability v1beta1.ProviderCapability) map[string]app.Guarantee {
	guarantees := make(map[string]app.Guarantee, len(capability.Guarantees))
	for _, guarantee := range capability.Guarantees {
		value := app.Guarantee{Enum: guarantee.Enum}
		if guarantee.Bool != nil {
			boolean := *guarantee.Bool
			value.Bool = &boolean
		}
		guarantees[guarantee.Key] = value
	}
	return guarantees
}
