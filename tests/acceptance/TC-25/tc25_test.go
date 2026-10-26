// Package tc25 verifies the three values of the Reconciled condition: applied,
// blocked with a reason, and unobserved (AC-13, TC-25).
package tc25

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// servedVersion is the storage version the case reads and writes.
	servedVersion = "v1beta1"
	// dependencyNamespaceAnnotation declares the namespace a test application
	// needs before its workload can be applied; it is case scaffolding, not a
	// kernel field.
	dependencyNamespaceAnnotation = "acceptance.kos.tsic.top/dependency-namespace"
	// missingNamespace is named by the blocked application and does not exist.
	missingNamespace = "acceptance-does-not-exist"
	// desiredReplicasAnnotation and readyReplicasAnnotation shape the health
	// axis without touching phase, so the case can show the two axes moving
	// independently.
	desiredReplicasAnnotation = "acceptance.kos.tsic.top/desired-replicas"
	readyReplicasAnnotation   = "acceptance.kos.tsic.top/ready-replicas"
)

// applicationReconciler writes the two status axes of one application: a
// normal application is applied, one whose declared namespace is missing stays
// blocked with its own failure condition.
type applicationReconciler struct {
	cluster *harness.Cluster
	rounds  chan<- reconcile.Request
}

func (r *applicationReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	application, err := r.cluster.GetObject(ctx, servedVersion, request.Kind, request.Namespace, request.Name)
	if err != nil {
		return reconcile.Result{}, err
	}
	status, err := r.status(ctx, application)
	if err != nil {
		return reconcile.Result{}, err
	}
	if _, err := r.cluster.ApplyStatus(ctx, servedVersion, request.Kind, request.Namespace, request.Name, status); err != nil {
		return reconcile.Result{}, err
	}
	r.rounds <- request
	return reconcile.Result{}, nil
}

func (r *applicationReconciler) status(ctx context.Context, application *unstructured.Unstructured) (map[string]any, error) {
	conditions, err := harness.ConditionsOf(application)
	if err != nil {
		return nil, err
	}
	generation := application.GetGeneration()
	desiredReplicas := replicaCount(application, desiredReplicasAnnotation, 1)
	readyReplicas := replicaCount(application, readyReplicasAnnotation, desiredReplicas)

	dependencyNamespace := application.GetAnnotations()[dependencyNamespaceAnnotation]
	if dependencyNamespace != "" {
		exists, err := r.namespaceExists(ctx, dependencyNamespace)
		if err != nil {
			return nil, err
		}
		if !exists {
			message := fmt.Sprintf("namespace %q does not exist", dependencyNamespace)
			conditions = state.Set(conditions, state.Blocked(generation, "NamespaceMissing", message))
			conditions = state.Set(conditions, state.Failure("NamespaceMissing", generation, "NamespaceMissing", message))
			health := state.EvaluateHealth(state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: desiredReplicas})
			return harness.StatusValue(v1beta1.ObjectStatus{
				Phase:      "Pending",
				Health:     v1beta1.Health{State: health},
				Conditions: conditions,
			})
		}
	}

	conditions = state.Set(conditions, state.Applied(generation, "desired state applied"))
	health := state.EvaluateHealth(state.HealthInput{
		IsObserved:               true,
		EffectiveDesiredReplicas: desiredReplicas,
		ReadyReplicas:            readyReplicas,
	})
	return harness.StatusValue(v1beta1.ObjectStatus{
		Phase:      "Ready",
		Health:     v1beta1.Health{State: health},
		Conditions: conditions,
	})
}

// replicaCount reads a case annotation; a missing or malformed value falls
// back so the reconciler never wedges on its own scaffolding.
func replicaCount(application *unstructured.Unstructured, key string, fallback int32) int32 {
	value, ok := application.GetAnnotations()[key]
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return fallback
	}
	return int32(parsed)
}

