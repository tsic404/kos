package extension

import (
	"testing"
	"time"
)

// TestResolveDisablesCountsOnlyActiveCoverage checks C-28's coverage rule: a
// substitute serves a capability edge only while it is itself active. A
// component that is being disabled, or that cannot run because its own
// dependencies are unmet, does not let the provider's disable through.
func TestResolveDisablesCountsOnlyActiveCoverage(t *testing.T) {
	target := disableCandidate("target", "demo.target", "demo/x")
	substitute := disableCandidate("substitute", "demo.substitute", "demo/x")
	consumer := disableConsumer("consumer", "demo.consumer", "demo/x")

	t.Run("a running consumer blocks the disable", func(t *testing.T) {
		blocked, disabled := resolveDisables([]candidate{target}, []candidate{consumer})
		if len(blocked) != 1 || len(disabled) != 0 {
			t.Fatalf("blocked=%d disabled=%d, want the target blocked", len(blocked), len(disabled))
		}
		if got := blocked[0].disableBlockedBy; len(got) != 1 || got[0] != "demo.consumer" {
			t.Fatalf("dependents = %v", got)
		}
	})

	t.Run("requested substitutes never count as coverage", func(t *testing.T) {
		// Neither provider may be disabled while the consumer depends on the
		// capability: the blocked set is resolved before any disable takes
		// effect, so no request may cover another request.
		blocked, disabled := resolveDisables([]candidate{target, substitute}, []candidate{consumer})
		if len(disabled) != 0 || len(blocked) != 2 {
			t.Fatalf("blocked=%d disabled=%d, want both requests blocked", len(blocked), len(disabled))
		}
		for _, component := range blocked {
			if len(component.disableBlockedBy) != 1 || component.disableBlockedBy[0] != "demo.consumer" {
				t.Fatalf("%s dependents = %v", component.name, component.disableBlockedBy)
			}
		}
	})

	t.Run("a substitute with unmet dependencies does not cover", func(t *testing.T) {
		unavailable := disableCandidate("unavailable", "demo.unavailable", "demo/x")
		unavailable.loaded.Dependencies = Dependencies{
			Capabilities: []CapabilityRequirement{{Name: "demo.absent/api", Versions: "v1"}},
		}
		blocked, disabled := resolveDisables([]candidate{target}, []candidate{consumer, unavailable})
		if len(blocked) != 1 || len(disabled) != 0 {
			t.Fatalf("blocked=%d disabled=%d, want the target blocked", len(blocked), len(disabled))
		}
	})

	t.Run("an active substitute covers the edge", func(t *testing.T) {
		blocked, disabled := resolveDisables([]candidate{target}, []candidate{consumer, substitute})
		if len(blocked) != 0 || len(disabled) != 1 || disabled[0].name != "target" {
			t.Fatalf("blocked=%d disabled=%d, want the target disabled", len(blocked), len(disabled))
		}
	})

	t.Run("a component edge always counts", func(t *testing.T) {
		componentConsumer := disableConsumer("component-consumer", "demo.component-consumer", "demo/other")
		componentConsumer.loaded.Dependencies.Components = []ComponentRequirement{{Identifier: "demo.target"}}
		blocked, disabled := resolveDisables([]candidate{target}, []candidate{componentConsumer, substitute})
		if len(blocked) != 1 || len(disabled) != 0 {
			t.Fatalf("blocked=%d disabled=%d, want the target blocked by the component edge", len(blocked), len(disabled))
		}
	})
}

// TestEvaluateKeepsAPreviouslyDisabledComponentDisabled checks the round after
// a disable took effect: the contribution is not reactivated, does not cover
// another component's requirement, and the dependent is Unavailable (C-28).
func TestEvaluateKeepsAPreviouslyDisabledComponentDisabled(t *testing.T) {
	start := time.Now()
	provider := Declaration{
		Identifier: "demo.sub", Version: "1.0.0",
		Contributions: Contributions{
			Capabilities: CapabilityContributions{Items: []Capability{{Name: "demo.sub/x", Version: "v1"}}},
		},
	}
	consumer := Declaration{
		Identifier: "demo.consumer", Version: "1.0.0",
		Contributions: Contributions{Pages: []Page{{
			ID: "consumer", Title: "Consumer", Location: "panel",
			Items: []PageItem{{ID: "x", Label: "X", Source: PageSourceObject, Reference: "X", Reason: "acceptance"}},
		}}},
		Dependencies: Dependencies{
			Capabilities: []CapabilityRequirement{{Name: "demo.sub/x", Versions: "v1"}},
		},
	}
	disabledRequest := packageRequest(t, "sub", provider, start)
	disabledRequest.IsDisabled = true
	disabledRequest.WasDisabled = true
	assessment := Evaluate([]Request{
		disabledRequest,
		packageRequest(t, "consumer", consumer, start.Add(time.Second)),
	})
	byName := indexResults(assessment)
	if byName["sub"].Phase != PhaseDisabled {
		t.Fatalf("sub = %s, want the stored disable kept", byName["sub"].Phase)
	}
	if byName["consumer"].Phase != PhaseUnavailable || len(byName["consumer"].Missing) == 0 {
		t.Fatalf("consumer = %s/%v, want it unavailable on the withdrawn capability", byName["consumer"].Phase, byName["consumer"].Missing)
	}
	if len(assessment.Registry.ExtensionCapabilities()) != 0 {
		t.Fatal("the previously disabled component registered its capability")
	}
}

