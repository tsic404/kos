package endpoint

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Failure condition types of an endpoint. They keep their own type and never
// stand in for Reconciled (C-07).
const (
	// ConditionInvalidSpec marks an endpoint whose shape C-43 rejects.
	ConditionInvalidSpec = "InvalidSpec"
	// ConditionConflict marks an endpoint whose ledger key is taken or whose
	// target application is gone (C-44, C-45).
	ConditionConflict = "Conflict"
	// ConditionRouteBlocked marks an endpoint whose route object could not be
	// written, so its desired state is not fully applied (C-07, C-43).
	ConditionRouteBlocked = "RouteBlocked"
	// ConditionApplicationPaused marks an endpoint whose target application is
	// stopped (C-66).
	ConditionApplicationPaused = "ApplicationPaused"
)

// Reasons of the two failure conditions.
const (
	// ReasonInvalidSpec is the reason of a rejected shape.
	ReasonInvalidSpec = "InvalidSpec"
	// ReasonConflict is the reason of a duplicate key, an exclusive host or a
	// missing target application.
	ReasonConflict = "Conflict"
	// ReasonRouteBlocked is the reason of a route object that could not be
	// written.
	ReasonRouteBlocked = "RouteBlocked"
	// ReasonApplicationPaused is the reason of a target application that is
	// stopped (C-66).
	ReasonApplicationPaused = "ApplicationPaused"
)

// Lister reads the objects the ledger is rebuilt from.
type Lister interface {
	// List returns every object of a kind; an empty namespace means all of
	// them.
	List(ctx context.Context, kind, namespace string) ([]*unstructured.Unstructured, error)
}

// Reconciler keeps the endpoint ledger, the status of every endpoint and the
// route object C-04 allocates to this subject (C-43, C-44, C-45). The route
// object follows ADR-024: an Ingress for the two HTTP shapes, a Service for the
// two L4 shapes, nothing for mdns.
type Reconciler struct {
	writer *write.Writer
	facts  Facts
	ledger *Ledger
	lister Lister

	mu         sync.Mutex
	rebuilt    bool
	generation uint64
	settled    uint64
	// pending holds the entries a failed round could not settle, with the
	// retry budget of each. The ledger reports a peer as moved only once, so a
	// transient failure would otherwise leave that peer behind (C-06).
	pending map[string]*pendingEntry
	// refused holds the entries whose route write a policy refused, with the
	// time of the next look. It is not a retry budget: a refusal waits on
	// another object, and that object is observed by nobody (C-06, C-07).
	refused map[string]*refusedEntry
	// Now supplies the retry clock; nil means the wall clock.
	Now func() time.Time
}

// NewReconciler builds a reconciler over one cluster. A nil facts means neither
// path-prefix declaration is readable, and C-43 then refuses that shape.
func NewReconciler(writer *write.Writer, facts Facts) *Reconciler {
	return &Reconciler{
		writer:  writer,
		facts:   facts,
		ledger:  NewLedger(),
		lister:  clusterLister{client: writer.Dynamic, resources: writer.Resources},
		pending: map[string]*pendingEntry{},
		refused: map[string]*refusedEntry{},
	}
}

// Ledger returns the allocation projection this reconciler owns.
func (r *Reconciler) Ledger() *Ledger {
	return r.ledger
}

// Rebuild recreates the ledger from the stored endpoints and applications. It
// is what the reconciler does on start: the ledger is derived state, never a
// source of truth (C-44). The next round settles every entry against its stored
// status, so a restart cannot leave a peer's condition behind.
func (r *Reconciler) Rebuild(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rebuild(ctx)
}

// rebuild replaces the ledger under the reconciler's lock.
func (r *Reconciler) rebuild(ctx context.Context) error {
	endpoints, err := r.lister.List(ctx, "Endpoint", "")
	if err != nil {
		return fmt.Errorf("list endpoints: %w", err)
	}
	applications, err := r.lister.List(ctx, "Application", "")
	if err != nil {
		return fmt.Errorf("list applications: %w", err)
	}
	targets := make(map[string]targetState, len(applications))
	for _, application := range applications {
		paused, _, _ := unstructured.NestedBool(application.Object, "spec", "paused")
		targets[application.GetNamespace()+"/"+application.GetName()] = targetState{present: true, paused: paused}
	}
	entries := make([]Entry, 0, len(endpoints))
	for _, object := range endpoints {
		entry, err := r.entry(ctx, object, targets)
		if err != nil {
			return fmt.Errorf("read endpoint %s: %w", object.GetName(), err)
		}
		entries = append(entries, entry)
	}
	r.ledger.Replace(entries)
	r.rebuilt = true
	r.generation++
	return nil
}

