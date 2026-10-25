package controlplane

import "time"

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
func refOf(object map[string]any, kind string) ObjectRef {
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	return kernelRef(kind, namespace, name)
}
