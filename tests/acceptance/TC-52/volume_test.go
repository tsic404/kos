// Volume refusals of C-34 through the production wiring: the preflight reads the
// size and class a volume is actually bound with, so a shrunk volume is refused
// and a storage class change asks for a migration without any test double
// standing in for the volume facts (AC-26, TC-52).
package tc52

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// volumeName is the volume declaration and the claim the volume module creates
// under the same name (C-38).
const volumeName = "data"

func TestTC52ShrunkVolumeIsRefused(t *testing.T) {
	cluster, controller, gateway, second := volumeApplication(t, "")
	// The claim asks for 1Gi and is bound with 10Gi: the volume wants a shrink,
	// which KOS refuses to perform (C-34).
	bindClaim(t, cluster, "1Gi", "10Gi", "fast", "fast")

	requestVolumeUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)

	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want the pre-upgrade %q", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the refused target to be unapplied", version)
	}
	condition := conditionOf(t, cluster, app.ConditionIrreversibleUpgrade)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the irreversible condition is missing: %+v", condition)
	}
	items, _ := statusMap(t, cluster, "upgrade")["irreversible"].([]any)
	if len(items) != 1 {
		t.Fatalf("irreversible = %v, want the shrunk volume", items)
	}
	item, _ := items[0].(map[string]any)
	if item["kind"] != "volumeShrink" {
		t.Fatalf("irreversible item = %v, want volumeShrink", item)
	}
	if object, _ := item["object"].(map[string]any); object["name"] != volumeName {
		t.Fatalf("the refused item names %v, want the volume %q", object["name"], volumeName)
	}
}

func TestTC52StorageClassChangeRequiresMigration(t *testing.T) {
	cluster, controller, gateway, second := volumeApplication(t, "")
	// The claim asks for "slow" and was bound with "fast": the upgrade needs a
	// migration object rather than a refusal (C-34).
	bindClaim(t, cluster, "1Gi", "1Gi", "slow", "fast")

	requestVolumeUpgrade(t, cluster, gateway, second.Name)
	reconcileOnce(t, controller)

	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want the pre-upgrade %q", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want the target to wait for the migration", version)
	}
	condition := conditionOf(t, cluster, app.ConditionMigrationRequired)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the migration condition is missing: %+v", condition)
	}
	migrations, _ := statusMap(t, cluster, "upgrade")["migrations"].([]any)
	if len(migrations) != 1 {
		t.Fatalf("migrations = %v, want one VolumeMigration", migrations)
	}
	migration, _ := migrations[0].(map[string]any)
	if migration["kind"] != "VolumeMigration" || migration["name"] != volumeName {
		t.Fatalf("migration = %v, want the VolumeMigration of %q", migration, volumeName)
	}
	if items, _ := statusMap(t, cluster, "upgrade")["irreversible"].([]any); len(items) != 0 {
		t.Fatalf("a storage class change was refused as irreversible: %v", items)
	}
}

// requestVolumeUpgrade writes the new definition reference into the application
// while keeping the volume declaration, so only the storage facts differ.
func requestVolumeUpgrade(t *testing.T, cluster *harness.Cluster, gateway *harness.ControlPlane, definition string) {
	t.Helper()
	update := harness.ApplicationManifest(applicationName, harness.Namespace, definition, targetNamespace, nil, []string{volumeName})
	update["metadata"] = map[string]any{
		"name":            applicationName,
		"namespace":       harness.Namespace,
		"labels":          map[string]any{"managed-by": "kos-shell"},
		"resourceVersion": cluster.Get(t, "v1beta1", "Application", applicationName).GetResourceVersion(),
	}
	if status, body := gateway.Call(t, http.MethodPut,
		"/api/v1/resources/Application/"+applicationName+"?namespace="+harness.Namespace, update, nil); status != http.StatusAccepted {
		t.Fatalf("request the upgrade: status %d, body %v", status, body)
	}
}