// disableCandidate builds one provider with a single capability.
func disableCandidate(name, identifier, capability string) candidate {
	return candidate{
		name: name,
		loaded: Loaded{
			Identifier: identifier,
			Version:    "1.0.0",
			Contributions: Contributions{
				Capabilities: CapabilityContributions{Items: []Capability{{Name: capability, Version: "v1"}}},
			},
		},
	}
}

// disableConsumer builds one component that requires a capability.
func disableConsumer(name, identifier, capability string) candidate {
	return candidate{
		name: name,
		loaded: Loaded{
			Identifier: identifier,
			Version:    "1.0.0",
			Dependencies: Dependencies{
				Capabilities: []CapabilityRequirement{{Name: capability, Versions: "v1"}},
			},
		},
	}
}

// TestSupportedFrameworksReturnsACopy keeps the served set from being mutated by
// a caller that sorts or edits the returned slice.
func TestSupportedFrameworksReturnsACopy(t *testing.T) {
	first := SupportedFrameworks()
	if len(first) != 1 || first[0] != FrameworkVersion {
		t.Fatalf("supported frameworks = %v, want [%s]", first, FrameworkVersion)
	}
	first[0] = "9.9"
	if second := SupportedFrameworks(); len(second) != 1 || second[0] != FrameworkVersion {
		t.Fatalf("a caller changed the shared set: %v", second)
	}
}

// TestCheckReferencePathRejectsIndexSegments checks that a path the reader
// cannot follow is refused at declaration time instead of silently failing
// C-19's deletion protection.
func TestCheckReferencePathRejectsIndexSegments(t *testing.T) {
	cases := map[string]bool{
		"spec.volumeRef":    true,
		"spec.vm.volumeRef": true,
		"spec.items[0].ref": false,
		"spec.items.0.ref":  false,
		"spec.items.ref":    true,
		"metadata.name":     false,
		"spec..ref":         false,
		" spec.volumeRef":   false,
	}
	for path, valid := range cases {
		problem := checkReferencePath(path)
		if (problem == "") != valid {
			t.Errorf("checkReferencePath(%q) = %q, want valid=%v", path, problem, valid)
		}
	}
}

// TestEvaluateRefusesDisableWhenTheOnlyCoverageIsDisabled builds the exact
// scenario C-28 protects: two declarations serve the same capability, one is
// already disabled, and a consumer depends on it. Disabling the remaining
// provider must be refused and must name the consumer; the disabled provider
// is not coverage.
func TestEvaluateRefusesDisableWhenTheOnlyCoverageIsDisabled(t *testing.T) {
	start := time.Now()
	page := []Page{{
		ID: "consumer", Title: "Consumer", Location: "panel",
		Items: []PageItem{{ID: "x", Label: "X", Source: PageSourceObject, Reference: "X", Reason: "acceptance"}},
	}}
	// Two identifiers may serve one capability name when one is a prefix of the
	// other, so the shared-capability scenario is constructible.
	target := Declaration{
		Identifier: "demo", Version: "1.0.0",
		Contributions: Contributions{
			Capabilities: CapabilityContributions{Items: []Capability{{Name: "demo/a/x", Version: "v1"}}},
		},
	}
	substitute := Declaration{
		Identifier: "demo/a", Version: "1.0.0",
		Contributions: Contributions{
			Capabilities: CapabilityContributions{Items: []Capability{{Name: "demo/a/x", Version: "v1"}}},
		},
	}
	consumer := Declaration{
		Identifier: "demo.c", Version: "1.0.0",
		Contributions: Contributions{Pages: page},
		Dependencies: Dependencies{
			Capabilities: []CapabilityRequirement{{Name: "demo/a/x", Versions: "v1"}},
		},
	}
	targetRequest := packageRequest(t, "target", target, start)
	targetRequest.IsDisabled = true
	substituteRequest := packageRequest(t, "substitute", substitute, start.Add(time.Second))
	substituteRequest.IsDisabled = true
	substituteRequest.WasDisabled = true

	assessment := Evaluate([]Request{
		targetRequest,
		substituteRequest,
		packageRequest(t, "consumer", consumer, start.Add(2*time.Second)),
	})
	byName := indexResults(assessment)
	if byName["target"].Phase != PhaseReady || len(byName["target"].DisableBlockedBy) != 1 ||
		byName["target"].DisableBlockedBy[0] != "demo.c" {
		t.Fatalf("target = %s dependents=%v, want the disable refused and the consumer named",
			byName["target"].Phase, byName["target"].DisableBlockedBy)
	}
	if byName["substitute"].Phase != PhaseDisabled {
		t.Fatalf("substitute = %s, want the stored disable kept", byName["substitute"].Phase)
	}
	if byName["consumer"].Phase != PhaseReady {
		t.Fatalf("consumer = %s, want it served by the remaining provider", byName["consumer"].Phase)
	}
	if got := assessment.Registry.ExtensionCapabilities(); len(got) != 1 {
		t.Fatalf("capabilities = %v, want exactly the remaining provider", got)
	}
}
