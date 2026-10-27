package volume

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// fakeStore is an in-memory Store: a round's reads and writes are observable
// without an API server.
type fakeStore struct {
	volume     *unstructured.Unstructured
	objects    map[string]*unstructured.Unstructured
	claims     []*unstructured.Unstructured
	status     map[string]any
	finalizers []string
}

func newFakeStore(volume *unstructured.Unstructured) *fakeStore {
	return &fakeStore{volume: volume, objects: map[string]*unstructured.Unstructured{}}
}

func objectKey(resource schema.GroupVersionResource, namespace, name string) string {
	return resource.String() + "/" + namespace + "/" + name
}

// Get implements Store.
func (s *fakeStore) Get(_ context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	if s.volume == nil || ref.Kind != "Volume" || ref.Name != s.volume.GetName() {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: ref.Group, Resource: ref.Kind}, ref.Name)
	}
	return s.volume.DeepCopy(), nil
}

// GetResource implements Store.
func (s *fakeStore) GetResource(_ context.Context, resource schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	object, ok := s.objects[objectKey(resource, namespace, name)]
	if !ok {
		return nil, apierrors.NewNotFound(resource.GroupResource(), name)
	}
	return object.DeepCopy(), nil
}

// List implements Store.
func (s *fakeStore) List(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error) {
	return nil, nil
}

// CreateClaim implements Store.
func (s *fakeStore) CreateClaim(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.claims = append(s.claims, document.DeepCopy())
	s.objects[objectKey(pvcResource, document.GetNamespace(), document.GetName())] = document.DeepCopy()
	return document, nil
}

// ApplyClaim implements Store.
func (s *fakeStore) ApplyClaim(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	s.claims = append(s.claims, document.DeepCopy())
	s.objects[objectKey(pvcResource, document.GetNamespace(), document.GetName())] = document.DeepCopy()
	return document, nil
}

// DeleteClaim implements Store.
func (s *fakeStore) DeleteClaim(_ context.Context, ref write.Ref) error {
	delete(s.objects, objectKey(pvcResource, ref.Namespace, ref.Name))
	return nil
}

// MarkOn implements Store.
func (s *fakeStore) MarkOn(_ context.Context, current *unstructured.Unstructured, set map[string]string, remove []string) (*unstructured.Unstructured, error) {
	updated := current.DeepCopy()
	labels := updated.GetLabels()
	for key, value := range set {
		labels[key] = value
	}
	for _, key := range remove {
		delete(labels, key)
	}
	updated.SetLabels(labels)
	s.objects[objectKey(pvcResource, updated.GetNamespace(), updated.GetName())] = updated
	return updated, nil
}

// ApplyStatus implements Store.
func (s *fakeStore) ApplyStatus(_ context.Context, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	status, _, err := unstructured.NestedMap(document.Object, "status")
	if err != nil {
		return nil, err
	}
	s.status = status
	return document, nil
}

// SetFinalizers implements Store.
func (s *fakeStore) SetFinalizers(_ context.Context, _ write.Ref, finalizers []string) (*unstructured.Unstructured, error) {
	s.finalizers = finalizers
	return s.volume, nil
}

// TestMissingMobilityStaysPending pins C-38's required shape: a volume without
// one of the three mobility values stays Pending with a readable reason and no
// claim is created.
func TestMissingMobilityStaysPending(t *testing.T) {
	volume := testVolume(map[string]any{"capacity": "1Gi"})
	store := newFakeStore(volume)
	controller := NewController(store, Options{})
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: "default", Name: "data"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requirePhase(t, store, PhasePending)
	requireCondition(t, store, ConditionInvalidSpec, "mobility")
	if len(store.claims) != 0 {
		t.Fatalf("a volume without mobility created %d claims", len(store.claims))
	}
}

