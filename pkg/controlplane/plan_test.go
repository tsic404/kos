package controlplane

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/pkg/markers"
)

// fakeSource serves a fixed object set for the plan cases.
type fakeSource struct {
	objects []ObservedObject
}

// List implements ObjectSource.
func (f *fakeSource) List(_ context.Context, objectType ObjectType, namespace string) ([]ObservedObject, error) {
	listed := make([]ObservedObject, 0, len(f.objects))
	for _, object := range f.objects {
		if text(object.Object["kind"]) != objectType.Kind {
			continue
		}
		if namespace != "" && nestedText(object.Object, "metadata", "namespace") != namespace {
			continue
		}
		listed = append(listed, object)
	}
	return listed, nil
}

// Get implements ObjectSource.
func (f *fakeSource) Get(_ context.Context, objectType ObjectType, namespace, name string) (ObservedObject, error) {
	for _, object := range f.objects {
		if text(object.Object["kind"]) != objectType.Kind || nestedText(object.Object, "metadata", "name") != name {
			continue
		}
		if namespace != "" && nestedText(object.Object, "metadata", "namespace") != namespace {
			continue
		}
		return object, nil
	}
	return ObservedObject{}, apierrors.NewNotFound(schema.GroupResource{Group: objectType.Group, Resource: objectType.Resource}, name)
}

// observe wraps an object as a source observation.
func observe(object map[string]any) ObservedObject {
	return ObservedObject{Object: object, ObservedAt: time.Now()}
}

// planServer builds a server over a fake source; the plan path never writes.
func planServer(objects ...map[string]any) *Server {
	source := &fakeSource{}
	for _, object := range objects {
		source.objects = append(source.objects, observe(object))
	}
	return &Server{source: source, authorizer: AllowAll{}, now: time.Now}
}

// object builds an object of a kernel kind.
func object(kind, namespace, name string, labels, annotations map[string]any) map[string]any {
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	if labels != nil {
		metadata["labels"] = labels
	}
	if annotations != nil {
		metadata["annotations"] = annotations
	}
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       kind,
		"metadata":   metadata,
		"spec":       map[string]any{},
	}
}

// TestBuildPlanReportsTheThreeDeviationClasses checks create, update and delete
// reporting, the retained data warning and the rendered summary (C-62).
func TestBuildPlanReportsTheThreeDeviationClasses(t *testing.T) {
	server := planServer(
		object("ApplicationDefinition", "", "existing",
			map[string]any{markers.ManagedBy: "kos-shell", markers.OwnerUID: "install-1"},
			map[string]any{"example.test/release": "v1"}),
		object("SystemComponent", "", "kept",
			map[string]any{markers.ManagedBy: "kos-shell", markers.OwnerUID: "install-1"}, nil),
		object("Volume", "default", "data", map[string]any{markers.OwnerUID: "install-1"}, nil),
		object("BackupPolicy", "default", "dropped", map[string]any{markers.OwnerUID: "install-1"}, nil),
	)

	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Scope: &Scope{OwnerUID: "install-1"},
		Objects: []map[string]any{
			object("ApplicationDefinition", "", "existing",
				map[string]any{markers.ManagedBy: "kos-shell", markers.OwnerUID: "install-1"},
				map[string]any{"example.test/release": "v2"}),
			object("SystemComponent", "", "kept",
				map[string]any{markers.ManagedBy: "kos-shell", markers.OwnerUID: "install-1"}, nil),
			object("SystemComponent", "", "fresh",
				map[string]any{markers.ManagedBy: "kos-shell", markers.OwnerUID: "install-1"}, nil),
		},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}

	if len(plan.Create) != 1 || plan.Create[0].Ref.Name != "fresh" {
		t.Fatalf("create = %v", plan.Create)
	}
	if len(plan.Update) != 1 || plan.Update[0].Ref.Name != "existing" {
		t.Fatalf("update = %v", plan.Update)
	}
	if len(plan.Update[0].Changes) != 1 || plan.Update[0].Changes[0].Path != "metadata.annotations.example.test/release" {
		t.Fatalf("update changes = %v", plan.Update[0].Changes)
	}
	if len(plan.Delete) != 1 || plan.Delete[0].Ref.Name != "dropped" {
		t.Fatalf("delete = %v", plan.Delete)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != WarningRetained {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
	want := "计划创建 1 个对象，更新 1 个对象，删除 1 个对象，保留 1 个对象"
	if plan.Summary != want {
		t.Fatalf("summary = %q, want %q", plan.Summary, want)
	}
}

// TestBuildPlanWithoutScopeDeletesNothing checks that an install that declares no
// scope never removes objects it cannot prove it owns (C-02).
func TestBuildPlanWithoutScopeDeletesNothing(t *testing.T) {
	server := planServer(object("BackupPolicy", "default", "orphan", map[string]any{markers.OwnerUID: "install-1"}, nil))
	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{object("SystemComponent", "", "fresh", nil, nil)},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	if len(plan.Delete) != 0 {
		t.Fatalf("delete = %v, want none", plan.Delete)
	}
	if len(plan.Create) != 1 {
		t.Fatalf("create = %v", plan.Create)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != WarningUnattributed {
		t.Fatalf("warnings = %v, want one unattributed warning", plan.Warnings)
	}
}

// TestBuildPlanRejectsDuplicatesAndEmptySets checks the request bounds.
func TestBuildPlanRejectsDuplicatesAndEmptySets(t *testing.T) {
	server := planServer()
	duplicate := object("SystemComponent", "", "twice", nil, nil)
	if _, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{duplicate, duplicate},
	}); failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("duplicate object = %v, want an invalid_field failure", failure)
	}
	if _, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{}); failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("empty set = %v, want an invalid_field failure", failure)
	}
}
