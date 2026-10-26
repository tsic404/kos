package extension

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Constraint is a version range: space-separated comparators such as
// ">=1.2 <2.0". An empty constraint admits every version.
type Constraint struct {
	comparators []comparator
}

// comparator is one operator and version of a constraint.
type comparator struct {
	operator string
	version  version
}

// version is a dot-separated number sequence; a leading v is allowed so a
// capability's first version can be written v1 (C-09).
type version []int

// ParseConstraint parses a version range. Each comparator is an optional
// operator (>=, >, <=, <, =, ==) followed by a version; a bare version means
// equality.
func ParseConstraint(text string) (Constraint, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Constraint{}, nil
	}
	constraint := Constraint{}
	for _, field := range strings.Fields(trimmed) {
		operator := ""
		value := field
		for _, candidate := range []string{">=", "<=", "==", ">", "<", "="} {
			if trimmedValue, found := strings.CutPrefix(field, candidate); found {
				operator, value = candidate, trimmedValue
				break
			}
		}
		parsed, err := parseVersion(value)
		if err != nil {
			return Constraint{}, fmt.Errorf("comparator %q: %w", field, err)
		}
		constraint.comparators = append(constraint.comparators, comparator{operator: operator, version: parsed})
	}
	return constraint, nil
}

// Allows reports whether a version satisfies every comparator.
func (c Constraint) Allows(text string) bool {
	if len(c.comparators) == 0 {
		return true
	}
	candidate, err := parseVersion(text)
	if err != nil {
		return false
	}
	for _, comparator := range c.comparators {
		order := compareVersions(candidate, comparator.version)
		switch comparator.operator {
		case ">=":
			if order < 0 {
				return false
			}
		case ">":
			if order <= 0 {
				return false
			}
		case "<=":
			if order > 0 {
				return false
			}
		case "<":
			if order >= 0 {
				return false
			}
		default:
			if order != 0 {
				return false
			}
		}
	}
	return true
}

// parseVersion reads a dot-separated number sequence with an optional v prefix.
func parseVersion(text string) (version, error) {
	trimmed := strings.TrimSpace(text)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return nil, errors.New("version is empty")
	}
	parts := strings.Split(trimmed, ".")
	parsed := make(version, len(parts))
	for index, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("version %q has an empty segment", text)
		}
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return nil, fmt.Errorf("version %q is not a number sequence", text)
		}
		parsed[index] = number
	}
	return parsed, nil
}

// compareVersions orders two versions; a missing segment counts as zero, so v1
// and v1.0 are the same version.
func compareVersions(left, right version) int {
	length := max(len(left), len(right))
	for index := 0; index < length; index++ {
		leftPart, rightPart := 0, 0
		if index < len(left) {
			leftPart = left[index]
		}
		if index < len(right) {
			rightPart = right[index]
		}
		switch {
		case leftPart < rightPart:
			return -1
		case leftPart > rightPart:
			return 1
		}
	}
	return 0
}
