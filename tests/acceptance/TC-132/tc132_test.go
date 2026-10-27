// Package tc132 verifies the shape of the delete confirmation AC-18 fixes: an
// application delete carries no token, and deleting the application touches
// nothing but the application — its volume and its claim stay exactly as they
// were (AC-18, TC-132).
//
// The confirmation itself is a panel step, so a caller that stops there sends no
// request at all; what this case judges is the API's half of the contract: the
// delete is one tokenless request, and it removes one object. The volume half of
// the same criterion — releasing the data is a second, independent confirmation,
// and the volume object disappears without it — is carried by TC-64.
package tc132

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC132DeleteConfirmation deletes one application and requires the objects
// the delete alone must not touch to survive field for field.
func TestTC132DeleteConfirmation(t *testing.T) {
	cluster := harness.Start(t)
	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")

	namespace := harness.Namespace
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest("Application", namespace, "deleting", nil, nil)})
	cluster.Create(t, harness.Volume(namespace, "kept", map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": "standard",
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cluster.Kube.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, claim(namespace), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the claim: %v", err)
	}

	volume := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace)
	claimOf := cluster.Direct(t, "", "v1", "PersistentVolumeClaim", namespace)
	beforeVolume := read(t, volume, "kept")
	beforeClaim := read(t, claimOf, "kept")

	// The confirmation the panel shows has no counterpart in the request: the
	// delete carries the concurrency precondition and nothing else, and it is
	// accepted (C-39).
	stored := cluster.GetIn(t, namespace, "v1beta1", "Application", "deleting")
	status, body := system.Call(t, owner, http.MethodDelete,
		"/api/v1/resources/Application/deleting?namespace="+namespace, nil,
		map[string]string{"If-Match": stored.GetResourceVersion()})
	if status != http.StatusAccepted {
		t.Fatalf("delete the application = %d %v, want 202", status, body)
	}
	if accepted, _ := body["accepted"].(bool); !accepted {
		t.Fatalf("delete the application answered %v, want accepted", body)
	}
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Application", namespace).
		Get(ctx, "deleting", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the application survives its own delete (err = %v)", err)
	}

	// Preserving the volume and the claim is the delete's promise (C-39), so
	// both objects are still there and unchanged in every field.
	if got := read(t, volume, "kept"); !reflect.DeepEqual(got.Object, beforeVolume.Object) {
		t.Fatalf("the delete changed the volume: %v, was %v", got.Object, beforeVolume.Object)
	}
	if got := read(t, claimOf, "kept"); !reflect.DeepEqual(got.Object, beforeClaim.Object) {
		t.Fatalf("the delete changed the claim: %v, was %v", got.Object, beforeClaim.Object)
	}
}

// claim builds the claim the volume's data lives in.
func claim(namespace string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "kept", Namespace: namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("1Gi"),
			}},
		},
	}
}

// read returns one stored object.
func read(t *testing.T, resource dynamic.ResourceInterface, name string) *unstructured.Unstructured {
	t.Helper()
	object, err := resource.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return object
}