// TestSharedRWXNeedsCapability pins C-38's shared requirement: without a
// registered storage.shared-rwx capability the volume stays Pending.
func TestSharedRWXNeedsCapability(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility": string(v1beta1.VolumeMobilitySharedRWX),
		"capacity": "1Gi",
	})
	store := newFakeStore(volume)
	controller := NewController(store, Options{})
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: "default", Name: "data"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requirePhase(t, store, PhasePending)
	requireCondition(t, store, ConditionCapabilityMissing, CapabilitySharedRWX)
}

// TestExpansionGrowsTheClaim pins the expansion side of C-38: a larger capacity
// is written to the claim, and a smaller one never shrinks it.
func TestExpansionGrowsTheClaim(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "10Gi",
		"storageClass": "standard",
	})
	store := newFakeStore(volume)
	store.objects[objectKey(storageClassResource, "", "standard")] = &unstructured.Unstructured{Object: map[string]any{
		"provisioner":          "standard.example",
		"allowVolumeExpansion": true,
	}}
	store.objects[objectKey(pvcResource, "default", "data")] = claimObject("4Gi", "pv-data", "Bound")
	controller := NewController(store, Options{})
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: "default", Name: "data"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(store.claims) != 1 {
		t.Fatalf("writes = %d, want one expansion", len(store.claims))
	}
	size, _, _ := unstructured.NestedString(store.claims[0].Object, "spec", "resources", "requests", "storage")
	if size != "10Gi" {
		t.Fatalf("expanded to %q, want 10Gi", size)
	}

	shrunkStore := newFakeStore(volume)
	shrunkStore.objects[objectKey(storageClassResource, "", "standard")] = store.objects[objectKey(storageClassResource, "", "standard")]
	shrunkStore.objects[objectKey(pvcResource, "default", "data")] = claimObject("20Gi", "pv-data", "Bound")
	if _, err := NewController(shrunkStore, Options{}).Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: "default", Name: "data"}); err != nil {
		t.Fatalf("reconcile a smaller capacity: %v", err)
	}
	if len(shrunkStore.claims) != 0 {
		t.Fatalf("a smaller capacity rewrote the claim: %v", shrunkStore.claims)
	}
}

// TestNodeLossRespectsTheConfirmationWindow pins the Lost timeline of C-38: a
// node-local binding is Lost only after the heartbeat grace period plus five
// minutes, and a healthy node is never lost.
func TestNodeLossRespectsTheConfirmationWindow(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		node    map[string]any
		lost    bool
		missing bool
	}{
		{
			name: "ready node",
			node: nodeObject("True", now.Add(-time.Minute)),
		},
		{
			name: "inside the window",
			node: nodeObject("False", now.Add(-5*time.Minute)),
		},
		{
			name: "beyond the window",
			node: nodeObject("False", now.Add(-6*time.Minute)),
			lost: true,
		},
		{
			name:    "node object gone",
			missing: true,
			lost:    true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := newFakeStore(testVolume(map[string]any{}))
			if !testCase.missing {
				store.objects[objectKey(nodeResource, "", "node-a")] = &unstructured.Unstructured{Object: testCase.node}
			}
			controller := NewController(store, Options{Clock: func() time.Time { return now }})
			lost, err := controller.nodeLost(context.Background(), "node-a")
			if err != nil {
				t.Fatalf("nodeLost: %v", err)
			}
			if lost != testCase.lost {
				t.Fatalf("lost = %v, want %v", lost, testCase.lost)
			}
		})
	}
}

// testVolume is one decoded volume in the default namespace.
func testVolume(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Volume",
		"metadata": map[string]any{
			"name":      "data",
			"namespace": "default",
			"uid":       "00000000-0000-0000-0000-0000000000aa",
			"labels":    map[string]any{"managed-by": "kos-shell"},
		},
		"spec": spec,
	}}
}

