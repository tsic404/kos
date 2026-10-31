// Package tc103 verifies C-49's three page projections over one cluster that
// holds an application with a volume, an endpoint and a backup policy: every
// must-display item of the overview, application and storage pages is readable
// with its source type, the lifecycle entries exist, and the items whose source
// has no reader state that instead of inventing a value (AC-51, TC-103).
package tc103

import (
	"context"
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app/kostaskmanager"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/volume/kosstorage"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName     = "owner"
	ownerPassword = "correct-horse-battery"
	application   = "shop"
	volumeName    = "data"
	backupName    = "nightly"
	definition    = "shop"
	providerName  = "local-path"
	deviceNode    = "node-a"
	storageClass  = "local"
)

// TestTC103ProjectionsOverAnAppliedApplication builds one application with a
// bound volume, an entry, a verified provider and a backup policy, then reads
// the three C-49 pages.
func TestTC103ProjectionsOverAnAppliedApplication(t *testing.T) {
	cluster := harness.Start(t)
	now := shellClock()
	createNode(t, cluster, deviceNode)
	cluster.StorageClass(t, storageClass, providerName, true, "Delete", nil)
	cluster.Create(t, &unstructured.Unstructured{Object: definitionOf()})
	cluster.Create(t, &unstructured.Unstructured{Object: providerOf()})
	cluster.Create(t, &unstructured.Unstructured{Object: applicationOf(now)})
	cluster.Create(t, harness.Volume(harness.Namespace, volumeName, map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityNodeLocal),
		"capacity":     "1Gi",
		"storageClass": storageClass,
	}))
	cluster.Create(t, harness.EndpointIn(harness.Namespace, "shop-web", harness.ManagedLabels(), map[string]any{
		"kind": string(v1beta1.EndpointKindHTTPHost), "targetApp": application, "host": "shop.test",
	}))
	cluster.Create(t, &unstructured.Unstructured{Object: backupOf(now)})
	cluster.Create(t, &unstructured.Unstructured{Object: deviceOf(now)})
	writeVolumeStatus(t, cluster, now)
	writeEndpointStatus(t, cluster, now)

	taskmanagerReadings := &kostaskmanager.Readings{Dynamic: cluster.Dynamic}
	storageReadings := &kosstorage.Readings{Dynamic: cluster.Dynamic}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		config.Readings = pages.Router{
			kostaskmanager.Identifier: taskmanagerReadings,
			kosstorage.Identifier:     storageReadings,
		}
	})
	taskmanagerReadings.Now = shell.Accounts.Clock.Now
	storageReadings.Now = shell.Accounts.Clock.Now
	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackageWith(t, "taskmanager", kostaskmanager.Declaration(), artifacts, shell.Retention())
	installation := cluster.InstallPackageWith(t, "storage", kosstorage.Declaration(), artifacts, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPassword)

	overview := readPage(t, shell, token, kostaskmanager.PageOverview)
	assertItems(t, overview, map[string]string{
		kostaskmanager.ItemNodesReady:          extension.PageSourceObject,
		kostaskmanager.ItemAbnormalObjects:     extension.PageSourceComputed,
		kostaskmanager.ItemPendingUpgrades:     extension.PageSourceComputed,
		kostaskmanager.ItemRecentEvents:        extension.PageSourceObject,
		kostaskmanager.ItemMaintenanceNodes:    extension.PageSourceObject,
		kostaskmanager.ItemMaintenanceAffected: extension.PageSourceComputed,
	})
	if overview["readOnly"] != true || harness.String(overview, "readOnlyReason") == "" {
		t.Fatalf("overview readOnly = %v reason = %q, want a stated read-only page",
			overview["readOnly"], harness.String(overview, "readOnlyReason"))
	}

	nodesReady := itemValue(t, overview, kostaskmanager.ItemNodesReady)
	if number(t, nodesReady, "ready") != 1 || number(t, nodesReady, "total") != 1 {
		t.Fatalf("nodes ready = %v, want one ready node of one", nodesReady)
	}
	pending := itemValue(t, overview, kostaskmanager.ItemPendingUpgrades)
	if !containsName(t, pending, "pending", application) {
		t.Fatalf("pending upgrades = %v, want %s behind its definition", pending, application)
	}
	events := itemValue(t, overview, kostaskmanager.ItemRecentEvents)
	if harness.String(events, "window") != controlplane.EventWindow.String() {
		t.Fatalf("event window = %q, want %q", harness.String(events, "window"), controlplane.EventWindow.String())
	}
	maintenance := itemValue(t, overview, kostaskmanager.ItemMaintenanceAffected)
	if nodes, ok := maintenance["nodes"].([]any); ok && len(nodes) != 0 {
		t.Fatalf("maintenance affected = %v, want none while the node is schedulable", maintenance)
	}

	applications := readPage(t, shell, token, kostaskmanager.PageApplications)
	assertItems(t, applications, map[string]string{
		kostaskmanager.ItemInstalled:     extension.PageSourceObject,
		kostaskmanager.ItemInstallable:   extension.PageSourceComputed,
		kostaskmanager.ItemResourceUsage: extension.PageSourceMetric,
		kostaskmanager.ItemStop:          extension.PageSourceObject,
		kostaskmanager.ItemStart:         extension.PageSourceObject,
		kostaskmanager.ItemRestart:       extension.PageSourceObject,
		kostaskmanager.ItemScale:         extension.PageSourceObject,
		kostaskmanager.ItemResourceSpec:  extension.PageSourceObject,
		kostaskmanager.ItemUpgrade:       extension.PageSourceObject,
		kostaskmanager.ItemUninstall:     extension.PageSourceObject,
	})
	assertEntries(t, applications, map[string]string{
		kostaskmanager.ItemStop:         kostaskmanager.ActionStop,
		kostaskmanager.ItemStart:        kostaskmanager.ActionStart,
		kostaskmanager.ItemRestart:      kostaskmanager.ActionRestart,
		kostaskmanager.ItemScale:        kostaskmanager.ActionScale,
		kostaskmanager.ItemResourceSpec: kostaskmanager.ActionResourceSpec,
		kostaskmanager.ItemUpgrade:      kostaskmanager.ActionUpgrade,
		kostaskmanager.ItemUninstall:    kostaskmanager.ActionUninstall,
	})
	installed := itemValue(t, applications, kostaskmanager.ItemInstalled)
	if !containsName(t, installed, "applications", application) {
		t.Fatalf("installed applications = %v, want %s", installed, application)
	}
	installable := itemValue(t, applications, kostaskmanager.ItemInstallable)
	if !definitionInstallable(t, installable, definition) {
		t.Fatalf("installable = %v, want %s installable with its verified capability", installable, definition)
	}
	usage := itemValue(t, applications, kostaskmanager.ItemResourceUsage)
	if usage["available"] != false || harness.String(usage, "reason") == "" {
		t.Fatalf("application usage = %v, want an unavailability with a reason, not a number", usage)
	}

	storage := readPage(t, shell, token, kosstorage.PageStorage)
	assertItems(t, storage, map[string]string{
		kosstorage.ItemDevicesAndPolicies: extension.PageSourceObject,
		kosstorage.ItemVolumes:            extension.PageSourceObject,
		kosstorage.ItemUsage:              extension.PageSourceMetric,
		kosstorage.ItemBackups:            extension.PageSourceObject,
		kosstorage.ItemMigration:          extension.PageSourceObject,
	})
	assertEntries(t, storage, map[string]string{kosstorage.ItemMigration: kosstorage.ActionMigration})
	volumes := itemValue(t, storage, kosstorage.ItemVolumes)
	if !containsName(t, volumes, "volumes", volumeName) {
		t.Fatalf("volumes = %v, want %s", volumes, volumeName)
	}
	policies := itemValue(t, storage, kosstorage.ItemDevicesAndPolicies)
	if devices, ok := policies["devices"].([]any); !ok || len(devices) != 1 {
		t.Fatalf("devices = %v, want the one stored device fact", policies)
	}
	if classes, ok := policies["storageClasses"].([]any); !ok || len(classes) != 1 {
		t.Fatalf("storage classes = %v, want the one stored class", policies)
	}
	backups := itemValue(t, storage, kosstorage.ItemBackups)
	if !backupSucceeded(t, backups, backupName) {
		t.Fatalf("backup policies = %v, want %s with a passing drill", backups, backupName)
	}
	storageUsage := itemValue(t, storage, kosstorage.ItemUsage)
	if storageUsage["available"] != false || harness.String(storageUsage, "reason") == "" {
		t.Fatalf("volume usage = %v, want an unavailability with a reason", storageUsage)
	}
}

