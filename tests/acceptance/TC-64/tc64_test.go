// Package tc64 deletes volumes with an empty, a wrong and the correct purge
// token: the first two keep the claim with its release marker, only the third
// removes it, and the released claims are what residual detection lists
// (AC-32, TC-64).
package tc64

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	namespace = "data"
	class     = "standard"
	driver    = "standard.example"
)

func TestTC64PurgeToken(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, namespace)
	cluster.StorageClass(t, class, driver, true, "Retain", map[string]string{"fsType": "ext4"})
	controller := cluster.VolumeController(t, volume.Options{})

	for _, name := range []string{"empty-token", "wrong-token", "right-token"} {
		bind(t, cluster, controller, name)
	}

	release(t, cluster, controller, "empty-token", "")
	release(t, cluster, controller, "wrong-token", "not-the-volume-name")
	release(t, cluster, controller, "right-token", "right-token")

	for _, name := range []string{"empty-token", "wrong-token"} {
		claim := cluster.Claim(t, namespace, name)
		if claim.GetLabels()["released"] == "" {
			t.Fatalf("claim %q was kept but carries no release marker: %v", name, claim.GetLabels())
		}
	}
	if _, err := cluster.Direct(t, "", "v1", "PersistentVolumeClaim", namespace).Get(context.Background(), "right-token", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("claim of the purged volume still exists (err = %v)", err)
	}

	residuals, err := controller.Residuals(context.Background(), namespace)
	if err != nil {
		t.Fatalf("list residuals: %v", err)
	}
	got := make(map[string]bool, len(residuals))
	for _, residual := range residuals {
		got[residual.Name] = true
	}
	if !got["empty-token"] || !got["wrong-token"] {
		t.Fatalf("residuals = %v, want the two released claims", residuals)
	}
	if got["right-token"] {
		t.Fatalf("the purged claim is listed as a residual: %v", residuals)
	}

	importReleased(t, cluster, controller, "empty-token")
}

// bind creates one volume and drives it to Bound, so the claim it owns exists
// before the delete path runs.
func bind(t *testing.T, cluster *harness.Cluster, controller *volume.Controller, name string) {
	t.Helper()
	spec := map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}
	cluster.CreateIn(t, namespace, harness.Volume(namespace, name, spec))
	harness.ReconcileVolume(t, controller, namespace, name)
	cluster.PersistentVolume(t, "pv-"+name, class, driver, "Retain")
	cluster.BindClaim(t, namespace, name, "pv-"+name)
	harness.ReconcileVolume(t, controller, namespace, name)
	stored := volumeOf(t, cluster, name)
	if stored.Status.Phase != volume.PhaseBound {
		t.Fatalf("volume %q is %q, want %q before the delete", name, stored.Status.Phase, volume.PhaseBound)
	}
}

// release deletes one volume with the given purge token and requires the
// volume object to disappear: the reconciler wrote the marker or removed the
// claim and then released its finalizer.
func release(t *testing.T, cluster *harness.Cluster, controller *volume.Controller, name, token string) {
	t.Helper()
	if token != "" {
		cluster.SetPurgeToken(t, namespace, name, token)
	}
	cluster.DeleteVolume(t, namespace, name)
	// The purge path deletes the claim and releases the gate on a later round:
	// the case drives the rounds the controller would enqueue itself (C-06).
	harness.Eventually(t, 15*time.Second, "volume "+name+" to be released", func() bool {
		harness.ReconcileVolume(t, controller, namespace, name)
		harness.ReleaseClaimDeletion(t, cluster, namespace, name)
		_, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Get(context.Background(), name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
}

// importReleased creates a volume named after a released claim and requires the
// reconciler to take the claim back and clear its marker (C-39).
func importReleased(t *testing.T, cluster *harness.Cluster, controller *volume.Controller, name string) {
	t.Helper()
	spec := map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}
	cluster.CreateIn(t, namespace, harness.Volume(namespace, name, spec))
	harness.ReconcileVolume(t, controller, namespace, name)

	claim := cluster.Claim(t, namespace, name)
	if claim.GetLabels()["released"] != "" {
		t.Fatalf("the import kept the release marker: %v", claim.GetLabels())
	}
	stored := volumeOf(t, cluster, name)
	if stored.Status.Phase != volume.PhaseBound {
		t.Fatalf("imported volume is %q, want %q", stored.Status.Phase, volume.PhaseBound)
	}
	if condition, found := findCondition(stored, volume.ConditionImported); !found || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the import is not recorded: %+v", condition)
	}
}

// volumeOf decodes one stored volume.
func volumeOf(t *testing.T, cluster *harness.Cluster, name string) *v1beta1.Volume {
	t.Helper()
	object, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get volume %q: %v", name, err)
	}
	decoded := &v1beta1.Volume{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, decoded); err != nil {
		t.Fatalf("decode volume %q: %v", name, err)
	}
	return decoded
}

func mustVolume(t *testing.T, cluster *harness.Cluster, name string) *unstructured.Unstructured {
	t.Helper()
	object, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get volume: %v", err)
	}
	return object
}

// findCondition reads one condition of a decoded volume.
func findCondition(volume *v1beta1.Volume, conditionType string) (metav1.Condition, bool) {
	for _, condition := range volume.Status.Conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
