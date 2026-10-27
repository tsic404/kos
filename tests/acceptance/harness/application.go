package harness

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// InstallDefinition writes one application definition as the catalogue would
// (C-03).
func (c *Cluster) InstallDefinition(t *testing.T, definition *v1beta1.ApplicationDefinition) {
	t.Helper()
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(definition)
	if err != nil {
		t.Fatalf("encode definition %q: %v", definition.Name, err)
	}
	object := &unstructured.Unstructured{Object: document}
	object.SetGroupVersionKind(schema.GroupVersionKind{Group: api.Group, Version: "v1beta1", Kind: "ApplicationDefinition"})
	object.SetLabels(map[string]string{markers.ManagedBy: write.ManagedByValue})
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := c.Writer().Create(ctx, write.SubjectCatalogueSync, write.Session{}, object); err != nil {
		t.Fatalf("install definition %q: %v", definition.Name, err)
	}
}

// ApplicationManifest builds the body that creates one application through the
// control plane (C-12, C-32).
func ApplicationManifest(name, namespace, definitionRef, targetNamespace string, config map[string]any, volumes []string) map[string]any {
	spec := map[string]any{
		"definitionRef": definitionRef,
		"namespace":     targetNamespace,
	}
	if len(config) > 0 {
		spec["config"] = config
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	body := Manifest("Application", namespace, name, map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)
	body["spec"] = spec
	return body
}

// ApplicationController builds the real application controller over the
// cluster. A case drives it round by round, so a lifecycle step is observed
// before the next one runs (C-06).
func (c *Cluster) ApplicationController(t *testing.T, options app.Options) *app.Controller {
	t.Helper()
	return app.NewController(app.NewWriterStore(c.Writer()), options)
}

// configTemplate renders one ConfigMap in the declared namespace. Lifecycle
// cases use it where the package content itself is not under test.
const configTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: "acceptance"
`

// Definition builds a definition that renders one ConfigMap and carries the
// given capability requirements. Its package digest is computed from the
// content, as the catalogue does (C-32).
func Definition(t *testing.T, name string, requires []v1beta1.CapabilityRequirement) *v1beta1.ApplicationDefinition {
	t.Helper()
	definition := &v1beta1.ApplicationDefinition{
		TypeMeta: metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "ApplicationDefinition"},
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1beta1.ApplicationDefinitionSpec{
			Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
			Template: configTemplate,
			Requires: requires,
		},
	}
	digest, err := app.DefinitionDigest(definition.Spec)
	if err != nil {
		t.Fatalf("digest definition %q: %v", name, err)
	}
	definition.Spec.Package.Digest = digest
	if err := app.ValidateDefinition(definition.Spec); err != nil {
		t.Fatalf("definition %q is invalid: %v", name, err)
	}
	return definition
}
