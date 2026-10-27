package provider

import (
	"context"
	"reflect"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/conformance"
)

// TestRegistrationProblems pins the rules C-09 and C-47 put on a registration.
func TestRegistrationProblems(t *testing.T) {
	durable := true
	valid := func() v1beta1.ProviderSpec {
		return v1beta1.ProviderSpec{
			ComponentRef:   "builtin-storage",
			Version:        "1.0.0",
			StorageClasses: []string{"local-path"},
			Capabilities: []v1beta1.ProviderCapability{{
				Name:          "storage.volume",
				Version:       "v1",
				Scope:         v1beta1.CapabilityScopeNode,
				Parameters:    []v1beta1.CapabilityParameter{{Name: "replicas", Type: "integer"}},
				Guarantees:    []v1beta1.CapabilityGuarantee{{Key: "durable", Bool: &durable}},
				CaseSet:       "kos/conformance/storage-volume-v1",
				CaseSetDigest: "digest",
			}},
		}
	}

	cases := []struct {
		name     string
		previous []v1beta1.ProviderCapability
		mutate   func(*v1beta1.ProviderSpec)
		refused  bool
	}{
		{
			name:   "a complete registration is accepted",
			mutate: func(*v1beta1.ProviderSpec) {},
		},
		{
			// C-47: a provider without a storage capability may declare an
			// empty storage class set.
			name: "an empty storage class set is accepted without a storage capability",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Name = "builtin-storage/queue"
				spec.StorageClasses = nil
			},
		},
		{
			name: "a storage capability needs a storage class",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.StorageClasses = nil
			},
			refused: true,
		},
		{
			name: "a capability names no case set",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].CaseSet = ""
			},
			refused: true,
		},
		{
			name: "a capability names no case set digest",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].CaseSetDigest = ""
			},
			refused: true,
		},
		{
			name: "the same capability version is registered twice",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities = append(spec.Capabilities, spec.Capabilities[0])
			},
			refused: true,
		},
		{
			// C-09: one provider may register two versions of a capability at
			// the same time.
			name: "two versions of one capability are accepted",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities = append(spec.Capabilities, v1beta1.ProviderCapability{
					Name:          "storage.volume",
					Version:       "v2",
					Scope:         v1beta1.CapabilityScopeNode,
					CaseSet:       "kos/conformance/storage-volume-v1",
					CaseSetDigest: "digest",
				})
			},
		},
		{
			// C-09: a published capability version keeps what it promises; a
			// weaker guarantee needs a new version.
			name: "a published capability changes its guarantees",
			previous: []v1beta1.ProviderCapability{{
				Name:       "storage.volume",
				Version:    "v1",
				Scope:      v1beta1.CapabilityScopeNode,
				Parameters: []v1beta1.CapabilityParameter{{Name: "replicas", Type: "integer"}},
				Guarantees: []v1beta1.CapabilityGuarantee{{Key: "durable", Bool: &durable}},
				CaseSet:    "kos/conformance/storage-volume-v1",
			}},
			mutate: func(spec *v1beta1.ProviderSpec) {
				weaker := false
				spec.Capabilities[0].Guarantees = []v1beta1.CapabilityGuarantee{{Key: "durable", Bool: &weaker}}
			},
			refused: true,
		},
		{
			name: "a published capability changes its parameters",
			previous: []v1beta1.ProviderCapability{{
				Name:       "storage.volume",
				Version:    "v1",
				Scope:      v1beta1.CapabilityScopeNode,
				Parameters: []v1beta1.CapabilityParameter{{Name: "replicas", Type: "integer"}},
				Guarantees: []v1beta1.CapabilityGuarantee{{Key: "durable", Bool: &durable}},
				CaseSet:    "kos/conformance/storage-volume-v1",
			}},
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Parameters = []v1beta1.CapabilityParameter{{Name: "replicas", Type: "string"}}
			},
			refused: true,
		},
		{
			// C-09: the scope is part of what a published version means; a
			// different scope needs a new version.
			name: "a published capability changes its scope",
			previous: []v1beta1.ProviderCapability{{
				Name:       "storage.volume",
				Version:    "v1",
				Scope:      v1beta1.CapabilityScopeCluster,
				Parameters: []v1beta1.CapabilityParameter{{Name: "replicas", Type: "integer"}},
				Guarantees: []v1beta1.CapabilityGuarantee{{Key: "durable", Bool: &durable}},
				CaseSet:    "kos/conformance/storage-volume-v1",
			}},
			mutate:  func(*v1beta1.ProviderSpec) {},
			refused: true,
		},
		{
			// The registration path fills a baseline capability's scope, so a
			// registration that still carries none is refused (C-09).
			name: "a capability names no scope",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Scope = ""
			},
			refused: true,
		},
		{
			name: "a scope outside the two values is refused",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Scope = "edge"
			},
			refused: true,
		},
		{
			name: "an explicitly cluster-level scope is accepted",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Scope = v1beta1.CapabilityScopeCluster
			},
		},
		{
			// Declaration order is not part of the promise.
			name: "a published capability keeps its meaning when reordered",
			previous: []v1beta1.ProviderCapability{{
				Name:    "storage.volume",
				Version: "v1",
				Scope:   v1beta1.CapabilityScopeNode,
				Parameters: []v1beta1.CapabilityParameter{
					{Name: "replicas", Type: "integer"},
					{Name: "mode", Type: "enum", Enum: []string{"a", "b"}},
				},
				Guarantees: []v1beta1.CapabilityGuarantee{
					{Key: "durable", Bool: &durable},
					{Key: "tier", Enum: "fast"},
				},
				CaseSet: "kos/conformance/storage-volume-v1",
			}},
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Parameters = []v1beta1.CapabilityParameter{
					{Name: "mode", Type: "enum", Enum: []string{"b", "a"}},
					{Name: "replicas", Type: "integer"},
				}
				spec.Capabilities[0].Guarantees = []v1beta1.CapabilityGuarantee{
					{Key: "tier", Enum: "fast"},
					{Key: "durable", Bool: &durable},
				}
			},
		},
		{
			name: "a guarantee carries prose",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Guarantees = []v1beta1.CapabilityGuarantee{{Key: "fast"}}
			},
			refused: true,
		},
		{
			name: "a guarantee is both boolean and enum",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Guarantees = []v1beta1.CapabilityGuarantee{{Key: "fast", Bool: &durable, Enum: "fast"}}
			},
			refused: true,
		},
		{
			name: "an enum parameter lists no value",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Parameters = []v1beta1.CapabilityParameter{{Name: "mode", Type: "enum"}}
			},
			refused: true,
		},
		{
			name: "a string parameter carries enum values",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.Capabilities[0].Parameters = []v1beta1.CapabilityParameter{{Name: "mode", Type: "string", Enum: []string{"a"}}}
			},
			refused: true,
		},
		{
			name: "the registration names no component",
			mutate: func(spec *v1beta1.ProviderSpec) {
				spec.ComponentRef = ""
			},
			refused: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			spec := valid()
			test.mutate(&spec)
			problems := registrationProblems(spec, test.previous)
			if test.refused && len(problems) == 0 {
				t.Fatal("the registration was accepted")
			}
			if !test.refused && len(problems) > 0 {
				t.Fatalf("the registration was refused: %v", problems)
			}
		})
	}
}

