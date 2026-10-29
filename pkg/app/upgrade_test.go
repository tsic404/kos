package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// TestSpecDigestCoversTheUpgradeInput covers C-34: the digest an upgrade answers
// names the definition, namespace, config and volume declarations, and nothing
// the workloads do not depend on.
func TestSpecDigestCoversTheUpgradeInput(t *testing.T) {
	base := v1beta1.ApplicationSpec{
		DefinitionRef: "web",
		Namespace:     "apps",
		Config:        runtime.RawExtension{Raw: []byte(`{"mode":"a"}`)},
		Volumes:       []string{"data", "cache"},
	}
	digest, err := SpecDigest(base)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	same := base
	same.Config = runtime.RawExtension{Raw: []byte(`{ "mode" : "a" }`)}
	same.Volumes = []string{"cache", "data"}
	same.Owner = "someone"
	same.Visibility = v1beta1.Visibility{AllowGuests: true}
	repeated, err := SpecDigest(same)
	if err != nil {
		t.Fatalf("digest of the reordered spec: %v", err)
	}
	if repeated != digest {
		t.Fatalf("owner, visibility, volume order or config whitespace moved the digest: %s != %s", repeated, digest)
	}

	changed := base
	changed.Config = runtime.RawExtension{Raw: []byte(`{"mode":"b"}`)}
	other, err := SpecDigest(changed)
	if err != nil {
		t.Fatalf("digest of the changed spec: %v", err)
	}
	if other == digest {
		t.Fatal("a config change left the spec digest unchanged")
	}

	// The lifecycle facts are written into the same spec but are not upgrade
	// input: a stop, a scale or a resource override never asks for an upgrade
	// (C-34, C-66, C-67).
	lifecycle := base
	lifecycle.Replicas = new(int32(7))
	lifecycle.Paused = true
	lifecycle.Resources = &v1beta1.ResourceRequirements{
		Requests: v1beta1.ResourceList{CPU: "250m"},
		Limits:   v1beta1.ResourceList{CPU: "1"},
	}
	moved, err := SpecDigest(lifecycle)
	if err != nil {
		t.Fatalf("digest of the lifecycle spec: %v", err)
	}
	if moved != digest {
		t.Fatalf("replicas, paused or resources moved the spec digest: %s != %s", moved, digest)
	}
}

// TestRenderDigestIsOrderIndependent covers C-34: the same manifest set digests
// the same however it is ordered, and one changed value moves it.
func TestRenderDigestIsOrderIndependent(t *testing.T) {
	first := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "a", "namespace": "apps"},
		"data":     map[string]any{"mode": "one"},
	}}
	second := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "b", "namespace": "apps"},
		"data":     map[string]any{"mode": "two"},
	}}
	digest, err := RenderDigest([]*unstructured.Unstructured{first, second})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	reversed, err := RenderDigest([]*unstructured.Unstructured{second, first})
	if err != nil {
		t.Fatalf("digest of the reversed set: %v", err)
	}
	if reversed != digest {
		t.Fatalf("the manifest order moved the digest: %s != %s", reversed, digest)
	}
	changed := first.DeepCopy()
	changed.Object["data"] = map[string]any{"mode": "three"}
	other, err := RenderDigest([]*unstructured.Unstructured{changed, second})
	if err != nil {
		t.Fatalf("digest of the changed set: %v", err)
	}
	if other == digest {
		t.Fatal("a changed value left the render digest unchanged")
	}
}

// fixedVolumes serves the volume facts a preflight reads.
type fixedVolumes map[string]VolumeFacts

// VolumeFacts implements VolumeSource.
func (v fixedVolumes) VolumeFacts(_ context.Context, _, name string) (VolumeFacts, error) {
	return v[name], nil
}

// targetFor builds one rendered target from a definition and a config.
func targetFor(t *testing.T, application *v1beta1.Application, definition *v1beta1.ApplicationDefinition, config map[string]any) *target {
	t.Helper()
	controller := NewController(nil, Options{})
	next, err := controller.buildTarget(application, definition, config)
	if err != nil {
		t.Fatalf("build target: %v", err)
	}
	return next
}

