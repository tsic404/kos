// Package tc44 verifies that a second provider offering the same capability
// registers without a kernel change and without touching anything but its own
// provider object: switching a volume's policy stays a user write on the
// volume, never a provider-name branch in the kernel (AC-22, TC-44).
package tc44

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	firstProvider   = "builtin-storage"
	secondProvider  = "acme-storage"
	firstClass      = "local-path"
	secondClass     = "acme-fast"
	appNamespace    = "apps"
	applicationName = "web"
	volumeName      = "data"
	workloadName    = "web"
)

// TestTC44SecondProviderNeedsNoKernelChange adds a provider of the same
// capability on one cluster and checks that only provider objects appear.
func TestTC44SecondProviderNeedsNoKernelChange(t *testing.T) {
	cluster := harness.Start(t)
	kindsBefore := servedKinds(t, cluster)
	assertFrozenBaseline(t)

	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, firstClass, "kos.test/local-path")
	cluster.CreateStorageClass(t, secondClass, "acme.test/fast")

	durable := true
	capability := func() extension.Capability {
		return harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
			v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable})
	}
	declaration := func(name, storageClass string) extension.Declaration {
		return harness.ProviderDeclaration(name, "1.0.0", extension.CapabilityContributions{
			StorageClasses: []string{storageClass},
			Items:          []extension.Capability{capability()},
		})
	}

	// The cluster already runs one provider, one application and one volume
	// under it.
	cluster.InstallPackage(t, firstProvider, declaration(firstProvider, firstClass), extension.MemoryArtifacts{})
	target := harness.CaseTarget()
	cluster.ValidateProvider(t, firstProvider, distribution, harness.ProviderTargets(map[string]conformance.Target{firstProvider: target}))
	harness.RequireProviderCondition(t, cluster.ProviderStatusOf(t, firstProvider), provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	cluster.EnsureNamespace(t, appNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, "needs-volume", []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "durable", Bool: &durable}},
	}}))
	cluster.CreateApplication(t, applicationName, "needs-volume", appNamespace)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest("Volume", harness.Namespace, volumeName,
		map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)})
	cluster.CreateClaim(t, harness.Namespace, volumeName, firstClass)
	controller := cluster.ApplicationController(t, app.Options{Capabilities: provider.NewCatalogue(cluster.Writer())})
	if phase := cluster.SettleApplication(t, controller, applicationName, 5); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", phase, app.PhaseReady)
	}
	before := versionsOf(t, cluster)

	// A second provider of the same capability, with its own storage class.
	cluster.InstallPackage(t, secondProvider, declaration(secondProvider, secondClass), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, secondProvider, distribution, harness.ProviderTargets(map[string]conformance.Target{
		firstProvider:  target,
		secondProvider: target,
	}))

	// Both register and verify; the verdict carries no provider name, so the
	// kernel cannot take a different path per provider (F11).
	verdicts := make(map[string]string, 2)
	for _, name := range []string{firstProvider, secondProvider} {
		status := cluster.ProviderStatusOf(t, name)
		harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
		harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionTrue, conformance.ReasonVerified)
		harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)
		result, found := harness.CapabilityResultOf(status, "storage.volume", "v1")
		if !found || result.State != conformance.StateVerified || result.Source != conformance.SourceKOS {
			t.Fatalf("%s verification = %+v, want a KOS-verified capability", name, result)
		}
		verdicts[name] = result.State + "/" + result.Source + "/" + result.Reason
	}
	if verdicts[firstProvider] != verdicts[secondProvider] {
		t.Fatalf("providers of the same capability produced different verdicts: %v", verdicts)
	}

	// Only the new provider appeared: the volume, its claim, the application
	// and the rendered workload keep the versions they had. Switching the
	// volume's policy is the user's write on the volume object, so provider
	// registration is observed as inert here.
	after := versionsOf(t, cluster)
	for key, version := range before {
		if after[key] != version {
			t.Fatalf("%s changed when a second provider registered: %s -> %s", key, version, after[key])
		}
	}

	// The capability stays in service for the running application.
	cluster.ApplicationRound(t, controller, applicationName)
	if phase := cluster.ApplicationPhase(t, applicationName); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q after the second provider registered", phase, app.PhaseReady)
	}

	// The closed kind set and the capability baseline did not change (C-01,
	// C-09): a new provider is data, not a kernel version.
	if kindsAfter := servedKinds(t, cluster); !slices.Equal(kindsAfter, kindsBefore) {
		t.Fatalf("the served kernel kinds changed: %v -> %v", kindsBefore, kindsAfter)
	}
	assertFrozenBaseline(t)
}

// versionsOf records the resource version of every object provider
// registration must leave alone.
func versionsOf(t *testing.T, cluster *harness.Cluster) map[string]string {
	t.Helper()
	versions := map[string]string{
		"Application": cluster.Get(t, "v1beta1", "Application", applicationName).GetResourceVersion(),
		"Volume":      cluster.Get(t, "v1beta1", "Volume", volumeName).GetResourceVersion(),
	}
	claim, err := cluster.Kube.CoreV1().PersistentVolumeClaims(harness.Namespace).Get(t.Context(), volumeName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the bound claim: %v", err)
	}
	versions["PersistentVolumeClaim"] = claim.GetResourceVersion()
	workload := cluster.Direct(t, "", "v1", "ConfigMap", appNamespace)
	object, err := workload.Get(t.Context(), workloadName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the rendered workload: %v", err)
	}
	versions["ConfigMap"] = object.GetResourceVersion()
	return versions
}

// servedKinds returns the sorted kinds the kernel group serves.
func servedKinds(t *testing.T, cluster *harness.Cluster) []string {
	t.Helper()
	resources, err := cluster.Kube.Discovery().ServerResourcesForGroupVersion(api.Group + "/v1beta1")
	if err != nil {
		t.Fatalf("discover %s: %v", api.Group, err)
	}
	kinds := make([]string, 0, len(resources.APIResources))
	for _, resource := range resources.APIResources {
		if resource.Kind == "" || strings.Contains(resource.Name, "/") {
			continue
		}
		kinds = append(kinds, resource.Kind)
	}
	slices.Sort(kinds)
	return kinds
}

// baselineNames returns the kernel capability baseline as name@version.
func baselineNames() []string {
	names := make([]string, 0, len(extension.BaselineCapabilities()))
	for _, capability := range extension.BaselineCapabilities() {
		names = append(names, capability.Name+"@"+capability.Version)
	}
	return names
}

// frozenBaseline is C-09's eight-capability baseline written out as data.
// Comparing baselineNames() against itself would accept any change, so the
// expectation has to come from somewhere other than the code under test.
var frozenBaseline = []string{
	"storage.volume@v1",
	"storage.snapshot@v1",
	"storage.shared-rwx@v1",
	"network.http-endpoint@v1",
	"network.path-prefix@v1",
	"network.l4-endpoint@v1",
	"network.policy@v1",
	"compute.gpu@v1",
}

// assertFrozenBaseline fails when the kernel baseline differs from C-09's.
func assertFrozenBaseline(t *testing.T) {
	t.Helper()
	if names := baselineNames(); !slices.Equal(names, frozenBaseline) {
		t.Fatalf("the capability baseline = %v, want C-09's %v", names, frozenBaseline)
	}
}
