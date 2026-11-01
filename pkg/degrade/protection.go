package degrade

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/tsic404/kos/pkg/budget"
)

// Bounds and factors of the degradation protection C-57 fixes.
const (
	// ThresholdFactor is the default degradation threshold as a multiple of
	// the steady control-plane memory budget.
	ThresholdFactor = 1.5
	// RecoveryFactor is the fraction of the threshold a measurement must fall
	// below before recovery may start.
	RecoveryFactor = 2.0 / 3.0
	// Window is the sample window the 95th percentile is taken over.
	Window = 5 * time.Minute
	// DegradeAfter is how long the 95th percentile must stay above the
	// threshold before real-time listening is turned off.
	DegradeAfter = 5 * time.Minute
	// RecoverAfter is how long the 95th percentile must stay below the
	// recovery fraction before real-time listening resumes.
	RecoverAfter = 10 * time.Minute
)

// defaultThreshold is the default threshold: the threshold factor applied to
// the steady memory budget of C-57. The two constants cannot drift apart.
func defaultThreshold() int64 {
	return int64(float64(budget.SteadyMemoryBytes) * ThresholdFactor)
}

// Reasons the protection records (C-57).
const (
	// ReasonMemoryExceeded says the memory percentile stayed above the
	// degradation threshold.
	ReasonMemoryExceeded = "控制面进程内存 P95 超过降级阈值"
	// ReasonNoReading is the reason recorded when no memory reader answered.
	ReasonNoReading = "降级保护无法读取进程内存"
	// ReasonStaleReading is the reason recorded when the newest measurement is
	// older than the sample window.
	ReasonStaleReading = "测量数据超过采样窗口，不再可测量"
)

// Listener is what the protection switches when it degrades: the native view's
// real-time listening (C-57).
type Listener interface {
	// Suspend turns real-time listening off and keeps the reason.
	Suspend(reason string)
	// Resume turns real-time listening back on.
	Resume()
}

// MemoryReader reads the control plane process's memory in bytes. The budget
// and the protection read the same measurement (C-57).
type MemoryReader interface {
	ReadMemory(ctx context.Context) (int64, error)
}

// ProtectionStatus is what the health endpoint and the diagnostic bundle
// report (C-56, C-57).
type ProtectionStatus struct {
	// IsMeasurable reports that the newest measurement is inside the window.
	IsMeasurable bool `json:"measurable"`
	// IsDegraded reports that real-time listening is off.
	IsDegraded bool `json:"degraded"`
	// Reason explains a degradation; empty when none is active.
	Reason string `json:"reason,omitempty"`
	// UnmeasurableReason explains why no judgement can be made; empty when
	// measurable.
	UnmeasurableReason string `json:"unmeasurableReason,omitempty"`
	// P95Bytes is the memory percentile of the sample window.
	P95Bytes int64 `json:"p95Bytes,omitempty"`
	// ThresholdBytes is the threshold in force.
	ThresholdBytes int64 `json:"thresholdBytes"`
	// MeasuredAt is the newest measurement.
	MeasuredAt time.Time `json:"measuredAt,omitempty"`
	// Since is when the current degradation began.
	Since time.Time `json:"since,omitempty"`
}

// ProtectionConfig wires the protection. Zero durations select the defaults of
// C-57; a non-positive threshold selects 1.5 times the steady memory budget.
type ProtectionConfig struct {
	// ThresholdBytes is the degradation threshold; it may be adjusted at
	// install time.
	ThresholdBytes int64
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
	// Listener is switched on degrade and recovery; nil records the state only.
	Listener Listener
	// IsExcluded reports the install and upgrade windows, which never enter
	// the judgement (C-57).
	IsExcluded func(time.Time) bool
	// Window is the percentile window.
	Window time.Duration
	// DegradeAfter is the sustain time above the threshold.
	DegradeAfter time.Duration
	// RecoverAfter is the sustain time below the recovery fraction.
	RecoverAfter time.Duration
}

// sample is one memory measurement.
type sample struct {
	at    time.Time
	bytes int64
}

// action is what a completed evaluation tells the listener to do, outside the
// lock so a listener may read the status back.
type action int

const (
	actionNone action = iota
	actionSuspend
	actionResume
)

// Protection implements the memory protection of C-57: one trigger and one
// recovery, with hysteresis between them, and no judgement while the
// measurement is unavailable.
type Protection struct {
	mu sync.Mutex

	threshold    int64
	window       time.Duration
	degradeAfter time.Duration
	recoverAfter time.Duration
	now          func() time.Time
	listener     Listener
	isExcluded   func(time.Time) bool

	samples    []sample
	isDegraded bool
	reason     string
	since      time.Time
	overSince  time.Time
	underSince time.Time

	isMeasurable       bool
	unmeasurableReason string
	measuredAt         time.Time
}

// NewProtection builds the protection with the defaults of C-57.
func NewProtection(config ProtectionConfig) *Protection {
	protection := &Protection{
		threshold:    config.ThresholdBytes,
		window:       config.Window,
		degradeAfter: config.DegradeAfter,
		recoverAfter: config.RecoverAfter,
		now:          config.Now,
		listener:     config.Listener,
		isExcluded:   config.IsExcluded,
	}
	if protection.threshold <= 0 {
		protection.threshold = defaultThreshold()
	}
	if protection.window <= 0 {
		protection.window = Window
	}
	if protection.degradeAfter <= 0 {
		protection.degradeAfter = DegradeAfter
	}
	if protection.recoverAfter <= 0 {
		protection.recoverAfter = RecoverAfter
	}
	if protection.now == nil {
		protection.now = time.Now
	}
	return protection
}

