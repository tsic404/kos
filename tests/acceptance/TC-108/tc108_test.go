// Package tc108 verifies C-21: an application that needs an extension's
// capability stays Pending while the extension is absent, proceeds once it is
// installed, and a provider's own conformance report does not make the
// capability verified (AC-53, TC-108).
package tc108

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	component     = "kubevirt"
	capability    = "thirdparty.kubevirt/vm"
	appNamespace  = "apps"
	application   = "vm-console"
	definitionRef = "needs-virtualization"
)

// declaration contributes the product capability and its own report about it.
func declaration(t *testing.T, distribution *conformance.Distribution) extension.Declaration {
	t.Helper()
	entry := harness.CapabilityEntry(t, distribution, capability, "v1", "kos/conformance/storage-volume-v1")
	entry.Scope = v1beta1.CapabilityScopeCluster
	return extension.Declaration{
		Identifier: "thirdparty.kubevirt",
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Capabilities: extension.CapabilityContributions{
				Items:        []extension.Capability{entry},
				SelfReported: []extension.SelfReport{{Capability: capability, Version: "v1"}},
			},
		},
	}
}

// TestTC108ExtensionCapabilityGatesTheApplication covers the missing, installed
// and self-reported states.
func TestTC108ExtensionCapabilityGatesTheApplication(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.EnsureNamespace(t, appNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, definitionRef, []v1beta1.CapabilityRequirement{{
		Name: capability, Version: ">=v1",
	}}))
	cluster.CreateApplication(t, application, definitionRef, appNamespace)
	controller := cluster.ApplicationController(t, app.Options{Capabilities: provider.NewCatalogue(cluster.Writer())})

	// Without the extension the required capability is missing.
	if phase := cluster.SettleApplication(t, controller, application, 3); phase != app.PhasePending {
		t.Fatalf("phase without the extension = %q, want %q", phase, app.PhasePending)
	}
	missing, found := state.Find(cluster.ApplicationStatusOf(t, application).Conditions, app.ConditionCapabilityMissing)
	if !found || missing.Status != metav1.ConditionTrue || !strings.Contains(missing.Message, capability) {
		t.Fatalf("the missing capability is not named: %+v", missing)
	}

	// Installing the extension registers the capability, and the application
	// enters the install sequence.
	cluster.InstallPackage(t, component, declaration(t, distribution), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, component, distribution, nil)
	for range 5 {
		cluster.ApplicationRound(t, controller, application)
		if cluster.ApplicationPhase(t, application) != app.PhasePending {
			break
		}
	}
	if phase := cluster.ApplicationPhase(t, application); phase == app.PhasePending {
		t.Fatalf("phase after the install = %q, want the install to proceed", phase)
	}

	// The provider's own report is a declaration, not verification (C-10).
	status := cluster.ProviderStatusOf(t, component)
	result, found := harness.CapabilityResultOf(status, capability, "v1")
	if !found {
		t.Fatalf("%s has no verification result: %v", capability, status.Capabilities)
	}
	if result.State != conformance.StateUnverified || result.Source != conformance.SourceSelfReported {
		t.Fatalf("capability result = %s/%s, want %s/%s",
			result.State, result.Source, conformance.StateUnverified, conformance.SourceSelfReported)
	}
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionFalse, conformance.ReasonSelfReported)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionFalse, conformance.ReasonSelfReported)
}
