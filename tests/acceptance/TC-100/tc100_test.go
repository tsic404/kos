// Package tc100 verifies C-49, C-50 and C-51 over one cluster that covers the
// panel: every page the six products of this issue declare renders its
// must-display items with their source types, a non-read-only page carries its
// modification entries, and a read-only page states why and offers none
// (AC-51, TC-100).
package tc100

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app/kostaskmanager"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/device/kosdevicemanager"
	"github.com/tsic404/kos/pkg/diagnostics/kosdiagnostics"
	"github.com/tsic404/kos/pkg/endpoint/kosnetwork"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/extension/kosupdater"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/volume/kosstorage"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName        = "owner"
	ownerPassword    = "correct-horse-battery"
	application      = "shop"
	brokenApp        = "broken"
	volumeName       = "data"
	backupName       = "nightly"
	definition       = "shop"
	gpuDefinition    = "gpu-lab"
	policyDefinition = "policy-lab"
	providerName     = "local-path"
	blockedName      = "gpu-provider"
	storageClass     = "local"
	readyNode        = "node-a"
	maintained       = "node-b"
	host             = "shop.test"
	l4Port           = 8080
)

// TestTC100EveryPageRendersItsDeclaredItems builds the panel's world and reads
// every page the six products declare.
func TestTC100EveryPageRendersItsDeclaredItems(t *testing.T) {
	cluster := harness.Start(t)
	now := shellClock()
	createNode(t, cluster, readyNode, true, false)
	createNode(t, cluster, maintained, false, true)
	cluster.StorageClass(t, storageClass, providerName, true, "Delete", nil)
	cluster.Create(t, &unstructured.Unstructured{Object: definitionOf(definition, []any{
		map[string]any{"name": "storage.volume", "version": "v1"},
	})})
	cluster.Create(t, &unstructured.Unstructured{Object: definitionOf(gpuDefinition, []any{
		map[string]any{"name": "compute.gpu", "version": "v1",
			"guarantees": []any{map[string]any{"key": "scheduling", "bool": true}}},
	})})
	cluster.Create(t, &unstructured.Unstructured{Object: definitionOf(policyDefinition, []any{
		map[string]any{"name": "network.policy", "version": "v1"},
	})})
	cluster.Create(t, &unstructured.Unstructured{Object: providerOf()})
	cluster.Create(t, &unstructured.Unstructured{Object: blockedProviderOf()})
	cluster.Create(t, &unstructured.Unstructured{Object: applicationOf(application, now, map[string]any{
		"phase":   "Ready",
		"health":  map[string]any{"state": string(v1beta1.HealthStateHealthy)},
		"applied": map[string]any{"definitionDigest": "sha256:applied", "dataFormat": "v1"},
		"upgrade": map[string]any{
			"target": "sha256:offered",
			"irreversible": []any{map[string]any{
				"kind": "volumeDelete", "object": map[string]any{"kind": "Volume", "namespace": harness.Namespace, "name": volumeName},
				"detail": "升级会删除卷",
			}},
		},
	})})
	cluster.Create(t, &unstructured.Unstructured{Object: applicationOf(brokenApp, now, map[string]any{
		"phase":  "Failed",
		"health": map[string]any{"state": string(v1beta1.HealthStateUnavailable)},
		"conditions": []any{
			map[string]any{"type": "Reconciled", "status": "False", "reason": "Failed", "message": "调和失败", "lastTransitionTime": nowText()},
			map[string]any{"type": "Failed", "status": "True", "reason": "BackendFailed", "message": "后端不可用", "lastTransitionTime": nowText()},
		},
		"uninstall": map[string]any{
			"blocked":      []any{map[string]any{"kind": "Volume", "namespace": harness.Namespace, "name": volumeName}},
			"blockedSince": now.Add(-20 * time.Minute).UTC().Format(time.RFC3339),
			"stalled":      true,
		},
	})})
	cluster.Create(t, harness.Volume(harness.Namespace, volumeName, map[string]any{
		"mobility": string(v1beta1.VolumeMobilityNodeLocal), "capacity": "1Gi", "storageClass": storageClass,
	}))
	writeVolumeStatus(t, cluster, now)
	cluster.Create(t, harness.EndpointIn(harness.Namespace, "shop-web", harness.ManagedLabels(), map[string]any{
		"kind": string(v1beta1.EndpointKindHTTPHost), "targetApp": application, "host": host,
	}))
	cluster.Create(t, harness.EndpointIn(harness.Namespace, "shop-l4", harness.ManagedLabels(), map[string]any{
		"kind": string(v1beta1.EndpointKindTCP), "targetApp": application, "port": l4Port,
	}))
	writeEndpointStatus(t, cluster, now)
	cluster.Create(t, &unstructured.Unstructured{Object: backupOf(now)})
	cluster.Create(t, &unstructured.Unstructured{Object: restoreJobOf(now)})
	cluster.Create(t, &unstructured.Unstructured{Object: deviceOf(readyNode, readyNode, now)})
	cluster.Create(t, &unstructured.Unstructured{Object: deviceOf("ghost", "elsewhere", now)})
	cluster.Create(t, &unstructured.Unstructured{Object: deviceOf("old", "old", now.Add(-25*time.Hour))})
	createEvent(t, cluster, "shop-degraded", now.Add(-5*time.Minute))
	createEvent(t, cluster, "shop-old", now.Add(-2*time.Hour))

	taskmanagerReadings := &kostaskmanager.Readings{Dynamic: cluster.Dynamic}
	storageReadings := &kosstorage.Readings{Dynamic: cluster.Dynamic}
	networkReadings := &kosnetwork.Readings{Dynamic: cluster.Dynamic, Resolver: fixedResolver{}}
	deviceReadings := &kosdevicemanager.Readings{Dynamic: cluster.Dynamic}
	updaterReadings := &kosupdater.Readings{Dynamic: cluster.Dynamic}
	diagnosticsReadings := &kosdiagnostics.Readings{Dynamic: cluster.Dynamic}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		config.Readings = pages.Router{
			kostaskmanager.Identifier:   taskmanagerReadings,
			kosstorage.Identifier:       storageReadings,
			kosnetwork.Identifier:       networkReadings,
			kosdevicemanager.Identifier: deviceReadings,
			kosupdater.Identifier:       updaterReadings,
			kosdiagnostics.Identifier:   diagnosticsReadings,
		}
	})
	clock := shell.Accounts.Clock.Now
	taskmanagerReadings.Now = clock
	storageReadings.Now = clock
	networkReadings.Now = clock
	deviceReadings.Now = clock
	updaterReadings.Now = clock
	diagnosticsReadings.Now = clock

	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackageWith(t, "taskmanager", kostaskmanager.Declaration(), artifacts, shell.Retention())
	cluster.InstallPackageWith(t, "storage", kosstorage.Declaration(), artifacts, shell.Retention())
	cluster.InstallPackageWith(t, "network", kosnetwork.Declaration(), artifacts, shell.Retention())
	cluster.InstallPackageWith(t, "devicemanager", kosdevicemanager.Declaration(), artifacts, shell.Retention())
	cluster.InstallPackageWith(t, "updater", kosupdater.Declaration(), artifacts, shell.Retention())
	installation := cluster.InstallPackageWith(t, "diagnostics", kosdiagnostics.Declaration(), artifacts, shell.Retention())
	shell.Use(installation)
	cluster.Create(t, &unstructured.Unstructured{Object: componentOf()})
	token := shell.SignInOwner(t, ownerName, ownerPassword)

	pages := readPages(t, shell, token)
	for _, id := range []string{
		kostaskmanager.PageOverview, kostaskmanager.PageApplications, kosstorage.PageStorage,
		kosnetwork.PageNetwork, kosdevicemanager.PageDevices, kosupdater.PageComponents,
		kosupdater.PageUpdates, kosdiagnostics.PageDiagnostics,
	} {
		if !pages[id] {
			t.Fatalf("page %q is not listed; the six products must render their pages", id)
		}
	}

	assertOverview(t, shell, token)
	assertApplications(t, shell, token)
	assertStorage(t, shell, token)
	assertNetwork(t, shell, token)
	assertDevices(t, shell, token)
	assertComponents(t, shell, token)
	assertUpdates(t, shell, token)
	assertDiagnostics(t, shell, token)
}