// ensureRebuilt rebuilds once before the first round, so a restarted process
// observes the keys that already exist, and reports the rebuild generation the
// caller has to settle.
func (r *Reconciler) ensureRebuilt(ctx context.Context) (uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.rebuilt {
		if err := r.rebuild(ctx); err != nil {
			return 0, err
		}
	}
	return r.generation, nil
}

// needsSettle reports a rebuilt ledger whose entries have not been settled
// against their stored statuses yet.
func (r *Reconciler) needsSettle(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.settled < generation
}

// markSettled records that the entries of the generation carry their current
// status.
func (r *Reconciler) markSettled(generation uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.settled < generation {
		r.settled = generation
	}
}

// Reconcile handles one endpoint, or every endpoint that names one application:
// an application's disappearance reaches the endpoints through this path, so a
// taken-over endpoint shows the missing target before the cascade removes it
// (C-45).
func (r *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	generation, err := r.ensureRebuilt(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	result := reconcile.Result{}
	// A rebuilt ledger describes the stored objects, not their stored statuses:
	// settle every entry once, so a restart converges the conditions of peers
	// that receive no event of their own (C-06).
	if r.needsSettle(generation) {
		settled, _ := r.settle(ctx, r.ledger.Refs(), write.Ref{})
		result = mergeResult(result, settled)
		r.markSettled(generation)
	}
	// Failed entries are replayed inside the budget. Their failure is a peer's
	// problem and never blocks the request this round belongs to (C-06).
	result = mergeResult(result, r.retryPending(ctx))
	// Refused route writes are looked at again on the fixed interval, so a
	// survivor blocked inside a peer's round is not stranded there (C-06).
	result = mergeResult(result, r.retryRefused(ctx))

	var requestResult reconcile.Result
	var requestErr error
	switch request.Kind {
	case "Endpoint":
		requestResult, requestErr = r.reconcileEndpoint(ctx, request.Namespace, request.Name)
	case "Application":
		requestResult, requestErr = r.reconcileApplication(ctx, request.Namespace, request.Name)
	}
	if requestErr != nil {
		return requestResult, requestErr
	}
	return mergeResult(result, mergeResult(requestResult, r.pendingHint())), nil
}

// reconcileEndpoint observes one endpoint and settles its status.
func (r *Reconciler) reconcileEndpoint(ctx context.Context, namespace, name string) (reconcile.Result, error) {
	ref := endpointRef(namespace, name)
	object, err := r.writer.Get(ctx, ref)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The round of a deleted entry owns its route cleanup; a failure
			// here is this request's own and is retried as such. The entry is
			// gone, so nothing may keep it on a schedule afterwards.
			if err := r.deleteRoutes(ctx, ref); err != nil {
				return reconcile.Result{}, err
			}
			r.clearPending(ref)
			r.clearRefused(ref)
			return r.settle(ctx, r.ledger.Remove(ref), write.Ref{})
		}
		return reconcile.Result{}, err
	}
	return r.observe(ctx, object, nil)
}