// TestPreflightRefusesTheThreeIrreversibleItems covers C-34: a data format
// change, a dropped volume declaration and a shrunken volume are refused, a
// storage class change asks for a migration, and the config and manifest
// differences are reported.
func TestPreflightRefusesTheThreeIrreversibleItems(t *testing.T) {
	template := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "{{ .Config.mode }}"
`
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec: v1beta1.ApplicationSpec{
			DefinitionRef: "web",
			Namespace:     "apps",
			Config:        runtime.RawExtension{Raw: []byte(`{"mode":"old"}`)},
			Volumes:       []string{"cache", "logs", "data"},
		},
	}
	before := definitionWith(t, "web", "1", template, nil)
	applied := targetFor(t, application, before, map[string]any{"mode": "old"})

	application.Spec.Volumes = []string{"logs", "cache"}
	application.Spec.Config = runtime.RawExtension{Raw: []byte(`{"mode":"new"}`)}
	after := definitionWith(t, "web", "2", template, nil)
	next := targetFor(t, application, after, map[string]any{"mode": "new"})

	controller := NewController(nil, Options{Volumes: fixedVolumes{
		"cache": {Found: true, Bound: true, DesiredSize: "5Gi", ActualSize: "10Gi"},
		"logs":  {Found: true, Bound: true, DesiredSize: "1Gi", ActualSize: "1Gi", DesiredPolicy: "fast", ActualPolicy: "slow"},
	}})
	report, err := controller.preflight(context.Background(), application, applied.applied, next)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}

	kinds := make(map[string]int)
	for _, item := range report.Irreversible {
		kinds[item.Kind]++
	}
	for _, kind := range []string{v1beta1.IrreversibleDataFormat, v1beta1.IrreversibleVolumeDelete, v1beta1.IrreversibleVolumeShrink} {
		if kinds[kind] != 1 {
			t.Fatalf("preflight reported %d %s items, want 1; report %+v", kinds[kind], kind, report)
		}
	}
	if len(report.Migrations) != 1 || report.Migrations[0].Kind != "VolumeMigration" || report.Migrations[0].Name != "logs" {
		t.Fatalf("migrations = %+v, want one VolumeMigration named logs", report.Migrations)
	}
	if len(report.ConfigChanges) != 1 || report.ConfigChanges[0].Field != "mode" {
		t.Fatalf("config changes = %+v, want the mode field", report.ConfigChanges)
	}
	if len(report.RenderChanges) != 1 || report.RenderChanges[0].Operation != v1beta1.RenderUpdate {
		t.Fatalf("render changes = %+v, want one update", report.RenderChanges)
	}
	if report.RenderDigest != next.convergedDigest || report.SpecDigest != next.applied.SpecDigest {
		t.Fatalf("the report does not name the target: %+v", report)
	}
}

// TestPreflightAcceptsARevertedDefinition covers the normal upgrade: a new
// package, the same data format and the same volumes, so nothing is
// irreversible.
func TestPreflightAcceptsARevertedDefinition(t *testing.T) {
	template := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "fixed"
`
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec:       v1beta1.ApplicationSpec{DefinitionRef: "web", Namespace: "apps"},
	}
	before := definitionWith(t, "web", "", template, nil)
	applied := targetFor(t, application, before, map[string]any{})

	upgraded := definitionWith(t, "web", "", strings.Replace(template, "fixed", "moved", 1), nil)
	next := targetFor(t, application, upgraded, map[string]any{})

	controller := NewController(nil, Options{})
	report, err := controller.preflight(context.Background(), application, applied.applied, next)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(report.Irreversible) != 0 {
		t.Fatalf("irreversible = %+v, want none", report.Irreversible)
	}
	if len(report.Migrations) != 0 {
		t.Fatalf("migrations = %+v, want none", report.Migrations)
	}
	if len(report.RenderChanges) != 1 || report.RenderChanges[0].Operation != v1beta1.RenderUpdate {
		t.Fatalf("render changes = %+v, want the changed data field", report.RenderChanges)
	}
}

