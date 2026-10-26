package extension

import (
	"strings"
	"testing"
)

// TestConstraintComparesVersions checks the range grammar dependency edges use
// (C-18, C-28).
func TestConstraintComparesVersions(t *testing.T) {
	cases := []struct {
		constraint string
		version    string
		allowed    bool
	}{
		{constraint: "", version: "v7", allowed: true},
		{constraint: "v1", version: "v1", allowed: true},
		{constraint: "v1", version: "v1.0", allowed: true},
		{constraint: "v1", version: "v2", allowed: false},
		{constraint: ">=1.2 <2.0", version: "1.5", allowed: true},
		{constraint: ">=1.2 <2.0", version: "2.0", allowed: false},
		{constraint: ">=1.2 <2.0", version: "1.1.9", allowed: false},
		{constraint: ">v1 <=v2", version: "v2", allowed: true},
		{constraint: ">v1 <=v2", version: "v1", allowed: false},
		{constraint: "<v3", version: "v3.0.1", allowed: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.constraint+"|"+testCase.version, func(t *testing.T) {
			constraint, err := ParseConstraint(testCase.constraint)
			if err != nil {
				t.Fatalf("parse %q: %v", testCase.constraint, err)
			}
			if got := constraint.Allows(testCase.version); got != testCase.allowed {
				t.Fatalf("%q allows %q = %v, want %v", testCase.constraint, testCase.version, got, testCase.allowed)
			}
		})
	}
}

// TestParseConstraintRejectsGarbage checks that a malformed range is a package
// error instead of a silent "any version".
func TestParseConstraintRejectsGarbage(t *testing.T) {
	for _, text := range []string{">=one", "1.x", ">="} {
		if _, err := ParseConstraint(text); err == nil {
			t.Errorf("constraint %q was accepted", text)
		}
	}
}

// TestBaselineCapabilitiesAreTheEightKernelNames pins C-09's baseline and its
// starting version; a product never changes them (TC-2).
func TestBaselineCapabilitiesAreTheEightKernelNames(t *testing.T) {
	capabilities := BaselineCapabilities()
	want := []string{
		"storage.volume", "storage.snapshot", "storage.shared-rwx",
		"network.http-endpoint", "network.path-prefix", "network.l4-endpoint",
		"network.policy", "compute.gpu",
	}
	if len(capabilities) != len(want) {
		t.Fatalf("baseline has %d capabilities, want %d", len(capabilities), len(want))
	}
	for index, name := range want {
		if capabilities[index].Name != name || capabilities[index].Version != "v1" {
			t.Errorf("baseline[%d] = %s@%s, want %s@v1", index, capabilities[index].Name, capabilities[index].Version, name)
		}
	}
}

// TestMissingDependenciesNamesEveryUnmetEdge checks the form a component status
// lists unmet edges in (C-28).
func TestMissingDependenciesNamesEveryUnmetEdge(t *testing.T) {
	catalogue := catalogueFor(nil)
	missing := catalogue.missingDependencies(Dependencies{
		Capabilities: []CapabilityRequirement{
			{Name: "storage.volume", Versions: "v1"},
			{Name: "demo.widgets/api", Versions: ">=v2"},
		},
		Components: []ComponentRequirement{
			{Identifier: "demo.base"},
		},
	})
	if len(missing) != 2 {
		t.Fatalf("missing = %v, want the capability and the component", missing)
	}
	if !strings.Contains(missing[0], "demo.widgets/api >=v2") {
		t.Errorf("capability item = %q", missing[0])
	}
	if !strings.Contains(missing[1], "component demo.base any") {
		t.Errorf("component item = %q", missing[1])
	}
}
