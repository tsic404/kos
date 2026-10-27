package harness

import (
	"context"
	"fmt"
	"testing"

	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// CapabilityEntry builds one registered capability whose case-set digest is
// the distribution's (C-09, C-10).
func CapabilityEntry(t *testing.T, distribution *conformance.Distribution, capability, version, caseSet string, guarantees ...v1beta1.CapabilityGuarantee) extension.Capability {
	t.Helper()
	digest, found := distribution.Digest(caseSet)
	if !found {
		t.Fatalf("the distribution ships no case set %q", caseSet)
	}
	return extension.Capability{
		Name:          capability,
		Version:       version,
		Guarantees:    guarantees,
		CaseSet:       caseSet,
		CaseSetDigest: digest,
	}
}

// ProviderDeclaration builds the declaration of a component that registers a
// provider (C-47).
func ProviderDeclaration(identifier, version string, contribution extension.CapabilityContributions) extension.Declaration {
	return extension.Declaration{
		Identifier: identifier,
		Version:    version,
		Contributions: extension.Contributions{
			Capabilities: contribution,
		},
	}
}

// CreateClaim creates one bound claim for a volume, through the row C-04 gives
// the volume reconciler (C-38).
func (c *Cluster) CreateClaim(t *testing.T, namespace, name, storageClass string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	claim := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels": map[string]any{
				markers.ManagedBy: write.ManagedByValue,
				markers.OwnerUID:  "volume-uid",
			},
		},
		"spec": map[string]any{
			"accessModes":      []any{"ReadWriteOnce"},
			"storageClassName": storageClass,
			"resources":        map[string]any{"requests": map[string]any{"storage": "1Gi"}},
		},
	}}
	if _, err := c.Writer().CreateClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, claim); err != nil {
		t.Fatalf("create claim %s/%s: %v", namespace, name, err)
	}
}

// RemoveClaim deletes one claim as the volume reconciler, the only subject
// C-04 lets delete a bound claim. The acceptance cluster runs no controller to
// clear the protection finalizer the API server adds, so the case clears it
// first; otherwise the claim would stay in Terminating.
func (c *Cluster) RemoveClaim(t *testing.T, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	claims := c.Kube.CoreV1().PersistentVolumeClaims(namespace)
	claim, err := claims.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read claim %s/%s: %v", namespace, name, err)
	}
	if len(claim.Finalizers) > 0 {
		claim.Finalizers = nil
		if _, err := claims.Update(ctx, claim, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("clear finalizers of claim %s/%s: %v", namespace, name, err)
		}
	}
	ref := write.Ref{Group: "", Version: "v1", Kind: "PersistentVolumeClaim", Namespace: namespace, Name: name}
	if err := c.Writer().DeleteClaim(ctx, write.SubjectVolumeReconciler, write.Session{}, write.ClaimBound, ref); err != nil {
		t.Fatalf("delete claim %s/%s: %v", namespace, name, err)
	}
}

// DeleteObject deletes one kernel object as the global Owner (C-03).
func (c *Cluster) DeleteObject(t *testing.T, version, kind, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	session := write.Session{Username: "owner", Role: write.RoleOwner}
	ref := write.Ref{Group: api.Group, Version: version, Kind: kind, Namespace: namespace, Name: name}
	if err := c.Writer().Delete(ctx, write.SubjectUser, session, ref); err != nil {
		t.Fatalf("delete %s %s/%s: %v", kind, namespace, name, err)
	}
}

// ValidateProvider runs one validation round over one provider (C-47).
func (c *Cluster) ValidateProvider(t *testing.T, name string, distribution *conformance.Distribution, targets conformance.Targets) {
	t.Helper()
	c.ValidateProviderWith(t, provider.NewReconciler(c.Writer(), provider.Options{Distribution: distribution, Targets: targets}), name)
}

// ValidateProviderWith runs one validation round through an existing
// reconciler, so a case can observe what that reconciler carries between
// rounds (C-06, C-10).
func (c *Cluster) ValidateProviderWith(t *testing.T, reconciler *provider.Reconciler, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := reconciler.Reconcile(ctx, reconcile.Request{Kind: "Provider", Name: name}); err != nil {
		t.Fatalf("validate provider %s: %v", name, err)
	}
}

