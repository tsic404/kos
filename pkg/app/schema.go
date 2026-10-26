package app

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/tsic404/kos/api/v1beta1"
)

// Config field types (C-32).
const (
	TypeString  = "string"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeEnum    = "enum"
)

// ConfigOwner names the kernel object that owns a config value the application
// must not repeat (F3).
type ConfigOwner string

const (
	// OwnerVolume is the Volume object: sizes and storage policy belong there
	// (C-38).
	OwnerVolume ConfigOwner = "Volume"
	// OwnerEndpoint is the Endpoint object: hostnames and ports belong there
	// (C-43).
	OwnerEndpoint ConfigOwner = "Endpoint"
)

// reservedField is one config concept the Volume or Endpoint object owns (F3).
type reservedField struct {
	// name is the concept's canonical spelling.
	name string
	// owner is the kernel object that owns the desired value.
	owner ConfigOwner
}

// reservedFields lists the config concepts the Volume and Endpoint objects own
// (F3). A definition may not declare one and a submission may not carry one.
var reservedFields = []reservedField{
	{name: "size", owner: OwnerVolume},
	{name: "volumeSize", owner: OwnerVolume},
	{name: "storageClass", owner: OwnerVolume},
	{name: "storagePolicy", owner: OwnerVolume},
	{name: "hostname", owner: OwnerEndpoint},
	{name: "host", owner: OwnerEndpoint},
	{name: "port", owner: OwnerEndpoint},
	{name: "ports", owner: OwnerEndpoint},
	{name: "targetPort", owner: OwnerEndpoint},
}

// reservedOwner reports the object that owns a config key. Matching folds case
// and separators, so Size, storage_class and storage-class name the same
// concept and cannot be declared as a new field (F3).
func reservedOwner(name string) (ConfigOwner, bool) {
	folded := foldKey(name)
	for _, field := range reservedFields {
		if foldKey(field.name) == folded {
			return field.owner, true
		}
	}
	return "", false
}

// foldKey normalizes a config key for the reserved comparison.
func foldKey(name string) string {
	return strings.NewReplacer("_", "", "-", "").Replace(strings.ToLower(name))
}

// FieldError refuses one config field and says what to do instead (C-32, F3).
type FieldError struct {
	// Field is the config key the submission or the definition carries.
	Field string
	// Owner is the object that owns the same desired value, when the field
	// collides with a kernel object.
	Owner ConfigOwner
	// Reason explains the refusal.
	Reason string
}

// Error names the field and, for a collision, the object that owns it.
func (e *FieldError) Error() string {
	if e.Owner != "" {
		return fmt.Sprintf("config field %q is owned by %s: %s", e.Field, e.Owner, e.Reason)
	}
	return fmt.Sprintf("config field %q: %s", e.Field, e.Reason)
}

// ConfigError collects every field refusal of one submission.
type ConfigError struct {
	// Fields lists the refusals in a stable order.
	Fields []*FieldError
}

// Error lists the refused fields.
func (e *ConfigError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		parts = append(parts, field.Error())
	}
	return strings.Join(parts, "; ")
}

// CheckReserved refuses config keys the Volume and Endpoint objects own (F3,
// TC-46). It runs before the schema lookup so a colliding key is reported as a
// collision rather than as an unknown field.
func CheckReserved(config map[string]any) error {
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var refused []*FieldError
	for _, key := range keys {
		owner, reserved := reservedOwner(key)
		if !reserved {
			continue
		}
		refused = append(refused, &FieldError{
			Field:  key,
			Owner:  owner,
			Reason: fmt.Sprintf("the same desired value belongs to the %s object", owner),
		})
	}
	if len(refused) == 0 {
		return nil
	}
	return &ConfigError{Fields: refused}
}

