// Package tc27 verifies that every read projection carries the time it was
// observed and that an observation older than the shared freshness bound is
// marked stale (AC-14, TC-27).
package tc27

import (
	"context"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// agedSource reports every observation as if it had been taken age ago, which is
// how a cached source looks once its cache stops refreshing (C-13).
type agedSource struct {
	base controlplane.ObjectSource
	age  time.Duration
}

// List implements ObjectSource.
func (s agedSource) List(ctx context.Context, object controlplane.ObjectType, namespace string) ([]controlplane.ObservedObject, error) {
	objects, err := s.base.List(ctx, object, namespace)
	for index := range objects {
		objects[index].ObservedAt = time.Now().Add(-s.age)
	}
	return objects, err
}

// Get implements ObjectSource.
func (s agedSource) Get(ctx context.Context, class controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	object, err := s.base.Get(ctx, class, namespace, name)
	if err != nil {
		return object, err
	}
	object.ObservedAt = time.Now().Add(-s.age)
	return object, nil
}

// TestTC27StaleMarkerFollowsTheObservationTime reads a fresh projection, then a
// projection observed before and after the two minute bound.
func TestTC27StaleMarkerFollowsTheObservationTime(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest(
		"Application", harness.Namespace, "web", map[string]string{"managed-by": "kos-shell"}, nil)})

	path := "/api/v1/resources/Application/web?namespace=" + harness.Namespace
	collection := "/api/v1/resources/Application?namespace=" + harness.Namespace

	fresh := cluster.StartControlPlane(t, nil)
	status, body := fresh.Call(t, http.MethodGet, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read a fresh projection: status %d, body %v", status, body)
	}
	if observedAt, ok := body["observedAt"].(string); !ok || observedAt == "" {
		t.Fatalf("a projection must carry its observation time, got %v", body)
	}
	if stale, ok := body["stale"].(bool); !ok || stale {
		t.Fatalf("a live read must not be stale, got %v", body)
	}

	// Just inside the bound: the marker must stay off.
	inside := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Source = agedSource{base: controlplane.NewClusterSource(cluster.Dynamic), age: 90 * time.Second}
	})
	if status, body := inside.Call(t, http.MethodGet, path, nil, nil); status != http.StatusOK {
		t.Fatalf("read an inside-bound projection: status %d, body %v", status, body)
	} else if stale, _ := body["stale"].(bool); stale {
		t.Fatalf("an observation inside the bound must not be stale: %v", body)
	}

	// Beyond the bound: the object read and the collection read must both mark it.
	beyond := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Source = agedSource{base: controlplane.NewClusterSource(cluster.Dynamic), age: 3 * time.Minute}
	})
	status, body = beyond.Call(t, http.MethodGet, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read a stale projection: status %d, body %v", status, body)
	}
	if stale, _ := body["stale"].(bool); !stale {
		t.Fatalf("an observation three minutes old must be stale: %v", body)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, harness.String(body, "observedAt"))
	if err != nil {
		t.Fatalf("observedAt is not a timestamp: %v", body)
	}
	if age := time.Since(observedAt); age < 3*time.Minute || age > 4*time.Minute {
		t.Fatalf("observedAt is %s old, want about three minutes", age)
	}

	status, body = beyond.Call(t, http.MethodGet, collection, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("list stale projections: status %d, body %v", status, body)
	}
	items := harness.Items(t, body)
	if len(items) != 1 {
		t.Fatalf("listed %d items, want 1", len(items))
	}
	if stale, _ := items[0]["stale"].(bool); !stale {
		t.Fatalf("a listed projection three minutes old must be stale: %v", items[0])
	}
	if _, ok := items[0]["observedAt"].(string); !ok {
		t.Fatalf("a listed projection must carry its observation time: %v", items[0])
	}
}