// reconcileApplication revisits every endpoint whose target is the application,
// so a deleted application leaves a conflict on the endpoints that point at it
// and a stopped one marks them paused (C-45, C-66).
func (r *Reconciler) reconcileApplication(ctx context.Context, namespace, name string) (reconcile.Result, error) {
	key := namespace + "/" + name
	targets := map[string]targetState{}
	object, err := r.writer.Get(ctx, write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application", Namespace: namespace, Name: name})
	switch {
	case apierrors.IsNotFound(err):
		targets[key] = targetState{}
	case err != nil:
		return reconcile.Result{}, err
	default:
		paused, _, _ := unstructured.NestedBool(object.Object, "spec", "paused")
		targets[key] = targetState{present: true, paused: paused}
	}
	objects, err := r.lister.List(ctx, "Endpoint", namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	result := reconcile.Result{}
	var firstErr error
	for _, object := range objects {
		if !TargetsApp(object, name) {
			continue
		}
		settled, err := r.observe(ctx, object, targets)
		result = mergeResult(result, settled)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return result, firstErr
}

// observe records one endpoint in the ledger and settles every endpoint whose
// admission the observation moved. One event therefore converges the observed
// object and its peers, whatever order the events arrive in (C-06). An unmanaged
// endpoint is read-only: KOS neither writes its status nor deletes it (C-02,
// F1), so it is skipped while the managed peers it affects are still settled.
func (r *Reconciler) observe(ctx context.Context, object *unstructured.Unstructured, targets map[string]targetState) (reconcile.Result, error) {
	entry, err := r.entry(ctx, object, targets)
	if err != nil {
		return reconcile.Result{}, err
	}
	return r.settle(ctx, r.ledger.Put(entry), entry.Ref)
}

// settle writes the status of every taken-over endpoint the ledger names. A peer
// that disappeared meanwhile is released here, which in turn settles the holders
// of the keys it freed.
func (r *Reconciler) settle(ctx context.Context, refs []write.Ref, request write.Ref) (reconcile.Result, error) {
	seen := make(map[string]bool, len(refs))
	queue := refs
	result := reconcile.Result{}
	var requestErr error
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		if seen[ref.Key()] {
			continue
		}
		seen[ref.Key()] = true
		entry, ok := r.ledger.Entry(ref)
		if !ok || entry.Attribution == markers.AttributionUnmanaged {
			// Nothing will settle this ref, so neither schedule may keep it:
			// an entry that left the ledger or became read-only has no round
			// of its own (C-06).
			r.clearPending(ref)
			r.clearRefused(ref)
			continue
		}
		object, err := r.writer.Get(ctx, ref)
		if apierrors.IsNotFound(err) {
			// The peer's own deletion round removes its route objects; this
			// path only releases the keys it held.
			r.clearPending(ref)
			queue = append(queue, r.ledger.Remove(ref)...)
			continue
		}
		if err != nil {
			r.keepPending(ref)
			if ref.Key() == request.Key() {
				requestErr = err
			}
			continue
		}
		settled, err := r.settleOne(ctx, object, entry)
		result = mergeResult(result, settled)
		if err != nil {
			r.keepPending(ref)
			if ref.Key() == request.Key() {
				requestErr = err
			}
			continue
		}
		r.clearPending(ref)
	}
	return result, requestErr
}

// targetState is what a round knows about one target application: whether it
// exists and whether it is paused (C-45, C-66).
type targetState struct {
	// present reports whether the application exists.
	present bool
	// paused reports whether the application carries the stop marker.
	paused bool
}

// entry describes one stored endpoint: its shape, its attribution, whether its
// target application exists and whether it is stopped.
func (r *Reconciler) entry(ctx context.Context, object *unstructured.Unstructured, targets map[string]targetState) (Entry, error) {
	entry := EntryOf(object)
	if entry.Invalid != nil {
		return entry, nil
	}
	if failure := Validate(ctx, entry.Ref.Namespace, entry.Spec, r.facts); failure != nil {
		entry.Invalid = failure
		return entry, nil
	}
	target, err := r.targetState(ctx, entry.Ref.Namespace, entry.Spec.TargetApp, targets)
	if err != nil {
		return Entry{}, err
	}
	entry.TargetMissing = !target.present
	entry.TargetPaused = target.paused
	certificate, err := r.certificateState(ctx, entry)
	if err != nil {
		return Entry{}, err
	}
	entry.Certificate = certificate
	return entry, nil
}

// targetState reports whether the application an endpoint names exists and
// whether it is paused. A round that already knows the application set answers
// from it; the objects the set does not name are read once.
func (r *Reconciler) targetState(ctx context.Context, namespace, app string, targets map[string]targetState) (targetState, error) {
	if app == "" {
		return targetState{present: true}, nil
	}
	if found, known := targets[namespace+"/"+app]; known {
		return found, nil
	}
	object, err := r.writer.Get(ctx, write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application", Namespace: namespace, Name: app})
	if apierrors.IsNotFound(err) {
		return targetState{}, nil
	}
	if err != nil {
		return targetState{}, err
	}
	paused, _, _ := unstructured.NestedBool(object.Object, "spec", "paused")
	return targetState{present: true, paused: paused}, nil
}

// settleOne applies the route object and then the status of one endpoint, so a
// failure to write the route is part of the status instead of hiding behind a
// condition that claims the desired state is applied (C-06, C-07, C-43).
func (r *Reconciler) settleOne(ctx context.Context, object *unstructured.Unstructured, entry Entry) (reconcile.Result, error) {
	conflicts := r.ledger.Conflicts(entry.Ref)
	routeErr := r.syncRoute(ctx, object, entry, entry.Invalid == nil && len(conflicts) == 0)
	if err := r.writeStatus(ctx, object, entry, conflicts, routeErr); err != nil {
		return reconcile.Result{}, err
	}
	var denied *write.Denied
	switch {
	case errors.As(routeErr, &denied):
		// The refusal lasts until the blocking object goes away, and nothing
		// observes that object: keep this entry on the fixed re-observation
		// schedule, whichever round hit the refusal (C-06).
		r.markRefused(entry.Ref)
		return reconcile.Result{RequeueAfter: routeReobserve}, nil
	case errors.Is(routeErr, errRouteNamespace):
		// The KOS namespace stays the installer's; this condition is final.
		r.clearRefused(entry.Ref)
		return reconcile.Result{}, nil
	case routeErr != nil:
		r.clearRefused(entry.Ref)
		return reconcile.Result{}, routeErr
	default:
		r.clearRefused(entry.Ref)
		return reconcile.Result{}, nil
	}
}

// writeStatus settles the two status axes of an endpoint: Reconciled follows
// admission and the route write, and the failure condition names what blocks it
// (C-07).
func (r *Reconciler) writeStatus(ctx context.Context, object *unstructured.Unstructured, entry Entry, conflicts []Conflict, routeErr error) error {
	conditions, err := conditionsOf(object)
	if err != nil {
		return err
	}
	generation := object.GetGeneration()
	conditions = without(conditions, ConditionInvalidSpec, ConditionConflict, ConditionRouteBlocked, ConditionApplicationPaused)
	status := v1beta1.EndpointStatus{
		ObjectStatus: v1beta1.ObjectStatus{Conditions: conditions},
	}

	switch {
	case entry.Invalid != nil:
		message := entry.Invalid.Error()
		status.Conditions = state.Set(conditions, state.Failure(ConditionInvalidSpec, generation, ReasonInvalidSpec, message))
		status.Conditions = state.Set(status.Conditions, state.Blocked(generation, ReasonInvalidSpec, message))
		status.Health.State = v1beta1.HealthStateUnavailable
	case len(conflicts) > 0:
		message := conflictMessage(conflicts)
		status.Conditions = state.Set(conditions, state.Failure(ConditionConflict, generation, ReasonConflict, message))
		status.Conditions = state.Set(status.Conditions, state.Blocked(generation, ReasonConflict, message))
		status.Health.State = v1beta1.HealthStateUnavailable
	case routeErr != nil:
		message := routeErr.Error()
		status.Conditions = state.Set(conditions, state.Failure(ConditionRouteBlocked, generation, ReasonRouteBlocked, message))
		status.Conditions = state.Set(status.Conditions, state.Blocked(generation, ReasonRouteBlocked, message))
		status.Health.State = v1beta1.HealthStateUnavailable
	default:
		status.Conditions = state.Set(conditions, state.Applied(generation, "endpoint admitted"))
		status.Health.State = v1beta1.HealthStateHealthy
	}
	if entry.Certificate == nil {
		status.Conditions = without(status.Conditions, ConditionCertificateReady)
	} else {
		status.Certificate = certificateStatus(*entry.Certificate)
		status.Conditions = state.Set(status.Conditions, certificateCondition(*entry.Certificate, generation))
	}
	if entry.TargetPaused {
		// The route stays in place; the condition is what tells a reader the
		// target application is stopped (C-66).
		status.Conditions = state.Set(status.Conditions, state.Condition(
			ConditionApplicationPaused, metav1.ConditionTrue, generation, ReasonApplicationPaused, "应用已暂停"))
	}

	document, err := statusDocument(entry.Ref, status)
	if err != nil {
		return err
	}
	_, err = r.writer.Apply(ctx, write.SubjectEndpointReconciler, write.Session{}, write.PartStatus, document)
	return err
}

// certificateStatus renders the observed certificate as the status field C-46
// makes a first-class item.
func certificateStatus(certificate Certificate) *v1beta1.EndpointCertificate {
	status := &v1beta1.EndpointCertificate{
		Issuer:       certificate.Issuer,
		Ready:        certificate.Ready,
		ExpiringSoon: certificate.ExpiringSoon,
		Reason:       certificate.Reason,
	}
	if certificate.NotAfter != nil {
		status.NotAfter = &metav1.Time{Time: *certificate.NotAfter}
	}
	return status
}

// statusDocument renders the status subresource document of one endpoint.
func statusDocument(ref write.Ref, status v1beta1.EndpointStatus) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return nil, fmt.Errorf("encode endpoint status: %w", err)
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Endpoint",
		"metadata":   map[string]any{"name": ref.Name, "namespace": ref.Namespace},
		"status":     raw,
	}}
	return document, nil
}