// ProviderSpecOf decodes the registration the component path wrote.
func (c *Cluster) ProviderSpecOf(t *testing.T, name string) v1beta1.ProviderSpec {
	t.Helper()
	raw, found, err := unstructured.NestedMap(c.Get(t, "v1beta1", "Provider", name).Object, "spec")
	if err != nil {
		t.Fatalf("read provider %s spec: %v", name, err)
	}
	if !found {
		t.Fatalf("provider %s has no spec", name)
	}
	var spec v1beta1.ProviderSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		t.Fatalf("decode provider %s spec: %v", name, err)
	}
	return spec
}

// ProviderStatusOf decodes the status the validator wrote.
func (c *Cluster) ProviderStatusOf(t *testing.T, name string) v1beta1.ProviderStatus {
	t.Helper()
	raw, found, err := unstructured.NestedMap(c.Get(t, "v1beta1", "Provider", name).Object, "status")
	if err != nil {
		t.Fatalf("read provider %s status: %v", name, err)
	}
	if !found {
		t.Fatalf("provider %s has no status", name)
	}
	var status v1beta1.ProviderStatus
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
		t.Fatalf("decode provider %s status: %v", name, err)
	}
	return status
}

// ProviderExists reports whether one provider object is still stored.
func (c *Cluster) ProviderExists(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_, err := c.GetObject(ctx, "v1beta1", "Provider", "", name)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read provider %s: %v", name, err)
	}
	return true
}

// DeleteProvider requests deletion of one provider as the global Owner, which
// is the only subject C-03 lets delete a kernel object.
func (c *Cluster) DeleteProvider(t *testing.T, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	session := write.Session{Username: "owner", Role: write.RoleOwner}
	ref := write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Provider", Name: name}
	if err := c.Writer().Delete(ctx, write.SubjectUser, session, ref); err != nil {
		t.Fatalf("delete provider %s: %v", name, err)
	}
}

// CreateStorageClass creates the storage class a provider declaration names,
// so the declaration and the cluster agree on the supplier (C-47).
func (c *Cluster) CreateStorageClass(t *testing.T, name, provisioner string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_, err := c.Kube.StorageV1().StorageClasses().Create(ctx, &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: name},
		Provisioner: provisioner,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create storage class %s: %v", name, err)
	}
}

// CaseTarget builds a conformance implementation that answers every case the
// listed failures do not name (C-10).
func CaseTarget(failures ...string) conformance.Target {
	failed := make(map[string]bool, len(failures))
	for _, caseID := range failures {
		failed[caseID] = true
	}
	return conformance.TargetFunc(func(_ context.Context, capability, version, caseID string) error {
		if failed[caseID] {
			return fmt.Errorf("case %s of %s@%s failed", caseID, capability, version)
		}
		return nil
	})
}

// ProviderTargets wires provider implementations by provider name. The names
// are data: the validator resolves an implementation, it never branches on one
// (F11).
func ProviderTargets(implementations map[string]conformance.Target) conformance.Targets {
	return conformance.MapTargets(implementations)
}

// CapabilityResultOf returns the stored verification result of one capability.
func CapabilityResultOf(status v1beta1.ProviderStatus, capability, version string) (v1beta1.ProviderCapabilityResult, bool) {
	for _, result := range status.Capabilities {
		if result.Name == capability && result.Version == version {
			return result, true
		}
	}
	return v1beta1.ProviderCapabilityResult{}, false
}

// ConditionOf returns one stored condition.
func ConditionOf(status v1beta1.ProviderStatus, conditionType string) (metav1.Condition, bool) {
	for _, condition := range status.Conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// RequireCondition fails the case when one provider condition does not carry
// the expected status and reason (C-47).
func RequireProviderCondition(t *testing.T, status v1beta1.ProviderStatus, conditionType string, want metav1.ConditionStatus, reason string) {
	t.Helper()
	condition, found := ConditionOf(status, conditionType)
	if !found {
		t.Fatalf("condition %s is missing: %v", conditionType, status.Conditions)
	}
	if condition.Status != want {
		t.Fatalf("%s = %s (%s), want %s", conditionType, condition.Status, condition.Message, want)
	}
	if condition.Reason != reason {
		t.Fatalf("%s reason = %s, want %s (%s)", conditionType, condition.Reason, reason, condition.Message)
	}
}

// ReferenceNames renders a reference list as "Kind name" entries.
func ReferenceNames(refs []v1beta1.ObjectRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := ref.Name
		if ref.Namespace != "" {
			name = ref.Namespace + "/" + ref.Name
		}
		names = append(names, ref.Kind+" "+name)
	}
	return names
}
