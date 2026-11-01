package app

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/write"
)

// application builds one stored application the sweep can resolve by UID.
func application(namespace, name, uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"uid":       uid,
		},
	}}
}

// actionRecord builds one stored one-shot record of an action for an
// application.
func actionRecord(name, owner, target string) unstructured.Unstructured {
	object := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				ActionLabel:      ActionRetry,
				ActionOwnerLabel: owner,
			},
		},
	}}
	if target != "" {
		object.Object["data"] = map[string]any{ActionDataKey: target}
	}
	return object
}

// sweepStore serves the applications and records the sweep reads and records
// the deletions it performs.
type sweepStore struct {
	Store
	applications []*unstructured.Unstructured
	records      []unstructured.Unstructured
	failReads    bool
	deleted      []string
}

// List returns the fixed record set.
func (s *sweepStore) List(context.Context, schema.GroupVersionResource, string, string) ([]unstructured.Unstructured, error) {
	return s.records, nil
}

// Get resolves one application by its name; every other kind is absent.
func (s *sweepStore) Get(_ context.Context, ref write.Ref) (*unstructured.Unstructured, error) {
	if s.failReads {
		return nil, errors.New("the API server is unreachable")
	}
	if ref.Kind != "Application" {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: ref.Group, Resource: "applications"}, ref.Name)
	}
	for _, stored := range s.applications {
		if stored.GetNamespace() == ref.Namespace && stored.GetName() == ref.Name {
			return stored, nil
		}
	}
	return nil, apierrors.NewNotFound(schema.GroupResource{Group: ref.Group, Resource: "applications"}, ref.Name)
}

// Delete records the removed record.
func (s *sweepStore) Delete(_ context.Context, _ string, ref write.Ref) error {
	s.deleted = append(s.deleted, ref.Name)
	return nil
}

// TestSweepDeletesOnlyRecordsWhoseApplicationIsGone covers C-69: a record is
// consumable only by the application whose UID it carries, so the sweep deletes
// exactly the records that have no such application left.
func TestSweepDeletesOnlyRecordsWhoseApplicationIsGone(t *testing.T) {
	store := &sweepStore{
		applications: []*unstructured.Unstructured{
			application("default", "live", "uid-live"),
			application("default", "recreated", "uid-new"),
		},
		records: []unstructured.Unstructured{
			actionRecord("kos-action-uid-gone-retry", "uid-gone", "default/gone"),
			actionRecord("kos-action-uid-live-retry", "uid-live", "default/live"),
			actionRecord("kos-action-uid-old-retry", "uid-old", "default/recreated"),
			actionRecord("kos-action-uid-nobody-retry", "uid-nobody", ""),
		},
	}
	runtime := &Runtime{controller: NewController(store, Options{})}
	runtime.sweepActionRecords(context.Background())

	want := map[string]bool{
		"kos-action-uid-gone-retry":   true,
		"kos-action-uid-old-retry":    true,
		"kos-action-uid-nobody-retry": true,
	}
	if len(store.deleted) != len(want) {
		t.Fatalf("deleted %v, want %d records", store.deleted, len(want))
	}
	for _, name := range store.deleted {
		if !want[name] {
			t.Fatalf("the sweep deleted %q, which a live application still owns", name)
		}
	}
}

// TestSweepKeepsRecordsItCannotJudge covers the read path of C-69: a read that
// failed is not a fact about the application, so the sweep leaves the record to
// the next interval instead of deleting it.
func TestSweepKeepsRecordsItCannotJudge(t *testing.T) {
	store := &sweepStore{
		records:   []unstructured.Unstructured{actionRecord("kos-action-uid-1-retry", "uid-1", "default/web")},
		failReads: true,
	}
	runtime := &Runtime{controller: NewController(store, Options{})}
	runtime.sweepActionRecords(context.Background())

	if len(store.deleted) != 0 {
		t.Fatalf("the sweep deleted %v after a failed read", store.deleted)
	}
}

// TestRecordTargetReadsTheApplicationTheRecordNames covers the enqueue mapping
// of C-69: only a record naming an application asks for a round of it.
func TestRecordTargetReadsTheApplicationTheRecordNames(t *testing.T) {
	cases := []struct {
		name   string
		record unstructured.Unstructured
		want   string
		ok     bool
	}{
		{
			name:   "a record names its application",
			record: actionRecord("kos-action-uid-1-retry", "uid-1", "default/web"),
			want:   "default/web",
			ok:     true,
		},
		{
			name:   "a record without a target names no application",
			record: actionRecord("kos-action-uid-1-retry", "uid-1", ""),
		},
		{
			name:   "a target without a namespace names no application",
			record: actionRecord("kos-action-uid-1-retry", "uid-1", "web"),
		},
		{
			name:   "a target without a name names no application",
			record: actionRecord("kos-action-uid-1-retry", "uid-1", "default/"),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, ok := recordTarget(&test.record)
			if ok != test.ok {
				t.Fatalf("target resolved = %v, want %v", ok, test.ok)
			}
			if !ok {
				return
			}
			if got := request.Namespace + "/" + request.Name; got != test.want {
				t.Fatalf("target = %q, want %q", got, test.want)
			}
			if request.Kind != "Application" {
				t.Fatalf("kind = %q, want Application", request.Kind)
			}
		})
	}
}

// TestApplicationUpdateAsksForARoundOnlyOnStateChange covers the update
// predicate: a new desired state or the start of a deletion asks for a round,
// while a status write — including the controller's own — does not (C-06).
func TestApplicationUpdateAsksForARoundOnlyOnStateChange(t *testing.T) {
	stored := func(generation int64, deleted bool, phase string) *unstructured.Unstructured {
		object := application("default", "web", "uid-1")
		object.SetGeneration(generation)
		object.Object["status"] = map[string]any{"phase": phase}
		if deleted {
			object.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		}
		return object
	}
	cases := []struct {
		name     string
		previous *unstructured.Unstructured
		current  *unstructured.Unstructured
		want     bool
	}{
		{
			name:     "a new desired state asks for a round",
			previous: stored(1, false, PhaseReady),
			current:  stored(2, false, PhaseReady),
			want:     true,
		},
		{
			name:     "the start of a deletion asks for a round",
			previous: stored(1, false, PhaseReady),
			current:  stored(1, true, PhaseReady),
			want:     true,
		},
		{
			name:     "a status write asks for no round",
			previous: stored(1, false, PhaseInstalling),
			current:  stored(1, false, PhaseReady),
		},
		{
			name:     "an unchanged object asks for no round",
			previous: stored(1, false, PhaseReady),
			current:  stored(1, false, PhaseReady),
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := applicationChanged(test.previous, test.current); got != test.want {
				t.Fatalf("applicationChanged = %v, want %v", got, test.want)
			}
		})
	}
}
