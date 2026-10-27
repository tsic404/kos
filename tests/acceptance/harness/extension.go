package harness

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/extension"
)

// ExtensionState runs one install pass over the components in the cluster.
func (c *Cluster) ExtensionState(t *testing.T, artifacts extension.ArtifactSource) *extension.Installation {
	t.Helper()
	return c.ExtensionStateWith(t, artifacts, nil)
}

// ExtensionStateWith runs one install pass and settles the declared fate of
// preference data through sink (C-27, C-29); a nil sink leaves the store
// untouched.
func (c *Cluster) ExtensionStateWith(t *testing.T, artifacts extension.ArtifactSource, sink extension.PreferenceRetention) *extension.Installation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	installer := c.ExtensionInstaller(artifacts)
	installer.Preferences = sink
	installation, err := installer.Install(ctx)
	if err != nil {
		t.Fatalf("install pass: %v", err)
	}
	return installation
}

// ExtensionInstaller returns the install pipeline over this cluster.
func (c *Cluster) ExtensionInstaller(artifacts extension.ArtifactSource) *extension.Installer {
	return &extension.Installer{
		Dynamic:   c.Dynamic,
		Resources: c.Resources(),
		Artifacts: artifacts,
		Status:    c.Writer(),
	}
}

// InstallPackage creates a component for declaration and runs one install pass.
func (c *Cluster) InstallPackage(t *testing.T, name string, declaration extension.Declaration, artifacts extension.MemoryArtifacts) *extension.Installation {
	t.Helper()
	return c.InstallPackageWith(t, name, declaration, artifacts, nil)
}

// InstallPackageWith creates a component for declaration and runs one install
// pass that settles preference retention through sink (C-27, C-29).
func (c *Cluster) InstallPackageWith(t *testing.T, name string, declaration extension.Declaration, artifacts extension.MemoryArtifacts, sink extension.PreferenceRetention) *extension.Installation {
	t.Helper()
	content, err := extension.Marshal(declaration)
	if err != nil {
		t.Fatalf("marshal declaration %s: %v", name, err)
	}
	reference := "test://" + name
	artifacts[reference] = content
	c.Create(t, &unstructured.Unstructured{Object: Component(name, reference, extension.DigestOf(content))})
	return c.ExtensionStateWith(t, artifacts, sink)
}

// UpgradePackage replaces one installed component's package with a new
// declaration and runs the install pass again (C-17).
func (c *Cluster) UpgradePackage(t *testing.T, name string, declaration extension.Declaration, artifacts extension.MemoryArtifacts) *extension.Installation {
	t.Helper()
	content, err := extension.Marshal(declaration)
	if err != nil {
		t.Fatalf("marshal declaration %s: %v", name, err)
	}
	reference := "test://" + name
	artifacts[reference] = content
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.resource(t, v1beta1.GroupVersion.Version, "SystemComponent")
	component, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read component %s: %v", name, err)
	}
	if err := unstructured.SetNestedField(component.Object, extension.DigestOf(content), "spec", "package", "digest"); err != nil {
		t.Fatalf("set package digest of %s: %v", name, err)
	}
	if err := unstructured.SetNestedField(component.Object, reference, "spec", "package", "reference"); err != nil {
		t.Fatalf("set package reference of %s: %v", name, err)
	}
	if _, err := resource.Update(ctx, component, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update component %s: %v", name, err)
	}
	return c.ExtensionState(t, artifacts)
}

// DeleteComponent removes one system component, which withdraws its
// contributions on the next install pass.
func (c *Cluster) DeleteComponent(t *testing.T, name string) {
	t.Helper()
	plural, ok := c.Plurals["SystemComponent"]
	if !ok {
		t.Fatal("the cluster serves no SystemComponent resource")
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.Dynamic.Resource(schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: plural,
	})
	if err := resource.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete component %s: %v", name, err)
	}
}

// RepointComponent points one component at a new package, the way a package
// upgrade does before the next install pass.
func (c *Cluster) RepointComponent(t *testing.T, name, reference, digest string) {
	t.Helper()
	resource := c.resourceIn(t, v1beta1.GroupVersion.Version, "SystemComponent", "")
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	object, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read component %s: %v", name, err)
	}
	if err := unstructured.SetNestedField(object.Object, reference, "spec", "package", "reference"); err != nil {
		t.Fatalf("set component %s reference: %v", name, err)
	}
	if err := unstructured.SetNestedField(object.Object, digest, "spec", "package", "digest"); err != nil {
		t.Fatalf("set component %s digest: %v", name, err)
	}
	if _, err := resource.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update component %s: %v", name, err)
	}
}

// ComponentStatus reads the stored status of one system component.
func (c *Cluster) ComponentStatus(t *testing.T, name string) map[string]any {
	t.Helper()
	object := c.Get(t, "v1beta1", "SystemComponent", name)
	status, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil || !found {
		t.Fatalf("component %s has no status: found=%v err=%v", name, found, err)
	}
	return status
}
