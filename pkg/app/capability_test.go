package app_test

import (
	"reflect"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// capability builds one registered capability the kernel verified.
func capability(name, version string, guarantees map[string]app.Guarantee) app.Capability {
	return app.Capability{Name: name, Version: version, Guarantees: guarantees, Verified: true}
}

// boolean reads a boolean guarantee value.
func boolean(value bool) app.Guarantee {
	return app.Guarantee{Bool: new(value)}
}

func TestCompareSeparatesMissingFromUnsatisfied(t *testing.T) {
	requirements := []v1beta1.CapabilityRequirement{
		{Name: "storage.block", Guarantees: []v1beta1.GuaranteeRequirement{{Key: "snapshot", Bool: new(true)}}},
		{Name: "network.ingress"},
	}
	available := []app.Capability{
		capability("storage.block", "1.2.0", map[string]app.Guarantee{"snapshot": boolean(false)}),
	}
	result, err := app.Compare(requirements, available)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if want := []string{"network.ingress"}; !reflect.DeepEqual(result.Missing, want) {
		t.Errorf("missing = %v, want %v", result.Missing, want)
	}
	if want := []string{"storage.block.snapshot"}; !reflect.DeepEqual(result.Unsatisfied, want) {
		t.Errorf("unsatisfied = %v, want %v", result.Unsatisfied, want)
	}
}

func TestComparePicksTheHighestVersionInRange(t *testing.T) {
	requirements := []v1beta1.CapabilityRequirement{{
		Name:    "storage.block",
		Version: ">=1.0,<2.0",
		Guarantees: []v1beta1.GuaranteeRequirement{
			{Key: "snapshot", Bool: new(true)},
		},
	}}
	available := []app.Capability{
		capability("storage.block", "1.0.0", map[string]app.Guarantee{"snapshot": boolean(true)}),
		capability("storage.block", "1.4.0", map[string]app.Guarantee{"snapshot": boolean(false)}),
		capability("storage.block", "2.0.0", map[string]app.Guarantee{"snapshot": boolean(true)}),
	}
	result, err := app.Compare(requirements, available)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing = %v, want none", result.Missing)
	}
	if want := []string{"storage.block.snapshot"}; !reflect.DeepEqual(result.Unsatisfied, want) {
		t.Errorf("unsatisfied = %v, want %v from the highest in-range version", result.Unsatisfied, want)
	}
}

func TestCompareEnumGuarantee(t *testing.T) {
	requirements := []v1beta1.CapabilityRequirement{{
		Name: "network.ingress",
		Guarantees: []v1beta1.GuaranteeRequirement{
			{Key: "mode", Enum: "strict"},
			{Key: "protocol", OneOf: []string{"http", "https"}},
		},
	}}
	available := []app.Capability{
		capability("network.ingress", "1.0.0", map[string]app.Guarantee{
			"mode":     {Enum: "strict"},
			"protocol": {Enum: "https"},
		}),
	}
	result, err := app.Compare(requirements, available)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Unsatisfied) != 0 {
		t.Fatalf("unsatisfied = %v, want none", result.Unsatisfied)
	}

	requirements[0].Guarantees[1].OneOf = []string{"h2"}
	result, err = app.Compare(requirements, available)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if want := []string{"network.ingress.protocol"}; !reflect.DeepEqual(result.Unsatisfied, want) {
		t.Errorf("unsatisfied = %v, want %v", result.Unsatisfied, want)
	}
}

func TestCompareOptionalFailureOnlyDegrades(t *testing.T) {
	requirements := []v1beta1.CapabilityRequirement{{Name: "metrics.basic", Optional: true}}
	result, err := app.Compare(requirements, nil)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing = %v, want none: an optional capability never blocks", result.Missing)
	}
	if want := []string{"metrics.basic"}; !reflect.DeepEqual(result.Unsatisfied, want) {
		t.Errorf("unsatisfied = %v, want %v", result.Unsatisfied, want)
	}
}

func TestCompareRefusesAnUndecidableRequirement(t *testing.T) {
	requirements := []v1beta1.CapabilityRequirement{{Name: "storage.block", Version: "next-release"}}
	if _, err := app.Compare(requirements, nil); err == nil {
		t.Fatal("a version range that does not parse was accepted")
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	want := []int64{10, 20, 40, 80, 160, 300, 300}
	for index, seconds := range want {
		got := app.Backoff(index + 1).Seconds()
		if int64(got) != seconds {
			t.Errorf("backoff for failure %d = %v, want %ds", index+1, got, seconds)
		}
	}
}

func TestValidateRequirementRejectsEveryMalformedTerm(t *testing.T) {
	for _, constraint := range []string{">=2.0,banana", ">=2.0,", ">=", "1.0,,2.0", "banana"} {
		requirement := v1beta1.CapabilityRequirement{Name: "storage.block", Version: constraint}
		if err := app.ValidateRequirement(requirement); err == nil {
			t.Errorf("constraint %q passed validation", constraint)
		}
	}
	if err := app.ValidateRequirement(v1beta1.CapabilityRequirement{Name: "storage.block", Version: ">=1.0,<2.0"}); err != nil {
		t.Errorf("a well-formed range was refused: %v", err)
	}
}

func TestCompareNeverPicksAnUnparseableRegisteredVersion(t *testing.T) {
	requirements := []v1beta1.CapabilityRequirement{{
		Name:       "storage.block",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "snapshot", Bool: new(true)}},
	}}
	// The provider registered a version that cannot be ordered first; the
	// comparison must still choose the highest parseable one (C-11).
	available := []app.Capability{
		capability("storage.block", "banana", map[string]app.Guarantee{"snapshot": boolean(false)}),
		capability("storage.block", "1.4.0", map[string]app.Guarantee{"snapshot": boolean(true)}),
	}
	result, err := app.Compare(requirements, available)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Missing) != 0 || len(result.Unsatisfied) != 0 {
		t.Fatalf("missing = %v, unsatisfied = %v, want none", result.Missing, result.Unsatisfied)
	}
}

