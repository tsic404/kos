package volume

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/markers"
)

// Residual is one released claim no volume binds (C-39).
type Residual struct {
	// Namespace is where the claim lives.
	Namespace string
	// Name is the claim name.
	Name string
}

// Residuals enumerates the claims that carry the release marker and are bound
// by no volume. The uninstall legacy list includes exactly these objects
// (C-39, C-54).
func (c *Controller) Residuals(ctx context.Context, namespace string) ([]Residual, error) {
	claims, err := c.store.List(ctx, pvcResource, namespace, markers.Released)
	if err != nil {
		return nil, err
	}
	volumes, err := c.store.List(ctx, volumeResource, namespace, "")
	if err != nil {
		return nil, err
	}
	bound := make(map[string]bool, 2*len(volumes))
	for index := range volumes {
		volume := &volumes[index]
		key := volume.GetNamespace() + "/" + volume.GetName()
		bound[key] = true
		if claim, _, _ := unstructured.NestedString(volume.Object, "status", "boundClaim"); claim != "" {
			bound[volume.GetNamespace()+"/"+claim] = true
		}
	}
	residuals := make([]Residual, 0, len(claims))
	for index := range claims {
		claim := &claims[index]
		key := claim.GetNamespace() + "/" + claim.GetName()
		if bound[key] {
			continue
		}
		residuals = append(residuals, Residual{Namespace: claim.GetNamespace(), Name: claim.GetName()})
	}
	sort.Slice(residuals, func(left, right int) bool {
		if residuals[left].Namespace != residuals[right].Namespace {
			return residuals[left].Namespace < residuals[right].Namespace
		}
		return residuals[left].Name < residuals[right].Name
	})
	return residuals, nil
}
