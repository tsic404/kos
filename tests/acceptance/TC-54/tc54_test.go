// Package tc54 verifies the phase rules of C-33: a missing capability and a
// missing namespace each stop the application at Pending with their own reason
// and nothing is installed; once the prerequisites hold, Installing precedes
// Ready (AC-27, TC-54).
package tc54

import (
	"context"
	"net/http"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace  = "apps"
	missingNamespace = "acceptance-does-not-exist"
)

func TestTC54PhaseProgressionAndPending(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)

	present := harness.Definition(t, "needs-block", []v1beta1.CapabilityRequirement{{
		Name: "storage.block",
		Guarantees: []v1beta1.GuaranteeRequirement{
			{Key: "snapshot", Bool: new(true)},
		},
	}})
	absent := harness.Definition(t, "needs-absent", []v1beta1.CapabilityRequirement{{
		Name:       "storage.snapshot",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "snapshot", Bool: new(true)}},
	}})
	cluster.InstallDefinition(t, present)
	cluster.InstallDefinition(t, absent)

	gateway := cluster.StartControlPlane(t, nil)
	for name, manifest := range map[string]map[string]any{
		"no-capability": harness.ApplicationManifest("no-capability", harness.Namespace, "needs-absent", targetNamespace, nil, nil),
		"no-namespace":  harness.ApplicationManifest("no-namespace", harness.Namespace, "needs-block", missingNamespace, nil, nil),
		"installs":      harness.ApplicationManifest("installs", harness.Namespace, "needs-block", targetNamespace, nil, nil),
	} {
		if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
			t.Fatalf("create %s: status %d, body %v", name, status, body)
		}
	}

	registered := app.StaticCapabilities{{
		Name:       "storage.block",
		Version:    "1.2.0",
		Guarantees: map[string]app.Guarantee{"snapshot": {Bool: new(true)}},
		Verified:   true,
	}}
	controller := cluster.ApplicationController(t, app.Options{Capabilities: registered})

	for name, reason := range map[string]string{
		"no-capability": app.ConditionCapabilityMissing,
		"no-namespace":  app.ConditionNamespaceMissing,
	} {
		reconcileOnce(t, controller, name)
		assertPending(t, cluster, name, reason)
		if downstreamExists(t, cluster, name) {
			t.Errorf("%s: a pending application installed its workload", name)
		}
	}

	reconcileOnce(t, controller, "installs")
	if phase := phaseOf(t, cluster, "installs"); phase != app.PhaseInstalling {
		t.Fatalf("phase = %q, want %q before the render", phase, app.PhaseInstalling)
	}
	reconcileOnce(t, controller, "installs")
	if phase := phaseOf(t, cluster, "installs"); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}
	if !downstreamExists(t, cluster, "installs") {
		t.Fatal("the installed application has no downstream object")
	}
}

// assertPending checks the phase, the blocked reason and the failure condition.
func assertPending(t *testing.T, cluster *harness.Cluster, name, reason string) {
	t.Helper()
	if phase := phaseOf(t, cluster, name); phase != app.PhasePending {
		t.Fatalf("%s: phase = %q, want %q", name, phase, app.PhasePending)
	}
	conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", name))
	blocked := state.ReconciledOf(conditions)
	if blocked.Status != metav1.ConditionFalse || blocked.Reason != reason {
		t.Fatalf("%s: Reconciled = %s/%s, want False/%s", name, blocked.Status, blocked.Reason, reason)
	}
	failure, found := state.Find(conditions, reason)
	if !found || failure.Status != metav1.ConditionTrue {
		t.Fatalf("%s: the failure condition %s is missing or not True", name, reason)
	}
}

// phaseOf reads the stored phase of one application.
func phaseOf(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", name).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read phase of %s: %v", name, err)
	}
	return phase
}

// downstreamExists reports whether the render of one application was applied.
func downstreamExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	_, err := cluster.Direct(t, "", "v1", "ConfigMap", targetNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the downstream object of %s: %v", name, err)
	}
	return true
}

// reconcileOnce drives one round of one application.
func reconcileOnce(t *testing.T, controller *app.Controller, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: name,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}
