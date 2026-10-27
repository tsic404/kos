package app

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tsic404/kos/api/v1beta1"
)

// Capability is one capability a provider registers at one version (C-09).
type Capability struct {
	// Name is the capability name.
	Name string
	// Version is the registered version.
	Version string
	// Scope is where the registration's guarantee holds (C-09). An empty value
	// is read as node: the weaker promise never satisfies a cluster-level
	// requirement (C-11).
	Scope v1beta1.CapabilityScope
	// Guarantees are the guarantee values the registration carries.
	Guarantees map[string]Guarantee
	// Verified reports that KOS ran the capability's conformance and every
	// case passed; guarantees of an unverified capability do not hold (C-10).
	Verified bool
}

// Guarantee is one guarantee value: a boolean or an enum (C-09).
type Guarantee struct {
	// Bool is the boolean value, when the guarantee is boolean.
	Bool *bool
	// Enum is the enum value, when the guarantee is an enum.
	Enum string
}

// CapabilityResult is what the comparison of C-11 found.
type CapabilityResult struct {
	// Missing lists required capabilities with no matching version; the
	// application stays Pending (C-11).
	Missing []string
	// Unsatisfied lists the requirements that failed without stopping the
	// install: the application installs and reports Degraded (C-11). Items are
	// "capability.key" for a guarantee and the capability name for a missing
	// optional capability.
	Unsatisfied []string
}

// Compare applies the C-11 comparison rules: pick the highest version that
// satisfies the declared range, then compare the guarantees one by one. A
// boolean must be equal and an enum must be equal or inside the declared set.
func Compare(requirements []v1beta1.CapabilityRequirement, available []Capability) (CapabilityResult, error) {
	var result CapabilityResult
	for _, requirement := range requirements {
		if err := ValidateRequirement(requirement); err != nil {
			return CapabilityResult{}, err
		}
		best, found := highest(requirement, available)
		if !found {
			if requirement.Optional {
				result.Unsatisfied = appendUnique(result.Unsatisfied, requirement.Name)
				continue
			}
			result.Missing = appendUnique(result.Missing, requirement.Name)
			continue
		}
		if !best.Verified {
			// An unverified capability promises no guarantee (C-10), so no
			// key of the requirement holds.
			for _, wanted := range requirement.Guarantees {
				result.Unsatisfied = appendUnique(result.Unsatisfied, requirement.Name+"."+wanted.Key)
			}
			continue
		}
		for _, wanted := range requirement.Guarantees {
			got, present := best.Guarantees[wanted.Key]
			if present && guaranteeMatches(wanted, got) {
				continue
			}
			result.Unsatisfied = appendUnique(result.Unsatisfied, requirement.Name+"."+wanted.Key)
		}
	}
	return result, nil
}

// satisfiesGuarantees reports whether one registration can promise the
// requirement. An unverified capability promises nothing: only a KOS run makes
// its guarantee level stand (C-10).
func satisfiesGuarantees(requirements []v1beta1.GuaranteeRequirement, candidate Capability) bool {
	if len(requirements) == 0 {
		return true
	}
	if !candidate.Verified {
		return false
	}
	for _, wanted := range requirements {
		got, present := candidate.Guarantees[wanted.Key]
		if !present || !guaranteeMatches(wanted, got) {
			return false
		}
	}
	return true
}

// MatchesVersion reports whether version falls inside constraint, using the
// version syntax C-11 compares with.
func MatchesVersion(version, constraint string) (bool, error) {
	return satisfies(version, constraint)
}

