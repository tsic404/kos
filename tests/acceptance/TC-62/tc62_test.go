// Package tc62 verifies C-36's readable settings domain: a cluster setting and
// a node setting are modified through the control plane and the projection reads
// the new value from the right source, while a setting the API cannot change
// states why and offers no entry (AC-31, TC-62).
package tc62

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/settings/kossettings"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName = "owner"
	ownerPass = "owner-password"
	// applicationNamespace is where the case's application runs and keeps its
	// node-local volume (C-32, C-38).
	applicationNamespace = "apps"
)

// TestTC62ClusterAndNodeSettings modifies one cluster and one node setting and
// reads a fact item the API cannot change.
func TestTC62ClusterAndNodeSettings(t *testing.T) {
	cluster := harness.Start(t)
	createNode(t, cluster, "node-a", "4", "8Gi", false, map[string]any{"node-role.kubernetes.io/worker": ""})
	createNode(t, cluster, "node-b", "2", "4Gi", true, nil)
	cluster.Create(t, &unstructured.Unstructured{Object: systemComponent()})
	cluster.Create(t, &unstructured.Unstructured{Object: provider()})
	cluster.Create(t, &unstructured.Unstructured{Object: nodeDevice()})
	cluster.EnsureNamespace(t, applicationNamespace)
	cluster.CreateIn(t, applicationNamespace, boundVolume())
	cluster.Create(t, applicationOfVolume())

	readings := &kossettings.Readings{Dynamic: cluster.Dynamic}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		// The panel dispatch is the rendering path the browser uses: each item
		// goes to the product named in its reference (C-22).
		config.Readings = pages.Router{kossettings.Identifier: readings}
	})
	readings.Settings = shell.Settings
	readings.Accounts = shell.Accounts.Store
	readings.Audit = shell.Accounts.Store
	installation := cluster.InstallPackageWith(t, "settings", kossettings.Declaration(), extension.MemoryArtifacts{}, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPass)

	// Both pages are in the panel's page list, so the browser can open them;
	// a component that is not active withdraws them with itself (C-22, C-23).
	_, registry := shell.Call(t, http.MethodGet, "/api/v1/pages", nil, token)
	for _, id := range []string{kossettings.PageSettings, kossettings.PageUsers} {
		if !listedPage(t, registry, id) {
			t.Fatalf("page %q is not listed for the installed product: %v", id, registry["items"])
		}
	}

	// The maintenance flow records its start; the node is unschedulable because
	// the case cordoned it as that flow does (C-65).
	recordMaintenance(t, shell, "node-b")
	// Far more unrelated audit traffic than one global window would hold, all
	// written after the maintenance record: the origin read is scoped to the
	// node's own records, so this must not move it (C-36, C-65).
	recordDistractors(t, shell, 600)

	// The cluster setting is declared by the product, is editable, and reports
	// the user as its writer and its own path as the entry (C-23).
	_, body := shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	clusterItem := findItem(t, body, kossettings.ClusterMaintenanceWindow)
	if source := harness.String(clusterItem, "source"); source != "User" {
		t.Fatalf("cluster setting source = %q, want the user writer", source)
	}
	clusterPath := harness.String(clusterItem, "entry", "path")
	if clusterPath != "/api/v1/settings/"+kossettings.ClusterMaintenanceWindow {
		t.Fatalf("cluster setting entry = %q", clusterPath)
	}

	// Modify the cluster setting through the entry it rendered; the projection
	// reads the written value.
	status, body := shell.Call(t, http.MethodPut, clusterPath,
		map[string]any{"value": "Sun 02:00-04:00"}, token)
	if status != http.StatusOK {
		t.Fatalf("modify the cluster setting = %d %v", status, body)
	}
	_, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if value := harness.String(findItem(t, body, kossettings.ClusterMaintenanceWindow), "value"); value != "Sun 02:00-04:00" {
		t.Fatalf("cluster setting value = %q, want the written value", value)
	}

	// The node setting is written against one node and read back for that node.
	// Its entry names the node it still needs, so the request is built from the
	// rendered entry rather than spelled out by the case (C-23).
	_, body = shell.Call(t, http.MethodGet, "/api/v1/settings?node=node-a", nil, token)
	nodeItem := findItem(t, body, kossettings.NodeReservedResources)
	if source := harness.String(nodeItem, "source"); source != "User" {
		t.Fatalf("node setting source = %q, want the user writer", source)
	}
	nodePath := harness.String(nodeItem, "entry", "path")
	if nodePath != "/api/v1/settings/"+kossettings.NodeReservedResources+"?node={node}" {
		t.Fatalf("node setting entry = %q, want the node target", nodePath)
	}
	status, body = shell.Call(t, http.MethodPut, strings.ReplaceAll(nodePath, "{node}", "node-a"),
		map[string]any{"value": "cpu=500m,memory=1Gi"}, token)
	if status != http.StatusOK {
		t.Fatalf("modify the node setting = %d %v", status, body)
	}
	_, body = shell.Call(t, http.MethodGet, "/api/v1/settings?node=node-a", nil, token)
	if value := harness.String(findItem(t, body, kossettings.NodeReservedResources), "value"); value != "cpu=500m,memory=1Gi" {
		t.Fatalf("node setting value = %q, want the written value", value)
	}

	// The panel reads the same domain: the modified values, their sources, and
	// the facts C-36 covers.
	status, body = shell.Call(t, http.MethodGet, "/api/v1/pages/"+kossettings.PageSettings, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the settings page = %d %v", status, body)
	}
	page := itemsByID(t, body)

	// The modified values are what the projection shows, from the database
	// source the setting value lives in, and the entry is still offered.
	assertItem(t, page, kossettings.ClusterMaintenanceWindow, "database", "Sun 02:00-04:00")
	if entry := harness.String(page[kossettings.ClusterMaintenanceWindow], "entry", "path"); entry != "/api/v1/settings/"+kossettings.ClusterMaintenanceWindow {
		t.Fatalf("cluster setting page entry = %q", entry)
	}
	reservations, ok := page[kossettings.NodeReservedResources]["value"].(map[string]any)
	if !ok || reservations["node-a"] != "cpu=500m,memory=1Gi" {
		t.Fatalf("node reservation value = %v, want the value written for node-a", page[kossettings.NodeReservedResources]["value"])
	}
	if entry := harness.String(page[kossettings.NodeReservedResources], "entry", "path"); entry != "/api/v1/settings/"+kossettings.NodeReservedResources+"?node={node}" {
		t.Fatalf("node setting page entry = %q, want the node target", entry)
	}

	// Cluster facts come from the objects and the computed rules C-36 names.
	versions, ok := page[kossettings.ClusterKubernetesVersion]["value"].(map[string]any)
	if !ok || !contains(versions["versions"], "v1.37.0") {
		t.Fatalf("kubernetes versions = %v, want the node's version", page[kossettings.ClusterKubernetesVersion]["value"])
	}
	if source := harness.String(page[kossettings.ClusterKubernetesVersion], "source"); source != "object" {
		t.Fatalf("kubernetes version source = %q, want object", source)
	}
	// The resource item's own states are judged by TC-135, which drives the
	// metric source; this case keeps to the settings domain and the other
	// facts C-36 covers.
	health, _ := page[kossettings.ClusterHealth]["value"].(map[string]any)
	if health["health"] != "Healthy" {
		t.Fatalf("cluster health = %v, want Healthy", page[kossettings.ClusterHealth]["value"])
	}
	if source := harness.String(page[kossettings.ClusterHealth], "source"); source != "computed" {
		t.Fatalf("cluster health source = %q, want computed", source)
	}

	// Node facts are keyed by node, so the panel reads one node at a time.
	assertNodeValue(t, page, kossettings.NodeStatus, "node-b", "schedulable", false)
	assertNodeValue(t, page, kossettings.NodeMaintenanceState, "node-b", "schedulable", false)
	assertNodeValue(t, page, kossettings.NodeMaintenanceOrigin, "node-b", "origin", kossettings.MaintenancePlanned)
	assertNodeValue(t, page, kossettings.NodeMaintenanceOrigin, "node-a", "origin", kossettings.MaintenanceNone)
	if applications := nodeEntry(t, page, kossettings.NodeMaintenanceAffected, "node-b")["applications"]; !contains(applications, harness.Namespace+"/shop") {
		t.Fatalf("affected applications = %v, want the application bound to node-b", applications)
	}
	if devices := nodeEntry(t, page, kossettings.NodeDevices, "node-a"); devices["devices"] != float64(2) || devices["occupied"] != float64(1) {
		t.Fatalf("node devices = %v, want two facts with one occupied", devices)
	}

	// A node setting the runtime kernel exposes neither for reading nor for
	// writing has a reason and no entry (C-36).
	hostConfig := page[kossettings.NodeHostConfig]
	if entry := harness.Nested(hostConfig, "entry"); entry != nil {
		t.Fatalf("host config item = %v, want no modification entry", hostConfig)
	}
	if reason := harness.String(hostConfig, "reason"); reason != kossettings.HostConfigReason {
		t.Fatalf("host config reason = %q, want the declared reason", reason)
	}

	// The user page reads the two sources C-50 fixes: members and roles from
	// the control-plane database, visibility and ownership from the objects.
	status, body = shell.Call(t, http.MethodGet, "/api/v1/pages/"+kossettings.PageUsers, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the user page = %d %v", status, body)
	}
	if readOnly, _ := body["readOnly"].(bool); !readOnly {
		t.Fatalf("user page = %v, want the read-only declaration", body)
	}
	// A read-only page states why and offers no modification entry (C-50,
	// C-53).
	if reason := harness.String(body, "readOnlyReason"); reason == "" {
		t.Fatalf("user page = %v, want the reason it is read-only", body)
	}
	if actions, ok := body["actions"].([]any); ok && len(actions) > 0 {
		t.Fatalf("user page actions = %v, want none on a read-only page", actions)
	}
	users := itemsByID(t, body)
	for id, item := range users {
		if entry := harness.Nested(item, "entry"); entry != nil {
			t.Fatalf("user page item %q = %v, want no modification entry", id, item)
		}
	}
	members := users[kossettings.UserMembers]
	if source := harness.String(members, "source"); source != "database" {
		t.Fatalf("members source = %q, want database", source)
	}
	if !hasField(members["value"], "username", ownerName, "role", "Owner") {
		t.Fatalf("members = %v, want the owner account and its role", members["value"])
	}
	visibility := users[kossettings.ApplicationVisibility]
	if source := harness.String(visibility, "source"); source != "object" {
		t.Fatalf("application visibility source = %q, want object", source)
	}
	if !hasField(visibility["value"], "name", "shop", "owner", ownerName) {
		t.Fatalf("application visibility = %v, want the application and its owner", visibility["value"])
	}
	owners := users[kossettings.ApplicationAndVolumeOwners]
	if !hasEntry(owners["value"], harness.Namespace, "shop", "Application", ownerName) ||
		!hasEntry(owners["value"], applicationNamespace, "data", "Volume", ownerName) {
		t.Fatalf("owners = %v, want the application and the volume with their namespace and owner", owners["value"])
	}

	// Every item carries an observation time, so the shell can apply C-13.
	for _, items := range []map[string]map[string]any{page, users} {
		for id, item := range items {
			if observed := harness.String(item, "observedAt"); observed == "" {
				t.Fatalf("item %q carries no observation time: %v", id, item)
			}
		}
	}
}

