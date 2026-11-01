package extension

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
)

// upgradeDeclaration is a valid declaration with one read-only page.
func upgradeDeclaration(identifier string) Declaration {
	return Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		APIGroup:   "widgets.example.test",
		Contributions: Contributions{
			Pages: []Page{{
				ID: "widgets", Title: "Widgets", Location: "panel",
				ReadOnly: true, ReadOnlyReason: "只读展示",
				Items: []PageItem{{ID: "widgets", Label: "Widgets", Source: PageSourceLocal, Store: "widgets"}},
			}},
		},
	}
}

// publishUpgradePackage stores a declaration under one reference and returns
// the digest the component must name.
func publishUpgradePackage(t *testing.T, artifacts MemoryArtifacts, reference string, declaration Declaration) string {
	t.Helper()
	content, err := Marshal(declaration)
	if err != nil {
		t.Fatalf("marshal %s: %v", reference, err)
	}
	artifacts[reference] = content
	return DigestOf(content)
}

// upgradeComponent builds a live system component that names one package.
func upgradeComponent(name, reference, digest string, labels map[string]any) *unstructured.Unstructured {
	metadata := map[string]any{
		"name":              name,
		"creationTimestamp": "2026-01-01T00:00:00Z",
	}
	if labels != nil {
		metadata["labels"] = labels
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "SystemComponent",
		"metadata":   metadata,
		"spec": map[string]any{
			"package": map[string]any{"reference": reference, "digest": digest},
		},
	}}
}

// managedLabels marks a component KOS manages (C-02).
func managedLabels() map[string]any {
	return map[string]any{markers.ManagedBy: "kos-shell"}
}

// upgradeInstaller builds an installer over an in-memory component list and
// artifact set. The preflight needs no status writer, so nothing can be written
// through it.
func upgradeInstaller(t *testing.T, artifacts ArtifactSource, objects ...runtime.Object) *Installer {
	t.Helper()
	resource := schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: "systemcomponents",
	}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(v1beta1.GroupVersion.WithKind("SystemComponent"), &unstructured.Unstructured{})
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{resource: "SystemComponentList"},
		objects...,
	)
	return &Installer{
		Dynamic:   client,
		Resources: map[schema.GroupKind]string{{Group: api.Group, Kind: "SystemComponent"}: "systemcomponents"},
		Artifacts: artifacts,
	}
}

// TestContributionDiffReportsDroppedPageAndAddedSetting checks C-55's
// item-by-item report: the page the target withdraws and the setting it adds
// are the only entries, in the class order C-18 fixes.
func TestContributionDiffReportsDroppedPageAndAddedSetting(t *testing.T) {
	before := Contributions{Pages: []Page{{ID: "widgets"}}}
	after := Contributions{Settings: []Setting{{ID: "widget-endpoint"}}}
	want := []ContributionChange{
		{Class: ClassPages, Removed: []string{"widgets"}},
		{Class: ClassSettings, Added: []string{"widget-endpoint"}},
	}
	if got := ContributionDiff(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff = %+v, want %+v", got, want)
	}
}

// TestContributionDiffKeepsOneEntryPerMovedClass checks that a class moving on
// both sides is one entry naming the removed and the added item, and that a
// class which did not move stays out.
func TestContributionDiffKeepsOneEntryPerMovedClass(t *testing.T) {
	before := Contributions{Pages: []Page{{ID: "old-page"}, {ID: "kept-page"}}}
	after := Contributions{Pages: []Page{{ID: "new-page"}, {ID: "kept-page"}}}
	want := []ContributionChange{{Class: ClassPages, Removed: []string{"old-page"}, Added: []string{"new-page"}}}
	if got := ContributionDiff(before, after); !reflect.DeepEqual(got, want) {
		t.Fatalf("diff = %+v, want %+v", got, want)
	}
}

