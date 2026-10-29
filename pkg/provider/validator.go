// Package provider validates the registration a system component declares
// (C-47) and keeps a provider with dependents in place (C-48).
//
// The validator is the Provider's only status writer: it judges the
// registration, runs the distribution's conformance case sets, writes the five
// conditions C-47 fixes and, while a deletion or a withdrawal is blocked,
// names the objects that block it.
package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Condition types the validator owns (C-47, C-48). A blocked deletion or a
// blocked withdrawal gets its own condition and never stands in for
// Reconciled (C-07).
const (
	// ConditionRegistered reports a registration the validator accepts.
	ConditionRegistered = "Registered"
	// ConditionVerified reports that a KOS run verified every capability.
	ConditionVerified = "Verified"
	// ConditionUpgradeBlocked reports a withdrawn capability that is still
	// referenced (C-48).
	ConditionUpgradeBlocked = "UpgradeBlocked"
	// ConditionReady reports Registered ∧ Verified ∧ ¬UpgradeBlocked (C-47).
	ConditionReady = "Ready"
	// ConditionDeprovisionBlocked reports a deletion a dependent blocks
	// (C-48).
	ConditionDeprovisionBlocked = "DeprovisionBlocked"
)

// Finalizer keeps a deleting provider observable until no dependent is left
// (C-48).
const Finalizer = "kos.tsic.top/provider"

// Reasons the validator writes. The conformance reasons are reused where the
// refusal belongs to the run rather than to the registration.
const (
	// ReasonRegistered is the reason of Registered=True.
	ReasonRegistered = "Registered"
	// ReasonInvalidRegistration is the reason of Registered=False.
	ReasonInvalidRegistration = "InvalidRegistration"
	// ReasonNoCapabilities is the reason of Verified=True for a provider that
	// contributes no capability (C-17, C-47).
	ReasonNoCapabilities = "NoCapabilities"
	// ReasonNotRegistered is the verification reason when the registration
	// itself is refused.
	ReasonNotRegistered = "NotRegistered"
	// ReasonWithdrawn is the reason of UpgradeBlocked=True.
	ReasonWithdrawn = "CapabilityWithdrawn"
	// ReasonNoWithdrawal is the reason of UpgradeBlocked=False.
	ReasonNoWithdrawal = "NoCapabilityWithdrawn"
	// ReasonReady is the reason of Ready=True.
	ReasonReady = "Ready"
	// ReasonDependents is the reason of DeprovisionBlocked=True.
	ReasonDependents = "DependentsRemain"
	// ReasonNoDependents is the reason of DeprovisionBlocked=False.
	ReasonNoDependents = "NoDependents"
)

// DefaultResync is the interval a settled provider is observed again: a
// withdrawal or a new dependent reaches the provider through its own bounded
// requeue, since the kernel has no watch source (C-06).
const DefaultResync = 30 * time.Second

// Options configures a Reconciler.
type Options struct {
	// Distribution supplies the case sets the kernel runs. Nil ships the
	// distribution's standard sets.
	Distribution *conformance.Distribution
	// Targets resolves the implementation of each provider. Nil runs no case
	// set, so only self-reported results exist (C-10).
	Targets conformance.Targets
	// Resync overrides DefaultResync.
	Resync time.Duration
}

// Reconciler validates provider registrations (C-47).
type Reconciler struct {
	writer *write.Writer
	runner *conformance.Runner
	resync time.Duration
	// mu guards runs; workers handle providers concurrently (C-05).
	mu sync.Mutex
	// runs keeps the last conformance round per provider name, so a resync
	// that changes nothing does not replay side-effecting cases (C-06, C-10).
	// The entry also records the object it belongs to: a deleted and re-created
	// provider reuses the name and the generation counter, so only the object
	// identity tells the two apart (C-10).
	runs map[string]cachedRun
}

// cachedRun is one provider object's last conformance round.
type cachedRun struct {
	identity   string
	generation int64
	signature  string
	results    []v1beta1.ProviderCapabilityResult
}

// NewReconciler builds a validator over one write path.
func NewReconciler(writer *write.Writer, options Options) *Reconciler {
	distribution := options.Distribution
	if distribution == nil {
		distribution = conformance.Standard()
	}
	resync := options.Resync
	if resync <= 0 {
		resync = DefaultResync
	}
	return &Reconciler{
		writer: writer,
		runner: conformance.NewRunner(distribution, options.Targets),
		resync: resync,
		runs:   make(map[string]cachedRun),
	}
}

