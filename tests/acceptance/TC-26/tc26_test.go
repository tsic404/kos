// Package tc26 verifies that a repeated round over one observed state changes
// nothing, and that a round over the time budget is observable (AC-13, TC-26).
package tc26

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	// servedVersion is the storage version the case reads and writes.
	servedVersion = "v1beta1"
	// downstreamSuffix names the object the case's reconciler applies.
	downstreamSuffix = "-downstream"
)

// applyingReconciler renders one downstream ConfigMap and writes the status
// axis. It reports an event only when a round changed something, so a repeated
// round over the same observed state must add neither a field nor an event.
type applyingReconciler struct {
	cluster *harness.Cluster
	// statusWrites counts the rounds that actually wrote status, so the case
	// can prove the repeated round skipped the write.
	statusWrites atomic.Int32
}

func (r *applyingReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	application, err := r.cluster.GetObject(ctx, servedVersion, request.Kind, request.Namespace, request.Name)
	if err != nil {
		return reconcile.Result{}, err
	}

	changed, err := r.applyDownstream(ctx, application)
	if err != nil {
		return reconcile.Result{}, err
	}

	conditions, err := harness.ConditionsOf(application)
	if err != nil {
		return reconcile.Result{}, err
	}
	conditions = state.Set(conditions, state.Applied(application.GetGeneration(), "desired state applied"))
	health := state.EvaluateHealth(state.HealthInput{IsObserved: true, EffectiveDesiredReplicas: 1, ReadyReplicas: 1})
	status, err := harness.StatusValue(v1beta1.ObjectStatus{
		Phase:      "Ready",
		Health:     v1beta1.Health{State: health},
		Conditions: conditions,
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	wrote, err := r.cluster.ApplyStatus(ctx, servedVersion, request.Kind, request.Namespace, request.Name, status)
	if err != nil {
		return reconcile.Result{}, err
	}
	if wrote {
		r.statusWrites.Add(1)
	}

	if changed {
		if err := r.recordEvent(ctx, application, "Applied", "downstream applied"); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *applyingReconciler) applyDownstream(ctx context.Context, application *unstructured.Unstructured) (bool, error) {
	name := application.GetName() + downstreamSuffix
	desired := map[string]string{"application": application.GetName()}
	configMaps := r.cluster.Kube.CoreV1().ConfigMaps(application.GetNamespace())

	current, err := configMaps.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: application.GetNamespace(),
				Labels:    map[string]string{"app.kubernetes.io/name": application.GetName()},
			},
			Data: desired,
		}, metav1.CreateOptions{})
		return err == nil, err
	}
	if err != nil {
		return false, err
	}
	if reflect.DeepEqual(current.Data, desired) {
		return false, nil
	}
	current.Data = desired
	_, err = configMaps.Update(ctx, current, metav1.UpdateOptions{})
	return err == nil, err
}

func (r *applyingReconciler) recordEvent(ctx context.Context, application *unstructured.Unstructured, reason, message string) error {
	now := metav1.Now()
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s.%d", application.GetName(), now.UnixNano()),
			Namespace: application.GetNamespace(),
		},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: application.GetAPIVersion(),
			Kind:       application.GetKind(),
			Namespace:  application.GetNamespace(),
			Name:       application.GetName(),
			UID:        application.GetUID(),
		},
		Reason:              reason,
		Message:             message,
		Type:                corev1.EventTypeNormal,
		Action:              "Apply",
		ReportingController: "kos.tsic.top/acceptance",
		ReportingInstance:   "acceptance",
		FirstTimestamp:      now,
		LastTimestamp:       now,
		Count:               1,
		EventTime:           metav1.NewMicroTime(now.Time),
	}
	_, err := r.cluster.Kube.CoreV1().Events(application.GetNamespace()).Create(ctx, event, metav1.CreateOptions{})
	return err
}

// observation is everything one round could change: the object, its downstream
// and the events of the namespace.
type observation struct {
	application map[string]string
	downstream  map[string]string
	events      []string
}

func TestTC26RepeatedRoundChangesNothing(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))

	reconciler := &applyingReconciler{cluster: cluster}
	request := reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: "web"}
	ctx := context.Background()

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("first round: %v", err)
	}
	if writes := reconciler.statusWrites.Load(); writes != 1 {
		t.Fatalf("the first round wrote status %d times, want 1", writes)
	}
	before := observe(t, cluster, "web")

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("second round over the same observed state: %v", err)
	}
	if writes := reconciler.statusWrites.Load(); writes != 1 {
		t.Fatalf("the repeated round rewrote status (%d writes)", writes)
	}
	after := observe(t, cluster, "web")

	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the repeated round changed the result:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func observe(t *testing.T, cluster *harness.Cluster, name string) observation {
	t.Helper()
	application := cluster.Get(t, servedVersion, "Application", name)

	configMap, err := cluster.Kube.CoreV1().ConfigMaps(harness.Namespace).Get(context.Background(), name+downstreamSuffix, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get downstream of %s: %v", name, err)
	}
	downstream, err := runtime.DefaultUnstructuredConverter.ToUnstructured(configMap)
	if err != nil {
		t.Fatalf("convert downstream of %s: %v", name, err)
	}

	events, err := cluster.Kube.CoreV1().Events(harness.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}

	return observation{
		application: harness.Flatten(application.Object),
		downstream:  harness.Flatten(downstream),
		events:      eventSummaries(events.Items),
	}
}

func eventSummaries(events []corev1.Event) []string {
	summaries := make([]string, 0, len(events))
	for _, event := range events {
		summaries = append(summaries, fmt.Sprintf("%s/%s/%s", event.InvolvedObject.Name, event.Reason, event.Type))
	}
	sort.Strings(summaries)
	return summaries
}

// slowReconciler overruns the budget once, then settles: the retry must not add
// another over-limit round.
type slowReconciler struct {
	rounds atomic.Int32
}

func (r *slowReconciler) Reconcile(ctx context.Context, _ reconcile.Request) (reconcile.Result, error) {
	if r.rounds.Add(1) == 1 {
		<-ctx.Done()
		return reconcile.Result{}, ctx.Err()
	}
	return reconcile.Result{}, nil
}

func TestTC26OverLimitRoundIsObservable(t *testing.T) {
	registry := prometheus.NewRegistry()
	controller, err := reconcile.NewController(&slowReconciler{}, reconcile.Options{
		Workers:          1,
		MaxRoundDuration: 50 * time.Millisecond,
		Registerer:       registry,
	})
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

	controller.Enqueue(reconcile.Request{Kind: "Application", Namespace: harness.Namespace, Name: "slow"})
	waitForMetric(t, registry, "kos_reconcile_round_over_limit_total", 1)
	if observations := metricValue(t, registry, "kos_reconcile_round_duration_seconds"); observations < 1 {
		t.Fatalf("round duration observations = %v, want at least 1", observations)
	}
}

// metricValue returns a counter value or a histogram's sample count.
func metricValue(t *testing.T, gatherer prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if counter := metric.GetCounter(); counter != nil {
				return counter.GetValue()
			}
			if histogram := metric.GetHistogram(); histogram != nil {
				return float64(histogram.GetSampleCount())
			}
		}
	}
	t.Fatalf("metric %q is not gathered", name)
	return 0
}

func waitForMetric(t *testing.T, gatherer prometheus.Gatherer, name string, want float64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if metricValue(t, gatherer, name) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("metric %q never reached %v", name, want)
}
