package builtin_test

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/app/builtin"
)

func TestBuiltinDefinitionsValidateAndRenderStably(t *testing.T) {
	definitions, err := builtin.Definitions()
	if err != nil {
		t.Fatalf("load the built-in definitions: %v", err)
	}
	if len(definitions) != 5 {
		t.Fatalf("the distribution ships %d definitions, want 5", len(definitions))
	}
	for _, definition := range definitions {
		t.Run(definition.Name, func(t *testing.T) {
			if err := app.ValidateDefinition(definition.Spec); err != nil {
				t.Fatalf("definition is invalid: %v", err)
			}
			config, err := app.Resolve(definition.Spec.Config, nil)
			if err != nil {
				t.Fatalf("resolve the defaults: %v", err)
			}
			input := app.RenderInput{Name: "sample", Namespace: "apps", Config: config}
			first, err := app.Render(definition.Spec, input)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			second, err := app.Render(definition.Spec, input)
			if err != nil {
				t.Fatalf("second render: %v", err)
			}
			firstJSON, _ := json.Marshal(first)
			secondJSON, _ := json.Marshal(second)
			if string(firstJSON) != string(secondJSON) {
				t.Fatal("the same config rendered differently")
			}
			explicit, err := app.Resolve(definition.Spec.Config, submitted(t, definition))
			if err != nil {
				t.Fatalf("resolve explicit defaults: %v", err)
			}
			third, err := app.Render(definition.Spec, app.RenderInput{Name: "sample", Namespace: "apps", Config: explicit})
			if err != nil {
				t.Fatalf("render the explicit defaults: %v", err)
			}
			thirdJSON, _ := json.Marshal(third)
			if string(thirdJSON) != string(firstJSON) {
				t.Fatal("an equivalent config rendered differently")
			}
		})
	}
}

// submitted spells every declared default out as an explicit value, which must
// render exactly like the omitted form (C-32).
func submitted(t *testing.T, definition *v1beta1.ApplicationDefinition) map[string]any {
	t.Helper()
	values := make(map[string]any, len(definition.Spec.Config))
	for _, field := range definition.Spec.Config {
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
