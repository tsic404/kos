package extension

import "sort"

// ContributionChange is one contribution class's difference between two
// declarations (C-55): the items the target drops and the items it adds.
type ContributionChange struct {
	Class   string   `json:"class"`
	Removed []string `json:"removed,omitempty"`
	Added   []string `json:"added,omitempty"`
}

// contributionClassOrder is the fixed class order C-18 lists. The diff follows
// it so an upgrade report reads the same way for every declaration pair.
var contributionClassOrder = []string{
	ClassObjectClasses,
	ClassControllers,
	ClassCapabilities,
	ClassPages,
	ClassSettings,
	ClassCatalogueSources,
	ClassDiscoveryAdapters,
	ClassDiagnosticAdapters,
	ClassPreferences,
}

// ContributionDiff renders the item-by-item difference from before to after.
// One entry per class that moved, in the fixed class order C-18 lists.
func ContributionDiff(before, after Contributions) []ContributionChange {
	changes := make([]ContributionChange, 0)
	for _, class := range contributionClassOrder {
		removed := itemsOnlyIn(ContributionItems(class, before), ContributionItems(class, after))
		added := itemsOnlyIn(ContributionItems(class, after), ContributionItems(class, before))
		if len(removed) == 0 && len(added) == 0 {
			continue
		}
		changes = append(changes, ContributionChange{Class: class, Removed: removed, Added: added})
	}
	return changes
}

// ContributionItems returns one class's item identities, sorted and without
// repeats. An unknown class has no items. Every identity is stable across
// versions of the same item, so the diff compares declarations rather than
// their rendering.
func ContributionItems(class string, contributions Contributions) []string {
	items := make([]string, 0)
	switch class {
	case ClassObjectClasses:
		for _, entry := range contributions.ObjectClasses {
			items = append(items, entry.GroupKind().String())
		}
	case ClassControllers:
		for _, entry := range contributions.Controllers {
			items = append(items, entry.ID)
		}
	case ClassCapabilities:
		for _, entry := range contributions.Capabilities.Items {
			items = append(items, entry.Name+"@"+entry.Version)
		}
		for _, entry := range contributions.Capabilities.StorageClasses {
			items = append(items, "storageClass:"+entry)
		}
		for _, entry := range contributions.Capabilities.SelfReported {
			items = append(items, "selfReported:"+entry.Capability+"@"+entry.Version)
		}
	case ClassPages:
		for _, entry := range contributions.Pages {
			items = append(items, entry.ID)
		}
	case ClassSettings:
		for _, entry := range contributions.Settings {
			items = append(items, entry.ID)
		}
	case ClassCatalogueSources:
		for _, entry := range contributions.CatalogueSources {
			items = append(items, entry.ID)
		}
	case ClassDiscoveryAdapters:
		for _, entry := range contributions.DiscoveryAdapters {
			items = append(items, entry.ID)
		}
	case ClassDiagnosticAdapters:
		for _, entry := range contributions.DiagnosticAdapters {
			items = append(items, entry.ID)
		}
	case ClassPreferences:
		for _, entry := range contributions.Preferences {
			items = append(items, entry.ID)
		}
	default:
		return nil
	}
	if len(items) == 0 {
		return nil
	}
	sort.Strings(items)
	return dedupeStrings(items)
}

// itemsOnlyIn lists the items of one sorted, deduped set that the other does
// not carry, in the same order. An empty difference is nil so it stays out of
// the rendered report.
func itemsOnlyIn(items, others []string) []string {
	kept := make([]string, 0, len(items))
	index := 0
	for _, item := range items {
		for index < len(others) && others[index] < item {
			index++
		}
		if index < len(others) && others[index] == item {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}
