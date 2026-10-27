package extension

import (
	"strings"

	"github.com/tsic404/kos/api/v1beta1"
)

// baselineCapabilities is the kernel capability baseline (C-09): eight names,
// each starting at v1 with the scope the kernel fixes. A product never adds to
// it or changes it (TC-2).
var baselineCapabilities = []Capability{
	{Name: "storage.volume", Version: "v1", Scope: v1beta1.CapabilityScopeNode},
	{Name: "storage.snapshot", Version: "v1", Scope: v1beta1.CapabilityScopeNode},
	{Name: "storage.shared-rwx", Version: "v1", Scope: v1beta1.CapabilityScopeCluster},
	{Name: "network.http-endpoint", Version: "v1", Scope: v1beta1.CapabilityScopeCluster},
	{Name: "network.path-prefix", Version: "v1", Scope: v1beta1.CapabilityScopeCluster},
	{Name: "network.l4-endpoint", Version: "v1", Scope: v1beta1.CapabilityScopeCluster},
	{Name: "network.policy", Version: "v1", Scope: v1beta1.CapabilityScopeCluster},
	{Name: "compute.gpu", Version: "v1", Scope: v1beta1.CapabilityScopeNode},
}

// BaselineCapabilities returns a copy of the kernel capability baseline.
func BaselineCapabilities() []Capability {
	return append([]Capability(nil), baselineCapabilities...)
}

// baselineCapability resolves one baseline entry by name.
func baselineCapability(name string) (Capability, bool) {
	for _, capability := range baselineCapabilities {
		if capability.Name == name {
			return capability, true
		}
	}
	return Capability{}, false
}

// isBaselineCapability reports whether a name is one of the eight kernel
// capabilities (C-09). A provider contributes a baseline capability under the
// name the kernel fixed; a new capability carries its extension's prefix.
func isBaselineCapability(name string) bool {
	_, found := baselineCapability(name)
	return found
}

// catalogue is the capability and component set one activation round can use.
type catalogue struct {
	capabilities []Capability
	components   map[string]string
}

// catalogueFor returns the set the activated components provide, on top of the
// kernel baseline (C-28).
func catalogueFor(active []activated) catalogue {
	available := catalogue{
		capabilities: BaselineCapabilities(),
		components:   make(map[string]string, len(active)),
	}
	for _, component := range active {
		available.capabilities = append(available.capabilities, component.loaded.Contributions.Capabilities.Items...)
		available.components[component.loaded.Identifier] = component.loaded.Version
	}
	return available
}

// missingDependencies lists the unmet edges of one declaration against the
// catalogue (C-28). An empty result means every edge is met.
func (c catalogue) missingDependencies(dependencies Dependencies) []string {
	var missing []string
	for _, requirement := range dependencies.Capabilities {
		constraint, err := ParseConstraint(requirement.Versions)
		isMet := err == nil && c.hasCapability(requirement.Name, constraint)
		if !isMet {
			missing = append(missing, missingCapability(requirement))
		}
	}
	for _, requirement := range dependencies.Components {
		constraint, err := ParseConstraint(requirement.Versions)
		version, present := c.components[requirement.Identifier]
		if err != nil || !present || !constraint.Allows(version) {
			missing = append(missing, missingComponent(requirement))
		}
	}
	return missing
}

// hasCapability reports whether any catalogue entry matches the requirement.
func (c catalogue) hasCapability(name string, constraint Constraint) bool {
	for _, capability := range c.capabilities {
		if capability.Name == name && constraint.Allows(capability.Version) {
			return true
		}
	}
	return false
}

// missingCapability renders one unmet capability edge in the form a component
// status lists it.
func missingCapability(requirement CapabilityRequirement) string {
	return "capability " + requirement.Name + " " + versionsOrAny(requirement.Versions)
}

// missingComponent renders one unmet component edge.
func missingComponent(requirement ComponentRequirement) string {
	return "component " + requirement.Identifier + " " + versionsOrAny(requirement.Versions)
}

// versionsOrAny renders an empty constraint as "any".
func versionsOrAny(versions string) string {
	if strings.TrimSpace(versions) == "" {
		return "any"
	}
	return versions
}
