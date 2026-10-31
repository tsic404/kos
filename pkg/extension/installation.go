package extension

import (
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/state"
)

// Component phases the install pipeline writes (C-28). The seven values keep
// installed, ready, degraded, unavailable, incompatible, disabled and
// terminating states apart.
const (
	// PhaseInstalled is an installed component with no contribution declared,
	// so there is nothing to activate.
	PhaseInstalled = "Installed"
	// PhaseReady is a component whose declared contributions are active.
	PhaseReady = "Ready"
	// PhaseDegraded is a component with one or more failed contribution classes.
	PhaseDegraded = "Degraded"
	// PhaseUnavailable is a component that could not install or has an unmet
	// dependency.
	PhaseUnavailable = "Unavailable"
	// PhaseIncompatible is a component whose target framework version this
	// kernel does not serve (C-16).
	PhaseIncompatible = "Incompatible"
	// PhaseDisabled is a component whose contributions are withdrawn by its
	// disable request while its objects and data stay (C-28).
	PhaseDisabled = "Disabled"
	// PhaseTerminating is a component whose deletion is under way.
	PhaseTerminating = "Terminating"
)

// Condition types a component status carries.
const (
	ConditionPackageVerified     = "PackageVerified"
	ConditionFrameworkCompatible = "FrameworkCompatible"
	ConditionDependenciesMet     = "DependenciesMet"
	ConditionContributionsLoaded = "ContributionsLoaded"
	// ConditionDisableBlocked reports a disable request a dependent keeps from
	// taking effect (C-28).
	ConditionDisableBlocked = "DisableBlocked"
)

// Reasons a component condition or a blocked reconcile carries.
const (
	ReasonAdopted               = "Adopted"
	ReasonVerified              = "Verified"
	ReasonDigestMismatch        = "DigestMismatch"
	ReasonPackageMissing        = "PackageMissing"
	ReasonPackageUnavailable    = "PackageUnavailable"
	ReasonInvalidDeclaration    = "InvalidDeclaration"
	ReasonDuplicateIdentifier   = "DuplicateIdentifier"
	ReasonIncompatibleFramework = "IncompatibleFramework"
	ReasonDependencyMissing     = "DependencyMissing"
	ReasonContributionFailed    = "ContributionFailed"
	ReasonLoaded                = "Loaded"
	ReasonMet                   = "Met"
	ReasonDisabled              = "Disabled"
	ReasonInstalled             = "Installed"
	ReasonTerminating           = "Terminating"
	ReasonDisableBlocked        = "DisableBlocked"
)

// Request is one install request read from a live system component (C-17).
type Request struct {
	// Name is the system component object's name.
	Name string
	// Generation is the component's metadata generation.
	Generation int64
	// Created orders install requests; the earlier one keeps a contested
	// identifier (C-17).
	Created time.Time
	// IsAdopted marks a component taken over from existing infrastructure: it
	// installs no package and contributes nothing (C-17).
	IsAdopted bool
	// IsDisabled is the component's disable request (C-28).
	IsDisabled bool
	// WasDisabled is true when the stored status already recorded the disabled
	// phase, so this round keeps the contributions withdrawn instead of
	// treating the request as new (C-28).
	WasDisabled bool
	// IsTerminating marks a component whose deletion is under way (C-28).
	IsTerminating bool
	// Package is the fetched artifact; nil when the request names none.
	Package *Package
	// FetchError explains why the artifact could not be read.
	FetchError error
}

// ComponentResult is what the install pipeline concluded about one component.
type ComponentResult struct {
	Name       string
	Generation int64
	Identifier string
	Version    string
	// FrameworkVersion is the framework range the declaration targets; it is
	// reported, not judged here (C-18).
	FrameworkVersion string
	Phase            string
	Health           v1beta1.HealthState
	// HasVerifiedPackage reports that the digest and the declaration passed.
	HasVerifiedPackage bool
	// HasEvaluatedDependencies reports that the dependency fixpoint ran for
	// this component.
	HasEvaluatedDependencies bool
	// HasMetDependencies reports that every declared edge is satisfied.
	HasMetDependencies bool
	// Missing lists the unmet dependency edges.
	Missing []string
	// Incompatible lists the target framework versions this kernel does not
	// serve (C-16).
	Incompatible []string
	// DisableBlockedBy lists the components that keep a disable request from
	// taking effect (C-28).
	DisableBlockedBy []string
	// Problems lists the contribution classes that did not load.
	Problems []ClassProblem
	// Contributions is what this component actually activated.
	Contributions Contributions
	// Retention is the declared fate of each withdrawn contribution's data
	// (C-18, C-29).
	Retention []Retention
	// Reason and Message explain a phase other than Ready.
	Reason  string
	Message string
}

