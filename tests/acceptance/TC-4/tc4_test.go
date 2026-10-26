// Package tc4 verifies that every contribution class a declaration names is
// observable after activation, with the declared identifier, and that the
// component reports itself ready (AC-2, TC-4).
package tc4

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// declaration declares one entry of every contribution class.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   "widgets.example.test",
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: "widgets.example.test", Kind: "Widget", Resource: "widgets",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []extension.Controller{{
				ID: "widget", Manages: []extension.GroupKind{{Group: "widgets.example.test", Kind: "Widget"}},
			}},
			Capabilities:       []extension.Capability{{Name: "demo.widgets/panel", Version: "v1"}},
			Pages:              []extension.Page{{ID: "widgets", Title: "Widgets", Actions: []extension.Action{{ID: "install"}}}},
			Settings:           []extension.Setting{{ID: "widgets-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser}},
			CatalogueSources:   []extension.CatalogueSource{{ID: "widgets", Sync: "pull"}},
			DiscoveryAdapters:  []extension.DiscoveryAdapter{{ID: "widget-discovery"}},
			DiagnosticAdapters: []extension.DiagnosticAdapter{{ID: "widget-diagnostics"}},
			Preferences:        []extension.PreferenceSchema{{ID: "widget-view", Keys: []string{"view", "columns"}}},
		},
	}
}

// TestTC4EveryDeclaredContributionIsVisible installs the package and reads each
// class from the loaded result and the cluster status.
func TestTC4EveryDeclaredContributionIsVisible(t *testing.T) {
	cluster := harness.Start(t)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	result, found := installation.Component("widgets")
	if !found {
		t.Fatal("the installed component is not in the installation")
	}
	if result.Phase != extension.PhaseReady || result.Health != "Healthy" {
		t.Fatalf("component = %s/%s, want Ready/Healthy", result.Phase, result.Health)
	}

	contributions := result.Contributions
	if len(contributions.ObjectClasses) != 1 || contributions.ObjectClasses[0].GroupKind().String() != "widgets.example.test/Widget" {
		t.Fatalf("object classes = %v", contributions.ObjectClasses)
	}
	if len(contributions.Controllers) != 1 || contributions.Controllers[0].ID != "widget" {
		t.Fatalf("controllers = %v", contributions.Controllers)
	}
	if len(contributions.Capabilities) != 1 || contributions.Capabilities[0].Name != "demo.widgets/panel" {
		t.Fatalf("capabilities = %v", contributions.Capabilities)
	}
	if len(contributions.Pages) != 1 || contributions.Pages[0].ID != "widgets" ||
		len(contributions.Pages[0].Actions) != 1 || contributions.Pages[0].Actions[0].ID != "install" {
		t.Fatalf("pages = %v", contributions.Pages)
	}
	if len(contributions.Settings) != 1 || contributions.Settings[0].ID != "widgets-endpoint" {
		t.Fatalf("settings = %v", contributions.Settings)
	}
	if len(contributions.CatalogueSources) != 1 || contributions.CatalogueSources[0].ID != "widgets" {
		t.Fatalf("catalogue sources = %v", contributions.CatalogueSources)
	}
	if len(contributions.DiscoveryAdapters) != 1 || contributions.DiscoveryAdapters[0].ID != "widget-discovery" {
		t.Fatalf("discovery adapters = %v", contributions.DiscoveryAdapters)
	}
	if len(contributions.DiagnosticAdapters) != 1 || contributions.DiagnosticAdapters[0].ID != "widget-diagnostics" {
		t.Fatalf("diagnostic adapters = %v", contributions.DiagnosticAdapters)
	}
	if len(contributions.Preferences) != 1 || contributions.Preferences[0].ID != "widget-view" ||
		len(contributions.Preferences[0].Keys) != 2 {
		t.Fatalf("preferences = %v", contributions.Preferences)
	}

	registry := installation.Registry()
	if !registry.IsServing("widgets.example.test", "Widget") {
		t.Fatal("the declared object class is not served")
	}
	if _, found := registry.Setting("widgets-endpoint"); !found {
		t.Fatal("the declared setting is not visible")
	}
	if _, found := registry.Capability("demo.widgets/panel", "v1"); !found {
		t.Fatal("the declared capability is not registered")
	}
	if len(registry.CatalogueSources()) != 1 || len(registry.DiscoveryAdapters()) != 1 ||
		len(registry.DiagnosticAdapters()) != 1 || len(registry.Preferences()) != 1 {
		t.Fatal("a declared contribution class is missing from the registry")
	}

	status := cluster.ComponentStatus(t, "widgets")
	if phase := harness.String(status, "phase"); phase != extension.PhaseReady {
		t.Fatalf("stored phase = %q, want %q", phase, extension.PhaseReady)
	}
	if state := harness.String(status, "health", "state"); state != "Healthy" {
		t.Fatalf("stored health = %q, want Healthy", state)
	}
	conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "SystemComponent", "widgets"))
	for _, conditionType := range []string{api.ConditionReconciled, extension.ConditionPackageVerified,
		extension.ConditionDependenciesMet, extension.ConditionContributionsLoaded} {
		condition, ok := conditionOf(conditions, conditionType)
		if !ok || condition.Status != metav1.ConditionTrue {
			t.Errorf("condition %s = %+v (found %v), want True", conditionType, condition, ok)
		}
	}
}

// conditionOf returns the condition of one type.
func conditionOf(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
