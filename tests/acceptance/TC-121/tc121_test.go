// Package tc121 verifies C-65's maintenance flow over a node that holds a
// node-local application and a movable one: the preflight names both the
// application and the declared downtime, the stop happens before the eviction,
// the node-local application keeps its data where it is, and the restore
// returns it to its own load path with the application spec untouched
// (AC-60, TC-121).
package tc121

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/maintenance"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	appsNamespace = "apps"
	nodeLocalApp  = "shop"
	movableApp    = "cache"
	nodeName      = "node-a"
	volumeName    = "data"
	storageClass  = "local"
	claimName     = "data"
	ownerName     = "owner"
	downtime      = "15m"
)

// maintenanceTemplate renders the Deployment the node-local application runs,
// mounting the declared volume.
const maintenanceTemplate = `apiVersion: apps/v1
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

// movableTemplate renders the Deployment of the application that declares no
// volume, so a drain may move it.
const movableTemplate = `apiVersion: apps/v1
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
`

// TestTC121MaintenancePreflightAndNodeLocalProtection runs the four steps of
// C-65 over a node that holds one node-local application and one movable
// application.
func TestTC121MaintenancePreflightAndNodeLocalProtection(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, appsNamespace)
	cluster.StorageClass(t, storageClass, "local.example", false, "Delete", nil)
	createNode(t, cluster, nodeName)
	cluster.CreateClaim(t, appsNamespace, claimName, storageClass)
	cluster.CreateIn(t, appsNamespace, boundVolume())
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, nodeLocalApp, maintenanceTemplate, nil))
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, movableApp, movableTemplate, nil))

	auditor := &harness.AuditRecorder{}
	service := &maintenance.Service{
		Dynamic: cluster.Dynamic,
		Writer:  cluster.Writer(),
		Pods:    maintenance.Cluster{Kube: cluster.Kube},
		Auditor: auditor,
	}
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Auditor = auditor
		config.Maintenance = service
	})
	createApplication(t, gateway, nodeLocalApp, nodeLocalApp, []string{volumeName})
	createApplication(t, gateway, movableApp, movableApp, nil)

	controller := cluster.ApplicationController(t, app.Options{})
	settleApplication(t, cluster, controller, nodeLocalApp)
	settleApplication(t, cluster, controller, movableApp)
	// The run kernel reports the running replicas; the stop's health rule reads
	// them (C-08).
	for _, name := range []string{nodeLocalApp, movableApp} {
		cluster.SetWorkloadStatus(t, "Deployment", appsNamespace, name, 2, 2)
		cluster.ApplicationRound(t, controller, name)
	}
	specBefore := harness.ShapeOf(cluster.Get(t, "v1beta1", "Application", nodeLocalApp))

	shopPod := createPod(t, cluster, "shop-abc-1", replicaSetOwner("shop-abc"))
	cachePod := createPod(t, cluster, "cache-abc-1", replicaSetOwner("cache-abc"))
	daemonPod := createPod(t, cluster, "agent-x", ownerReference{kind: "DaemonSet", name: "agent"})
	createReplicaSet(t, cluster, "shop-abc", nodeLocalApp)
	createReplicaSet(t, cluster, "cache-abc", movableApp)

	// An item the preflight cannot judge is not approved by default: without the
	// declared downtime the outage is undetermined and the flow does not start
	// (C-65).
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance/preflight", map[string]any{"node": nodeName}, nil)
	if status != http.StatusOK {
		t.Fatalf("preflight without a downtime: status %d, body %v", status, body)
	}
	if items := undeterminedOf(t, reportOf(t, body)); !contains(items, "downtime") {
		t.Fatalf("undetermined items = %v, want the missing downtime", items)
	}
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance", map[string]any{"node": nodeName}, nil)
	if status != http.StatusConflict {
		t.Fatalf("a start with an undetermined preflight: status %d, body %v, want 409", status, body)
	}
	if nodeUnschedulable(t, cluster, nodeName) {
		t.Fatal("the refused start took the node out of scheduling")
	}

	// The preflight names what the maintenance takes out of service (C-65).
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance/preflight", map[string]any{
		"node": nodeName, "downtime": downtime,
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("preflight: status %d, body %v", status, body)
	}
	preflight := reportOf(t, body)
	if got := stringsOf(t, preflight["applications"]); len(got) != 1 || got[0] != harness.Namespace+"/"+nodeLocalApp {
		t.Fatalf("affected applications = %v, want [%s/%s]", got, harness.Namespace, nodeLocalApp)
	}
	if got := stringsOf(t, preflight["volumes"]); len(got) != 1 || got[0] != appsNamespace+"/"+volumeName {
		t.Fatalf("affected volumes = %v, want [%s/%s]", got, appsNamespace, volumeName)
	}
	if preflight["downtime"] != downtime {
		t.Fatalf("downtime = %v, want %q", preflight["downtime"], downtime)
	}
	if items, _ := preflight["undetermined"].([]any); len(items) != 0 {
		t.Fatalf("unexpected undetermined items: %v", items)
	}

	// Step two: the cordon is written by the API process and recorded (C-65).
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance", map[string]any{
		"node": nodeName, "downtime": downtime,
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("cordon: status %d, body %v", status, body)
	}
	if !nodeUnschedulable(t, cluster, nodeName) {
		t.Fatal("the cordon did not take the node out of scheduling")
	}
	// A second start on the same node writes nothing: the flow reports that this
	// call cordoned nothing, so a restore built on that answer cannot lift a
	// cordon it did not set, and the audit trail gains no record (C-65).
	before := len(auditor.Verbs())
	repeated, wrote, failure := service.Cordon(context.Background(), controlplane.MaintenanceRequest{
		Node: nodeName, Downtime: downtime,
	}, ownerName)
	if failure != nil {
		t.Fatalf("a repeated start: %v", failure)
	}
	if wrote {
		t.Fatal("a start on an already cordoned node reported a write")
	}
	if len(repeated.Applications) != 1 || repeated.Applications[0] != harness.Namespace+"/"+nodeLocalApp {
		t.Fatalf("the repeated start reports %v, want the preflight the node carries", repeated.Applications)
	}
	if after := len(auditor.Verbs()); after != before {
		t.Fatalf("a repeated start wrote %d records: %v", after-before, auditor.Verbs())
	}
	// The application enters the maintenance while it is upgrading, so the phase
	// the stop must hold is one a hardcoded value would not produce (C-66).
	setApplicationPhase(t, cluster, nodeLocalApp, app.PhaseUpgrading)

	// The node-local application stops itself without the spec moving, and the
	// movable one keeps running (C-65, C-66).
	harness.Eventually(t, 30*time.Second, "the node-local application stops", func() bool {
		cluster.ApplicationRound(t, controller, nodeLocalApp)
		return cluster.WorkloadReplicas(t, "Deployment", appsNamespace, nodeLocalApp) == 0
	})
	if replicas := cluster.WorkloadReplicas(t, "Deployment", appsNamespace, movableApp); replicas != 2 {
		t.Fatalf("the movable application stopped with %d replicas, want 2", replicas)
	}
	stopped := cluster.ApplicationStatusOf(t, nodeLocalApp)
	if stopped.Phase != app.PhaseUpgrading {
		t.Fatalf("stopped phase = %q, want the pre-stop %q", stopped.Phase, app.PhaseUpgrading)
	}
	if stopped.Health.State != v1beta1.HealthStateHealthy {
		t.Fatalf("stopped health = %q, want Healthy", stopped.Health.State)
	}
	condition, found := conditionOf(t, cluster, nodeLocalApp, app.ConditionQuiesced)
	if !found || condition.Reason != app.ReasonNodeMaintenance {
		t.Fatalf("the stop records %q/%q, want %s/%s", condition.Type, condition.Reason, app.ConditionQuiesced, app.ReasonNodeMaintenance)
	}

	// Step three: the drain moves the movable workload and keeps the node-local
	// one on its node (C-65).
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance/drain", map[string]any{"node": nodeName}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("drain: status %d, body %v", status, body)
	}
	drain, _ := body["drain"].(map[string]any)
	if got := stringsOf(t, drain["evicted"]); len(got) != 1 || got[0] != appsNamespace+"/"+cachePod {
		t.Fatalf("evicted = %v, want [%s/%s]", got, appsNamespace, cachePod)
	}
	retained := retainedOf(t, drain)
	if !retained[appsNamespace+"/"+shopPod] || !retained[appsNamespace+"/"+daemonPod] {
		t.Fatalf("retained = %v, want the node-local application pod and the daemon set pod", retained)
	}
	if podExists(t, cluster, cachePod) {
		t.Fatal("the drain left the movable pod on the node")
	}
	if !podExists(t, cluster, shopPod) {
		t.Fatal("the drain moved the node-local application off its node")
	}

	// Step four: the restore lifts the cordon and the application returns to its
	// own load path (C-65, C-33).
	status, body = gateway.Call(t, http.MethodDelete, "/api/v1/node-maintenance/"+nodeName, nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore: status %d, body %v", status, body)
	}
	if nodeUnschedulable(t, cluster, nodeName) {
		t.Fatal("the restore left the node out of scheduling")
	}
	harness.Eventually(t, 30*time.Second, "the node-local application returns", func() bool {
		cluster.ApplicationRound(t, controller, nodeLocalApp)
		return cluster.WorkloadReplicas(t, "Deployment", appsNamespace, nodeLocalApp) == 2
	})
	if _, found := conditionOf(t, cluster, nodeLocalApp, app.ConditionQuiesced); found {
		t.Fatal("the restore left the stop condition behind")
	}
	if phase := cluster.ApplicationPhase(t, nodeLocalApp); phase != app.PhaseReady {
		t.Fatalf("restored phase = %q, want Ready", phase)
	}
	specAfter := harness.ShapeOf(cluster.Get(t, "v1beta1", "Application", nodeLocalApp))
	if !reflect.DeepEqual(specBefore, specAfter) {
		t.Fatalf("the maintenance changed the application spec:\n before %v\n after  %v", specBefore, specAfter)
	}
	if !cluster.ClaimExists(t, appsNamespace, claimName) {
		t.Fatal("the maintenance removed the claim that holds the data")
	}

	// Both steps leave the pair of records the attribution rule reads (C-65).
	verbs := auditor.Verbs()
	if !contains(verbs, controlplane.VerbMaintain) || !contains(verbs, controlplane.VerbRestore) {
		t.Fatalf("audit verbs = %v, want %s and %s", verbs, controlplane.VerbMaintain, controlplane.VerbRestore)
	}
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

// createApplication writes one application through the control plane.
func createApplication(t *testing.T, gateway *harness.ControlPlane, name, definition string, volumes []string) {
	t.Helper()
	manifest := harness.ApplicationManifest(name, harness.Namespace, definition, appsNamespace, nil, volumes)
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create application %s: status %d, body %v", name, status, body)
	}
}

// settleApplication runs rounds until the application reaches Ready.
func settleApplication(t *testing.T, cluster *harness.Cluster, controller *app.Controller, name string) {
	t.Helper()
	harness.Eventually(t, 30*time.Second, "application "+name+" reaches Ready", func() bool {
		cluster.ApplicationRound(t, controller, name)
		return cluster.ApplicationPhase(t, name) == app.PhaseReady
	})
}

// ownerReference is one pod owner the drain resolves.
type ownerReference struct {
	kind string
	name string
}

// replicaSetOwner names the ReplicaSet a workload created.
func replicaSetOwner(name string) ownerReference {
	return ownerReference{kind: "ReplicaSet", name: name}
}

// createPod writes one pod scheduled to the case's node.
func createPod(t *testing.T, cluster *harness.Cluster, name string, owner ownerReference) string {
	t.Helper()
	ownerReferences := []any{map[string]any{
		"apiVersion": "apps/v1", "kind": owner.kind, "name": owner.name,
		"uid": "uid-" + owner.name, "controller": true,
	}}
	cluster.CreateClusterObject(t, "", "v1", "pods", appsNamespace, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "namespace": appsNamespace, "ownerReferences": ownerReferences},
		"spec": map[string]any{
			"nodeName":                      nodeName,
			"terminationGracePeriodSeconds": int64(0),
			"containers":                    []any{map[string]any{"name": "app", "image": "nginx:1.27"}},
		},
	})
	return name
}

// createReplicaSet writes the ReplicaSet one Deployment owns, so a pod's owner
// chain resolves to the workload the application rendered.
func createReplicaSet(t *testing.T, cluster *harness.Cluster, name, deployment string) {
	t.Helper()
	cluster.CreateClusterObject(t, "apps", "v1", "replicasets", appsNamespace, map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "ReplicaSet",
		"metadata": map[string]any{
			"name":      name,
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

// createNode writes one ready, schedulable node.
func createNode(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
	})
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
		"nodeInfo":    map[string]any{"kubeletVersion": "v1.37.0"},
	}
	if _, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the status of node %q: %v", name, err)
	}
}

// setApplicationPhase writes the phase the application enters maintenance with,
// through the reconciler's own status part, so the stop has a value to hold that
// a hardcoded phase would not reproduce.
func setApplicationPhase(t *testing.T, cluster *harness.Cluster, name, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stored, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read application %q: %v", name, err)
	}
	status, found, err := unstructured.NestedMap(stored.Object, "status")
	if err != nil || !found {
		t.Fatalf("application %q carries no status: %v", name, err)
	}
	status["phase"] = phase
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"status":     status,
	}}
	if _, err := cluster.Writer().Apply(ctx, write.SubjectApplicationReconciler, write.Session{}, write.PartStatus, document); err != nil {
		t.Fatalf("write the phase of application %q: %v", name, err)
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

// reportOf reads the preflight report out of one response body.
func reportOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	report, ok := body["preflight"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no preflight report: %v", body)
	}
	return report
}

// undeterminedOf reads the items one preflight could not judge.
func undeterminedOf(t *testing.T, report map[string]any) []string {
	t.Helper()
	items, _ := report["undetermined"].([]any)
	names := make([]string, 0, len(items))
	for _, item := range items {
		entry, _ := item.(map[string]any)
		name, _ := entry["item"].(string)
		names = append(names, name)
	}
	return names
}

// stringsOf reads a JSON string array.
func stringsOf(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, _ := item.(string)
		names = append(names, name)
	}
	return names
}

// retainedOf reads the pods one drain left on the node.
func retainedOf(t *testing.T, drain map[string]any) map[string]bool {
	t.Helper()
	retained := map[string]bool{}
	items, _ := drain["retained"].([]any)
	for _, item := range items {
		entry, _ := item.(map[string]any)
		name, _ := entry["pod"].(string)
		retained[name] = true
	}
	return retained
}

// podExists reports whether the cluster still holds one pod.
func podExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, "", "v1", "Pod", appsNamespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// conditionOf reads one condition of the stored application.
func conditionOf(t *testing.T, cluster *harness.Cluster, name, conditionType string) (metav1.Condition, bool) {
	t.Helper()
	conditions, err := harness.ConditionsOf(cluster.Get(t, "v1beta1", "Application", name))
	if err != nil {
		t.Fatalf("read the conditions of %q: %v", name, err)
	}
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// contains reports whether values names want.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
