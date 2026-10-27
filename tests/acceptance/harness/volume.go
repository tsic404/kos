package harness

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/volume"
)

// Volume returns a namespaced volume with the given spec and no status, so the
// volume reconciler is its only status writer.
func Volume(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	manifest := Manifest("Volume", namespace, name, map[string]string{"managed-by": "acceptance"}, nil)
	manifest["spec"] = spec
	return &unstructured.Unstructured{Object: manifest}
}

// VolumeController builds the real volume controller over the cluster.
func (c *Cluster) VolumeController(t *testing.T, options volume.Options) *volume.Controller {
	t.Helper()
	return volume.NewController(volume.NewWriterStore(c.Writer()), options)
}

// ReconcileVolume runs one volume round and fails the case on error.
func ReconcileVolume(t *testing.T, r *volume.Controller, namespace, name string) reconcile.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: namespace, Name: name})
	if err != nil {
		t.Fatalf("reconcile volume %s/%s: %v", namespace, name, err)
	}
	return result
}

// ReconcileApplicationRound runs one application round and fails the case on
// error.
func ReconcileApplicationRound(t *testing.T, r interface {
	Reconcile(context.Context, reconcile.Request) (reconcile.Result, error)
}, namespace, name string) reconcile.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), reconcile.Request{Kind: "Application", Namespace: namespace, Name: name})
	if err != nil {
		t.Fatalf("reconcile application %s/%s: %v", namespace, name, err)
	}
	return result
}

// StorageClass writes a storage class. envtest runs no provisioner, so a case
// supplies the two backends C-38 resolves a volume against.
func (c *Cluster) StorageClass(t *testing.T, name, provisioner string, allowExpansion bool, reclaimPolicy string, parameters map[string]string) {
	t.Helper()
	object := map[string]any{
		"apiVersion":    "storage.k8s.io/v1",
		"kind":          "StorageClass",
		"metadata":      map[string]any{"name": name},
		"provisioner":   provisioner,
		"reclaimPolicy": reclaimPolicy,
	}
	if allowExpansion {
		object["allowVolumeExpansion"] = true
	}
	if len(parameters) > 0 {
		values := make(map[string]any, len(parameters))
		for key, value := range parameters {
			values[key] = value
		}
		object["parameters"] = values
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.ExtensionResource("storage.k8s.io", "v1", "storageclasses", "")
	if _, err := resource.Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create storage class %s: %v", name, err)
	}
}

// PersistentVolume writes a persistent volume the case binds a claim to. The
// CSI driver is what the volume status resolves its provider from (C-38).
func (c *Cluster) PersistentVolume(t *testing.T, name, storageClass, driver, reclaimPolicy string) {
	t.Helper()
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"capacity":                      map[string]any{"storage": "1Gi"},
			"accessModes":                   []any{"ReadWriteOnce"},
			"persistentVolumeReclaimPolicy": reclaimPolicy,
			"storageClassName":              storageClass,
			"csi": map[string]any{
				"driver":       driver,
				"volumeHandle": name,
			},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := c.Direct(t, "", "v1", "PersistentVolume", "").Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create persistent volume %s: %v", name, err)
	}
}

// BindClaim plays the binding envtest does not run: it names the persistent
// volume on the claim and sets the claim's phase to Bound.
func (c *Cluster) BindClaim(t *testing.T, namespace, claimName, pvName string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.Direct(t, "", "v1", "PersistentVolumeClaim", namespace)
	claim, err := resource.Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim %s/%s: %v", namespace, claimName, err)
	}
	if err := unstructured.SetNestedField(claim.Object, pvName, "spec", "volumeName"); err != nil {
		t.Fatalf("set volumeName of claim %s: %v", claimName, err)
	}
	if _, err := resource.Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update claim %s: %v", claimName, err)
	}
	claim, err = resource.Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-get claim %s: %v", claimName, err)
	}
	claim.Object["status"] = map[string]any{"phase": "Bound"}
	if _, err := resource.UpdateStatus(ctx, claim, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("bind claim %s: %v", claimName, err)
	}
}

// Claim reads one claim and fails the case when it is missing.
func (c *Cluster) Claim(t *testing.T, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	claim, err := c.Direct(t, "", "v1", "PersistentVolumeClaim", namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claim %s/%s: %v", namespace, name, err)
	}
	return claim
}

// DeleteVolume deletes a volume through the native path, as kubectl does: the
// deletion reads the same object fields the panel path writes (C-39).
func (c *Cluster) DeleteVolume(t *testing.T, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := c.Direct(t, api.Group, "v1beta1", "Volume", namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete volume %s/%s: %v", namespace, name, err)
	}
}

// SetPurgeToken writes the purge field of a volume before a delete (C-39).
func (c *Cluster) SetPurgeToken(t *testing.T, namespace, name, token string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.Direct(t, api.Group, "v1beta1", "Volume", namespace)
	stored, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get volume %s/%s: %v", namespace, name, err)
	}
	if err := unstructured.SetNestedField(stored.Object, token, "spec", "purgeToken"); err != nil {
		t.Fatalf("set purge token of %s: %v", name, err)
	}
	if _, err := resource.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update volume %s: %v", name, err)
	}
}

// ReleaseClaimDeletion plays the controller manager envtest does not run: a
// claim the reconciler deleted keeps the pvc-protection finalizer until the
// in-use controller removes it. The case removes it once the deletion is under
// way, so the purge path can be observed to completion.
func ReleaseClaimDeletion(t *testing.T, cluster *Cluster, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := cluster.Direct(t, "", "v1", "PersistentVolumeClaim", namespace)
	claim, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || err != nil {
		return
	}
	if claim.GetDeletionTimestamp() == nil {
		return
	}
	finalizers := claim.GetFinalizers()
	remaining := make([]string, 0, len(finalizers))
	for _, finalizer := range finalizers {
		if finalizer == "kubernetes.io/pvc-protection" {
			continue
		}
		remaining = append(remaining, finalizer)
	}
	if len(remaining) == len(finalizers) {
		return
	}
	claim.SetFinalizers(remaining)
	if _, err := resource.Update(ctx, claim, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("release the protection finalizer of claim %s/%s: %v", namespace, name, err)
	}
}
