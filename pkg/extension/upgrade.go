package extension

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// UpgradeTarget names the package one installed component moves to (C-55).
type UpgradeTarget struct {
	Component string
	Reference string
	Digest    string
}

// ComponentUpgrade is one component's upgrade preflight (C-55): the package it
// moves from and to, the item-by-item contribution difference, and the
// problems the same evaluation an install runs reports for the target.
type ComponentUpgrade struct {
	Component string               `json:"component"`
	From      string               `json:"from"`
	To        string               `json:"to"`
	Changes   []ContributionChange `json:"changes,omitempty"`
	Problems  []string             `json:"problems,omitempty"`
	IsBlocked bool                 `json:"isBlocked,omitempty"`
}

// UpgradePreflight is the whole answer: one entry per target and whether any
// target is blocked.
type UpgradePreflight struct {
	Components []ComponentUpgrade `json:"components"`
	IsBlocked  bool               `json:"isBlocked,omitempty"`
}

// UpgradePreflight judges a declaration replacement before it is applied
// (C-55). The live components are read exactly as an install pass reads them,
// each target's package replaces the component's current one, and the
// resulting request set goes through the same evaluation an install runs. The
// preflight writes nothing.
func (i *Installer) UpgradePreflight(ctx context.Context, targets []UpgradeTarget) (UpgradePreflight, error) {
	if i.Dynamic == nil || i.Artifacts == nil {
		return UpgradePreflight{}, errors.New("upgrade preflight needs a dynamic client and an artifact source")
	}
	read, err := i.readComponents(ctx, nil)
	if err != nil {
		return UpgradePreflight{}, err
	}
	installed := make(map[string]Request, len(read.requests))
	for _, request := range read.requests {
		installed[request.Name] = request
	}

	requests := append([]Request(nil), read.requests...)
	replacements := make(map[string]Request, len(targets))
	currentContributions := make(map[string]Contributions, len(targets))
	targetContributions := make(map[string]Contributions, len(targets))
	upgrades := make([]ComponentUpgrade, 0, len(targets))
	for _, target := range targets {
		current, ok := installed[target.Component]
		if !ok {
			return UpgradePreflight{}, fmt.Errorf("component %s is not an installed system component", target.Component)
		}
		if current.IsAdopted {
			return UpgradePreflight{}, fmt.Errorf("component %s is adopted and installs no package", target.Component)
		}
		if _, duplicate := replacements[target.Component]; duplicate {
			return UpgradePreflight{}, fmt.Errorf("component %s is listed more than once", target.Component)
		}
		declaration, err := installedDeclaration(target.Component, current)
		if err != nil {
			return UpgradePreflight{}, err
		}
		request, contributions, hasDeclaration := i.upgradedRequest(ctx, current, target)
		replacements[target.Component] = request
		currentContributions[target.Component] = declaration.Contributions
		if hasDeclaration {
			targetContributions[target.Component] = contributions
		}
		upgrades = append(upgrades, ComponentUpgrade{
			Component: target.Component,
			From:      current.Package.Reference,
			To:        target.Reference,
		})
	}
	for index := range requests {
		if request, ok := replacements[requests[index].Name]; ok {
			requests[index] = request
		}
	}

	assessment := EvaluateWith(requests, i.frameworks())
	results := make(map[string]ComponentResult, len(assessment.Components))
	for _, result := range assessment.Components {
		results[result.Name] = result
	}

	preflight := UpgradePreflight{Components: make([]ComponentUpgrade, 0, len(upgrades))}
	for _, upgrade := range upgrades {
		result, evaluated := results[upgrade.Component]
		if evaluated {
			upgrade.Problems = upgradeProblems(result)
		}
		upgrade.IsBlocked = len(upgrade.Problems) > 0 || (evaluated && blocksUpgrade(result))
		if contributions, hasDeclaration := targetContributions[upgrade.Component]; hasDeclaration {
			upgrade.Changes = ContributionDiff(currentContributions[upgrade.Component], contributions)
		}
		if upgrade.IsBlocked {
			preflight.IsBlocked = true
		}
		preflight.Components = append(preflight.Components, upgrade)
	}
	return preflight, nil
}

// installedDeclaration parses the package an installed component names. The
// current declaration is what the diff compares against, so a component whose
// package cannot be read cannot be preflighted.
func installedDeclaration(name string, request Request) (Declaration, error) {
	if request.FetchError != nil {
		return Declaration{}, fmt.Errorf("component %s: read installed package: %w", name, request.FetchError)
	}
	if request.Package == nil || request.Package.Reference == "" {
		return Declaration{}, fmt.Errorf("component %s: installed package is missing", name)
	}
	declaration, err := Parse(request.Package.Content)
	if err != nil {
		return Declaration{}, fmt.Errorf("component %s: %w", name, err)
	}
	return declaration, nil
}

// upgradedRequest swaps one component's package for the target package. A
// package that cannot be fetched, does not match the named digest or does not
// parse keeps its place in the evaluation, which then reports it as the install
// pass would; only a verified declaration yields contributions to diff.
func (i *Installer) upgradedRequest(ctx context.Context, current Request, target UpgradeTarget) (Request, Contributions, bool) {
	request := current
	request.Package = nil
	request.FetchError = nil
	if target.Reference == "" {
		request.FetchError = errors.New("the target package names no reference")
		return request, Contributions{}, false
	}
	content, err := i.Artifacts.Fetch(ctx, target.Reference)
	if err != nil {
		request.FetchError = err
		return request, Contributions{}, false
	}
	request.Package = &Package{Reference: target.Reference, Digest: target.Digest, Content: content}
	if err := request.Package.Verify(); err != nil {
		return request, Contributions{}, false
	}
	declaration, err := Parse(content)
	if err != nil {
		return request, Contributions{}, false
	}
	return request, declaration.Contributions, true
}

// upgradeProblems renders the evaluation problems of one target: unmet
// dependency edges, incompatible framework versions, failed contribution
// classes and, when none of those is itemized, the reason the component could
// not be installed.
func upgradeProblems(result ComponentResult) []string {
	problems := make([]string, 0, len(result.Missing)+len(result.Incompatible)+len(result.Problems)+1)
	for _, edge := range result.Missing {
		problems = append(problems, "依赖未满足："+edge)
	}
	for _, version := range result.Incompatible {
		problems = append(problems, "框架版本不兼容："+version)
	}
	for _, problem := range result.Problems {
		problems = append(problems, "贡献类 "+problem.Class+" 未生效："+strings.Join(problem.Items, ", "))
	}
	if len(problems) == 0 && blocksUpgrade(result) && result.Message != "" {
		problems = append(problems, result.Message)
	}
	return problems
}

// blocksUpgrade reports whether the install pass left the target in a state
// that neither installs its contributions nor records a deliberate withdrawal:
// a disabled or held terminating component keeps its current state instead.
func blocksUpgrade(result ComponentResult) bool {
	switch result.Phase {
	case PhaseReady, PhaseInstalled, PhaseDisabled, PhaseTerminating:
		return false
	}
	return true
}
