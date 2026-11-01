// Package tc87 verifies the control-plane restart behaviour of AC-43 and C-56:
// a restart neither duplicates objects nor replays a consumed action, and the
// fresh process rebuilds its read path from current facts instead of a cache it
// did not write.
package tc87

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	application    = "shop"
	definitionName = "shop"
	volumeName     = "late"
)

const template = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: app
          image: busybox
`

func TestTC87ControlPlaneRestartDoesNotReplay(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, definitionName, template, nil))
	gateway := cluster.StartControlPlane(t, nil)

	manifest := harness.ApplicationManifest(application, harness.Namespace, definitionName, harness.Namespace, nil, nil)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create the application: %d %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, application, 10); phase != app.PhaseReady {
		t.Fatalf("phase = %s, want Ready", phase)
	}
	cluster.SetWorkloadStatus(t, "Deployment", harness.Namespace, application, 1, 1)
	cluster.ApplicationRound(t, controller, application)

	// A restart action runs once and its record is consumed.
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": application, "action": app.ActionRestart,
	}, nil); status != http.StatusAccepted {
		t.Fatalf("restart action: %d %v", status, body)
	}
	cluster.ApplicationRound(t, controller, application)
	rebuilt := cluster.Workload(t, "Deployment", harness.Namespace, application)
	recordName := app.ActionRecordName(string(cluster.Get(t, "v1beta1", "Application", application).GetUID()), app.ActionRestart)
	if recordExists(t, cluster, recordName) {
		t.Fatalf("the consumed restart record %s is still stored", recordName)
	}
	objectsBefore := countObjects(t, cluster)

	// The control plane restarts: a new reconciler and a new API process read
	// current facts. The consumed action is not replayed, so the workload is
	// not rebuilt a second time and no object is duplicated.
	restarted := cluster.ApplicationController(t, app.Options{})
	cluster.ApplicationRound(t, restarted, application)
	if got := cluster.Workload(t, "Deployment", harness.Namespace, application); got.GetUID() != rebuilt.GetUID() {
		t.Fatalf("the restart action was replayed: uid %s became %s", rebuilt.GetUID(), got.GetUID())
	}
	if recordExists(t, cluster, recordName) {
		t.Fatal("the new process recreated the consumed action record")
	}
	if after := countObjects(t, cluster); after != objectsBefore {
		t.Fatalf("objects = %d after the restart, want the %d present before it", after, objectsBefore)
	}

	// An object written while the API process was down is what the new
	// process reads: its cache is built from current facts, not carried over.
	cluster.Create(t, harness.Volume(harness.Namespace, volumeName, map[string]any{
		"mobility": "cluster-movable",
		"capacity": "1Gi",
	}))
	fresh := cluster.StartControlPlane(t, nil)
	status, body := fresh.Call(t, http.MethodGet, "/api/v1/resources/Volume/"+volumeName+"?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the volume written while down: %d %v", status, body)
	}
	if stale, _ := body["stale"].(bool); stale {
		t.Fatal("the fresh process served the volume with a stale marker")
	}
	observedAt := harness.String(body, "observedAt")
	parsed, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil {
		t.Fatalf("observedAt %q is not a time: %v", observedAt, err)
	}
	if age := time.Since(parsed); age > controlplane.StaleThreshold {
		t.Fatalf("the fresh process observed the volume %s ago", age)
	}
}

// recordExists reports whether a one-shot action record is stored.
func recordExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the action record %s: %v", name, err)
	}
	return true
}

// countObjects counts the objects one application's rounds may create: the
// workload, the action records and the kernel application itself.
func countObjects(t *testing.T, cluster *harness.Cluster) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deployments, err := cluster.Kube.AppsV1().Deployments(harness.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Deployments: %v", err)
	}
	records, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list action records: %v", err)
	}
	applications, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list applications: %v", err)
	}
	return len(deployments.Items) + len(records.Items) + len(applications.Items)
}