// createNode writes one node with the status the settings domain reads.
func createNode(t *testing.T, cluster *harness.Cluster, name, cpu, memory string, unschedulable bool, labels map[string]any) {
	t.Helper()
	metadata := map[string]any{"name": name}
	if labels != nil {
		metadata["labels"] = labels
	}
	spec := map[string]any{}
	if unschedulable {
		spec["unschedulable"] = true
	}
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   metadata,
		"spec":       spec,
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
		"allocatable": map[string]any{"cpu": cpu, "memory": memory},
		"nodeInfo":    map[string]any{"kubeletVersion": "v1.37.0"},
	}
	if _, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the status of node %q: %v", name, err)
	}
}

// systemComponent is one healthy system component.
func systemComponent() map[string]any {
	return map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "SystemComponent",
		"metadata":   map[string]any{"name": "kos-shell"},
		"spec":       map[string]any{},
		"status":     map[string]any{"phase": "Ready", "health": map[string]any{"state": "Healthy"}},
	}
}

// provider is a storage provider that declares one node-scoped capability.
func provider() map[string]any {
	return map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Provider",
		"metadata":   map[string]any{"name": "standard.example"},
		"spec": map[string]any{
			"version":        "1.2.0",
			"storageClasses": []any{"standard"},
			"capabilities": []any{map[string]any{
				"name": "storage.block", "version": "1", "scope": "node",
				"caseSet": "storage-cases", "caseSetDigest": "sha256:case-set",
			}},
		},
		"status": map[string]any{"health": map[string]any{"state": "Healthy"}},
	}
}