// assertOverview checks C-49's overview items.
func assertOverview(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kostaskmanager.PageOverview)
	assertItems(t, page, map[string]string{
		kostaskmanager.ItemNodesReady:          extension.PageSourceObject,
		kostaskmanager.ItemAbnormalObjects:     extension.PageSourceComputed,
		kostaskmanager.ItemPendingUpgrades:     extension.PageSourceComputed,
		kostaskmanager.ItemRecentEvents:        extension.PageSourceObject,
		kostaskmanager.ItemMaintenanceNodes:    extension.PageSourceObject,
		kostaskmanager.ItemMaintenanceAffected: extension.PageSourceComputed,
	})
	assertReadOnly(t, page)
	nodes := valueOf(t, page, kostaskmanager.ItemNodesReady)
	if number(t, nodes, "ready") != 1 || number(t, nodes, "total") != 2 {
		t.Fatalf("nodes ready = %v, want one ready node of two", nodes)
	}
	maintenance := valueOf(t, page, kostaskmanager.ItemMaintenanceNodes)
	if !listHas(t, maintenance, "nodes", maintained) {
		t.Fatalf("maintenance nodes = %v, want %s", maintenance, maintained)
	}
	affected := valueOf(t, page, kostaskmanager.ItemMaintenanceAffected)
	if !affectedByNode(t, affected, maintained, application) {
		t.Fatalf("maintenance affected = %v, want %s on %s", affected, application, maintained)
	}
	pending := valueOf(t, page, kostaskmanager.ItemPendingUpgrades)
	if !listHas(t, pending, "pending", application) || !listHas(t, pending, "pending", brokenApp) {
		t.Fatalf("pending upgrades = %v, want both applications behind the definition", pending)
	}
	events := valueOf(t, page, kostaskmanager.ItemRecentEvents)
	if harness.String(events, "window") != controlplane.EventWindow.String() {
		t.Fatalf("event window = %q, want %q", harness.String(events, "window"), controlplane.EventWindow.String())
	}
	if count(t, events, "events") != 1 {
		t.Fatalf("events = %v, want only the event inside the one-hour window", events)
	}
	abnormal := valueOf(t, page, kostaskmanager.ItemAbnormalObjects)
	if !listHas(t, abnormal, "objects", brokenApp) || !listHas(t, abnormal, "objects", "restore-1") {
		t.Fatalf("abnormal objects = %v, want the failed application and restore job", abnormal)
	}
}

