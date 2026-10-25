package reconcile

import (
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// metrics is the round observability of one controller: the duration of every
// round, the rounds that reached the time budget and the rounds that panicked
// (C-06).
type metrics struct {
	roundDuration prometheus.Histogram
	overLimit     prometheus.Counter
	panics        prometheus.Counter
}

func newMetrics(registerer prometheus.Registerer) (*metrics, error) {
	observability := &metrics{
		roundDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "kos",
			Subsystem: "reconcile",
			Name:      "round_duration_seconds",
			Help:      "Duration of one reconcile round.",
			Buckets:   prometheus.ExponentialBuckets(0.005, 2, 16),
		}),
		overLimit: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "kos",
			Subsystem: "reconcile",
			Name:      "round_over_limit_total",
			Help:      "Reconcile rounds that exceeded the per-round time budget.",
		}),
		panics: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: "kos",
			Subsystem: "reconcile",
			Name:      "round_panics_total",
			Help:      "Reconcile rounds that panicked and were recovered.",
		}),
	}
	if err := registerer.Register(observability.roundDuration); err != nil {
		return nil, fmt.Errorf("register reconcile round duration: %w", err)
	}
	if err := registerer.Register(observability.overLimit); err != nil {
		return nil, fmt.Errorf("register reconcile over-limit counter: %w", err)
	}
	if err := registerer.Register(observability.panics); err != nil {
		return nil, fmt.Errorf("register reconcile panic counter: %w", err)
	}
	return observability, nil
}

func (m *metrics) observeRound(elapsed time.Duration, isOverLimit bool) {
	m.roundDuration.Observe(elapsed.Seconds())
	if isOverLimit {
		m.overLimit.Inc()
	}
}

func (m *metrics) observePanic() {
	m.panics.Inc()
}
