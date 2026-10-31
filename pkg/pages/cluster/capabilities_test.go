package cluster

import (
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/conformance"
)

// providerOf builds one provider whose effective set and verifier results are
// the capabilities a case names.
func providerOf(capabilities []v1beta1.ProviderCapability, results []v1beta1.ProviderCapabilityResult) v1beta1.Provider {
	return v1beta1.Provider{Status: v1beta1.ProviderStatus{
		EffectiveCapabilities: capabilities,
		Capabilities:          results,
	}}
}

// verifiedResult is the verifier outcome of one capability.
func verifiedResult(name, version string) v1beta1.ProviderCapabilityResult {
	return v1beta1.ProviderCapabilityResult{Name: name, Version: version, State: conformance.StateVerified}
}

// capabilityOf is one effective capability with an optional guarantee.
func capabilityOf(name, version string, scope v1beta1.CapabilityScope, guarantee *bool) v1beta1.ProviderCapability {
	capability := v1beta1.ProviderCapability{Name: name, Version: version, Scope: scope}
	if guarantee != nil {
		capability.Guarantees = []v1beta1.CapabilityGuarantee{{Key: "snapshot", Bool: guarantee}}
	}
	return capability
}

// TestComparePicksTheHighestVersionBeforeGuarantees pins C-11's order: the
// highest version inside the range is chosen first, and only its guarantees are
// compared. A lower version that would satisfy every guarantee does not rescue
// the requirement.
func TestComparePicksTheHighestVersionBeforeGuarantees(t *testing.T) {
	providers := []v1beta1.Provider{providerOf(
		[]v1beta1.ProviderCapability{
			capabilityOf("storage.volume", "v2", v1beta1.CapabilityScopeNode, new(false)),
			capabilityOf("storage.volume", "v1", v1beta1.CapabilityScopeNode, new(true)),
		},
		[]v1beta1.ProviderCapabilityResult{
			verifiedResult("storage.volume", "v2"),
			verifiedResult("storage.volume", "v1"),
		},
	)}
	requirements := []v1beta1.CapabilityRequirement{{
		Name: "storage.volume", Version: ">=v1",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "snapshot", Bool: new(true)}},
	}}
	result, err := Compare(requirements, providers)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing = %v, want the capability present", result.Missing)
	}
	if len(result.Unsatisfied) != 1 || result.Unsatisfied[0] != "storage.volume.snapshot" {
		t.Fatalf("unsatisfied = %v, want the v2 guarantee refused", result.Unsatisfied)
	}
	if gaps := Gaps(result); len(gaps) != 1 || gaps[0].Kind != "unsatisfied" {
		t.Fatalf("gaps = %+v, want one unsatisfied guarantee", gaps)
	}
}

// TestCompareFiltersByScopeBeforeChoosingAVersion pins C-11's scope rule: a
// cluster requirement is satisfied by a cluster-scoped registration even when a
// higher node-scoped version exists.
func TestCompareFiltersByScopeBeforeChoosingAVersion(t *testing.T) {
	providers := []v1beta1.Provider{providerOf(
		[]v1beta1.ProviderCapability{
			capabilityOf("storage.volume", "v2", v1beta1.CapabilityScopeNode, nil),
			capabilityOf("storage.volume", "v1", v1beta1.CapabilityScopeCluster, nil),
		},
		[]v1beta1.ProviderCapabilityResult{
			verifiedResult("storage.volume", "v2"),
			verifiedResult("storage.volume", "v1"),
		},
	)}
	met, err := Compare([]v1beta1.CapabilityRequirement{{
		Name: "storage.volume", Version: ">=v1", Scope: v1beta1.CapabilityScopeCluster,
	}}, providers)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(met.Missing) != 0 || len(met.Unsatisfied) != 0 {
		t.Fatalf("result = %+v, want the cluster-scoped v1 to satisfy the cluster requirement", met)
	}
	nodeOnly := []v1beta1.Provider{providerOf(
		[]v1beta1.ProviderCapability{capabilityOf("storage.volume", "v2", v1beta1.CapabilityScopeNode, nil)},
		[]v1beta1.ProviderCapabilityResult{verifiedResult("storage.volume", "v2")},
	)}
	blocked, err := Compare([]v1beta1.CapabilityRequirement{{
		Name: "storage.volume", Version: ">=v1", Scope: v1beta1.CapabilityScopeCluster,
	}}, nodeOnly)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(blocked.Missing) != 1 || blocked.Missing[0] != "storage.volume" {
		t.Fatalf("missing = %v, want the node-only registration to leave the requirement missing", blocked.Missing)
	}
}

// TestCompareSeparatesMissingFromUnsatisfied checks C-11's two failure states:
// a required capability no provider serves is missing, an optional one is
// unsatisfied.
func TestCompareSeparatesMissingFromUnsatisfied(t *testing.T) {
	providers := []v1beta1.Provider{providerOf(
		[]v1beta1.ProviderCapability{capabilityOf("storage.volume", "v1", v1beta1.CapabilityScopeNode, nil)},
		[]v1beta1.ProviderCapabilityResult{verifiedResult("storage.volume", "v1")},
	)}
	result, err := Compare([]v1beta1.CapabilityRequirement{
		{Name: "compute.gpu", Version: "v1"},
		{Name: "network.policy", Version: "v1", Optional: true},
	}, providers)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Missing) != 1 || result.Missing[0] != "compute.gpu" {
		t.Fatalf("missing = %v, want the required capability", result.Missing)
	}
	if len(result.Unsatisfied) != 1 || result.Unsatisfied[0] != "network.policy" {
		t.Fatalf("unsatisfied = %v, want the optional capability", result.Unsatisfied)
	}
	gaps := Gaps(result)
	if len(gaps) != 2 || gaps[0].Kind != "missing" || gaps[1].Kind != "unsatisfied" {
		t.Fatalf("gaps = %+v, want missing then unsatisfied", gaps)
	}
}

// TestCompareRefusesAnInvalidRequirement checks that an unparsable version range
// is the definition's error, not a missing capability (C-11).
func TestCompareRefusesAnInvalidRequirement(t *testing.T) {
	_, err := Compare([]v1beta1.CapabilityRequirement{{Name: "storage.volume", Version: "not a range"}}, nil)
	if err == nil {
		t.Fatalf("compare returned no error for an unparsable requirement")
	}
}
