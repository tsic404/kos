// Package tc122 verifies C-65's attribution rule over maintenance KOS did not
// start: a node taken out of scheduling with the cluster's own interface is
// reported as an unplanned maintenance with the applications it affects, the
// stop KOS performs is recorded rather than silent, and lifting the cordon by
// hand ends the attribution. A node that only has a pod evicted keeps no
// maintenance, and a role other than the Owner only reads the state
// (AC-60, TC-122).
package tc122

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/maintenance"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/settings/kossettings"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	appsNamespace   = "apps"
	application     = "shop"
	cordonedNode    = "node-a"
	drainedNode     = "node-b"
	quietNode       = "node-c"
	volumeName      = "data"
	storageClass    = "local"
	claimName       = "data"
	hiddenApp       = "hidden"
	ownerName       = "owner"
	ownerPassword   = "owner-password"
	memberName      = "member"
	memberPass      = "member-password"
	maintenanceKind = "Node"
)

// shopTemplate renders the Deployment the case's node-local application runs.
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

// TestTC122OutOfBandCordonIsVisible reports what a maintenance started outside
// the control plane looks like (C-65).
func TestTC122OutOfBandCordonIsVisible(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, appsNamespace)
	cluster.StorageClass(t, storageClass, "local.example", false, "Delete", nil)
	for _, node := range []string{cordonedNode, drainedNode, quietNode} {
		createNode(t, cluster, node)
	}
	cluster.CreateClaim(t, appsNamespace, claimName, storageClass)
	cluster.CreateIn(t, appsNamespace, boundVolume())
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, application, shopTemplate, nil))
	createApplication(t, cluster)
	// An application the member may not see: it declares a volume no object
	// backs, so the preflight reports an item it cannot judge about it (C-65).
	hidden := harness.ApplicationManifest(hiddenApp, harness.Namespace, application, appsNamespace, nil, []string{"missing"})
	cluster.Create(t, &unstructured.Unstructured{Object: hidden})
	controller := cluster.ApplicationController(t, app.Options{})
	harness.Eventually(t, 30*time.Second, "the application reaches Ready", func() bool {
		cluster.ApplicationRound(t, controller, application)
		return cluster.ApplicationPhase(t, application) == app.PhaseReady
	})
	cluster.SetWorkloadStatus(t, "Deployment", appsNamespace, application, 2, 2)
	cluster.ApplicationRound(t, controller, application)

	readings := &kossettings.Readings{Dynamic: cluster.Dynamic}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		config.Readings = pages.Router{kossettings.Identifier: readings}
	})
	readings.Settings = shell.Settings
	readings.Accounts = shell.Accounts.Store
	readings.Audit = shell.Accounts.Store
	installation := cluster.InstallPackageWith(t, "settings", kossettings.Declaration(), extension.MemoryArtifacts{}, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPassword)
	memberToken := shell.Account(t, token, memberName, memberPass, write.RoleMember)

	// Out of band: the cluster interface cordons node-a and evicts its pod, so
	// no KOS record of the write exists (C-65).
	createPod(t, cluster, "shop-abc-1", cordonedNode)
	cordonExternally(t, cluster, cordonedNode)
	deletePod(t, cluster, "shop-abc-1")
	// The application enters the stop while it is upgrading, so the phase the
	// stop must hold is one a hardcoded value would not produce (C-66).
	setApplicationPhase(t, cluster, application, app.PhaseUpgrading)

	if origin := nodeOrigin(t, shell, token, cordonedNode); origin != kossettings.MaintenanceUnplanned {
		t.Fatalf("maintenance origin = %q, want %q", origin, kossettings.MaintenanceUnplanned)
	}
	affected := nodeAffected(t, shell, token, cordonedNode)
	if len(affected) != 1 || affected[0] != harness.Namespace+"/"+application {
		t.Fatalf("affected applications = %v, want [%s/%s]", affected, harness.Namespace, application)
	}

	// The stop KOS performs is recorded, not silent: the application keeps its
	// phase, stops its workloads and states the node it stopped for (C-65, C-66).
	harness.Eventually(t, 30*time.Second, "the node-local application stops", func() bool {
		cluster.ApplicationRound(t, controller, application)
		return cluster.WorkloadReplicas(t, "Deployment", appsNamespace, application) == 0
	})
	condition, found := quiescedCondition(t, cluster)
	if !found || condition.Reason != app.ReasonNodeMaintenance {
		t.Fatalf("the stop records %q/%q, want %s/%s", condition.Type, condition.Reason, app.ConditionQuiesced, app.ReasonNodeMaintenance)
	}
	if phase := cluster.ApplicationPhase(t, application); phase != app.PhaseUpgrading {
		t.Fatalf("stopped phase = %q, want the pre-stop %q", phase, app.PhaseUpgrading)
	}
	if state := cluster.ApplicationStatusOf(t, application).Health.State; state != v1beta1.HealthStateHealthy {
		t.Fatalf("stopped health = %q, want Healthy", state)
	}

	// Only the Owner may read this state; a member of the cluster may not start
	// a maintenance of its own (C-59, C-65).
	if origin := nodeOrigin(t, shell, memberToken, cordonedNode); origin != kossettings.MaintenanceUnplanned {
		t.Fatalf("the member reads maintenance origin = %q, want %q", origin, kossettings.MaintenanceUnplanned)
	}
	access, ownerToken := startRefusedForMember(t, cluster)

	// Lifting the cordon by hand ends the attribution, and a start record with
	// no matching restore produces none once the node schedules again (C-65).
	setSchedulable(t, cluster, cordonedNode, false)
	if origin := nodeOrigin(t, shell, token, cordonedNode); origin != kossettings.MaintenanceNone {
		t.Fatalf("origin after the manual restore = %q, want %q", origin, kossettings.MaintenanceNone)
	}
	recordStart(t, shell, quietNode)
	if origin := nodeOrigin(t, shell, token, quietNode); origin != kossettings.MaintenanceNone {
		t.Fatalf("origin of a schedulable node with an unpaired start record = %q, want %q", origin, kossettings.MaintenanceNone)
	}

	// A node that only loses a pod keeps no maintenance: eviction alone is not a
	// maintenance (C-65). The pod is evicted through the kernel's own eviction
	// interface, the one a maintenance drain uses, and the drain route refuses
	// the node because nothing cordoned it. Neither path writes an attribution,
	// so an implementation that treated an eviction as a maintenance fails here.
	createPod(t, cluster, "cache-abc-1", drainedNode)
	evictThroughKernel(t, cluster, "cache-abc-1")
	if podExists(t, cluster, "cache-abc-1") {
		t.Fatal("the kernel did not evict the pod")
	}
	status, body := access.Call(t, ownerToken, http.MethodPost, "/api/v1/node-maintenance/drain", map[string]any{"node": drainedNode}, nil)
	if status != http.StatusConflict {
		t.Fatalf("drain of a node nothing cordoned: status %d, body %v, want 409", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodeConflict {
		t.Fatalf("drain refusal code = %q, want %q", code, controlplane.CodeConflict)
	}
	if !schedulable(t, cluster, drainedNode) {
		t.Fatal("the eviction or the refused drain took the node out of scheduling")
	}
	if records := maintenanceRecords(t, shell, drainedNode); len(records) != 0 {
		t.Fatalf("the eviction left maintenance records on %q: %+v", drainedNode, records)
	}
	if origin := nodeOrigin(t, shell, token, drainedNode); origin != kossettings.MaintenanceNone {
		t.Fatalf("origin of a node with only an eviction = %q, want %q", origin, kossettings.MaintenanceNone)
	}
}

// startRefusedForMember posts a maintenance start as a member and expects the
// role rule of C-59 to refuse it, then reads the preflight as both roles.
func startRefusedForMember(t *testing.T, cluster *harness.Cluster) (*harness.Access, string) {
	t.Helper()
	access := cluster.StartAccess(t, nil, func(config *controlplane.Config) {
		config.Maintenance = &maintenance.Service{
			Dynamic: cluster.Dynamic,
			Writer:  cluster.Writer(),
			Pods:    maintenance.Cluster{Kube: cluster.Kube},
			Auditor: config.Auditor,
		}
	})
	ownerToken := access.Owner(t, ownerName, ownerPassword)
	memberToken := access.Account(t, ownerToken, memberName, memberPass, write.RoleMember)
	status, body := access.Call(t, memberToken, http.MethodPost, "/api/v1/node-maintenance", map[string]any{
		"node": quietNode, "downtime": "15m",
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("a member's maintenance start: status %d, body %v, want 403", status, body)
	}
	if code := failureCode(t, body); code != controlplane.CodePermissionDenied {
		t.Fatalf("refusal code = %q, want %q", code, controlplane.CodePermissionDenied)
	}
	if !schedulable(t, cluster, quietNode) {
		t.Fatal("the refused start took the node out of scheduling")
	}

	// A member reads the maintenance state, but the preflight names only the
	// objects that member may see: the read seam and C-59's visibility predicate
	// apply to it like to any other read (C-59, C-65).
	memberReport := preflightOf(t, access, memberToken, cordonedNode)
	if affected := applicationsOf(t, memberReport); len(affected) != 0 {
		t.Fatalf("a member's preflight names applications it may not read: %v", affected)
	}
	if items := undeterminedOf(t, memberReport); len(items) != 0 {
		t.Fatalf("a member's preflight names items it may not read: %v", items)
	}
	ownerReport := preflightOf(t, access, ownerToken, cordonedNode)
	if affected := applicationsOf(t, ownerReport); len(affected) != 1 || affected[0] != harness.Namespace+"/"+application {
		t.Fatalf("the owner's preflight names %v, want [%s/%s]", affected, harness.Namespace, application)
	}
	if items := undeterminedOf(t, ownerReport); !contains(items, "应用 "+harness.Namespace+"/"+hiddenApp) {
		t.Fatalf("the owner's preflight does not name the application it cannot judge: %v", items)
	}
	return access, ownerToken
}

// preflightOf reads one node's preflight as one role.
func preflightOf(t *testing.T, access *harness.Access, token, node string) map[string]any {
	t.Helper()
	status, body := access.Call(t, token, http.MethodPost, "/api/v1/node-maintenance/preflight", map[string]any{
		"node": node, "downtime": "15m",
	}, nil)
	if status != http.StatusOK {
		t.Fatalf("preflight of %q as %q: status %d, body %v", node, token, status, body)
	}
	report, ok := body["preflight"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries no preflight report: %v", body)
	}
	return report
}

// applicationsOf reads the applications one preflight report names.
func applicationsOf(t *testing.T, report map[string]any) []string {
	t.Helper()
	applications, _ := report["applications"].([]any)
	names := make([]string, 0, len(applications))
	for _, item := range applications {
		name, _ := item.(string)
		names = append(names, name)
	}
	return names
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

// contains reports whether values names want.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// evictThroughKernel evicts one pod through the interface a maintenance drain
// uses, the way an outside actor would.
func evictThroughKernel(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := (maintenance.Cluster{Kube: cluster.Kube}).Evict(ctx, appsNamespace, name); err != nil {
		t.Fatalf("evict pod %q through the kernel: %v", name, err)
	}
}

// podExists reports whether the cluster still holds one pod.
func podExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, "", "v1", "Pod", appsNamespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// boundVolume is the node-local volume whose data sits on node-a.
func boundVolume() *unstructured.Unstructured {
	object := harness.Volume(appsNamespace, volumeName, map[string]any{
		"owner":        ownerName,
		"mobility":     "node-local",
		"capacity":     "1Gi",
		"storageClass": storageClass,
	})
	object.Object["status"] = map[string]any{"node": cordonedNode}
	return object
}

// createApplication writes the application the case maintains around.
func createApplication(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	manifest := harness.ApplicationManifest(application, harness.Namespace, application, appsNamespace, nil, []string{volumeName})
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	cluster.Create(t, &unstructured.Unstructured{Object: manifest})
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

// createPod writes one pod scheduled to one node.
func createPod(t *testing.T, cluster *harness.Cluster, name, node string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "pods", appsNamespace, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name, "namespace": appsNamespace},
		"spec": map[string]any{
			"nodeName":                      node,
			"terminationGracePeriodSeconds": int64(0),
			"containers":                    []any{map[string]any{"name": "app", "image": "nginx:1.27"}},
		},
	})
}

// deletePod removes one pod the way the cluster interface does.
func deletePod(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cluster.Direct(t, "", "v1", "Pod", appsNamespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod %q: %v", name, err)
	}
}

// cordonExternally takes a node out of scheduling without the control plane, so
// the write leaves no KOS record (C-65).
func cordonExternally(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	setSchedulable(t, cluster, name, true)
}

// setSchedulable writes the unschedulable field as an outside writer would.
func setSchedulable(t *testing.T, cluster *harness.Cluster, name string, cordoned bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes := cluster.Direct(t, "", "v1", "Node", "")
	node, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	if err := unstructured.SetNestedField(node.Object, cordoned, "spec", "unschedulable"); err != nil {
		t.Fatalf("set the unschedulable field of %q: %v", name, err)
	}
	if _, err := nodes.Update(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("cordon node %q: %v", name, err)
	}
}

// schedulable reports whether the cluster schedules on one node.
func schedulable(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := cluster.Direct(t, "", "v1", "Node", "").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	cordoned, _, _ := unstructured.NestedBool(node.Object, "spec", "unschedulable")
	return !cordoned
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

// maintenanceRecords reads the attribution records the audit trail keeps for
// one node (C-14, C-65).
func maintenanceRecords(t *testing.T, shell *harness.Shell, node string) []identity.AuditEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ref := controlplane.ObjectRef{Group: api.Group, Kind: maintenanceKind, Name: node}
	entries, err := shell.Accounts.Store.RecentAuditEntriesForObject(ctx, ref.String(),
		[]string{controlplane.VerbMaintain, controlplane.VerbRestore}, nil, 10)
	if err != nil {
		t.Fatalf("read the maintenance records of %q: %v", node, err)
	}
	return entries
}

// recordStart appends the maintenance write a flow records when it cordons a
// node (C-14, C-65).
func recordStart(t *testing.T, shell *harness.Shell, node string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ref := controlplane.ObjectRef{Group: api.Group, Kind: maintenanceKind, Name: node}
	err := shell.Accounts.Store.AppendAudit(ctx, identity.AuditEntry{
		Actor:      ownerName,
		Object:     ref.String(),
		Verb:       controlplane.VerbMaintain,
		Result:     controlplane.ResultAccepted,
		Source:     controlplane.SourceAPI,
		OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("append the maintenance record of %q: %v", node, err)
	}
}

// nodeOrigin reads the maintenance origin the node settings report (C-36).
func nodeOrigin(t *testing.T, shell interface {
	Call(t *testing.T, method, path string, body any, token string) (int, map[string]any)
}, token, node string) string {
	t.Helper()
	for _, entry := range nodeItemValue(t, shell, token, node, kossettings.NodeMaintenanceOrigin) {
		values, _ := entry.(map[string]any)
		if name, _ := values["name"].(string); name == node {
			origin, _ := values["origin"].(string)
			return origin
		}
	}
	t.Fatalf("the node settings carry no origin for %q", node)
	return ""
}

// nodeAffected reads the applications the node settings report as affected
// (C-36).
func nodeAffected(t *testing.T, shell *harness.Shell, token, node string) []string {
	t.Helper()
	for _, entry := range nodeItemValue(t, shell, token, node, kossettings.NodeMaintenanceAffected) {
		values, _ := entry.(map[string]any)
		if name, _ := values["name"].(string); name != node {
			continue
		}
		applications, _ := values["applications"].([]any)
		names := make([]string, 0, len(applications))
		for _, item := range applications {
			name, _ := item.(string)
			names = append(names, name)
		}
		return names
	}
	t.Fatalf("the node settings carry no affected applications for %q", node)
	return nil
}

// nodeItemValue reads one settings page item's resolved value. The maintenance
// facts are page items, not writable settings: C-36 puts them on the settings
// page and the panel reads them there.
func nodeItemValue(t *testing.T, shell interface {
	Call(t *testing.T, method, path string, body any, token string) (int, map[string]any)
}, token, node, item string) []any {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+kossettings.PageSettings, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the settings page: status %d, body %v", status, body)
	}
	items, _ := body["items"].([]any)
	for _, entry := range items {
		values, _ := entry.(map[string]any)
		if id, _ := values["id"].(string); id != item {
			continue
		}
		resolved, _ := values["value"].([]any)
		return resolved
	}
	t.Fatalf("the settings page carries no item %q", item)
	return nil
}

// failureCode reads the stable code of one error response.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}

// quiescedCondition reads the stop condition of the case's application.
func quiescedCondition(t *testing.T, cluster *harness.Cluster) (metav1.Condition, bool) {
	t.Helper()
	conditions, err := harness.ConditionsOf(cluster.Get(t, "v1beta1", "Application", application))
	if err != nil {
		t.Fatalf("read the conditions of %q: %v", application, err)
	}
	for _, condition := range conditions {
		if condition.Type == app.ConditionQuiesced {
			return condition, true
		}
	}
	return metav1.Condition{}, false
}
