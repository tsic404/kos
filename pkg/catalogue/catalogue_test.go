package catalogue

import (
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
)

// template renders one ConfigMap; the catalogue only checks that a translation
// template exists.
const template = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Name }}\n"

// definition builds one valid definition and returns it with its digest.
func definition(t *testing.T, requires []v1beta1.CapabilityRequirement) v1beta1.ApplicationDefinitionSpec {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Template: template,
		Requires: requires,
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	spec.Package.Digest = digest
	return spec
}

// tampered builds one definition whose content no longer matches its digest.
func tampered(t *testing.T) v1beta1.ApplicationDefinitionSpec {
	t.Helper()
	spec := definition(t, nil)
	spec.Template = template + "data:\n  extra: \"x\"\n"
	return spec
}

// TestJudgeEntryReportsWhyAnEntryCannotBeInstalled checks the two refusals C-24
// names and the cause each one carries: a package that does not match its digest
// and a missing dependency (C-24, C-12).
func TestJudgeEntryReportsWhyAnEntryCannotBeInstalled(t *testing.T) {
	available := []app.Capability{{Name: "storage.volume", Version: "v1"}}

	valid := definition(t, nil)
	entry, cause := judgeEntry("demo", offer("ok", valid), available)
	if !entry.Installable || entry.Reason != "" || cause != "" {
		t.Fatalf("valid entry = %+v (cause %q), want installable", entry, cause)
	}

	entry, cause = judgeEntry("demo", offer("tampered", tampered(t)), available)
	if entry.Installable || !strings.Contains(entry.Reason, "digest") {
		t.Fatalf("tampered entry = %+v, want the digest refusal", entry)
	}
	if cause != controlplane.CatalogueRefusalPackage {
		t.Fatalf("cause = %q, want the package refusal", cause)
	}

	missing := definition(t, []v1beta1.CapabilityRequirement{{Name: "compute.gpu", Version: "v1"}})
	entry, cause = judgeEntry("demo", offer("needs-gpu", missing), available)
	if entry.Installable || !strings.Contains(entry.Reason, "compute.gpu") {
		t.Fatalf("entry with a missing dependency = %+v, want the dependency named", entry)
	}
	if cause != controlplane.CatalogueRefusalCapability {
		t.Fatalf("cause = %q, want the capability refusal", cause)
	}

	optional := definition(t, []v1beta1.CapabilityRequirement{{Name: "compute.gpu", Version: "v1", Optional: true}})
	entry, cause = judgeEntry("demo", offer("optional-gpu", optional), available)
	if !entry.Installable || cause != "" {
		t.Fatalf("entry with an optional missing dependency = %+v (cause %q), want installable", entry, cause)
	}
}

// TestEntriesCarryTheDeclaredStructure checks every field C-24 requires of an
// entry.
func TestEntriesCarryTheDeclaredStructure(t *testing.T) {
	spec := definition(t, []v1beta1.CapabilityRequirement{{Name: "storage.volume", Version: "v1"}})
	declarations := fixedDeclarations{[]extension.CatalogueSource{{
		ID: "demo",
		Entries: []extension.CatalogueEntry{{
			ID:         "blog",
			Display:    extension.CatalogueDisplay{Name: "Blog", Description: "notes", Icon: "blog"},
			Definition: spec,
		}},
	}}}
	service := New(declarations, nil)

	entries := service.Entries()
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	entry := entries[0]
	if entry.Source != "demo" || entry.ID != "blog" || entry.Name != "Blog" ||
		entry.Description != "notes" || entry.Version != "1.0.0" ||
		entry.Digest != spec.Package.Digest || entry.Definition != "blog" ||
		len(entry.Dependencies) != 1 || !entry.Installable {
		t.Fatalf("entry = %+v", entry)
	}
}

