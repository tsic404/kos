package controlplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/volume"
)

// fakeSource serves a fixed object set for the plan cases.
type fakeSource struct {
	objects []ObservedObject
	listErr error
}

// List implements ObjectSource.
func (f *fakeSource) List(_ context.Context, objectType ObjectType, namespace string) ([]ObservedObject, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
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

// TestBuildPlanAppliesTheApplicationFieldBoundary covers F3 on the desired-set
// path: a plan (and therefore an apply) refuses an Application whose config
// repeats a field the Volume or Endpoint object owns.
func TestBuildPlanAppliesTheApplicationFieldBoundary(t *testing.T) {
	server := planServer()
	_, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{{
			"apiVersion": api.Group + "/v1beta1",
			"kind":       "Application",
			"metadata":   map[string]any{"name": "web", "namespace": "default"},
			"spec":       map[string]any{"config": map[string]any{"size": "20Gi"}},
		}},
	})
	if failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("failure = %v, want %s", failure, CodeInvalidField)
	}
}

// volumeObject is one Volume manifest with the given spec.
func volumeObject(namespace, name string, spec map[string]any) map[string]any {
	manifest := object("Volume", namespace, name, map[string]any{markers.ManagedBy: "kos-shell"}, nil)
	manifest["spec"] = spec
	return manifest
}

// storageClassObject is one native storage class.
func storageClassObject(name, reclaimPolicy string, isDefault bool) map[string]any {
	metadata := map[string]any{"name": name}
	if isDefault {
		metadata["annotations"] = map[string]any{"storageclass.kubernetes.io/is-default-class": "true"}
	}
	return map[string]any{
		"apiVersion":    "storage.k8s.io/v1",
		"kind":          "StorageClass",
		"metadata":      metadata,
		"provisioner":   "standard.example",
		"reclaimPolicy": reclaimPolicy,
	}
}

// TestBuildPlanWarnsBeforeCreatingAVolumeWithDeleteReclaim pins C-39's create
// warning: a volume whose storage class reclaims the backend data says so
// before the object is created, a Retain class does not, and an unnamed class
// is judged by the cluster default class (k3s's local-path is a Delete class).
func TestBuildPlanWarnsBeforeCreatingAVolumeWithDeleteReclaim(t *testing.T) {
	named := func() map[string]any {
		return map[string]any{"mobility": "cluster-movable", "capacity": "1Gi", "storageClass": "standard"}
	}
	unnamed := func() map[string]any {
		return map[string]any{"mobility": "cluster-movable", "capacity": "1Gi"}
	}
	cases := []struct {
		name      string
		classes   []map[string]any
		spec      map[string]any
		delete    int
		wantClass string
	}{
		{name: "named delete reclaim", classes: []map[string]any{storageClassObject("standard", "Delete", false)}, spec: named(), delete: 1, wantClass: "standard"},
		{name: "named default policy reclaims", classes: []map[string]any{storageClassObject("standard", "", false)}, spec: named(), delete: 1, wantClass: "standard"},
		{name: "named retain reclaim", classes: []map[string]any{storageClassObject("standard", "Retain", false)}, spec: named()},
		{name: "named class missing", spec: named()},
		{name: "default class reclaims", classes: []map[string]any{storageClassObject("local-path", "Delete", true)}, spec: unnamed(), delete: 1, wantClass: "local-path"},
		{name: "default class retains", classes: []map[string]any{storageClassObject("local-path", "Retain", true)}, spec: unnamed()},
		{name: "no default class", classes: []map[string]any{storageClassObject("standard", "Delete", false)}, spec: unnamed()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := planServer(testCase.classes...)
			plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
				Objects: []map[string]any{volumeObject("default", "data", testCase.spec)},
			})
			if failure != nil {
				t.Fatalf("build plan: %v", failure)
			}
			if len(plan.Create) != 1 {
				t.Fatalf("create = %v", plan.Create)
			}
			reclaim := 0
			for _, warning := range plan.Warnings {
				if warning.Code != WarningReclaimDelete {
					continue
				}
				reclaim++
				if warning.Object == nil || warning.Object.Name != "data" {
					t.Fatalf("warning object = %v", warning.Object)
				}
				if !strings.Contains(warning.Message, testCase.wantClass) {
					t.Fatalf("warning message %q does not name the class %q", warning.Message, testCase.wantClass)
				}
			}
			if reclaim != testCase.delete {
				t.Fatalf("reclaim warnings = %d, want %d (%v)", reclaim, testCase.delete, plan.Warnings)
			}
		})
	}
}

// TestBuildPlanWarnsConservativelyWhenTheDefaultClassIsUnreadable pins C-39's
// unnamed-class fallback: an unanswerable policy is reported as unknown, never
// as safe.
func TestBuildPlanWarnsConservativelyWhenTheDefaultClassIsUnreadable(t *testing.T) {
	server := planServer()
	server.source.(*fakeSource).listErr = errors.New("storage classes unavailable")
	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{volumeObject("default", "data", map[string]any{
			"mobility": "cluster-movable", "capacity": "1Gi",
		})},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	if len(plan.Warnings) != 1 || plan.Warnings[0].Code != WarningReclaimUnknown {
		t.Fatalf("warnings = %v, want one conservative reclaim warning", plan.Warnings)
	}
}

