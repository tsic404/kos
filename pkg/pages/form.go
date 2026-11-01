package pages

import (
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// Control kinds the shell lays out for one config field (C-32). The field's
// declared type picks the control: no field name is special, and no application
// field is hardcoded into the shell.
const (
	// ControlText is a single-line text input.
	ControlText = "text"
	// ControlNumber is a numeric input, bounded by the field's range.
	ControlNumber = "number"
	// ControlSwitch is a boolean toggle.
	ControlSwitch = "switch"
	// ControlSelect is a choice among the field's accepted values.
	ControlSelect = "select"
)

// FormField is one install-parameter field the shell lays out from a config
// structure (C-31, C-32). The structure is the only source of the fields, and
// every constraint travels unchanged, so the interface shows what the write path
// enforces before a submission is made. A sensitive field is marked and carries
// no value: the shell renders it write-only and reads whether it is set from the
// projection's mask, never the parameter itself (C-14, C-31).
type FormField struct {
	// Name is the config key the submission uses.
	Name string `json:"name"`
	// Description is the help text the structure declares.
	Description string `json:"description,omitempty"`
	// Type is the value shape the structure declares.
	Type string `json:"type"`
	// Control is the input the shell lays out for Type.
	Control string `json:"control"`
	// Required rejects a submission that omits the field.
	Required bool `json:"required,omitempty"`
	// Default is the textual default the structure declares.
	Default *string `json:"default,omitempty"`
	// Minimum is the textual inclusive lower bound of an integer or number.
	Minimum *string `json:"minimum,omitempty"`
	// Maximum is the textual inclusive upper bound of an integer or number.
	Maximum *string `json:"maximum,omitempty"`
	// Enum lists the accepted values of an enum field.
	Enum []string `json:"enum,omitempty"`
	// Sensitive marks a parameter the read path never returns (C-31).
	Sensitive bool `json:"sensitive,omitempty"`
}

// FormOf generates the fields the shell lays out for one config structure. Every
// declared field appears once, in declaration order, with the type, default,
// required mark, range, accepted values and sensitive mark the structure sets.
// The renderer presents the structure and never judges a value: a submission is
// refused by the write path, which names the field and its constraint (C-32).
// An empty structure lays out no field, which the shell shows as an empty form
// rather than a blank panel.
func FormOf(fields []v1beta1.ConfigField) []FormField {
	laid := make([]FormField, 0, len(fields))
	for _, field := range fields {
		laid = append(laid, FormField{
			Name:        field.Name,
			Description: field.Description,
			Type:        field.Type,
			Control:     controlOf(field.Type),
			Required:    field.Required,
			Default:     field.Default,
			Minimum:     field.Minimum,
			Maximum:     field.Maximum,
			Enum:        field.Enum,
			Sensitive:   field.Sensitive,
		})
	}
	return laid
}

// controlOf maps a field type to the input the shell lays out. The type is
// judged when the definition is written, not while it is displayed, so a type
// the structure should not have declared lays out as text instead of failing the
// page (C-32).
func controlOf(fieldType string) string {
	switch fieldType {
	case app.TypeInteger, app.TypeNumber:
		return ControlNumber
	case app.TypeBoolean:
		return ControlSwitch
	case app.TypeEnum:
		return ControlSelect
	default:
		return ControlText
	}
}
