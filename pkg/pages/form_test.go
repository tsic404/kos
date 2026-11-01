package pages

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// TestFormOfLaysOutTheDeclaredStructure checks that the form is generated from
// the structure alone: every declared field appears once, in declaration order,
// with the control its type picks and the constraints the structure sets, and no
// application field is hardcoded (C-31, C-32).
func TestFormOfLaysOutTheDeclaredStructure(t *testing.T) {
	fields := []v1beta1.ConfigField{
		{Name: "tier", Type: app.TypeEnum, Required: true, Enum: []string{"small", "large"}, Description: "运行规格"},
		{Name: "replicas", Type: app.TypeInteger, Default: new("2"), Minimum: new("1"), Maximum: new("5")},
		{Name: "ratio", Type: app.TypeNumber, Minimum: new("0.5")},
		{Name: "enabled", Type: app.TypeBoolean, Default: new("true")},
		{Name: "label", Type: app.TypeString, Default: new("kos")},
		{Name: "password", Type: app.TypeString, Sensitive: true, Description: "访问口令"},
	}
	want := []FormField{
		{Name: "tier", Description: "运行规格", Type: app.TypeEnum, Control: ControlSelect, Required: true, Enum: []string{"small", "large"}},
		{Name: "replicas", Type: app.TypeInteger, Control: ControlNumber, Default: new("2"), Minimum: new("1"), Maximum: new("5")},
		{Name: "ratio", Type: app.TypeNumber, Control: ControlNumber, Minimum: new("0.5")},
		{Name: "enabled", Type: app.TypeBoolean, Control: ControlSwitch, Default: new("true")},
		{Name: "label", Type: app.TypeString, Control: ControlText, Default: new("kos")},
		{Name: "password", Description: "访问口令", Type: app.TypeString, Control: ControlText, Sensitive: true},
	}
	if laid := FormOf(fields); !reflect.DeepEqual(laid, want) {
		t.Fatalf("form = %+v, want %+v", laid, want)
	}
}

// TestFormOfEmptyStructureLaysOutNoField checks the empty state: a definition
// without install parameters shows an empty form, not a missing value.
func TestFormOfEmptyStructureLaysOutNoField(t *testing.T) {
	laid := FormOf(nil)
	if laid == nil || len(laid) != 0 {
		t.Fatalf("form = %#v, want an empty list", laid)
	}
	encoded, err := json.Marshal(laid)
	if err != nil {
		t.Fatalf("encode the form: %v", err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("encoded form = %s, want an empty list", encoded)
	}
}
