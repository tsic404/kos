// Package tc133 verifies C-11's scope comparison (AC-27, TC-133): a
// cluster-level requirement is not satisfied by a node-level registration and
// keeps the application Pending, the same requirement declared node-level
// installs, an optional requirement only degrades, and a requirement that omits
// the scope still installs against a node-level registration.
package tc133

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
	providerName = "builtin-storage"
	storageClass = "local-path"
	provisioner  = "kos.test/local-path"
	caseSet      = "kos/conformance/storage-volume-v1"
	appNamespace = "apps"
	requiredApp  = "cluster-required"
	nodeApp      = "node-required"
	optionalApp  = "cluster-optional"
	omittedApp   = "scope-omitted"
	clusterDef   = "needs-cluster-volume"
	nodeDef      = "needs-node-volume"
	optionalDef  = "needs-cluster-volume-optionally"
	omittedDef   = "needs-volume-scope-omitted"
)

// TestTC133ClusterRequirementIsNotMetByANodeCapability registers a node-level
// provider and compares four requirements against it.
func TestTC133ClusterRequirementIsNotMetByANodeCapability(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	durable := true
	entry := harness.CapabilityEntry(t, distribution, "storage.volume", "v1", caseSet,
		v1beta1.CapabilityGuarantee{Key: "durable", Bool: &durable})
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0",
		extension.CapabilityContributions{
			StorageClasses: []string{storageClass},
			Items:          []extension.Capability{entry},
		}), extension.MemoryArtifacts{})
	cluster.ValidateProvider(t, providerName, distribution, harness.ProviderTargets(map[string]conformance.Target{
		providerName: harness.CaseTarget(),
	}))

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionVerified, metav1.ConditionTrue, conformance.ReasonVerified)
	if len(status.EffectiveCapabilities) != 1 || status.EffectiveCapabilities[0].Scope != v1beta1.CapabilityScopeNode {
		t.Fatalf("effective capabilities = %+v, want the baseline node scope", status.EffectiveCapabilities)
	}

	cluster.EnsureNamespace(t, appNamespace)
	guarantee := []v1beta1.GuaranteeRequirement{{Key: "durable", Bool: &durable}}
	cluster.InstallDefinition(t, harness.Definition(t, clusterDef, []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Scope:      v1beta1.CapabilityScopeCluster,
		Guarantees: guarantee,
	}}))
	cluster.InstallDefinition(t, harness.Definition(t, nodeDef, []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Scope:      v1beta1.CapabilityScopeNode,
		Guarantees: guarantee,
	}}))
	cluster.InstallDefinition(t, harness.Definition(t, optionalDef, []v1beta1.CapabilityRequirement{{
		Name:     "storage.volume",
		Scope:    v1beta1.CapabilityScopeCluster,
		Optional: true,
	}}))
	// A requirement that omits the scope reads as node-level (C-11), so the
	// node-level registration installs it.
	cluster.InstallDefinition(t, harness.Definition(t, omittedDef, []v1beta1.CapabilityRequirement{{
		Name:       "storage.volume",
		Guarantees: guarantee,
	}}))

	cluster.CreateApplication(t, requiredApp, clusterDef, appNamespace)
	cluster.CreateApplication(t, nodeApp, nodeDef, appNamespace)
	cluster.CreateApplication(t, optionalApp, optionalDef, appNamespace)
	cluster.CreateApplication(t, omittedApp, omittedDef, appNamespace)
	controller := cluster.ApplicationController(t, app.Options{Capabilities: provider.NewCatalogue(cluster.Writer())})

	// The cluster-level requirement is not met by the node-level registration.
	if phase := cluster.SettleApplication(t, controller, requiredApp, 3); phase != app.PhasePending {
		t.Fatalf("phase = %q, want %q", phase, app.PhasePending)
	}
	missing, found := state.Find(cluster.ApplicationStatusOf(t, requiredApp).Conditions, app.ConditionCapabilityMissing)
	if !found || missing.Status != metav1.ConditionTrue || !strings.Contains(missing.Message, "storage.volume") {
		t.Fatalf("the missing capability is not named: %+v", missing)
	}

	// The same capability declared node-level installs.
	if phase := cluster.SettleApplication(t, controller, nodeApp, 5); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}

	// An optional cluster-level requirement installs and reports the shortfall.
	if phase := cluster.SettleApplication(t, controller, optionalApp, 5); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}
	optional := cluster.ApplicationStatusOf(t, optionalApp)
	unsatisfied, found := state.Find(optional.Conditions, app.ConditionCapabilityUnsatisfied)
	if !found || unsatisfied.Status != metav1.ConditionTrue || !strings.Contains(unsatisfied.Message, "storage.volume") {
		t.Fatalf("the optional shortfall is not reported: %+v", optional.Conditions)
	}
	if optional.Health.State != v1beta1.HealthStateDegraded {
		t.Fatalf("health = %q, want %q", optional.Health.State, v1beta1.HealthStateDegraded)
	}

	// With only a node-level registration in the cluster, an omitted scope
	// still installs: it is judged node-level, not missing.
	if phase := cluster.SettleApplication(t, controller, omittedApp, 5); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}
	if missing, found := state.Find(cluster.ApplicationStatusOf(t, omittedApp).Conditions, app.ConditionCapabilityMissing); found && missing.Status == metav1.ConditionTrue {
		t.Fatalf("the omitted scope was judged missing: %+v", missing)
	}
}
