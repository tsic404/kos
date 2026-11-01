package appstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/catalogue"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// template renders one ConfigMap; the catalogue only requires a template.
const template = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Name }}\n"

// declarations is a fixed contribution projection.
type declarations struct {
	sources []extension.CatalogueSource
}

// CatalogueSources implements catalogue.Declarations.
func (d declarations) CatalogueSources() []extension.CatalogueSource { return d.sources }

// Capabilities implements catalogue.Declarations.
func (d declarations) Capabilities() []extension.Capability { return nil }

// definitionWith builds one valid definition pinned to its content digest.
func definitionWith(t *testing.T, structure []v1beta1.ConfigField, requires []v1beta1.CapabilityRequirement) v1beta1.ApplicationDefinitionSpec {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Config:   structure,
		Template: template,
		Requires: requires,
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest the definition: %v", err)
	}
	spec.Package.Digest = digest
	if err := app.ValidateDefinition(spec); err != nil {
		t.Fatalf("the definition is invalid: %v", err)
	}
	return spec
}

// entriesOf reads the app-center entries item the way the shell receives it.
func entriesOf(t *testing.T, reader Readings) []map[string]any {
	t.Helper()
	reading, failure := reader.Read(context.Background(), identity.Session{Role: write.RoleMember},
		extension.PageItem{ID: EntriesItem, Reference: EntriesRef})
	if failure != nil {
		t.Fatalf("read the entries item: %v", failure)
	}
	encoded, err := json.Marshal(reading.Value)
	if err != nil {
		t.Fatalf("encode the entries value: %v", err)
	}
	var offers []map[string]any
	if err := json.Unmarshal(encoded, &offers); err != nil {
		t.Fatalf("decode the entries value: %v", err)
	}
	return offers
}

// offerNamed returns one rendered entry by identity.
func offerNamed(t *testing.T, offers []map[string]any, id string) map[string]any {
	t.Helper()
	for _, offer := range offers {
		if name, _ := offer["id"].(string); name == id {
			return offer
		}
	}
	t.Fatalf("the entries item carries no entry %q: %v", id, offers)
	return nil
}

// fieldsOf returns the generated form fields of one rendered entry by name.
func fieldsOf(t *testing.T, offer map[string]any) map[string]map[string]any {
	t.Helper()
	listed, ok := offer["config"].([]any)
	if !ok {
		t.Fatalf("entry %v carries no field list", offer["id"])
	}
	fields := make(map[string]map[string]any, len(listed))
	for _, raw := range listed {
		field, _ := raw.(map[string]any)
		name, _ := field["name"].(string)
		fields[name] = field
	}
	return fields
}

// TestAppCenterLaysOutTheInstallFormFromTheStructure checks the entries item the
// interface installs from: every entry carries the fields its config structure
// generates, and an entry that cannot be installed carries the same refusal the
// write path answers with (C-31, C-32).
func TestAppCenterLaysOutTheInstallFormFromTheStructure(t *testing.T) {
	structure := []v1beta1.ConfigField{
		{Name: "tier", Type: app.TypeEnum, Required: true, Enum: []string{"small", "large"}, Description: "运行规格"},
		{Name: "replicas", Type: app.TypeInteger, Minimum: new("1"), Maximum: new("4")},
		{Name: "owner", Type: app.TypeString, Sensitive: true},
	}
	reader := NewReadings(catalogue.New(declarations{sources: []extension.CatalogueSource{{
		ID:   SourceID,
		Sync: SyncPull,
		Entries: []extension.CatalogueEntry{
			{ID: "wiki", Display: extension.CatalogueDisplay{Name: "Wiki"}, Definition: definitionWith(t, structure, nil)},
			{ID: "gpu", Display: extension.CatalogueDisplay{Name: "GPU 任务"},
				Definition: definitionWith(t, structure, []v1beta1.CapabilityRequirement{{Name: "compute.gpu", Version: "v1"}})},
		},
	}}}, nil), nil)

	offers := entriesOf(t, reader)
	if len(offers) != 2 {
		t.Fatalf("entries = %v, want the two offered entries", offers)
	}

	fields := fieldsOf(t, offerNamed(t, offers, "wiki"))
	if len(fields) != len(structure) {
		t.Fatalf("fields = %v, want one per declared field", fields)
	}
	if tier := fields["tier"]; tier["control"] != "select" || tier["required"] != true {
		t.Fatalf("tier = %v, want a required select", tier)
	}
	if accepted, _ := fields["tier"]["enum"].([]any); len(accepted) != 2 {
		t.Fatalf("tier = %v, want the accepted values", fields["tier"])
	}
	if replicas := fields["replicas"]; replicas["control"] != "number" || replicas["maximum"] != "4" {
		t.Fatalf("replicas = %v, want a bounded number", replicas)
	}
	if owner := fields["owner"]; owner["sensitive"] != true {
		t.Fatalf("owner = %v, want the sensitive mark so the shell renders it write-only", owner)
	}
	for name, field := range fields {
		if _, carried := field["value"]; carried {
			t.Fatalf("field %s carries a value; an offer shows the structure, never a parameter", name)
		}
	}

	refused := offerNamed(t, offers, "gpu")
	if installable, _ := refused["installable"].(bool); installable {
		t.Fatalf("entry = %v, want not installable while its capability is absent", refused)
	}
	missing, _ := refused["missing"].([]any)
	if len(missing) != 1 || missing[0] != "compute.gpu" {
		t.Fatalf("missing = %v, want the absent capability listed", refused["missing"])
	}
	if reason, _ := refused["reason"].(string); !strings.Contains(reason, "compute.gpu") {
		t.Fatalf("reason = %q, want the missing capability named", reason)
	}
	if len(fieldsOf(t, refused)) != len(structure) {
		t.Fatalf("a refused entry lost its form: %v", refused)
	}
}

// TestAppCenterShowsAnEmptyEntryListWithoutSources checks the empty state: with
// no active source the page read answers with an empty list, not a null value.
func TestAppCenterShowsAnEmptyEntryListWithoutSources(t *testing.T) {
	offers := entriesOf(t, NewReadings(catalogue.New(declarations{}, nil), nil))
	if offers == nil || len(offers) != 0 {
		t.Fatalf("entries = %v, want an empty list", offers)
	}
}
