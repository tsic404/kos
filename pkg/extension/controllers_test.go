package extension

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
)

// functionReconciler adapts a function to the reconcile interface.
type functionReconciler func(context.Context, reconcile.Request) (reconcile.Result, error)

// Reconcile runs the function.
func (f functionReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	return f(ctx, request)
}

// waitFor polls a condition with a deadline.
func waitFor(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

// startControllerSet runs a set and stops it when the test ends.
func startControllerSet(t *testing.T, set *ControllerSet) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		set.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestControllerFailureTurnsOnlyItsObjectsUnknown checks C-20: a controller that
// keeps failing makes only the classes it manages unobservable, and another
// controller keeps reconciling.
func TestControllerFailureTurnsOnlyItsObjectsUnknown(t *testing.T) {
	set := NewControllerSet(2)
	failing := functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, errors.New("boom")
	})
	var healthyRounds atomic.Int64
	healthy := functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		healthyRounds.Add(1)
		return reconcile.Result{}, nil
	})
	if err := set.Register("demo.failing", Controller{
		ID: "failing", Manages: []GroupKind{{Group: "a.example.test", Kind: "Widget"}},
	}, failing); err != nil {
		t.Fatalf("register failing controller: %v", err)
	}
	if err := set.Register("demo.healthy", Controller{
		ID: "healthy", Manages: []GroupKind{{Group: "b.example.test", Kind: "Thing"}},
	}, healthy); err != nil {
		t.Fatalf("register healthy controller: %v", err)
	}
	startControllerSet(t, set)

	if err := set.Enqueue("demo.failing", "failing", reconcile.Request{Kind: "Widget", Namespace: "default", Name: "one"}); err != nil {
		t.Fatalf("enqueue failing controller: %v", err)
	}
	waitFor(t, "the failing controller to become unavailable", func() bool {
		observed, ok := set.State("demo.failing", "failing")
		return ok && !observed.IsAvailable
	})

	stored := v1beta1.ObjectStatus{
		Phase:  "Running",
		Health: v1beta1.Health{State: v1beta1.HealthStateHealthy},
		Conditions: []metav1.Condition{
			state.Applied(1, "healthy"),
		},
	}
	projected := set.ProjectedStatus("a.example.test", "Widget", stored)
	if projected.Health.State != v1beta1.HealthStateUnknown {
		t.Fatalf("managed object health = %s, want Unknown", projected.Health.State)
	}
	if condition, ok := state.Find(projected.Conditions, api.ConditionReconciled); !ok || condition.Status != metav1.ConditionUnknown {
		t.Fatalf("managed object Reconciled = %+v, want Unknown", condition)
	}

	untouched := set.ProjectedStatus("b.example.test", "Thing", stored)
	if untouched.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("another extension's object = %s, want its stored health", untouched.Health.State)
	}
	kernelStored := stored
	projectedKernel := set.ProjectedStatus(api.Group, "Application", kernelStored)
	if projectedKernel.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("a kernel object = %s, want its stored health", projectedKernel.Health.State)
	}

	// The other controller keeps working while its peer is unavailable.
	before := healthyRounds.Load()
	if err := set.Enqueue("demo.healthy", "healthy", reconcile.Request{Kind: "Thing", Namespace: "default", Name: "one"}); err != nil {
		t.Fatalf("enqueue healthy controller: %v", err)
	}
	waitFor(t, "the healthy controller to keep reconciling", func() bool {
		return healthyRounds.Load() > before
	})
}

// TestControllerRecoversAfterASuccessfulRound checks that an unavailable
// controller becomes observable again once a round succeeds.
func TestControllerRecoversAfterASuccessfulRound(t *testing.T) {
	set := NewControllerSet(1)
	var isBroken atomic.Bool
	isBroken.Store(true)
	flapping := functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		if isBroken.Load() {
			return reconcile.Result{}, errors.New("boom")
		}
		return reconcile.Result{}, nil
	})
	if err := set.Register("demo.flapping", Controller{
		ID: "flapping", Manages: []GroupKind{{Group: "a.example.test", Kind: "Widget"}},
	}, flapping); err != nil {
		t.Fatalf("register: %v", err)
	}
	startControllerSet(t, set)

	request := reconcile.Request{Kind: "Widget", Namespace: "default", Name: "one"}
	if err := set.Enqueue("demo.flapping", "flapping", request); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	waitFor(t, "the controller to become unavailable", func() bool {
		observed, _ := set.State("demo.flapping", "flapping")
		return !observed.IsAvailable
	})

	isBroken.Store(false)
	if err := set.Enqueue("demo.flapping", "flapping", request); err != nil {
		t.Fatalf("enqueue after recovery: %v", err)
	}
	waitFor(t, "the controller to recover", func() bool {
		observed, _ := set.State("demo.flapping", "flapping")
		return observed.IsAvailable
	})
}

// TestControllerSetRefusesTwoControllersForOneClass checks C-20's single writer:
// two controllers cannot claim one object class.
func TestControllerSetRefusesTwoControllersForOneClass(t *testing.T) {
	set := NewControllerSet(1)
	noop := functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, nil
	})
	contribution := Controller{ID: "first", Manages: []GroupKind{{Group: "a.example.test", Kind: "Widget"}}}
	if err := set.Register("demo.one", contribution, noop); err != nil {
		t.Fatalf("register first controller: %v", err)
	}
	second := Controller{ID: "second", Manages: []GroupKind{{Group: "a.example.test", Kind: "Widget"}}}
	if err := set.Register("demo.two", second, noop); err == nil {
		t.Fatal("a second controller for one class was registered")
	}
}
