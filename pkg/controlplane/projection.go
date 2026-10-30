package controlplane

import (
	"context"
	"encoding/json"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
)

// StaleThreshold is the one freshness bound every source type shares: a
// projection observed longer ago than this is marked stale (C-13). Object
// classes do not get their own values.
const StaleThreshold = 2 * time.Minute

// VolumeImpactNodeLoss is the loss impact of a node-local binding: the
// workload cannot follow its data to another node, so losing that node makes
// what depends on it unavailable (C-38).
const VolumeImpactNodeLoss = "node-loss-unavailable"

// VolumeImpactUnresolved is the impact of a referenced volume the projection
// cannot resolve: the binding is unknown, never silently absent (C-38).
const VolumeImpactUnresolved = "volume-unresolved"

// VolumeBinding is the constraint one node-local volume puts on an application
// projection: the node its data is bound to and what losing that node means
// (C-38).
type VolumeBinding struct {
	// Volume is the volume object the application references.
	Volume string `json:"volume"`
	// Node is the node the volume's data is bound to; empty while unbound.
	Node string `json:"node,omitempty"`
	// Impact is the consequence of losing the binding node.
	Impact string `json:"impact"`
	// Lost reports a binding already beyond the confirmation window.
	Lost bool `json:"lost,omitempty"`
}

// Projection is what the read path returns: the object and the moment it was
// observed (C-13). Stale is set here rather than left to the page so every
// caller marks data the same way.
type Projection struct {
	// Ref identifies the projected object.
	Ref ObjectRef `json:"ref"`
	// ObservedAt is when the source observed this object.
	ObservedAt time.Time `json:"observedAt"`
	// Stale tells whether ObservedAt is older than StaleThreshold at read time.
	Stale bool `json:"stale"`
	// Sources lists the discovery adapters that reported this object; empty
	// unless a discovery index is configured (C-25).
	Sources []string `json:"sources,omitempty"`
	// Object is the projected object as the source returned it.
	Object map[string]any `json:"object"`
}

// newProjection marks the projection stale when its observation is older than
// StaleThreshold. The comparison is strict: an observation exactly at the
// threshold is still fresh. A stale volume is projected unhealthy as well,
// because its state was not observed within the freshness bound (C-13, C-38).
func newProjection(ref ObjectRef, object map[string]any, observedAt, now time.Time) Projection {
	stale := now.Sub(observedAt) > StaleThreshold
	if stale && ref.Kind == "Volume" {
		object = unobservedVolume(object)
	}
	return Projection{
		Ref:        ref,
		ObservedAt: observedAt.UTC(),
		Stale:      stale,
		Object:     object,
	}
}

// unobservedVolume copies a volume and replaces its health with Unknown. The
// stored status is never written: only the projection says the state could not
// be observed (C-38).
func unobservedVolume(object map[string]any) map[string]any {
	status, isStatus := object["status"].(map[string]any)
	if !isStatus || len(status) == 0 {
		return object
	}
	health, isHealth := status["health"].(map[string]any)
	if !isHealth || len(health) == 0 {
		return object
	}
	projectedHealth := maps.Clone(health)
	projectedHealth["state"] = string(v1beta1.HealthStateUnknown)
	projectedStatus := maps.Clone(status)
	projectedStatus["health"] = projectedHealth
	projected := maps.Clone(object)
	projected["status"] = projectedStatus
	return projected
}

// refOf builds the reference of an object read from a source.
func refOf(object map[string]any, objectType ObjectType) ObjectRef {
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	namespace := ""
	if objectType.Namespaced {
		namespace, _ = metadata["namespace"].(string)
	}
	return ObjectRef{Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: name}
}

// applicationVolumes returns the namespace an application's volumes live in
// and the names it references; ok is false when the projection carries none.
// The declared namespace wins over the application object's own namespace,
// because that is where the render and the claims live (C-32, C-38).
func applicationVolumes(object map[string]any) (string, []string, bool) {
	if text(object["kind"]) != "Application" {
		return "", nil, false
	}
	names, _, _ := unstructured.NestedStringSlice(object, "spec", "volumes")
	if len(names) == 0 {
		return "", nil, false
	}
	namespace := nestedText(object, "spec", "namespace")
	if namespace == "" {
		namespace = nestedText(object, "metadata", "namespace")
	}
	if namespace == "" {
		return "", nil, false
	}
	return namespace, names, true
}

