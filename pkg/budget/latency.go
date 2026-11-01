package budget

import (
	"math"
	"sort"
	"sync"
	"time"
)

// defaultLatencySamples bounds one recorder. The window usually limits the
// sample count first; the cap keeps a burst from growing without bound, and a
// recorder that had to discard an in-window sample says so instead of passing
// the shorter span off as the declared window (C-57).
const defaultLatencySamples = 200_000

// Latency records the durations of one path and answers a quantile over the
// declared window (C-57). One recorder measures one path; the reconcile and the
// projection paths each own one.
type Latency struct {
	mu        sync.Mutex
	samples   []timedSample
	window    time.Duration
	limit     int
	truncated bool
	now       func() time.Time
}

// timedSample is one duration with the time it was recorded.
type timedSample struct {
	at       time.Time
	duration time.Duration
}

// NewLatency builds a recorder over one window. A non-positive window selects
// LatencyWindow and a non-positive limit selects the bounded default.
func NewLatency(window time.Duration, limit int) *Latency {
	if window <= 0 {
		window = LatencyWindow
	}
	if limit <= 0 {
		limit = defaultLatencySamples
	}
	return &Latency{window: window, limit: limit, now: time.Now}
}

// Observe records one duration. A nil recorder ignores the call, so a caller
// without a measurement keeps working.
func (l *Latency) Observe(duration time.Duration) {
	if l == nil || duration < 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.samples = append(l.samples, timedSample{at: now, duration: duration})
	l.prune(now)
}

// Quantile is the requested quantile of the durations inside the window, or
// zero when the window holds none. A quantile outside (0, 1] is treated as 1.
func (l *Latency) Quantile(q float64) time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	values := make([]time.Duration, 0, len(l.samples))
	cutoff := now.Add(-l.window)
	for _, item := range l.samples {
		if item.at.After(cutoff) {
			values = append(values, item.duration)
		}
	}
	if len(values) == 0 {
		return 0
	}
	if q <= 0 || q > 1 {
		q = 1
	}
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	index := int(math.Ceil(q*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

// Count is how many measurements the window holds.
func (l *Latency) Count() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	cutoff := now.Add(-l.window)
	count := 0
	for _, item := range l.samples {
		if item.at.After(cutoff) {
			count++
		}
	}
	return count
}

// Coverage is how much of the declared window the recorder actually holds: the
// span from the oldest retained sample to now, never more than the window and
// zero when the recorder is empty. A percentile is only a window figure while
// Coverage equals the window, which is what Truncated reports (C-57).
func (l *Latency) Coverage() time.Duration {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	if len(l.samples) == 0 {
		return 0
	}
	coverage := now.Sub(l.samples[0].at)
	if coverage > l.window {
		return l.window
	}
	if coverage < 0 {
		return 0
	}
	return coverage
}

// Truncated reports that the sample cap discarded measurements still inside the
// window, so the window is not fully covered. The flag stays set for the life of
// the recorder: every percentile it answers afterwards is taken over a shorter
// span than the window, and a measurement run that loses its window has to be
// rerun rather than judged (C-57).
func (l *Latency) Truncated() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(l.now())
	return l.truncated
}

// prune drops samples outside the window and, when the cap is reached, the
// oldest half so the recorder keeps recent measurements. Discarding a sample
// that is still inside the window is the truncation the recorder reports.
func (l *Latency) prune(now time.Time) {
	cutoff := now.Add(-l.window)
	first := 0
	for first < len(l.samples) && !l.samples[first].at.After(cutoff) {
		first++
	}
	if first > 0 {
		l.samples = append(l.samples[:0], l.samples[first:]...)
	}
	if len(l.samples) > l.limit {
		l.samples = append(l.samples[:0], l.samples[len(l.samples)/2:]...)
		l.truncated = true
	}
}
