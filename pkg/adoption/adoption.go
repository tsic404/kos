// Package adoption implements the three attribution actions of C-02: adopting
// an existing object, releasing an adopted object and declining one.
//
// Adoption writes the attribution markers and an immutable snapshot of the spec
// the object had at that moment; release removes the markers and stops KOS
// writes; decline records that the object stays unmanaged, so it is no longer
// offered for adoption.
package adoption

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/serial"
	"github.com/tsic404/kos/pkg/write"
)

// Record labels and data keys of the snapshot object. The record is a
// ConfigMap in the KOS namespace: a record carries no desired state, so it does
// not need a kernel kind (C-02, C-35).
const (
	recordKey     = "kos.tsic.top/record"
	recordValue   = "adoption-snapshot"
	adoptedUIDKey = "kos.tsic.top/adopted-uid"
	dataKey       = "snapshot"
	namePrefix    = "adoption-snapshot-"
)

// Snapshot is the pre-adoption record of one object. It is written once and
// never modified.
type Snapshot struct {
	Object    write.Ref      `json:"object"`
	Spec      map[string]any `json:"spec"`
	Reason    string         `json:"reason"`
	AdoptedBy string         `json:"adoptedBy"`
	AdoptedAt time.Time      `json:"adoptedAt"`
}

// Actions performs the attribution actions against one cluster. Build it with
// New: the gate is not a zero value.
type Actions struct {
	// Writer executes the marker and record writes.
	Writer *write.Writer
	// Gate serializes the actions on one object (C-05).
	Gate *serial.Gate
	// Clock supplies adopted-at; nil means the wall clock.
	Clock func() time.Time
}

// New returns actions against one cluster.
func New(writer *write.Writer) *Actions {
	return &Actions{Writer: writer, Gate: serial.New(), Clock: func() time.Time { return time.Now().UTC() }}
}

// Adopt takes an unmanaged object over. The snapshot is written before the
// attribution markers, so an adopted object never lacks one; a failed marker
// write leaves the object unmanaged and the snapshot behind for the uninstall
// list (C-54).
func (a *Actions) Adopt(ctx context.Context, session write.Session, ref write.Ref, reason string) (*unstructured.Unstructured, error) {
	if err := checkReason(ref, reason); err != nil {
		return nil, err
	}
	var adopted *unstructured.Unstructured
	err := a.Gate.Do(ctx, ref.Key(), func(ctx context.Context) error {
		current, err := a.Writer.Get(ctx, ref)
		if err != nil {
			return err
		}
		attribution, err := markers.AttributionOf(current.GetLabels())
		if err != nil {
			return err
		}
		if attribution != markers.AttributionUnmanaged {
			return &write.Denied{Subject: write.SubjectUser, Object: ref, Reason: fmt.Sprintf("object is already %s", attribution)}
		}
		adoptedAt := a.now()
		if err := a.createSnapshot(ctx, session, current, reason, adoptedAt); err != nil {
			return err
		}
		adopted, err = a.Writer.MarkOn(ctx, write.SubjectUser, session, current, map[string]string{
			markers.AdoptedBy:      session.Username,
			markers.AdoptionReason: reason,
			markers.AdoptedAt:      adoptedAt.Format(time.RFC3339),
		}, []string{markers.Declined, markers.ManagedBy})
		return err
	})
	if err != nil {
		return nil, err
	}
	return adopted, nil
}