// readPage reads one page projection through the API.
func readPage(t *testing.T, shell *harness.Shell, token, id string) map[string]any {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+id, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read page %s = %d %v", id, status, body)
	}
	return body
}

// assertItems checks a page's item set, source types and observation times.
func assertItems(t *testing.T, page map[string]any, want map[string]string) {
	t.Helper()
	items := itemsByID(t, page)
	if len(items) != len(want) {
		t.Fatalf("page %q has %d items, want %d", harness.String(page, "id"), len(items), len(want))
	}
	for id, source := range want {
		item, known := items[id]
		if !known {
			t.Errorf("page %q is missing item %q", harness.String(page, "id"), id)
			continue
		}
		if harness.String(item, "source") != source {
			t.Errorf("item %q source = %q, want %q", id, harness.String(item, "source"), source)
		}
		if harness.String(item, "observedAt") == "" {
			t.Errorf("item %q carries no observation time (C-13)", id)
		}
		if item["stale"] == true {
			t.Errorf("item %q is marked stale on a live read", id)
		}
	}
}

// assertEntries checks the modification entries of the items that carry one.
func assertEntries(t *testing.T, page map[string]any, want map[string]string) {
	t.Helper()
	items := itemsByID(t, page)
	for id, action := range want {
		item, known := items[id]
		if !known {
			t.Errorf("page is missing entry item %q", id)
			continue
		}
		entry, ok := item["entry"].(map[string]any)
		if !ok || harness.String(entry, "method") == "" || harness.String(entry, "path") == "" {
			t.Errorf("item %q entry = %v, want the control-plane write of %s", id, item["entry"], action)
		}
	}
}

