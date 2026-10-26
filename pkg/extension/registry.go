package extension

import (
	"fmt"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// Registry is the visibility projection of the active components (C-28): it
// answers what is served and what is visible, and never shows a contribution a
// component did not declare.
type Registry struct {
	classes      map[GroupKind]ObjectClass
	objectOrder  []GroupKind
	controllers  []Controller
	capabilities []Capability
	pages        []Page
	settings     []Setting
	sources      []CatalogueSource
	discovery    []DiscoveryAdapter
	diagnostics  []DiagnosticAdapter
	preferences  []PreferenceSchema
}

// ObjectClass resolves one served extension class (C-19).
func (r *Registry) ObjectClass(group, kind string) (controlplane.ExtensionObjectClass, bool) {
	if r == nil {
		return controlplane.ExtensionObjectClass{}, false
	}
	class, ok := r.classes[GroupKind{Group: group, Kind: kind}]
	if !ok {
		return controlplane.ExtensionObjectClass{}, false
	}
	return controlplane.ExtensionObjectClass{
		Versions:   append([]string(nil), class.Versions...),
		Resource:   class.Resource,
		Namespaced: class.Namespaced,
	}, true
}

// IsServing reports whether an extension object class is served now.
func (r *Registry) IsServing(group, kind string) bool {
	_, ok := r.ObjectClass(group, kind)
	return ok
}

// ObjectClasses returns the served extension classes in declaration order.
func (r *Registry) ObjectClasses() []ObjectClass {
	if r == nil {
		return nil
	}
	classes := make([]ObjectClass, 0, len(r.objectOrder))
	for _, key := range r.objectOrder {
		classes = append(classes, r.classes[key])
	}
	return classes
}

// Controllers returns the loaded controller contributions.
func (r *Registry) Controllers() []Controller {
	if r == nil {
		return nil
	}
	return append([]Controller(nil), r.controllers...)
}

// ExtensionCapabilities returns the capabilities extensions contribute.
func (r *Registry) ExtensionCapabilities() []Capability {
	if r == nil {
		return nil
	}
	return append([]Capability(nil), r.capabilities...)
}

// Capabilities returns the capability catalogue: the eight kernel baseline
// capabilities followed by the extension contributions (C-09, C-21).
func (r *Registry) Capabilities() []Capability {
	capabilities := BaselineCapabilities()
	if r == nil {
		return capabilities
	}
	return append(capabilities, r.capabilities...)
}

// Capability resolves one catalogue entry by name and version.
func (r *Registry) Capability(name, version string) (Capability, bool) {
	if r == nil {
		return Capability{}, false
	}
	if capability, ok := findCapability(BaselineCapabilities(), name, version); ok {
		return capability, true
	}
	return findCapability(r.capabilities, name, version)
}

// Pages returns the loaded pages.
func (r *Registry) Pages() []Page {
	if r == nil {
		return nil
	}
	return append([]Page(nil), r.pages...)
}

// PageFor returns a page a session role may see; an unknown page and a page the
// role may not see are both absent (C-59).
func (r *Registry) PageFor(id string, role write.Role) (Page, bool) {
	if r == nil {
		return Page{}, false
	}
	for _, page := range r.pages {
		if page.ID == id && page.Allows(role) {
			return page, true
		}
	}
	return Page{}, false
}

// Settings returns the loaded setting items.
func (r *Registry) Settings() []Setting {
	if r == nil {
		return nil
	}
	return append([]Setting(nil), r.settings...)
}

// Setting resolves one setting item.
func (r *Registry) Setting(id string) (Setting, bool) {
	if r == nil {
		return Setting{}, false
	}
	for _, setting := range r.settings {
		if setting.ID == id {
			return setting, true
		}
	}
	return Setting{}, false
}

// CatalogueSources returns the loaded catalogue sources.
func (r *Registry) CatalogueSources() []CatalogueSource {
	if r == nil {
		return nil
	}
	return append([]CatalogueSource(nil), r.sources...)
}

// DiscoveryAdapters returns the loaded discovery adapters.
func (r *Registry) DiscoveryAdapters() []DiscoveryAdapter {
	if r == nil {
		return nil
	}
	return append([]DiscoveryAdapter(nil), r.discovery...)
}

// DiagnosticAdapters returns the loaded diagnostic adapters.
func (r *Registry) DiagnosticAdapters() []DiagnosticAdapter {
	if r == nil {
		return nil
	}
	return append([]DiagnosticAdapter(nil), r.diagnostics...)
}

// Preferences returns the loaded preference schemas.
func (r *Registry) Preferences() []PreferenceSchema {
	if r == nil {
		return nil
	}
	return append([]PreferenceSchema(nil), r.preferences...)
}

// activated is one component that passed the dependency gate.
type activated struct {
	name   string
	loaded Loaded
}

// buildRegistry merges the activated contributions. A class two components both
// declare stays with the earlier one; the later component keeps its other
// contributions and loses that class (C-19, C-20). The returned map is the
// effective contribution set of each component after conflicts.
func buildRegistry(components []activated) (*Registry, map[string]Contributions, map[string][]ClassProblem) {
	registry := &Registry{classes: map[GroupKind]ObjectClass{}}
	conflicts := map[string][]ClassProblem{}
	effective := make(map[string]Contributions, len(components))
	serving := map[GroupKind]string{}

	for _, component := range components {
		entry := component.loaded.Contributions
		kept := make([]ObjectClass, 0, len(entry.ObjectClasses))
		for _, class := range entry.ObjectClasses {
			key := class.GroupKind()
			if owner, taken := serving[key]; taken {
				recordConflict(conflicts, component.name, ClassObjectClasses,
					fmt.Sprintf("class %s is already served by %s", key.String(), owner))
				continue
			}
			serving[key] = component.name
			registry.classes[key] = class
			registry.objectOrder = append(registry.objectOrder, key)
			kept = append(kept, class)
		}
		entry.ObjectClasses = kept
		effective[component.name] = entry
	}

	managed := map[GroupKind]string{}
	for _, component := range components {
		entry := effective[component.name]
		kept := make([]Controller, 0, len(entry.Controllers))
		for _, controller := range entry.Controllers {
			// Judge every class before taking any of them: a controller rejected
			// for one class must leave no placeholder behind, or the next
			// component's controller for another class is refused for nothing.
			var problems []string
			for _, class := range controller.Manages {
				if _, served := registry.classes[class]; !served {
					problems = append(problems, fmt.Sprintf("controller %s manages class %s that is not served", controller.ID, class.String()))
					continue
				}
				if owner, taken := managed[class]; taken && owner != component.name {
					problems = append(problems, fmt.Sprintf("class %s already has a controller", class.String()))
				}
			}
			if len(problems) > 0 {
				for _, problem := range problems {
					recordConflict(conflicts, component.name, ClassControllers, problem)
				}
				continue
			}
			for _, class := range controller.Manages {
				managed[class] = component.name
			}
			registry.controllers = append(registry.controllers, controller)
			kept = append(kept, controller)
		}
		entry.Controllers = kept
		effective[component.name] = entry
	}

	for _, component := range components {
		entry := effective[component.name]
		registry.capabilities = append(registry.capabilities, entry.Capabilities...)
		registry.pages = append(registry.pages, entry.Pages...)
		registry.settings = append(registry.settings, entry.Settings...)
		registry.sources = append(registry.sources, entry.CatalogueSources...)
		registry.discovery = append(registry.discovery, entry.DiscoveryAdapters...)
		registry.diagnostics = append(registry.diagnostics, entry.DiagnosticAdapters...)
		registry.preferences = append(registry.preferences, entry.Preferences...)
	}
	return registry, effective, conflicts
}

// recordConflict appends one item to a component's class problem.
func recordConflict(conflicts map[string][]ClassProblem, component, class, item string) {
	for index := range conflicts[component] {
		if conflicts[component][index].Class == class {
			conflicts[component][index].Items = append(conflicts[component][index].Items, item)
			return
		}
	}
	conflicts[component] = append(conflicts[component], ClassProblem{Class: class, Items: []string{item}})
}

// findCapability resolves one capability entry.
func findCapability(capabilities []Capability, name, version string) (Capability, bool) {
	for _, capability := range capabilities {
		if capability.Name == name && capability.Version == version {
			return capability, true
		}
	}
	return Capability{}, false
}