// claimObject is one bound claim.
func claimObject(size, pvName, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      "data",
			"namespace": "default",
			"labels":    map[string]any{"managed-by": "kos-shell"},
		},
		"spec": map[string]any{
			"volumeName": pvName,
			"resources":  map[string]any{"requests": map[string]any{"storage": size}},
		},
		"status": map[string]any{"phase": phase},
	}}
}

// nodeObject is one node with a Ready condition.
func nodeObject(ready string, heartbeat time.Time) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": "node-a"},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type":               "Ready",
				"status":             ready,
				"lastHeartbeatTime":  heartbeat.Format(time.RFC3339),
				"lastTransitionTime": heartbeat.Format(time.RFC3339),
			}},
		},
	}
}

// requirePhase requires the status phase a round wrote.
func requirePhase(t *testing.T, store *fakeStore, want string) {
	t.Helper()
	if store.status == nil {
		t.Fatal("the round wrote no status")
	}
	if phase := textValue(store.status["phase"]); phase != want {
		t.Fatalf("phase = %q, want %q", phase, want)
	}
}

// requireCondition requires one failure condition whose message names a fact.
func requireCondition(t *testing.T, store *fakeStore, conditionType, fragment string) {
	t.Helper()
	conditions, _ := store.status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] != conditionType {
			continue
		}
		if condition["status"] != "True" {
			t.Fatalf("%s is not True: %v", conditionType, condition)
		}
		if message := textValue(condition["message"]); !strings.Contains(message, fragment) {
			t.Fatalf("%s message %q does not name %q", conditionType, message, fragment)
		}
		return
	}
	t.Fatalf("no %s condition in %v", conditionType, conditions)
}

// textValue reads a string from a decoded status value.
func textValue(value any) string {
	text, _ := value.(string)
	return text
}

// TestBoundVolumeDoesNotRecreateAMissingClaim pins C-38's data boundary: a
// volume that was bound and whose claim disappeared is Lost, and no empty
// claim is created in its place.
func TestBoundVolumeDoesNotRecreateAMissingClaim(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility": string(v1beta1.VolumeMobilityClusterMovable),
		"capacity": "1Gi",
	})
	volume.Object["status"] = map[string]any{"phase": PhaseBound, "boundClaim": "data"}
	store := newFakeStore(volume)
	controller := NewController(store, Options{})
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: "default", Name: "data"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	requirePhase(t, store, PhaseLost)
	requireCondition(t, store, ConditionLost, "missing")
	if len(store.claims) != 0 {
		t.Fatalf("a bound volume recreated %d claims after its claim disappeared", len(store.claims))
	}
}

// TestRecoveryClearsFailureConditions pins C-07's recovery path: a corrected
// spec and a recovered binding each remove the failure condition the earlier
// round wrote, instead of leaving it True forever.
func TestRecoveryClearsFailureConditions(t *testing.T) {
	t.Run("invalid spec to bound", func(t *testing.T) {
		volume := testVolume(map[string]any{"capacity": "1Gi"})
		store := newFakeStore(volume)
		controller := NewController(store, Options{})
		reconcileVolume(t, controller)

		// The user corrects the spec; the claim is bound on the next round.
		volume.Object["spec"] = map[string]any{"mobility": string(v1beta1.VolumeMobilityClusterMovable), "capacity": "1Gi"}
		applyWrittenStatus(t, store, volume)
		store.objects[objectKey(pvcResource, "default", "data")] = claimObject("1Gi", "pv-data", "Bound")
		store.objects[objectKey(pvResource, "", "pv-data")] = pvObject("pv-data", nil)
		reconcileVolume(t, controller)

		requirePhase(t, store, PhaseBound)
		requireNoCondition(t, store, ConditionInvalidSpec)
		requireCondition(t, store, api.ConditionReconciled, "bound")
	})

	t.Run("lost to bound", func(t *testing.T) {
		volume := testVolume(map[string]any{
			"mobility": string(v1beta1.VolumeMobilityClusterMovable),
			"capacity": "1Gi",
		})
		volume.Object["status"] = map[string]any{
			"phase":  PhaseLost,
			"health": map[string]any{"state": "Unavailable"},
			"conditions": []any{map[string]any{
				"type": ConditionLost, "status": "True", "reason": ConditionLost,
				"message":            "persistent volume was missing",
				"lastTransitionTime": "2026-10-25T12:00:00Z",
			}},
		}
		store := newFakeStore(volume)
		store.objects[objectKey(pvcResource, "default", "data")] = claimObject("1Gi", "pv-data", "Bound")
		store.objects[objectKey(pvResource, "", "pv-data")] = pvObject("pv-data", nil)
		controller := NewController(store, Options{})
		reconcileVolume(t, controller)

		requirePhase(t, store, PhaseBound)
		requireNoCondition(t, store, ConditionLost)
		requireCondition(t, store, api.ConditionReconciled, "bound")
	})
}

