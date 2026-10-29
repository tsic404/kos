package extension

import (
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/state"
)

// Component phases the install pipeline writes; the rest of C-28's phase set
// belongs to disable and uninstall, which are separate contracts.
const (
	PhaseReady       = "Ready"
	PhaseDegraded    = "Degraded"
	PhaseUnavailable = "Unavailable"
)

// Condition types a component status carries.
const (
	ConditionPackageVerified     = "PackageVerified"
	ConditionDependenciesMet     = "DependenciesMet"
	ConditionContributionsLoaded = "ContributionsLoaded"
)

// Reasons a component condition or a blocked reconcile carries.
const (
	ReasonAdopted             = "Adopted"
	ReasonVerified            = "Verified"
	ReasonDigestMismatch      = "DigestMismatch"
	ReasonPackageMissing      = "PackageMissing"
	ReasonPackageUnavailable  = "PackageUnavailable"
	ReasonInvalidDeclaration  = "InvalidDeclaration"
	ReasonDuplicateIdentifier = "DuplicateIdentifier"
	ReasonDependencyMissing   = "DependencyMissing"
	ReasonContributionFailed  = "ContributionFailed"
	ReasonLoaded              = "Loaded"
	ReasonMet                 = "Met"
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

// Evaluate applies the extension rules to the install requests: package
// digest, one version per identifier, the dependency fixpoint and class by
// class loading (C-17, C-18, C-28). It touches no cluster.
func Evaluate(requests []Request) Assessment {
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
		if owner, taken := identifiers[loaded.Identifier]; taken {
			unavailable(result, ReasonDuplicateIdentifier, "标识 "+loaded.Identifier+" 已由组件 "+owner+" 安装")
			continue
		}
		identifiers[loaded.Identifier] = request.Name
		candidates = append(candidates, candidate{name: request.Name, loaded: loaded})
	}

	active, pending := activate(candidates)
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
		result.Problems = append(append([]ClassProblem(nil), component.loaded.Problems...), conflicts[component.name]...)
		if len(result.Problems) > 0 {
			result.Phase = PhaseDegraded
			result.Health = v1beta1.HealthStateDegraded
			result.Reason = ReasonContributionFailed
			result.Message = problemMessage(result.Problems)
			continue
		}
		result.Phase = PhaseReady
		result.Health = v1beta1.HealthStateHealthy
		result.Message = "声明的贡献已全部生效"
	}

	results := make([]ComponentResult, 0, len(order))
	for _, name := range order {
		results = append(results, *outcomes[name])
	}
	return Assessment{Components: results, Registry: registry}
}

// candidate is a request whose package and declaration are usable.
type candidate struct {
	name   string
	loaded Loaded
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
			active = append(active, activated{name: component.name, loaded: component.loaded})
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
	switch result.Phase {
	case PhaseReady:
		conditions = state.Set(conditions, state.Applied(result.Generation, result.Message))
	default:
		conditions = state.Set(conditions, state.Blocked(result.Generation, result.Reason, result.Message))
	}
	if result.Reason != ReasonAdopted {
		conditions = state.Set(conditions, state.Condition(
			ConditionPackageVerified, conditionStatus(result.HasVerifiedPackage), result.Generation,
			packageReason(result), result.Message,
		))
		if result.HasVerifiedPackage && result.HasEvaluatedDependencies {
			conditions = state.Set(conditions, state.Condition(
				ConditionDependenciesMet, conditionStatus(result.HasMetDependencies), result.Generation,
				dependencyReason(result), dependencyMessage(result),
			))
			conditions = state.Set(conditions, state.Condition(
				ConditionContributionsLoaded, conditionStatus(result.HasMetDependencies && len(result.Problems) == 0),
				result.Generation, contributionReason(result), contributionMessage(result),
			))
		}
	}
	return v1beta1.ObjectStatus{
		Phase:      result.Phase,
		Health:     v1beta1.Health{State: result.Health},
		Conditions: conditions,
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
	if result.HasMetDependencies && len(result.Problems) == 0 {
		return ReasonLoaded
	}
	if result.HasMetDependencies {
		return ReasonContributionFailed
	}
	return ReasonDependencyMissing
}

// contributionMessage explains the contribution condition.
func contributionMessage(result ComponentResult) string {
	switch {
	case result.HasMetDependencies && len(result.Problems) == 0:
		return "声明的贡献已全部装载"
	case !result.HasMetDependencies:
		return "依赖缺失时不激活任何贡献"
	default:
		return problemMessage(result.Problems)
	}
}
