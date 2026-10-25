package reconcile

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/client-go/util/workqueue"
)

// Options configures a Controller.
type Options struct {
	// Workers is the number of concurrent rounds. One object is never
	// processed by two workers at once: a request added while its round runs
	// is deferred until that round ends (C-05). Values below one mean one.
	Workers int
	// MaxRoundDuration is the fixed time budget of one round (C-06). It is
	// delivered as the round context's deadline; a reconciler that ignores the
	// context blocks its worker until it returns. Zero selects
	// DefaultMaxRoundDuration.
	MaxRoundDuration time.Duration
	// Registerer receives the round metrics. When nil the controller keeps
	// them on a private registry that Gatherer returns.
	Registerer prometheus.Registerer
}

// Controller drains the work queue with a fixed number of workers.
type Controller struct {
	reconciler Reconciler
	queue      workqueue.TypedRateLimitingInterface[Request]
	workers    int
	maxRound   time.Duration
	metrics    *metrics
	gatherer   prometheus.Gatherer
}

// NewController builds a controller around one reconciler.
func NewController(reconciler Reconciler, options Options) (*Controller, error) {
	if reconciler == nil {
		return nil, errors.New("reconcile: nil reconciler")
	}
	workers := options.Workers
	if workers < 1 {
		workers = 1
	}
	maxRound := options.MaxRoundDuration
	if maxRound <= 0 {
		maxRound = DefaultMaxRoundDuration
	}
	var gatherer prometheus.Gatherer
	registerer := options.Registerer
	if registerer == nil {
		registry := prometheus.NewRegistry()
		registerer = registry
		gatherer = registry
	}
	metrics, err := newMetrics(registerer)
	if err != nil {
		return nil, err
	}
	return &Controller{
		reconciler: reconciler,
		queue:      workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[Request]()),
		workers:    workers,
		maxRound:   maxRound,
		metrics:    metrics,
		gatherer:   gatherer,
	}, nil
}

// Enqueue asks for a round of one object. A request for an object already
// waiting collapses into the waiting one; an object whose round is running is
// queued again after that round ends (C-05).
func (c *Controller) Enqueue(request Request) {
	c.queue.Add(request)
}

// EnqueueAfter asks for a round of one object after delay.
func (c *Controller) EnqueueAfter(request Request, delay time.Duration) {
	c.queue.AddAfter(request, delay)
}

// Gatherer returns the registry holding the round metrics when the controller
// created it (Options.Registerer was nil); otherwise nil.
func (c *Controller) Gatherer() prometheus.Gatherer {
	return c.gatherer
}

// Run starts the workers and blocks until ctx is done. Shutting down stops
// taking new work and returns after the rounds in flight finish.
func (c *Controller) Run(ctx context.Context) {
	var workers sync.WaitGroup
	workers.Add(c.workers)
	for range c.workers {
		go func() {
			defer workers.Done()
			c.work(ctx)
		}()
	}
	<-ctx.Done()
	c.queue.ShutDown()
	workers.Wait()
}

func (c *Controller) work(ctx context.Context) {
	for {
		request, isShutdown := c.queue.Get()
		if isShutdown {
			return
		}
		c.dispatch(ctx, request)
	}
}

// dispatch runs one round with panic isolation and always releases the queue
// item, so a panic cannot leave the request in the processing set or take down
// the other objects' rounds. The panicking round is counted and retried.
func (c *Controller) dispatch(ctx context.Context, request Request) {
	defer c.queue.Done(request)
	defer func() {
		if recovered := recover(); recovered != nil {
			c.metrics.observePanic()
			slog.Error("reconcile round panicked", "request", request, "panic", recovered)
			c.queue.AddRateLimited(request)
		}
	}()
	c.round(ctx, request)
}

// round runs one bounded reconcile. Only the round's own deadline counts as
// over budget: a reconciler that times out its own API call returns
// DeadlineExceeded without spending the budget, so it stays a plain failure
// (C-06).
func (c *Controller) round(ctx context.Context, request Request) {
	roundCtx, cancel := context.WithTimeout(ctx, c.maxRound)
	defer cancel()
	start := time.Now()
	result, err := c.reconciler.Reconcile(roundCtx, request)
	elapsed := time.Since(start)
	isOverLimit := elapsed > c.maxRound || errors.Is(roundCtx.Err(), context.DeadlineExceeded)

	c.metrics.observeRound(elapsed, isOverLimit)

	switch {
	case err != nil:
		if ctx.Err() != nil {
			return
		}
		c.queue.AddRateLimited(request)
	case result.RequeueAfter > 0:
		c.queue.Forget(request)
		c.queue.AddAfter(request, result.RequeueAfter)
	default:
		c.queue.Forget(request)
	}
}
