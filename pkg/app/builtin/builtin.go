// Package builtin carries the application definitions the distribution ships.
// Each definition is loaded from the embedded package content, its digest is
// computed the way the catalogue computes it, and the result is validated
// before it can be installed (C-32).
package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"sigs.k8s.io/yaml"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

//go:embed definitions/*.yaml
var catalog embed.FS

// Definitions returns every built-in definition, ordered by name. Each shipped
// definition pins its package digest; loading recomputes it from the content and
// refuses a package whose template no longer matches its pin.
func Definitions() ([]*v1beta1.ApplicationDefinition, error) {
	return loadDefinitions(catalog)
}

// loadDefinitions loads, pins and validates every definition below fsys. The
// decode is strict: a misspelled field would otherwise be dropped before the
// digest is computed, and the pin would cover content the package never used.
func loadDefinitions(fsys fs.FS) ([]*v1beta1.ApplicationDefinition, error) {
	paths, err := fs.Glob(fsys, "definitions/*.yaml")
	if err != nil {
		return nil, fmt.Errorf("list the built-in definitions: %w", err)
	}
	definitions := make([]*v1beta1.ApplicationDefinition, 0, len(paths))
	for _, path := range paths {
		raw, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		definition := &v1beta1.ApplicationDefinition{}
		if err := yaml.UnmarshalStrict(raw, definition); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		if definition.Kind != "ApplicationDefinition" {
			return nil, fmt.Errorf("%s is a %s, not an ApplicationDefinition", path, definition.Kind)
		}
		digest, err := app.DefinitionDigest(definition.Spec)
		if err != nil {
			return nil, fmt.Errorf("digest %s: %w", path, err)
		}
		if definition.Spec.Package.Digest == "" {
			return nil, fmt.Errorf("%s pins no package digest", path)
		}
		if definition.Spec.Package.Digest != digest {
			return nil, fmt.Errorf("%s: the package content does not match its pinned digest", path)
		}
		if err := app.ValidateDefinition(definition.Spec); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		definitions = append(definitions, definition)
	}
	sort.Slice(definitions, func(left, right int) bool {
		return definitions[left].Name < definitions[right].Name
	})
	return definitions, nil
}

// Definition returns one built-in definition by name.
func Definition(name string) (*v1beta1.ApplicationDefinition, error) {
	definitions, err := Definitions()
	if err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		if definition.Name == name {
			return definition, nil
		}
	}
	return nil, fmt.Errorf("the distribution ships no definition %q", name)
}

// Names returns the names of the built-in definitions, ordered.
func Names() ([]string, error) {
	definitions, err := Definitions()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names, nil
}
