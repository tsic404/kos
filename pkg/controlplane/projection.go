package controlplane

import (
	"encoding/json"
	"maps"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"

	"github.com/tsic404/kos/api/v1beta1"
)

// StaleThreshold is the one freshness bound every source type shares: a
// projection observed longer ago than this is marked stale (C-13). Object
// classes do not get their own values.
const StaleThreshold = 2 * time.Minute

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
	// Object is the projected object as the source returned it.
	Object map[string]any `json:"object"`
}

// newProjection marks the projection stale when its observation is older than
// StaleThreshold. The comparison is strict: an observation exactly at the
// threshold is still fresh.
func newProjection(ref ObjectRef, object map[string]any, observedAt, now time.Time) Projection {
	return Projection{
		Ref:        ref,
		ObservedAt: observedAt.UTC(),
		Stale:      now.Sub(observedAt) > StaleThreshold,
		Object:     object,
	}
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