// Reconcile runs one provider round: validation for a live provider, the
// dependent check for a deleting one.
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	if request.Kind != "Provider" {
		return reconcile.Result{}, nil
	}
	ref := providerRef(request.Name)
	object, err := r.writer.Get(ctx, ref)
	if apierrors.IsNotFound(err) {
		// The object is gone; anything the cache still holds under this name
		// belongs to it and must not answer for a later object (C-10).
		r.forget(request.Name)
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if object.GetDeletionTimestamp() != nil {
		return r.deprovision(ctx, object)
	}
	if !hasFinalizer(object) {
		finalizers := append(append([]string{}, object.GetFinalizers()...), Finalizer)
		if _, err := r.writer.SetFinalizers(ctx, write.SubjectValidator, ref, finalizers); err != nil {
			return reconcile.Result{}, err
		}
		if object, err = r.writer.Get(ctx, ref); err != nil {
			return reconcile.Result{}, err
		}
	}
	return r.validate(ctx, object)
}

// validate judges the registration, runs the case sets and writes the status.
func (r *Reconciler) validate(ctx context.Context, object *unstructured.Unstructured) (reconcile.Result, error) {
	generation := object.GetGeneration()
	spec, err := providerSpecOf(object)
	if err != nil {
		return reconcile.Result{}, err
	}
	previous, err := providerStatusOf(object)
	if err != nil {
		return reconcile.Result{}, err
	}
	conditions := append([]metav1.Condition(nil), previous.Conditions...)

	problems := registrationProblems(spec, previous.EffectiveCapabilities)
	registered := len(problems) == 0

	// The effective set is what applications compare against. A withdrawal is
	// judged against the previous set before, and independently of, the
	// registration: a refused registration withdraws nothing, and a blocked
	// withdrawal leaves the old set in service (C-48).
	effective := spec.Capabilities
	references := []v1beta1.ObjectRef{}
	upgradeBlocked := false
	if withdrawn := withdrawnFrom(previous.EffectiveCapabilities, spec.Capabilities); len(withdrawn) > 0 {
		references, err = r.references(ctx, withdrawn)
		if err != nil {
			return reconcile.Result{}, err
		}
		upgradeBlocked = len(references) > 0
	}
	if upgradeBlocked || !registered {
		effective = previous.EffectiveCapabilities
	}

	results := r.capabilityResults(ctx, object.GetName(), string(object.GetUID()), generation, spec, effective)

	registeredMessage := "the registration is accepted"
	if len(problems) > 0 {
		registeredMessage = "the registration is refused: " + strings.Join(problems, "; ")
	}
	verified, verifiedReason, verifiedMessage := verification(registered, results)
	withdrawalMessage := "no registered capability was withdrawn"
	if upgradeBlocked {
		withdrawalMessage = "withdrawn capabilities are still referenced: " + refsMessage(references)
	}
	ready := registered && verified && !upgradeBlocked
	readyReason, readyMessage := ReasonReady, "the provider is ready"
	switch {
	case !registered:
		readyReason, readyMessage = ReasonInvalidRegistration, registeredMessage
	case upgradeBlocked:
		readyReason, readyMessage = ReasonWithdrawn, withdrawalMessage
	case !verified:
		readyReason, readyMessage = verifiedReason, verifiedMessage
	}

	conditions = state.Set(conditions, condition(ConditionRegistered, registered, generation, registeredReason(registered), registeredMessage))
	conditions = state.Set(conditions, condition(ConditionVerified, verified, generation, verifiedReason, verifiedMessage))
	conditions = state.Set(conditions, condition(ConditionUpgradeBlocked, upgradeBlocked, generation, withdrawalReason(upgradeBlocked), withdrawalMessage))
	conditions = state.Set(conditions, condition(ConditionReady, ready, generation, readyReason, readyMessage))
	conditions = state.Set(conditions, condition(ConditionDeprovisionBlocked, false, generation, ReasonNoDependents, "no object depends on the provider"))
	conditions = state.Set(conditions, state.Applied(generation, "provider registration validated"))

	status := v1beta1.ProviderStatus{
		ObjectStatus: v1beta1.ObjectStatus{
			Health:     v1beta1.Health{State: healthOf(registered, ready)},
			Conditions: conditions,
		},
		Capabilities:          results,
		EffectiveCapabilities: effective,
		References:            references,
	}
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, object.GetName(), status)
}