// assertApplications checks C-49's and C-66's application page.
func assertApplications(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kostaskmanager.PageApplications)
	assertItems(t, page, map[string]string{
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
	if len(actionsOf(t, page)) != 7 {
		t.Fatalf("actions = %v, want the seven lifecycle entries", actionsOf(t, page))
	}
	for id, action := range map[string]string{
		kostaskmanager.ItemStop:         kostaskmanager.ActionStop,
		kostaskmanager.ItemStart:        kostaskmanager.ActionStart,
		kostaskmanager.ItemRestart:      kostaskmanager.ActionRestart,
		kostaskmanager.ItemScale:        kostaskmanager.ActionScale,
		kostaskmanager.ItemResourceSpec: kostaskmanager.ActionResourceSpec,
		kostaskmanager.ItemUpgrade:      kostaskmanager.ActionUpgrade,
		kostaskmanager.ItemUninstall:    kostaskmanager.ActionUninstall,
	} {
		assertEntry(t, page, id, action)
	}
	installed := valueOf(t, page, kostaskmanager.ItemInstalled)
	if !listHas(t, installed, "applications", application) {
		t.Fatalf("installed = %v, want %s", installed, application)
	}
	installable := valueOf(t, page, kostaskmanager.ItemInstallable)
	if !installableDefinition(t, installable, definition) {
		t.Fatalf("installable = %v, want %s installable", installable, definition)
	}
	if !installableDefinition(t, installable, gpuDefinition) {
		t.Fatalf("installable = %v, want %s installable with an unsatisfied guarantee", installable, gpuDefinition)
	}
	if installableDefinition(t, installable, policyDefinition) {
		t.Fatalf("installable = %v, want %s blocked by its missing capability", installable, policyDefinition)
	}
	usage := valueOf(t, page, kostaskmanager.ItemResourceUsage)
	if usage["available"] != false || harness.String(usage, "reason") == "" {
		t.Fatalf("usage = %v, want an unavailability with a reason", usage)
	}
}

// assertStorage checks C-49's storage page.
func assertStorage(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kosstorage.PageStorage)
	assertItems(t, page, map[string]string{
		kosstorage.ItemDevicesAndPolicies: extension.PageSourceObject,
		kosstorage.ItemVolumes:            extension.PageSourceObject,
		kosstorage.ItemUsage:              extension.PageSourceMetric,
		kosstorage.ItemBackups:            extension.PageSourceObject,
		kosstorage.ItemMigration:          extension.PageSourceObject,
	})
	assertEntry(t, page, kosstorage.ItemMigration, kosstorage.ActionMigration)
	if count(t, valueOf(t, page, kosstorage.ItemVolumes), "volumes") != 1 {
		t.Fatalf("volumes = %v, want the one stored volume", valueOf(t, page, kosstorage.ItemVolumes))
	}
	if count(t, valueOf(t, page, kosstorage.ItemDevicesAndPolicies), "devices") != 3 {
		t.Fatalf("devices = %v, want the three stored facts", valueOf(t, page, kosstorage.ItemDevicesAndPolicies))
	}
	if count(t, valueOf(t, page, kosstorage.ItemBackups), "policies") != 1 {
		t.Fatalf("backups = %v, want the one policy", valueOf(t, page, kosstorage.ItemBackups))
	}
}

