// Package tc85 verifies the single-node degradation promise of AC-42 and F13:
// the control plane is one replica, a lost node neither recreates nor migrates
// the workload, and the installation says it promises no high availability.
package tc85

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/install"
	"github.com/tsic404/kos/pkg/nodehealth"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	nodeName       = "node-only"
	definitionName = "shop"
	application    = "shop"
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

func TestTC85SingleNodeDegradation(t *testing.T) {
	cluster := harness.Start(t)
	installerIdentity := harness.InstallerIdentity()
	cluster.GrantClusterAdmin(t, installerIdentity)
	identity := harness.OpenIdentity(t, nil, nil)
	clients := cluster.InstallClients(t, installerIdentity)
	ctx := context.Background()

	plan := harness.InstallerPlan(nil)
	runner, err := install.New(install.Config{
		Plan:          plan,
		Dynamic:       clients.Dynamic,
		Kube:          clients.Kube,
		APIExtensions: clients.APIExtensions,
		Definitions:   harness.Definitions(t),
		Accounts:      identity.Service,
		QuietWindow:   200 * time.Millisecond,
		SettleTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build installer: %v", err)
	}
	if _, err := runner.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}

	// One control-plane replica and no disruption budget: the single-node form
	// makes no high-availability promise (F13).
	controlPlane, err := cluster.Kube.AppsV1().Deployments(api.SystemNamespace).Get(ctx, install.ControlPlaneName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the control-plane deployment: %v", err)
	}
	if replicas := *controlPlane.Spec.Replicas; replicas != 1 {
		t.Fatalf("control-plane replicas = %d, want one", replicas)
	}
	if _, err := cluster.Kube.PolicyV1().PodDisruptionBudgets(api.SystemNamespace).List(ctx, metav1.ListOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("read disruption budgets: %v", err)
	}
	commitment := runner.Plan().Commitment()
	if commitment.Replicas != 1 || commitment.AutomaticMigration {
		t.Fatalf("commitment = %+v, want one replica and no automatic migration", commitment)
	}
	if !strings.Contains(commitment.Notice, "不承诺高可用") {
		t.Fatalf("the installation notice %q does not state the degraded form", commitment.Notice)
	}

	// One workload on the only node.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	createNode(t, cluster, nodeName)
	cluster.InstallDefinition(t, harness.DefinitionContent(t, definitionName, "", template, nil, nil))
	manifest := harness.ApplicationManifest(application, harness.Namespace, definitionName, harness.Namespace, nil, nil)
	cluster.Create(t, &unstructured.Unstructured{Object: manifest})
	controller := cluster.ApplicationController(t, app.Options{Clock: clock})
	if phase := cluster.SettleApplication(t, controller, application, 10); phase != app.PhaseReady {
		t.Fatalf("phase = %s, want Ready", phase)
	}
	cluster.SetWorkloadStatus(t, "Deployment", harness.Namespace, application, 1, 1)
	cluster.ApplicationRound(t, controller, application)
	workload := deploymentOf(t, cluster)

	// The only node goes away: the same workload object stays, nothing is
	// recreated on another node, and the application degrades in place.
	now = now.Add(time.Minute)
	setNodeReady(t, cluster, nodeName, false, now)
	cluster.ApplicationRound(t, controller, application)
	now = now.Add(nodehealth.DefaultGrace + nodehealth.ConfirmationWindow + time.Minute)
	cluster.ApplicationRound(t, controller, application)
	if got := deploymentOf(t, cluster); got.UID != workload.UID {
		t.Fatalf("the workload was recreated: uid %s became %s", workload.UID, got.UID)
	}
	if count := workloadCount(t, cluster); count != 1 {
		t.Fatalf("workloads = %d while the only node is lost, want one", count)
	}
	if replicas := controlPlaneReplicas(t, cluster); replicas != 1 {
		t.Fatalf("control-plane replicas = %d after the node loss, want one", replicas)
	}

	// The node returns and the same workload becomes healthy again.
	now = now.Add(time.Minute)
	setNodeReady(t, cluster, nodeName, true, now)
	cluster.ApplicationRound(t, controller, application)
	if got := deploymentOf(t, cluster); got.UID != workload.UID {
		t.Fatalf("the workload was replaced on recovery: uid %s became %s", workload.UID, got.UID)
	}
}

// createNode writes a Ready node.
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
		LastHeartbeatTime:  metav1.NewTime(at),
		LastTransitionTime: metav1.NewTime(at),
	}}
	if _, err := cluster.Kube.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark node %s ready=%v: %v", name, isReady, err)
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

// controlPlaneReplicas reads the control-plane replica count.
func controlPlaneReplicas(t *testing.T, cluster *harness.Cluster) int32 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	deployment, err := cluster.Kube.AppsV1().Deployments(api.SystemNamespace).Get(ctx, install.ControlPlaneName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the control-plane deployment: %v", err)
	}
	return *deployment.Spec.Replicas
}