// TestRecordableDriftRefusesWhatConfigCannotExpress covers C-35: the adopt
// policy records a value the config structure can produce and refuses one it
// cannot.
func TestRecordableDriftRefusesWhatConfigCannotExpress(t *testing.T) {
	definition := definitionWithConfig(t, []v1beta1.ConfigField{{Name: "mode", Type: TypeString}},
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "{{ .Config.mode }}"
  fixed: "constant"
`)
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec:       v1beta1.ApplicationSpec{DefinitionRef: "web", Namespace: "apps", Config: runtime.RawExtension{Raw: []byte(`{"mode":"a"}`)}},
	}
	config := map[string]any{"mode": "a"}
	paths, err := configPaths(definition.Spec, renderInput(application, config), config)
	if err != nil {
		t.Fatalf("config paths: %v", err)
	}
	ref := v1beta1.ObjectRef{Kind: "ConfigMap", Namespace: "apps", Name: "web"}

	expressible := []v1beta1.DriftEntry{{Object: ref, Path: "data.mode", External: `"b"`, Expected: `"a"`}}
	if entries, ok := recordableDrift(expressible, paths); !ok || len(entries) != 1 {
		t.Fatalf("a config-driven difference was refused: %v %v", entries, ok)
	}
	hardcoded := []v1beta1.DriftEntry{{Object: ref, Path: "data.fixed", External: `"changed"`, Expected: `"constant"`}}
	if entries, ok := recordableDrift(hardcoded, paths); ok || entries != nil {
		t.Fatalf("a difference the config cannot express was recorded: %v", entries)
	}
}

// TestRunHookReportsExitStatus covers C-32: a hook's exit status is its result.
func TestRunHookReportsExitStatus(t *testing.T) {
	if err := RunHook(context.Background(), v1beta1.Hook{Name: HookUpgrade, Command: []string{"sh", "-c", "exit 0"}}); err != nil {
		t.Fatalf("a succeeding hook failed: %v", err)
	}
	if err := RunHook(context.Background(), v1beta1.Hook{Name: HookUpgrade, Command: []string{"sh", "-c", "exit 3"}}); err == nil {
		t.Fatal("a failing hook reported success")
	}
}

// definitionWith builds a definition pinned to its own digest.
func definitionWith(t *testing.T, name, dataFormat, template string, hooks []v1beta1.Hook) *v1beta1.ApplicationDefinition {
	t.Helper()
	return definitionWithConfig(t, nil, template, func(spec *v1beta1.ApplicationDefinitionSpec) {
		spec.Package = v1beta1.PackageIdentity{Version: "1.0.0", DataFormat: dataFormat}
		spec.Hooks = hooks
	}, name)
}

// definitionWithConfig builds a definition of one config structure and
// template, pinned to the digest of its own content.
func definitionWithConfig(t *testing.T, fields []v1beta1.ConfigField, template string, options ...any) *v1beta1.ApplicationDefinition {
	t.Helper()
	name := "web"
	adjust := func(spec *v1beta1.ApplicationDefinitionSpec) {}
	for _, option := range options {
		switch typed := option.(type) {
		case string:
			name = typed
		case func(*v1beta1.ApplicationDefinitionSpec):
			adjust = typed
		}
	}
	definition := &v1beta1.ApplicationDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1beta1.ApplicationDefinitionSpec{
			Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
			Config:   fields,
			Template: template,
		},
	}
	adjust(&definition.Spec)
	digest, err := DefinitionDigest(definition.Spec)
	if err != nil {
		t.Fatalf("digest definition %q: %v", name, err)
	}
	definition.Spec.Package.Digest = digest
	if err := ValidateDefinition(definition.Spec); err != nil {
		t.Fatalf("definition %q is invalid: %v", name, err)
	}
	return definition
}

// TestManifestDiffReportsDrops covers C-34: a manifest the upgrade no longer
// renders is reported as a drop, not silently forgotten.
func TestManifestDiffReportsDrops(t *testing.T) {
	kept := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "a", "namespace": "apps"},
		"data":     map[string]any{"mode": "one"},
	}}
	dropped := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "b", "namespace": "apps"},
	}}
	keptRaw, err := json.Marshal(kept.Object)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	droppedRaw, err := json.Marshal(dropped.Object)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	changes, err := manifestDiff([]runtime.RawExtension{{Raw: keptRaw}, {Raw: droppedRaw}}, []*unstructured.Unstructured{kept})
	if err != nil {
		t.Fatalf("manifest diff: %v", err)
	}
	if len(changes) != 1 || changes[0].Operation != v1beta1.RenderDelete || changes[0].Object.Name != "b" {
		t.Fatalf("changes = %+v, want the drop of b", changes)
	}
}

// TestDataFormatDeclarationChangeIsRefused covers C-34's missing-declarations
// rule: a package that drops its data format declaration or starts declaring one
// is refused exactly like a value change, because the running version wrote data
// under the old declaration.
func TestDataFormatDeclarationChangeIsRefused(t *testing.T) {
	template := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "fixed"
`
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec:       v1beta1.ApplicationSpec{DefinitionRef: "web", Namespace: "apps"},
	}
	cases := []struct {
		name   string
		before string
		after  string
		want   bool
	}{
		{name: "declared then dropped", before: "1", after: "", want: true},
		{name: "undeclared then declared", before: "", after: "1", want: true},
		{name: "value changed", before: "1", after: "2", want: true},
		{name: "same value", before: "1", after: "1"},
		{name: "both undeclared", before: "", after: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			applied := targetFor(t, application, definitionWith(t, "web", test.before, template, nil), map[string]any{})
			next := targetFor(t, application, definitionWith(t, "web", test.after, template, nil), map[string]any{})
			controller := NewController(nil, Options{})
			report, err := controller.preflight(context.Background(), application, applied.applied, next)
			if err != nil {
				t.Fatalf("preflight: %v", err)
			}
			refused := false
			for _, item := range report.Irreversible {
				if item.Kind == v1beta1.IrreversibleDataFormat {
					refused = true
				}
			}
			if refused != test.want {
				t.Fatalf("data format %q -> %q refused = %v, want %v; report %+v", test.before, test.after, refused, test.want, report.Irreversible)
			}
		})
	}
}