// deprovision keeps a deleting provider in place while a dependent exists and
// names every one of them (C-48). The capability status is left untouched: a
// blocked deletion does not change what the provider serves.
func (r *Reconciler) deprovision(ctx context.Context, object *unstructured.Unstructured) (reconcile.Result, error) {
	generation := object.GetGeneration()
	spec, err := providerSpecOf(object)
	if err != nil {
		return reconcile.Result{}, err
	}
	previous, err := providerStatusOf(object)
	if err != nil {
		return reconcile.Result{}, err
	}
	dependents, err := r.dependents(ctx, object.GetName(), spec, previous)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(dependents) == 0 {
		return reconcile.Result{}, r.removeFinalizer(ctx, object)
	}

	message := "deletion is blocked by " + refsMessage(dependents)
	conditions := append([]metav1.Condition(nil), previous.Conditions...)
	conditions = state.Set(conditions, condition(ConditionDeprovisionBlocked, true, generation, ReasonDependents, message))
	conditions = state.Set(conditions, state.Blocked(generation, ReasonDependents, message))
	status := previous
	status.Conditions = conditions
	status.Dependents = dependents
	return reconcile.Result{RequeueAfter: r.resync}, r.writeStatus(ctx, object.GetName(), status)
}

// capabilityResults renders one result per effective capability. A round whose
// generation, effective set and self-reports are unchanged reuses the previous
// outcomes: the case sets provision and delete backend objects, so replaying
// them on every resync would churn a live cluster without new information
// (C-06, C-10).
func (r *Reconciler) capabilityResults(ctx context.Context, provider, identity string, generation int64, spec v1beta1.ProviderSpec, effective []v1beta1.ProviderCapability) []v1beta1.ProviderCapabilityResult {
	signature := runSignature(spec, effective)
	r.mu.Lock()
	cached, found := r.runs[provider]
	r.mu.Unlock()
	if found && cached.identity == identity && cached.generation == generation && cached.signature == signature {
		return cached.results
	}
	results := make([]v1beta1.ProviderCapabilityResult, 0, len(effective))
	interrupted := false
	for _, capability := range effective {
		outcome := r.runner.Run(ctx, provider, registrationOf(capability), selfReportFor(spec.SelfReported, capability))
		if outcome.Reason == conformance.ReasonInterrupted {
			interrupted = true
		}
		results = append(results, resultOf(capability, outcome))
	}
	// An interrupted round is not a conclusion; the next round runs again.
	if !interrupted {
		r.mu.Lock()
		r.runs[provider] = cachedRun{identity: identity, generation: generation, signature: signature, results: results}
		r.mu.Unlock()
	}
	return results
}

// forget drops the cached round of a provider object that no longer exists.
func (r *Reconciler) forget(provider string) {
	r.mu.Lock()
	delete(r.runs, provider)
	r.mu.Unlock()
}

// runSignature identifies what a conformance round depends on besides the
// distribution and the targets, which a Reconciler fixes at construction. The
// registration's own identity inputs belong here too: a declaration version or
// component change makes the previous outcomes stale even at the same
// generation.
func runSignature(spec v1beta1.ProviderSpec, effective []v1beta1.ProviderCapability) string {
	var builder strings.Builder
	builder.WriteString("component\n")
	builder.WriteString(spec.ComponentRef)
	builder.WriteByte('\n')
	builder.WriteString("version\n")
	builder.WriteString(spec.Version)
	builder.WriteByte('\n')
	for _, capability := range effective {
		builder.WriteString(capability.Name)
		builder.WriteByte('@')
		builder.WriteString(capability.Version)
		builder.WriteByte('/')
		builder.WriteString(capability.CaseSet)
		builder.WriteByte('#')
		builder.WriteString(capability.CaseSetDigest)
		builder.WriteByte('\n')
	}
	builder.WriteString("reports\n")
	for _, report := range spec.SelfReported {
		builder.WriteString(report.Capability)
		builder.WriteByte('@')
		builder.WriteString(report.Version)
		builder.WriteByte(':')
		builder.WriteString(strings.Join(report.FailedCases, ","))
		builder.WriteByte('\n')
	}
	return builder.String()
}

// registrationOf converts one registered capability into the runner's input.
func registrationOf(capability v1beta1.ProviderCapability) conformance.Registration {
	return conformance.Registration{
		Capability:    capability.Name,
		Version:       capability.Version,
		CaseSet:       capability.CaseSet,
		CaseSetDigest: capability.CaseSetDigest,
	}
}