// highest picks the highest registered version that satisfies the requirement's
// range, then the first registration at that version whose guarantees hold: two
// providers may offer the same version, and one that cannot promise the
// guarantees does not shadow one that can (C-10, C-11). A version that does not
// parse cannot be ordered, so it never becomes the pick.
func highest(requirement v1beta1.CapabilityRequirement, available []Capability) (Capability, bool) {
	var bestVersion []int
	candidates := make([]Capability, 0)
	for _, candidate := range available {
		if candidate.Name != requirement.Name {
			continue
		}
		// A registration whose scope is narrower than the requirement cannot
		// serve it (C-11); the version menu is built from what remains, so a
		// lower version that does hold cluster-wide is still reachable.
		if !satisfiesScope(requirement.Scope, candidate.Scope) {
			continue
		}
		version, ok := parseVersion(candidate.Version)
		if !ok {
			continue
		}
		satisfied, err := satisfies(candidate.Version, requirement.Version)
		if err != nil || !satisfied {
			continue
		}
		switch {
		case len(candidates) == 0 || compare(version, bestVersion) > 0:
			bestVersion = version
			candidates = []Capability{candidate}
		case compare(version, bestVersion) == 0:
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range candidates {
		if satisfiesGuarantees(requirement.Guarantees, candidate) {
			return candidate, true
		}
	}
	// No registration at the picked version promises everything. A verified
	// one is the better answer: its failure names the guarantee that is short,
	// while an unverified one voids every key.
	for _, candidate := range candidates {
		if candidate.Verified {
			return candidate, true
		}
	}
	if len(candidates) > 0 {
		return candidates[0], true
	}
	return Capability{}, false
}

// satisfiesScope reports whether a registration covers what a requirement asks
// for (C-11): a cluster-level requirement needs a cluster-level capability,
// while a node-level requirement accepts either scope. An omitted scope on
// either side reads as node, the weaker promise.
func satisfiesScope(required, provided v1beta1.CapabilityScope) bool {
	if scopeOf(required) == v1beta1.CapabilityScopeNode {
		return true
	}
	return scopeOf(provided) == v1beta1.CapabilityScopeCluster
}

// scopeOf reads a scope value, defaulting an omitted one to node (C-11).
func scopeOf(scope v1beta1.CapabilityScope) v1beta1.CapabilityScope {
	if scope == "" {
		return v1beta1.CapabilityScopeNode
	}
	return scope
}

// guaranteeMatches applies the guarantee comparison of C-11.
func guaranteeMatches(required v1beta1.GuaranteeRequirement, got Guarantee) bool {
	if required.Bool != nil {
		return got.Bool != nil && *got.Bool == *required.Bool
	}
	if required.Enum != "" && got.Enum != required.Enum {
		return false
	}
	if len(required.OneOf) > 0 {
		for _, accepted := range required.OneOf {
			if got.Enum == accepted {
				return true
			}
		}
		return false
	}
	return required.Enum != ""
}

// ValidateRequirement reports a requirement the comparison cannot decide: an
// unknown version syntax, or a guarantee that names neither a boolean nor an
// enum (C-11).
func ValidateRequirement(requirement v1beta1.CapabilityRequirement) error {
	if requirement.Name == "" {
		return fmt.Errorf("a capability requirement carries no name")
	}
	if _, err := parseConstraint(requirement.Version); err != nil {
		return fmt.Errorf("capability %q: %w", requirement.Name, err)
	}
	switch requirement.Scope {
	case "", v1beta1.CapabilityScopeCluster, v1beta1.CapabilityScopeNode:
	default:
		return fmt.Errorf("capability %q: the scope %q is neither cluster nor node", requirement.Name, requirement.Scope)
	}
	for _, guarantee := range requirement.Guarantees {
		if guarantee.Key == "" {
			return fmt.Errorf("capability %q: a guarantee carries no key", requirement.Name)
		}
		if guarantee.Bool == nil && guarantee.Enum == "" && len(guarantee.OneOf) == 0 {
			return fmt.Errorf("capability %q guarantee %q carries no value", requirement.Name, guarantee.Key)
		}
		if guarantee.Bool != nil && (guarantee.Enum != "" || len(guarantee.OneOf) > 0) {
			return fmt.Errorf("capability %q guarantee %q mixes a boolean with an enum", requirement.Name, guarantee.Key)
		}
		if guarantee.Enum != "" && len(guarantee.OneOf) > 0 {
			return fmt.Errorf("capability %q guarantee %q declares both an enum and a oneOf set", requirement.Name, guarantee.Key)
		}
	}
	return nil
}

// versionTerm is one parsed term of a version range: the operator and the
// version it bounds.
type versionTerm struct {
	operator string
	bound    string
}

// parseConstraint validates every term of a range before any comparison runs,
// so a malformed term is reported even when an earlier term already failed. The
// syntax is a comma-separated list; each term is a dotted version with an
// optional =, >=, <=, >, <, ~ or ^ prefix, and an empty range or "*" is no
// constraint at all.
func parseConstraint(constraint string) ([]versionTerm, error) {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" {
		return nil, nil
	}
	terms := make([]versionTerm, 0, strings.Count(constraint, ",")+1)
	for _, raw := range strings.Split(constraint, ",") {
		term := strings.TrimSpace(raw)
		if term == "" {
			return nil, fmt.Errorf("the version range has an empty term")
		}
		operator := ""
		for _, candidate := range []string{">=", "<=", ">", "<", "=", "~", "^"} {
			if strings.HasPrefix(term, candidate) {
				operator = candidate
				term = strings.TrimSpace(strings.TrimPrefix(term, candidate))
				break
			}
		}
		if _, ok := parseVersion(term); !ok {
			return nil, fmt.Errorf("version %q is not a dotted number", term)
		}
		terms = append(terms, versionTerm{operator: operator, bound: term})
	}
	return terms, nil
}

// satisfies reports whether version falls inside constraint.
func satisfies(version, constraint string) (bool, error) {
	terms, err := parseConstraint(constraint)
	if err != nil {
		return false, err
	}
	for _, term := range terms {
		matched, err := compareTerm(version, term.bound, term.operator)
		if err != nil || !matched {
			return matched, err
		}
	}
	return true, nil
}

// compareTerm applies one version comparison term.
func compareTerm(version, bound, operator string) (bool, error) {
	base, ok := parseVersion(version)
	if !ok {
		return false, fmt.Errorf("version %q is not a dotted number", version)
	}
	target, ok := parseVersion(bound)
	if !ok {
		return false, fmt.Errorf("version %q is not a dotted number", bound)
	}
	order := compare(base, target)
	switch operator {
	case "", "=":
		return order == 0, nil
	case ">=":
		return order >= 0, nil
	case "<=":
		return order <= 0, nil
	case ">":
		return order > 0, nil
	case "<":
		return order < 0, nil
	case "~":
		return order >= 0 && compare(base, nextMinor(target)) < 0, nil
	case "^":
		return order >= 0 && compare(base, nextMajor(target)) < 0, nil
	default:
		return false, fmt.Errorf("unknown version operator %q", operator)
	}
}

// nextMinor is the exclusive upper bound of a tilde range.
func nextMinor(target []int) []int {
	major := target[0]
	minor := 0
	if len(target) > 1 {
		minor = target[1]
	}
	return []int{major, minor + 1, 0}
}

// nextMajor is the exclusive upper bound of a caret range; a zero major moves
// the first non-zero segment, as semver defines.
func nextMajor(target []int) []int {
	if len(target) > 0 && target[0] > 0 {
		return []int{target[0] + 1, 0, 0}
	}
	if len(target) > 1 && target[1] > 0 {
		return []int{0, target[1] + 1, 0}
	}
	patch := 0
	if len(target) > 2 {
		patch = target[2]
	}
	return []int{0, 0, patch + 1}
}

// compare orders two parsed versions, padding the shorter one with zeros.
func compare(left, right []int) int {
	length := max(len(left), len(right))
	for index := 0; index < length; index++ {
		leftPart := part(left, index)
		rightPart := part(right, index)
		switch {
		case leftPart > rightPart:
			return 1
		case leftPart < rightPart:
			return -1
		}
	}
	return 0
}

// part reads one version segment, treating a missing segment as zero.
func part(version []int, index int) int {
	if index >= len(version) {
		return 0
	}
	return version[index]
}

// parseVersion reads a dotted numeric version; a leading v and a pre-release or
// build suffix are ignored.
func parseVersion(text string) ([]int, bool) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "v"), "V")
	if cut, _, found := strings.Cut(text, "-"); found {
		text = cut
	}
	if cut, _, found := strings.Cut(text, "+"); found {
		text = cut
	}
	if text == "" {
		return nil, false
	}
	parts := strings.Split(text, ".")
	numbers := make([]int, 0, len(parts))
	for _, part := range parts {
		number, err := strconv.Atoi(part)
		if err != nil || number < 0 {
			return nil, false
		}
		numbers = append(numbers, number)
	}
	return numbers, true
}

// appendUnique appends item unless it is already present.
func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}