// Observe records one memory measurement and evaluates the state machine.
func (p *Protection) Observe(memoryBytes int64) {
	p.mu.Lock()
	action := p.observeLocked(memoryBytes, p.now())
	p.mu.Unlock()
	p.perform(action)
}

// Sample reads the process memory and records it. A reader that fails makes
// the protection unmeasurable: no degradation is triggered and the reason is
// kept for the health endpoint and the bundle (C-57).
func (p *Protection) Sample(ctx context.Context, reader MemoryReader) ProtectionStatus {
	if reader == nil {
		p.MarkUnmeasurable(ReasonNoReading)
		return p.Status()
	}
	memoryBytes, err := reader.ReadMemory(ctx)
	if err != nil {
		p.MarkUnmeasurable(fmt.Sprintf("%s: %v", ReasonNoReading, err))
		return p.Status()
	}
	p.Observe(memoryBytes)
	return p.Status()
}

// MarkUnmeasurable records a measurement failure. The state does not change: a
// degradation that cannot be measured is neither triggered nor recovered
// (C-57).
func (p *Protection) MarkUnmeasurable(reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.isMeasurable = false
	p.unmeasurableReason = reason
}

// SetThreshold changes the degradation threshold, which an installation may
// set (C-57). The hysteresis of an active degradation is unchanged.
func (p *Protection) SetThreshold(thresholdBytes int64) {
	if thresholdBytes <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.threshold = thresholdBytes
}

// Status reports the current protection state without changing it.
func (p *Protection) Status() ProtectionStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	status := ProtectionStatus{
		IsMeasurable:   p.isMeasurable,
		IsDegraded:     p.isDegraded,
		Reason:         p.reason,
		P95Bytes:       p.percentile(now),
		ThresholdBytes: p.threshold,
		MeasuredAt:     p.measuredAt,
		Since:          p.since,
	}
	if p.isMeasurable && now.Sub(p.measuredAt) > p.window {
		// A silent reader is not a zero measurement: the protection cannot
		// judge and says so instead of degrading or recovering (C-57).
		status.IsMeasurable = false
		status.UnmeasurableReason = ReasonStaleReading
		return status
	}
	status.UnmeasurableReason = p.unmeasurableReason
	if p.isMeasurable {
		status.UnmeasurableReason = ""
	}
	return status
}

// observeLocked records one sample and returns the listener action it caused.
func (p *Protection) observeLocked(memoryBytes int64, at time.Time) action {
	p.samples = append(p.samples, sample{at: at, bytes: memoryBytes})
	p.prune(at)
	p.isMeasurable = true
	p.unmeasurableReason = ""
	p.measuredAt = at
	if p.isExcluded != nil && p.isExcluded(at) {
		// The install and upgrade windows never enter the judgement, so a
		// spike inside one cannot trigger and a pending sustain restarts after
		// it (C-57). The sample is still taken: the window excludes the
		// judgement, not the measurement, and reporting a measured process as
		// unmeasurable would make the health endpoint lie.
		p.overSince = time.Time{}
		p.underSince = time.Time{}
		return actionNone
	}
	return p.evaluate(at, p.percentile(at))
}

// evaluate advances the hysteresis of C-57: above the threshold for the
// sustain time degrades once, below the recovery fraction for its own sustain
// time recovers once.
func (p *Protection) evaluate(at time.Time, p95 int64) action {
	if !p.isDegraded {
		if p95 <= p.threshold {
			p.overSince = time.Time{}
			return actionNone
		}
		if p.overSince.IsZero() {
			p.overSince = at
		}
		if at.Sub(p.overSince) < p.degradeAfter {
			return actionNone
		}
		p.isDegraded = true
		p.reason = ReasonMemoryExceeded
		p.since = at
		p.overSince = time.Time{}
		return actionSuspend
	}
	if p95 >= recoveryThreshold(p.threshold) {
		p.underSince = time.Time{}
		return actionNone
	}
	if p.underSince.IsZero() {
		p.underSince = at
	}
	if at.Sub(p.underSince) < p.recoverAfter {
		return actionNone
	}
	p.isDegraded = false
	p.reason = ""
	p.since = time.Time{}
	p.underSince = time.Time{}
	return actionResume
}

// recoveryThreshold is the fraction of the threshold a measurement must fall
// below; the comparison is strict, so exactly two thirds does not recover.
func recoveryThreshold(threshold int64) int64 {
	return int64(float64(threshold) * RecoveryFactor)
}

// prune drops samples older than the window.
func (p *Protection) prune(now time.Time) {
	cutoff := now.Add(-p.window)
	kept := p.samples[:0]
	for _, item := range p.samples {
		if item.at.After(cutoff) {
			kept = append(kept, item)
		}
	}
	p.samples = kept
}

// percentile is the 95th percentile of the samples inside the window, or zero
// when none is inside it.
func (p *Protection) percentile(now time.Time) int64 {
	cutoff := now.Add(-p.window)
	values := make([]int64, 0, len(p.samples))
	for _, item := range p.samples {
		if item.at.After(cutoff) {
			values = append(values, item.bytes)
		}
	}
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(left, right int) bool { return values[left] < values[right] })
	index := int(math.Ceil(0.95*float64(len(values)))) - 1
	if index < 0 {
		index = 0
	}
	return values[index]
}

// perform runs the listener call outside the lock.
func (p *Protection) perform(action action) {
	if p.listener == nil {
		return
	}
	switch action {
	case actionSuspend:
		p.mu.Lock()
		reason := p.reason
		p.mu.Unlock()
		p.listener.Suspend(reason)
	case actionResume:
		p.listener.Resume()
	}
}