// selfReportFor returns the provider's own result for one capability, if it
// declared one (C-10).
func selfReportFor(reports []v1beta1.ProviderSelfReport, capability v1beta1.ProviderCapability) *conformance.SelfReport {
	for _, report := range reports {
		if report.Capability == capability.Name && report.Version == capability.Version {
			return &conformance.SelfReport{FailedCases: report.FailedCases}
		}
	}
	return nil
}

// resultOf renders one capability outcome as the status entry the display
// reads (C-10).
func resultOf(capability v1beta1.ProviderCapability, outcome conformance.Outcome) v1beta1.ProviderCapabilityResult {
	return v1beta1.ProviderCapabilityResult{
		Name:        capability.Name,
		Version:     capability.Version,
		Source:      outcome.Source,
		State:       outcome.State,
		Reason:      outcome.Reason,
		FailedCases: outcome.Failed,
	}
}

// verification derives the Verified condition from the capability results
// (C-10): every effective capability must carry a KOS run that passed.
func verification(registered bool, results []v1beta1.ProviderCapabilityResult) (bool, string, string) {
	if !registered {
		return false, ReasonNotRegistered, "the registration is refused"
	}
	if len(results) == 0 {
		return true, ReasonNoCapabilities, "no capability is registered"
	}
	var unverified []string
	reason := conformance.ReasonVerified
	for _, result := range results {
		if result.State == conformance.StateVerified {
			continue
		}
		if reason == conformance.ReasonVerified {
			reason = result.Reason
		}
		unverified = append(unverified, fmt.Sprintf("%s@%s is %s", result.Name, result.Version, result.State))
	}
	if len(unverified) == 0 {
		return true, conformance.ReasonVerified, "every registered capability is verified"
	}
	return false, reason, "capabilities without a passing KOS run: " + strings.Join(unverified, ", ")
}

// registrationProblems reports every rule C-09 and C-47 put on a registration.
// previous is the effective set in service; C-09 freezes what a published
// capability version means.
func registrationProblems(spec v1beta1.ProviderSpec, previous []v1beta1.ProviderCapability) []string {
	var problems []string
	if strings.TrimSpace(spec.ComponentRef) == "" {
		problems = append(problems, "the registration names no owning component")
	}
	if strings.TrimSpace(spec.Version) == "" {
		problems = append(problems, "the registration names no provider version")
	}
	published := make(map[string]v1beta1.ProviderCapability, len(previous))
	for _, capability := range previous {
		published[capability.Name+"@"+capability.Version] = capability
	}
	storageClasses := map[string]bool{}
	for _, class := range spec.StorageClasses {
		if failures := validation.IsDNS1123Subdomain(class); len(failures) > 0 {
			problems = append(problems, fmt.Sprintf("storage class %q is not a DNS subdomain: %s", class, failures[0]))
			continue
		}
		if storageClasses[class] {
			problems = append(problems, fmt.Sprintf("storage class %q is declared twice", class))
		}
		storageClasses[class] = true
	}
	registered := map[string]bool{}
	hasStorageCapability := false
	for index, capability := range spec.Capabilities {
		where := fmt.Sprintf("capabilities[%d]", index)
		if strings.TrimSpace(capability.Name) == "" {
			problems = append(problems, where+": name is required")
		}
		if strings.TrimSpace(capability.Version) == "" {
			problems = append(problems, where+": version is required")
		}
		if strings.HasPrefix(capability.Name, "storage.") {
			hasStorageCapability = true
		}
		if !validScope(capability.Scope) {
			problems = append(problems, where+": the scope is neither cluster nor node")
		}
		if strings.TrimSpace(capability.CaseSet) == "" {
			problems = append(problems, where+": no conformance case set is named")
		}
		if strings.TrimSpace(capability.CaseSetDigest) == "" {
			problems = append(problems, where+": no conformance case set digest is named")
		}
		key := capability.Name + "@" + capability.Version
		if registered[key] {
			problems = append(problems, where+": "+key+" is registered twice")
		}
		registered[key] = true
		if before, found := published[key]; found && !sameMeaning(before, capability) {
			problems = append(problems, where+": "+key+" changes what a published capability promises; a semantic change needs a new version")
		}
		problems = append(problems, parameterProblems(where, capability.Parameters)...)
		problems = append(problems, guaranteeProblems(where, capability.Guarantees)...)
	}
	if hasStorageCapability && len(spec.StorageClasses) == 0 {
		problems = append(problems, "a storage capability needs at least one declared storage class")
	}
	return problems
}

