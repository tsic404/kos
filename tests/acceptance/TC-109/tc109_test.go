// Package tc109 checks C-20's write scope: an extension controller can write
// its own declared kinds, but its writes to a kernel object and to a claim are
// refused and leave the objects untouched (AC-53, TC-109).
package tc109

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	vmGroup      = "kubevirt.example.test"
	appNamespace = "apps"
)

// requestTimeout bounds every direct cluster call of the case.
const requestTimeout = 30 * time.Second

// declaration grants one extension controller a namespace and the Deployment
// kind, but no kernel or claim writes.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "thirdparty.kubevirt",
		Version:    "1.0.0",
		APIGroup:   vmGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: vmGroup, Kind: "VirtualMachine", Resource: "virtualmachines",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []extension.Controller{{
				ID:      "virtual-machine",
				Manages: []extension.GroupKind{{Group: vmGroup, Kind: "VirtualMachine"}},
				Scope: extension.WriteScope{
					Groups:     []string{vmGroup},
					Kinds:      []extension.GroupKind{{Group: "apps", Kind: "Deployment"}},
					Namespaces: []string{appNamespace},
				},
			}},
		},
	}
}

// TestTC109OutOfScopeWritesAreRefused installs the package, then replays one
// refused kernel write, one refused claim write and one admitted downstream
// write through the same scope.
func TestTC109OutOfScopeWritesAreRefused(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, vmGroup, "v1", "VirtualMachine", "virtualmachines", true)
	cluster.EnsureNamespace(t, appNamespace)
	installation := cluster.InstallPackage(t, "kubevirt", declaration(), extension.MemoryArtifacts{})
	result, found := installation.Component("kubevirt")
	if !found || len(result.Contributions.Controllers) != 1 {
		t.Fatalf("the installed controller is missing: %+v", result)
	}
	writer := cluster.Writer()
	writer.Scope = result.Contributions.Controllers[0].Scope.Admit()

	// A kernel object is not the extension's to write (C-03).
	victim := cluster.Create(t, harness.Application("victim", nil))
	before, _, err := unstructured.NestedMap(victim.Object, "spec")
	if err != nil {
		t.Fatalf("read the victim spec: %v", err)
	}
	application := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "victim", "namespace": harness.Namespace},
		"spec":       map[string]any{"replicas": 5},
	}}
	_, err = writer.Apply(context.Background(), write.SubjectExtensionReconciler, write.Session{}, write.PartSpec, application)
	denied := &write.Denied{}
	if !errors.As(err, &denied) {
		t.Fatalf("kernel write error = %v, want a denial", err)
	}
	if denied.Subject != write.SubjectExtensionReconciler || denied.Object.Kind != "Application" ||
		!strings.Contains(denied.Error(), "Application") {
		t.Fatalf("denial = %+v, want the extension subject and the Application", denied)
	}
	stored := cluster.Get(t, "v1beta1", "Application", "victim")
	after, _, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil {
		t.Fatalf("re-read the victim spec: %v", err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the refused write changed the kernel object: before %v after %v", before, after)
	}

	// A claim belongs to the C-04 rows, not to an extension controller.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	claim := &unstructured.Unstructured{Object: harness.Manifest("PersistentVolumeClaim", appNamespace, "claim-one",
		map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)}
	claim.Object["spec"] = map[string]any{"accessModes": []any{"ReadWriteOnce"}}
	if _, err := writer.Create(ctx, write.SubjectExtensionReconciler, write.Session{}, claim); !errors.As(err, &denied) {
		t.Fatalf("claim write error = %v, want a denial", err)
	}
	if _, err := cluster.Direct(t, "", "v1", "PersistentVolumeClaim", appNamespace).Get(ctx, "claim-one", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the refused claim exists (err %v)", err)
	}

	// The declared downstream kind in the declared namespace is still written.
	deployment := &unstructured.Unstructured{Object: harness.Manifest("Deployment", appNamespace, "vm-workload",
		map[string]string{markers.ManagedBy: write.ManagedByValue, markers.OwnerUID: "vm-uid"}, nil)}
	deployment.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	deployment.Object["spec"] = map[string]any{
		"replicas": 1,
		"selector": map[string]any{"matchLabels": map[string]any{"app": "vm-workload"}},
		"template": map[string]any{
			"metadata": map[string]any{"labels": map[string]any{"app": "vm-workload"}},
			"spec": map[string]any{"containers": []any{
				map[string]any{"name": "vm", "image": "example.test/vm:1"},
			}},
		},
	}
	if _, err := writer.Create(ctx, write.SubjectExtensionReconciler, write.Session{}, deployment); err != nil {
		t.Fatalf("declared downstream write: %v", err)
	}
	storedDeployment, err := cluster.Direct(t, "apps", "v1", "Deployment", appNamespace).Get(ctx, "vm-workload", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the declared downstream object: %v", err)
	}
	if replicas, _, _ := unstructured.NestedInt64(storedDeployment.Object, "spec", "replicas"); replicas != 1 {
		t.Fatalf("stored replicas = %d, want 1", replicas)
	}
}