// TestJudgeInstallReportsAnEntryThatCannotBeInstalled checks the judgement the
// write path reads before anything is written (C-24, C-31): a missing
// capability is reported with its name and cause, a package that no longer
// matches its digest reports the package cause, and a definition no source
// offers is not a catalogue install at all.
func TestJudgeInstallReportsAnEntryThatCannotBeInstalled(t *testing.T) {
	declarations := fixedDeclarations{[]extension.CatalogueSource{{
		ID: "demo",
		Entries: []extension.CatalogueEntry{
			offer("needs-gpu", definition(t, []v1beta1.CapabilityRequirement{{Name: "compute.gpu", Version: "v1"}})),
			offer("tampered", tampered(t)),
			offer("ok", definition(t, nil)),
		},
	}}}
	service := New(declarations, nil)

	verdict, found := service.JudgeInstall("needs-gpu")
	if !found || verdict.Installable {
		t.Fatalf("verdict = %+v (found %v), want a refused install", verdict, found)
	}
	if len(verdict.Missing) != 1 || verdict.Missing[0] != "compute.gpu" {
		t.Fatalf("missing = %v, want compute.gpu", verdict.Missing)
	}
	if !strings.Contains(verdict.Reason, "compute.gpu") {
		t.Fatalf("reason = %q, want the capability named", verdict.Reason)
	}
	if verdict.Cause != controlplane.CatalogueRefusalCapability {
		t.Fatalf("cause = %q, want the capability refusal", verdict.Cause)
	}

	verdict, found = service.JudgeInstall("tampered")
	if !found || verdict.Installable {
		t.Fatalf("verdict = %+v (found %v), want a refused install", verdict, found)
	}
	if verdict.Cause != controlplane.CatalogueRefusalPackage {
		t.Fatalf("cause = %q, want the package refusal, not a missing capability", verdict.Cause)
	}
	if len(verdict.Missing) != 0 {
		t.Fatalf("missing = %v, want no missing capability for a broken package", verdict.Missing)
	}

	verdict, found = service.JudgeInstall("ok")
	if !found || !verdict.Installable || verdict.Reason != "" || verdict.Cause != "" {
		t.Fatalf("verdict = %+v (found %v), want an installable entry", verdict, found)
	}

	if _, found := service.JudgeInstall("absent"); found {
		t.Fatal("a definition no source offers was judged as a catalogue entry")
	}
}

// TestCapabilityScopeTravelsWithTheRegistration checks that a cluster-wide
// requirement is served by a cluster-wide registration (C-11): dropping the
// scope would report a satisfied dependency as missing and refuse the install.
func TestCapabilityScopeTravelsWithTheRegistration(t *testing.T) {
	available := catalogueCapabilities([]extension.Capability{{
		Name: "network.http-endpoint", Version: "v1", Scope: v1beta1.CapabilityScopeCluster,
	}})
	result, err := app.Compare([]v1beta1.CapabilityRequirement{{
		Name: "network.http-endpoint", Version: "v1", Scope: v1beta1.CapabilityScopeCluster,
	}}, available)
	if err != nil {
		t.Fatalf("compare the requirement: %v", err)
	}
	if len(result.Missing) != 0 || len(result.Unsatisfied) != 0 {
		t.Fatalf("result = %+v, want the cluster-wide registration to satisfy it", result)
	}
}

// fixedDeclarations is a declaration set with a fixed source list.
type fixedDeclarations struct {
	sources []extension.CatalogueSource
}

// CatalogueSources implements Declarations.
func (d fixedDeclarations) CatalogueSources() []extension.CatalogueSource {
	return d.sources
}

// Capabilities implements Declarations.
func (fixedDeclarations) Capabilities() []extension.Capability {
	return []extension.Capability{{Name: "storage.volume", Version: "v1"}}
}

// offer builds one declared entry around a definition.
func offer(id string, spec v1beta1.ApplicationDefinitionSpec) extension.CatalogueEntry {
	return extension.CatalogueEntry{
		ID:         id,
		Display:    extension.CatalogueDisplay{Name: id},
		Definition: spec,
	}
}