// conditionsOf reads the current conditions; an object without a status has
// none.
func conditionsOf(object *unstructured.Unstructured) ([]metav1.Condition, error) {
	raw, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		return nil, fmt.Errorf("read endpoint conditions: %w", err)
	}
	if !found {
		return nil, nil
	}
	conditions := make([]metav1.Condition, 0, len(raw))
	for _, item := range raw {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("endpoint condition is not an object")
		}
		var condition metav1.Condition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, &condition); err != nil {
			return nil, fmt.Errorf("decode endpoint condition: %w", err)
		}
		conditions = append(conditions, condition)
	}
	return conditions, nil
}

// without removes the conditions of the given types, so a resolved failure
// disappears instead of staying True forever.
func without(conditions []metav1.Condition, types ...string) []metav1.Condition {
	kept := make([]metav1.Condition, 0, len(conditions))
	for _, condition := range conditions {
		drop := false
		for _, kind := range types {
			if condition.Type == kind {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, condition)
		}
	}
	return kept
}

// conflictMessage lists every conflict of one endpoint.
func conflictMessage(conflicts []Conflict) string {
	messages := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		messages = append(messages, conflict.Message())
	}
	return strings.Join(messages, "; ")
}

// routeReobserve is how often a permanently refused route is re-observed, so an
// entry recovers when the blocking object disappears (C-06).
const routeReobserve = time.Minute