// volumeApplication installs one application that declares a volume and returns
// the controller, the control plane and the package the case upgrades to. The
// policy is the application's drift policy; an empty value keeps the default.
func volumeApplication(t *testing.T, policy string) (*harness.Cluster, *app.Controller, *harness.ControlPlane, *v1beta1.ApplicationDefinition) {
	t.Helper()
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	first := harness.DefinitionContent(t, "web-one", "", template("one"), nil, nil)
	second := harness.DefinitionContent(t, "web-two", "", template("two"), nil, nil)
	cluster.InstallDefinition(t, first)
	cluster.InstallDefinition(t, second)

	gateway := cluster.StartControlPlane(t, nil)
	manifest := harness.ApplicationManifest(applicationName, harness.Namespace, first.Name, targetNamespace, nil, []string{volumeName})
	if policy != "" {
		manifest["spec"].(map[string]any)["driftPolicy"] = policy
	}
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q after the install", phase, app.PhaseReady)
	}
	if version := downstreamVersion(t, cluster); version != "one" {
		t.Fatalf("downstream version = %q, want %q after the install", version, "one")
	}
	return cluster, controller, gateway, second
}

// bindClaim writes the claim the volume module creates and the volume it is bound
// with (C-38), so the preflight reads the facts the cluster actually holds.
func bindClaim(t *testing.T, cluster *harness.Cluster, requested, capacity, desiredClass, actualClass string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	volume := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-" + volumeName},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/srv/" + volumeName},
			},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			StorageClassName:              actualClass,
		},
	}
	if _, err := cluster.Kube.CoreV1().PersistentVolumes().Create(ctx, volume, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the bound volume: %v", err)
	}
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: volumeName, Namespace: targetNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(requested)}},
			StorageClassName: &desiredClass,
			VolumeName:       volume.GetName(),
		},
	}
	created, err := cluster.Kube.CoreV1().PersistentVolumeClaims(targetNamespace).Create(ctx, claim, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the claim: %v", err)
	}
	created.Status.Phase = corev1.ClaimBound
	created.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}
	if _, err := cluster.Kube.CoreV1().PersistentVolumeClaims(targetNamespace).UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("bind the claim: %v", err)
	}
}

// TestTC52VolumeShrinkPausesAfterAnExternalWrite covers the pause of C-35 for the
// volume class of C-34: a volume whose bound size is above the requested one is
// irreversible, so an external write suspends reconciliation exactly like a data
// format change does, and the drift policy still decides what the pause records.
func TestTC52VolumeShrinkPausesAfterAnExternalWrite(t *testing.T) {
	cases := []struct {
		policy  string
		message string
	}{
		{policy: "", message: "waits for the confirmation"},
		{policy: "adopt", message: "cannot be expressed by the config structure"},
	}
	for _, test := range cases {
		t.Run("policy="+test.policy, func(t *testing.T) {
			cluster, controller, _, _ := volumeApplication(t, test.policy)
			bindClaim(t, cluster, "1Gi", "10Gi", "fast", "fast")
			writeDownstreamVersion(t, cluster, "modified")

			reconcileOnce(t, controller)

			reconciled := state.ReconciledOf(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", applicationName)))
			if reconciled.Status != metav1.ConditionFalse || reconciled.Reason != app.ReasonExternalChangePending {
				t.Fatalf("Reconciled = %s/%s, want False/%s", reconciled.Status, reconciled.Reason, app.ReasonExternalChangePending)
			}
			if phase := phaseOf(t, cluster); phase != app.PhaseReady {
				t.Fatalf("phase = %q, want %q while the volume waits", phase, app.PhaseReady)
			}
			items, _ := statusMap(t, cluster, "upgrade")["irreversible"].([]any)
			if len(items) != 1 {
				t.Fatalf("irreversible = %v, want the shrunk volume", items)
			}
			if item, _ := items[0].(map[string]any); item["kind"] != "volumeShrink" {
				t.Fatalf("irreversible item = %v, want volumeShrink", item)
			}
			condition := conditionOf(t, cluster, app.ConditionDrifted)
			if condition == nil || condition.Status != metav1.ConditionTrue {
				t.Fatalf("the drift condition is missing: %+v", condition)
			}
			if !strings.Contains(condition.Message, test.message) {
				t.Fatalf("the drift condition of policy %q reads %q, want %q", test.policy, condition.Message, test.message)
			}
		})
	}
}

// writeDownstreamVersion writes one value into the rendered object outside KOS.
func writeDownstreamVersion(t *testing.T, cluster *harness.Cluster, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	configMap, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Get(ctx, applicationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the downstream ConfigMap: %v", err)
	}
	configMap.Data["version"] = version
	if _, err := cluster.Kube.CoreV1().ConfigMaps(targetNamespace).Update(ctx, configMap, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the downstream ConfigMap outside KOS: %v", err)
	}
}
