package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
)

// TestNewProjectionMarksStaleBeyondThreshold pins the one freshness bound every
// source type shares: only an observation strictly older than two minutes is
// stale (C-13).
func TestNewProjectionMarksStaleBeyondThreshold(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ref := ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "web"}
	object := map[string]any{"kind": "Application"}

	cases := []struct {
		name  string
		age   time.Duration
		stale bool
	}{
		{name: "read now", age: 0, stale: false},
		{name: "one nanosecond below the threshold", age: StaleThreshold - time.Nanosecond, stale: false},
		{name: "exactly at the threshold", age: StaleThreshold, stale: false},
		{name: "one nanosecond beyond the threshold", age: StaleThreshold + time.Nanosecond, stale: true},
		{name: "three minutes old", age: 3 * time.Minute, stale: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observedAt := now.Add(-testCase.age)
			projection := newProjection(ref, object, observedAt, now)
			if projection.Stale != testCase.stale {
				t.Fatalf("stale = %v, want %v for an age of %s", projection.Stale, testCase.stale, testCase.age)
			}
			if !projection.ObservedAt.Equal(observedAt) {
				t.Fatalf("observedAt = %s, want %s", projection.ObservedAt, observedAt)
			}
			if projection.Ref != ref {
				t.Fatalf("ref = %v, want %v", projection.Ref, ref)
			}
		})
	}
}

// stubSource serves one fixed object set.
type stubSource struct {
	objects []ObservedObject
	listErr error
}

// List implements ObjectSource.
func (s stubSource) List(_ context.Context, objectType ObjectType, _ string) ([]ObservedObject, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	matched := make([]ObservedObject, 0, len(s.objects))
	for _, object := range s.objects {
		if text(object.Object["kind"]) == objectType.Kind && nestedText(object.Object, "apiVersion") == objectType.Group+"/"+objectType.Version {
			matched = append(matched, object)
		}
	}
	return matched, nil
}

// Get implements ObjectSource.
func (s stubSource) Get(_ context.Context, objectType ObjectType, _, name string) (ObservedObject, error) {
	for _, object := range s.objects {
		metadata, _ := object.Object["metadata"].(map[string]any)
		if metadata["name"] == name && text(object.Object["kind"]) == objectType.Kind {
			return object, nil
		}
	}
	return ObservedObject{}, apierrors.NewNotFound(schema.GroupResource{Resource: "objects"}, name)
}

// oneClassKinds serves one extension class.
type oneClassKinds struct{}

// ObjectClass implements ExtensionKinds.
func (oneClassKinds) ObjectClass(group, kind string) (ExtensionObjectClass, bool) {
	if group == "widgets.example.test" && kind == "Widget" {
		return ExtensionObjectClass{Versions: []string{"v1"}, Resource: "widgets", Namespaced: true}, true
	}
	return ExtensionObjectClass{}, false
}

// unavailableWidgets projects one class Unknown, as a failed controller does.
type unavailableWidgets struct{}

// ProjectedStatus implements ControllerAvailability.
func (unavailableWidgets) ProjectedStatus(group, kind string, stored v1beta1.ObjectStatus) v1beta1.ObjectStatus {
	if group == "widgets.example.test" && kind == "Widget" {
		stored.Health.State = v1beta1.HealthStateUnknown
	}
	return stored
}