// TestContributionItemsNamesEveryClass checks the item identities C-55
// compares, so a caller rendering one class sees the same values the diff used.
func TestContributionItemsNamesEveryClass(t *testing.T) {
	contributions := Contributions{
		ObjectClasses:      []ObjectClass{{Group: "widgets.example.test", Kind: "Widget"}, {Kind: "Node"}},
		Controllers:        []Controller{{ID: "widget-controller"}},
		Capabilities:       CapabilityContributions{Items: []Capability{{Name: "demo.widgets/x", Version: "v1"}}, StorageClasses: []string{"fast"}, SelfReported: []SelfReport{{Capability: "demo.widgets/x", Version: "v1"}}},
		Pages:              []Page{{ID: "widgets"}},
		Settings:           []Setting{{ID: "widget-endpoint"}},
		CatalogueSources:   []CatalogueSource{{ID: "widgets"}},
		DiscoveryAdapters:  []DiscoveryAdapter{{ID: "widget-discovery"}},
		DiagnosticAdapters: []DiagnosticAdapter{{ID: "widget-diagnostics"}},
		Preferences:        []PreferenceSchema{{ID: "widget-view"}},
	}
	cases := map[string][]string{
		ClassObjectClasses:      {"Node", "widgets.example.test/Widget"},
		ClassControllers:        {"widget-controller"},
		ClassCapabilities:       {"demo.widgets/x@v1", "selfReported:demo.widgets/x@v1", "storageClass:fast"},
		ClassPages:              {"widgets"},
		ClassSettings:           {"widget-endpoint"},
		ClassCatalogueSources:   {"widgets"},
		ClassDiscoveryAdapters:  {"widget-discovery"},
		ClassDiagnosticAdapters: {"widget-diagnostics"},
		ClassPreferences:        {"widget-view"},
		"unknown":               nil,
	}
	for class, want := range cases {
		if got := ContributionItems(class, contributions); !reflect.DeepEqual(got, want) {
			t.Errorf("%s = %v, want %v", class, got, want)
		}
	}
}

// TestUpgradePreflightReportsContributionChanges checks C-55 end to end: the
// target package goes through the install evaluation, the report names the
// package moved from and to, the item-by-item changes are the only ones, and
// the read writes nothing.
func TestUpgradePreflightReportsContributionChanges(t *testing.T) {
	artifacts := MemoryArtifacts{}
	current := upgradeDeclaration("demo.widgets")
	fromDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v1", current)
	target := current
	target.Contributions.Pages = nil
	target.Contributions.Settings = []Setting{{ID: "widget-endpoint", Scope: SettingScopeCluster, Writer: SettingWriterUser}}
	toDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v2", target)

	installer := upgradeInstaller(t, artifacts,
		upgradeComponent("widgets", "demo-widgets-v1", fromDigest, managedLabels()))
	preflight, err := installer.UpgradePreflight(context.Background(), []UpgradeTarget{{
		Component: "widgets", Reference: "demo-widgets-v2", Digest: toDigest,
	}})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(preflight.Components) != 1 {
		t.Fatalf("components = %+v, want one", preflight.Components)
	}
	upgrade := preflight.Components[0]
	if upgrade.Component != "widgets" || upgrade.From != "demo-widgets-v1" || upgrade.To != "demo-widgets-v2" {
		t.Fatalf("upgrade = %+v", upgrade)
	}
	want := []ContributionChange{
		{Class: ClassPages, Removed: []string{"widgets"}},
		{Class: ClassSettings, Added: []string{"widget-endpoint"}},
	}
	if !reflect.DeepEqual(upgrade.Changes, want) {
		t.Fatalf("changes = %+v, want %+v", upgrade.Changes, want)
	}
	if upgrade.IsBlocked || preflight.IsBlocked || len(upgrade.Problems) != 0 {
		t.Fatalf("upgrade = %+v, preflight blocked = %v", upgrade, preflight.IsBlocked)
	}
	client := installer.Dynamic.(*dynamicfake.FakeDynamicClient)
	for _, action := range client.Actions() {
		if verb := action.GetVerb(); verb != "list" && verb != "get" {
			t.Fatalf("preflight wrote through %q", verb)
		}
	}
}

// TestUpgradePreflightAcceptsAnUnchangedDeclaration checks that a target with
// the same contributions reports no changes and blocks nothing.
func TestUpgradePreflightAcceptsAnUnchangedDeclaration(t *testing.T) {
	artifacts := MemoryArtifacts{}
	declaration := upgradeDeclaration("demo.widgets")
	fromDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v1", declaration)
	toDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v2", declaration)

	installer := upgradeInstaller(t, artifacts,
		upgradeComponent("widgets", "demo-widgets-v1", fromDigest, managedLabels()))
	preflight, err := installer.UpgradePreflight(context.Background(), []UpgradeTarget{{
		Component: "widgets", Reference: "demo-widgets-v2", Digest: toDigest,
	}})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	upgrade := preflight.Components[0]
	if len(upgrade.Changes) != 0 {
		t.Fatalf("changes = %+v, want none", upgrade.Changes)
	}
	if upgrade.IsBlocked || preflight.IsBlocked || len(upgrade.Problems) != 0 {
		t.Fatalf("unchanged upgrade = %+v, preflight blocked = %v", upgrade, preflight.IsBlocked)
	}
}

