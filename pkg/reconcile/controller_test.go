package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tsic404/kos/pkg/reconcile"
)

// roundFunc adapts a function to the Reconciler interface and reports every
// finished round, so a test can wait without polling.
type roundFunc struct {
	mu        sync.Mutex
	calls     int
	rounds    chan reconcile.Request
	reconcile func(ctx context.Context, request reconcile.Request, call int) (reconcile.Result, error)
}

func (f *roundFunc) Reconcile(ctx context.Context, request reconcile.Request) (result reconcile.Result, err error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	f.mu.Unlock()

	defer func() {
		if f.rounds != nil {
			f.rounds <- request
		}
	}()
	result, err = f.reconcile(ctx, request, call)
	return result, err
}

func (f *roundFunc) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// settleReconciler never asks for another round.
func settleReconciler(rounds chan reconcile.Request) *roundFunc {
	return &roundFunc{
		rounds: rounds,
		reconcile: func(context.Context, reconcile.Request, int) (reconcile.Result, error) {
			return reconcile.Result{}, nil
		},
	}
}

// start runs the controller until the test ends or the returned cancel is called.
func start(t *testing.T, controller *reconcile.Controller) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return cancel
}

func waitRound(t *testing.T, rounds <-chan reconcile.Request) reconcile.Request {
	t.Helper()
	select {
	case request := <-rounds:
		return request
	case <-time.After(10 * time.Second):
		t.Fatal("no round completed")
		return reconcile.Request{}
	}
}

func expectNoRound(t *testing.T, rounds <-chan reconcile.Request, within time.Duration) {
	t.Helper()
	select {
	case request := <-rounds:
		t.Fatalf("unexpected round for %+v", request)
	case <-time.After(within):
	}
}

func newController(t *testing.T, reconciler reconcile.Reconciler, options reconcile.Options) *reconcile.Controller {
	t.Helper()
	controller, err := reconcile.NewController(reconciler, options)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	return controller
}

func TestWaitingDuplicateBecomesOneRound(t *testing.T) {
	rounds := make(chan reconcile.Request, 4)
	controller := newController(t, settleReconciler(rounds), reconcile.Options{Workers: 1})
	request := reconcile.Request{Kind: "Application", Namespace: "default", Name: "web"}

	// Both adds land before the queue is drained, so the duplicate collapses.
	controller.Enqueue(request)
	controller.Enqueue(request)
	start(t, controller)

	if got := waitRound(t, rounds); got != request {
		t.Fatalf("round = %+v, want %+v", got, request)
	}
	expectNoRound(t, rounds, 100*time.Millisecond)
}

func TestAnErrorRoundIsRetried(t *testing.T) {
	rounds := make(chan reconcile.Request, 4)
	reconciler := &roundFunc{
		rounds: rounds,
		reconcile: func(_ context.Context, _ reconcile.Request, call int) (reconcile.Result, error) {
			if call == 1 {
				return reconcile.Result{}, errors.New("transient failure")
			}
			return reconcile.Result{}, nil
		},
	}
	controller := newController(t, reconciler, reconcile.Options{Workers: 1})
	start(t, controller)
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: "default", Name: "web"})

	waitRound(t, rounds)
	waitRound(t, rounds)
	if calls := reconciler.callCount(); calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	expectNoRound(t, rounds, 100*time.Millisecond)
}

func TestRequeueAfterSchedulesAnotherRound(t *testing.T) {
	rounds := make(chan reconcile.Request, 4)
	reconciler := &roundFunc{
		rounds: rounds,
		reconcile: func(_ context.Context, _ reconcile.Request, call int) (reconcile.Result, error) {
			if call == 1 {
				return reconcile.Result{RequeueAfter: 20 * time.Millisecond}, nil
			}
			return reconcile.Result{}, nil
		},
	}
	controller := newController(t, reconciler, reconcile.Options{Workers: 1})
	start(t, controller)
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: "default", Name: "web"})

	waitRound(t, rounds)
	waitRound(t, rounds)
	if calls := reconciler.callCount(); calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	expectNoRound(t, rounds, 100*time.Millisecond)
}