// Assessment is the outcome of one install evaluation over every live
// component.
type Assessment struct {
	Components []ComponentResult
	Registry   *Registry
}

// Evaluate applies the extension rules to the install requests against the
// kernel's own framework versions (C-16, C-28).
func Evaluate(requests []Request) Assessment {
	return EvaluateWith(requests, SupportedFrameworks())
}

// EvaluateWith applies the extension rules to the install requests: package
// digest, framework compatibility, one version per identifier, disable,
// deletion, the dependency fixpoint and class by class loading (C-16, C-17,
// C-18, C-28). It touches no cluster.
func EvaluateWith(requests []Request, frameworks []string) Assessment {
	ordered := append([]Request(nil), requests...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if !ordered[left].Created.Equal(ordered[right].Created) {
			return ordered[left].Created.Before(ordered[right].Created)
		}
		return ordered[left].Name < ordered[right].Name
	})

	outcomes := make(map[string]*ComponentResult, len(ordered))
	order := make([]string, 0, len(ordered))
	candidates := make([]candidate, 0, len(ordered))
	identifiers := map[string]string{}

	for _, request := range ordered {
		result := &ComponentResult{Name: request.Name, Generation: request.Generation}
		outcomes[request.Name] = result
		order = append(order, request.Name)
		if request.IsAdopted {
			result.Phase = PhaseReady
			result.Health = v1beta1.HealthStateHealthy
			result.Reason = ReasonAdopted
			result.Message = "已接管组件不安装包，也不贡献对象类、页面或控制器"
			continue
		}
		if request.FetchError != nil {
			unavailable(result, ReasonPackageUnavailable, "扩展包不可读取："+request.FetchError.Error())
			continue
		}
		if request.Package == nil || request.Package.Reference == "" {
			unavailable(result, ReasonPackageMissing, "组件没有声明扩展包")
			continue
		}
		if err := request.Package.Verify(); err != nil {
			unavailable(result, ReasonDigestMismatch, err.Error())
			continue
		}
		result.HasVerifiedPackage = true
		declaration, err := Parse(request.Package.Content)
		if err != nil {
			unavailable(result, ReasonInvalidDeclaration, err.Error())
			continue
		}
		loaded, err := declaration.Load()
		if err != nil {
			unavailable(result, ReasonInvalidDeclaration, err.Error())
			continue
		}
		result.Identifier = loaded.Identifier
		result.Version = loaded.Version
		result.FrameworkVersion = loaded.FrameworkVersion
		if ok, incompatible := compatibleFramework(loaded.FrameworkVersion, frameworks); !ok {
			result.Incompatible = incompatible
			result.Phase = PhaseIncompatible
			result.Health = v1beta1.HealthStateUnavailable
			result.Reason = ReasonIncompatibleFramework
			result.Message = frameworkMessage(incompatible)
			continue
		}
		if owner, taken := identifiers[loaded.Identifier]; taken {
			unavailable(result, ReasonDuplicateIdentifier, "标识 "+loaded.Identifier+" 已由组件 "+owner+" 安装")
			continue
		}
		identifiers[loaded.Identifier] = request.Name
		candidates = append(candidates, candidate{
			name:          request.Name,
			loaded:        loaded,
			isDisabled:    request.IsDisabled,
			wasDisabled:   request.WasDisabled,
			isTerminating: request.IsTerminating,
		})
	}

	// A disable request takes effect only when no component that still runs
	// depends on the component (C-28). Coverage comes from the components that
	// stay active, never from the ones being disabled or blocked by their own
	// dependencies; blocked requests join the running set, so the outcome is a
	// fixpoint rather than a decision read off the pre-disable candidates.
	running := make([]candidate, 0, len(candidates))
	requested := make([]candidate, 0)
	keptDisabled := make([]candidate, 0)
	for _, component := range candidates {
		switch {
		case component.isDisabled && component.wasDisabled:
			// The disable already took effect; it is not a new request and its
			// withdrawn contributions cover nothing.
			keptDisabled = append(keptDisabled, component)
		case component.isDisabled:
			requested = append(requested, component)
		default:
			running = append(running, component)
		}
	}
	blocked, disabled := resolveDisables(requested, running)
	disabled = append(disabled, keptDisabled...)
	enabled := append(running, blocked...)
	for _, component := range disabled {
		result := outcomes[component.name]
		result.Phase = PhaseDisabled
		result.Health = v1beta1.HealthStateUnavailable
		result.Reason = ReasonDisabled
		result.Message = "组件已停用，声明的贡献已撤除，对象与数据保留"
	}

	active, pending := activate(enabled)
	for _, inactive := range pending {
		result := outcomes[inactive.name]
		result.HasEvaluatedDependencies = true
		result.Missing = catalogueFor(active).missingDependencies(inactive.loaded.Dependencies)
		unavailable(result, ReasonDependencyMissing, "依赖未满足："+strings.Join(result.Missing, "; "))
	}

	registry, effective, conflicts := buildRegistry(active)
	for _, component := range active {
		result := outcomes[component.name]
		result.HasEvaluatedDependencies = true
		result.HasMetDependencies = true
		result.Contributions = effective[component.name]
		result.Retention = component.loaded.Retention
		result.DisableBlockedBy = component.disableBlockedBy
		result.Problems = append(append([]ClassProblem(nil), component.loaded.Problems...), conflicts[component.name]...)
		if len(result.Problems) > 0 {
			result.Phase = PhaseDegraded
			result.Health = v1beta1.HealthStateDegraded
			result.Reason = ReasonContributionFailed
			result.Message = problemMessage(result.Problems)
			continue
		}
		result.Health = v1beta1.HealthStateHealthy
		if !hasContribution(result.Contributions) {
			result.Phase = PhaseInstalled
			result.Reason = ReasonInstalled
			result.Message = "组件已安装，未声明可激活的贡献"
			continue
		}
		result.Phase = PhaseReady
		result.Message = "声明的贡献已全部生效"
	}

	// A held deletion stays terminating while its dependents keep it installed:
	// the gate releases an unblocked deletion in the same pass, so a terminating
	// component here is one a dependent still holds open (C-28, C-29).
	for _, request := range ordered {
		if !request.IsTerminating {
			continue
		}
		result := outcomes[request.Name]
		result.Phase = PhaseTerminating
		result.Reason = ReasonTerminating
		result.Message = "组件正在卸载，依赖解除前声明的贡献保持生效"
	}
	results := make([]ComponentResult, 0, len(order))
	for _, name := range order {
		results = append(results, *outcomes[name])
	}
	return Assessment{Components: results, Registry: registry}
}

