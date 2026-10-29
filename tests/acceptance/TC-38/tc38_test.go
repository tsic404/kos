// Package tc38 verifies that a distribution built-in provider registers its
// capabilities and that only a KOS-run conformance, over a case set whose
// digest matches the distribution, verifies them (AC-19, TC-38).
package tc38

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	providerName = "builtin-storage"
	storageClass = "local-path"
	provisioner  = "kos.test/local-path"
)

// TestTC38BuiltinProviderRegisters checks the registration declaration, the
// storage class the provider supplies, and the KOS-sourced verification.
func TestTC38BuiltinProviderRegisters(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	durable := true
	declaration := harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
				v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable}),
			harness.CapabilityEntry(t, distribution, "storage.snapshot", "v1", "kos/conformance/storage-snapshot-v1"),
		},
	})
	installation := cluster.InstallPackage(t, providerName, declaration, extension.MemoryArtifacts{})
	if result, found := installation.Component(providerName); !found || result.Phase != extension.PhaseReady {
		t.Fatalf("the built-in provider component is not Ready: %+v", result)
	}

	// The registration declaration is readable, and the provider belongs to
	// the component that declared it (C-02, C-47).
	object := cluster.Get(t, "v1beta1", "Provider", providerName)
	component := cluster.Get(t, "v1beta1", "SystemComponent", providerName)
	if owner := object.GetLabels()[markers.OwnerUID]; owner != string(component.GetUID()) {
		t.Fatalf("provider owner-uid = %q, want the component UID %q", owner, component.GetUID())
	}
	spec := cluster.ProviderSpecOf(t, providerName)
	if spec.ComponentRef != providerName || spec.Version != "1.0.0" {
		t.Fatalf("registration = %s@%s, want %s@1.0.0", spec.ComponentRef, spec.Version, providerName)
	}
	if len(spec.Capabilities) != 2 {
		t.Fatalf("registered capabilities = %v, want two", spec.Capabilities)
	}
	for _, capability := range spec.Capabilities {
		if capability.CaseSet == "" || capability.CaseSetDigest == "" {
			t.Fatalf("capability %s@%s carries no conformance case set", capability.Name, capability.Version)
		}
	}
	if len(spec.StorageClasses) != 1 || spec.StorageClasses[0] != storageClass {
		t.Fatalf("declared storage classes = %v, want [%s]", spec.StorageClasses, storageClass)
	}
	assertStorageClasses(t, cluster, spec.StorageClasses)

	cluster.ValidateProvider(t, providerName, distribution, harness.ProviderTargets(map[string]conformance.Target{
		providerName: harness.CaseTarget(),
	}))

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionTrue, conformance.ReasonVerified)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)
	harness.RequireProviderCondition(t, status, provider.ConditionUpgradeBlocked, metav1.ConditionFalse, provider.ReasonNoWithdrawal)
	harness.RequireProviderCondition(t, status, provider.ConditionDeprovisionBlocked, metav1.ConditionFalse, provider.ReasonNoDependents)
	if len(status.Capabilities) != 2 || len(status.EffectiveCapabilities) != 2 {
		t.Fatalf("status capabilities = %v, effective = %v, want both sets of two",
			status.Capabilities, status.EffectiveCapabilities)
	}
	for _, capability := range status.Capabilities {
		if capability.State != conformance.StateVerified {
			t.Errorf("%s@%s = %s, want %s", capability.Name, capability.Version, capability.State, conformance.StateVerified)
		}
		if capability.Source != conformance.SourceKOS {
			t.Errorf("%s@%s verified by %q, want %q", capability.Name, capability.Version, capability.Source, conformance.SourceKOS)
		}
		if len(capability.FailedCases) != 0 {
			t.Errorf("%s@%s lists failed cases %v", capability.Name, capability.Version, capability.FailedCases)
		}
	}
}