// TestBuildPlanLogsAFailedClassReadOnce pins C-39's failure log: a class set the
// plan cannot read warns on every volume it affects, but the shared error is
// logged once per plan rather than once per volume.
func TestBuildPlanLogsAFailedClassReadOnce(t *testing.T) {
	var logged bytes.Buffer
	server := &Server{
		source:     &fakeSource{listErr: errors.New("storage classes unavailable")},
		authorizer: AllowAll{},
		now:        time.Now,
		log:        slog.New(slog.NewTextHandler(&logged, nil)),
	}
	spec := func() map[string]any {
		return map[string]any{"mobility": "cluster-movable", "capacity": "1Gi"}
	}
	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{
			volumeObject("default", "data", spec()),
			volumeObject("default", "logs", spec()),
		},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	if failures := strings.Count(logged.String(), "storage class read failed"); failures != 1 {
		t.Fatalf("failed class read logged %d times, want 1: %s", failures, logged.String())
	}
	unknown := 0
	for _, warning := range plan.Warnings {
		if warning.Code == WarningReclaimUnknown {
			unknown++
		}
	}
	if unknown != 2 {
		t.Fatalf("conservative warnings = %d, want one per volume (%v)", unknown, plan.Warnings)
	}
}

// TestBuildPlanReadsTheClassSetOncePerPlan pins C-39's read bound: a plan that
// creates several volumes with an unnamed class lists the class set once, not
// once per object, and the named branch resolves against the same index.
func TestBuildPlanReadsTheClassSetOncePerPlan(t *testing.T) {
	source := &countingSource{stubSource: stubSource{objects: []ObservedObject{
		observe(storageClassObject("local-path", "Delete", true)),
		observe(storageClassObject("standard", "Delete", false)),
	}}}
	server := &Server{source: source, authorizer: AllowAll{}, now: time.Now}
	unnamed := func() map[string]any {
		return map[string]any{"mobility": "cluster-movable", "capacity": "1Gi"}
	}
	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{
			volumeObject("default", "data", unnamed()),
			volumeObject("default", "logs", unnamed()),
			volumeObject("default", "cache", map[string]any{
				"mobility": "cluster-movable", "capacity": "1Gi", "storageClass": "standard",
			}),
		},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	// Both branches must share the one index: a per-class get for the named
	// volume is as much a regression as a second list for the unnamed ones.
	classLists, classGets := 0, 0
	for _, listed := range source.lists {
		if strings.HasPrefix(listed, storageClassType.Kind+"?") {
			classLists++
		}
	}
	for _, got := range source.gets {
		if strings.HasPrefix(got, storageClassType.Kind+"?") {
			classGets++
		}
	}
	if classLists != 1 || classLists+classGets != 1 {
		t.Fatalf("class reads = %v lists + %v gets, want one StorageClass list and no get",
			source.lists, source.gets)
	}
	if len(plan.Create) != 3 {
		t.Fatalf("create = %v", plan.Create)
	}
	reclaim := 0
	for _, warning := range plan.Warnings {
		if warning.Code != WarningReclaimDelete {
			continue
		}
		reclaim++
	}
	if reclaim != 3 {
		t.Fatalf("reclaim warnings = %d, want 3 (%v)", reclaim, plan.Warnings)
	}
}

// fixedExpansion answers the C-38 preflight with one fixed plan.
type fixedExpansion struct {
	plan volume.ExpansionPlan
	err  error
}

// ExpansionPreflight implements VolumeExpansion.
func (f fixedExpansion) ExpansionPreflight(context.Context, string, string) (volume.ExpansionPlan, error) {
	return f.plan, f.err
}

// TestBuildPlanReportsTheExpansionPreflight pins C-38's three-question answer
// on the write path: a capacity increase carries the backend, filesystem and
// duration answers, and an unanswerable one says downtime is required.
func TestBuildPlanReportsTheExpansionPreflight(t *testing.T) {
	stored := volumeObject("default", "data", map[string]any{"mobility": "cluster-movable", "capacity": "1Gi"})
	server := planServer(stored)
	server.volumeExpansion = fixedExpansion{plan: volume.ExpansionPlan{
		BackendOnline: true, FilesystemOnline: false, DowntimeRequired: true,
	}}
	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{volumeObject("default", "data", map[string]any{
			"mobility": "cluster-movable", "capacity": "10Gi",
		})},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	if len(plan.Update) != 1 {
		t.Fatalf("update = %v", plan.Update)
	}
	found := false
	for _, warning := range plan.Warnings {
		if warning.Code != WarningExpansionPreflight {
			continue
		}
		found = true
		for _, fragment := range []string{"后端在线扩容=支持", "文件系统在线扩容=不支持", "预计耗时=不可判定", "需要停机"} {
			if !strings.Contains(warning.Message, fragment) {
				t.Fatalf("warning message %q does not carry %q", warning.Message, fragment)
			}
		}
	}
	if !found {
		t.Fatalf("no expansion preflight warning in %v", plan.Warnings)
	}
}

// TestBuildPlanSkipsThePreflightWhenTheCapacityDoesNotGrow pins the trigger:
// a plan that only rewrites another field never answers an expansion question.
func TestBuildPlanSkipsThePreflightWhenTheCapacityDoesNotGrow(t *testing.T) {
	stored := volumeObject("default", "data", map[string]any{"mobility": "cluster-movable", "capacity": "10Gi"})
	server := planServer(stored)
	server.volumeExpansion = fixedExpansion{plan: volume.ExpansionPlan{DowntimeRequired: true}}
	plan, _, failure := server.buildPlan(context.Background(), VerbPlan, SetRequest{
		Objects: []map[string]any{volumeObject("default", "data", map[string]any{
			"mobility": "cluster-movable", "capacity": "10Gi", "storageClass": "standard",
		})},
	})
	if failure != nil {
		t.Fatalf("build plan: %v", failure)
	}
	for _, warning := range plan.Warnings {
		if warning.Code == WarningExpansionPreflight {
			t.Fatalf("a non-growing plan answered the preflight: %v", warning)
		}
	}
}