// assertNetwork checks C-50's network page.
func assertNetwork(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kosnetwork.PageNetwork)
	assertItems(t, page, map[string]string{
		kosnetwork.ItemEndpoints:    extension.PageSourceObject,
		kosnetwork.ItemLedger:       extension.PageSourceObject,
		kosnetwork.ItemResolution:   extension.PageSourceProbe,
		kosnetwork.ItemCertificates: extension.PageSourceComputed,
	})
	assertReadOnly(t, page)
	ledger := valueOf(t, page, kosnetwork.ItemLedger)
	projection, ok := ledger["projection"].([]any)
	if !ok || len(projection) != 2 {
		t.Fatalf("ledger = %v, want one row per endpoint", ledger)
	}
	if !projectionHasKey(t, projection, "host", host) || !projectionHasKey(t, projection, "tcp", "8080") {
		t.Fatalf("ledger = %v, want the host and TCP keys", ledger)
	}
	resolution := valueOf(t, page, kosnetwork.ItemResolution)
	if !resolutionHasAddress(t, resolution, host, "10.0.0.1") {
		t.Fatalf("resolution = %v, want the probed address of %s", resolution, host)
	}
	alerts := valueOf(t, page, kosnetwork.ItemCertificates)
	if !listHas(t, alerts, "alerts", "shop-web") {
		t.Fatalf("certificate alerts = %v, want the expiring endpoint", alerts)
	}
}

// assertDevices checks C-50's read-only device page.
func assertDevices(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kosdevicemanager.PageDevices)
	assertItems(t, page, map[string]string{
		kosdevicemanager.ItemHardware: extension.PageSourceObject,
		kosdevicemanager.ItemHealth:   extension.PageSourceComputed,
		kosdevicemanager.ItemMarkers:  extension.PageSourceObject,
	})
	assertReadOnly(t, page)
	hardware := valueOf(t, page, kosdevicemanager.ItemHardware)
	if count(t, hardware, "devices") != 3 {
		t.Fatalf("hardware = %v, want the three device facts", hardware)
	}
	health := valueOf(t, page, kosdevicemanager.ItemHealth)
	if !alertFor(t, health, "ghost", "驱动未就绪") {
		t.Fatalf("health = %v, want the ghost device's driver layer listed as missing", health)
	}
	markers := valueOf(t, page, kosdevicemanager.ItemMarkers)
	if count(t, markers, "facts") != 3 {
		t.Fatalf("markers = %v, want one row per device fact", markers)
	}
	if !markerInflated(t, markers, "ghost") || !markerStale(t, markers, "old") {
		t.Fatalf("markers = %v, want ghost invalid and old stale", markers)
	}
}

// assertComponents checks C-51's component page.
func assertComponents(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kosupdater.PageComponents)
	assertItems(t, page, map[string]string{
		kosupdater.ItemCapabilities:     extension.PageSourceObject,
		kosupdater.ItemGaps:             extension.PageSourceComputed,
		kosupdater.ItemInstallable:      extension.PageSourceComputed,
		kosupdater.ItemBlockedProviders: extension.PageSourceObject,
		kosupdater.ItemExtensionState:   extension.PageSourceObject,
	})
	assertReadOnly(t, page)
	capabilities := valueOf(t, page, kosupdater.ItemCapabilities)
	if !capabilityVerified(t, capabilities, "storage.volume") {
		t.Fatalf("capabilities = %v, want the verified storage capability", capabilities)
	}
	gaps := valueOf(t, page, kosupdater.ItemGaps)
	if !definitionHasGap(t, gaps, gpuDefinition) || !definitionHasGap(t, gaps, policyDefinition) {
		t.Fatalf("gaps = %v, want the unsatisfied GPU guarantee and the missing policy capability", gaps)
	}
	blocked := valueOf(t, page, kosupdater.ItemBlockedProviders)
	if !listHas(t, blocked, "providers", blockedName) {
		t.Fatalf("blocked providers = %v, want %s", blocked, blockedName)
	}
	extensionState := valueOf(t, page, kosupdater.ItemExtensionState)
	if !componentMissingDependency(t, extensionState, "kos-network") {
		t.Fatalf("extension state = %v, want kos-network listing its missing dependency", extensionState)
	}
}

// assertUpdates checks C-51's update page.
func assertUpdates(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kosupdater.PageUpdates)
	assertItems(t, page, map[string]string{
		kosupdater.ItemVersions:            extension.PageSourceComputed,
		kosupdater.ItemApplicationUpgrades: extension.PageSourceObject,
		kosupdater.ItemIrreversible:        extension.PageSourceComputed,
		kosupdater.ItemCompatibility:       extension.PageSourceComputed,
	})
	assertReadOnly(t, page)
	versions := valueOf(t, page, kosupdater.ItemVersions)
	if harness.String(versions, "kos", "storageVersion") != "v1beta1" {
		t.Fatalf("versions = %v, want the storage version", versions)
	}
	if !nodeVersioned(t, versions, readyNode) {
		t.Fatalf("versions = %v, want the node's kubelet version", versions)
	}
	upgrades := valueOf(t, page, kosupdater.ItemApplicationUpgrades)
	if !listHas(t, upgrades, "applications", application) {
		t.Fatalf("application upgrades = %v, want %s", upgrades, application)
	}
	irreversible := valueOf(t, page, kosupdater.ItemIrreversible)
	if !listHas(t, irreversible, "warnings", application) {
		t.Fatalf("irreversible = %v, want %s's refused change", irreversible, application)
	}
	compatibility := valueOf(t, page, kosupdater.ItemCompatibility)
	if !componentCompatible(t, compatibility, "kos-network") {
		t.Fatalf("compatibility = %v, want kos-network's framework condition", compatibility)
	}
}

