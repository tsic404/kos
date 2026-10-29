// Package tc2 verifies that installing and removing a product never changes the
// kernel capability baseline and that the product capability is readable only
// while the component is installed (AC-1, TC-2).
package tc2

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// declaration contributes one capability on top of the kernel baseline.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Capabilities: extension.CapabilityContributions{Items: []extension.Capability{{Name: "demo.widgets/panel", Version: "v1"}}},
		},
	}
}

// TestTC2CapabilityBaselineDoesNotChange reads the catalogue before, during and
// after a product install.
func TestTC2CapabilityBaselineDoesNotChange(t *testing.T) {
	cluster := harness.Start(t)
	artifacts := extension.MemoryArtifacts{}
	baseline := extension.BaselineCapabilities()

	before := cluster.ExtensionState(t, artifacts)
	if got := before.Registry().Capabilities(); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("catalogue before the install = %+v, want the baseline %+v", got, baseline)
	}

	installed := cluster.InstallPackage(t, "widgets", declaration(), artifacts)
	capabilities := installed.Registry().Capabilities()
	if len(capabilities) != len(baseline)+1 {
		t.Fatalf("catalogue = %v, want the baseline plus one contribution", capabilities)
	}
	// The whole entry is compared, not just its identity: the projection may
	// not add or rewrite guarantees, parameters or case sets either.
	for index, want := range baseline {
		if !reflect.DeepEqual(capabilities[index], want) {
			t.Errorf("baseline entry %d = %+v, want %+v", index, capabilities[index], want)
		}
		if capabilities[index].Version != "v1" {
			t.Errorf("baseline %s moved to %s", want.Name, capabilities[index].Version)
		}
	}
	contributed := capabilities[len(capabilities)-1]
	if !strings.HasPrefix(contributed.Name, "demo.widgets") {
		t.Fatalf("extension capability %q does not carry the extension identifier as a prefix", contributed.Name)
	}
	if _, found := installed.Registry().Capability(contributed.Name, contributed.Version); !found {
		t.Fatalf("the extension capability %s@%s is not registered while installed", contributed.Name, contributed.Version)
	}

	cluster.DeleteComponent(t, "widgets")
	removed := cluster.ExtensionState(t, artifacts)
	if got := removed.Registry().Capabilities(); !reflect.DeepEqual(got, baseline) {
		t.Fatalf("catalogue after the uninstall = %+v, want the baseline %+v", got, baseline)
	}
	if _, found := removed.Registry().Capability(contributed.Name, contributed.Version); found {
		t.Fatalf("the extension capability %s@%s is still registered", contributed.Name, contributed.Version)
	}
}