// Release gives an adopted object back to the user: the attribution markers go,
// the object keeps its current state, and KOS writes to it are refused from
// then on (C-02).
func (a *Actions) Release(ctx context.Context, session write.Session, ref write.Ref) (*unstructured.Unstructured, error) {
	var released *unstructured.Unstructured
	err := a.Gate.Do(ctx, ref.Key(), func(ctx context.Context) error {
		current, err := a.Writer.Get(ctx, ref)
		if err != nil {
			return err
		}
		attribution, err := markers.AttributionOf(current.GetLabels())
		if err != nil {
			return err
		}
		if attribution != markers.AttributionAdopted {
			return &write.Denied{Subject: write.SubjectUser, Object: ref, Reason: fmt.Sprintf("object is %s, not adopted", attribution)}
		}
		released, err = a.Writer.MarkOn(ctx, write.SubjectUser, session, current, nil, []string{
			markers.AdoptedBy, markers.AdoptionReason, markers.AdoptedAt,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return released, nil
}

// Decline records that the user keeps an object unmanaged; it stays in the
// unmanaged state and is no longer offered for adoption.
func (a *Actions) Decline(ctx context.Context, session write.Session, ref write.Ref) (*unstructured.Unstructured, error) {
	var declined *unstructured.Unstructured
	err := a.Gate.Do(ctx, ref.Key(), func(ctx context.Context) error {
		current, err := a.Writer.Get(ctx, ref)
		if err != nil {
			return err
		}
		attribution, err := markers.AttributionOf(current.GetLabels())
		if err != nil {
			return err
		}
		if attribution != markers.AttributionUnmanaged {
			return &write.Denied{Subject: write.SubjectUser, Object: ref, Reason: fmt.Sprintf("object is already %s", attribution)}
		}
		declined, err = a.Writer.MarkOn(ctx, write.SubjectUser, session, current,
			map[string]string{markers.Declined: "true"}, nil)
		return err
	})
	if err != nil {
		return nil, err
	}
	return declined, nil
}

// SnapshotOf returns the pre-adoption snapshot of an object, newest first, so a
// re-adoption after a release reads the adoption that is in force.
func (a *Actions) SnapshotOf(ctx context.Context, ref write.Ref) (*Snapshot, error) {
	object, err := a.Writer.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	resource := a.Writer.Dynamic.Resource(schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "configmaps",
	}).Namespace(api.SystemNamespace)
	list, err := resource.List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", adoptedUIDKey, object.GetUID()),
	})
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "adoption-snapshot"}, ref.Name)
	}
	sort.Slice(list.Items, func(i, j int) bool {
		left, right := list.Items[i], list.Items[j]
		leftTime, rightTime := left.GetCreationTimestamp(), right.GetCreationTimestamp()
		if !leftTime.Equal(&rightTime) {
			return rightTime.Before(&leftTime)
		}
		return left.GetName() > right.GetName()
	})
	record := list.Items[0]
	raw, found, err := unstructured.NestedString(record.Object, "data", dataKey)
	if err != nil || !found {
		return nil, fmt.Errorf("snapshot record %s carries no %s", record.GetName(), dataKey)
	}
	snapshot := &Snapshot{}
	if err := json.Unmarshal([]byte(raw), snapshot); err != nil {
		return nil, fmt.Errorf("decode snapshot record %s: %w", record.GetName(), err)
	}
	return snapshot, nil
}

// Adoptable reports whether an object is offered for adoption: unmanaged and
// not declined (C-02).
func Adoptable(labels map[string]string) (bool, error) {
	attribution, err := markers.AttributionOf(labels)
	if err != nil {
		return false, err
	}
	if attribution != markers.AttributionUnmanaged {
		return false, nil
	}
	_, declined := labels[markers.Declined]
	return !declined, nil
}

// checkReason refuses a reason the adoption-reason label cannot carry, so the
// failure names the action instead of failing on the object update.
func checkReason(ref write.Ref, reason string) error {
	if reason == "" {
		return &write.Denied{Subject: write.SubjectUser, Object: ref, Reason: "adoption needs a reason"}
	}
	if problems := validation.IsValidLabelValue(reason); len(problems) > 0 {
		return &write.Denied{
			Subject: write.SubjectUser,
			Object:  ref,
			Reason:  fmt.Sprintf("%q cannot be carried by the %q label: %s", reason, markers.AdoptionReason, problems[0]),
		}
	}
	return nil
}

// createSnapshot writes the immutable record of the object's current spec.
func (a *Actions) createSnapshot(ctx context.Context, session write.Session, object *unstructured.Unstructured, reason string, adoptedAt time.Time) error {
	ref := write.RefOf(object)
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return fmt.Errorf("read spec of %s: %w", ref, err)
	}
	if !found {
		spec = map[string]any{}
	}
	snapshot := Snapshot{
		Object:    ref,
		Spec:      spec,
		Reason:    reason,
		AdoptedBy: session.Username,
		AdoptedAt: adoptedAt,
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode snapshot of %s: %w", ref, err)
	}
	record := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      snapshotName(object.GetUID(), adoptedAt),
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				recordKey:     recordValue,
				adoptedUIDKey: string(object.GetUID()),
			},
		},
		"data":      map[string]any{dataKey: string(encoded)},
		"immutable": true,
	}}
	_, err = a.Writer.Create(ctx, write.SubjectAPIProcess, session, record)
	return err
}

// snapshotName keeps one record per adoption: the UID names the object and the
// timestamp keeps a later adoption from colliding with an earlier record.
func snapshotName(uid types.UID, adoptedAt time.Time) string {
	return fmt.Sprintf("%s%s-%d", namePrefix, uid, adoptedAt.UnixNano())
}

// now returns the action time.
func (a *Actions) now() time.Time {
	if a.Clock == nil {
		return time.Now().UTC()
	}
	return a.Clock().UTC()
}