// TestUpgradePreflightBlocksAMissingDependency checks C-28 through the
// preflight: the target's unmet edge is reported as a problem and blocks the
// upgrade.
func TestUpgradePreflightBlocksAMissingDependency(t *testing.T) {
	artifacts := MemoryArtifacts{}
	current := upgradeDeclaration("demo.widgets")
	fromDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v1", current)
	target := current
	target.Dependencies.Capabilities = []CapabilityRequirement{{Name: "demo.absent/capability", Versions: "v1"}}
	toDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v2", target)

	installer := upgradeInstaller(t, artifacts,
		upgradeComponent("widgets", "demo-widgets-v1", fromDigest, managedLabels()))
	preflight, err := installer.UpgradePreflight(context.Background(), []UpgradeTarget{{
		Component: "widgets", Reference: "demo-widgets-v2", Digest: toDigest,
	}})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	upgrade := preflight.Components[0]
	if len(upgrade.Problems) == 0 || !upgrade.IsBlocked || !preflight.IsBlocked {
		t.Fatalf("upgrade = %+v, preflight blocked = %v", upgrade, preflight.IsBlocked)
	}
	if !strings.Contains(strings.Join(upgrade.Problems, "; "), "demo.absent/capability") {
		t.Fatalf("problems = %v, want the missing capability named", upgrade.Problems)
	}
}

// TestUpgradePreflightReportsAnUnverifiedTargetPackage checks that a target
// package the evaluation rejects is a reported problem, not a preflight error.
func TestUpgradePreflightReportsAnUnverifiedTargetPackage(t *testing.T) {
	artifacts := MemoryArtifacts{}
	current := upgradeDeclaration("demo.widgets")
	fromDigest := publishUpgradePackage(t, artifacts, "demo-widgets-v1", current)
	publishUpgradePackage(t, artifacts, "demo-widgets-v2", current)

	installer := upgradeInstaller(t, artifacts,
		upgradeComponent("widgets", "demo-widgets-v1", fromDigest, managedLabels()))
	preflight, err := installer.UpgradePreflight(context.Background(), []UpgradeTarget{{
		Component: "widgets", Reference: "demo-widgets-v2", Digest: DigestOf([]byte("other artifact")),
	}})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	upgrade := preflight.Components[0]
	if len(upgrade.Problems) == 0 || !upgrade.IsBlocked || !preflight.IsBlocked {
		t.Fatalf("upgrade = %+v, preflight blocked = %v", upgrade, preflight.IsBlocked)
	}
	if len(upgrade.Changes) != 0 {
		t.Fatalf("changes = %+v, want none for an unverified package", upgrade.Changes)
	}
}

// TestUpgradePreflightRejectsATargetThatIsNotInstalled checks that only an
// installed, managed component can be an upgrade target.
func TestUpgradePreflightRejectsATargetThatIsNotInstalled(t *testing.T) {
	artifacts := MemoryArtifacts{}
	declaration := upgradeDeclaration("demo.widgets")
	digest := publishUpgradePackage(t, artifacts, "demo-widgets-v2", declaration)
	target := UpgradeTarget{Component: "ghost", Reference: "demo-widgets-v2", Digest: digest}

	cases := map[string][]runtime.Object{
		"unknown component": {upgradeComponent("widgets", "demo-widgets-v1", digest, managedLabels())},
		"adopted component": {upgradeComponent("ghost", "demo-widgets-v1", digest, map[string]any{
			markers.AdoptedBy: "kos-shell", markers.AdoptionReason: "operator",
		})},
	}
	for name, objects := range cases {
		installer := upgradeInstaller(t, artifacts, objects...)
		if _, err := installer.UpgradePreflight(context.Background(), []UpgradeTarget{target}); err == nil {
			t.Errorf("%s: preflight accepted the target", name)
		}
	}
}