// itemValue reads one item off a rendered page.
func itemValue(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	item, known := itemsByID(t, page)[id]
	if !known {
		t.Fatalf("page %q has no item %q", harness.String(page, "id"), id)
	}
	value, ok := item["value"].(map[string]any)
	if !ok {
		t.Fatalf("item %q value = %v, want an object", id, item["value"])
	}
	return value
}

// itemsByID indexes a rendered page's items.
func itemsByID(t *testing.T, page map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := page["items"].([]any)
	if !ok {
		t.Fatalf("page has no items: %v", page)
	}
	items := make(map[string]map[string]any, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object: %v", entry)
		}
		items[harness.String(item, "id")] = item
	}
	return items
}

// containsName checks one list field of a value for an entry naming name.
func containsName(t *testing.T, value map[string]any, field, name string) bool {
	t.Helper()
	raw, ok := value[field].([]any)
	if !ok {
		t.Fatalf("value has no %s list: %v", field, value)
	}
	for _, entry := range raw {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if harness.String(object, "name") == name {
			return true
		}
	}
	return false
}

// definitionInstallable reports whether the installable list marks one
// definition installable.
func definitionInstallable(t *testing.T, value map[string]any, name string) bool {
	t.Helper()
	raw, ok := value["definitions"].([]any)
	if !ok {
		t.Fatalf("installable has no definitions: %v", value)
	}
	for _, entry := range raw {
		object, ok := entry.(map[string]any)
		if !ok || harness.String(object, "name") != name {
			continue
		}
		return object["installable"] == true
	}
	return false
}