// validScope reports whether a registration carries one of C-09's two scope
// values. An empty scope is refused: the registration path fills a baseline
// capability's scope, and an extension capability must declare one.
func validScope(scope v1beta1.CapabilityScope) bool {
	switch scope {
	case v1beta1.CapabilityScopeCluster, v1beta1.CapabilityScopeNode:
		return true
	}
	return false
}

// sameMeaning reports whether two declarations of one capability version carry
// the same promise (C-09). The case set digest is not compared: it pins the
// distribution's corpus and moves when the distribution does, which C-10
// already judges as an unverifiable mismatch rather than a semantic change.
func sameMeaning(before, after v1beta1.ProviderCapability) bool {
	if before.CaseSet != after.CaseSet || before.Scope != after.Scope {
		return false
	}
	return sameParameters(before.Parameters, after.Parameters) && sameGuarantees(before.Guarantees, after.Guarantees)
}

// sameParameters compares parameter structures by name, not by declaration
// order: reordering a structure changes nothing it promises.
func sameParameters(before, after []v1beta1.CapabilityParameter) bool {
	if len(before) != len(after) {
		return false
	}
	index := make(map[string]v1beta1.CapabilityParameter, len(before))
	for _, parameter := range before {
		index[parameter.Name] = parameter
	}
	for _, parameter := range after {
		other, found := index[parameter.Name]
		if !found || other.Type != parameter.Type || other.Required != parameter.Required {
			return false
		}
		if !sameStrings(other.Enum, parameter.Enum) {
			return false
		}
	}
	return true
}

// sameGuarantees compares guarantee levels by key: a boolean must hold the same
// value, an enum the same member.
func sameGuarantees(before, after []v1beta1.CapabilityGuarantee) bool {
	if len(before) != len(after) {
		return false
	}
	index := make(map[string]v1beta1.CapabilityGuarantee, len(before))
	for _, guarantee := range before {
		index[guarantee.Key] = guarantee
	}
	for _, guarantee := range after {
		other, found := index[guarantee.Key]
		if !found || other.Enum != guarantee.Enum {
			return false
		}
		switch {
		case other.Bool == nil && guarantee.Bool == nil:
		case other.Bool == nil || guarantee.Bool == nil:
			return false
		case *other.Bool != *guarantee.Bool:
			return false
		}
	}
	return true
}

// sameStrings compares two string sets, order aside.
func sameStrings(before, after []string) bool {
	if len(before) != len(after) {
		return false
	}
	counts := make(map[string]int, len(before))
	for _, value := range before {
		counts[value]++
	}
	for _, value := range after {
		if counts[value] == 0 {
			return false
		}
		counts[value]--
	}
	return true
}

// parameterProblems checks the parameter structure of one capability (C-09).
func parameterProblems(where string, parameters []v1beta1.CapabilityParameter) []string {
	var problems []string
	seen := map[string]bool{}
	for index, parameter := range parameters {
		at := fmt.Sprintf("%s.parameters[%d]", where, index)
		if strings.TrimSpace(parameter.Name) == "" {
			problems = append(problems, at+": name is required")
		}
		switch parameter.Type {
		case "string", "integer", "number", "boolean":
			if len(parameter.Enum) > 0 {
				problems = append(problems, at+": a "+parameter.Type+" parameter carries no enum values")
			}
		case "enum":
			if len(parameter.Enum) == 0 {
				problems = append(problems, at+": an enum parameter lists no value")
			}
		default:
			problems = append(problems, at+": type "+parameter.Type+" is not string, integer, number, boolean or enum")
		}
		if seen[parameter.Name] {
			problems = append(problems, at+": parameter "+parameter.Name+" is declared twice")
		}
		seen[parameter.Name] = true
	}
	return problems
}

// guaranteeProblems checks that every guarantee is a boolean or an enum, never
// prose and never both (C-09).
func guaranteeProblems(where string, guarantees []v1beta1.CapabilityGuarantee) []string {
	var problems []string
	seen := map[string]bool{}
	for index, guarantee := range guarantees {
		at := fmt.Sprintf("%s.guarantees[%d]", where, index)
		if strings.TrimSpace(guarantee.Key) == "" {
			problems = append(problems, at+": key is required")
		}
		hasBool := guarantee.Bool != nil
		hasEnum := guarantee.Enum != ""
		switch {
		case hasBool && hasEnum:
			problems = append(problems, at+": guarantee "+guarantee.Key+" is both boolean and enum")
		case !hasBool && !hasEnum:
			problems = append(problems, at+": guarantee "+guarantee.Key+" names neither a boolean nor an enum")
		}
		if seen[guarantee.Key] {
			problems = append(problems, at+": guarantee "+guarantee.Key+" is declared twice")
		}
		seen[guarantee.Key] = true
	}
	return problems
}

