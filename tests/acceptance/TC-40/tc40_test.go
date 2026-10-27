// Package tc40 verifies that a provider's own conformance report is a
// declaration, not proof: the capability stays unverified, the verification
// condition names the self-report, and a dependent application sees the
// guarantee level as unverified (AC-20, TC-40).
package tc40

import (
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
	providerName    = "acme-storage"
	storageClass    = "acme-fast"
	appNamespace    = "apps"
	applicationName = "web"
)

// TestTC40SelfReportDoesNotVerify registers a provider that submits only its own
// result and never lets KOS run a case set.
func TestTC40SelfReportDoesNotVerify(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, "acme.test/fast")

	durable := true
	declaration := harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1",
				v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable}),
		},
		SelfReported: []extension.SelfReport{{Capability: "storage.volume", Version: "v1"}},
	})
	cluster.InstallPackage(t, providerName, declaration, extension.MemoryArtifacts{})

	// The self-report is readable as part of the registration declaration,
	// next to — not inside — the verification result (C-47).
	spec := cluster.ProviderSpecOf(t, providerName)
	if len(spec.SelfReported) != 1 || spec.SelfReported[0].Capability != "storage.volume" {
		t.Fatalf("self-reported results = %v, want one for storage.volume", spec.SelfReported)
	}

	// No provider implementation is registered, so the kernel runs nothing and
	// the declaration is the only result (C-10).
	cluster.ValidateProvider(t, providerName, distribution, nil)

	status := cluster.ProviderStatusOf(t, providerName)
	result, found := harness.CapabilityResultOf(status, "storage.volume", "v1")
	if !found {
		t.Fatalf("storage.volume has no result: %v", status.Capabilities)
	}
	if result.State != conformance.StateUnverified {
		t.Fatalf("capability state = %s, want %s", result.State, conformance.StateUnverified)
	}
	if result.Source != conformance.SourceSelfReported {
		t.Fatalf("result source = %q, want %q", result.Source, conformance.SourceSelfReported)
	}
	if result.Reason != conformance.ReasonSelfReported {
		t.Fatalf("result reason = %q, want %q", result.Reason, conformance.ReasonSelfReported)
	}
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionTrue, provider.ReasonRegistered)
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionFalse, conformance.ReasonSelfReported)
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionFalse, conformance.ReasonSelfReported)

	// The dependent application installs but shows the unverified guarantee
	// (C-10, C-11).
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
