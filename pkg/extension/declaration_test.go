package extension

import (
	"strings"
	"testing"
)

// fullDeclaration is a declaration that uses every contribution class.
func fullDeclaration(identifier string) Declaration {
	return Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		APIGroup:   "widgets.example.test",
		Contributions: Contributions{
			ObjectClasses: []ObjectClass{{
				Group: "widgets.example.test", Kind: "Widget", Resource: "widgets",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []Controller{{
				ID:      "widget-controller",
				Manages: []GroupKind{{Group: "widgets.example.test", Kind: "Widget"}},
				Scope: WriteScope{
					Groups:     []string{"widgets.example.test"},
					Kinds:      []GroupKind{{Kind: "Deployment"}},
					Namespaces: []string{"default"},
				},
			}},
			Capabilities:       []Capability{{Name: identifier + "/widgets", Version: "v1"}},
			Pages:              []Page{{ID: "widgets", Title: "Widgets", Actions: []Action{{ID: "create"}}}},
			Settings:           []Setting{{ID: "widget-endpoint", Scope: SettingScopeCluster, Writer: SettingWriterUser, Editable: true}},
			CatalogueSources:   []CatalogueSource{{ID: "widgets", Sync: "pull"}},
			DiscoveryAdapters:  []DiscoveryAdapter{{ID: "widget-discovery", Covers: []GroupKind{{Kind: "Node"}}}},
			DiagnosticAdapters: []DiagnosticAdapter{{ID: "widget-diagnostics", Covers: []GroupKind{{Group: "widgets.example.test", Kind: "Widget"}}}},
			Preferences:        []PreferenceSchema{{ID: "widget-view", Keys: []string{"view", "columns"}}},
		},
	}
}

// TestLoadKeepsEveryContributionClass checks that one valid entry of each class
// survives loading (C-18).
func TestLoadKeepsEveryContributionClass(t *testing.T) {
	loaded, err := fullDeclaration("demo.widgets").Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("valid declaration has problems: %v", loaded.Problems)
	}
	contributions := loaded.Contributions
	counts := map[string]int{
		ClassObjectClasses:      len(contributions.ObjectClasses),
		ClassControllers:        len(contributions.Controllers),
		ClassCapabilities:       len(contributions.Capabilities),
		ClassPages:              len(contributions.Pages),
		ClassSettings:           len(contributions.Settings),
		ClassCatalogueSources:   len(contributions.CatalogueSources),
		ClassDiscoveryAdapters:  len(contributions.DiscoveryAdapters),
		ClassDiagnosticAdapters: len(contributions.DiagnosticAdapters),
		ClassPreferences:        len(contributions.Preferences),
	}
	for class, count := range counts {
		if count != 1 {
			t.Errorf("%s kept %d entries, want 1", class, count)
		}
	}
	if got := contributions.ObjectClasses[0].GroupKind().String(); got != "widgets.example.test/Widget" {
		t.Fatalf("object class = %s", got)
	}
}

// TestLoadFailsOneClassWithoutTakingOthersDown checks C-28's class isolation:
// invalid entries are dropped by class, the rest stays loadable.
func TestLoadFailsOneClassWithoutTakingOthersDown(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Contributions.ObjectClasses = append(declaration.Contributions.ObjectClasses, ObjectClass{
		Group: "kos.tsic.top", Kind: "Secret", Resource: "secrets", Versions: []string{"v1"},
	})
	declaration.Contributions.Capabilities = append(declaration.Contributions.Capabilities,
		Capability{Name: "unprefixed", Version: "v1"})
	declaration.Contributions.Pages = append(declaration.Contributions.Pages,
		Page{ID: "bad-role", Title: "Bad", RequiredRole: "admin"})
	declaration.Contributions.Settings = append(declaration.Contributions.Settings,
		Setting{ID: "bad-scope", Scope: "galaxy", Writer: SettingWriterUser})

	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	problems := map[string]bool{}
	for _, problem := range loaded.Problems {
		problems[problem.Class] = true
	}
	for _, class := range []string{ClassObjectClasses, ClassCapabilities, ClassPages, ClassSettings} {
		if !problems[class] {
			t.Errorf("%s did not report its invalid entry: %v", class, loaded.Problems)
		}
	}
	if len(loaded.Contributions.ObjectClasses) != 1 || len(loaded.Contributions.Pages) != 1 ||
		len(loaded.Contributions.Settings) != 1 || len(loaded.Contributions.Capabilities) != 1 {
		t.Fatalf("valid entries were dropped with the invalid ones: %+v", loaded.Contributions)
	}
}

// TestLoadRejectsKernelRedefinition checks that an object class may not take the
// kernel group or a kernel kind (C-19).
func TestLoadRejectsKernelRedefinition(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Contributions.ObjectClasses = []ObjectClass{{Group: "kos.tsic.top", Kind: "Application", Resource: "applications", Versions: []string{"v1"}}}
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Contributions.ObjectClasses) != 0 {
		t.Fatal("a kernel class was accepted as an extension class")
	}
	if len(loaded.Problems) == 0 || loaded.Problems[0].Class != ClassObjectClasses {
		t.Fatalf("problems = %v", loaded.Problems)
	}
}

