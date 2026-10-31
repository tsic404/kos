package extension

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestCompatibleFramework checks the target range against the served set
// (C-16): an empty range admits every version, a range that covers a served
// version installs, and a range outside the set is named.
func TestCompatibleFramework(t *testing.T) {
	cases := []struct {
		name         string
		target       string
		supported    []string
		compatible   bool
		wantsMessage string
	}{
		{name: "omitted", target: "", supported: []string{"1.1"}, compatible: true},
		{name: "covered", target: ">=1.0 <2.0", supported: []string{"1.1"}, compatible: true},
		{name: "exact", target: "1.1", supported: []string{"1.1"}, compatible: true},
		{name: "ahead", target: ">=2.0", supported: []string{"1.1"}, compatible: false, wantsMessage: ">=2.0"},
		{name: "behind", target: "<1.0", supported: []string{"1.1"}, compatible: false, wantsMessage: "<1.0"},
		{name: "malformed", target: "~>1", supported: []string{"1.1"}, compatible: false, wantsMessage: "~>1"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			compatible, problems := compatibleFramework(test.target, test.supported)
			if compatible != test.compatible {
				t.Fatalf("compatible = %v, want %v (problems %v)", compatible, test.compatible, problems)
			}
			if test.compatible {
				if len(problems) != 0 {
					t.Fatalf("a compatible target listed %v", problems)
				}
				return
			}
			if len(problems) == 0 || !strings.Contains(problems[0], test.wantsMessage) {
				t.Fatalf("problems = %v, want a message naming %q", problems, test.wantsMessage)
			}
		})
	}
}

// TestPreflightUpgradeBlocksOnlyStrandedExtensions checks C-16: an extension
// that runs on a target version does not block, one that only runs on a removed
// version does, and removing it clears the block.
func TestPreflightUpgradeBlocksOnlyStrandedExtensions(t *testing.T) {
	installed := []FrameworkDeclaration{
		{Component: "old-only", Identifier: "demo.old", FrameworkVersion: ">=1.0 <2.0"},
		{Component: "both", Identifier: "demo.both", FrameworkVersion: ">=1.0"},
		{Component: "new-only", Identifier: "demo.new", FrameworkVersion: ">=2.0"},
		{Component: "unbounded", Identifier: "demo.any"},
	}
	preflight, err := PreflightUpgradeFrom([]string{"1.1"}, []string{"2.0"}, installed)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if len(preflight.Removed) != 1 || preflight.Removed[0] != "1.1" {
		t.Fatalf("removed = %v, want [1.1]", preflight.Removed)
	}
	if !preflight.IsBlocked || len(preflight.Dependents) != 1 || preflight.Dependents[0].Component != "old-only" {
		t.Fatalf("dependents = %v, blocked = %v", preflight.Dependents, preflight.IsBlocked)
	}

	cleared, err := PreflightUpgradeFrom([]string{"1.1"}, []string{"2.0"}, []FrameworkDeclaration{
		{Component: "both", Identifier: "demo.both", FrameworkVersion: ">=1.0"},
	})
	if err != nil {
		t.Fatalf("preflight after the dependency left: %v", err)
	}
	if cleared.IsBlocked || len(cleared.Dependents) != 0 {
		t.Fatalf("preflight still blocked: %+v", cleared)
	}

	noop, err := PreflightUpgradeFrom([]string{"1.0", "1.1"}, []string{"1.0", "1.1"}, installed)
	if err != nil {
		t.Fatalf("preflight without a removal: %v", err)
	}
	if noop.IsBlocked || len(noop.Removed) != 0 {
		t.Fatalf("a preflight that removes nothing = %+v", noop)
	}
	if _, err := PreflightUpgradeFrom([]string{"1.1"}, nil, installed); err == nil {
		t.Fatal("a preflight with no target version passed")
	}
}

// TestEvaluateFrameworkIncompatibleActivatessNothing checks that an
// incompatible target rejects the install without contributing anything (C-16,
// C-28).
func TestEvaluateFrameworkIncompatibleActivatesNothing(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.FrameworkVersion = ">=2.0"
	assessment := Evaluate([]Request{packageRequest(t, "widgets", declaration, time.Now())})
	result := assessment.Components[0]
	if result.Phase != PhaseIncompatible || result.Reason != ReasonIncompatibleFramework {
		t.Fatalf("result = %s/%s, want incompatible", result.Phase, result.Reason)
	}
	if len(result.Incompatible) == 0 || !strings.Contains(result.Incompatible[0], ">=2.0") {
		t.Fatalf("incompatible items = %v", result.Incompatible)
	}
	if len(assessment.Registry.ObjectClasses()) != 0 || len(assessment.Registry.Pages()) != 0 ||
		len(assessment.Registry.ExtensionCapabilities()) != 0 {
		t.Fatal("an incompatible component contributed something")
	}
	status := StatusFor(result, nil)
	if status.Phase != PhaseIncompatible || status.Health.State != "Unavailable" {
		t.Fatalf("status = %+v", status)
	}
	found := false
	for _, condition := range status.Conditions {
		if condition.Type == ConditionFrameworkCompatible {
			found = condition.Status != "True" && condition.Reason == ReasonIncompatibleFramework
		}
	}
	if !found {
		t.Fatalf("no FrameworkCompatible=False condition: %+v", status.Conditions)
	}
}