// attachVolumeBindings adds C-38's node-local bindings to a read page. Each
// target namespace is read once, after the same read authorization every other
// projection passes; a failed read or a refused authorization fails the page
// instead of silently dropping the binding (C-13, C-38, C-59).
func (s *Server) attachVolumeBindings(ctx context.Context, page []Projection) *Error {
	namespaces := make(map[string]bool)
	for index := range page {
		if namespace, _, ok := applicationVolumes(page[index].Object); ok {
			namespaces[namespace] = true
		}
	}
	if len(namespaces) == 0 {
		return nil
	}
	volumeType, known := kernelType("Volume")
	if !known {
		return nil
	}
	indexes := make(map[string]map[string]map[string]any, len(namespaces))
	for namespace := range namespaces {
		ref := ObjectRef{Group: volumeType.Group, Kind: volumeType.Kind, Namespace: namespace}
		if _, failure := s.authorize(ctx, VerbRead, ref, nil, nil); failure != nil {
			return failure
		}
		observed, err := s.source.List(ctx, volumeType, namespace)
		if err != nil {
			s.log.Error("volume binding read failed", "namespace", namespace, "error", err)
			return internal(StageRead, "读取 Volume 失败").about(ref)
		}
		index := make(map[string]map[string]any, len(observed))
		for _, item := range observed {
			index[nestedText(item.Object, "metadata", "name")] = item.Object
		}
		indexes[namespace] = index
	}
	for index := range page {
		namespace, names, ok := applicationVolumes(page[index].Object)
		if !ok {
			continue
		}
		page[index].Object = withVolumeBindings(page[index].Object, names, indexes[namespace])
	}
	return nil
}

// withVolumeBindings returns a copy of an application projection carrying the
// node-local bindings found in the namespace's volume index. A referenced
// volume the index does not carry is reported as unresolved, never dropped; a
// movable volume contributes nothing (C-38).
func withVolumeBindings(object map[string]any, names []string, index map[string]map[string]any) map[string]any {
	bindings := make([]any, 0, len(names))
	for _, name := range names {
		volume, found := index[name]
		if !found {
			bindings = append(bindings, map[string]any{
				"volume": name,
				"impact": VolumeImpactUnresolved,
			})
			continue
		}
		mobility, _, _ := unstructured.NestedString(volume, "spec", "mobility")
		if mobility != string(v1beta1.VolumeMobilityNodeLocal) {
			continue
		}
		node, _, _ := unstructured.NestedString(volume, "status", "node")
		phase, _, _ := unstructured.NestedString(volume, "status", "phase")
		bindings = append(bindings, map[string]any{
			"volume": name,
			"node":   node,
			"impact": VolumeImpactNodeLoss,
			"lost":   phase == "Lost",
		})
	}
	if len(bindings) == 0 {
		return object
	}
	status, _ := object["status"].(map[string]any)
	projectedStatus := maps.Clone(status)
	if projectedStatus == nil {
		projectedStatus = map[string]any{}
	}
	projectedStatus["volumeBindings"] = bindings
	projected := maps.Clone(object)
	projected["status"] = projectedStatus
	return projected
}

// discoverySources names the adapters that reported one object (C-25).
func (s *Server) discoverySources(ref ObjectRef) []string {
	if s.discoveries == nil {
		return nil
	}
	return s.discoveries.Sources(ref.Group, ref.Kind, ref.Namespace, ref.Name)
}

// projectedObject returns one object as a reader sees it: an extension object
// whose controller is unavailable is Unknown, and every other object is
// returned unchanged (C-08, C-20). The object the source returned is never
// modified, so a shared cache stays stored-state only.
func (s *Server) projectedObject(objectType ObjectType, object map[string]any) map[string]any {
	if s.controllers == nil || objectType.IsKernel() {
		return object
	}
	stored, isStatus := object["status"].(map[string]any)
	if !isStatus || len(stored) == 0 {
		return object
	}
	status := v1beta1.ObjectStatus{}
	if err := reshapeJSON(stored, &status); err != nil {
		s.log.Warn("extension object status is unreadable", "kind", objectType.Kind, "error", err)
		return object
	}
	projected := s.controllers.ProjectedStatus(objectType.Group, objectType.Kind, status)
	if equality.Semantic.DeepEqual(projected, status) {
		return object
	}
	value := map[string]any{}
	if err := reshapeJSON(projected, &value); err != nil {
		s.log.Warn("projected status is not encodable", "kind", objectType.Kind, "error", err)
		return object
	}
	copied := make(map[string]any, len(object))
	maps.Copy(copied, object)
	copied["status"] = value
	return copied
}

// reshapeJSON decodes one JSON value into the shape another one needs; the
// status map a source returns and its typed form carry the same encoding.
func reshapeJSON(source, target any) error {
	encoded, err := json.Marshal(source)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}