// assertDiagnostics checks C-51's diagnostics page and its export entry.
func assertDiagnostics(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, kosdiagnostics.PageDiagnostics)
	assertItems(t, page, map[string]string{
		kosdiagnostics.ItemDegradation: extension.PageSourceComputed,
		kosdiagnostics.ItemBlocked:     extension.PageSourceObject,
		kosdiagnostics.ItemEvidence:    extension.PageSourceObject,
		kosdiagnostics.ItemExport:      extension.PageSourceComputed,
	})
	if page["readOnly"] == true {
		t.Fatalf("diagnostics page is read-only; C-51 requires the export entry")
	}
	assertEntry(t, page, kosdiagnostics.ItemExport, kosdiagnostics.ActionExport)
	entry := entryOf(t, page, kosdiagnostics.ItemExport)
	if harness.String(entry, "method") != http.MethodPost || harness.String(entry, "path") != "/api/v1/diagnostics/bundles" {
		t.Fatalf("export entry = %v, want POST /api/v1/diagnostics/bundles", entry)
	}
	degradation := valueOf(t, page, kosdiagnostics.ItemDegradation)
	if !listHas(t, degradation, "reasons", brokenApp) || !listHas(t, degradation, "reasons", "restore-1") {
		t.Fatalf("degradation = %v, want the failed application and restore job", degradation)
	}
	blocked := valueOf(t, page, kosdiagnostics.ItemBlocked)
	if !listHas(t, blocked, "objects", brokenApp) || !listHas(t, blocked, "objects", volumeName) {
		t.Fatalf("blocked = %v, want the stalled uninstall and the blocked volume", blocked)
	}
	evidence := valueOf(t, page, kosdiagnostics.ItemEvidence)
	if !listHas(t, evidence, "applications", brokenApp) {
		t.Fatalf("evidence = %v, want %s's chain", evidence, brokenApp)
	}
	targets := valueOf(t, page, kosdiagnostics.ItemExport)
	if !listHas(t, targets, "applications", application) {
		t.Fatalf("export targets = %v, want %s", targets, application)
	}
}

// readPages lists the pages the panel serves.
func readPages(t *testing.T, shell *harness.Shell, token string) map[string]bool {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages", nil, token)
	if status != http.StatusOK {
		t.Fatalf("list pages = %d %v", status, body)
	}
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("page list has no items: %v", body)
	}
	listed := make(map[string]bool, len(raw))
	for _, entry := range raw {
		page, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("page entry is not an object: %v", entry)
		}
		listed[harness.String(page, "id")] = true
	}
	return listed
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

// assertItems checks one page's item set, source types and observation times.
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
	}
}

// assertReadOnly checks that a page states why it is read-only and offers no
// entry (C-53).
func assertReadOnly(t *testing.T, page map[string]any) {
	t.Helper()
	if page["readOnly"] != true || harness.String(page, "readOnlyReason") == "" {
		t.Fatalf("page %q readOnly = %v reason = %q, want a stated read-only page",
			harness.String(page, "id"), page["readOnly"], harness.String(page, "readOnlyReason"))
	}
	actions, _ := page["actions"].([]any)
	if len(actions) != 0 {
		t.Fatalf("read-only page %q carries actions %v", harness.String(page, "id"), actions)
	}
	for id, item := range itemsByID(t, page) {
		if item["entry"] != nil {
			t.Errorf("read-only item %q carries a modification entry", id)
		}
	}
}

// assertEntry checks one item's modification entry.
func assertEntry(t *testing.T, page map[string]any, id, action string) {
	t.Helper()
	entry := entryOf(t, page, id)
	if harness.String(entry, "method") == "" || harness.String(entry, "path") == "" {
		t.Errorf("item %q entry = %v, want the control-plane write of %s", id, entry, action)
	}
}

// entryOf reads one item's entry.
func entryOf(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	item, known := itemsByID(t, page)[id]
	if !known {
		t.Fatalf("page %q has no item %q", harness.String(page, "id"), id)
	}
	entry, ok := item["entry"].(map[string]any)
	if !ok {
		t.Fatalf("item %q entry = %v, want an object", id, item["entry"])
	}
	return entry
}

// actionsOf reads the page actions.
func actionsOf(t *testing.T, page map[string]any) []map[string]any {
	t.Helper()
	raw, _ := page["actions"].([]any)
	actions := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		action, ok := entry.(map[string]any)
		if ok {
			actions = append(actions, action)
		}
	}
	return actions
}

