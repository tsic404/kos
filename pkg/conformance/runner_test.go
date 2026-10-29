package conformance_test

import (
	"context"
	"testing"

	"github.com/tsic404/kos/pkg/conformance"
)

// registered builds the registration of one standard capability, with the
// distribution's digest unless the caller overrides it.
func registered(t *testing.T, distribution *conformance.Distribution, capability, version, caseSet string) conformance.Registration {
	t.Helper()
	digest, found := distribution.Digest(caseSet)
	if !found {
		t.Fatalf("the distribution ships no case set %q", caseSet)
	}
	return conformance.Registration{Capability: capability, Version: version, CaseSet: caseSet, CaseSetDigest: digest}
}

// TestRunVerifiesOnlyTheDistributionsCaseSet covers C-10's trust rules.
func TestRunVerifiesOnlyTheDistributionsCaseSet(t *testing.T) {
	distribution := conformance.Standard()
	registration := registered(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1")

	t.Run("a passing run is verified by KOS", func(t *testing.T) {
		runner := conformance.NewRunner(distribution, conformance.MapTargets{"builtin": conformance.TargetFunc(pass)})
		outcome := runner.Run(context.Background(), "builtin", registration, nil)
		if outcome.State != conformance.StateVerified || outcome.Source != conformance.SourceKOS {
			t.Fatalf("outcome = %+v, want a KOS verification", outcome)
		}
		if outcome.Reason != conformance.ReasonVerified {
			t.Fatalf("reason = %q, want %q", outcome.Reason, conformance.ReasonVerified)
		}
	})

	t.Run("a partial run is degraded and names the failed cases", func(t *testing.T) {
		failing := conformance.TargetFunc(func(_ context.Context, _, _, caseID string) error {
			if caseID == "volume.expand" {
				return conformanceError{}
			}
			return nil
		})
		runner := conformance.NewRunner(distribution, conformance.MapTargets{"builtin": failing})
		outcome := runner.Run(context.Background(), "builtin", registration, nil)
		if outcome.State != conformance.StateDegraded || outcome.Source != conformance.SourceKOS {
			t.Fatalf("outcome = %+v, want a degraded KOS run", outcome)
		}
		if len(outcome.Failed) != 1 || outcome.Failed[0] != "volume.expand" {
			t.Fatalf("failed cases = %v, want [volume.expand]", outcome.Failed)
		}
	})

	t.Run("a digest that differs from the distribution verifies nothing", func(t *testing.T) {
		tampered := registration
		tampered.CaseSetDigest = "not-the-distribution-digest"
		runner := conformance.NewRunner(distribution, conformance.MapTargets{"builtin": conformance.TargetFunc(pass)})
		outcome := runner.Run(context.Background(), "builtin", tampered, &conformance.SelfReport{})
		if outcome.State != conformance.StateUnverified || outcome.Reason != conformance.ReasonCaseSetDigestMismatch {
			t.Fatalf("outcome = %+v, want an incomparable case set", outcome)
		}
		if outcome.Source != "" {
			t.Fatalf("source = %q, want no source: neither run nor report counts", outcome.Source)
		}
	})

	t.Run("a case set the distribution does not ship is unverified", func(t *testing.T) {
		unknown := registration
		unknown.CaseSet = "acme/conformance/own-set"
		runner := conformance.NewRunner(distribution, conformance.MapTargets{"builtin": conformance.TargetFunc(pass)})
		outcome := runner.Run(context.Background(), "builtin", unknown, nil)
		if outcome.State != conformance.StateUnverified || outcome.Reason != conformance.ReasonCaseSetUnknown {
			t.Fatalf("outcome = %+v, want an unknown case set", outcome)
		}
	})

	t.Run("a self-report is the only result without a run", func(t *testing.T) {
		runner := conformance.NewRunner(distribution, nil)
		outcome := runner.Run(context.Background(), "builtin", registration, &conformance.SelfReport{FailedCases: []string{"volume.bind"}})
		if outcome.State != conformance.StateUnverified || outcome.Source != conformance.SourceSelfReported {
			t.Fatalf("outcome = %+v, want an unverified self-report", outcome)
		}
		if outcome.Reason != conformance.ReasonSelfReported {
			t.Fatalf("reason = %q, want %q", outcome.Reason, conformance.ReasonSelfReported)
		}
		if len(outcome.Failed) != 1 || outcome.Failed[0] != "volume.bind" {
			t.Fatalf("reported failures = %v, want [volume.bind]", outcome.Failed)
		}
	})

	t.Run("nothing runs and nothing is reported", func(t *testing.T) {
		runner := conformance.NewRunner(distribution, nil)
		outcome := runner.Run(context.Background(), "builtin", registration, nil)
		if outcome.State != conformance.StateUnverified || outcome.Reason != conformance.ReasonNotRun {
			t.Fatalf("outcome = %+v, want an unrun capability", outcome)
		}
	})
}

// pass answers every case successfully.
func pass(context.Context, string, string, string) error { return nil }

// conformanceError is one failed case.
type conformanceError struct{}

// Error implements error.
func (conformanceError) Error() string { return "case failed" }
