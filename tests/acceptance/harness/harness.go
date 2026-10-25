// Package harness boots a throwaway API server with the KOS object model
// installed and shapes the objects the acceptance cases run on.
package harness

import (
	"bytes"
	"context"
	"encoding/json"
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

// List reads every object of a kind in the acceptance namespace.
func (c *Cluster) List(t *testing.T, version, kind string) []*unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	list, err := c.resource(t, version, kind).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list %s in %s: %v", kind, version, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects
}

// Resource resolves the client of one kind for code that runs off the test
// goroutine. The namespace is ignored for cluster-scoped kinds.
func (c *Cluster) Resource(version, kind, namespace string) (dynamic.ResourceInterface, error) {
	plural, ok := c.Plurals[kind]
	if !ok {
		return nil, fmt.Errorf("no plural discovered for kind %q", kind)
	}
	resource := c.Dynamic.Resource(schema.GroupVersionResource{Group: api.Group, Version: version, Resource: plural})
	if scope, _ := api.ScopeOf(kind); scope == api.ScopeNamespaced {
		return resource.Namespace(namespace), nil
	}
	return resource.Namespace(""), nil
}

// GetObject reads one kernel object for code that runs off the test goroutine.
func (c *Cluster) GetObject(ctx context.Context, version, kind, namespace, name string) (*unstructured.Unstructured, error) {
	resource, err := c.Resource(version, kind, namespace)
	if err != nil {
		return nil, err
	}
	return resource.Get(ctx, name, metav1.GetOptions{})
}

// ApplyStatus writes a status only when the stored value differs, so an
// unchanged observed state produces no write. It reports whether it wrote.
func (c *Cluster) ApplyStatus(ctx context.Context, version, kind, namespace, name string, status map[string]any) (bool, error) {
	resource, err := c.Resource(version, kind, namespace)
	if err != nil {
		return false, err
	}
	object, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	current, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil {
		return false, err
	}
	if found {
		equal, err := sameJSON(current, status)
		if err != nil {
			return false, err
		}
		if equal {
			return false, nil
		}
	}
	object.Object["status"] = status
	_, err = resource.UpdateStatus(ctx, object, metav1.UpdateOptions{})
	return err == nil, err
}

// sameJSON compares two decoded JSON values by their encoding: the dynamic
// client decodes integers as int64 and encoding/json as float64, so a direct
// deep comparison would always report a difference.
func sameJSON(left, right any) (bool, error) {
	leftEncoded, err := json.Marshal(left)
	if err != nil {
		return false, err
	}
	rightEncoded, err := json.Marshal(right)
	if err != nil {
		return false, err
	}
	return bytes.Equal(leftEncoded, rightEncoded), nil
}

// Application returns a namespaced Application with an empty status, so the
// case's reconciler is the only status writer.
func Application(name string, annotations map[string]string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      name,
			"namespace": Namespace,
			"labels":    map[string]any{markers.ManagedBy: "acceptance"},
		},
		"spec": map[string]any{},
	}}
	if len(annotations) > 0 {
		object.SetAnnotations(annotations)
	}
	return object
}

// ConditionsOf reads status.conditions of a stored object; a missing list is
// nil, not an error.
func ConditionsOf(object *unstructured.Unstructured) ([]metav1.Condition, error) {
	raw, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil || !found {
		return nil, err
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var conditions []metav1.Condition
	if err := json.Unmarshal(encoded, &conditions); err != nil {
		return nil, err
	}
	return conditions, nil
}

// Conditions is ConditionsOf for the test goroutine.
func Conditions(t *testing.T, object *unstructured.Unstructured) []metav1.Condition {
	t.Helper()
	conditions, err := ConditionsOf(object)
	if err != nil {
		t.Fatalf("read conditions of %s %s: %v", object.GetKind(), object.GetName(), err)
	}
	return conditions
}

// StatusValue renders a status struct as the map the dynamic client writes.
func StatusValue(status v1beta1.ObjectStatus) (map[string]any, error) {
	encoded, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	return value, nil
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
