// Package tc42 verifies C-48's withdrawal rule: an upgrade that drops a
// capability still referenced is not rejected, but the provider blocks at
// UpgradeBlocked, the old capability set keeps serving, and the dependent
// application's workload is not rolled (AC-21, TC-42).
package tc42

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	providerName    = "builtin-storage"
	storageClass    = "local-path"
	appNamespace    = "apps"
	applicationName = "web"
	workloadName    = "web"
)

// TestTC42WithdrawnCapabilityBlocksTheUpgrade upgrades the provider to a
// declaration that no longer carries storage.volume.
func TestTC42WithdrawnCapabilityBlocksTheUpgrade(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, "kos.test/local-path")

	durable := true
	volume := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
		v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable})
	snapshot := harness.CapabilityEntry(t, distribution, "storage.snapshot", "v1", "kos/conformance/storage-snapshot-v1")
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items:          []extension.Capability{volume, snapshot},
	}), extension.MemoryArtifacts{})
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	harness.RequireProviderCondition(t, cluster.ProviderStatusOf(t, providerName), provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	cluster.EnsureNamespace(t, appNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, "needs-volume", []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "durable", Bool: &durable}},
	}}))
	cluster.CreateApplication(t, applicationName, "needs-volume", appNamespace)
	controller := cluster.ApplicationController(t, app.Options{Capabilities: provider.NewCatalogue(cluster.Writer())})
	if phase := cluster.SettleApplication(t, controller, applicationName, 5); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q before the upgrade", phase, app.PhaseReady)
	}
	workload := cluster.Direct(t, "", "v1", "ConfigMap", appNamespace)
	before, err := workload.Get(t.Context(), workloadName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the rendered workload: %v", err)
	}

	// The upgrade writes a declaration without storage.volume; the write is
	// accepted and the provider has to block its advance (C-48).
	cluster.UpgradePackage(t, providerName, harness.ProviderDeclaration(providerName, "1.1.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items:          []extension.Capability{snapshot},
	}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, providerName, distribution, targets)

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionUpgradeBlocked, metav1.ConditionTrue, provider.ReasonWithdrawn)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionFalse, provider.ReasonWithdrawn)
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionTrue, conformance.ReasonVerified)

	// The effective set stays at the old set (C-48).
	effective := make([]string, 0, len(status.EffectiveCapabilities))
	for _, capability := range status.EffectiveCapabilities {
		effective = append(effective, capability.Name+"@"+capability.Version)
	}
	if !slices.Contains(effective, "storage.volume@v1") {
		t.Fatalf("effective capabilities = %v, want storage.volume@v1 still in service", effective)
	}
	references := harness.ReferenceNames(status.References)
	if !slices.Contains(references, "ApplicationDefinition needs-volume") {
		t.Fatalf("references = %v, want the definition that requires the capability", references)
	}
	if !slices.Contains(references, "Application "+harness.Namespace+"/"+applicationName) {
		t.Fatalf("references = %v, want the dependent application", references)
	}

	// The old capability keeps serving: another application round leaves the
	// application Ready and its rendered workload untouched.
	cluster.ApplicationRound(t, controller, applicationName)
	if phase := cluster.ApplicationPhase(t, applicationName); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q after the withdrawal", phase, app.PhaseReady)
	}
	after, err := workload.Get(t.Context(), workloadName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-read the rendered workload: %v", err)
	}
	if before.GetResourceVersion() != after.GetResourceVersion() {
		t.Fatalf("the workload was rewritten: resourceVersion %s -> %s",
			before.GetResourceVersion(), after.GetResourceVersion())
	}
}

// TestTC42RefusedRegistrationKeepsTheEffectiveSet pins the withdrawal baseline
// C-48 depends on: a registration refused for a metadata error withdraws
// nothing, so the following accepted upgrade still sees the capability it
// dropped and blocks the advance.
func TestTC42RefusedRegistrationKeepsTheEffectiveSet(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, "kos.test/local-path")

	durable := true
	volume := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
		v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable})
	snapshot := harness.CapabilityEntry(t, distribution, "storage.snapshot", "v1", "kos/conformance/storage-snapshot-v1")
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items:          []extension.Capability{volume, snapshot},
	}), extension.MemoryArtifacts{})
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	harness.RequireProviderCondition(t, cluster.ProviderStatusOf(t, providerName), provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	cluster.EnsureNamespace(t, appNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, "needs-volume", []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "durable", Bool: &durable}},
	}}))

	// The middle upgrade drops storage.volume and writes a storage class that
	// is not a DNS name, so the registration itself is refused. That refusal
	// must not take the set in service with it.
	cluster.UpgradePackage(t, providerName, harness.ProviderDeclaration(providerName, "1.1.0", extension.CapabilityContributions{
		StorageClasses: []string{"Not_A_DNS_Name"},
		Items:          []extension.Capability{snapshot},
	}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	refused := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, refused, provider.ConditionRegistered, metav1.ConditionFalse, provider.ReasonInvalidRegistration)
	requireEffective(t, refused, "storage.volume@v1")

	// The next upgrade fixes the class name and still drops the capability.
	// The withdrawal is now judged against the preserved baseline: it is still
	// referenced, so the advance blocks and the old set keeps serving.
	cluster.UpgradePackage(t, providerName, harness.ProviderDeclaration(providerName, "1.2.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items:          []extension.Capability{snapshot},
	}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	harness.RequireProviderCondition(t, status, provider.ConditionUpgradeBlocked, metav1.ConditionTrue, provider.ReasonWithdrawn)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionFalse, provider.ReasonWithdrawn)
	requireEffective(t, status, "storage.volume@v1")
}

// requireEffective fails when one capability is missing from the set in
// service.
func requireEffective(t *testing.T, status v1beta1.ProviderStatus, want string) {
	t.Helper()
	names := make([]string, 0, len(status.EffectiveCapabilities))
	for _, capability := range status.EffectiveCapabilities {
		names = append(names, capability.Name+"@"+capability.Version)
	}
	if !slices.Contains(names, want) {
		t.Fatalf("effective capabilities = %v, want %s in service", names, want)
	}
}