// TestNodeLocalBindingMustResolveToANode pins C-38's node-local observation: a
// binding whose node cannot be resolved is unknown, never reported healthy with
// an unusable confirmation window. Only an In expression names a node.
func TestNodeLocalBindingMustResolveToANode(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility": string(v1beta1.VolumeMobilityNodeLocal),
		"capacity": "1Gi",
	})
	store := newFakeStore(volume)
	store.objects[objectKey(pvcResource, "default", "data")] = claimObject("1Gi", "pv-data", "Bound")
	store.objects[objectKey(pvResource, "", "pv-data")] = pvObject("pv-data", map[string]any{
		"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{
			"matchExpressions": []any{map[string]any{
				"key": hostnameLabel, "operator": "NotIn", "values": []any{"node-a"},
			}},
		}}},
	})
	controller := NewController(store, Options{})
	reconcileVolume(t, controller)

	requirePhase(t, store, PhaseBound)
	if health := statusText(store.status, "health", "state"); health != string(v1beta1.HealthStateUnknown) {
		t.Fatalf("health = %q, want Unknown for an unresolvable binding node", health)
	}
	condition := requireConditionStatus(t, store, api.ConditionReconciled)
	if condition["status"] != string(metav1.ConditionUnknown) {
		t.Fatalf("Reconciled = %v, want Unknown", condition["status"])
	}
	if message := textValue(condition["message"]); !strings.Contains(message, "no resolvable node") {
		t.Fatalf("Reconciled message %q does not explain the unresolvable binding", message)
	}
}

// TestLostKeepsTheBindingNode pins C-38's Lost status: losing the node does not
// lose the fact of which node the data was bound to.
func TestLostKeepsTheBindingNode(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility": string(v1beta1.VolumeMobilityNodeLocal),
		"capacity": "1Gi",
	})
	store := newFakeStore(volume)
	store.objects[objectKey(pvcResource, "default", "data")] = claimObject("1Gi", "pv-data", "Bound")
	store.objects[objectKey(pvResource, "", "pv-data")] = pvObject("pv-data", map[string]any{
		"required": map[string]any{"nodeSelectorTerms": []any{map[string]any{
			"matchExpressions": []any{map[string]any{
				"key": hostnameLabel, "operator": "In", "values": []any{"node-a"},
			}},
		}}},
	})
	controller := NewController(store, Options{})
	reconcileVolume(t, controller)

	requirePhase(t, store, PhaseLost)
	if node := textValue(store.status["node"]); node != "node-a" {
		t.Fatalf("lost binding node = %q, want node-a", node)
	}
}