// TestEvaluateDisableRespectsDependents checks C-28: a dependent keeps a
// disable request from taking effect and is named; without one the component's
// contributions disappear and its phase is Disabled.
func TestEvaluateDisableRespectsDependents(t *testing.T) {
	start := time.Now()
	target := fullDeclaration("demo.base")
	consumer := Declaration{
		Identifier: "demo.consumer",
		Version:    "1.0.0",
		Contributions: Contributions{
			Pages: []Page{{ID: "consumer", Title: "Consumer"}},
		},
		Dependencies: Dependencies{
			Components: []ComponentRequirement{{Identifier: "demo.base"}},
		},
	}
	blockedRequest := packageRequest(t, "base", target, start)
	blockedRequest.IsDisabled = true
	assessment := Evaluate([]Request{
		blockedRequest,
		packageRequest(t, "consumer", consumer, start.Add(time.Second)),
	})
	base := indexResults(assessment)["base"]
	if base.Phase != PhaseReady {
		t.Fatalf("base = %s, want the active phase while a dependent holds it", base.Phase)
	}
	if len(base.DisableBlockedBy) != 1 || base.DisableBlockedBy[0] != "demo.consumer" {
		t.Fatalf("DisableBlockedBy = %v", base.DisableBlockedBy)
	}
	if !assessment.Registry.IsServing("widgets.example.test", "Widget") {
		t.Fatal("the blocked disable withdrew the contributions anyway")
	}
	status := StatusFor(base, nil)
	condition, found := conditionOfType(status.Conditions, ConditionDisableBlocked)
	if !found || condition.Status != metav1.ConditionTrue || !strings.Contains(condition.Message, "demo.consumer") {
		t.Fatalf("DisableBlocked condition = %+v", condition)
	}

	alone := packageRequest(t, "base", target, start)
	alone.IsDisabled = true
	disabled := Evaluate([]Request{alone})
	result := disabled.Components[0]
	if result.Phase != PhaseDisabled || result.Reason != ReasonDisabled {
		t.Fatalf("disabled result = %s/%s", result.Phase, result.Reason)
	}
	if len(disabled.Registry.ObjectClasses()) != 0 || len(disabled.Registry.Pages()) != 0 ||
		len(disabled.Registry.ExtensionCapabilities()) != 0 {
		t.Fatal("a disabled component still contributes")
	}
	if status := StatusFor(result, nil); status.Phase != PhaseDisabled || status.Health.State != "Unavailable" {
		t.Fatalf("disabled status = %+v", status)
	}
}

// TestEvaluateLifecycleStates checks the remaining C-28 phases: an installed
// extension with nothing to activate, and a component whose deletion is under
// way.
func TestEvaluateLifecycleStates(t *testing.T) {
	empty := Declaration{Identifier: "demo.anchor", Version: "1.0.0"}
	installed := Evaluate([]Request{packageRequest(t, "anchor", empty, time.Now())})
	if result := installed.Components[0]; result.Phase != PhaseInstalled || result.Reason != ReasonInstalled {
		t.Fatalf("installed result = %s/%s", result.Phase, result.Reason)
	}
	if _, found := installed.Registry.PreferenceKeys("demo.anchor"); found {
		t.Fatal("an installed anchor contributes a preference domain")
	}

	terminatingRequest := packageRequest(t, "anchor", fullDeclaration("demo.anchor"), time.Now())
	terminatingRequest.IsTerminating = true
	terminating := Evaluate([]Request{terminatingRequest})
	result := terminating.Components[0]
	if result.Phase != PhaseTerminating || result.Reason != ReasonTerminating {
		t.Fatalf("terminating result = %s/%s", result.Phase, result.Reason)
	}
	// A held deletion keeps the contributions serving until the dependents that
	// hold it are gone (C-28, C-29).
	if !terminating.Registry.IsServing("widgets.example.test", "Widget") {
		t.Fatal("a held uninstall withdrew its class while a dependent holds it")
	}
}

// conditionOfType finds one condition by type.
func conditionOfType(conditions []metav1.Condition, conditionType string) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
