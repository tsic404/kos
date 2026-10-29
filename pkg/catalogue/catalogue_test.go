package catalogue

import (
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
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

// TestJudgeEntryReportsWhyAnEntryCannotBeInstalled checks the two refusals C-24
// names: a package that does not match its digest and a missing dependency.
func TestJudgeEntryReportsWhyAnEntryCannotBeInstalled(t *testing.T) {
	available := []app.Capability{{Name: "storage.volume", Version: "v1"}}

	valid := definition(t, nil)
	entry := judgeEntry("demo", offer("ok", valid), available)
	if !entry.Installable || entry.Reason != "" {
		t.Fatalf("valid entry = %+v, want installable", entry)
	}

	mismatched := definition(t, nil)
	mismatched.Template = template + "data:\n  extra: \"x\"\n"
	entry = judgeEntry("demo", offer("tampered", mismatched), available)
	if entry.Installable || !strings.Contains(entry.Reason, "digest") {
		t.Fatalf("tampered entry = %+v, want the digest refusal", entry)
	}

	missing := definition(t, []v1beta1.CapabilityRequirement{{Name: "compute.gpu", Version: "v1"}})
	entry = judgeEntry("demo", offer("needs-gpu", missing), available)
	if entry.Installable || !strings.Contains(entry.Reason, "compute.gpu") {
		t.Fatalf("entry with a missing dependency = %+v, want the dependency named", entry)
	}

	optional := definition(t, []v1beta1.CapabilityRequirement{{Name: "compute.gpu", Version: "v1", Optional: true}})
	entry = judgeEntry("demo", offer("optional-gpu", optional), available)
	if !entry.Installable {
		t.Fatalf("entry with an optional missing dependency = %+v, want installable", entry)
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
