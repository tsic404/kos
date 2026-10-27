package app_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// field returns one config field with a textual default.
func field(name, kind string, required bool, defaultValue *string) v1beta1.ConfigField {
	return v1beta1.ConfigField{Name: name, Type: kind, Required: required, Default: defaultValue}
}

func TestResolveAppliesDefaultsAndNormalizesNumbers(t *testing.T) {
	schema := []v1beta1.ConfigField{
		field("replicas", app.TypeInteger, false, new("2")),
		field("ratio", app.TypeNumber, false, new("1.5")),
		field("verbose", app.TypeBoolean, false, new("true")),
		{
			Name:    "logLevel",
			Type:    app.TypeEnum,
			Default: new("info"),
			Enum:    []string{"error", "info", "debug"},
		},
	}

	omitted, err := app.Resolve(schema, map[string]any{})
	if err != nil {
		t.Fatalf("resolve defaults: %v", err)
	}
	explicit, err := app.Resolve(schema, map[string]any{
		"replicas": int64(2),
		"ratio":    1.5,
		"verbose":  true,
		"logLevel": "info",
	})
	if err != nil {
		t.Fatalf("resolve explicit values: %v", err)
	}
	for key, want := range omitted {
		if explicit[key] != want {
			t.Errorf("%s = %v (%T), want %v (%T)", key, explicit[key], explicit[key], want, want)
		}
	}
	if _, isInt := omitted["replicas"].(int64); !isInt {
		t.Errorf("replicas = %T, want int64 so a default and a submission render alike", omitted["replicas"])
	}
}

func TestResolveRefusesReservedFieldsWithTheirOwner(t *testing.T) {
	cases := map[string]app.ConfigOwner{
		"size":          app.OwnerVolume,
		"storagePolicy": app.OwnerVolume,
		"hostname":      app.OwnerEndpoint,
		"port":          app.OwnerEndpoint,
	}
	for key, owner := range cases {
		_, err := app.Resolve(nil, map[string]any{key: "x"})
		if err == nil {
			t.Fatalf("%s was accepted", key)
		}
		var configError *app.ConfigError
		if !errors.As(err, &configError) {
			t.Fatalf("%s: error %T is not a ConfigError", key, err)
		}
		if len(configError.Fields) != 1 {
			t.Fatalf("%s: %d refusals, want 1", key, len(configError.Fields))
		}
		if got := configError.Fields[0]; got.Field != key || got.Owner != owner {
			t.Errorf("%s refused as field %q owner %q, want %q owner %q", key, got.Field, got.Owner, key, owner)
		}
		if !strings.Contains(err.Error(), string(owner)) {
			t.Errorf("%s: the message does not name %s: %v", key, owner, err)
		}
	}
}

func TestResolveRefusesUnknownRequiredAndOutOfRange(t *testing.T) {
	schema := []v1beta1.ConfigField{
		{Name: "replicas", Type: app.TypeInteger, Minimum: new("2"), Maximum: new("4")},
		{Name: "name", Type: app.TypeString, Required: true},
	}
	cases := []map[string]any{
		{"replicas": int64(1), "name": "a"},
		{"replicas": int64(5), "name": "a"},
		{"replicas": int64(2)},
		{"replicas": int64(2), "name": "a", "extra": "value"},
		{"replicas": 2.5, "name": "a"},
	}
	for _, submitted := range cases {
		if _, err := app.Resolve(schema, submitted); err == nil {
			t.Errorf("config %v was accepted", submitted)
		}
	}
}

func TestValidateSchemaRefusesReservedDeclarations(t *testing.T) {
	err := app.ValidateSchema([]v1beta1.ConfigField{{Name: "hostname", Type: app.TypeString}})
	if err == nil {
		t.Fatal("a definition declaring a reserved config field was accepted")
	}
	if !strings.Contains(err.Error(), string(app.OwnerEndpoint)) {
		t.Fatalf("the refusal does not name the Endpoint object: %v", err)
	}
}

func TestReservedKeysFoldCaseAndSeparators(t *testing.T) {
	for _, key := range []string{"Size", "storage_class", "storage-class", "hostName", "targetPort"} {
		if err := app.CheckReserved(map[string]any{key: "value"}); err == nil {
			t.Errorf("submitted key %q was not refused", key)
		}
		schema := []v1beta1.ConfigField{{Name: key, Type: app.TypeString}}
		if err := app.ValidateSchema(schema); err == nil {
			t.Errorf("declared field %q was not refused", key)
		}
	}
	if err := app.CheckReserved(map[string]any{"logLevel": "info"}); err != nil {
		t.Errorf("a field no kernel object owns was refused: %v", err)
	}
}

func TestReservedRefusalNamesTheSubmittedKeyAndOwner(t *testing.T) {
	err := app.CheckReserved(map[string]any{"storage_class": "fast"})
	var config *app.ConfigError
	if !errors.As(err, &config) || len(config.Fields) != 1 {
		t.Fatalf("error %v is not a single-field ConfigError", err)
	}
	if config.Fields[0].Field != "storage_class" || config.Fields[0].Owner != app.OwnerVolume {
		t.Fatalf("refusal = %+v, want the submitted key and the Volume owner", config.Fields[0])
	}
}