// TestWithdrawnFromKeepsTheOldCapability checks the withdrawal detection C-48
// depends on: a capability absent from the new registration is withdrawn.
func TestWithdrawnFromKeepsTheOldCapability(t *testing.T) {
	previous := []v1beta1.ProviderCapability{
		{Name: "storage.volume", Version: "v1"},
		{Name: "storage.snapshot", Version: "v1"},
	}
	current := []v1beta1.ProviderCapability{{Name: "storage.snapshot", Version: "v1"}}
	withdrawn := withdrawnFrom(previous, current)
	if len(withdrawn) != 1 || withdrawn[0].Name != "storage.volume" {
		t.Fatalf("withdrawn = %v, want storage.volume", withdrawn)
	}
}

// TestCapabilityResultsReusesARound pins the resync rule: a round with an
// unchanged generation and set reuses the previous outcomes instead of
// replaying side-effecting cases, and a generation change runs them again.
func TestCapabilityResultsReusesARound(t *testing.T) {
	distribution := conformance.Standard()
	digest, found := distribution.Digest("kos/conformance/storage-volume-v1")
	if !found {
		t.Fatal("the distribution ships no storage.volume case set")
	}
	cases := 0
	target := conformance.TargetFunc(func(context.Context, string, string, string) error {
		cases++
		return nil
	})
	reconciler := NewReconciler(nil, Options{
		Distribution: distribution,
		Targets:      conformance.MapTargets{"acme-storage": target},
	})
	effective := []v1beta1.ProviderCapability{{
		Name:          "storage.volume",
		Version:       "v1",
		CaseSet:       "kos/conformance/storage-volume-v1",
		CaseSetDigest: digest,
	}}
	spec := v1beta1.ProviderSpec{Capabilities: effective}

	first := reconciler.capabilityResults(context.Background(), "acme-storage", "uid-1", 1, spec, effective)
	if cases != 4 {
		t.Fatalf("the first round ran %d cases, want 4", cases)
	}
	second := reconciler.capabilityResults(context.Background(), "acme-storage", "uid-1", 1, spec, effective)
	if cases != 4 {
		t.Fatalf("an unchanged round replayed %d cases", cases-4)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("the reused round = %+v, want %+v", second, first)
	}

	reconciler.capabilityResults(context.Background(), "acme-storage", "uid-1", 2, spec, effective)
	if cases != 8 {
		t.Fatalf("a new generation ran %d cases, want 4", cases-4)
	}
}