// resolveDisables decides which disable requests take effect. A request is
// blocked while a component that still runs depends on its contribution; the
// coverage test reads only the active set, so a component that is disabled or
// unable to run never counts as a substitute (C-28). Blocked components join
// the running set and the pass repeats until no further request is blocked.
func resolveDisables(requested, running []candidate) (blocked, disabled []candidate) {
	remaining := append([]candidate(nil), requested...)
	for {
		current := append(append([]candidate(nil), running...), blocked...)
		active, _ := activate(current)
		changed := false
		nextRemaining := make([]candidate, 0, len(remaining))
		for _, target := range remaining {
			dependents := dependentsOnActive(target, current, active)
			if len(dependents) == 0 {
				nextRemaining = append(nextRemaining, target)
				continue
			}
			target.disableBlockedBy = dependents
			blocked = append(blocked, target)
			changed = true
		}
		remaining = nextRemaining
		if !changed {
			return blocked, remaining
		}
	}
}

// dependentsOnActive lists the components that still depend on the target when
// the current active set is in service. A component edge always counts; a
// capability edge counts while no active component serves it (C-28).
func dependentsOnActive(target candidate, current []candidate, active []activated) []string {
	available := catalogueFor(active)
	dependents := make([]string, 0)
	for _, other := range current {
		if other.name == target.name {
			continue
		}
		if hasComponentEdge(target, other) {
			dependents = append(dependents, other.loaded.Identifier)
			continue
		}
		for _, requirement := range other.loaded.Dependencies.Capabilities {
			if !contributesCapability(target.loaded, requirement) {
				continue
			}
			if requirementServed(available, requirement) {
				continue
			}
			dependents = append(dependents, other.loaded.Identifier)
			break
		}
	}
	sort.Strings(dependents)
	return dedupeStrings(dependents)
}

// requirementServed reports whether the active catalogue satisfies one
// capability requirement.
func requirementServed(available catalogue, requirement CapabilityRequirement) bool {
	constraint, err := ParseConstraint(requirement.Versions)
	return err == nil && available.hasCapability(requirement.Name, constraint)
}