// Retry budget of a pending entry: five attempts with a delay starting at ten
// seconds and capped at five minutes (C-15).
const (
	pendingAttempts  = 5
	pendingBaseDelay = 10 * time.Second
	pendingMaxDelay  = 5 * time.Minute
)

// pendingEntry is one entry a failed round could not settle.
type pendingEntry struct {
	ref      write.Ref
	attempts int
	nextAt   time.Time
}

// keepPending schedules one failed entry by the retry budget. An entry that
// exhausts it is dropped: its failure is already in the object's status.
func (r *Reconciler) keepPending(ref write.Ref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.pending[ref.Key()]
	if !ok {
		entry = &pendingEntry{ref: ref}
	}
	entry.attempts++
	if entry.attempts >= pendingAttempts {
		delete(r.pending, ref.Key())
		return
	}
	entry.nextAt = r.clock().Add(pendingDelay(entry.attempts))
	r.pending[ref.Key()] = entry
}

// pendingDelay is the wait before the next attempt of an entry.
func pendingDelay(attempt int) time.Duration {
	delay := pendingBaseDelay << (attempt - 1)
	if delay <= 0 || delay > pendingMaxDelay {
		return pendingMaxDelay
	}
	return delay
}

// takeDuePending returns the entries whose next attempt is due. It keeps them
// scheduled: the round that retries one clears it on success and pushes the
// next delay on failure, so the attempt count survives the retry.
func (r *Reconciler) takeDuePending() []write.Ref {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	due := make([]write.Ref, 0, len(r.pending))
	for _, entry := range r.pending {
		if !entry.nextAt.After(now) {
			due = append(due, entry.ref)
		}
	}
	sort.Slice(due, func(left, right int) bool { return due[left].Key() < due[right].Key() })
	return due
}

