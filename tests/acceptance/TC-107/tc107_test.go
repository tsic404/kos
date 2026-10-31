// Package tc107 checks C-19's declarative reference: a volume an extension
// object names cannot be deleted while the reference exists, the referrer is
// listed, and after the object is gone the volume follows the C-39 release path
// (AC-53, TC-107).
package tc107

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	vmGroup   = "kubevirt.example.test"
	namespace = "vms"
	name      = "data"
	class     = "standard"
	driver    = "standard.example"
)

// requestTimeout bounds every direct cluster call of the case.
const requestTimeout = 30 * time.Second

// declaration serves one product class whose objects may reference a volume.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "thirdparty.kubevirt",
		Version:    "1.0.0",
		APIGroup:   vmGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: vmGroup, Kind: "VirtualMachine", Resource: "virtualmachines",
				Versions: []string{"v1"}, Namespaced: true,
				References: []string{"spec.volumeRef"},
			}},
			Controllers: []extension.Controller{{
				ID:      "virtual-machine",
				Manages: []extension.GroupKind{{Group: vmGroup, Kind: "VirtualMachine"}},
			}},
		},
	}
}

// TestTC107ReferenceBlocksDeletion deletes a bound volume while a virtual
// machine references it, then removes the machine and deletes the volume again.
func TestTC107ReferenceBlocksDeletion(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, vmGroup, "v1", "VirtualMachine", "virtualmachines", true)
	cluster.EnsureNamespace(t, namespace)
	cluster.StorageClass(t, class, driver, true, "Retain", nil)
	artifacts := extension.MemoryArtifacts{}
	installed := cluster.InstallPackage(t, "kubevirt", declaration(), artifacts)

	controller := cluster.VolumeController(t, volume.Options{
		References: extension.ReferenceGate{Dynamic: cluster.Dynamic, Registry: installed.Registry()},
	})
	cluster.CreateIn(t, namespace, harness.Volume(namespace, name, map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}))
	harness.ReconcileVolume(t, controller, namespace, name)
	cluster.PersistentVolume(t, "pv-"+name, class, driver, "Retain")
	cluster.BindClaim(t, namespace, name, "pv-"+name)
	harness.ReconcileVolume(t, controller, namespace, name)
	if stored := cluster.GetIn(t, namespace, "v1beta1", "Volume", name); harness.String(stored.Object, "status", "phase") != volume.PhaseBound {
		t.Fatalf("volume phase = %q, want %q before the delete", harness.String(stored.Object, "status", "phase"), volume.PhaseBound)
	}

	// The product object names the volume through its declared reference path.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	virtualMachine := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": vmGroup + "/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name": "vm-one", "namespace": namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{"volumeRef": map[string]any{
			"apiVersion": api.Group + "/v1beta1", "kind": "Volume", "name": name,
		}},
	}}
	if _, err := cluster.ExtensionResource(vmGroup, "v1", "virtualmachines", namespace).Create(ctx, virtualMachine, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the virtual machine: %v", err)
	}

	// The first delete is blocked and names the referring object.
	cluster.DeleteVolume(t, namespace, name)
	harness.ReconcileVolume(t, controller, namespace, name)
	stored := cluster.GetIn(t, namespace, "v1beta1", "Volume", name)
	condition := findCondition(t, harness.Conditions(t, stored), volume.ConditionDeletionBlocked)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("DeletionBlocked = %+v, want True: %v", condition, harness.Conditions(t, stored))
	}
	if !strings.Contains(condition.Message, "VirtualMachine") || !strings.Contains(condition.Message, "vm-one") {
		t.Fatalf("DeletionBlocked message %q does not list the referrer", condition.Message)
	}

	// Removing the reference lets the volume take the C-39 release path.
	if err := cluster.ExtensionResource(vmGroup, "v1", "virtualmachines", namespace).Delete(ctx, "vm-one", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the virtual machine: %v", err)
	}
	harness.Eventually(t, 15*time.Second, "the volume to be released", func() bool {
		harness.ReconcileVolume(t, controller, namespace, name)
		_, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace).Get(context.Background(), name, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	claim := cluster.Claim(t, namespace, name)
	if claim.GetLabels()["released"] == "" {
		t.Fatalf("the claim was kept without a release marker: %v", claim.GetLabels())
	}
}

// findCondition returns one condition or nil.
func findCondition(t *testing.T, conditions []metav1.Condition, conditionType string) *metav1.Condition {
	t.Helper()
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}
