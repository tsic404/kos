package diagnostics

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
)

// fakeSource serves the objects and failures a case needs.
type fakeSource struct {
	objects  map[Target]*unstructured.Unstructured
	getErrs  map[Target]error
	listErrs map[string]error
}

func (f *fakeSource) Get(_ context.Context, target Target) (Observed, error) {
	key := Target{
		Group: target.Group, Version: target.Version, Kind: target.Kind,
		Resource: target.Resource, Namespace: target.Namespace, Name: target.Name,
	}
	if err := f.getErrs[key]; err != nil {
		return Observed{}, err
	}
	object, found := f.objects[key]
	if !found {
		return Observed{}, apierrors.NewNotFound(schema.GroupResource{Group: target.Group, Resource: target.Resource}, target.Name)
	}
	return Observed{Ref: refOf(target, object), Object: object, ObservedAt: time.Unix(0, 0).UTC()}, nil
}

func (f *fakeSource) List(_ context.Context, target Target) ([]Observed, error) {
	if err := f.listErrs[target.Kind]; err != nil {
		return nil, err
	}
	selector, err := fields.ParseSelector(target.FieldSelector)
	if err != nil {
		return nil, err
	}
	observed := make([]Observed, 0, len(f.objects))
	for key, object := range f.objects {
		if key.Group != target.Group || key.Kind != target.Kind {
			continue
		}
		if target.Namespace != "" && key.Namespace != target.Namespace {
			continue
		}
		if !selector.Matches(involvedFields(object)) {
			continue
		}
		observed = append(observed, Observed{Ref: refOf(key, object), Object: object, ObservedAt: time.Unix(0, 0).UTC()})
	}
	return observed, nil
}

// involvedFields is the field set the API server selects events on, so the
// fake narrows a list the way a real source does.
func involvedFields(object *unstructured.Unstructured) fields.Set {
	set := fields.Set{}
	kind, _, _ := unstructured.NestedString(object.Object, "involvedObject", "kind")
	name, _, _ := unstructured.NestedString(object.Object, "involvedObject", "name")
	if kind != "" {
		set["involvedObject.kind"] = kind
	}
	if name != "" {
		set["involvedObject.name"] = name
	}
	return set
}

// TestIngressBackendsReadBothForms pins the two Ingress backend shapes and the
// default backend (C-43). Nested field helpers reject the array indices this
// shape needs, so the parser reads maps and slices itself.
func TestIngressBackendsReadBothForms(t *testing.T) {
	route := Observed{
		Ref: ObjectRef{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Namespace: "apps", Name: "entry"},
		Object: &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "networking.k8s.io/v1",
			"kind":       "Ingress",
			"metadata":   map[string]any{"name": "entry", "namespace": "apps"},
			"spec": map[string]any{
				"defaultBackend": map[string]any{"resource": map[string]any{
					"apiGroup": "widgets.example.test", "kind": "Widget", "name": "shared",
				}},
				"rules": []any{
					map[string]any{"http": map[string]any{"paths": []any{
						map[string]any{"backend": map[string]any{"service": map[string]any{
							"name": "frontend", "port": map[string]any{"number": int64(8080)},
						}}},
						map[string]any{"backend": map[string]any{"resource": map[string]any{
							"kind": "ConfigMap", "name": "settings",
						}}},
					}}},
					map[string]any{"http": map[string]any{"paths": []any{
						map[string]any{"backend": map[string]any{"service": map[string]any{
							"name": "frontend", "port": map[string]any{"name": "http"},
						}}},
					}}},
				},
			},
		}},
	}
	refs := backendsOf(route)
	want := []backendRef{
		{Group: "widgets.example.test", Kind: "Widget", Name: "shared", Namespace: "apps"},
		{Kind: "Service", Name: "frontend", Namespace: "apps", Port: "8080"},
		{Kind: "ConfigMap", Name: "settings", Namespace: "apps"},
	}
	if len(refs) != len(want) {
		t.Fatalf("backends = %+v, want %+v", refs, want)
	}
	for index, ref := range want {
		if refs[index] != ref {
			t.Fatalf("backend %d = %+v, want %+v", index, refs[index], ref)
		}
	}
}