// withdrawnFrom lists the effective capabilities the current registration no
// longer declares (C-48).
func withdrawnFrom(previous, current []v1beta1.ProviderCapability) []v1beta1.ProviderCapability {
	present := make(map[string]bool, len(current))
	for _, capability := range current {
		present[capability.Name+"@"+capability.Version] = true
	}
	var withdrawn []v1beta1.ProviderCapability
	for _, capability := range previous {
		if !present[capability.Name+"@"+capability.Version] {
			withdrawn = append(withdrawn, capability)
		}
	}
	return withdrawn
}

// registeredReason names why Registered does or does not hold.
func registeredReason(registered bool) string {
	if registered {
		return ReasonRegistered
	}
	return ReasonInvalidRegistration
}

// withdrawalReason names why UpgradeBlocked does or does not hold.
func withdrawalReason(blocked bool) string {
	if blocked {
		return ReasonWithdrawn
	}
	return ReasonNoWithdrawal
}

// healthOf maps the two conditions onto the availability axis (C-08).
func healthOf(registered, ready bool) v1beta1.HealthState {
	switch {
	case ready:
		return v1beta1.HealthStateHealthy
	case !registered:
		return v1beta1.HealthStateUnavailable
	default:
		return v1beta1.HealthStateDegraded
	}
}

// condition builds one condition of the five the validator owns.
func condition(conditionType string, holds bool, generation int64, reason, message string) metav1.Condition {
	status := metav1.ConditionFalse
	if holds {
		status = metav1.ConditionTrue
	}
	return state.Condition(conditionType, status, generation, reason, message)
}

// providerRef names one provider object.
func providerRef(name string) write.Ref {
	return write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Provider", Name: name}
}

// providerSpecOf decodes the registration the component path wrote.
func providerSpecOf(object *unstructured.Unstructured) (v1beta1.ProviderSpec, error) {
	var spec v1beta1.ProviderSpec
	raw, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return spec, fmt.Errorf("read provider spec: %w", err)
	}
	if !found {
		return spec, nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		return v1beta1.ProviderSpec{}, fmt.Errorf("decode provider spec: %w", err)
	}
	return spec, nil
}

// providerStatusOf decodes the status this validator wrote earlier.
func providerStatusOf(object *unstructured.Unstructured) (v1beta1.ProviderStatus, error) {
	var status v1beta1.ProviderStatus
	raw, found, err := unstructured.NestedMap(object.Object, "status")
	if err != nil {
		return status, fmt.Errorf("read provider status: %w", err)
	}
	if !found {
		return status, nil
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &status); err != nil {
		return v1beta1.ProviderStatus{}, fmt.Errorf("decode provider status: %w", err)
	}
	return status, nil
}

// writeStatus applies the validator's status (C-03, C-07).
func (r *Reconciler) writeStatus(ctx context.Context, name string, status v1beta1.ProviderStatus) error {
	document, err := statusDocument(name, status)
	if err != nil {
		return err
	}
	_, err = r.writer.Apply(ctx, write.SubjectValidator, write.Session{}, write.PartStatus, document)
	return err
}

// statusDocument renders the status subresource document of one provider.
func statusDocument(name string, status v1beta1.ProviderStatus) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return nil, fmt.Errorf("encode provider status: %w", err)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Provider",
		"metadata":   map[string]any{"name": name},
		"status":     raw,
	}}, nil
}

// hasFinalizer reports whether the provider carries the deletion gate.
func hasFinalizer(object *unstructured.Unstructured) bool {
	for _, finalizer := range object.GetFinalizers() {
		if finalizer == Finalizer {
			return true
		}
	}
	return false
}

// removeFinalizer releases the deletion gate so the API server can remove the
// provider.
func (r *Reconciler) removeFinalizer(ctx context.Context, object *unstructured.Unstructured) error {
	remaining := make([]string, 0, len(object.GetFinalizers()))
	for _, finalizer := range object.GetFinalizers() {
		if finalizer != Finalizer {
			remaining = append(remaining, finalizer)
		}
	}
	_, err := r.writer.SetFinalizers(ctx, write.SubjectValidator, providerRef(object.GetName()), remaining)
	return err
}
