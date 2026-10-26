package app_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// renderedModel is a definition whose output depends only on the config.
func renderedModel(template string) v1beta1.ApplicationDefinitionSpec {
	return v1beta1.ApplicationDefinitionSpec{
		Config: []v1beta1.ConfigField{{
			Name:    "replicas",
			Type:    app.TypeInteger,
			Default: new("2"),
		}},
		Template: template,
	}
}

const renderedTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  replicas: "{{ .Config.replicas }}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: {{ .Config.replicas }}
`

// encode renders objects into one comparable string.
func encode(t *testing.T, objects any) string {
	t.Helper()
	encoded, err := json.Marshal(objects)
	if err != nil {
		t.Fatalf("encode the render: %v", err)
	}
	return string(encoded)
}

func TestRenderIsStableAcrossRuns(t *testing.T) {
	spec := renderedModel(renderedTemplate)
	input := app.RenderInput{
		Name:      "demo",
		Namespace: "apps",
		Config:    map[string]any{"replicas": int64(2)},
	}
	first, err := app.Render(spec, input)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	second, err := app.Render(spec, input)
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("rendered %d objects, want 2", len(first))
	}
	if encode(t, first) != encode(t, second) {
		t.Fatal("the same input rendered differently")
	}
	for _, object := range first {
		if object.GetNamespace() != "apps" {
			t.Errorf("%s landed in %q, want apps", object.GetKind(), object.GetNamespace())
		}
	}
}

func TestRenderRefusesObjectsOutsideTheDeclaredNamespace(t *testing.T) {
	spec := renderedModel(`apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: elsewhere
`)
	_, err := app.Render(spec, app.RenderInput{Name: "demo", Namespace: "apps"})
	var invalid *app.InvalidPackage
	if !errors.As(err, &invalid) {
		t.Fatalf("error %v is not an InvalidPackage", err)
	}
}

func TestRenderRefusesStorageClassAndVolumeTemplates(t *testing.T) {
	cases := map[string]string{
		"storage class": `apiVersion: v1
kind: PersistentVolume
metadata:
  name: {{ .Name }}
spec:
  storageClassName: fast
`,
		"volume claims": `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ .Name }}
`,
		"claim templates": `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Name }}
spec:
  volumeClaimTemplates:
    - metadata:
        name: data
`,
		"storage class field": `apiVersion: example.com/v1
kind: Widget
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  storageClassName: fast
`,
	}
	for name, template := range cases {
		_, err := app.Render(renderedModel(template), app.RenderInput{Name: "demo", Namespace: "apps"})
		var invalid *app.InvalidPackage
		if !errors.As(err, &invalid) {
			t.Errorf("%s: error %v is not an InvalidPackage", name, err)
		}
	}
}

func TestRenderRefusesAnEmptyPackage(t *testing.T) {
	if _, err := app.Render(v1beta1.ApplicationDefinitionSpec{}, app.RenderInput{Name: "demo", Namespace: "apps"}); err == nil {
		t.Fatal("a definition without a template rendered")
	}
}

func TestDefinitionDigestIgnoresItself(t *testing.T) {
	spec := renderedModel(renderedTemplate)
	first, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	spec.Package.Digest = first
	second, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest with the digest set: %v", err)
	}
	if first != second {
		t.Fatalf("the digest covers itself: %s vs %s", first, second)
	}
	if err := app.ValidateDefinition(spec); err != nil {
		t.Fatalf("a digest-carrying definition is invalid: %v", err)
	}
}

func TestHookResultFollowsExitStatus(t *testing.T) {
	if !app.HookSucceeded(0) {
		t.Fatal("a zero exit status must succeed")
	}
	for _, code := range []int{1, 2, 127} {
		if app.HookSucceeded(code) {
			t.Errorf("exit status %d must fail", code)
		}
	}
}

func TestRenderRefusesClusterScopedKinds(t *testing.T) {
	cases := map[string]string{
		"namespace": `apiVersion: v1
kind: Namespace
metadata:
  name: elsewhere
`,
		"cluster role": `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: admin-again
`,
		"storage class": `apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fast
`,
	}
	for name, template := range cases {
		_, err := app.Render(renderedModel(template), app.RenderInput{Name: "demo", Namespace: "apps"})
		var invalid *app.InvalidPackage
		if !errors.As(err, &invalid) {
			t.Errorf("%s: error %v is not an InvalidPackage", name, err)
		}
	}
}

func TestRenderRefusesEphemeralVolumeClaimTemplate(t *testing.T) {
	template := `apiVersion: v1
kind: Pod
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  volumes:
    - name: data
      ephemeral:
        volumeClaimTemplate:
          spec:
            accessModes: ["ReadWriteOnce"]
`
	_, err := app.Render(renderedModel(template), app.RenderInput{Name: "demo", Namespace: "apps"})
	var invalid *app.InvalidPackage
	if !errors.As(err, &invalid) {
		t.Fatalf("error %v is not an InvalidPackage", err)
	}
}

// TestRenderIgnoresStorageKeysOnMetadataCarriers covers the boundary of the
// package rule: a label or annotation key is data, not a volume declaration.
func TestRenderIgnoresStorageKeysOnMetadataCarriers(t *testing.T) {
	template := `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
  labels:
    storageClassName: fast
  annotations:
    volumeClaimTemplates: "not a template"
`
	if _, err := app.Render(renderedModel(template), app.RenderInput{Name: "demo", Namespace: "apps"}); err != nil {
		t.Fatalf("metadata carriers were treated as volume declarations: %v", err)
	}
}

// TestValidateDefinitionRejectsADigestMismatch covers C-32's pin check directly.
func TestValidateDefinitionRejectsADigestMismatch(t *testing.T) {
	spec := renderedModel(renderedTemplate)
	spec.Package.Digest = app.DigestPrefix + strings.Repeat("0", 64)
	if err := app.ValidateDefinition(spec); err == nil {
		t.Fatal("a definition whose digest does not match its content was accepted")
	}
}