// valueOf reads one item's value.
func valueOf(t *testing.T, page map[string]any, id string) map[string]any {
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

// listHas checks one list field of a value for an entry naming name.
func listHas(t *testing.T, value map[string]any, field, name string) bool {
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

// count reads the length of one list field.
func count(t *testing.T, value map[string]any, field string) int {
	t.Helper()
	raw, ok := value[field].([]any)
	if !ok {
		t.Fatalf("value has no %s list: %v", field, value)
	}
	return len(raw)
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

// affectedByNode reports whether one node's affected list names an application.
func affectedByNode(t *testing.T, value map[string]any, node, name string) bool {
	t.Helper()
	raw, ok := value["nodes"].([]any)
	if !ok {
		t.Fatalf("affected has no nodes: %v", value)
	}
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		if !ok || harness.String(item, "node") != node {
			continue
		}
		applications, _ := item["applications"].([]any)
		for _, application := range applications {
			if text, ok := application.(string); ok && text == harness.Namespace+"/"+name {
				return true
			}
		}
	}
	return false
}

// installableDefinition reports whether the installable list marks one
// definition installable.
func installableDefinition(t *testing.T, value map[string]any, name string) bool {
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

// projectionHasKey reports whether one ledger row holds a key in a space.
func projectionHasKey(t *testing.T, projection []any, space, value string) bool {
	t.Helper()
	for _, entry := range projection {
		row, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		keys, _ := row["keys"].([]any)
		for _, key := range keys {
			held, ok := key.(map[string]any)
			if ok && harness.String(held, "space") == space && harness.String(held, "value") == value {
				return true
			}
		}
	}
	return false
}

// resolutionHasAddress reports whether one host resolved to an address.
func resolutionHasAddress(t *testing.T, value map[string]any, host, address string) bool {
	t.Helper()
	raw, ok := value["results"].([]any)
	if !ok {
		t.Fatalf("resolution has no results: %v", value)
	}
	for _, entry := range raw {
		result, ok := entry.(map[string]any)
		if !ok || harness.String(result, "host") != host {
			continue
		}
		addresses, _ := result["addresses"].([]any)
		for _, item := range addresses {
			if text, ok := item.(string); ok && text == address {
				return true
			}
		}
	}
	return false
}

// alertFor reports whether one device health alert names a node and detail.
func alertFor(t *testing.T, value map[string]any, node, detail string) bool {
	t.Helper()
	raw, ok := value["alerts"].([]any)
	if !ok {
		t.Fatalf("health has no alerts: %v", value)
	}
	for _, entry := range raw {
		alert, ok := entry.(map[string]any)
		if !ok || harness.String(alert, "node") != node {
			continue
		}
		if strings.Contains(harness.String(alert, "detail"), detail) {
			return true
		}
	}
	return false
}

// markerInflated reports whether one fact is marked invalid.
func markerInflated(t *testing.T, value map[string]any, node string) bool {
	t.Helper()
	return markerField(t, value, node, "invalid")
}

// markerStale reports whether one fact is marked stale.
func markerStale(t *testing.T, value map[string]any, node string) bool {
	t.Helper()
	return markerField(t, value, node, "stale")
}

// markerField reads one marker of one fact.
func markerField(t *testing.T, value map[string]any, node, field string) bool {
	t.Helper()
	raw, ok := value["facts"].([]any)
	if !ok {
		t.Fatalf("markers has no facts: %v", value)
	}
	for _, entry := range raw {
		fact, ok := entry.(map[string]any)
		if ok && harness.String(fact, "node") == node {
			return fact[field] == true
		}
	}
	return false
}

// capabilityVerified reports whether one capability result is verified.
func capabilityVerified(t *testing.T, value map[string]any, name string) bool {
	t.Helper()
	raw, ok := value["capabilities"].([]any)
	if !ok {
		t.Fatalf("capabilities has no list: %v", value)
	}
	for _, entry := range raw {
		capability, ok := entry.(map[string]any)
		if ok && harness.String(capability, "capability") == name && harness.String(capability, "state") == "Verified" {
			return true
		}
	}
	return false
}

// definitionHasGap reports whether one definition lists a missing capability.
func definitionHasGap(t *testing.T, value map[string]any, name string) bool {
	t.Helper()
	raw, ok := value["definitions"].([]any)
	if !ok {
		t.Fatalf("gaps has no definitions: %v", value)
	}
	for _, entry := range raw {
		definition, ok := entry.(map[string]any)
		if !ok || harness.String(definition, "name") != name {
			continue
		}
		gaps, _ := definition["gaps"].([]any)
		return len(gaps) > 0
	}
	return false
}

// componentMissingDependency reports whether one component lists a missing
// dependency in its DependenciesMet condition (C-28).
func componentMissingDependency(t *testing.T, value map[string]any, name string) bool {
	t.Helper()
	raw, ok := value["components"].([]any)
	if !ok {
		t.Fatalf("extension state has no components: %v", value)
	}
	for _, entry := range raw {
		component, ok := entry.(map[string]any)
		if !ok || harness.String(component, "name") != name {
			continue
		}
		conditions, _ := component["conditions"].([]any)
		for _, entry := range conditions {
			condition, ok := entry.(map[string]any)
			if !ok || harness.String(condition, "type") != extension.ConditionDependenciesMet {
				continue
			}
			return condition["status"] == "False" && harness.String(condition, "message") != ""
		}
	}
	return false
}

// componentCompatible reports whether one component reports framework
// compatibility.
func componentCompatible(t *testing.T, value map[string]any, name string) bool {
	t.Helper()
	raw, ok := value["components"].([]any)
	if !ok {
		t.Fatalf("compatibility has no components: %v", value)
	}
	for _, entry := range raw {
		component, ok := entry.(map[string]any)
		if ok && harness.String(component, "name") == name && harness.String(component, "frameworkCompatible") == "True" {
			return true
		}
	}
	return false
}

// nodeVersioned reports whether the version matrix names one node.
func nodeVersioned(t *testing.T, value map[string]any, node string) bool {
	t.Helper()
	cluster, ok := value["cluster"].(map[string]any)
	if !ok {
		t.Fatalf("versions has no cluster: %v", value)
	}
	nodes, ok := cluster["nodes"].([]any)
	if !ok {
		t.Fatalf("cluster has no nodes: %v", cluster)
	}
	for _, entry := range nodes {
		name, ok := entry.(map[string]any)
		if ok && harness.String(name, "name") == node && harness.String(name, "kubeletVersion") != "" {
			return true
		}
	}
	return false
}

// fixedResolver answers every probe with one address.
type fixedResolver struct{}

// LookupHost implements kosnetwork.Resolver.
func (fixedResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	return []string{"10.0.0.1"}, nil
}

// shellClock is the clock the shell and the objects share.
func shellClock() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }

// nowText is the acceptance clock as a timestamp.
func nowText() string { return shellClock().Format(time.RFC3339) }

// createNode writes one node with its ready condition and schedulability.
func createNode(t *testing.T, cluster *harness.Cluster, name string, ready, unschedulable bool) {
	t.Helper()
	status := "False"
	reason := "NodeNotReady"
	if ready {
		status = "True"
		reason = "KubeletReady"
	}
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"unschedulable": unschedulable},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Ready", "status": status, "reason": reason,
				"lastHeartbeatTime": nowText(), "lastTransitionTime": nowText(),
			}},
			"nodeInfo":    map[string]any{"kubeletVersion": "v1.37.0", "osImage": "acceptance"},
			"allocatable": map[string]any{"cpu": "4", "memory": "8Gi"},
		},
	})
}