// TestReadPathProjectsAnUnavailableController checks that the API both lists and
// reads an extension object whose controller is unavailable as Unknown, leaves
// another class alone, and never writes the projection back (C-08, C-20).
func TestReadPathProjectsAnUnavailableController(t *testing.T) {
	widget := map[string]any{
		"apiVersion": "widgets.example.test/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "one", "namespace": "default"},
		"status":     map[string]any{"health": map[string]any{"state": "Healthy"}},
	}
	thing := map[string]any{
		"apiVersion": "things.example.test/v1",
		"kind":       "Thing",
		"metadata":   map[string]any{"name": "two", "namespace": "default"},
		"status":     map[string]any{"health": map[string]any{"state": "Healthy"}},
	}
	observedAt := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	server, err := New(Config{
		Dynamic:     &writeRecorder{},
		Source:      stubSource{objects: []ObservedObject{{Object: widget, ObservedAt: observedAt}, {Object: thing, ObservedAt: observedAt}}},
		Extensions:  oneClassKinds{},
		Controllers: unavailableWidgets{},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}

	list := httptest.NewRecorder()
	server.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet,
		"/api/v1/resources/Widget?apiVersion=widgets.example.test/v1&namespace=default", nil))
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", list.Code, list.Body)
	}
	listed := map[string]any{}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	items, _ := listed["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	item, _ := items[0].(map[string]any)
	if state := nestedText(item, "object", "status", "health", "state"); state != string(v1beta1.HealthStateUnknown) {
		t.Fatalf("listed health = %q, want Unknown", state)
	}

	one := httptest.NewRecorder()
	server.Handler().ServeHTTP(one, httptest.NewRequest(http.MethodGet,
		"/api/v1/resources/Widget/one?apiVersion=widgets.example.test/v1&namespace=default", nil))
	if one.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", one.Code, one.Body)
	}
	read := map[string]any{}
	if err := json.Unmarshal(one.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if state := nestedText(read, "object", "status", "health", "state"); state != string(v1beta1.HealthStateUnknown) {
		t.Fatalf("read health = %q, want Unknown", state)
	}

	// The source's object is untouched: the projection exists only in the answer.
	if state := nestedText(widget, "status", "health", "state"); state != "Healthy" {
		t.Fatalf("the read changed the stored object: %q", state)
	}
}

// TestVolumeProjectionUnknownWhenStale pins the volume freshness rule of C-38:
// a stale observation is projected Unknown and the stored status is untouched.
func TestVolumeProjectionUnknownWhenStale(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	ref := ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "default", Name: "data"}
	stored := map[string]any{
		"kind": "Volume",
		"status": map[string]any{
			"phase":  "Bound",
			"health": map[string]any{"state": "Healthy"},
		},
	}

	fresh := newProjection(ref, stored, now, now)
	if state := fresh.Object["status"].(map[string]any)["health"].(map[string]any)["state"]; state != "Healthy" {
		t.Fatalf("a fresh volume projects %v, want Healthy", state)
	}
	stale := newProjection(ref, stored, now.Add(-StaleThreshold-time.Second), now)
	if state := stale.Object["status"].(map[string]any)["health"].(map[string]any)["state"]; state != "Unknown" {
		t.Fatalf("a stale volume projects %v, want Unknown", state)
	}
	if phase := stale.Object["status"].(map[string]any)["phase"]; phase != "Bound" {
		t.Fatalf("phase = %v, want the stored Bound", phase)
	}
	if state := stored["status"].(map[string]any)["health"].(map[string]any)["state"]; state != "Healthy" {
		t.Fatalf("the stored status was written: health = %v", state)
	}
	other := ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "web"}
	if object := newProjection(other, stored, now.Add(-StaleThreshold-time.Second), now).Object; object["status"].(map[string]any)["health"].(map[string]any)["state"] != "Healthy" {
		t.Fatal("a stale application projection was rewritten; C-38 applies to volumes only")
	}
}

