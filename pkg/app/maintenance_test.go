package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// maintenanceTemplate renders the two-replica workload a maintenance stop
// drives to zero.
const maintenanceTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 2
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: web
          image: nginx
`

// nodeStates serves the maintenance state of the nodes a test cordons.
type nodeStates map[string]bool

// IsUnschedulable implements NodeSource.
func (n nodeStates) IsUnschedulable(_ context.Context, name string) (bool, error) {
	return n[name], nil
}

// maintenanceStore keeps the objects one application round reads and writes.
type maintenanceStore struct {
	objects map[string]*unstructured.Unstructured
	written []v1beta1.ApplicationStatus
}

// keyOf names one stored object.
func keyOf(ref write.Ref) string {
	return ref.Kind + "|" + ref.Namespace + "|" + ref.Name
}

// Get returns the stored object of one identity.
func (s *maintenanceStore) Get(_ context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	object, found := s.objects[keyOf(ref)]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: ref.Group, Resource: ref.Kind}, ref.Name)
	}
	return object.DeepCopy(), nil
}

// List returns an empty set: no action record and no LimitRange exists here.
func (s *maintenanceStore) List(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error) {
	return nil, nil
}

// Apply writes one rendered object and observes its replicas as ready.
func (s *maintenanceStore) Apply(_ context.Context, _ string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	stored := document.DeepCopy()
	if stored.GetKind() == "Deployment" {
		_ = unstructured.SetNestedField(stored.Object, replicasOf(stored), "status", "readyReplicas")
	}
	s.objects[keyOf(referenceOf(objectRefOf(stored)))] = stored
	return stored.DeepCopy(), nil
}

// ApplyStatus merges the submitted status into the stored application and keeps
// it for the assertions of the test.
func (s *maintenanceStore) ApplyStatus(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	key := keyOf(write.Ref{Kind: "Application", Namespace: document.GetNamespace(), Name: document.GetName()})
	stored, found := s.objects[key]
	if !found {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: api.Group, Resource: "applications"}, document.GetName())
	}
	status, written, err := unstructured.NestedMap(document.Object, "status")
	if err != nil {
		return nil, err
	}
	if !written {
		return document, nil
	}
	stored.Object["status"] = status
	decoded := &v1beta1.Application{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(document.Object, decoded); err != nil {
		return nil, err
	}
	s.written = append(s.written, decoded.Status)
	return document, nil
}

// CreateRecord accepts one record without keeping it.
func (s *maintenanceStore) CreateRecord(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return document, nil
}

// CreateEndpoint accepts one cascade endpoint without keeping it.
func (s *maintenanceStore) CreateEndpoint(_ context.Context, _ string, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return document, nil
}

// Delete removes one stored object.
func (s *maintenanceStore) Delete(_ context.Context, _ string, ref write.Ref) error {
	delete(s.objects, keyOf(ref))
	return nil
}

// SetFinalizers writes the finalizer list of the stored application.
func (s *maintenanceStore) SetFinalizers(_ context.Context, ref write.Ref, finalizers []string) (*unstructured.Unstructured, error) {
	stored, err := s.Get(context.Background(), ref)
	if err != nil {
		return nil, err
	}
	stored.SetFinalizers(finalizers)
	s.objects[keyOf(ref)] = stored
	return stored.DeepCopy(), nil
}

// RecordEvent accepts one event without keeping it.
func (s *maintenanceStore) RecordEvent(context.Context, *v1beta1.Application, string, string, string, time.Time) error {
	return nil
}

// maintenanceFixture builds an application in the given phase whose node-local
// volume is bound to node-a and whose rendered Deployment runs two replicas.
func maintenanceFixture(t *testing.T, mobility v1beta1.VolumeMobility, phase string) (*maintenanceStore, *Controller, nodeStates) {
	t.Helper()
	definition := definitionWith(t, "web", "", maintenanceTemplate, nil)
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1", Finalizers: []string{Finalizer}},
		Spec: v1beta1.ApplicationSpec{
			DefinitionRef: "web",
			Namespace:     "apps",
			Volumes:       []string{"data"},
		},
	}
	applied := targetFor(t, application, definition, map[string]any{})
	application.Status = v1beta1.ApplicationStatus{
		ObjectStatus: v1beta1.ObjectStatus{
			Phase:  phase,
			Health: v1beta1.Health{State: v1beta1.HealthStateHealthy},
		},
		Applied:          applied.applied,
		Rendered:         refsOf(applied.objects),
		ResourceDefaults: &v1beta1.ResourceRequirements{},
	}

	store := &maintenanceStore{objects: map[string]*unstructured.Unstructured{}}
	store.objects[keyOf(write.Ref{Kind: "Application", Namespace: "default", Name: "web"})] = unstructuredOf(t, application)
	store.objects[keyOf(write.Ref{Kind: "ApplicationDefinition", Name: "web"})] = unstructuredOf(t, definition)
	store.objects[keyOf(write.Ref{Kind: "Namespace", Name: "apps"})] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "apps"},
	}}
	// The node-local binding reads the node's readiness: a healthy node keeps the
	// stopped application's health at the stop rule instead of the loss timeline
	// (C-38, C-66).
	store.objects[keyOf(write.Ref{Version: "v1", Kind: "Node", Name: "node-a"})] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": "node-a"},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Ready", "status": "True"},
		}},
	}}
	store.objects[keyOf(write.Ref{Kind: "Volume", Namespace: "apps", Name: "data"})] = &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "data", "namespace": "apps"},
		"spec":       map[string]any{"mobility": string(mobility)},
		"status":     map[string]any{"node": "node-a"},
	}}
	for _, object := range applied.objects {
		live := object.DeepCopy()
		_ = unstructured.SetNestedField(live.Object, int64(2), "status", "readyReplicas")
		store.objects[keyOf(referenceOf(objectRefOf(live)))] = live
	}
	nodes := nodeStates{"node-a": true}
	return store, NewController(store, Options{Nodes: nodes}), nodes
}

// unstructuredOf encodes one typed object the way the API server stores it.
func unstructuredOf(t *testing.T, object any) *unstructured.Unstructured {
	t.Helper()
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		t.Fatalf("encode %T: %v", object, err)
	}
	return &unstructured.Unstructured{Object: encoded}
}

// roundOf runs one reconcile of the fixture application and returns the status
// it wrote.
func roundOf(t *testing.T, controller *Controller, store *maintenanceStore) v1beta1.ApplicationStatus {
	t.Helper()
	before := len(store.written)
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: "default", Name: "web",
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(store.written) == before {
		t.Fatalf("the round wrote no status")
	}
	return store.written[len(store.written)-1]
}

// replicasOf reads the replica count of one stored object, whether the
// template rendered it as a number of either encoding.
func replicasOf(object *unstructured.Unstructured) int64 {
	replicas, present, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
	if err == nil && present {
		return replicas
	}
	other, _, _ := unstructured.NestedFloat64(object.Object, "spec", "replicas")
	return int64(other)
}

// liveReplicas reads the replica count the cluster holds for the rendered
// Deployment.
func liveReplicas(t *testing.T, store *maintenanceStore) int64 {
	t.Helper()
	object, found := store.objects["Deployment|apps|web"]
	if !found {
		t.Fatal("the rendered Deployment is not in the cluster")
	}
	return replicasOf(object)
}

// applicationSpec reads the stored spec of the fixture application.
func applicationSpec(t *testing.T, store *maintenanceStore) string {
	t.Helper()
	object, found := store.objects["Application|default|web"]
	if !found {
		t.Fatal("the application is not in the cluster")
	}
	raw, err := json.Marshal(object.Object["spec"])
	if err != nil {
		t.Fatalf("encode the application spec: %v", err)
	}
	return string(raw)
}

// TestNodeMaintenanceStopsTheWorkloadsOnTheNode covers C-65 and C-66: a
// node-local volume bound to an unschedulable node stops the workloads without
// writing the spec, the phase holds and health follows the stop rules, and the
// round that finds the node schedulable again starts the workloads back.
func TestNodeMaintenanceStopsTheWorkloadsOnTheNode(t *testing.T) {
	store, controller, nodes := maintenanceFixture(t, v1beta1.VolumeMobilityNodeLocal, PhaseUpgrading)
	before := applicationSpec(t, store)

	status := roundOf(t, controller, store)

	if replicas := liveReplicas(t, store); replicas != 0 {
		t.Fatalf("live replicas = %d, want the stop to drive them to 0", replicas)
	}
	if after := applicationSpec(t, store); after != before {
		t.Fatalf("the stop wrote the application spec: %s -> %s", before, after)
	}
	// The application entered the stop while upgrading, so the held value is
	// the one it had: a stop that wrote Ready would pass a case that only ever
	// saw Ready before the stop.
	if status.Phase != PhaseUpgrading {
		t.Fatalf("phase = %q, want the held pre-stop %q", status.Phase, PhaseUpgrading)
	}
	if status.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("health = %q, want %q for a stopped application", status.Health.State, v1beta1.HealthStateHealthy)
	}
	condition := meta.FindStatusCondition(status.Conditions, ConditionQuiesced)
	if condition == nil || condition.Reason != ReasonNodeMaintenance {
		t.Fatalf("quiesced condition = %+v, want reason %q", condition, ReasonNodeMaintenance)
	}
	if !strings.Contains(condition.Message, "node-a") {
		t.Fatalf("quiesced message %q does not name the node", condition.Message)
	}

	// The node returns to scheduling: the same round starts the workloads back
	// to the replicas the spec names (C-65).
	nodes["node-a"] = false
	status = roundOf(t, controller, store)

	if replicas := liveReplicas(t, store); replicas != 2 {
		t.Fatalf("live replicas = %d after the node returned, want the spec value 2", replicas)
	}
	if condition := meta.FindStatusCondition(status.Conditions, ConditionQuiesced); condition != nil {
		t.Fatalf("a schedulable node left the stop recorded: %+v", condition)
	}
	if status.Phase != PhaseReady || status.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("phase/health = %q/%q after the node returned, want %q/%q",
			status.Phase, status.Health.State, PhaseReady, v1beta1.HealthStateHealthy)
	}
	if after := applicationSpec(t, store); after != before {
		t.Fatalf("resuming wrote the application spec: %s -> %s", before, after)
	}
}

// TestNodeMaintenanceHoldsThePhaseItEnteredWith covers C-66's phase rule for
// every phase an application can enter a stop with: the stop holds the value it
// found instead of writing one of its own (AC-60).
func TestNodeMaintenanceHoldsThePhaseItEnteredWith(t *testing.T) {
	// A Failed application is not reconciled at all, so the phases a stop can
	// meet are the ones a running application carries.
	for _, phase := range []string{PhaseReady, PhaseUpgrading, PhaseInstalling} {
		t.Run(phase, func(t *testing.T) {
			store, controller, _ := maintenanceFixture(t, v1beta1.VolumeMobilityNodeLocal, phase)
			status := roundOf(t, controller, store)
			if status.Phase != phase {
				t.Fatalf("phase = %q, want the pre-stop %q", status.Phase, phase)
			}
			if replicas := liveReplicas(t, store); replicas != 0 {
				t.Fatalf("live replicas = %d, want the stop to drive them to 0", replicas)
			}
			if status.Health.State != v1beta1.HealthStateHealthy {
				t.Fatalf("health = %q, want %q for a stopped application", status.Health.State, v1beta1.HealthStateHealthy)
			}
		})
	}
}

// TestNodeMaintenanceLeavesMovableVolumesRunning covers C-65's placement rule: a
// volume that can follow its workload to another node is not stopped by a
// cordoned node.
func TestNodeMaintenanceLeavesMovableVolumesRunning(t *testing.T) {
	store, controller, _ := maintenanceFixture(t, v1beta1.VolumeMobilityClusterMovable, PhaseReady)

	status := roundOf(t, controller, store)

	if replicas := liveReplicas(t, store); replicas != 2 {
		t.Fatalf("live replicas = %d, want the movable volume left at 2", replicas)
	}
	if condition := meta.FindStatusCondition(status.Conditions, ConditionQuiesced); condition != nil {
		t.Fatalf("a movable volume was stopped for the node: %+v", condition)
	}
}

// failingReader answers every read with the same failure.
type failingReader struct {
	Store
}

// Get fails the read.
func (failingReader) Get(context.Context, write.Ref) (*unstructured.Unstructured, error) {
	return nil, errors.New("the read failed")
}

// TestStoreNodesReadsTheMaintenanceState covers C-65's default node source: the
// node's scheduling state is read from the object, a node the cluster does not
// have is not in maintenance, and a read failure propagates.
func TestStoreNodesReadsTheMaintenanceState(t *testing.T) {
	store := liveStore{objects: map[string]*unstructured.Unstructured{
		"Node||node-a": {Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Node",
			"metadata":   map[string]any{"name": "node-a"},
			"spec":       map[string]any{"unschedulable": true},
		}},
		"Node||node-b": {Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Node",
			"metadata":   map[string]any{"name": "node-b"},
			"spec":       map[string]any{},
		}},
	}}
	nodes := storeNodes{store: store}

	if unschedulable, err := nodes.IsUnschedulable(context.Background(), "node-a"); err != nil || !unschedulable {
		t.Fatalf("node-a = %v/%v, want it read as unschedulable", unschedulable, err)
	}
	if unschedulable, err := nodes.IsUnschedulable(context.Background(), "node-b"); err != nil || unschedulable {
		t.Fatalf("node-b = %v/%v, want it read as schedulable", unschedulable, err)
	}
	if unschedulable, err := nodes.IsUnschedulable(context.Background(), "node-gone"); err != nil || unschedulable {
		t.Fatalf("a missing node = %v/%v, want it read as schedulable", unschedulable, err)
	}
	if _, err := (storeNodes{store: failingReader{}}).IsUnschedulable(context.Background(), "node-a"); err == nil {
		t.Fatal("a failed read was answered as a scheduling state")
	}
}