// definitionOf is one application definition with its capability requirements.
func definitionOf(name string, requires []any) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "ApplicationDefinition",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{
			"package":  map[string]any{"version": "1.2.0", "digest": "sha256:offered", "dataFormat": "v1"},
			"requires": requires,
		},
	}
}

// providerOf is the verified storage provider.
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

// blockedProviderOf is a provider that registers GPU support, never verified,
// whose upgrade is blocked by a dependent (C-48).
func blockedProviderOf() map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "Provider",
		"metadata": map[string]any{
			"name": blockedName,
			"labels": map[string]any{
				"managed-by": "acceptance", "kos.tsic.top/owner-uid": "00000000-0000-0000-0000-000000000001",
			},
		},
		"spec": map[string]any{
			"componentRef": "kos-gpu",
			"version":      "0.9.0",
			"capabilities": []any{map[string]any{
				"name": "compute.gpu", "version": "v1", "scope": "node",
				"caseSet": "gpu", "caseSetDigest": "digest-gpu",
			}},
		},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": string(v1beta1.HealthStateDegraded)},
			"conditions": []any{
				map[string]any{"type": "Registered", "status": "True", "reason": "Registered", "message": "acceptance", "lastTransitionTime": nowText()},
				map[string]any{"type": "Verified", "status": "False", "reason": "CasesFailed", "message": "GPU 用例未通过", "lastTransitionTime": nowText()},
				map[string]any{"type": "UpgradeBlocked", "status": "True", "reason": "CapabilityWithdrawn", "message": "撤回仍被引用的能力", "lastTransitionTime": nowText()},
			},
			"effectiveCapabilities": []any{map[string]any{
				"name": "compute.gpu", "version": "v1", "scope": "node",
				"caseSet": "gpu", "caseSetDigest": "digest-gpu",
			}},
			"capabilities": []any{map[string]any{
				"name": "compute.gpu", "version": "v1", "source": "KOS", "state": "Unverified",
				"reason": "CasesFailed", "failedCases": []any{"gpu-scheduling"},
			}},
			"dependents": []any{map[string]any{"kind": "Application", "namespace": harness.Namespace, "name": application}},
		},
	}
}

