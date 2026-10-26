// Package tc45 verifies that the translation template is a pure function: each
// release definition renders the same bytes for the same config, and a config
// that spells the defaults out renders exactly like the omitted form (AC-23,
// TC-45).
package tc45

import (
	"encoding/json"
	"sort"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/app/builtin"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC45TranslationStability(t *testing.T) {
	cluster := harness.Start(t)
	definitions, err := builtin.Definitions()
	if err != nil {
		t.Fatalf("load the release definitions: %v", err)
	}
	if len(definitions) != 5 {
		t.Fatalf("the distribution ships %d definitions, want 5", len(definitions))
	}
	for _, definition := range definitions {
		cluster.InstallDefinition(t, definition)
	}

	for _, shipped := range definitions {
		t.Run(shipped.Name, func(t *testing.T) {
			stored := cluster.Get(t, "v1beta1", "ApplicationDefinition", shipped.Name)
			definition := &v1beta1.ApplicationDefinition{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, definition); err != nil {
				t.Fatalf("decode the stored definition: %v", err)
			}
			config, err := app.Resolve(definition.Spec.Config, nil)
			if err != nil {
				t.Fatalf("resolve the defaults: %v", err)
			}
			first := render(t, definition.Spec, config)
			snapshot := encodeConfig(t, config)
			// Repeated renders on freshly built maps: a template that depends on
			// map iteration order, or a render that writes its input, shows up
			// here instead of passing by luck.
			for round := range 20 {
				sample := rebuild(config, round)
				if got := render(t, definition.Spec, sample); got != first {
					t.Fatalf("round %d rendered differently:\n%s\n%s", round, first, got)
				}
			}
			if encodeConfig(t, config) != snapshot {
				t.Fatal("a render modified the submitted config")
			}
			explicit, err := app.Resolve(definition.Spec.Config, spelledOut(t, definition.Spec.Config))
			if err != nil {
				t.Fatalf("resolve the explicit defaults: %v", err)
			}
			explicitSnapshot := encodeConfig(t, explicit)
			equivalent := render(t, definition.Spec, explicit)
			if equivalent != first {
				t.Fatalf("an equivalent config rendered differently:\n%s\n%s", first, equivalent)
			}
			if encodeConfig(t, explicit) != explicitSnapshot {
				t.Fatal("a render modified the explicit config")
			}
		})
	}
}

// encodeConfig renders a config map into one comparable string.
func encodeConfig(t *testing.T, config map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("encode the config: %v", err)
	}
	return string(encoded)
}

// rebuild copies a config with its keys inserted in a different order each
// round, so iteration order is exercised without relying on chance.
func rebuild(config map[string]any, round int) map[string]any {
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if round%2 == 1 {
		for left, right := 0, len(keys)-1; left < right; left, right = left+1, right-1 {
			keys[left], keys[right] = keys[right], keys[left]
		}
	}
	rebuilt := make(map[string]any, len(config))
	for _, key := range keys {
		rebuilt[key] = config[key]
	}
	return rebuilt
}

// render encodes one render for byte comparison.
func render(t *testing.T, spec v1beta1.ApplicationDefinitionSpec, config map[string]any) string {
	t.Helper()
	objects, err := app.Render(spec, app.RenderInput{Name: "sample", Namespace: "apps", Config: config})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		t.Fatalf("encode the render: %v", err)
	}
	return string(encoded)
}

// spelledOut turns every declared default into an explicit submitted value.
func spelledOut(t *testing.T, fields []v1beta1.ConfigField) map[string]any {
	t.Helper()
	values := make(map[string]any, len(fields))
	for _, field := range fields {
		if field.Default == nil {
			continue
		}
		switch field.Type {
		case app.TypeInteger:
			number, err := strconv.ParseInt(*field.Default, 10, 64)
			if err != nil {
				t.Fatalf("%s default %q: %v", field.Name, *field.Default, err)
			}
			values[field.Name] = number
		case app.TypeNumber:
			number, err := strconv.ParseFloat(*field.Default, 64)
			if err != nil {
				t.Fatalf("%s default %q: %v", field.Name, *field.Default, err)
			}
			values[field.Name] = number
		case app.TypeBoolean:
			flag, err := strconv.ParseBool(*field.Default)
			if err != nil {
				t.Fatalf("%s default %q: %v", field.Name, *field.Default, err)
			}
			values[field.Name] = flag
		default:
			values[field.Name] = *field.Default
		}
	}
	return values
}