// TestCapabilityResultsDoesNotCacheAnInterruptedRound keeps a budget stop from
// becoming a conclusion: the next round runs the cases again.
func TestCapabilityResultsDoesNotCacheAnInterruptedRound(t *testing.T) {
	distribution := conformance.Standard()
	digest, found := distribution.Digest("kos/conformance/storage-volume-v1")
	if !found {
		t.Fatal("the distribution ships no storage.volume case set")
	}
	reconciler := NewReconciler(nil, Options{
		Distribution: distribution,
		Targets: conformance.MapTargets{"acme-storage": conformance.TargetFunc(
			func(context.Context, string, string, string) error { return nil })},
	})
	effective := []v1beta1.ProviderCapability{{
		Name:          "storage.volume",
		Version:       "v1",
		CaseSet:       "kos/conformance/storage-volume-v1",
		CaseSetDigest: digest,
	}}
	spec := v1beta1.ProviderSpec{Capabilities: effective}

	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	interrupted := reconciler.capabilityResults(stopped, "acme-storage", "uid-1", 1, spec, effective)
	if len(interrupted) != 1 || interrupted[0].Reason != conformance.ReasonInterrupted {
		t.Fatalf("the stopped round = %+v, want one interrupted result", interrupted)
	}
	verified := reconciler.capabilityResults(context.Background(), "acme-storage", "uid-1", 1, spec, effective)
	if len(verified) != 1 || verified[0].State != conformance.StateVerified {
		t.Fatalf("the next round = %+v, want a verified result", verified)
	}
}

// TestCapabilityResultsRunsAgainForANewObject keeps a replacement provider from
// inheriting the previous object's conclusion: a re-created provider reuses its
// name and starts its generation counter again, so only the object identity and
// the registration inputs can tell the two rounds apart (C-10).
func TestCapabilityResultsRunsAgainForANewObject(t *testing.T) {
	distribution := conformance.Standard()
	digest, found := distribution.Digest("kos/conformance/storage-volume-v1")
	if !found {
		t.Fatal("the distribution ships no storage.volume case set")
	}
	cases := 0
	target := conformance.TargetFunc(func(context.Context, string, string, string) error {
		cases++
		return nil
	})
	reconciler := NewReconciler(nil, Options{
		Distribution: distribution,
		Targets:      conformance.MapTargets{"acme-storage": target},
	})
	effective := []v1beta1.ProviderCapability{{
		Name:          "storage.volume",
		Version:       "v1",
		CaseSet:       "kos/conformance/storage-volume-v1",
		CaseSetDigest: digest,
	}}
	spec := v1beta1.ProviderSpec{ComponentRef: "acme-storage", Version: "1.0.0", Capabilities: effective}

	reconciler.capabilityResults(context.Background(), "acme-storage", "uid-1", 1, spec, effective)
	if cases != 4 {
		t.Fatalf("the first round ran %d cases, want 4", cases)
	}

	// Same name, same generation, different object: the cases run again and
	// the result is the new object's own KOS run.
	results := reconciler.capabilityResults(context.Background(), "acme-storage", "uid-2", 1, spec, effective)
	if cases != 8 {
		t.Fatalf("the re-created provider ran %d cases, want 4 of its own", cases-4)
	}
	if len(results) != 1 || results[0].Source != conformance.SourceKOS {
		t.Fatalf("the re-created provider's result = %+v, want a KOS run", results)
	}
	if revisited := reconciler.capabilityResults(context.Background(), "acme-storage", "uid-2", 1, spec, effective); cases != 8 || !reflect.DeepEqual(revisited, results) {
		t.Fatalf("an unchanged round of the new object replayed %d cases", cases-8)
	}

	// A registration input change invalidates the round even when the
	// generation stands still.
	spec.Version = "1.0.1"
	reconciler.capabilityResults(context.Background(), "acme-storage", "uid-2", 1, spec, effective)
	if cases != 12 {
		t.Fatalf("a changed declaration ran %d cases, want 4", cases-8)
	}

	// A removed object takes its round with it, so the next object of the same
	// name never answers from it.
	reconciler.forget("acme-storage")
	reconciler.capabilityResults(context.Background(), "acme-storage", "uid-2", 1, spec, effective)
	if cases != 16 {
		t.Fatalf("a forgotten round ran %d cases, want 4", cases-12)
	}
}