// applicationOf is one installed application whose definition moved past the
// baseline the case names.
func applicationOf(name string, now time.Time, status map[string]any) map[string]any {
	base := map[string]any{
		"phase":  "Ready",
		"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
		"conditions": []any{map[string]any{
			"type": "Reconciled", "status": "True", "reason": "Reconciled", "message": "acceptance",
			"lastTransitionTime": nowText(),
		}},
		"applied": map[string]any{"definitionDigest": "sha256:applied", "dataFormat": "v1"},
	}
	for key, value := range status {
		base[key] = value
	}
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner":         ownerName,
			"definitionRef": definition,
			"namespace":     harness.Namespace,
			"volumes":       []any{volumeName},
			"replicas":      2,
		},
		"status": base,
	}
}

// writeVolumeStatus bounds the volume to the maintained node and marks its claim
// blocked, which the diagnostics page lists (C-51).
func writeVolumeStatus(t *testing.T, cluster *harness.Cluster, now time.Time) {
	t.Helper()
	status := map[string]any{
		"phase":         "Bound",
		"health":        map[string]any{"state": string(v1beta1.HealthStateDegraded)},
		"boundClaim":    volumeName,
		"storageClass":  storageClass,
		"provider":      map[string]any{"name": providerName, "version": "1.0.0"},
		"reclaimPolicy": "Delete",
		"node":          maintained,
		"conditions": []any{
			map[string]any{"type": "Reconciled", "status": "True", "reason": "Reconciled", "message": "acceptance", "lastTransitionTime": nowText()},
			map[string]any{"type": "ClaimBlocked", "status": "True", "reason": "ClaimPending", "message": "声明仍在绑定", "lastTransitionTime": nowText()},
		},
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

// restoreJobOf is a restore that failed its drill comparison, which both the
// overview's abnormal scan and the diagnostics page must show (C-42, C-49).
func restoreJobOf(now time.Time) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "RestoreJob",
		"metadata":   map[string]any{"name": "restore-1", "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner": ownerName, "policy": backupName, "targetApplication": "shop-restored",
		},
		"status": map[string]any{
			"phase":  "Failed",
			"health": map[string]any{"state": string(v1beta1.HealthStateUnavailable)},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "False", "reason": "Failed", "message": "演练不一致",
				"lastTransitionTime": nowText(),
			}},
		},
	}
}

// backupOf is the backup policy with a completed run and drill (C-41, C-42).
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

// deviceOf is one device fact; the ghost fact names another node, which marks it
// invalid, and the old fact is beyond C-37's refresh bound.
func deviceOf(name, node string, observedAt time.Time) map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "NodeDevice",
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{"managed-by": "discovery"},
		},
		"spec": map[string]any{
			"node":       node,
			"collector":  name,
			"observedAt": observedAt.UTC().Format(time.RFC3339),
			"devices": []any{map[string]any{
				"name": "gpu0", "serial": "S-" + name, "model": "Acceptance", "kind": "gpu",
				"present": true, "recognized": true,
				"driverReady": node == name,
				"published":   node == name,
				"occupancy":   map[string]any{"source": "device-plugin", "reference": "gpu-pool"},
				"observedAt":  observedAt.UTC().Format(time.RFC3339),
			}},
		},
	}
}

// componentOf is an installed component whose dependency on a GPU capability is
// not met and whose framework version is compatible (C-28, C-51).
func componentOf() map[string]any {
	return map[string]any{
		"apiVersion": v1beta1.GroupVersion.String(),
		"kind":       "SystemComponent",
		"metadata": map[string]any{
			"name": "kos-network",
			"labels": map[string]any{
				"managed-by": "acceptance", "kos.tsic.top/owner-uid": "00000000-0000-0000-0000-000000000002",
			},
		},
		"spec": map[string]any{
			"package": map[string]any{"reference": "acceptance://kos-network", "digest": "sha256:network"},
			"disable": false,
		},
		"status": map[string]any{
			"phase":  "Unavailable",
			"health": map[string]any{"state": string(v1beta1.HealthStateDegraded)},
			"conditions": []any{
				map[string]any{"type": "PackageVerified", "status": "True", "reason": "PackageVerified", "message": "包摘要匹配", "lastTransitionTime": nowText()},
				map[string]any{"type": "FrameworkCompatible", "status": "True", "reason": "Compatible", "message": "框架版本兼容", "lastTransitionTime": nowText()},
				map[string]any{"type": "DependenciesMet", "status": "False", "reason": "DependenciesMissing", "message": "依赖未满足：capability compute.gpu v1", "lastTransitionTime": nowText()},
				map[string]any{"type": "ContributionsLoaded", "status": "False", "reason": "DependenciesMissing", "message": "依赖未满足", "lastTransitionTime": nowText()},
			},
		},
	}
}

// createEvent writes one event about the application.
func createEvent(t *testing.T, cluster *harness.Cluster, name string, at time.Time) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace, map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"involvedObject": map[string]any{
			"apiVersion": v1beta1.GroupVersion.String(), "kind": "Application",
			"namespace": harness.Namespace, "name": application,
		},
		"type":          "Warning",
		"reason":        "BackendUnavailable",
		"message":       "后端不可用",
		"count":         3,
		"lastTimestamp": at.UTC().Format(time.RFC3339),
	})
}
