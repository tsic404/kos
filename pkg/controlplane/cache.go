package controlplane

import (
	"context"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
)

// cachedGetEntries bounds the single-object cache: a list is bounded by the
// cluster's own object count, while reads name objects one by one.
const cachedGetEntries = 4096

// listKey identifies one cached list.
type listKey struct {
	group     string
	kind      string
	namespace string
}

// getKey identifies one cached object.
type getKey struct {
	group     string
	kind      string
	namespace string
	name      string
}

// CachedSource keeps the last successful read of every class and serves it when
// the live source fails (C-56). A cached read keeps the observation time of the
// read that filled it, so the projection carries the C-13 stale marker instead
// of pretending the data is fresh. With no cached value the live error stands.
type CachedSource struct {
	live ObjectSource

	mu      sync.RWMutex
	lists   map[listKey][]ObservedObject
	objects map[getKey]ObservedObject
}

// NewCachedSource wraps a live source with the last-known cache.
func NewCachedSource(live ObjectSource) *CachedSource {
	return &CachedSource{
		live:    live,
		lists:   map[listKey][]ObservedObject{},
		objects: map[getKey]ObservedObject{},
	}
}

// List implements ObjectSource.
func (c *CachedSource) List(ctx context.Context, objectType ObjectType, namespace string) ([]ObservedObject, error) {
	observed, err := c.live.List(ctx, objectType, namespace)
	if err != nil {
		key := listKey{group: objectType.Group, kind: objectType.Kind, namespace: namespace}
		c.mu.RLock()
		cached, found := c.lists[key]
		c.mu.RUnlock()
		if !found {
			return nil, err
		}
		return copyObserved(cached), nil
	}
	key := listKey{group: objectType.Group, kind: objectType.Kind, namespace: namespace}
	c.mu.Lock()
	// The source hands out a fresh value per read and every projection path
	// treats it as read-only, so the cache keeps it rather than paying a deep
	// copy on the hot path. Only a cache hit is copied, because it outlives the
	// read that filled it.
	c.lists[key] = observed
	c.mu.Unlock()
	return observed, nil
}

// Get implements ObjectSource.
func (c *CachedSource) Get(ctx context.Context, objectType ObjectType, namespace, name string) (ObservedObject, error) {
	observed, err := c.live.Get(ctx, objectType, namespace, name)
	key := getKey{group: objectType.Group, kind: objectType.Kind, namespace: namespace, name: name}
	if err != nil {
		c.mu.RLock()
		cached, found := c.objects[key]
		c.mu.RUnlock()
		if !found {
			return ObservedObject{}, err
		}
		return copyObservedObject(cached), nil
	}
	c.mu.Lock()
	c.storeObject(key, observed)
	c.mu.Unlock()
	return observed, nil
}

// storeObject caches one object, dropping the oldest entry when the cache is
// full. The caller holds the write lock.
func (c *CachedSource) storeObject(key getKey, observed ObservedObject) {
	if len(c.objects) >= cachedGetEntries {
		var oldestKey getKey
		var oldestAt time.Time
		for candidate, entry := range c.objects {
			if oldestAt.IsZero() || entry.ObservedAt.Before(oldestAt) {
				oldestKey, oldestAt = candidate, entry.ObservedAt
			}
		}
		delete(c.objects, oldestKey)
	}
	c.objects[key] = observed
}

// copyObserved copies one list so a caller cannot mutate the cache.
func copyObserved(observed []ObservedObject) []ObservedObject {
	copied := make([]ObservedObject, 0, len(observed))
	for _, item := range observed {
		copied = append(copied, copyObservedObject(item))
	}
	return copied
}

// copyObservedObject copies one object so a caller cannot mutate the cache.
func copyObservedObject(observed ObservedObject) ObservedObject {
	return ObservedObject{Object: runtime.DeepCopyJSON(observed.Object), ObservedAt: observed.ObservedAt}
}