// TestExtensionReadFailureIsChainError pins that a read failure of an adapter's
// object fails the chain like any other segment; only an absent adapter makes a
// segment unobservable (C-26).
func TestExtensionReadFailureIsChainError(t *testing.T) {
	source := chainFixture()
	source.getErrs[widgetTarget()] = errors.New("permission denied")
	service := &Service{
		Source: source,
		Environment: Environment{Adapters: []Adapter{{
			ID: "widget-diagnostics", Group: "widgets.example.test", Kind: "Widget",
			Version: "v1", Resource: "widgets", Segment: LinkExtension, Source: "adapter source",
		}}},
		Now: func() time.Time { return time.Unix(0, 0).UTC() },
	}
	if _, err := service.Chain(context.Background(), "default", "shop"); err == nil {
		t.Fatal("a read failure of the adapter object did not fail the chain")
	}
}

// TestMissingExtensionObjectIsFact pins the NotFound path: the object being
// gone is evidence, not an unobservable segment, and the degradation reason
// names that object and its segment (C-26).
func TestMissingExtensionObjectIsFact(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		segment Link
	}{
		{"declared segment", LinkExtension},
		{"default dependency segment", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			adapter := Adapter{
				ID: "widget-diagnostics", Group: "widgets.example.test", Kind: "Widget",
				Version: "v1", Resource: "widgets", Segment: testCase.segment, Source: "adapter source",
			}
			service := &Service{
				Source:      chainFixture(),
				Environment: Environment{Adapters: []Adapter{adapter}},
				Now:         func() time.Time { return time.Unix(0, 0).UTC() },
			}
			chain, err := service.Chain(context.Background(), "default", "shop")
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			widget := segmentByKind(t, chain, "Widget")
			if !widget.Observable {
				t.Fatalf("a missing extension object was marked unobservable: %s", widget.Reason)
			}
			if len(widget.Evidence) != 1 || widget.Evidence[0].Source != "adapter source" {
				t.Fatalf("widget evidence = %+v", widget.Evidence)
			}
			// The application is degraded too, so the reason has to outrank the
			// application segment and name the extension object.
			conclusion := chain.Conclusion
			if conclusion.Segment != adapter.link() || conclusion.Object != widget.Object {
				t.Fatalf("conclusion = %+v, want the missing %s object at %s", conclusion, widget.Object.Kind, adapter.link())
			}
		})
	}
}

// TestRecentEventsWindowAndUnavailable pins the one-hour window, the newest-first
// order, a cluster-scoped object's event, and the unreadable path (C-64).
func TestRecentEventsWindowAndUnavailable(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	application := ObjectRef{Group: api.Group, Kind: "Application", Namespace: "default", Name: "shop"}
	node := ObjectRef{Version: "v1", Kind: "Node", Name: "node-a"}
	source := &fakeSource{objects: map[Target]*unstructured.Unstructured{
		{Version: "v1", Kind: "Event", Resource: "events", Namespace: "default", Name: "recent"}: eventObject(
			"recent", "default", "Application", "default", "shop", now.Add(-10*time.Minute)),
		{Version: "v1", Kind: "Event", Resource: "events", Namespace: "default", Name: "outside"}: eventObject(
			"outside", "default", "Application", "default", "shop", now.Add(-2*time.Hour)),
		{Version: "v1", Kind: "Event", Resource: "events", Namespace: "default", Name: "node"}: eventObject(
			"node", "default", "Node", "", "node-a", now.Add(-5*time.Minute)),
	}}
	service := &Service{Source: source, Now: func() time.Time { return now }}
	assembly := &assembly{ref: application, observed: []Observed{
		{Ref: application}, {Ref: node},
	}}
	events := service.recentEvents(context.Background(), assembly)
	if !events.Available {
		t.Fatalf("events unavailable: %s", events.Reason)
	}
	if len(events.Events) != 2 || events.Events[0].Name != "node" || events.Events[1].Name != "recent" {
		t.Fatalf("events = %+v, want the node event then the in-window application event", events.Events)
	}
	if events.Window != "1h0m0s" {
		t.Fatalf("window = %q", events.Window)
	}

	source.listErrs = map[string]error{"Event": errors.New("source unavailable")}
	unavailable := service.recentEvents(context.Background(), assembly)
	if unavailable.Available || unavailable.Reason == "" {
		t.Fatalf("an unreadable event source reported available=%t reason=%q", unavailable.Available, unavailable.Reason)
	}
}

