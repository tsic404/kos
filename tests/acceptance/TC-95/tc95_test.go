// Package tc95 verifies C-55's cluster upgrade over a node that holds a
// node-local application: the preflight names the application and the declared
// downtime, the upgrade stops it before the node is drained, the restore brings
// it back with its data in place, and an extension upgrade reports the
// contribution difference item by item (AC-48, TC-95).
package tc95

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/maintenance"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/upgrade"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	appsNamespace = "apps"
	application   = "shop"
	brokenApp     = "broken"
	nodeName      = "node-a"
	volumeName    = "data"
	storageClass  = "local"
	claimName     = "data"
	ownerName     = "owner"
	downtime      = "10m"
	fromVersion   = "v1.37.0"
	targetVersion = "v1.38.0"
	product       = "demo"
)

// shopTemplate renders the Deployment of the node-local application.
const shopTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 2
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
          image: nginx:1.27
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: {{ index .Volumes "data" }}
`

// TestTC95UpgradePreflightStopsAndRestores runs one cluster upgrade and one
// extension upgrade over the same cluster.
func TestTC95UpgradePreflightStopsAndRestores(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, appsNamespace)
	cluster.StorageClass(t, storageClass, "local.example", false, "Delete", nil)
	createNode(t, cluster, nodeName, fromVersion)
	cluster.CreateClaim(t, appsNamespace, claimName, storageClass)
	cluster.CreateIn(t, appsNamespace, boundVolume())
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, application, shopTemplate, nil))

	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Maintenance = maintenanceService(cluster, config.Auditor)
	})
	manifest := harness.ApplicationManifest(application, harness.Namespace, application, appsNamespace, nil, []string{volumeName})
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create the application: status %d, body %v", status, body)
	}
	createUnhealthyApplication(t, cluster)

	controller := cluster.ApplicationController(t, app.Options{})
	harness.Eventually(t, 30*time.Second, "the application reaches Ready", func() bool {
		cluster.ApplicationRound(t, controller, application)
		return cluster.ApplicationPhase(t, application) == app.PhaseReady
	})
	cluster.SetWorkloadStatus(t, "Deployment", appsNamespace, application, 2, 2)
	cluster.ApplicationRound(t, controller, application)

	// The node carries one pod of the node-local application and one pod nothing
	// binds to it: the drain must keep the first and move the second, and it may
	// only do so after the application stopped.
	shopPod := createPod(t, cluster, "shop-abc-1", "shop")
	movablePod := createPod(t, cluster, "batch-abc-1", "")

	flow := &upgrade.Flow{
		Maintenance:  maintenanceService(cluster, nil),
		Applications: upgrade.ClusterApplications{Dynamic: cluster.Dynamic},
		Nodes:        upgrade.ClusterNodes{Dynamic: cluster.Dynamic},
		Poll:         20 * time.Millisecond,
		Wait:         60 * time.Second,
	}
	request := upgrade.Request{Nodes: []string{nodeName}, Version: targetVersion, Downtime: downtime}

	// The preflight names what the upgrade affects and what it cannot judge
	// (C-55).
	report, failure := flow.Preflight(context.Background(), request)
	if failure != nil {
		t.Fatalf("preflight: %v", failure)
	}
	if len(report.Nodes) != 1 {
		t.Fatalf("the preflight plans %d nodes, want 1", len(report.Nodes))
	}
	plan := report.Nodes[0]
	if plan.Node != nodeName || plan.KubeletVersion != fromVersion {
		t.Fatalf("node plan = %+v, want %s at %s", plan, nodeName, fromVersion)
	}
	if len(plan.Applications) != 1 || plan.Applications[0] != harness.Namespace+"/"+application {
		t.Fatalf("affected applications = %v, want [%s/%s]", plan.Applications, harness.Namespace, application)
	}
	if len(plan.Volumes) != 1 || plan.Volumes[0] != appsNamespace+"/"+volumeName {
		t.Fatalf("affected volumes = %v, want [%s/%s]", plan.Volumes, appsNamespace, volumeName)
	}
	if plan.Downtime != downtime {
		t.Fatalf("downtime = %q, want %q", plan.Downtime, downtime)
	}
	if report.UnhealthyApplications != 1 {
		t.Fatalf("unhealthy applications = %d, want 1", report.UnhealthyApplications)
	}
	if report.Blocked {
		t.Fatalf("the preflight blocks an upgrade it can judge: %v", report.Undetermined)
	}

	// The drain refuses while the node-local application still runs, so the stop
	// always precedes the eviction, and a run that fails there puts the node back
	// instead of leaving it out of scheduling (C-55, C-65).
	impatient := &upgrade.Flow{
		Maintenance:  maintenanceService(cluster, nil),
		Applications: upgrade.ClusterApplications{Dynamic: cluster.Dynamic},
		Nodes:        upgrade.ClusterNodes{Dynamic: cluster.Dynamic},
		Poll:         20 * time.Millisecond,
		Wait:         200 * time.Millisecond,
	}
	if _, failure := impatient.Begin(context.Background(), request, ownerName); failure == nil {
		t.Fatal("the upgrade drained a node whose node-local application still runs")
	}
	if nodeUnschedulable(t, cluster, nodeName) {
		t.Fatal("the failed begin left the node out of scheduling")
	}
	if !podExists(t, cluster, shopPod) || !podExists(t, cluster, movablePod) {
		t.Fatal("the refused drain moved a pod")
	}

	// Begin takes the node out and waits for the application to stop; the
	// application's own reconciliation performs the stop (C-65).
	stopRounds := driveRounds(cluster, controller)
	result, failure := flow.Begin(context.Background(), request, ownerName)
	stopRounds()
	if failure != nil {
		t.Fatalf("begin the upgrade: %v", failure)
	}
	if !nodeUnschedulable(t, cluster, nodeName) {
		t.Fatal("the upgrade did not take the node out of scheduling")
	}
	if replicas := cluster.WorkloadReplicas(t, "Deployment", appsNamespace, application); replicas != 0 {
		t.Fatalf("the application still runs %d replicas during the upgrade", replicas)
	}
	if len(result.Nodes) != 1 {
		t.Fatalf("the upgrade reports %d node results, want 1", len(result.Nodes))
	}
	drain := result.Nodes[0].Drain
	if !only(drain.Evicted, appsNamespace+"/"+movablePod) {
		t.Fatalf("evicted = %v, want [%s/%s]", drain.Evicted, appsNamespace, movablePod)
	}
	if !retainedOf(t, drain)[appsNamespace+"/"+shopPod] {
		t.Fatalf("retained = %+v, want the node-local application's pod", drain.Retained)
	}
	if podExists(t, cluster, movablePod) {
		t.Fatal("the drain left the movable pod on the node")
	}
	if !podExists(t, cluster, shopPod) {
		t.Fatal("the drain moved the node-local application off its node")
	}
	if !cluster.ClaimExists(t, appsNamespace, claimName) {
		t.Fatal("the upgrade removed the claim that holds the data")
	}
	if node := volumeNode(t, cluster); node != nodeName {
		t.Fatalf("the data moved to node %q, want %q", node, nodeName)
	}

	// The operator finishes the node, which reports the target version, and the
	// upgrade restores what it stopped (C-55).
	setKubeletVersion(t, cluster, nodeName, targetVersion)
	if _, failure := flow.Finish(context.Background(), result, ownerName); failure != nil {
		t.Fatalf("finish the upgrade: %v", failure)
	}
	if nodeUnschedulable(t, cluster, nodeName) {
		t.Fatal("the upgrade left the node out of scheduling")
	}
	harness.Eventually(t, 30*time.Second, "the application returns to Ready", func() bool {
		cluster.ApplicationRound(t, controller, application)
		return cluster.WorkloadReplicas(t, "Deployment", appsNamespace, application) == 2 &&
			cluster.ApplicationPhase(t, application) == app.PhaseReady
	})
	if !cluster.ClaimExists(t, appsNamespace, claimName) || volumeNode(t, cluster) != nodeName {
		t.Fatal("the data moved during the upgrade")
	}

	// The extension upgrade reports its contribution difference item by item
	// (C-55).
	checkExtensionUpgrade(t, cluster)
}

// checkExtensionUpgrade installs one product, preflights its upgrade and applies
// it, asserting the difference the preflight reports (C-55).
func checkExtensionUpgrade(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	artifacts := extension.MemoryArtifacts{}
	before := extension.Declaration{
		Identifier: product,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Pages: []extension.Page{demoPage()},
		},
	}
	cluster.InstallPackageWith(t, product, before, artifacts, nil)

	after := extension.Declaration{
		Identifier: product,
		Version:    "2.0.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{{
				ID: "demo-setting", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true,
			}},
		},
	}
	content, err := extension.Marshal(after)
	if err != nil {
		t.Fatalf("marshal the target declaration: %v", err)
	}
	reference := "test://" + product + "-2"
	artifacts[reference] = content
	digest := extension.DigestOf(content)

	preflight, err := cluster.ExtensionInstaller(artifacts).UpgradePreflight(context.Background(), []extension.UpgradeTarget{{
		Component: product, Reference: reference, Digest: digest,
	}})
	if err != nil {
		t.Fatalf("preflight the extension upgrade: %v", err)
	}
	if len(preflight.Components) != 1 {
		t.Fatalf("the extension preflight reports %d components, want 1", len(preflight.Components))
	}
	changes := preflight.Components[0].Changes
	if len(changes) != 2 {
		t.Fatalf("contribution changes = %+v, want one per moved class", changes)
	}
	if change := changeOf(t, changes, extension.ClassPages); !only(change.Removed, "demo") || len(change.Added) != 0 {
		t.Fatalf("pages change = %+v, want demo removed", change)
	}
	if change := changeOf(t, changes, extension.ClassSettings); !only(change.Added, "demo-setting") || len(change.Removed) != 0 {
		t.Fatalf("settings change = %+v, want demo-setting added", change)
	}
	if preflight.IsBlocked {
		t.Fatalf("the extension upgrade is blocked: %+v", preflight.Components[0].Problems)
	}

	// The upgrade itself is the replacement the component declares, judged by
	// the same evaluation an install runs (C-55).
	cluster.RepointComponent(t, product, reference, digest)
	installation := cluster.ExtensionState(t, artifacts)
	component, found := installation.Component(product)
	if !found {
		t.Fatal("the upgraded component disappeared from the installation")
	}
	if len(component.Contributions.Pages) != 0 {
		t.Fatalf("the upgraded product still contributes pages: %+v", component.Contributions.Pages)
	}
	if _, served := installation.Registry().Setting("demo-setting"); !served {
		t.Fatal("the upgraded product does not contribute its new setting")
	}
}

// demoPage is the page the first version of the product contributes.
func demoPage() extension.Page {
	return extension.Page{
		ID:             "demo",
		Title:          "演示",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: "本页只读：演示",
		Items: []extension.PageItem{{
			ID: "demo-item", Label: "演示", Source: extension.PageSourceComputed, Reference: "demo/demo-item",
		}},
	}
}

// changeOf reads one class's difference.
func changeOf(t *testing.T, changes []extension.ContributionChange, class string) extension.ContributionChange {
	t.Helper()
	for _, change := range changes {
		if change.Class == class {
			return change
		}
	}
	t.Fatalf("the preflight reports no change for %q: %+v", class, changes)
	return extension.ContributionChange{}
}

// driveRounds advances the application reconciler in the background until the
// returned function is called. The loop never touches the test, so it is safe
// to run beside the upgrade: the case under test is the orchestration.
func driveRounds(cluster *harness.Cluster, controller *app.Controller) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
			}
			_, _ = controller.Reconcile(context.Background(), reconcile.Request{
				Kind: "Application", Namespace: harness.Namespace, Name: application,
			})
			time.Sleep(10 * time.Millisecond)
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// maintenanceService builds the C-65 flow over the case's cluster.
func maintenanceService(cluster *harness.Cluster, auditor controlplane.Auditor) *maintenance.Service {
	return &maintenance.Service{
		Dynamic: cluster.Dynamic,
		Writer:  cluster.Writer(),
		Pods:    maintenance.Cluster{Kube: cluster.Kube},
		Auditor: auditor,
	}
}

// createPod writes one pod on the case's node. A pod that names a deployment is
// owned by that deployment's ReplicaSet, so a drain resolves the workload the
// application rendered.
func createPod(t *testing.T, cluster *harness.Cluster, name, deployment string) string {
	t.Helper()
	metadata := map[string]any{"name": name, "namespace": appsNamespace}
	if deployment != "" {
		metadata["ownerReferences"] = []any{map[string]any{
			"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": deployment + "-abc",
			"uid": "uid-" + deployment, "controller": true,
		}}
		cluster.CreateClusterObject(t, "apps", "v1", "replicasets", appsNamespace, map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "ReplicaSet",
			"metadata": map[string]any{
				"name":      deployment + "-abc",
				"namespace": appsNamespace,
				"ownerReferences": []any{map[string]any{
					"apiVersion": "apps/v1", "kind": "Deployment", "name": deployment,
					"uid": "uid-" + deployment, "controller": true,
				}},
			},
			"spec": map[string]any{
				"replicas": int64(2),
				"selector": map[string]any{"matchLabels": map[string]any{"app": deployment}},
				"template": map[string]any{
					"metadata": map[string]any{"labels": map[string]any{"app": deployment}},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "app", "image": "nginx:1.27"}}},
				},
			},
		})
	}
	cluster.CreateClusterObject(t, "", "v1", "pods", appsNamespace, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   metadata,
		"spec": map[string]any{
			"nodeName":                      nodeName,
			"terminationGracePeriodSeconds": int64(0),
			"containers":                    []any{map[string]any{"name": "app", "image": "nginx:1.27"}},
		},
	})
	return name
}

// podExists reports whether the cluster still holds one pod.
func podExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, "", "v1", "Pod", appsNamespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// retainedOf reads the pods one drain left on the node.
func retainedOf(t *testing.T, drain controlplane.MaintenanceDrain) map[string]bool {
	t.Helper()
	retained := map[string]bool{}
	for _, entry := range drain.Retained {
		retained[entry.Pod] = true
	}
	return retained
}

// createUnhealthyApplication writes one application whose health is Degraded,
// which the platform upgrade warns about (C-55).
func createUnhealthyApplication(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	body := harness.Manifest("Application", harness.Namespace, brokenApp, nil, nil)
	body["spec"] = map[string]any{"definitionRef": "missing", "namespace": appsNamespace}
	body["status"] = map[string]any{"phase": "Failed", "health": map[string]any{"state": string(v1beta1.HealthStateDegraded)}}
	cluster.Create(t, &unstructured.Unstructured{Object: body})
}

// boundVolume is the node-local volume whose data sits on the case's node.
func boundVolume() *unstructured.Unstructured {
	object := harness.Volume(appsNamespace, volumeName, map[string]any{
		"owner":        ownerName,
		"mobility":     "node-local",
		"capacity":     "1Gi",
		"storageClass": storageClass,
	})
	object.Object["status"] = map[string]any{"node": nodeName}
	return object
}

// volumeNode reads where the volume's data sits.
func volumeNode(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	volume := cluster.GetIn(t, appsNamespace, "v1beta1", "Volume", volumeName)
	node, _, _ := unstructured.NestedString(volume.Object, "status", "node")
	return node
}

// createNode writes one ready node that reports version.
func createNode(t *testing.T, cluster *harness.Cluster, name, version string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
	})
	setKubeletVersion(t, cluster, name, version)
}

// setKubeletVersion writes the version the node reports, which is what ends its
// maintenance (C-55).
func setKubeletVersion(t *testing.T, cluster *harness.Cluster, name, version string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes := cluster.Direct(t, "", "v1", "Node", "")
	node, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	node.Object["status"] = map[string]any{
		"conditions":  []any{map[string]any{"type": "Ready", "status": "True"}},
		"allocatable": map[string]any{"cpu": "4", "memory": "8Gi"},
		"nodeInfo":    map[string]any{"kubeletVersion": version},
	}
	if _, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the status of node %q: %v", name, err)
	}
}

// nodeUnschedulable reads the maintenance state the flow writes (C-65).
func nodeUnschedulable(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := cluster.Direct(t, "", "v1", "Node", "").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	cordoned, _, _ := unstructured.NestedBool(node.Object, "spec", "unschedulable")
	return cordoned
}

// only reports whether values holds exactly want.
func only(values []string, want string) bool {
	return len(values) == 1 && values[0] == want
}