// TestValidateRequirementRejectsAmbiguousEnum covers C-11's comparison rule: an
// enum requirement is either one value or a declared set, never both.
func TestValidateRequirementRejectsAmbiguousEnum(t *testing.T) {
	requirement := v1beta1.CapabilityRequirement{
		Name: "storage.block",
		Guarantees: []v1beta1.GuaranteeRequirement{
			{Key: "mode", Enum: "fast", OneOf: []string{"fast", "slow"}},
		},
	}
	if err := app.ValidateRequirement(requirement); err == nil {
		t.Fatal("a guarantee declaring both enum and oneOf passed validation")
	}
	if err := app.ValidateRequirement(v1beta1.CapabilityRequirement{
		Name:       "storage.block",
		Guarantees: []v1beta1.GuaranteeRequirement{{Key: "mode", OneOf: []string{"fast", "slow"}}},
	}); err != nil {
		t.Fatalf("a oneOf-only guarantee was refused: %v", err)
	}
}

// TestCompareScopeDecidesWhichRegistrationsServe covers C-11's scope rule: a
// cluster-level requirement is not satisfied by a node-level registration, a
// node-level requirement accepts either scope, and an omitted scope on either
// side reads as node.
func TestCompareScopeDecidesWhichRegistrationsServe(t *testing.T) {
	node := app.Capability{Name: "storage.volume", Version: "v1", Scope: v1beta1.CapabilityScopeNode, Verified: true}
	clusterWide := app.Capability{Name: "storage.volume", Version: "v1", Scope: v1beta1.CapabilityScopeCluster, Verified: true}
	unstated := app.Capability{Name: "storage.volume", Version: "v1", Verified: true}

	cases := []struct {
		name        string
		scope       v1beta1.CapabilityScope
		available   []app.Capability
		wantMissing []string
	}{
		{
			name:        "a cluster requirement is not met by a node capability",
			scope:       v1beta1.CapabilityScopeCluster,
			available:   []app.Capability{node},
			wantMissing: []string{"storage.volume"},
		},
		{
			name:      "a cluster requirement is met by a cluster capability",
			scope:     v1beta1.CapabilityScopeCluster,
			available: []app.Capability{clusterWide},
		},
		{
			name:      "a node requirement is met by a node capability",
			scope:     v1beta1.CapabilityScopeNode,
			available: []app.Capability{node},
		},
		{
			name:      "a node requirement is met by a cluster capability",
			scope:     v1beta1.CapabilityScopeNode,
			available: []app.Capability{clusterWide},
		},
		{
			name:      "an omitted requirement scope asks for node",
			available: []app.Capability{node},
		},
		{
			name:        "a registration without a scope never satisfies cluster",
			scope:       v1beta1.CapabilityScopeCluster,
			available:   []app.Capability{unstated},
			wantMissing: []string{"storage.volume"},
		},
		{
			name:      "a cluster requirement reads the lower version that holds",
			scope:     v1beta1.CapabilityScopeCluster,
			available: []app.Capability{{Name: "storage.volume", Version: "v2", Scope: v1beta1.CapabilityScopeNode, Verified: true}, clusterWide},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := app.Compare([]v1beta1.CapabilityRequirement{{Name: "storage.volume", Scope: test.scope}}, test.available)
			if err != nil {
				t.Fatalf("compare: %v", err)
			}
			if !reflect.DeepEqual(result.Missing, test.wantMissing) {
				t.Errorf("missing = %v, want %v", result.Missing, test.wantMissing)
			}
			if len(result.Unsatisfied) != 0 {
				t.Errorf("unsatisfied = %v, want none", result.Unsatisfied)
			}
		})
	}
}

// TestCompareOptionalClusterRequirementOnlyDegrades keeps a scope shortfall on
// an optional requirement from blocking the install (C-11).
func TestCompareOptionalClusterRequirementOnlyDegrades(t *testing.T) {
	result, err := app.Compare([]v1beta1.CapabilityRequirement{{
		Name:     "compute.gpu",
		Scope:    v1beta1.CapabilityScopeCluster,
		Optional: true,
	}}, []app.Capability{{Name: "compute.gpu", Version: "v1", Scope: v1beta1.CapabilityScopeNode, Verified: true}})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if len(result.Missing) != 0 {
		t.Fatalf("missing = %v, want none: an optional requirement never blocks", result.Missing)
	}
	if want := []string{"compute.gpu"}; !reflect.DeepEqual(result.Unsatisfied, want) {
		t.Errorf("unsatisfied = %v, want %v", result.Unsatisfied, want)
	}
}

// TestValidateRequirementRejectsAnUnknownScope keeps an undecidable scope from
// being read as either value (C-11).
func TestValidateRequirementRejectsAnUnknownScope(t *testing.T) {
	requirement := v1beta1.CapabilityRequirement{Name: "storage.volume", Scope: "edge"}
	if err := app.ValidateRequirement(requirement); err == nil {
		t.Fatal("a requirement with an unknown scope passed validation")
	}
	for _, scope := range []v1beta1.CapabilityScope{"", v1beta1.CapabilityScopeNode, v1beta1.CapabilityScopeCluster} {
		requirement.Scope = scope
		if err := app.ValidateRequirement(requirement); err != nil {
			t.Errorf("scope %q was refused: %v", scope, err)
		}
	}
}