// assertStorageClasses checks every class the provider declares against the
// cluster that supplies it (C-47). The model links a provider to a class by
// name, so "the declared supplier matches" is judged here as name resolution
// plus a provisioner the cluster itself reports: the provisioner value is the
// provider's own identifier, and KOS holds nothing to compare it against.
func assertStorageClasses(t *testing.T, cluster *harness.Cluster, declared []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if len(declared) == 0 {
		t.Fatal("the provider declares no storage class")
	}
	for _, name := range declared {
		class, err := cluster.Kube.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("declared storage class %s is not a cluster object: %v", name, err)
		}
		if class.Provisioner == "" {
			t.Fatalf("storage class %s reports no provisioner", name)
		}
	}
}

// TestTC38TwoVersionsOfOneCapability pins C-09's version rule: one provider may
// register the same capability at two versions at once, and both registrations
// are readable in the status.
func TestTC38TwoVersionsOfOneCapability(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
			harness.CapabilityEntry(t, distribution, "storage.volume", "v2", "kos/conformance/storage-volume-v1"),
		},
	}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, providerName, distribution, harness.ProviderTargets(map[string]conformance.Target{
		providerName: harness.CaseTarget(),
	}))

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	for _, version := range []string{"v1", "v2"} {
		if _, found := harness.CapabilityResultOf(status, "storage.volume", version); !found {
			t.Fatalf("storage.volume@%s is not registered: %v", version, status.Capabilities)
		}
	}
}

// TestTC38ProviderWithoutCapabilities pins C-47's empty case: a component that
// contributes no capability still registers, and the empty registration is
// Ready.
func TestTC38ProviderWithoutCapabilities(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()

	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0",
		extension.CapabilityContributions{}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, providerName, distribution, nil)

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionTrue, provider.ReasonNoCapabilities)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)
}

// TestTC38RecreatedProviderRunsItsOwnRound pins C-10 across object identity: a
// provider that is deleted and re-created under the same name with the same
// declaration must run its case set again instead of inheriting the previous
// object's conclusion.
func TestTC38RecreatedProviderRunsItsOwnRound(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	cases := 0
	target := conformance.TargetFunc(func(context.Context, string, string, string) error {
		cases++
		return nil
	})
	// One reconciler answers for both objects, as the running controller does.
	reconciler := provider.NewReconciler(cluster.Writer(), provider.Options{
		Distribution: distribution,
		Targets:      conformance.MapTargets{providerName: target},
	})

	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0",
		extension.CapabilityContributions{
			StorageClasses: []string{storageClass},
			Items: []extension.Capability{
				harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
			},
		}), artifacts)
	cluster.ValidateProviderWith(t, reconciler, providerName)

	first := cases
	if first == 0 {
		t.Fatal("the first provider ran no conformance case")
	}
	status := cluster.ProviderStatusOf(t, providerName)
	if result, found := harness.CapabilityResultOf(status, "storage.volume", "v1"); !found || result.Source != conformance.SourceKOS {
		t.Fatalf("the first provider's result = %+v, want a KOS run", status.Capabilities)
	}

	// Nothing depends on the provider, so the deletion goes through and the
	// round that sees the missing object drops its cache entry.
	cluster.DeleteProvider(t, providerName)
	cluster.ValidateProviderWith(t, reconciler, providerName)
	if cluster.ProviderExists(t, providerName) {
		t.Fatal("the provider is still present although nothing depends on it")
	}
	cluster.ValidateProviderWith(t, reconciler, providerName)

	// The component path registers the same declaration again as a new object.
	cluster.ExtensionState(t, artifacts)
	cluster.ValidateProviderWith(t, reconciler, providerName)

	status = cluster.ProviderStatusOf(t, providerName)
	result, found := harness.CapabilityResultOf(status, "storage.volume", "v1")
	if !found || result.Source != conformance.SourceKOS || result.State != conformance.StateVerified {
		t.Fatalf("the re-created provider's result = %+v, want its own KOS run", status.Capabilities)
	}
	if cases <= first {
		t.Fatalf("the re-created provider ran %d cases, want more than the previous object's %d", cases, first)
	}
}