// TestPreflightRefusesAnUndecidableVolume covers C-34's no-guess rule: a bound
// volume whose sizes cannot be read is refused instead of passing as unchanged.
func TestPreflightRefusesAnUndecidableVolume(t *testing.T) {
	template := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "fixed"
`
	application := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", UID: "uid-1"},
		Spec:       v1beta1.ApplicationSpec{DefinitionRef: "web", Namespace: "apps", Volumes: []string{"data"}},
	}
	applied := targetFor(t, application, definitionWith(t, "web", "", template, nil), map[string]any{})
	next := targetFor(t, application, definitionWith(t, "web", "", template, nil), map[string]any{})
	controller := NewController(nil, Options{Volumes: fixedVolumes{
		"data": {Found: true, Bound: true, DesiredSize: "1Gi"},
	}})
	report, err := controller.preflight(context.Background(), application, applied.applied, next)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(report.Irreversible) != 1 || report.Irreversible[0].Kind != v1beta1.IrreversibleVolumeShrink {
		t.Fatalf("irreversible = %+v, want one volumeShrink refusal", report.Irreversible)
	}
	if report.Irreversible[0].Detail == "" {
		t.Fatal("the refusal does not say why the volume cannot be judged")
	}
}

// liveStore serves the objects a manifest set names.
type liveStore struct {
	Store
	objects map[string]*unstructured.Unstructured
}

// Get returns the stored object of one identity.
func (s liveStore) Get(_ context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	object, ok := s.objects[ref.Kind+"|"+ref.Namespace+"|"+ref.Name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: ref.Kind}, ref.Name)
	}
	return object.DeepCopy(), nil
}

// TestObservedRenderDigestIsAnObservation covers C-35: the third condition of a
// confirmation is the downstream state the cluster holds, so it matches the
// target only when the objects really carry it.
func TestObservedRenderDigestIsAnObservation(t *testing.T) {
	manifest := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "web", "namespace": "apps"},
		"data":     map[string]any{"version": "two"},
	}}
	digest, err := RenderDigest([]*unstructured.Unstructured{manifest})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	live := manifest.DeepCopy()
	live.SetResourceVersion("42")
	live.SetLabels(map[string]string{"managed-by": "kos-shell", "kos.tsic.top/owner": "uid-1"})
	key := "ConfigMap|apps|web"

	observed := func(objects map[string]*unstructured.Unstructured) string {
		t.Helper()
		controller := NewController(liveStore{objects: objects}, Options{})
		value, err := controller.observedRenderDigest(context.Background(), []*unstructured.Unstructured{manifest})
		if err != nil {
			t.Fatalf("observed digest: %v", err)
		}
		return value
	}

	if got := observed(map[string]*unstructured.Unstructured{key: live}); got != digest {
		t.Fatalf("the object that holds the manifest observed %s, want %s", got, digest)
	}
	changed := live.DeepCopy()
	changed.Object["data"] = map[string]any{"version": "three"}
	if got := observed(map[string]*unstructured.Unstructured{key: changed}); got == digest {
		t.Fatal("a value written outside KOS left the observed digest at the target")
	}
	if got := observed(map[string]*unstructured.Unstructured{}); got == digest {
		t.Fatal("a missing object observed the target digest")
	}
}