// TestLoadBindsObjectClassesToTheDeclaredAPIGroup checks that a class outside
// the extension's own group is refused, so a package cannot have the control
// plane write another group's resources with its credentials (C-19).
func TestLoadBindsObjectClassesToTheDeclaredAPIGroup(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Contributions.ObjectClasses = append(declaration.Contributions.ObjectClasses, ObjectClass{
		Group: "apps", Kind: "Deployment", Resource: "deployments", Versions: []string{"v1"},
	})
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	classes := loaded.Contributions.ObjectClasses
	if len(classes) != 1 || classes[0].Group != declaration.APIGroup {
		t.Fatalf("kept classes = %v, want only the declared group", classes)
	}
	if len(loaded.Problems) == 0 || loaded.Problems[0].Class != ClassObjectClasses {
		t.Fatalf("problems = %v", loaded.Problems)
	}
	if !strings.Contains(strings.Join(loaded.Problems[0].Items, "; "), declaration.APIGroup) {
		t.Fatalf("problem does not name the declared group: %v", loaded.Problems[0].Items)
	}
}

// TestLoadRejectsTheKernelAPIGroup checks that an extension never declares
// itself as the kernel group (C-19).
func TestLoadRejectsTheKernelAPIGroup(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.APIGroup = "kos.tsic.top"
	if _, err := declaration.Load(); err == nil || !strings.Contains(err.Error(), "kernel group") {
		t.Fatalf("kernel apiGroup error = %v", err)
	}
}

// TestLoadRefusesClassesWithoutAnAPIGroup keeps a declaration from serving a
// class whose group it never declared (C-19).
func TestLoadRefusesClassesWithoutAnAPIGroup(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.APIGroup = ""
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Contributions.ObjectClasses) != 0 {
		t.Fatalf("classes loaded without an apiGroup: %v", loaded.Contributions.ObjectClasses)
	}
	if len(loaded.Problems) == 0 || loaded.Problems[0].Class != ClassObjectClasses {
		t.Fatalf("problems = %v", loaded.Problems)
	}
}

// TestParseCarriesTheDeclarationItems parses a package that names the read
// scope, the configuration structure and the retention semantics: the carriers
// must survive parsing instead of being rejected as unknown fields (C-18).
func TestParseCarriesTheDeclarationItems(t *testing.T) {
	content := []byte(`{
		"identifier": "demo.carriers",
		"version": "1.0.0",
		"apiGroup": "carriers.example.test",
		"permissions": [{
			"contribution": "overview",
			"readScope": {"groups": ["metrics.example.test"], "kinds": [{"kind": "Node"}]},
			"requiredRole": "Member",
			"objectWriters": ["Owner", "Member"]
		}],
		"configuration": {
			"schema": {"type": "object"},
			"statusWriters": ["Owner"],
			"conditions": ["Reconciled"]
		},
		"retention": [{"resource": "overview", "keep": true, "externalReference": "metrics.example.test/overview"}]
	}`)
	declaration, err := Parse(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	loaded, err := declaration.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Problems) != 0 {
		t.Fatalf("carriers produced problems: %v", loaded.Problems)
	}
	if len(loaded.Permissions) != 1 || loaded.Permissions[0].ReadScope.Groups[0] != "metrics.example.test" ||
		loaded.Permissions[0].RequiredRole != "Member" || len(loaded.Permissions[0].ObjectWriters) != 2 {
		t.Fatalf("permissions = %+v", loaded.Permissions)
	}
	if loaded.Configuration.Schema["type"] != "object" || len(loaded.Configuration.Conditions) != 1 {
		t.Fatalf("configuration = %+v", loaded.Configuration)
	}
	if len(loaded.Retention) != 1 || !loaded.Retention[0].Keep {
		t.Fatalf("retention = %+v", loaded.Retention)
	}
	if loaded.APIGroup != "carriers.example.test" {
		t.Fatalf("apiGroup = %q", loaded.APIGroup)
	}
}

// TestParseRejectsUnknownFields keeps a mistyped declaration from being loaded
// as if the field were absent.
func TestParseRejectsUnknownFields(t *testing.T) {
	if _, err := Parse([]byte(`{"identifier":"demo","version":"1","contribution":{}}`)); err == nil {
		t.Fatal("an unknown declaration field was accepted")
	}
}

// TestLoadRejectsMalformedDependency keeps a package with an unreadable version
// range from being activated.
func TestLoadRejectsMalformedDependency(t *testing.T) {
	declaration := fullDeclaration("demo.widgets")
	declaration.Dependencies = Dependencies{
		Capabilities: []CapabilityRequirement{{Name: "storage.volume", Versions: ">=one"}},
	}
	if _, err := declaration.Load(); err == nil || !strings.Contains(err.Error(), "storage.volume") {
		t.Fatalf("malformed constraint error = %v", err)
	}
}

// TestControllerScopeBecomesTheWriteAllocation checks the declaration's scope is
// what the write path checks extension writes against (C-20).
func TestControllerScopeBecomesTheWriteAllocation(t *testing.T) {
	loaded, err := fullDeclaration("demo.widgets").Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	scope := loaded.Contributions.Controllers[0].Scope.Admit()
	if !scope.Groups["widgets.example.test"] || !scope.Namespaces["default"] {
		t.Fatalf("scope = %+v", scope)
	}
	if scope.Groups["other.example.test"] {
		t.Fatal("a group outside the declaration is writable")
	}
}
