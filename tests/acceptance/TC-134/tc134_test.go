// Package tc134 verifies C-09's scope values (AC-19, TC-134): a registration
// outside the two values is refused, both legal values are accepted, a
// baseline capability takes the kernel's value when it omits one, and an
// extension capability must declare one.
package tc134

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	storageClass = "local-path"
	provisioner  = "kos.test/local-path"
	caseSet      = "kos/conformance/storage-volume-v1"
)

// TestTC134RegistrationOutsideTheScopeValuesIsRefused writes a scope the two
// values do not cover and reads the refused registration.
func TestTC134RegistrationOutsideTheScopeValuesIsRefused(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	entry := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", caseSet)
	entry.Scope = "edge"
	cluster.InstallPackage(t, "edge-scope", harness.ProviderDeclaration("edge-scope", "1.0.0",
		extension.CapabilityContributions{
			StorageClasses: []string{storageClass},
			Items:          []extension.Capability{entry},
		}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, "edge-scope", distribution, nil)

	status := cluster.ProviderStatusOf(t, "edge-scope")
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionFalse, provider.ReasonInvalidRegistration)
	condition, found := harness.ConditionOf(status, provider.ConditionRegistered)
	if !found || !strings.Contains(condition.Message, "scope") {
		t.Fatalf("the refusal does not name the scope: %+v", condition)
	}
}

// TestTC134BaselineCapabilityTakesTheKernelScope checks both legal values on a
// baseline name: the omitted scope takes the kernel's node value, and an
// explicit cluster-level registration is accepted.
func TestTC134BaselineCapabilityTakesTheKernelScope(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	implicit := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", caseSet)
	cluster.InstallPackage(t, "implicit-scope", harness.ProviderDeclaration("implicit-scope", "1.0.0",
		extension.CapabilityContributions{
			StorageClasses: []string{storageClass},
			Items:          []extension.Capability{implicit},
		}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, "implicit-scope", distribution, nil)

	spec := cluster.ProviderSpecOf(t, "implicit-scope")
	if len(spec.Capabilities) != 1 || spec.Capabilities[0].Scope != v1beta1.CapabilityScopeNode {
		t.Fatalf("registered capabilities = %+v, want storage.volume at the baseline node scope", spec.Capabilities)
	}
	status := cluster.ProviderStatusOf(t, "implicit-scope")
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	if len(status.EffectiveCapabilities) != 1 || status.EffectiveCapabilities[0].Scope != v1beta1.CapabilityScopeNode {
		t.Fatalf("effective capabilities = %+v, want the node scope", status.EffectiveCapabilities)
	}

	explicit := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", caseSet)
	explicit.Scope = v1beta1.CapabilityScopeCluster
	cluster.InstallPackage(t, "explicit-scope", harness.ProviderDeclaration("explicit-scope", "1.0.0",
		extension.CapabilityContributions{
			StorageClasses: []string{storageClass},
			Items:          []extension.Capability{explicit},
		}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, "explicit-scope", distribution, nil)

	spec = cluster.ProviderSpecOf(t, "explicit-scope")
	if len(spec.Capabilities) != 1 || spec.Capabilities[0].Scope != v1beta1.CapabilityScopeCluster {
		t.Fatalf("registered capabilities = %+v, want storage.volume at cluster scope", spec.Capabilities)
	}
	status = cluster.ProviderStatusOf(t, "explicit-scope")
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
}

// TestTC134ExtensionCapabilityMustDeclareItsScope keeps an extension capability
// from leaning on the kernel baseline: without a declared scope the
// registration is refused, with one it is accepted.
func TestTC134ExtensionCapabilityMustDeclareItsScope(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()

	undeclared := harness.CapabilityEntry(t, distribution, "demo.widgets/queue", "v1", caseSet)
	cluster.InstallPackage(t, "demo.widgets", harness.ProviderDeclaration("demo.widgets", "1.0.0",
		extension.CapabilityContributions{Items: []extension.Capability{undeclared}}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, "demo.widgets", distribution, nil)

	status := cluster.ProviderStatusOf(t, "demo.widgets")
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionFalse, provider.ReasonInvalidRegistration)
	condition, found := harness.ConditionOf(status, provider.ConditionRegistered)
	if !found || !strings.Contains(condition.Message, "scope") {
		t.Fatalf("the refusal does not name the scope: %+v", condition)
	}

	declared := harness.CapabilityEntry(t, distribution, "demo.other/widgets", "v1", caseSet)
	declared.Scope = v1beta1.CapabilityScopeCluster
	cluster.InstallPackage(t, "demo.other", harness.ProviderDeclaration("demo.other", "1.0.0",
		extension.CapabilityContributions{Items: []extension.Capability{declared}}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, "demo.other", distribution, nil)

	status = cluster.ProviderStatusOf(t, "demo.other")
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	if len(status.EffectiveCapabilities) != 1 || status.EffectiveCapabilities[0].Scope != v1beta1.CapabilityScopeCluster {
		t.Fatalf("effective capabilities = %+v, want the declared cluster scope", status.EffectiveCapabilities)
	}
}
