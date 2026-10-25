// Package harness boots a throwaway API server with the KOS object model
// installed and shapes the objects the acceptance cases run on.
package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
)

// Namespace is where namespaced kernel objects of the acceptance cases live.
const Namespace = "default"

// requestTimeout bounds every API call so a wedged server fails the case.
const requestTimeout = 30 * time.Second

// Cluster is a running API server with the generated CRDs installed.
type Cluster struct {
	// Dynamic reads and writes the kernel objects in either served version.
	Dynamic dynamic.Interface
	// Kube inspects cluster-wide admission configuration.
	Kube kubernetes.Interface
	// APIExtensions reads the installed CRDs.
	APIExtensions apiextensionsclientset.Interface
	// Plurals maps a kind to the resource name the API server serves.
	Plurals map[string]string
}

// Start boots etcd and kube-apiserver, installs the generated CRDs and returns
// clients pointed at the server. KUBEBUILDER_ASSETS must name the envtest
// binaries; `make test` downloads and exports them.
func Start(t *testing.T) *Cluster {
	t.Helper()
	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(RepoRoot(t), "config", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatalf("start envtest (KUBEBUILDER_ASSETS=%q): %v", os.Getenv("KUBEBUILDER_ASSETS"), err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("build dynamic client: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("build kubernetes client: %v", err)
	}
	extensionsClient, err := apiextensionsclientset.NewForConfig(config)
	if err != nil {
		t.Fatalf("build apiextensions client: %v", err)
	}

	cluster := &Cluster{
		Dynamic:       dynamicClient,
		Kube:          kubeClient,
		APIExtensions: extensionsClient,
	}
	cluster.Plurals = cluster.discoverPlurals(t)
	return cluster
}

// RepoRoot returns the repository root the tests run from.
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the harness source file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(source))))
}

// KernelObjects returns one object per kernel kind, shaped identically for
// every acceptance case: the same labels, annotations, spec and status.
func KernelObjects(t *testing.T) []*unstructured.Unstructured {
	t.Helper()
	objects := make([]*unstructured.Unstructured, 0, len(api.Kinds()))
	for _, kind := range api.Kinds() {
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
			"kind":       kind,
			"metadata": map[string]any{
				"name":        strings.ToLower(kind),
				"labels":      markerLabels(kind),
				"annotations": markerAnnotations(kind),
			},
			"spec": map[string]any{},
		}}
		if hasStatus, _ := api.HasStatus(kind); hasStatus {
			object.Object["status"] = ObjectStatus()
		}
		objects = append(objects, object)
	}
	return objects
}

// ObjectStatus is the status value the acceptance objects carry (C-07, C-08).
func ObjectStatus() map[string]any {
	return map[string]any{
		"phase":  "Running",
		"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
		"conditions": []any{map[string]any{
			"type":               api.ConditionReconciled,
			"status":             "True",
			"reason":             "Reconciled",
			"message":            "acceptance",
			"lastTransitionTime": "2026-10-25T00:00:00Z",
		}},
	}
}

// markerLabels returns the attribution labels for one kind: an adopted
// definition, a discovered device and managed objects otherwise.
func markerLabels(kind string) map[string]any {
	switch kind {
	case "ApplicationDefinition":
		return map[string]any{
			markers.AdoptedBy:      "acceptance",
			markers.AdoptionReason: "pre-existing-catalogue",
		}
	case "NodeDevice":
		return map[string]any{markers.ManagedBy: "discovery"}
	default:
		return map[string]any{
			markers.ManagedBy: "kos-shell",
			markers.OwnerUID:  "00000000-0000-0000-0000-000000000001",
		}
	}
}

// markerAnnotations returns the annotation carrier: adopted-at for the adopted
// definition plus an unrelated annotation that must not change behaviour.
func markerAnnotations(kind string) map[string]any {
	annotations := map[string]any{"example.test/note": "acceptance"}
	if kind == "ApplicationDefinition" {
		annotations[markers.AdoptedAt] = "2026-10-25T00:00:00Z"
	}
	return annotations
}

// GVR resolves the resource of a kind in one served version.
func (c *Cluster) GVR(t *testing.T, version, kind string) schema.GroupVersionResource {
	t.Helper()
	plural, ok := c.Plurals[kind]
	if !ok {
		t.Fatalf("no plural discovered for kind %q", kind)
	}
	return schema.GroupVersionResource{Group: api.Group, Version: version, Resource: plural}
}

// Create writes an object and, for kinds with a status writer, its status.
// The returned object is what the server stored.
func (c *Cluster) Create(t *testing.T, object *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	kind := object.GetKind()
	resource := c.resource(t, v1beta1.GroupVersion.Version, kind)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	created, err := resource.Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create %s: %v", kind, err)
	}
	status, hasStatus := object.Object["status"]
	if !hasStatus {
		return created
	}
	created.Object["status"] = status
	updated, err := resource.UpdateStatus(ctx, created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("write status of %s: %v", kind, err)
	}
	return updated
}

// Get reads an existing object in one served version.
func (c *Cluster) Get(t *testing.T, version, kind, name string) *unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	object, err := c.resource(t, version, kind).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s %s in %s: %v", kind, name, version, err)
	}
	return object
}

// resource returns the client for a kind, namespace-scoped when the kind is.
func (c *Cluster) resource(t *testing.T, version, kind string) dynamic.ResourceInterface {
	t.Helper()
	resource := c.Dynamic.Resource(c.GVR(t, version, kind))
	if scope, _ := api.ScopeOf(kind); scope == api.ScopeNamespaced {
		return resource.Namespace(Namespace)
	}
	return resource.Namespace("")
}

// discoverPlurals reads the resource names the API server serves.
func (c *Cluster) discoverPlurals(t *testing.T) map[string]string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	list, err := c.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list CRDs: %v", err)
	}
	plurals := make(map[string]string, len(list.Items))
	for _, crd := range list.Items {
		plurals[crd.Spec.Names.Kind] = crd.Spec.Names.Plural
	}
	return plurals
}

// Flatten turns an object into path to typed-value entries so two reads can be
// compared field by field. The version identity and the server's write history
// are left out: they name the version rather than the object.
func Flatten(object map[string]any) map[string]string {
	fields := make(map[string]string)
	flattenInto("", object, fields)
	return fields
}

// isVersionIdentity reports paths that differ between two reads of one object
// by construction and carry no object data.
func isVersionIdentity(path string) bool {
	return path == "apiVersion" || path == "kind" || strings.HasPrefix(path, "metadata.managedFields")
}

// flattenInto records every leaf and container of a JSON tree.
func flattenInto(path string, node any, fields map[string]string) {
	if isVersionIdentity(path) {
		return
	}
	switch typed := node.(type) {
	case map[string]any:
		if path != "" {
			fields[path] = "object"
		}
		for key, value := range typed {
			flattenInto(join(path, key), value, fields)
		}
	case []any:
		fields[path] = fmt.Sprintf("array[%d]", len(typed))
		for index, value := range typed {
			flattenInto(fmt.Sprintf("%s[%d]", path, index), value, fields)
		}
	default:
		fields[path] = fmt.Sprintf("%T=%v", typed, typed)
	}
}

// join appends a JSON key to a path.
func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