func TestAReconcilerDeadlineIsNotARoundOverrun(t *testing.T) {
	rounds := make(chan reconcile.Request, 4)
	registry := prometheus.NewRegistry()
	reconciler := &roundFunc{
		rounds: rounds,
		reconcile: func(_ context.Context, _ reconcile.Request, call int) (reconcile.Result, error) {
			if call == 1 {
				// A reconciler timing out its own API call returns a wrapped
				// DeadlineExceeded without spending the round budget.
				return reconcile.Result{}, fmt.Errorf("read object: %w", context.DeadlineExceeded)
			}
			return reconcile.Result{}, nil
		},
	}
	controller := newController(t, reconciler, reconcile.Options{
		Workers:          1,
		MaxRoundDuration: time.Second,
		Registerer:       registry,
	})
	start(t, controller)
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: "default", Name: "web"})

	waitRound(t, rounds)
	waitRound(t, rounds)
	if value := metricValue(t, registry, "kos_reconcile_round_over_limit_total"); value != 0 {
		t.Fatalf("over-limit counter = %v, want 0 for a reconciler-side deadline", value)
	}
}

func TestAPanicRoundIsIsolatedAndRetried(t *testing.T) {
	rounds := make(chan reconcile.Request, 4)
	registry := prometheus.NewRegistry()
	reconciler := &roundFunc{
		rounds: rounds,
		reconcile: func(_ context.Context, _ reconcile.Request, call int) (reconcile.Result, error) {
			if call == 1 {
				panic("application reconciler exploded")
			}
			return reconcile.Result{}, nil
		},
	}
	controller := newController(t, reconciler, reconcile.Options{Workers: 1, Registerer: registry})
	start(t, controller)
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: "default", Name: "web"})

	// The panicking round still signals through the deferred send; the worker
	// must recover, release the queue item and retry it.
	waitRound(t, rounds)
	waitRound(t, rounds)
	if calls := reconciler.callCount(); calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if value := metricValue(t, registry, "kos_reconcile_round_panics_total"); value != 1 {
		t.Fatalf("panic counter = %v, want 1", value)
	}
	expectNoRound(t, rounds, 100*time.Millisecond)
}

func TestRoundOverTheTimeBudgetIsObservable(t *testing.T) {
	const maxRound = 50 * time.Millisecond
	registry := prometheus.NewRegistry()
	capHit := make(chan time.Time, 1)
	reconciler := &roundFunc{
		reconcile: func(ctx context.Context, _ reconcile.Request, call int) (reconcile.Result, error) {
			if call > 1 {
				return reconcile.Result{}, nil
			}
			deadline, hasDeadline := ctx.Deadline()
			if !hasDeadline {
				return reconcile.Result{}, errors.New("round context has no deadline")
			}
			<-ctx.Done()
			capHit <- deadline
			return reconcile.Result{}, ctx.Err()
		},
	}
	controller := newController(t, reconciler, reconcile.Options{
		Workers:          1,
		MaxRoundDuration: maxRound,
		Registerer:       registry,
	})
	started := time.Now()
	start(t, controller)
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: "default", Name: "web"})

	select {
	case deadline := <-capHit:
		if budget := deadline.Sub(started); budget < maxRound || budget > 5*time.Second {
			t.Errorf("round deadline budget = %v, want about %v", budget, maxRound)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the slow reconciler never reached the deadline")
	}

	waitForMetric(t, registry, "kos_reconcile_round_over_limit_total", 1)
	if observations := metricValue(t, registry, "kos_reconcile_round_duration_seconds"); observations < 1 {
		t.Fatalf("round duration observations = %v, want at least 1", observations)
	}
}

func TestControllerKeepsItsOwnRegistry(t *testing.T) {
	controller := newController(t, settleReconciler(nil), reconcile.Options{})
	if controller.Gatherer() == nil {
		t.Fatal("controller without a registerer exposes no gatherer")
	}
	metricValue(t, controller.Gatherer(), "kos_reconcile_round_duration_seconds")
}

func TestSharedRegistererRejectsASecondController(t *testing.T) {
	registry := prometheus.NewRegistry()
	newController(t, settleReconciler(nil), reconcile.Options{Registerer: registry})
	if _, err := reconcile.NewController(settleReconciler(nil), reconcile.Options{Registerer: registry}); err == nil {
		t.Fatal("a second controller registered the same metric names")
	}
}

// metricValue returns a counter value or a histogram's sample count.
func metricValue(t *testing.T, gatherer prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if counter := metric.GetCounter(); counter != nil {
				return counter.GetValue()
			}
			if histogram := metric.GetHistogram(); histogram != nil {
				return float64(histogram.GetSampleCount())
			}
		}
	}
	t.Fatalf("metric %q is not gathered", name)
	return 0
}

// waitForMetric polls until a metric reaches want; the observation happens on
// the worker goroutine, so the test cannot simply read it once.
func waitForMetric(t *testing.T, gatherer prometheus.Gatherer, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if metricValue(t, gatherer, name) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("metric %q never reached %v", name, want)
}