// TestReleaseLeavesAForeignClaimAlone pins C-02 on the deletion path: a
// same-named claim KOS did not create is neither marked nor deleted, the volume
// explains why, and the volume's gate is released instead of blocking forever.
func TestReleaseLeavesAForeignClaimAlone(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility": string(v1beta1.VolumeMobilityClusterMovable),
		"capacity": "1Gi",
	})
	volume.Object["metadata"].(map[string]any)["finalizers"] = []any{Finalizer}
	volume.Object["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-25T12:00:00Z"
	store := newFakeStore(volume)
	foreign := claimObject("1Gi", "pv-data", "Bound")
	foreign.Object["metadata"].(map[string]any)["labels"] = map[string]any{}
	store.objects[objectKey(pvcResource, "default", "data")] = foreign
	controller := NewController(store, Options{})
	reconcileVolume(t, controller)

	for _, finalizer := range store.finalizers {
		if finalizer == Finalizer {
			t.Fatalf("the volume kept its finalizer: %v", store.finalizers)
		}
	}
	kept := store.objects[objectKey(pvcResource, "default", "data")]
	if _, marked := kept.GetLabels()[markers.Released]; marked {
		t.Fatal("a claim KOS does not own was marked released")
	}
	requireCondition(t, store, ConditionClaimBlocked, "not managed by KOS")
}

// TestReleaseRecognizesAMarkerWithAnEmptyValue pins C-39's marker semantics:
// the label's presence is the contract, so an empty value is still a release.
func TestReleaseRecognizesAMarkerWithAnEmptyValue(t *testing.T) {
	volume := testVolume(map[string]any{
		"mobility": string(v1beta1.VolumeMobilityClusterMovable),
		"capacity": "1Gi",
	})
	store := newFakeStore(volume)
	claim := claimObject("1Gi", "pv-data", "Bound")
	claim.Object["metadata"].(map[string]any)["labels"].(map[string]any)[markers.Released] = ""
	store.objects[objectKey(pvcResource, "default", "data")] = claim
	controller := NewController(store, Options{})
	reconcileVolume(t, controller)

	stored := store.objects[objectKey(pvcResource, "default", "data")]
	if _, marked := stored.GetLabels()[markers.Released]; marked {
		t.Fatal("an imported claim kept the release marker")
	}
	requireCondition(t, store, ConditionImported, "release marker")
}

// reconcileVolume runs one round and fails the test on error.
func reconcileVolume(t *testing.T, controller *Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{Kind: "Volume", Namespace: "default", Name: "data"}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// applyWrittenStatus makes the stored volume carry what the last round wrote,
// as an API server read would return it.
func applyWrittenStatus(t *testing.T, store *fakeStore, volume *unstructured.Unstructured) {
	t.Helper()
	if store.status == nil {
		t.Fatal("the round wrote no status")
	}
	volume.Object["status"] = store.status
	store.status = nil
}

// pvObject is one persistent volume with an optional node affinity.
func pvObject(name string, affinity map[string]any) *unstructured.Unstructured {
	spec := map[string]any{}
	if affinity != nil {
		spec["nodeAffinity"] = affinity
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// requireNoCondition requires the status not to carry a condition type.
func requireNoCondition(t *testing.T, store *fakeStore, conditionType string) {
	t.Helper()
	if store.status == nil {
		t.Fatal("the round wrote no status")
	}
	conditions, _ := store.status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == conditionType {
			t.Fatalf("%s is still reported after recovery: %v", conditionType, condition)
		}
	}
}

// statusText reads one nested string from a decoded status.
func statusText(status map[string]any, path ...string) string {
	current := status
	for _, key := range path[:len(path)-1] {
		nested, _ := current[key].(map[string]any)
		current = nested
	}
	return textValue(current[path[len(path)-1]])
}

// requireConditionStatus returns one condition of the written status whatever
// its value; the caller judges the value.
func requireConditionStatus(t *testing.T, store *fakeStore, conditionType string) map[string]any {
	t.Helper()
	if store.status == nil {
		t.Fatal("the round wrote no status")
	}
	conditions, _ := store.status["conditions"].([]any)
	for _, raw := range conditions {
		condition, _ := raw.(map[string]any)
		if condition["type"] == conditionType {
			return condition
		}
	}
	t.Fatalf("no %s condition in %v", conditionType, conditions)
	return nil
}
