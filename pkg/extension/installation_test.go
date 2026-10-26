package extension

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kos/api/v1beta1"
)

// packageRequest builds an install request carrying a verified package.
func packageRequest(t *testing.T, name string, declaration Declaration, created time.Time) Request {
	t.Helper()
	content, err := Marshal(declaration)
	if err != nil {
		t.Fatalf("marshal declaration: %v", err)
	}
	return Request{
		Name:    name,
		Created: created,
		Package: &Package{Reference: name, Digest: DigestOf(content), Content: content},
	}
}

// TestEvaluateRejectsDigestMismatch checks that a package whose artifact does
// not match the named digest installs nothing (C-17).
func TestEvaluateRejectsDigestMismatch(t *testing.T) {
	content, err := Marshal(fullDeclaration("demo.widgets"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := Request{
		Name:    "widgets",
		Package: &Package{Reference: "widgets", Digest: "sha256:" + strings.Repeat("0", 64), Content: content},
	}
	assessment := Evaluate([]Request{request})
	result := assessment.Components[0]
	if result.Phase != PhaseUnavailable || result.Reason != ReasonDigestMismatch {
		t.Fatalf("result = %s/%s, want unavailable on a digest mismatch", result.Phase, result.Reason)
	}
	if len(result.Contributions.ObjectClasses) != 0 || len(assessment.Registry.ObjectClasses()) != 0 {
		t.Fatal("a rejected package contributed an object class")
	}
	if len(assessment.Registry.Pages()) != 0 || len(assessment.Registry.Settings()) != 0 {
		t.Fatal("a rejected package contributed a page or a setting")
	}
	if len(assessment.Registry.ExtensionCapabilities()) != 0 {
		t.Fatal("a rejected package contributed a capability")
	}
}

// TestEvaluateKeepsOneVersionPerIdentifier checks C-17: the earlier install keeps
// a contested identifier and the later one is unavailable.
func TestEvaluateKeepsOneVersionPerIdentifier(t *testing.T) {
	start := time.Now()
	first := fullDeclaration("demo.widgets")
	first.Version = "1.0.0"
	second := fullDeclaration("demo.widgets")
	second.Version = "2.0.0"
	assessment := Evaluate([]Request{
		packageRequest(t, "older", first, start),
		packageRequest(t, "newer", second, start.Add(time.Minute)),
	})
	byName := indexResults(assessment)
	if byName["older"].Phase != PhaseReady {
		t.Fatalf("older = %s, want ready", byName["older"].Phase)
	}
	if byName["newer"].Phase != PhaseUnavailable || byName["newer"].Reason != ReasonDuplicateIdentifier {
		t.Fatalf("newer = %s/%s, want unavailable on a duplicate identifier", byName["newer"].Phase, byName["newer"].Reason)
	}
	if got := assessment.Registry.ExtensionCapabilities(); len(got) != 1 || got[0].Version != "v1" {
		t.Fatalf("capabilities = %v, want only the installed version", got)
	}
}

// TestEvaluateMissingDependencyActivatesNothing checks C-28: an unmet edge makes
// the component unavailable, lists the edge and activates no contribution.
func TestEvaluateMissingDependencyActivatesNothing(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Dependencies = Dependencies{
		Capabilities: []CapabilityRequirement{{Name: "demo.other/api", Versions: ">=v2"}},
		Components:   []ComponentRequirement{{Identifier: "demo.base", Versions: "v1"}},
	}
	assessment := Evaluate([]Request{packageRequest(t, "widgets", declaration, time.Now())})
	result := assessment.Components[0]
	if result.Phase != PhaseUnavailable || result.Reason != ReasonDependencyMissing {
		t.Fatalf("result = %s/%s", result.Phase, result.Reason)
	}
	want := []string{"capability demo.other/api >=v2", "component demo.base v1"}
	if !reflect.DeepEqual(result.Missing, want) {
		t.Fatalf("missing = %v, want %v", result.Missing, want)
	}
	if len(result.Contributions.ObjectClasses) != 0 || result.HasMetDependencies {
		t.Fatalf("contributions = %+v", result.Contributions)
	}
	registry := assessment.Registry
	if registry.IsServing("widgets.example.test", "Widget") || len(registry.Pages()) != 0 ||
		len(registry.Settings()) != 0 || len(registry.ExtensionCapabilities()) != 0 ||
		len(registry.CatalogueSources()) != 0 || len(registry.DiagnosticAdapters()) != 0 {
		t.Fatal("a component with an unmet dependency activated a contribution")
	}
}

// TestEvaluateDependencyOrderDoesNotMatter checks the fixpoint: a consumer
// listed before the provider still activates once the provider does.
func TestEvaluateDependencyOrderDoesNotMatter(t *testing.T) {
	provider := Declaration{
		Identifier: "demo.base",
		Version:    "1.0.0",
		Contributions: Contributions{
			Capabilities: []Capability{{Name: "demo.base/api", Version: "v2"}},
		},
	}
	consumer := fullDeclaration("demo.widgets")
	consumer.Dependencies = Dependencies{
		Capabilities: []CapabilityRequirement{{Name: "demo.base/api", Versions: ">=v2"}},
		Components:   []ComponentRequirement{{Identifier: "demo.base"}},
	}
	start := time.Now()
	assessment := Evaluate([]Request{
		packageRequest(t, "widgets", consumer, start),
		packageRequest(t, "base", provider, start.Add(time.Second)),
	})
	byName := indexResults(assessment)
	if byName["widgets"].Phase != PhaseReady || byName["base"].Phase != PhaseReady {
		t.Fatalf("phases: widgets=%s base=%s", byName["widgets"].Phase, byName["base"].Phase)
	}
	if !assessment.Registry.IsServing("widgets.example.test", "Widget") {
		t.Fatal("the consumer's object class is not served")
	}
}

// TestEvaluateDegradesOneClassOnly checks C-28's class isolation: an invalid
// page leaves the object class, the setting and the capability active.
func TestEvaluateDegradesOneClassOnly(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Contributions.Pages = append(declaration.Contributions.Pages,
		Page{ID: "broken", Title: "Broken", RequiredRole: "admin"})
	assessment := Evaluate([]Request{packageRequest(t, "widgets", declaration, time.Now())})
	result := assessment.Components[0]
	if result.Phase != PhaseDegraded || result.Reason != ReasonContributionFailed {
		t.Fatalf("result = %s/%s, want degraded on a failed class", result.Phase, result.Reason)
	}
	if len(result.Problems) != 1 || result.Problems[0].Class != ClassPages {
		t.Fatalf("problems = %v", result.Problems)
	}
	if !assessment.Registry.IsServing("widgets.example.test", "Widget") {
		t.Fatal("a failed page class took the object class down")
	}
	if len(assessment.Registry.Settings()) != 1 || len(assessment.Registry.ExtensionCapabilities()) != 1 {
		t.Fatal("a failed page class took other classes down")
	}
	if _, found := assessment.Registry.PageFor("broken", "Owner"); found {
		t.Fatal("the invalid page is visible")
	}
}

// TestEvaluateKeepsTheFirstClaimOnAClass checks that two components cannot serve
// one class: the later one loses that class and becomes degraded.
func TestEvaluateKeepsTheFirstClaimOnAClass(t *testing.T) {
	first := fullDeclaration("demo.first")
	second := fullDeclaration("demo.second")
	start := time.Now()
	assessment := Evaluate([]Request{
		packageRequest(t, "first", first, start),
		packageRequest(t, "second", second, start.Add(time.Second)),
	})
	byName := indexResults(assessment)
	if byName["first"].Phase != PhaseReady {
		t.Fatalf("first = %s", byName["first"].Phase)
	}
	if byName["second"].Phase != PhaseDegraded {
		t.Fatalf("second = %s, want degraded on the class conflict", byName["second"].Phase)
	}
	if len(byName["second"].Contributions.ObjectClasses) != 0 {
		t.Fatal("the conflicting class stayed with the later component")
	}
	if len(byName["second"].Contributions.Settings) != 1 {
		t.Fatal("the later component lost an unrelated contribution")
	}
	if got := len(assessment.Registry.ObjectClasses()); got != 1 {
		t.Fatalf("served classes = %d, want 1", got)
	}
}

// TestEvaluateAdoptedContributesNothing checks C-17: an adopted component
// installs no package and contributes no object class, page or controller.
func TestEvaluateAdoptedContributesNothing(t *testing.T) {
	assessment := Evaluate([]Request{{Name: "adopted", IsAdopted: true}})
	result := assessment.Components[0]
	if result.Phase != PhaseReady || result.Reason != ReasonAdopted {
		t.Fatalf("result = %s/%s", result.Phase, result.Reason)
	}
	if len(assessment.Registry.ObjectClasses()) != 0 || len(assessment.Registry.Pages()) != 0 ||
		len(assessment.Registry.Controllers()) != 0 {
		t.Fatal("an adopted component contributed an installed-only class")
	}
}

// TestStatusForIsStableAcrossRounds checks that re-evaluating an unchanged
// component produces the same status document (C-06).
func TestStatusForIsStableAcrossRounds(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Dependencies = Dependencies{Capabilities: []CapabilityRequirement{{Name: "absent/api", Versions: "v1"}}}
	assessment := Evaluate([]Request{packageRequest(t, "widgets", declaration, time.Now())})
	first := StatusFor(assessment.Components[0], nil)
	second := StatusFor(assessment.Components[0], first.Conditions)
	if !sameJSON(mustDocument(t, first), mustDocument(t, second)) {
		t.Fatalf("status changed between identical rounds:\n%v\n%v", mustDocument(t, first), mustDocument(t, second))
	}
	if second.Phase != PhaseUnavailable || second.Health.State != "Unavailable" {
		t.Fatalf("status = %+v", second)
	}
}

// indexResults maps component names to their results.
func indexResults(assessment Assessment) map[string]ComponentResult {
	byName := make(map[string]ComponentResult, len(assessment.Components))
	for _, result := range assessment.Components {
		byName[result.Name] = result
	}
	return byName
}

// mustDocument renders one status for comparison.
func mustDocument(t *testing.T, status v1beta1.ObjectStatus) map[string]any {
	t.Helper()
	document, err := statusDocument(status)
	if err != nil {
		t.Fatalf("render status: %v", err)
	}
	return document
}