// TestApplicationProjectionCarriesNodeLocalBindings pins C-38's application
// projection: a referenced node-local volume contributes its bound node and
// loss impact, a movable volume contributes nothing, a missing volume is
// reported as unresolved instead of dropped, and the stored object is never
// rewritten.
func TestApplicationProjectionCarriesNodeLocalBindings(t *testing.T) {
	application := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "web", "namespace": "default"},
		"spec":       map[string]any{"volumes": []any{"data", "cache", "gone"}},
	}
	nodeLocal := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "data", "namespace": "default"},
		"spec":       map[string]any{"mobility": "node-local"},
		"status":     map[string]any{"phase": "Lost", "node": "node-a"},
	}
	movable := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "cache", "namespace": "default"},
		"spec":       map[string]any{"mobility": "cluster-movable"},
		"status":     map[string]any{"phase": "Bound"},
	}
	server, err := New(Config{
		Dynamic: &writeRecorder{},
		Source: stubSource{objects: []ObservedObject{
			observe(application), observe(nodeLocal), observe(movable),
		}},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/v1/resources/Application/web?namespace=default", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", response.Code, response.Body)
	}
	read := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	binding, _ := bindingsByVolume(nestedValue(read, "object", "status", "volumeBindings"))["data"]
	if binding["volume"] != "data" || binding["node"] != "node-a" {
		t.Fatalf("binding = %v, want data on node-a", binding)
	}
	if binding["impact"] != VolumeImpactNodeLoss || binding["lost"] != true {
		t.Fatalf("binding = %v, want the node-loss impact and the lost state", binding)
	}
	gone, found := bindingsByVolume(nestedValue(read, "object", "status", "volumeBindings"))["gone"]
	if !found || gone["impact"] != VolumeImpactUnresolved {
		t.Fatalf("missing volume binding = %v, want an unresolved entry", gone)
	}
	if _, present := bindingsByVolume(read["object"].(map[string]any)["status"].(map[string]any)["volumeBindings"])["cache"]; present {
		t.Fatal("a movable volume produced a binding")
	}
	if _, present := application["status"]; present {
		t.Fatalf("the read wrote the binding into the stored object: %v", application["status"])
	}
}

// TestApplicationProjectionResolvesVolumesInTheDeclaredNamespace pins the C-38
// convention: volumes live in the application's declared namespace, where its
// render and its claims live, not in the namespace of the Application object.
// The volume read is authorized like every other read (C-59).
func TestApplicationProjectionResolvesVolumesInTheDeclaredNamespace(t *testing.T) {
	application := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "web", "namespace": "apps"},
		"spec":       map[string]any{"namespace": "workloads", "volumes": []any{"data"}},
	}
	decoy := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "data", "namespace": "apps"},
		"spec":       map[string]any{"mobility": "cluster-movable"},
		"status":     map[string]any{"phase": "Bound"},
	}
	declared := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "data", "namespace": "workloads"},
		"spec":       map[string]any{"mobility": "node-local"},
		"status":     map[string]any{"phase": "Bound", "node": "node-b"},
	}
	authorizer := &recordingAuthorizer{}
	server, err := New(Config{
		Dynamic:    &writeRecorder{},
		Source:     stubSource{objects: []ObservedObject{observe(application), observe(decoy), observe(declared)}},
		Authorizer: authorizer,
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/v1/resources/Application/web?namespace=apps", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", response.Code, response.Body)
	}
	read := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	binding := bindingsByVolume(nestedValue(read, "object", "status", "volumeBindings"))["data"]
	if binding["node"] != "node-b" {
		t.Fatalf("binding = %v, want the volume in the declared namespace", binding)
	}
	volumeRead := ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "workloads"}
	if !authorizer.allowed(volumeRead) {
		t.Fatalf("the volume read was not authorized: %v", authorizer.refs)
	}
	if authorizer.allowed(ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "apps"}) {
		t.Fatalf("the projection read a volume in the application's own namespace: %v", authorizer.refs)
	}
}

// TestApplicationProjectionFailsWhenTheVolumeReadFails pins C-38's read path:
// a failed volume read fails the projection instead of silently dropping the
// binding.
func TestApplicationProjectionFailsWhenTheVolumeReadFails(t *testing.T) {
	application := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "web", "namespace": "default"},
		"spec":       map[string]any{"volumes": []any{"data"}},
	}
	server, err := New(Config{
		Dynamic: &writeRecorder{},
		Source: stubSource{
			objects: []ObservedObject{observe(application)},
			listErr: errors.New("volumes unavailable"),
		},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/v1/resources/Application/web?namespace=default", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("get status = %d, want the read to fail instead of dropping the binding; body %s",
			response.Code, response.Body)
	}
}

