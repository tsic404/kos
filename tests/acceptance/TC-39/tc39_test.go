// Package tc39 verifies that a capability whose KOS-run case set fails in part
// is Degraded, lists every failed case, and shows its guarantee level as unmet
// on the applications that depend on it (AC-19, TC-39).
package tc39

import (
	"reflect"
	"strings"
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
)

// TestTC39PartialConformanceFailureDegrades runs a case set whose expansion and
// deletion cases fail.
func TestTC39PartialConformanceFailureDegrades(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, "kos.test/local-path")

	durable := true
	declaration := harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
				v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable}),
		},
	})
	cluster.InstallPackage(t, providerName, declaration, extension.MemoryArtifacts{})

	failed := []string{"volume.expand", "volume.delete"}
	cluster.ValidateProvider(t, providerName, distribution, harness.ProviderTargets(map[string]conformance.Target{
		providerName: harness.CaseTarget(failed...),
	}))

	status := cluster.ProviderStatusOf(t, providerName)
	result, found := harness.CapabilityResultOf(status, "storage.volume", "v1")
	if !found {
		t.Fatalf("storage.volume has no verification result: %v", status.Capabilities)
	}
	if result.State != conformance.StateDegraded {
		t.Fatalf("capability state = %s, want %s", result.State, conformance.StateDegraded)
	}
	if result.Source != conformance.SourceKOS {
		t.Fatalf("verification source = %q, want %q", result.Source, conformance.SourceKOS)
	}
	if !reflect.DeepEqual(result.FailedCases, failed) {
		t.Fatalf("failed cases = %v, want %v", result.FailedCases, failed)
	}
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionFalse, conformance.ReasonCasesFailed)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionFalse, conformance.ReasonCasesFailed)

	// The application side reads the guarantee level as unmet, installs anyway
	// and reports Degraded (C-10, C-11).
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
	if application.Health.State != v1beta1.HealthStateDegraded {
		t.Fatalf("application health = %s, want %s", application.Health.State, v1beta1.HealthStateDegraded)
	}
	condition, found := harness.FindCondition(t, cluster, "Application", applicationName, app.ConditionCapabilityUnsatisfied)
	if !found {
		t.Fatalf("the application carries no %s condition: %v", app.ConditionCapabilityUnsatisfied, application.Conditions)
	}
	if !strings.Contains(condition.Message, "storage.volume.durable") {
		t.Fatalf("the unsatisfied condition does not name the guarantee: %s", condition.Message)
	}
}
