package extension

import (
	"errors"
	"sort"
	"strings"
)

// FrameworkVersion is the framework API version this kernel serves (C-16). A
// declaration whose target range admits none of it is incompatible.
const FrameworkVersion = "1.1"

// SupportedFrameworks returns the served framework API version set. It returns
// a fresh slice, so a caller that sorts or edits it cannot race another reader.
func SupportedFrameworks() []string {
	return []string{FrameworkVersion}
}

// ReasonCompatible is the reason of FrameworkCompatible=True.
const ReasonCompatible = "Compatible"

// compatibleFramework reports whether a declared target range is served by at
// least one supported framework version (C-16). An empty range admits every
// version.
func compatibleFramework(target string, supported []string) (bool, []string) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return true, nil
	}
	constraint, err := ParseConstraint(trimmed)
	if err != nil {
		return false, []string{"目标框架版本 " + trimmed + " 无法解析：" + err.Error()}
	}
	for _, version := range supported {
		if constraint.Allows(version) {
			return true, nil
		}
	}
	return false, []string{"目标框架版本 " + trimmed + " 与内核支持的版本 " + strings.Join(supported, ", ") + " 不兼容"}
}

// frameworkMessage renders the incompatible target list.
func frameworkMessage(incompatible []string) string {
	return "框架版本不兼容：" + strings.Join(incompatible, "; ")
}

// FrameworkDeclaration is one installed extension's framework target, as the
// upgrade preflight reads it (C-16).
type FrameworkDeclaration struct {
	// Component is the system component object name.
	Component string
	// Identifier is the extension identifier.
	Identifier string
	// FrameworkVersion is the declared target range.
	FrameworkVersion string
}

// FrameworkDependent is one installed extension the upgrade would strand.
type FrameworkDependent struct {
	Component        string
	Identifier       string
	FrameworkVersion string
}

// FrameworkPreflight is the outcome of a kernel upgrade preflight (C-16,
// C-55): the versions being removed, the extensions that depend on them and
// whether the upgrade is blocked.
type FrameworkPreflight struct {
	Target     []string
	Removed    []string
	Dependents []FrameworkDependent
	IsBlocked  bool
}

// PreflightUpgrade runs the preflight against the kernel's own framework
// versions.
func PreflightUpgrade(target []string, installed []FrameworkDeclaration) (FrameworkPreflight, error) {
	return PreflightUpgradeFrom(SupportedFrameworks(), target, installed)
}

// PreflightUpgradeFrom runs the preflight from an explicit current framework
// version set. An extension strands the upgrade when its target range admits a
// removed version and admits no target version; an extension that also runs on
// a target version does not block (C-16).
func PreflightUpgradeFrom(current, target []string, installed []FrameworkDeclaration) (FrameworkPreflight, error) {
	if len(target) == 0 {
		return FrameworkPreflight{}, errors.New("框架升级至少要给出一个目标版本")
	}
	preflight := FrameworkPreflight{
		Target:  append([]string(nil), target...),
		Removed: removedVersions(current, target),
	}
	if len(preflight.Removed) == 0 {
		return preflight, nil
	}
	for _, declaration := range installed {
		if !dependsOnRemovedFramework(declaration.FrameworkVersion, preflight.Removed, target) {
			continue
		}
		preflight.Dependents = append(preflight.Dependents, FrameworkDependent{
			Component:        declaration.Component,
			Identifier:       declaration.Identifier,
			FrameworkVersion: declaration.FrameworkVersion,
		})
	}
	sort.SliceStable(preflight.Dependents, func(left, right int) bool {
		return preflight.Dependents[left].Component < preflight.Dependents[right].Component
	})
	preflight.IsBlocked = len(preflight.Dependents) > 0
	return preflight, nil
}

// removedVersions lists the served versions the upgrade drops.
func removedVersions(current, target []string) []string {
	retained := make(map[string]bool, len(target))
	for _, version := range target {
		retained[version] = true
	}
	removed := make([]string, 0, len(current))
	for _, version := range current {
		if !retained[version] {
			removed = append(removed, version)
		}
	}
	return removed
}

// dependsOnRemovedFramework reports whether one target range only runs on the
// versions the upgrade removes. An empty range admits every version, and a
// range that also admits a target version survives the upgrade (C-16).
func dependsOnRemovedFramework(target string, removed, retained []string) bool {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return false
	}
	constraint, err := ParseConstraint(trimmed)
	if err != nil {
		// An unreadable range cannot prove the extension survives.
		return true
	}
	isRemoved := false
	for _, version := range removed {
		if constraint.Allows(version) {
			isRemoved = true
			break
		}
	}
	if !isRemoved {
		return false
	}
	for _, version := range retained {
		if constraint.Allows(version) {
			return false
		}
	}
	return true
}