// clearPending drops one entry from the retry set after a settled round.
func (r *Reconciler) clearPending(ref write.Ref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pending, ref.Key())
}

// pendingHint asks for a round when the next replay or re-observation is
// scheduled.
func (r *Reconciler) pendingHint() reconcile.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	var earliest time.Time
	for _, entry := range r.pending {
		if earliest.IsZero() || entry.nextAt.Before(earliest) {
			earliest = entry.nextAt
		}
	}
	for _, entry := range r.refused {
		if earliest.IsZero() || entry.nextAt.Before(earliest) {
			earliest = entry.nextAt
		}
	}
	if earliest.IsZero() {
		return reconcile.Result{}
	}
	delay := earliest.Sub(now)
	if delay <= 0 {
		delay = time.Second
	}
	return reconcile.Result{RequeueAfter: delay}
}

// retryPending replays the due entries and never fails the current request:
// their failure stays theirs (C-06).
func (r *Reconciler) retryPending(ctx context.Context) reconcile.Result {
	refs := r.takeDuePending()
	if len(refs) == 0 {
		return reconcile.Result{}
	}
	result, _ := r.settle(ctx, refs, write.Ref{})
	return result
}

// refusedEntry is one entry whose route write a policy refused, with the time
// of its next look.
type refusedEntry struct {
	ref    write.Ref
	nextAt time.Time
}

// markRefused schedules one entry refused by a route write on the fixed
// re-observation interval. A refusal is not a failure of the entry, so it has
// no attempt budget: it lasts as long as the blocking object does (C-06, C-07).
func (r *Reconciler) markRefused(ref write.Ref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused[ref.Key()] = &refusedEntry{ref: ref, nextAt: r.clock().Add(routeReobserve)}
}

// clearRefused drops one entry from the re-observation set once a round
// settles it.
func (r *Reconciler) clearRefused(ref write.Ref) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.refused, ref.Key())
}

// takeDueRefused returns the entries whose next look is due. They stay
// scheduled: a round that still finds the write refused pushes the next look
// out again.
func (r *Reconciler) takeDueRefused() []write.Ref {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.clock()
	due := make([]write.Ref, 0, len(r.refused))
	for _, entry := range r.refused {
		if !entry.nextAt.After(now) {
			due = append(due, entry.ref)
		}
	}
	sort.Slice(due, func(left, right int) bool { return due[left].Key() < due[right].Key() })
	return due
}

// retryRefused looks again at the entries a route write refused, so a survivor
// blocked inside a peer's round recovers when the blocking object goes away
// (C-06).
func (r *Reconciler) retryRefused(ctx context.Context) reconcile.Result {
	refs := r.takeDueRefused()
	if len(refs) == 0 {
		return reconcile.Result{}
	}
	result, _ := r.settle(ctx, refs, write.Ref{})
	return result
}

// clock reads the retry clock.
func (r *Reconciler) clock() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// mergeResult keeps the earlier of two requeue requests.
func mergeResult(left, right reconcile.Result) reconcile.Result {
	switch {
	case left.RequeueAfter == 0:
		return right
	case right.RequeueAfter == 0:
		return left
	case right.RequeueAfter < left.RequeueAfter:
		return right
	default:
		return left
	}
}

// endpointRef names an endpoint in the storage version.
func endpointRef(namespace, name string) write.Ref {
	return write.Ref{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Endpoint", Namespace: namespace, Name: name}
}

// clusterLister reads endpoints and applications through the cluster client the
// writer holds.
type clusterLister struct {
	client    dynamic.Interface
	resources map[schema.GroupKind]string
}

// List implements Lister.
func (l clusterLister) List(ctx context.Context, kind, namespace string) ([]*unstructured.Unstructured, error) {
	if l.client == nil {
		return nil, errors.New("endpoint: no cluster client")
	}
	resource := l.resources[schema.GroupKind{Group: api.Group, Kind: kind}]
	if resource == "" {
		return nil, fmt.Errorf("endpoint: the resource name of %s is unknown", kind)
	}
	list, err := l.client.Resource(schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: resource,
	}).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects, nil
}
