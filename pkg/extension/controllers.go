package extension

import (
	"context"
	"errors"
	"fmt"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
)

// DefaultFailureThreshold is how many consecutive failed rounds make a
// controller unavailable (C-20).
const DefaultFailureThreshold = 3

// ControllerState is the observed availability of one extension controller.
type ControllerState struct {
	Component           string
	Controller          string
	IsAvailable         bool
	ConsecutiveFailures int
	LastError           string
}

// ControllerSet runs one reconcile loop per declared controller (C-06, C-20).
// Each loop has its own health: a controller that keeps failing never stops
// another one, and only the classes it manages become unobservable.
type ControllerSet struct {
	threshold int
	mu        sync.Mutex
	entries   map[controllerKey]*controllerEntry
	serving   map[schema.GroupKind]controllerKey
}

// controllerKey identifies one controller contribution.
type controllerKey struct {
	component  string
	controller string
}

// controllerEntry is one registered loop and its health.
type controllerEntry struct {
	controller *reconcile.Controller
	state      ControllerState
}

// NewControllerSet builds a set of controller loops; a threshold below one
// selects DefaultFailureThreshold.
func NewControllerSet(threshold int) *ControllerSet {
	if threshold < 1 {
		threshold = DefaultFailureThreshold
	}
	return &ControllerSet{
		threshold: threshold,
		entries:   map[controllerKey]*controllerEntry{},
		serving:   map[schema.GroupKind]controllerKey{},
	}
}

// Register adds one controller contribution. A class already served by another
// controller is refused: one object has one extension controller (C-20).
func (s *ControllerSet) Register(component string, contribution Controller, reconciler reconcile.Reconciler) error {
	if reconciler == nil {
		return errors.New("controller has no reconciler")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := controllerKey{component: component, controller: contribution.ID}
	if _, exists := s.entries[key]; exists {
		return fmt.Errorf("controller %s/%s is already registered", component, contribution.ID)
	}
	for _, class := range contribution.Manages {
		if owner, taken := s.serving[class.Schema()]; taken {
			return fmt.Errorf("class %s already has controller %s/%s", class.String(), owner.component, owner.controller)
		}
	}
	controller, err := reconcile.NewController(
		recordingReconciler{set: s, key: key, inner: reconciler},
		reconcile.Options{},
	)
	if err != nil {
		return err
	}
	s.entries[key] = &controllerEntry{
		controller: controller,
		state: ControllerState{
			Component:   component,
			Controller:  contribution.ID,
			IsAvailable: true,
		},
	}
	for _, class := range contribution.Manages {
		s.serving[class.Schema()] = key
	}
	return nil
}

// Enqueue asks for a round of one object on one controller.
func (s *ControllerSet) Enqueue(component, id string, request reconcile.Request) error {
	s.mu.Lock()
	entry, ok := s.entries[controllerKey{component: component, controller: id}]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no controller %s/%s", component, id)
	}
	entry.controller.Enqueue(request)
	return nil
}

// Run starts every loop and blocks until ctx is done.
func (s *ControllerSet) Run(ctx context.Context) {
	s.mu.Lock()
	controllers := make([]*reconcile.Controller, 0, len(s.entries))
	for _, entry := range s.entries {
		controllers = append(controllers, entry.controller)
	}
	s.mu.Unlock()

	var running sync.WaitGroup
	running.Add(len(controllers))
	for _, controller := range controllers {
		go func() {
			defer running.Done()
			controller.Run(ctx)
		}()
	}
	<-ctx.Done()
	running.Wait()
}

// State returns one controller's state.
func (s *ControllerSet) State(component, id string) (ControllerState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[controllerKey{component: component, controller: id}]
	if !ok {
		return ControllerState{}, false
	}
	return entry.state, true
}

// ProjectedStatus is what a reader sees of one object: when its controller is
// unavailable the object is Unknown, and every other class keeps its status
// (C-08, C-20).
func (s *ControllerSet) ProjectedStatus(group, kind string, stored v1beta1.ObjectStatus) v1beta1.ObjectStatus {
	s.mu.Lock()
	key, served := s.serving[schema.GroupKind{Group: group, Kind: kind}]
	entry, known := s.entries[key]
	available, controller := true, ""
	if served && known {
		available, controller = entry.state.IsAvailable, entry.state.Controller
	}
	s.mu.Unlock()

	if available {
		return stored
	}
	projected := stored
	projected.Health.State = v1beta1.HealthStateUnknown
	projected.Conditions = append([]metav1.Condition(nil), stored.Conditions...)
	observed := state.ReconciledOf(stored.Conditions)
	projected.Conditions = state.Set(projected.Conditions,
		state.Unobserved(observed.ObservedGeneration, "扩展控制器 "+controller+" 不可用"))
	return projected
}

// record applies one round's outcome to the controller's health. A successful
// round makes an unavailable controller available again.
func (s *ControllerSet) record(key controllerKey, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return
	}
	if err == nil {
		entry.state.IsAvailable = true
		entry.state.ConsecutiveFailures = 0
		entry.state.LastError = ""
		return
	}
	entry.state.ConsecutiveFailures++
	entry.state.LastError = err.Error()
	if entry.state.ConsecutiveFailures >= s.threshold {
		entry.state.IsAvailable = false
	}
}

// recordingReconciler reports each round's outcome to the set before returning
// it to the loop.
type recordingReconciler struct {
	set   *ControllerSet
	key   controllerKey
	inner reconcile.Reconciler
}

// Reconcile runs the round and records its outcome.
func (r recordingReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	result, err := r.inner.Reconcile(ctx, request)
	r.set.record(r.key, err)
	return result, err
}
