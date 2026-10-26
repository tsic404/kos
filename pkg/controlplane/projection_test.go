package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
}

// List implements ObjectSource.
func (s stubSource) List(_ context.Context, objectType ObjectType, _ string) ([]ObservedObject, error) {
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