// dedupeStrings drops repeats from a sorted list.
func dedupeStrings(items []string) []string {
	kept := items[:0]
	for _, item := range items {
		if len(kept) > 0 && kept[len(kept)-1] == item {
			continue
		}
		kept = append(kept, item)
	}
	return kept
}

// hasComponentEdge reports whether the other component requires the target's
// identifier at the target's version.
func hasComponentEdge(target, other candidate) bool {
	for _, requirement := range other.loaded.Dependencies.Components {
		if requirement.Identifier != target.loaded.Identifier {
			continue
		}
		constraint, err := ParseConstraint(requirement.Versions)
		if err == nil && constraint.Allows(target.loaded.Version) {
			return true
		}
	}
	return false
}

// contributesCapability reports whether one declaration serves a requirement.
func contributesCapability(loaded Loaded, requirement CapabilityRequirement) bool {
	constraint, err := ParseConstraint(requirement.Versions)
	if err != nil {
		return false
	}
	for _, capability := range loaded.Contributions.Capabilities.Items {
		if capability.Name == requirement.Name && constraint.Allows(capability.Version) {
			return true
		}
	}
	return false
}

// hasContribution reports whether one loaded declaration declares any
// contribution at all. An extension that declares none is installed and
// registered for dependency protection (C-17), which is PhaseInstalled.
func hasContribution(contributions Contributions) bool {
	return len(contributions.ObjectClasses) > 0 ||
		len(contributions.Controllers) > 0 ||
		len(contributions.Capabilities.Items) > 0 ||
		len(contributions.Capabilities.StorageClasses) > 0 ||
		len(contributions.Capabilities.SelfReported) > 0 ||
		len(contributions.Pages) > 0 ||
		len(contributions.Settings) > 0 ||
		len(contributions.CatalogueSources) > 0 ||
		len(contributions.DiscoveryAdapters) > 0 ||
		len(contributions.DiagnosticAdapters) > 0 ||
		len(contributions.Preferences) > 0
}

// candidate is a request whose package and declaration are usable.
type candidate struct {
	name             string
	loaded           Loaded
	isDisabled       bool
	wasDisabled      bool
	isTerminating    bool
	disableBlockedBy []string
}

// activate runs the dependency fixpoint: a component joins the active set only
// when every edge is met by the baseline or by a component already active
// (C-28). Everything left over is unavailable, never partially active.
func activate(candidates []candidate) ([]activated, []candidate) {
	active := make([]activated, 0, len(candidates))
	pending := append([]candidate(nil), candidates...)
	for {
		available := catalogueFor(active)
		var remaining []candidate
		for _, component := range pending {
			if len(available.missingDependencies(component.loaded.Dependencies)) > 0 {
				remaining = append(remaining, component)
				continue
			}
			active = append(active, activated{name: component.name, loaded: component.loaded, disableBlockedBy: component.disableBlockedBy})
		}
		if len(remaining) == len(pending) {
			return active, remaining
		}
		pending = remaining
	}
}

// unavailable marks a result that cannot contribute anything.
func unavailable(result *ComponentResult, reason, message string) {
	result.Phase = PhaseUnavailable
	result.Health = v1beta1.HealthStateUnavailable
	result.Reason = reason
	result.Message = message
}

// problemMessage renders the failed classes of one component.
func problemMessage(problems []ClassProblem) string {
	parts := make([]string, 0, len(problems))
	for _, problem := range problems {
		parts = append(parts, problem.Class+": "+strings.Join(problem.Items, ", "))
	}
	return "贡献类未生效：" + strings.Join(parts, "; ")
}