// backupSucceeded reports whether one policy's newest backup and drill passed.
func backupSucceeded(t *testing.T, value map[string]any, name string) bool {
	t.Helper()
	raw, ok := value["policies"].([]any)
	if !ok {
		t.Fatalf("backups has no policies: %v", value)
	}
	for _, entry := range raw {
		policy, ok := entry.(map[string]any)
		if !ok || harness.String(policy, "name") != name {
			continue
		}
		backup, hasBackup := policy["lastBackup"].(map[string]any)
		drill, hasDrill := policy["lastDrill"].(map[string]any)
		return hasBackup && backup["succeeded"] == true && hasDrill && drill["succeeded"] == true
	}
	return false
}

// number reads a JSON number as an integer.
func number(t *testing.T, value map[string]any, field string) int {
	t.Helper()
	raw, ok := value[field].(float64)
	if !ok {
		t.Fatalf("field %q = %v, want a number", field, value[field])
	}
	return int(raw)
}

// createNode writes one ready node.
func createNode(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "True", "reason": "KubeletReady",
				"lastHeartbeatTime": nowText(), "lastTransitionTime": nowText(),
			}},
			"nodeInfo":    map[string]any{"kubeletVersion": "v1.37.0", "osImage": "acceptance"},
			"allocatable": map[string]any{"cpu": "4", "memory": "8Gi"},
		},
	})
}

// shellClock is the clock the shell and the acceptance objects share.
func shellClock() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// nowText is the acceptance clock as a timestamp.
func nowText() string { return shellClock().Format(time.RFC3339) }

// definitionOf is the application definition the case installs.
func definitionOf() map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "ApplicationDefinition",
		"metadata": map[string]any{
			"name": definition,
			"labels": map[string]any{
				"managed-by": "acceptance",
			},
		},
		"spec": map[string]any{
			"package":  map[string]any{"version": "1.2.0", "digest": "sha256:offered", "dataFormat": "v1"},
			"requires": []any{map[string]any{"name": "storage.volume", "version": "v1"}},
		},
	}
}

// applicationOf is the installed application: applied on an older package
// digest, so the version comparison reports a pending upgrade.
func applicationOf(now time.Time) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "Application",
		"metadata":   map[string]any{"name": application, "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner":         ownerName,
			"definitionRef": definition,
			"namespace":     harness.Namespace,
			"volumes":       []any{volumeName},
			"replicas":      2,
		},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Reconciled", "message": "acceptance",
				"lastTransitionTime": nowText(),
			}},
			"applied": map[string]any{"definitionDigest": "sha256:applied"},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
				"limits":   map[string]any{"cpu": "500m", "memory": "512Mi"},
			},
		},
	}
}

// providerOf is the verified storage provider the volume resolves to.
func providerOf() map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "Provider",
		"metadata": map[string]any{
			"name": providerName,
			"labels": map[string]any{
				"managed-by": "acceptance", "kos.tsic.top/owner-uid": "00000000-0000-0000-0000-000000000001",
			},
		},
		"spec": map[string]any{
			"componentRef":   "kos-storage",
			"version":        "1.0.0",
			"storageClasses": []any{storageClass},
			"capabilities": []any{map[string]any{
				"name": "storage.volume", "version": "v1", "scope": "node",
				"caseSet": "volumes", "caseSetDigest": "digest-volumes",
			}},
		},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
			"conditions": []any{
				map[string]any{"type": "Registered", "status": "True", "reason": "Registered", "message": "acceptance", "lastTransitionTime": nowText()},
				map[string]any{"type": "Verified", "status": "True", "reason": "Verified", "message": "acceptance", "lastTransitionTime": nowText()},
				map[string]any{"type": "Ready", "status": "True", "reason": "Ready", "message": "acceptance", "lastTransitionTime": nowText()},
				map[string]any{"type": "UpgradeBlocked", "status": "False", "reason": "NoCapabilityWithdrawn", "message": "acceptance", "lastTransitionTime": nowText()},
			},
			"effectiveCapabilities": []any{map[string]any{
				"name": "storage.volume", "version": "v1", "scope": "node",
				"caseSet": "volumes", "caseSetDigest": "digest-volumes",
			}},
			"capabilities": []any{map[string]any{
				"name": "storage.volume", "version": "v1", "source": "KOS", "state": "Verified",
			}},
		},
	}
}