// nodeDevice reports two devices on node-a, one of them occupied.
func nodeDevice() map[string]any {
	return map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "NodeDevice",
		"metadata":   map[string]any{"name": "node-a"},
		"spec": map[string]any{
			"node":      "node-a",
			"collector": "kos-devicemanager",
			"devices": []any{
				map[string]any{
					"name": "gpu0", "serial": "gpu-0", "model": "example", "kind": "gpu",
					"present": true, "recognized": true, "driverReady": true, "published": true,
					"occupancy": map[string]any{"source": "kos-storage"},
				},
				map[string]any{"name": "nic0", "serial": "nic-0", "model": "example", "kind": "net", "present": true},
			},
		},
	}
}

// boundVolume is a node-local volume whose data sits on node-b. It lives in the
// application's target namespace, which is where the application's own volume
// references resolve (C-32, C-38).
func boundVolume() *unstructured.Unstructured {
	object := harness.Volume(applicationNamespace, "data", map[string]any{
		"owner":        ownerName,
		"mobility":     "node-local",
		"capacity":     "1Gi",
		"storageClass": "standard",
	})
	object.Object["status"] = map[string]any{"node": "node-b", "provider": map[string]any{"name": "standard.example"}}
	return object
}

// applicationOfVolume is the application whose node-local data is on node-b.
func applicationOfVolume() *unstructured.Unstructured {
	body := harness.Manifest("Application", harness.Namespace, "shop", nil, nil)
	body["spec"] = map[string]any{
		"owner":         ownerName,
		"definitionRef": "shop",
		"namespace":     applicationNamespace,
		"volumes":       []any{"data"},
	}
	return &unstructured.Unstructured{Object: body}
}

