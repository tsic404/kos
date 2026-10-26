package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// Installer evaluates the live system components and writes their status: it
// is the install pipeline of C-17 read through C-28.
type Installer struct {
	// Dynamic reads the system components.
	Dynamic dynamic.Interface
	// Resources maps a kernel kind to its served resource name.
	Resources map[schema.GroupKind]string
	// Artifacts resolves a package reference to its bytes.
	Artifacts ArtifactSource
	// Status writes component status as the component reconciler (C-03).
	Status *write.Writer
}

// Installation is the result of one install pass.
type Installation struct {
	components []ComponentResult
	registry   *Registry
}

// Components returns every evaluated component in install order.
func (i *Installation) Components() []ComponentResult {
	return append([]ComponentResult(nil), i.components...)
}

// Component returns one component's result by object name.
func (i *Installation) Component(name string) (ComponentResult, bool) {
	for _, component := range i.components {
		if component.Name == name {
			return component, true
		}
	}
	return ComponentResult{}, false
}

// Registry returns the visibility projection of the active components.
func (i *Installation) Registry() *Registry {
	return i.registry
}

// Install reads every live component, verifies its package, evaluates the
// declarations and writes the resulting status. Components without an
// attribution marker are not KOS's to write and are left alone (C-02).
func (i *Installer) Install(ctx context.Context) (*Installation, error) {
	if i.Dynamic == nil || i.Artifacts == nil || i.Status == nil {
		return nil, errors.New("installer needs a dynamic client, an artifact source and a status writer")
	}
	resource, err := i.resource()
	if err != nil {
		return nil, err
	}
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list system components: %w", err)
	}

	requests := make([]Request, 0, len(list.Items))
	objects := make(map[string]*unstructured.Unstructured, len(list.Items))
	for index := range list.Items {
		object := &list.Items[index]
		attribution, err := markers.AttributionOf(object.GetLabels())
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", object.GetName(), err)
		}
		if attribution == markers.AttributionUnmanaged {
			continue
		}
		request := Request{
			Name:       object.GetName(),
			Generation: object.GetGeneration(),
			Created:    object.GetCreationTimestamp().Time,
			IsAdopted:  attribution == markers.AttributionAdopted,
		}
		if !request.IsAdopted {
			reference, _, err := unstructured.NestedString(object.Object, "spec", "package", "reference")
			if err != nil {
				return nil, fmt.Errorf("component %s: read spec.package.reference: %w", object.GetName(), err)
			}
			digest, _, err := unstructured.NestedString(object.Object, "spec", "package", "digest")
			if err != nil {
				return nil, fmt.Errorf("component %s: read spec.package.digest: %w", object.GetName(), err)
			}
			if reference != "" {
				content, fetchErr := i.Artifacts.Fetch(ctx, reference)
				if fetchErr != nil {
					request.FetchError = fetchErr
				} else {
					request.Package = &Package{Reference: reference, Digest: digest, Content: content}
				}
			}
		}
		requests = append(requests, request)
		objects[request.Name] = object
	}

	assessment := Evaluate(requests)
	for _, result := range assessment.Components {
		object := objects[result.Name]
		if object == nil {
			continue
		}
		status := StatusFor(result, existingConditions(object))
		if err := i.writeStatus(ctx, result.Name, status, object); err != nil {
			return nil, err
		}
	}
	return &Installation{components: assessment.Components, registry: assessment.Registry}, nil
}

// resource resolves the system component client.
func (i *Installer) resource() (dynamic.ResourceInterface, error) {
	plural, ok := i.Resources[schema.GroupKind{Group: api.Group, Kind: "SystemComponent"}]
	if !ok {
		return nil, errors.New("installer has no resource name for SystemComponent")
	}
	return i.Dynamic.Resource(schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: plural,
	}), nil
}

// writeStatus applies the component status only when it differs from the stored
// one, so an unchanged round writes nothing (C-06).
func (i *Installer) writeStatus(ctx context.Context, name string, status v1beta1.ObjectStatus, current *unstructured.Unstructured) error {
	document, err := statusDocument(status)
	if err != nil {
		return fmt.Errorf("component %s: render status: %w", name, err)
	}
	stored, _, err := unstructured.NestedMap(current.Object, "status")
	if err != nil {
		return fmt.Errorf("component %s: read status: %w", name, err)
	}
	if sameJSON(stored, document) {
		return nil
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "SystemComponent",
		"metadata":   map[string]any{"name": name},
		"status":     document,
	}}
	if _, err := i.Status.Apply(ctx, write.SubjectBackupReconciler, write.Session{}, write.PartStatus, object); err != nil {
		return fmt.Errorf("component %s: write status: %w", name, err)
	}
	return nil
}

// statusDocument renders the status as the map the dynamic client writes.
func statusDocument(status v1beta1.ObjectStatus) (map[string]any, error) {
	encoded, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	if _, present := document["conditions"]; !present {
		document["conditions"] = []any{}
	}
	return document, nil
}

// existingConditions reads the stored condition list.
func existingConditions(object *unstructured.Unstructured) []metav1.Condition {
	raw, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var conditions []metav1.Condition
	if err := json.Unmarshal(encoded, &conditions); err != nil {
		return nil
	}
	return conditions
}

// sameJSON compares two decoded JSON values by their encoding.
func sameJSON(left, right any) bool {
	leftEncoded, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightEncoded, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(leftEncoded) == string(rightEncoded)
}
