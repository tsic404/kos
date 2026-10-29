// Package tc43 verifies that a provider written only from the documented
// registration fields passes the distribution's case set, and that a packaged
// application reaches Ready without any kernel change (AC-22, TC-43).
package tc43

import (
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	providerName    = "acme-storage"
	storageClass    = "acme-fast"
	provisioner     = "acme.test/fast"
	appNamespace    = "apps"
	applicationName = "web"
)

// TestTC43MinimalProviderPassesConformance implements a third-party provider
// from the documented fields and case set only.
func TestTC43MinimalProviderPassesConformance(t *testing.T) {
	cluster := harness.Start(t)
	kindsBefore := servedKinds(t, cluster)
	assertFrozenBaseline(t)

	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	durable := true
	capability := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
		v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable})
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items:          []extension.Capability{capability},
	}), extension.MemoryArtifacts{})

	spec := cluster.ProviderSpecOf(t, providerName)
	if len(spec.Capabilities) != 1 {
		t.Fatalf("registered capabilities = %v, want one", spec.Capabilities)
	}
	digest, found := distribution.Digest("kos/conformance/storage-volume-v1")
	if !found || spec.Capabilities[0].CaseSetDigest != digest {
		t.Fatalf("registered digest %q, want the distribution digest %q", spec.Capabilities[0].CaseSetDigest, digest)
	}

	cluster.ValidateProvider(t, providerName, distribution, harness.ProviderTargets(map[string]conformance.Target{
		providerName: harness.CaseTarget(),
	}))
	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionTrue, conformance.ReasonVerified)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)
	result, found := harness.CapabilityResultOf(status, "storage.volume", "v1")
	if !found || result.Source != conformance.SourceKOS || len(result.FailedCases) != 0 {
		t.Fatalf("verification result = %+v, want a KOS run with no failed case", result)
	}

	// The packaged application installs against the verified capability.
	cluster.EnsureNamespace(t, appNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, "needs-volume", []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "durable", Bool: &durable}},
	}}))
	cluster.CreateApplication(t, applicationName, "needs-volume", appNamespace)
	controller := cluster.ApplicationController(t, app.Options{Capabilities: provider.NewCatalogue(cluster.Writer())})
	if phase := cluster.SettleApplication(t, controller, applicationName, 5); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", phase, app.PhaseReady)
	}
	application := cluster.ApplicationStatusOf(t, applicationName)
	if _, unsatisfied := findCondition(application.Conditions, app.ConditionCapabilityUnsatisfied); unsatisfied {
		t.Fatal("the application reports an unsatisfied capability although the provider is verified")
	}
	if len(application.Rendered) == 0 {
		t.Fatal("the application rendered no downstream object")
	}

	// Adding the provider changed neither the closed kind set nor the
	// capability baseline (C-01, C-09).
	if kindsAfter := servedKinds(t, cluster); !slices.Equal(kindsAfter, kindsBefore) {
		t.Fatalf("the served kernel kinds changed: %v -> %v", kindsBefore, kindsAfter)
	}
	assertFrozenBaseline(t)
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

// findCondition looks one condition up by type.
func findCondition(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