// recordMaintenance appends the audit record the maintenance flow writes when it
// cordons a node (C-65).
func recordMaintenance(t *testing.T, shell *harness.Shell, node string) {
	t.Helper()
	err := shell.Accounts.Trail.Store.AppendAudit(context.Background(), identity.AuditEntry{
		Actor:      "kos-shell",
		Object:     api.Group + "/Node/" + node,
		Verb:       controlplane.VerbMaintain,
		Result:     controlplane.ResultAccepted,
		Source:     controlplane.SourceAPI,
		OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("append the maintenance record: %v", err)
	}
}

// recordDistractors appends unrelated audit records after the maintenance
// record: a reader judging the node must not depend on them staying out of the
// way (C-14, C-65).
func recordDistractors(t *testing.T, shell *harness.Shell, count int) {
	t.Helper()
	moment := time.Now().UTC()
	for index := range count {
		err := shell.Accounts.Trail.Store.AppendAudit(context.Background(), identity.AuditEntry{
			Actor:      "kos-shell",
			Object:     api.Group + "/Setting/distractor-" + strconv.Itoa(index),
			Verb:       "set",
			Result:     controlplane.ResultAccepted,
			Source:     controlplane.SourceAPI,
			OccurredAt: moment,
		})
		if err != nil {
			t.Fatalf("append distractor record %d: %v", index, err)
		}
	}
}

// listedPage reports whether the page registry lists one page identity.
func listedPage(t *testing.T, body map[string]any, id string) bool {
	t.Helper()
	items, _ := body["items"].([]any)
	for _, raw := range items {
		page, _ := raw.(map[string]any)
		if harness.String(page, "id") == id {
			return true
		}
	}
	return false
}

// findItem returns one item of the settings response.
func findItem(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	items, _ := body["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if harness.String(item, "id") == id {
			return item
		}
	}
	t.Fatalf("setting %q is absent from %v", id, body["items"])
	return nil
}

// itemsByID indexes one rendered page by item identity.
func itemsByID(t *testing.T, body map[string]any) map[string]map[string]any {
	t.Helper()
	items, _ := body["items"].([]any)
	index := make(map[string]map[string]any, len(items))
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		index[harness.String(item, "id")] = item
	}
	return index
}

// assertItem checks one rendered item's source and value.
func assertItem(t *testing.T, page map[string]map[string]any, id, source, value string) {
	t.Helper()
	item, ok := page[id]
	if !ok {
		t.Fatalf("item %q is absent from the page", id)
	}
	if got := harness.String(item, "source"); got != source {
		t.Fatalf("item %q source = %q, want %q", id, got, source)
	}
	if got, _ := item["value"].(string); got != value {
		t.Fatalf("item %q value = %v, want %q", id, item["value"], value)
	}
}

// nodeEntry returns the per-node entry of one page item.
func nodeEntry(t *testing.T, page map[string]map[string]any, id, node string) map[string]any {
	t.Helper()
	entries, _ := page[id]["value"].([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["name"] == node {
			return entry
		}
	}
	t.Fatalf("item %q carries no entry for %q: %v", id, node, page[id]["value"])
	return nil
}

// assertNodeValue checks one field of a per-node page entry.
func assertNodeValue(t *testing.T, page map[string]map[string]any, id, node, field string, want any) {
	t.Helper()
	if got := nodeEntry(t, page, id, node)[field]; got != want {
		t.Fatalf("item %q field %q of %q = %v, want %v", id, field, node, got, want)
	}
}

// hasField reports whether one entry of the JSON list carries an owner and a
// value under the two fields.
func hasField(list any, first, firstValue, second, secondValue string) bool {
	entries, _ := list.([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry[first] == firstValue && entry[second] == secondValue {
			return true
		}
	}
	return false
}

// hasEntry reports whether the ownership list names one object with its
// namespace, kind and owner.
func hasEntry(list any, namespace, name, kind, owner string) bool {
	entries, _ := list.([]any)
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if entry["namespace"] == namespace && entry["name"] == name && entry["kind"] == kind && entry["owner"] == owner {
			return true
		}
	}
	return false
}

// contains reports whether the JSON list carries value.
func contains(list any, value string) bool {
	entries, _ := list.([]any)
	for _, entry := range entries {
		if entry == value {
			return true
		}
	}
	return false
}