// ValidateSchema checks a definition's config structure (C-32). The schema is
// the only source of interface fields, so a malformed one is refused instead of
// rendered.
func ValidateSchema(fields []v1beta1.ConfigField) error {
	var refused []*FieldError
	seen := make(map[string]bool, len(fields))
	for _, field := range fields {
		switch {
		case field.Name == "":
			refused = append(refused, &FieldError{Field: field.Name, Reason: "a field needs a name"})
			continue
		case !validKey(field.Name):
			refused = append(refused, &FieldError{Field: field.Name, Reason: "a field name is a letter followed by letters, digits or underscores"})
		case seen[field.Name]:
			refused = append(refused, &FieldError{Field: field.Name, Reason: "the name is declared twice"})
		}
		seen[field.Name] = true
		if owner, reserved := reservedOwner(field.Name); reserved {
			refused = append(refused, &FieldError{Field: field.Name, Owner: owner, Reason: "the object that owns it declares it"})
			continue
		}
		if _, err := defaultFor(field); err != nil && field.Default != nil {
			refused = append(refused, &FieldError{Field: field.Name, Reason: err.Error()})
		}
		if err := checkFieldShape(field); err != nil {
			refused = append(refused, &FieldError{Field: field.Name, Reason: err.Error()})
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return &ConfigError{Fields: refused}
}

// Resolve applies the declared defaults and validates a submitted config,
// returning the values the template runs on (C-32). Unknown keys, type
// mismatches, out-of-range values and missing required fields are refused; the
// reserved keys of the Volume and Endpoint objects are refused first (F3).
func Resolve(fields []v1beta1.ConfigField, submitted map[string]any) (map[string]any, error) {
	if err := CheckReserved(submitted); err != nil {
		return nil, err
	}
	declared := make(map[string]v1beta1.ConfigField, len(fields))
	for _, field := range fields {
		declared[field.Name] = field
	}
	var refused []*FieldError
	unknown := make([]string, 0)
	for key := range submitted {
		if _, known := declared[key]; !known {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	for _, key := range unknown {
		refused = append(refused, &FieldError{Field: key, Reason: "the definition declares no such field"})
	}

	resolved := make(map[string]any, len(fields))
	for _, field := range fields {
		value, present := submitted[field.Name]
		if !present {
			def, err := defaultFor(field)
			if err != nil {
				refused = append(refused, &FieldError{Field: field.Name, Reason: err.Error()})
				continue
			}
			if def != nil {
				resolved[field.Name] = def
				continue
			}
			if field.Required {
				refused = append(refused, &FieldError{Field: field.Name, Reason: "the field is required"})
			}
			continue
		}
		normalized, err := normalize(field, value)
		if err != nil {
			refused = append(refused, &FieldError{Field: field.Name, Reason: err.Error()})
			continue
		}
		resolved[field.Name] = normalized
	}
	if len(refused) > 0 {
		return nil, &ConfigError{Fields: refused}
	}
	return resolved, nil
}

// checkFieldShape validates the parts of a field that do not depend on a
// submission.
func checkFieldShape(field v1beta1.ConfigField) error {
	switch field.Type {
	case TypeString, TypeNumber:
		if len(field.Enum) > 0 || field.Minimum != nil || field.Maximum != nil {
			return fmt.Errorf("%s fields take neither an enum nor a range", field.Type)
		}
	case TypeInteger:
		if len(field.Enum) > 0 {
			return fmt.Errorf("integer fields take no enum")
		}
	case TypeBoolean:
		if len(field.Enum) > 0 || field.Minimum != nil || field.Maximum != nil {
			return fmt.Errorf("boolean fields take neither an enum nor a range")
		}
	case TypeEnum:
		if len(field.Enum) == 0 {
			return fmt.Errorf("enum fields need the accepted values")
		}
		if field.Minimum != nil || field.Maximum != nil {
			return fmt.Errorf("enum fields take no range")
		}
	default:
		return fmt.Errorf("unknown type %q", field.Type)
	}
	minimum, err := bound(field.Minimum)
	if err != nil {
		return fmt.Errorf("the minimum is not a number")
	}
	maximum, err := bound(field.Maximum)
	if err != nil {
		return fmt.Errorf("the maximum is not a number")
	}
	if minimum != nil && maximum != nil && *minimum > *maximum {
		return fmt.Errorf("the minimum is above the maximum")
	}
	return nil
}

// bound parses a textual range bound; a missing bound is nil.
func bound(text *string) (*float64, error) {
	if text == nil {
		return nil, nil
	}
	value, err := strconv.ParseFloat(*text, 64)
	if err != nil {
		return nil, err
	}
	return &value, nil
}

// defaultFor interprets the textual default of one field according to its
// type.
func defaultFor(field v1beta1.ConfigField) (any, error) {
	if field.Default == nil {
		return nil, nil
	}
	if field.Type == TypeBoolean {
		switch strings.TrimSpace(*field.Default) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return nil, fmt.Errorf("the default is not a boolean")
		}
	}
	return normalize(field, *field.Default)
}

// normalize turns one submitted or defaulted value into the shape its type
// promises. Integer and number values are normalized whatever the decoder
// produced, so an explicit default and an omitted field render identically.
func normalize(field v1beta1.ConfigField, value any) (any, error) {
	switch field.Type {
	case TypeString:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("the value is not a string")
		}
		return text, nil
	case TypeEnum:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("the value is not a string")
		}
		for _, accepted := range field.Enum {
			if text == accepted {
				return text, nil
			}
		}
		return nil, fmt.Errorf("the value must be one of %s", strings.Join(field.Enum, ", "))
	case TypeBoolean:
		flag, ok := value.(bool)
		if !ok {
			return nil, fmt.Errorf("the value is not a boolean")
		}
		return flag, nil
	case TypeInteger:
		number, err := toFloat(field, value)
		if err != nil {
			return nil, err
		}
		if number != float64(int64(number)) {
			return nil, fmt.Errorf("the value is not an integer")
		}
		return int64(number), nil
	case TypeNumber:
		return toFloat(field, value)
	default:
		return nil, fmt.Errorf("unknown type %q", field.Type)
	}
}

// toFloat reads a numeric value and enforces the declared range.
func toFloat(field v1beta1.ConfigField, value any) (float64, error) {
	var number float64
	switch typed := value.(type) {
	case int64:
		number = float64(typed)
	case int32:
		number = float64(typed)
	case int:
		number = float64(typed)
	case float64:
		number = typed
	case string:
		parsed, err := strconv.ParseFloat(typed, 64)
		if err != nil {
			return 0, fmt.Errorf("the value is not a number")
		}
		number = parsed
	default:
		return 0, fmt.Errorf("the value is not a number")
	}
	minimum, err := bound(field.Minimum)
	if err != nil {
		return 0, fmt.Errorf("the minimum is not a number")
	}
	maximum, err := bound(field.Maximum)
	if err != nil {
		return 0, fmt.Errorf("the maximum is not a number")
	}
	if minimum != nil && number < *minimum {
		return 0, fmt.Errorf("the value is below the minimum %v", *minimum)
	}
	if maximum != nil && number > *maximum {
		return 0, fmt.Errorf("the value is above the maximum %v", *maximum)
	}
	return number, nil
}

// validKey reports whether name is usable as a config key: a letter followed
// by letters, digits or underscores.
func validKey(name string) bool {
	for index, character := range name {
		letter := character == '_' ||
			(character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9')
		if !letter {
			return false
		}
		if index == 0 && (character == '_' || (character >= '0' && character <= '9')) {
			return false
		}
	}
	return name != ""
}
