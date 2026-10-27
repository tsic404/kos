package conformance

import "context"

// Outcome sources: who produced a capability's result (C-10).
const (
	// SourceKOS marks a result from a run the kernel performed.
	SourceKOS = "KOS"
	// SourceSelfReported marks a result the provider declared about itself.
	SourceSelfReported = "SelfReported"
)

// Capability states (C-10).
const (
	// StateVerified means the kernel ran the distribution case set, every case
	// passed and the digest matched.
	StateVerified = "Verified"
	// StateDegraded means the kernel ran the case set and some case failed, so
	// the guarantee level does not hold.
	StateDegraded = "Degraded"
	// StateUnverified means no comparable run stands behind the capability.
	StateUnverified = "Unverified"
)

// Reasons an outcome is not verified. They are the machine-readable half of the
// display C-10 requires.
const (
	// ReasonVerified is the reason of a fully verified capability.
	ReasonVerified = "Verified"
	// ReasonCasesFailed is the reason of a capability with failed cases.
	ReasonCasesFailed = "CasesFailed"
	// ReasonCaseSetDigestMismatch marks a case set whose contents differ from
	// the distribution's; the result is not comparable.
	ReasonCaseSetDigestMismatch = "CaseSetDigestMismatch"
	// ReasonCaseSetUnknown marks a registration naming a set the distribution
	// does not ship.
	ReasonCaseSetUnknown = "CaseSetUnknown"
	// ReasonSelfReported marks a capability whose only result is the
	// provider's own declaration.
	ReasonSelfReported = "SelfReported"
	// ReasonNotRun marks a capability the kernel has not run any case set for.
	ReasonNotRun = "NotRun"
	// ReasonInterrupted marks a run the round budget stopped before it settled.
	ReasonInterrupted = "Interrupted"
)

// Registration is the capability entry a provider declares (C-09): the case
// set it names and the digest it pins.
type Registration struct {
	Capability    string
	Version       string
	CaseSet       string
	CaseSetDigest string
}

// SelfReport is the provider's own result for one capability (C-10).
type SelfReport struct {
	// FailedCases are the cases the provider reports as failed.
	FailedCases []string
}

// Outcome is what the kernel concluded about one capability (C-10).
type Outcome struct {
	// Source is who produced the result; empty when nothing ran.
	Source string
	// State is Verified, Degraded or Unverified.
	State string
	// Failed lists the case identifiers that did not pass.
	Failed []string
	// Reason names why the result is not verified.
	Reason string
}

// Target is one provider implementation a run exercises. The case set fixes
// what each case promises; the target answers by case identifier, so the
// kernel never branches on a provider name (C-10, F11).
type Target interface {
	// Run executes one case and returns nil when the promise holds.
	Run(ctx context.Context, capability, version, caseID string) error
}

// TargetFunc adapts a function to a Target.
type TargetFunc func(ctx context.Context, capability, version, caseID string) error

// Run implements Target.
func (f TargetFunc) Run(ctx context.Context, capability, version, caseID string) error {
	return f(ctx, capability, version, caseID)
}

// Targets resolves the implementation registered for one provider. Provider
// names are data here, never a kernel branch (F11).
type Targets interface {
	Target(provider string) (Target, bool)
}

// MapTargets is a fixed provider-to-implementation set.
type MapTargets map[string]Target

// Target implements Targets.
func (m MapTargets) Target(provider string) (Target, bool) {
	target, ok := m[provider]
	return target, ok
}

// Runner runs distribution case sets against provider implementations.
type Runner struct {
	distribution *Distribution
	targets      Targets
}

// NewRunner builds a runner over one distribution and one target set.
func NewRunner(distribution *Distribution, targets Targets) *Runner {
	return &Runner{distribution: distribution, targets: targets}
}

// Run executes the case set one registration names. A nil self-report means
// the provider declared nothing; a self-report is only consulted when no
// comparable run happened (C-10).
func (r *Runner) Run(ctx context.Context, provider string, registration Registration, selfReport *SelfReport) Outcome {
	if r == nil || r.distribution == nil {
		return selfReported(selfReport, ReasonNotRun)
	}
	set, known := r.distribution.CaseSet(registration.CaseSet)
	if !known || set.Capability != registration.Capability || set.Version != registration.Version {
		return selfReported(selfReport, ReasonCaseSetUnknown)
	}
	// A case set whose contents differ from the distribution's makes the
	// conclusion incomparable, so neither the run nor the self-report counts.
	if digest, found := r.distribution.Digest(registration.CaseSet); !found || digest != registration.CaseSetDigest {
		return Outcome{State: StateUnverified, Reason: ReasonCaseSetDigestMismatch}
	}
	target, hasTarget := targetOf(r.targets, provider)
	if !hasTarget {
		return selfReported(selfReport, ReasonNotRun)
	}
	failed := make([]string, 0, len(set.Cases))
	for _, entry := range set.Cases {
		if ctx.Err() != nil {
			return Outcome{State: StateUnverified, Reason: ReasonInterrupted}
		}
		if err := target.Run(ctx, set.Capability, set.Version, entry.ID); err != nil {
			failed = append(failed, entry.ID)
		}
	}
	if len(failed) > 0 {
		return Outcome{Source: SourceKOS, State: StateDegraded, Failed: failed, Reason: ReasonCasesFailed}
	}
	return Outcome{Source: SourceKOS, State: StateVerified, Reason: ReasonVerified}
}

// targetOf resolves a provider's implementation from a target set; a nil set
// registers none.
func targetOf(targets Targets, provider string) (Target, bool) {
	if targets == nil {
		return nil, false
	}
	target, ok := targets.Target(provider)
	return target, ok && target != nil
}

// selfReported renders a capability only a declaration stands behind (C-10).
func selfReported(report *SelfReport, absentReason string) Outcome {
	if report == nil {
		return Outcome{State: StateUnverified, Reason: absentReason}
	}
	return Outcome{
		Source: SourceSelfReported,
		State:  StateUnverified,
		Failed: append([]string(nil), report.FailedCases...),
		Reason: ReasonSelfReported,
	}
}