// backupOf is the backup policy with a completed run and drill.
func backupOf(now time.Time) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "BackupPolicy",
		"metadata":   map[string]any{"name": backupName, "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner":       ownerName,
			"selector":    application,
			"schedule":    "0 2 * * *",
			"consistency": "crash-consistent",
			"target":      map[string]any{"path": "/backup", "failureDomain": "disk-b"},
			"retention":   map[string]any{"keep": 3},
			"drill":       map[string]any{"enabled": true},
		},
		"status": map[string]any{
			"phase":  "Scheduled",
			"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Reconciled", "message": "acceptance",
				"lastTransitionTime": nowText(),
			}},
			"lastBackup": map[string]any{
				"scheduledAt": now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
				"completedAt": now.Add(-time.Hour).UTC().Format(time.RFC3339),
				"consistency": "crash-consistent",
				"succeeded":   true,
			},
			"lastDrill": map[string]any{
				"phase":       "Succeeded",
				"completedAt": now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
				"succeeded":   true,
			},
		},
	}
}

// deviceOf is the device fact of the one node.
func deviceOf(now time.Time) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "NodeDevice",
		"metadata": map[string]any{
			"name":   deviceNode,
			"labels": map[string]any{"managed-by": "discovery"},
		},
		"spec": map[string]any{
			"node":       deviceNode,
			"collector":  deviceNode,
			"observedAt": now.UTC().Format(time.RFC3339),
			"devices": []any{map[string]any{
				"name": "nvme0", "serial": "S1", "model": "Acceptance", "kind": "disk",
				"present": true, "recognized": true, "driverReady": true, "published": true,
				"occupancy":  map[string]any{"source": "storage-pool", "reference": "pool-a"},
				"observedAt": now.UTC().Format(time.RFC3339),
			}},
		},
	}
}

// writeVolumeStatus marks the volume bound to the node.
func writeVolumeStatus(t *testing.T, cluster *harness.Cluster, now time.Time) {
	t.Helper()
	status := map[string]any{
		"phase":         "Bound",
		"health":        map[string]any{"state": string(v1beta1.HealthStateHealthy)},
		"boundClaim":    volumeName,
		"storageClass":  storageClass,
		"provider":      map[string]any{"name": providerName, "version": "1.0.0"},
		"reclaimPolicy": "Delete",
		"node":          deviceNode,
		"conditions": []any{map[string]any{
			"type": "Reconciled", "status": "True", "reason": "Reconciled", "message": "acceptance",
			"lastTransitionTime": nowText(),
		}},
	}
	if _, err := cluster.ApplyStatus(context.Background(), v1beta1.GroupVersion.Version, "Volume", harness.Namespace, volumeName, status); err != nil {
		t.Fatalf("write volume status: %v", err)
	}
}

// writeEndpointStatus records a serving certificate that expires inside the
// warning window (C-46).
func writeEndpointStatus(t *testing.T, cluster *harness.Cluster, now time.Time) {
	t.Helper()
	status := map[string]any{
		"phase":  "Ready",
		"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
		"conditions": []any{map[string]any{
			"type": "Reconciled", "status": "True", "reason": "Reconciled", "message": "acceptance",
			"lastTransitionTime": nowText(),
		}},
		"certificate": map[string]any{
			"issuer":       "internal-issuer.kos.tsic.top",
			"ready":        true,
			"notAfter":     now.Add(10 * 24 * time.Hour).UTC().Format(time.RFC3339),
			"expiringSoon": true,
		},
	}
	if _, err := cluster.ApplyStatus(context.Background(), v1beta1.GroupVersion.Version, "Endpoint", harness.Namespace, "shop-web", status); err != nil {
		t.Fatalf("write endpoint status: %v", err)
	}
}