// TestRenderDiffResidualFields pins both directions of the render diff: a
// rendered path whose value drifted, and a live path an apply manager owns that
// the template dropped. A server-defaulted field is not drift (C-26).
func TestRenderDiffResidualFields(t *testing.T) {
	desired := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "shop", "namespace": "default"},
		"spec": map[string]any{
			"replicas": int64(2),
			"selector": map[string]any{"matchLabels": map[string]any{"app": "shop"}},
		},
	}
	live := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "shop", "namespace": "default",
			"labels": map[string]any{"legacy": "old"},
			"managedFields": []any{map[string]any{
				"manager": "application-reconciler", "operation": "Apply",
				"fieldsV1": map[string]any{
					"f:metadata": map[string]any{"f:labels": map[string]any{"f:legacy": map[string]any{}}},
					"f:spec":     map[string]any{"f:replicas": map[string]any{}},
				},
			}},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"strategy": map[string]any{"type": "RollingUpdate"},
			"selector": map[string]any{"matchLabels": map[string]any{"app": "shop"}},
		},
	}
	fields := differingFields(desired, live)
	want := []string{"metadata.labels.legacy", "spec.replicas"}
	if len(fields) != len(want) {
		t.Fatalf("diff fields = %v, want %v", fields, want)
	}
	for index, field := range want {
		if fields[index] != field {
			t.Fatalf("diff fields = %v, want %v", fields, want)
		}
	}
	for _, field := range fields {
		if field == "spec.strategy" {
			t.Fatal("a server-defaulted field read as drift")
		}
	}
}

// TestAppliedPathsResolveAssociativeKeys pins that an apply manager's field tree
// resolves associative-list keys against the live object, so a dropped list
// element field is still a residual path.
func TestAppliedPathsResolveAssociativeKeys(t *testing.T) {
	live := map[string]any{
		"metadata": map[string]any{"managedFields": []any{map[string]any{
			"operation": "Apply",
			"fieldsV1": map[string]any{
				"f:spec": map[string]any{"f:template": map[string]any{"f:spec": map[string]any{
					"f:containers": map[string]any{
						`k:{"name":"app"}`: map[string]any{
							"f:env": map[string]any{
								`k:{"name":"PASSWORD"}`: map[string]any{"f:value": map[string]any{}},
							},
						},
					},
				}}},
			},
		}}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{
				"name": "app",
				"env":  []any{map[string]any{"name": "PASSWORD", "value": "x"}},
			}},
		}}},
	}
	paths := appliedPaths(live)
	want := "spec.template.spec.containers[0].env[0].value"
	if !paths[want] {
		t.Fatalf("applied paths = %v, want %s", paths, want)
	}
}

// chainFixture is an application whose rendered set names one Widget and no
// other object; every other segment resolves to an absence.
func chainFixture() *fakeSource {
	application := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "shop", "namespace": "default"},
		"spec":       map[string]any{"namespace": "default"},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": "Degraded"},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Applied", "message": "applied",
			}},
			"rendered": []any{map[string]any{
				"group": "widgets.example.test", "version": "v1", "kind": "Widget",
				"namespace": "default", "name": "shop-widget",
			}},
		},
	}}
	return &fakeSource{
		objects: map[Target]*unstructured.Unstructured{
			{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application", Resource: "applications", Namespace: "default", Name: "shop"}: application,
		},
		getErrs:  map[Target]error{},
		listErrs: map[string]error{},
	}
}

// widgetTarget is the read target of the Widget the fixture renders.
func widgetTarget() Target {
	return Target{
		Group: "widgets.example.test", Version: "v1", Kind: "Widget",
		Resource: "widgets", Namespace: "default", Name: "shop-widget",
	}
}

// eventObject builds one stored event.
func eventObject(name, namespace, kind, involvedNamespace, involvedName string, at time.Time) *unstructured.Unstructured {
	stamp := at.Format(time.RFC3339)
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"involvedObject": map[string]any{
			"kind": kind, "namespace": involvedNamespace, "name": involvedName,
		},
		"firstTimestamp": stamp,
		"lastTimestamp":  stamp,
	}}
}

// segmentByKind returns the one segment that observed a kind.
func segmentByKind(t *testing.T, chain Chain, kind string) Segment {
	t.Helper()
	for _, segment := range chain.Segments {
		if segment.Object.Kind == kind {
			return segment
		}
	}
	t.Fatalf("no segment observed a %s", kind)
	return Segment{}
}