// recordingAuthorizer allows every request and remembers what it saw.
type recordingAuthorizer struct {
	refs []ObjectRef
}

// Decide implements Authorizer.
func (r *recordingAuthorizer) Decide(_ context.Context, request Request) error {
	r.refs = append(r.refs, request.Ref)
	return nil
}

// allowed reports whether the authorizer saw a read of exactly ref.
func (r *recordingAuthorizer) allowed(ref ObjectRef) bool {
	for _, seen := range r.refs {
		if seen == ref {
			return true
		}
	}
	return false
}

// bindingsByVolume indexes a decoded volumeBindings list by volume name.
func bindingsByVolume(value any) map[string]map[string]any {
	list, _ := value.([]any)
	byName := make(map[string]map[string]any, len(list))
	for _, raw := range list {
		binding, _ := raw.(map[string]any)
		if name, _ := binding["volume"].(string); name != "" {
			byName[name] = binding
		}
	}
	return byName
}

// nestedValue reads one nested JSON value; it returns nil on a missing path.
func nestedValue(object map[string]any, path ...string) any {
	current := any(object)
	for _, key := range path {
		value, _ := current.(map[string]any)
		if value == nil {
			return nil
		}
		current = value[key]
	}
	return current
}

// countingSource records which classes a read listed or got.
type countingSource struct {
	stubSource
	lists []string
	gets  []string
}

// List implements ObjectSource.
func (s *countingSource) List(ctx context.Context, objectType ObjectType, namespace string) ([]ObservedObject, error) {
	s.lists = append(s.lists, objectType.Kind+"?"+namespace)
	return s.stubSource.List(ctx, objectType, namespace)
}

// Get implements ObjectSource.
func (s *countingSource) Get(ctx context.Context, objectType ObjectType, namespace, name string) (ObservedObject, error) {
	s.gets = append(s.gets, objectType.Kind+"?"+namespace+"/"+name)
	return s.stubSource.Get(ctx, objectType, namespace, name)
}

// TestApplicationListResolvesBindingsWithOneVolumeRead pins the list path of
// C-38: bindings are attached after pagination from one Volume list per target
// namespace, not from a get per application and volume.
func TestApplicationListResolvesBindingsWithOneVolumeRead(t *testing.T) {
	first := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "web-1", "namespace": "default"},
		"spec":       map[string]any{"volumes": []any{"data"}},
	}
	second := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "web-2", "namespace": "default"},
		"spec":       map[string]any{"volumes": []any{"data", "cache"}},
	}
	nodeLocal := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "data", "namespace": "default"},
		"spec":       map[string]any{"mobility": "node-local"},
		"status":     map[string]any{"phase": "Bound", "node": "node-a"},
	}
	movable := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Volume",
		"metadata":   map[string]any{"name": "cache", "namespace": "default"},
		"spec":       map[string]any{"mobility": "cluster-movable"},
		"status":     map[string]any{"phase": "Bound"},
	}
	source := &countingSource{stubSource: stubSource{objects: []ObservedObject{
		observe(first), observe(second), observe(nodeLocal), observe(movable),
	}}}
	server, err := New(Config{Dynamic: &writeRecorder{}, Source: source})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"/api/v1/resources/Application?namespace=default", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", response.Code, response.Body)
	}
	read := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	items, _ := read["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want both applications", items)
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if _, found := bindingsByVolume(nestedValue(item, "object", "status", "volumeBindings"))["data"]; !found {
			t.Fatalf("application %v carries no node-local binding", nestedValue(item, "object", "metadata", "name"))
		}
	}
	volumeLists := 0
	for _, listed := range source.lists {
		if strings.HasPrefix(listed, "Volume?") {
			volumeLists++
		}
	}
	if volumeLists != 1 {
		t.Fatalf("volume lists = %d, want one per target namespace (%v)", volumeLists, source.lists)
	}
}
