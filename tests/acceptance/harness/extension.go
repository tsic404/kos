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
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	installation, err := c.ExtensionInstaller(artifacts).Install(ctx)
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
	content, err := extension.Marshal(declaration)
	if err != nil {
		t.Fatalf("marshal declaration %s: %v", name, err)
	}
	reference := "test://" + name
	artifacts[reference] = content
	c.Create(t, &unstructured.Unstructured{Object: Component(name, reference, extension.DigestOf(content))})
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
