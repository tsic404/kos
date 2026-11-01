// Package tc84 verifies the node-loss timeline of AC-42 and C-56: a node-local
// application is Unknown inside the confirmation window, its volume is Lost and
// the application Unavailable after it, and both return to Bound and Healthy
// when the node comes back. The workload and the claim are never recreated.
package tc84

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/nodehealth"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	nodeName         = "node-a"
	definitionName   = "shop"
	application      = "shop"
	volumeName       = "data"
	persistentVolume = "data-pv"
)

// template renders one Deployment, so application health has observed replicas
// to read (C-08).
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

func TestTC84NodeLossAndRecovery(t *testing.T) {
	cluster := harness.Start(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	createNode(t, cluster, nodeName)
	cluster.PersistentVolumeOnNode(t, persistentVolume, "", "local-path", "Delete", nodeName)
	cluster.InstallDefinition(t, harness.DefinitionContent(t, definitionName, "", template, nil, nil))
	cluster.Create(t, harness.Volume(harness.Namespace, volumeName, map[string]any{
		"mobility": string(v1beta1.VolumeMobilityNodeLocal),
		"capacity": "1Gi",
	}))
	body := harness.ApplicationManifest(application, harness.Namespace, definitionName, harness.Namespace, nil, []string{volumeName})
	cluster.Create(t, &unstructured.Unstructured{Object: body})

	volumes := cluster.VolumeController(t, volume.Options{Clock: clock})
	harness.ReconcileVolume(t, volumes, harness.Namespace, volumeName)
	cluster.BindClaim(t, harness.Namespace, volumeName, persistentVolume)
	harness.ReconcileVolume(t, volumes, harness.Namespace, volumeName)

	controller := cluster.ApplicationController(t, app.Options{Clock: clock})
	phase := cluster.SettleApplication(t, controller, application, 10)
	if phase != app.PhaseReady {
		t.Fatalf("phase = %s, want Ready", phase)
	}
	cluster.SetWorkloadStatus(t, "Deployment", harness.Namespace, application, 1, 1)
	cluster.ApplicationRound(t, controller, application)
	assertHealth(t, cluster, v1beta1.HealthStateHealthy)
	assertVolume(t, cluster, volume.PhaseBound)

	deployment := deploymentOf(t, cluster)
	claim := cluster.Claim(t, harness.Namespace, volumeName)

	// The node stops answering. Inside the confirmation window the binding
	// cannot be observed, so the application is Unknown while the volume keeps
	// the last state it observed.
	now = now.Add(time.Minute)
	setNodeReady(t, cluster, nodeName, false, now)
	cluster.ApplicationRound(t, controller, application)
	assertHealth(t, cluster, v1beta1.HealthStateUnknown)
	if phase := cluster.ApplicationPhase(t, application); phase != app.PhaseReady {
		t.Fatalf("phase inside the window = %s, want Ready", phase)
	}
	assertVolume(t, cluster, volume.PhaseBound)

	// Beyond the heartbeat grace plus five minutes the node is confirmed lost:
	// the volume is Lost and the application Unavailable.
	now = now.Add(nodehealth.DefaultGrace + nodehealth.ConfirmationWindow + time.Minute)
	harness.ReconcileVolume(t, volumes, harness.Namespace, volumeName)
	cluster.ApplicationRound(t, controller, application)
	assertVolume(t, cluster, volume.PhaseLost)
	assertHealth(t, cluster, v1beta1.HealthStateUnavailable)

	// The node returns: the same claim binds again and health follows it.
	now = now.Add(time.Minute)
	setNodeReady(t, cluster, nodeName, true, now)
	harness.ReconcileVolume(t, volumes, harness.Namespace, volumeName)
	cluster.ApplicationRound(t, controller, application)
	assertVolume(t, cluster, volume.PhaseBound)
	assertHealth(t, cluster, v1beta1.HealthStateHealthy)

	// Nothing was recreated or evicted: one workload and one claim, the ones
	// present before the loss (C-56).
	if got := deploymentOf(t, cluster); got.UID != deployment.UID {
		t.Fatalf("the Deployment was recreated: uid %s became %s", deployment.UID, got.UID)
	}
	if got := cluster.Claim(t, harness.Namespace, volumeName); got.GetUID() != claim.GetUID() {
		t.Fatalf("the claim was recreated: uid %s became %s", claim.GetUID(), got.GetUID())
	}
	if count := workloadCount(t, cluster); count != 1 {
		t.Fatalf("workloads = %d after recovery, want one", count)
	}
}

// createNode writes a Ready node, which the confirmation window reads.
func createNode(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cluster.Kube.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	setNodeReady(t, cluster, name, true, time.Now())
}

// setNodeReady writes the Ready condition the kubelet would write.
func setNodeReady(t *testing.T, cluster *harness.Cluster, name string, isReady bool, at time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := cluster.Kube.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %s: %v", name, err)
	}
	status := corev1.ConditionFalse
	if isReady {
		status = corev1.ConditionTrue
	}
	node.Status.Conditions = []corev1.NodeCondition{{
		Type:               corev1.NodeReady,
		Status:             status,
		Reason:             "Acceptance",
		LastHeartbeatTime:  metav1.NewTime(at),
		LastTransitionTime: metav1.NewTime(at),
	}}
	if _, err := cluster.Kube.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark node %s ready=%v: %v", name, isReady, err)
	}
}

// assertHealth checks the stored application health (C-08).
func assertHealth(t *testing.T, cluster *harness.Cluster, want v1beta1.HealthState) {
	t.Helper()
	status := cluster.ApplicationStatusOf(t, application)
	if status.Health.State != want {
		t.Fatalf("application health = %s, want %s", status.Health.State, want)
	}
}

// assertVolume checks the stored volume phase (C-38).
func assertVolume(t *testing.T, cluster *harness.Cluster, want string) {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Volume", volumeName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read volume phase: %v", err)
	}
	if phase != want {
		t.Fatalf("volume phase = %s, want %s", phase, want)
	}
}

// deploymentOf reads the rendered workload.
func deploymentOf(t *testing.T, cluster *harness.Cluster) *appsv1.Deployment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deployment, err := cluster.Kube.AppsV1().Deployments(harness.Namespace).Get(ctx, application, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the rendered Deployment: %v", err)
	}
	return deployment
}

// workloadCount counts the workloads in the namespace.
func workloadCount(t *testing.T, cluster *harness.Cluster) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := cluster.Kube.AppsV1().Deployments(harness.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list Deployments: %v", err)
	}
	return len(list.Items)
}
