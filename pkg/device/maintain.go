package device

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/write"
)

// Maintainer refreshes the validity markers of device facts and reclaims the
// fact of a node that has been gone for the declared window (C-37).
type Maintainer struct {
	// Writer executes the marker and delete writes.
	Writer *write.Writer
	// NodeExists reports whether a node object still exists. A nil function
	// disables reclamation because absence cannot be observed.
	NodeExists func(ctx context.Context, name string) (bool, error)
	// Now supplies the current time; nil means the wall clock.
	Now func() time.Time
	// Validator writes the validity markers; nil builds one from Writer.
	Validator *Validator

	mu      sync.Mutex
	missing map[string]time.Time
}

// NewMaintainer builds a maintainer over one cluster.
func NewMaintainer(writer *write.Writer, nodeExists func(ctx context.Context, name string) (bool, error)) *Maintainer {
	return &Maintainer{Writer: writer, NodeExists: nodeExists, missing: map[string]time.Time{}}
}

// Run performs one maintenance round: it marks stale facts, validates the node
// boundary and reclaims facts whose node has been gone long enough. A failure
// on one object does not stop the round (C-37: collection failure never blocks
// another function).
func (m *Maintainer) Run(ctx context.Context) error {
	if m.Writer == nil {
		return errors.New("device: a writer is required")
	}
	if m.Writer.Dynamic == nil {
		return errors.New("device: a dynamic client is required")
	}
	now := m.now()
	list, err := m.Writer.Dynamic.Resource(nodeDeviceResource).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list device facts: %w", err)
	}
	validator := m.validator()
	var problems []error
	for index := range list.Items {
		object := &list.Items[index]
		ref := write.RefOf(object)
		if _, err := validator.CheckBoundary(ctx, object); err != nil {
			problems = append(problems, err)
		}
		observed := observedAtOf(object)
		if now.Sub(observed) > StaleAfter {
			if err := validator.MarkStale(ctx, ref); err != nil {
				problems = append(problems, err)
			}
		} else if err := validator.ClearStale(ctx, ref); err != nil {
			problems = append(problems, err)
		}
		if m.NodeExists == nil {
			continue
		}
		exists, err := m.NodeExists(ctx, object.GetName())
		if err != nil {
			problems = append(problems, fmt.Errorf("observe node %q: %w", object.GetName(), err))
			continue
		}
		if exists {
			m.forget(object.GetName())
			continue
		}
		since := m.missingSince(object.GetName(), now)
		if now.Sub(since) < ReclaimAfter {
			continue
		}
		if err := m.Writer.Delete(ctx, write.SubjectDiscovery, write.Session{}, ref); err != nil && !apierrors.IsNotFound(err) {
			problems = append(problems, fmt.Errorf("reclaim device fact %q: %w", object.GetName(), err))
			continue
		}
		m.forget(object.GetName())
	}
	return errors.Join(problems...)
}

// missingSince returns when the node was first seen missing. The window starts
// at the round that observed the absence, not at the last collection: the node
// existed on an earlier round, and KOS can only prove it gone now. Starting
// from the last observation would begin the window before the disappearance and
// reclaim the fact early (C-37).
func (m *Maintainer) missingSince(name string, now time.Time) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.missing == nil {
		m.missing = map[string]time.Time{}
	}
	if since, seen := m.missing[name]; seen {
		return since
	}
	m.missing[name] = now
	return now
}

// forget drops the absence record of a node that exists again.
func (m *Maintainer) forget(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.missing, name)
}

// validator returns the validator this maintainer writes through.
func (m *Maintainer) validator() *Validator {
	if m.Validator != nil {
		return m.Validator
	}
	return &Validator{Writer: m.Writer}
}

// now returns the round's clock.
func (m *Maintainer) now() time.Time {
	if m.Now == nil {
		return time.Now().UTC()
	}
	return m.Now().UTC()
}

// observedAtOf reads the last observation time of a stored fact; the creation
// time stands in when the collector never stamped one.
func observedAtOf(object *unstructured.Unstructured) time.Time {
	text, found, err := unstructured.NestedString(object.Object, "spec", "observedAt")
	if err == nil && found && text != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, text); parseErr == nil {
			return parsed
		}
	}
	return object.GetCreationTimestamp().Time
}