func (r *applicationReconciler) namespaceExists(ctx context.Context, name string) (bool, error) {
	_, err := r.cluster.Kube.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func TestTC25ReconciledThreeValues(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("progressing", nil))
	cluster.Create(t, harness.Application("blocked", map[string]string{
		dependencyNamespaceAnnotation: missingNamespace,
	}))
	// "degraded" keeps phase Ready like "progressing" but reports partial
	// readiness, so the two axes can be seen moving independently.
	cluster.Create(t, harness.Application("degraded", map[string]string{
		desiredReplicasAnnotation: "2",
		readyReplicasAnnotation:   "1",
	}))

	rounds := make(chan reconcile.Request, 8)
	controller, err := reconcile.NewController(
		&applicationReconciler{cluster: cluster, rounds: rounds},
		reconcile.Options{Workers: 1, MaxRoundDuration: 10 * time.Second},
	)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		controller.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: "progressing"})
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: "blocked"})
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: "degraded"})
	waitForRound(t, rounds)
	waitForRound(t, rounds)
	waitForRound(t, rounds)

	var progressing []metav1.Condition
	waitFor(t, "the progressing application to be reconciled", func() bool {
		progressing = harness.Conditions(t, cluster.Get(t, servedVersion, "Application", "progressing"))
		return state.ReconciledOf(progressing).Status == metav1.ConditionTrue
	})
	applied := state.ReconciledOf(progressing)
	if applied.Reason != state.ReasonApplied {
		t.Errorf("progressing reason = %q, want %q", applied.Reason, state.ReasonApplied)
	}

	var blocked []metav1.Condition
	waitFor(t, "the blocked application to be reconciled", func() bool {
		blocked = harness.Conditions(t, cluster.Get(t, servedVersion, "Application", "blocked"))
		return state.ReconciledOf(blocked).Status == metav1.ConditionFalse
	})
	reconciled := state.ReconciledOf(blocked)
	if reconciled.Reason != "NamespaceMissing" {
		t.Errorf("blocked reason = %q, want NamespaceMissing", reconciled.Reason)
	}
	failure, ok := state.Find(blocked, "NamespaceMissing")
	if !ok {
		t.Fatal("the blocked application carries no failure condition")
	}
	if failure.Status != metav1.ConditionTrue {
		t.Errorf("failure condition status = %q, want True", failure.Status)
	}

	// The two axes are written independently: the blocked application is
	// Pending and unavailable while the applied one is Ready and healthy.
	blockedApplication := cluster.Get(t, servedVersion, "Application", "blocked")
	if phase := nestedString(t, blockedApplication.Object, "status", "phase"); phase != "Pending" {
		t.Errorf("blocked phase = %q, want Pending", phase)
	}
	if health := nestedString(t, blockedApplication.Object, "status", "health", "state"); health != string(v1beta1.HealthStateUnavailable) {
		t.Errorf("blocked health = %q, want %q", health, v1beta1.HealthStateUnavailable)
	}
	progressingApplication := cluster.Get(t, servedVersion, "Application", "progressing")
	if phase := nestedString(t, progressingApplication.Object, "status", "phase"); phase != "Ready" {
		t.Errorf("progressing phase = %q, want Ready", phase)
	}
	if health := nestedString(t, progressingApplication.Object, "status", "health", "state"); health != string(v1beta1.HealthStateHealthy) {
		t.Errorf("progressing health = %q, want %q", health, v1beta1.HealthStateHealthy)
	}

	// The degraded application keeps the same phase as the applied one but
	// reports different health, so one axis moves without the other.
	degradedApplication := cluster.Get(t, servedVersion, "Application", "degraded")
	if phase := nestedString(t, degradedApplication.Object, "status", "phase"); phase != "Ready" {
		t.Errorf("degraded phase = %q, want Ready", phase)
	}
	if health := nestedString(t, degradedApplication.Object, "status", "health", "state"); health != string(v1beta1.HealthStateDegraded) {
		t.Errorf("degraded health = %q, want %q", health, v1beta1.HealthStateDegraded)
	}

	// Stopping the controller leaves the next application without an observer,
	// so its Reconciled value reads as Unknown rather than a stale True.
	cancel()
	<-done
	cluster.Create(t, harness.Application("unobserved", nil))
	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: "unobserved"})

	conditions := harness.Conditions(t, cluster.Get(t, servedVersion, "Application", "unobserved"))
	if len(conditions) != 0 {
		t.Fatalf("a stopped controller wrote %d conditions", len(conditions))
	}
	if value := state.ReconciledOf(conditions).Status; value != metav1.ConditionUnknown {
		t.Fatalf("unobserved Reconciled = %q, want Unknown", value)
	}
}

func waitForRound(t *testing.T, rounds <-chan reconcile.Request) {
	t.Helper()
	select {
	case <-rounds:
	case <-time.After(20 * time.Second):
		t.Fatal("a round did not finish")
	}
}

func waitFor(t *testing.T, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func nestedString(t *testing.T, object map[string]any, fields ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(object, fields...)
	if err != nil || !found {
		t.Fatalf("read %v: found=%v err=%v", fields, found, err)
	}
	return value
}