// StatusFor renders the component status of one result. existing keeps the
// transition times of unchanged conditions, so repeating an install over an
// unchanged state writes nothing (C-06, C-07).
func StatusFor(result ComponentResult, existing []metav1.Condition) v1beta1.ObjectStatus {
	conditions := append([]metav1.Condition(nil), existing...)
	conditions = withoutCondition(conditions, ConditionDisableBlocked)
	conditions = withoutCondition(conditions, ConditionFrameworkCompatible)
	switch result.Phase {
	case PhaseReady, PhaseInstalled, PhaseDisabled:
		// The ready, installed and disabled desired states are all applied;
		// the phase says which one it is.
		conditions = state.Set(conditions, state.Applied(result.Generation, result.Message))
	default:
		conditions = state.Set(conditions, state.Blocked(result.Generation, result.Reason, result.Message))
	}
	if result.Reason != ReasonAdopted {
		conditions = state.Set(conditions, state.Condition(
			ConditionPackageVerified, conditionStatus(result.HasVerifiedPackage), result.Generation,
			packageReason(result), result.Message,
		))
		if strings.TrimSpace(result.FrameworkVersion) != "" || result.Phase == PhaseIncompatible {
			conditions = state.Set(conditions, state.Condition(
				ConditionFrameworkCompatible, conditionStatus(len(result.Incompatible) == 0), result.Generation,
				frameworkReason(result), frameworkMessageInStatus(result),
			))
		}
		if result.HasVerifiedPackage && result.HasEvaluatedDependencies {
			conditions = state.Set(conditions, state.Condition(
				ConditionDependenciesMet, conditionStatus(result.HasMetDependencies), result.Generation,
				dependencyReason(result), dependencyMessage(result),
			))
			conditions = state.Set(conditions, state.Condition(
				ConditionContributionsLoaded, conditionStatus(contributionsLoaded(result)), result.Generation,
				contributionReason(result), contributionMessage(result),
			))
		} else if result.Phase == PhaseDisabled || result.Phase == PhaseTerminating || result.Phase == PhaseIncompatible {
			conditions = state.Set(conditions, state.Condition(
				ConditionContributionsLoaded, metav1.ConditionFalse, result.Generation,
				result.Reason, result.Message,
			))
		}
	}
	if len(result.DisableBlockedBy) > 0 {
		conditions = state.Set(conditions, state.Condition(
			ConditionDisableBlocked, metav1.ConditionTrue, result.Generation, ReasonDisableBlocked,
			"停用被依赖阻塞：依赖者 "+strings.Join(result.DisableBlockedBy, ", "),
		))
	}
	return v1beta1.ObjectStatus{
		Phase:      result.Phase,
		Health:     v1beta1.Health{State: result.Health},
		Conditions: conditions,
	}
}

// withoutCondition drops one condition type from a list.
func withoutCondition(conditions []metav1.Condition, conditionType string) []metav1.Condition {
	kept := make([]metav1.Condition, 0, len(conditions))
	for _, condition := range conditions {
		if condition.Type == conditionType {
			continue
		}
		kept = append(kept, condition)
	}
	return kept
}

// contributionsLoaded reports whether every declared contribution class is
// active in this result.
func contributionsLoaded(result ComponentResult) bool {
	switch result.Phase {
	case PhaseReady, PhaseInstalled, PhaseTerminating:
		return result.HasMetDependencies && len(result.Problems) == 0
	default:
		return false
	}
}

// conditionStatus maps a boolean to the condition value.
func conditionStatus(holds bool) metav1.ConditionStatus {
	if holds {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// packageReason names why a package did not pass verification.
func packageReason(result ComponentResult) string {
	if result.HasVerifiedPackage {
		return ReasonVerified
	}
	return result.Reason
}

// frameworkReason names why the framework compatibility check passed or failed.
func frameworkReason(result ComponentResult) string {
	if len(result.Incompatible) == 0 {
		return ReasonCompatible
	}
	return ReasonIncompatibleFramework
}

// frameworkMessageInStatus renders the framework check result.
func frameworkMessageInStatus(result ComponentResult) string {
	if len(result.Incompatible) == 0 {
		return "目标框架版本在当前内核支持的版本范围内"
	}
	return result.Message
}

// dependencyReason names why the dependency check did not pass.
func dependencyReason(result ComponentResult) string {
	if result.HasMetDependencies {
		return ReasonMet
	}
	return ReasonDependencyMissing
}

// dependencyMessage lists the unmet edges, or states that every edge is met.
func dependencyMessage(result ComponentResult) string {
	if result.HasMetDependencies {
		return "声明的依赖均已满足"
	}
	if len(result.Missing) > 0 {
		return "依赖未满足：" + strings.Join(result.Missing, "; ")
	}
	return result.Message
}

// contributionReason names why contributions are not all active.
func contributionReason(result ComponentResult) string {
	switch result.Phase {
	case PhaseDisabled:
		return ReasonDisabled
	}
	if contributionsLoaded(result) {
		return ReasonLoaded
	}
	if result.HasMetDependencies {
		return ReasonContributionFailed
	}
	return ReasonDependencyMissing
}

// contributionMessage explains the contribution condition.
func contributionMessage(result ComponentResult) string {
	if result.Phase == PhaseDisabled {
		return "组件已停用，声明的贡献已撤除"
	}
	switch {
	case contributionsLoaded(result):
		return "声明的贡献已全部装载"
	case !result.HasMetDependencies:
		return "依赖缺失时不激活任何贡献"
	default:
		return problemMessage(result.Problems)
	}
}
