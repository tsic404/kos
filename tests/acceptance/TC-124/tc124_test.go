// Package tc124 verifies C-66's restart, scale and concurrent-action rules: a
// restart rebuilds the workload once without touching the user spec or the
// volume, a scale converges the workload, concurrent stops are serialized or
// refused with a conflict, and the four operations stay distinguishable in the
// audit trail (AC-61, TC-124).
package tc124

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace  = "apps"
	applicationName  = "web"
	operatorName     = "operator"
	operatorPassword = "operator-password"
)

// statefulTemplate renders a StatefulSet pinned to one node, so a rebuild keeps
// the placement constraint a node-local volume would impose, and a Service.
const statefulTemplate = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  serviceName: {{ .Name }}
  replicas: 2
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      nodeSelector:
        kubernetes.io/hostname: node-a
      containers:
        - name: app
          image: nginx:1.27
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  selector:
    app: {{ .Name }}
  ports:
    - port: 80
      targetPort: 80
`

func TestTC124RestartScaleAndConcurrentStops(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "lifecycle", statefulTemplate, nil))

	audit := &harness.AuditRecorder{}
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Auditor = audit
	})
	manifest := harness.ApplicationManifest(applicationName, harness.Namespace, "lifecycle", targetNamespace, nil, []string{"data"})
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create the application: status %d, body %v", status, body)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	reconcileOnce(t, controller)
	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseReady)
	}

	before := cluster.Get(t, "v1beta1", "Application", applicationName)
	specBefore := specOf(t, before)
	uidBefore := cluster.Workload(t, "StatefulSet", targetNamespace, applicationName).GetUID()

	action(t, gateway, map[string]any{"action": "restart"})
	reconcileOnce(t, controller)
	rebuilt := cluster.Workload(t, "StatefulSet", targetNamespace, applicationName)
	if rebuilt.GetUID() == uidBefore {
		t.Fatal("the restart did not rebuild the workload")
	}
	if got := cluster.WorkloadReplicas(t, "StatefulSet", targetNamespace, applicationName); got != 2 {
		t.Fatalf("rebuilt replicas = %d, want 2", got)
	}
	nodeSelector, _, _ := unstructured.NestedStringMap(rebuilt.Object, "spec", "template", "spec", "nodeSelector")
	if nodeSelector["kubernetes.io/hostname"] != "node-a" {
		t.Fatalf("the rebuild lost the node placement constraint: %v", nodeSelector)
	}
	if specAfter := specOf(t, cluster.Get(t, "v1beta1", "Application", applicationName)); !reflect.DeepEqual(specBefore, specAfter) {
		t.Fatalf("the restart changed the user spec: %v -> %v", specBefore, specAfter)
	}
	if recordName := app.ActionRecordName(string(before.GetUID()), app.ActionRestart); recordExists(t, cluster, recordName) {
		t.Fatalf("the restart record %s was not consumed", recordName)
	}
	// Later rounds must not rebuild the workload again.
	reconcileOnce(t, controller)
	if uid := cluster.Workload(t, "StatefulSet", targetNamespace, applicationName).GetUID(); uid != rebuilt.GetUID() {
		t.Fatal("a later round rebuilt the workload a second time")
	}

	action(t, gateway, map[string]any{"action": "scale", "replicas": 3})
	reconcileOnce(t, controller)
	if got := cluster.WorkloadReplicas(t, "StatefulSet", targetNamespace, applicationName); got != 3 {
		t.Fatalf("scaled replicas = %d, want 3", got)
	}

	// Two concurrent stops: each is accepted or refused with a conflict, and
	// neither loses the scale (C-05).
	statuses := concurrentActions(t, gateway, map[string]any{"action": "stop"})
	for _, status := range statuses {
		if status != http.StatusAccepted && status != http.StatusConflict {
			t.Fatalf("concurrent stop answered %d, want 202 or 409", status)
		}
	}
	stopped := cluster.Get(t, "v1beta1", "Application", applicationName)
	if paused, _, _ := unstructured.NestedBool(stopped.Object, "spec", "paused"); !paused {
		t.Fatal("the concurrent stops left no pause marker")
	}
	if replicas, _, _ := unstructured.NestedInt64(stopped.Object, "spec", "replicas"); replicas != 3 {
		t.Fatalf("the concurrent stops changed the replica count to %d", replicas)
	}
	reconcileOnce(t, controller)
	if got := cluster.WorkloadReplicas(t, "StatefulSet", targetNamespace, applicationName); got != 0 {
		t.Fatalf("stopped replicas = %d, want 0", got)
	}
	action(t, gateway, map[string]any{"action": "start"})

	verbs := audit.Verbs()
	for _, want := range []string{app.ActionRestart, app.ActionScale, app.ActionStop, app.ActionStart} {
		if !contains(verbs, want) {
			t.Fatalf("the audit trail %v does not distinguish %q", verbs, want)
		}
	}
	// Without an actor hook the write stays attributed to the process (C-14).
	for _, actor := range audit.Actors() {
		if actor != string(write.SubjectAPIProcess) {
			t.Fatalf("audit actor = %q, want the process subject", actor)
		}
	}
}

// TestTC124RestartRebuildsUnderEveryDriftPolicy covers C-66's rebuild against the
// drift policies of C-35: a restart deletes its own workloads, so their absence
// is not an external write and the adopt and ignore policies must not leave them
// deleted. The rebuilt workload keeps the package content and the user spec stays
// byte-identical.
func TestTC124RestartRebuildsUnderEveryDriftPolicy(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "lifecycle", statefulTemplate, nil))

	gateway := cluster.StartControlPlane(t, nil)
	controller := cluster.ApplicationController(t, app.Options{})

	policies := map[string]v1beta1.DriftPolicy{
		"restart-adopted": v1beta1.DriftPolicyAdopt,
		"restart-ignored": v1beta1.DriftPolicyIgnore,
	}
	for name, policy := range policies {
		manifest := harness.ApplicationManifest(name, harness.Namespace, "lifecycle", targetNamespace, nil, nil)
		manifest["spec"].(map[string]any)["driftPolicy"] = string(policy)
		if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
			t.Fatalf("%s: create the application: status %d, body %v", name, status, body)
		}
		reconcileNamed(t, controller, name)
		reconcileNamed(t, controller, name)
	}

	for name := range policies {
		before := cluster.Get(t, "v1beta1", "Application", name)
		specBefore := specOf(t, before)
		uidBefore := cluster.Workload(t, "StatefulSet", targetNamespace, name).GetUID()

		actionOn(t, gateway, name, map[string]any{"action": app.ActionRestart})
		reconcileNamed(t, controller, name)

		if !cluster.WorkloadExists("StatefulSet", targetNamespace, name) {
			t.Fatalf("%s: the restart left the workload deleted", name)
		}
		rebuilt := cluster.Workload(t, "StatefulSet", targetNamespace, name)
		if rebuilt.GetUID() == uidBefore {
			t.Fatalf("%s: the restart did not rebuild the workload", name)
		}
		if got := cluster.WorkloadReplicas(t, "StatefulSet", targetNamespace, name); got != 2 {
			t.Fatalf("%s: rebuilt replicas = %d, want 2", name, got)
		}
		nodeSelector, _, _ := unstructured.NestedStringMap(rebuilt.Object, "spec", "template", "spec", "nodeSelector")
		if nodeSelector["kubernetes.io/hostname"] != "node-a" {
			t.Fatalf("%s: the rebuild lost the node placement constraint: %v", name, nodeSelector)
		}
		if specAfter := specOf(t, cluster.Get(t, "v1beta1", "Application", name)); !reflect.DeepEqual(specBefore, specAfter) {
			t.Fatalf("%s: the restart changed the user spec: %v -> %v", name, specBefore, specAfter)
		}
		// The delete is KOS's own write, so no policy reports it as an external
		// change.
		if entries := driftOfApp(t, cluster, name); len(entries) != 0 {
			t.Fatalf("%s: status.drift = %v, want no external write", name, entries)
		}
		if driftedCondition(t, cluster, name) {
			t.Fatalf("%s: the restart reported a drift condition", name)
		}
		// Later rounds must not rebuild the workload a second time.
		reconcileNamed(t, controller, name)
		if uid := cluster.Workload(t, "StatefulSet", targetNamespace, name).GetUID(); uid != rebuilt.GetUID() {
			t.Fatalf("%s: a later round rebuilt the workload a second time", name)
		}
	}
}

// actionOn posts one action of the named application and requires acceptance.
func actionOn(t *testing.T, gateway *harness.ControlPlane, name string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": name}
	for key, value := range extra {
		body[key] = value
	}
	if status, response := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", body, nil); status != http.StatusAccepted {
		t.Fatalf("%s: action %v: status %d, body %v", name, extra, status, response)
	}
}

// reconcileNamed drives one round of the named application.
func reconcileNamed(t *testing.T, controller *app.Controller, name string) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: name,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
}

// driftOfApp reads the drift record of the named application.
func driftOfApp(t *testing.T, cluster *harness.Cluster, name string) []any {
	t.Helper()
	entries, _, err := unstructured.NestedSlice(cluster.Get(t, "v1beta1", "Application", name).Object, "status", "drift")
	if err != nil {
		t.Fatalf("%s: read status.drift: %v", name, err)
	}
	return entries
}

// driftedCondition reports whether the named application carries the drifted
// failure condition.
func driftedCondition(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	_, present := state.Find(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", name)), app.ConditionDrifted)
	return present
}

// TestTC124AuditNamesTheOperator checks that a lifecycle action submitted
// through a signed-in session is attributed to that account and lands in the
// account database, not under the API process subject (C-14, C-66).
func TestTC124AuditNamesTheOperator(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "lifecycle", statefulTemplate, nil))

	ctx := context.Background()
	store, err := identity.Open(ctx, identity.OpenOptions{Path: filepath.Join(t.TempDir(), "kos.db"), Now: time.Now})
	if err != nil {
		t.Fatalf("open the account database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := identity.New(store, identity.Config{Now: time.Now})
	gateway, err := controlplane.New(controlplane.Config{
		Dynamic:    cluster.Dynamic,
		Authorizer: identity.Authorizer{},
		Actor:      identity.Operator,
		Auditor:    identity.AuditTrail{Store: store},
	})
	if err != nil {
		t.Fatalf("build the control plane: %v", err)
	}
	t.Cleanup(gateway.Close)
	server := httptest.NewServer(identity.NewAPI(service).Handler(gateway.Handler()))
	t.Cleanup(server.Close)
	surface := &harness.HTTP{URL: server.URL}

	bootstrap, err := service.BootstrapOwner(ctx, operatorName)
	if err != nil {
		t.Fatalf("bootstrap the owner: %v", err)
	}
	status, body := surface.Call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": operatorName, "password": bootstrap.OneTimePassword,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("login: status %d, body %v", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("login returned no token: %v", body)
	}
	status, body = surface.Call(t, http.MethodPut, "/api/v1/auth/password", map[string]any{
		"currentPassword": bootstrap.OneTimePassword, "newPassword": operatorPassword,
	}, map[string]string{"Authorization": "Bearer " + token})
	if status != http.StatusNoContent {
		t.Fatalf("change the one-time password: status %d, body %v", status, body)
	}
	headers := map[string]string{"Authorization": "Bearer " + token}

	manifest := harness.ApplicationManifest(applicationName, harness.Namespace, "lifecycle", targetNamespace, nil, nil)
	if status, body := surface.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, headers); status != http.StatusAccepted {
		t.Fatalf("create the application: status %d, body %v", status, body)
	}
	stop := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": applicationName, "action": app.ActionStop}
	if status, body := surface.Call(t, http.MethodPost, "/api/v1/action-requests", stop, headers); status != http.StatusAccepted {
		t.Fatalf("stop the application: status %d, body %v", status, body)
	}

	entries, err := store.AuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("audit entries = %d, want the one stop: %+v", len(entries), entries)
	}
	if entries[0].Actor != operatorName {
		t.Fatalf("audit actor = %q, want the signed-in account %q", entries[0].Actor, operatorName)
	}
	if entries[0].Verb != app.ActionStop {
		t.Fatalf("audit verb = %q, want %q", entries[0].Verb, app.ActionStop)
	}
}

// concurrentActions sends the same action twice at once and returns the two
// statuses.
func concurrentActions(t *testing.T, gateway *harness.ControlPlane, extra map[string]any) []int {
	t.Helper()
	body := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": applicationName}
	for key, value := range extra {
		body[key] = value
	}
	statuses := make([]int, 2)
	var wait sync.WaitGroup
	for index := range statuses {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			status, _, err := gateway.Raw(http.MethodPost, "/api/v1/action-requests", body, nil)
			if err != nil {
				statuses[index] = -1
				return
			}
			statuses[index] = status
		}(index)
	}
	wait.Wait()
	return statuses
}

// action posts one application action and requires acceptance.
func action(t *testing.T, gateway *harness.ControlPlane, extra map[string]any) {
	t.Helper()
	body := map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": applicationName}
	for key, value := range extra {
		body[key] = value
	}
	if status, response := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", body, nil); status != http.StatusAccepted {
		t.Fatalf("action %v: status %d, body %v", extra, status, response)
	}
}

// recordExists reports whether a one-shot action record is still stored.
func recordExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx := context.Background()
	_, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the action record %s: %v", name, err)
	}
	return true
}

// specOf copies the user spec for field-by-field comparison.
func specOf(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, _, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		t.Fatalf("read the spec: %v", err)
	}
	return spec
}

// reconcileOnce drives one application round.
func reconcileOnce(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile %s: %v", applicationName, err)
	}
}

// phaseOf reads the stored phase.
func phaseOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	return phase
}

// contains reports whether values holds want.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
