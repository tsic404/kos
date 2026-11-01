// Package tc101 verifies the user-space surfaces of C-52 (AC-51, TC-101): the
// desktop renders its entries, its applications and the notification centre, the
// app centre renders its catalogue, the application detail page renders one
// application's health, volumes, endpoints, backup, drift, events and its own log
// lines, and the read-only native view renders one native resource's raw fields
// and related objects without offering a write.
package tc101

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/pkg/app/kostaskmanager"
	"github.com/tsic404/kos/pkg/appstore"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/diagnostics/kosdiagnostics"
	"github.com/tsic404/kos/pkg/dock"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName     = "owner"
	ownerPassword = "correct-horse-battery"
	application   = "shop"
	otherApp      = "blog"
	volumeName    = "data"
	endpointName  = "shop-web"
	backupName    = "nightly"
	entryName     = "shop-desktop"
	brokenEntry   = "broken-desktop"
	mistypedEntry = "mistyped-desktop"
	credential    = "shop-credentials"
	credentialVal = "czNjcjN0"
	host          = "shop.test"
	shopPod       = "shop-0"
	otherPod      = "blog-0"
	shopLine      = "shop started"
	otherLine     = "blog started"
	driftPath     = "spec.replicas"
)

// readTime is the clock the case reads on, so the event fixtures and the
// one-hour window agree.
var readTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// TestTC101UserSpaceSurfaces opens the five surfaces and checks each one's
// must-display items against their sources.
func TestTC101UserSpaceSurfaces(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, dock.APIGroup, dock.EntryVersion, dock.EntryKind, dock.EntryResource, true)

	// The world both applications live in: each renders one deployment whose
	// pod writes its own log lines, and the shop application declares a volume,
	// an endpoint and a backup policy.
	createApplication(t, cluster, application, volumeName)
	createApplication(t, cluster, otherApp)
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, deployment(application))
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, deployment(otherApp))
	cluster.CreateClusterObject(t, "", "v1", "pods", harness.Namespace, pod(shopPod, application))
	cluster.CreateClusterObject(t, "", "v1", "pods", harness.Namespace, pod(otherPod, otherApp))
	cluster.Create(t, harness.Volume(harness.Namespace, volumeName, map[string]any{
		"mobility": "node-local", "capacity": "1Gi", "storageClass": "local", "owner": ownerName,
	}))
	cluster.Create(t, harness.EndpointIn(harness.Namespace, endpointName, harness.ManagedLabels(), map[string]any{
		"kind": "http.host", "targetApp": application, "host": host,
	}))
	cluster.Create(t, harness.BackupPolicyManifest(harness.Namespace, backupName, map[string]any{
		"selector": "app=" + application, "schedule": "0 2 * * *", "consistency": "application",
		"target":    map[string]any{"path": "/backups", "failureDomain": "local"},
		"retention": map[string]any{"keep": 3}, "owner": ownerName,
	}))
	// A policy without an owner is not a wildcard (C-41, C-60): the reconciler
	// backs up only objects owned by the policy's owner, so the detail page must
	// not list this policy for an application owned by someone else.
	cluster.Create(t, harness.BackupPolicyManifest(harness.Namespace, "ownerless", map[string]any{
		"selector": "app=" + application, "schedule": "0 3 * * *",
	}))
	createEntry(t, cluster)
	// A second entry whose definition does not decode keeps its place on the
	// desktop with the reason, instead of failing the whole page (C-30).
	createBrokenEntry(t, cluster)
	// A credential resource the read-only view does not serve (C-14): the view
	// must refuse the class rather than project the value.
	cluster.CreateClusterObject(t, "", "v1", "secrets", harness.Namespace, map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": credential, "namespace": harness.Namespace},
		"type":     "Opaque",
		"data":     map[string]any{"password": credentialVal},
	})

	// The notification centre shows the same events the events route serves, so
	// the fixtures are the window's events plus one about the other application
	// and one older than the window.
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		event("shop-warning", "Application", application, readTime.Add(-5*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		event("blog-warning", "Application", otherApp, readTime.Add(-10*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		event("shop-old", "Application", application, readTime.Add(-2*time.Hour)))

	logs := harness.NewFixtureLogs()
	logs.Add(harness.Namespace, shopPod, "web", readTime.Add(-time.Minute), shopLine)
	logs.Add(harness.Namespace, otherPod, "web", readTime.Add(-time.Minute), otherLine)

	taskmanager := &kostaskmanager.Readings{Dynamic: cluster.Dynamic}
	desktop := &dock.Readings{Dynamic: cluster.Dynamic}
	native := &kosdiagnostics.Readings{Dynamic: cluster.Dynamic}
	shell := cluster.StartShellOptions(t, harness.ShellOptions{
		Logs: logs,
		ControlPlane: func(config *controlplane.Config) {
			config.Now = func() time.Time { return readTime }
		},
	}, func(config *pages.Config) {
		appCenter := config.Readings
		config.Readings = pages.Router{
			dock.Identifier:           desktop,
			appstore.Identifier:       appCenter,
			kostaskmanager.Identifier: taskmanager,
			kosdiagnostics.Identifier: native,
		}
	})
	clock := shell.Accounts.Clock.Now
	taskmanager.Now = clock
	desktop.Now = clock
	native.Now = clock
	taskmanager.Logs = shell.Server

	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackageWith(t, "dock", dock.Declaration(), artifacts, shell.Retention())
	cluster.InstallPackageWith(t, "appstore", appstoreDeclaration(t), artifacts, shell.Retention())
	cluster.InstallPackageWith(t, "taskmanager", kostaskmanager.Declaration(), artifacts, shell.Retention())
	installation := cluster.InstallPackageWith(t, "diagnostics", kosdiagnostics.Declaration(), artifacts, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPassword)

	assertDesktop(t, shell, token)
	assertAppCenter(t, shell, token)
	assertApplicationDetail(t, shell, token)
	assertNativeView(t, shell, token)

	// A session that may not see the application reads no detail of it, and is
	// answered as if it did not exist (C-59).
	memberToken := shell.Account(t, token, "member", "member-password", write.RoleMember)
	status, body := shell.Call(t, http.MethodGet, detailPath(application), nil, memberToken)
	if status != http.StatusNotFound {
		t.Fatalf("hidden application's detail = %d %v, want 404", status, body)
	}
}

// appstoreDeclaration loads the app centre's package, which pins the digest of
// every definition it ships.
func appstoreDeclaration(t *testing.T) extension.Declaration {
	t.Helper()
	declaration, err := appstore.Declaration()
	if err != nil {
		t.Fatalf("build the app-centre declaration: %v", err)
	}
	return declaration
}

// assertDesktop checks C-52's desktop: the entries with the address they open,
// the installed applications, the browser-local recent list and the notification
// centre.
func assertDesktop(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, "/api/v1/pages/"+dock.PageID)
	assertSources(t, page, map[string]string{
		dock.ItemEntries:       extension.PageSourceObject,
		dock.ItemApplications:  extension.PageSourceObject,
		dock.ItemRecent:        extension.PageSourceLocal,
		dock.ItemNotifications: extension.PageSourceObject,
	})

	entries, _ := valueOf(t, page, dock.ItemEntries)["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("entries = %v, want the readable entry and the two unreadable ones", entries)
	}
	readable := entryNamed(t, entries, entryName)
	if harness.String(readable, "application") != application {
		t.Fatalf("entry = %v, want %s on %s", readable, entryName, application)
	}
	if readable["available"] != true {
		t.Fatalf("entry = %v, want an entry the desktop opens", readable)
	}
	if address := harness.String(readable, "launch", "address"); address != "http://"+host+"/" {
		t.Fatalf("launch address = %q, want the endpoint's address", address)
	}
	// An entry whose definition does not decode stays on the desktop and states
	// why it cannot open; it never fails the whole page (C-30).
	for _, name := range []string{brokenEntry, mistypedEntry} {
		unreadable := entryNamed(t, entries, name)
		if unreadable["available"] != false || harness.String(unreadable, "reason") == "" {
			t.Fatalf("entry %q = %v, want the unreadable entry with its reason", name, unreadable)
		}
	}

	applications, _ := valueOf(t, page, dock.ItemApplications)["applications"].([]any)
	if !hasName(applications, application) || !hasName(applications, otherApp) {
		t.Fatalf("applications = %v, want both installed applications", applications)
	}

	// The recent list is browser state (C-53): the item names the storage key,
	// carries no value and has no control-plane observation.
	recent := itemOf(t, page, dock.ItemRecent)
	if harness.String(recent, "store") != dock.StoreRecent {
		t.Fatalf("recent store = %q, want %q", harness.String(recent, "store"), dock.StoreRecent)
	}
	if recent["value"] != nil || recent["observedAt"] != nil {
		t.Fatalf("recent item = %v, want no control-plane value", recent)
	}

	// The notification centre and the events route report the same events in the
	// same order (C-52, C-64).
	notifications := valueOf(t, page, dock.ItemNotifications)
	status, body := shell.Call(t, http.MethodGet, "/api/v1/events?namespace="+harness.Namespace, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the events route = %d %v", status, body)
	}
	if got, want := eventNames(t, notifications), eventNames(t, body); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("notification centre = %v, want the route's events %v", got, want)
	}
	if names := eventNames(t, notifications); len(names) != 2 {
		t.Fatalf("notification centre = %v, want only the two in-window events", names)
	}
}

// assertAppCenter checks the app centre's catalogue items (C-31).
func assertAppCenter(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, "/api/v1/pages/"+appstore.PageID)
	assertSources(t, page, map[string]string{
		appstore.EntriesItem:  extension.PageSourceComputed,
		appstore.UpgradesItem: extension.PageSourceComputed,
	})
	entries := listOf(t, page, appstore.EntriesItem)
	// The value is the entry list the catalogue serves, whatever its length: the
	// page must lay out the install form each entry's config structure generates.
	if len(entries) == 0 {
		t.Fatalf("entries = %v, want the catalogue's entries", entries)
	}
	for _, raw := range entries {
		entry, _ := raw.(map[string]any)
		if harness.String(entry, "source") != appstore.SourceID {
			t.Fatalf("entry = %v, want the app centre's own source", entry)
		}
		if config, _ := entry["config"].([]any); len(config) == 0 {
			t.Fatalf("entry = %v, want the install form its config structure generates", entry)
		}
	}
}

// assertApplicationDetail checks C-52's application detail page: one
// application's must-display items and its own log lines.
func assertApplicationDetail(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, detailPath(application))
	assertSources(t, page, map[string]string{
		kostaskmanager.ItemDetailHealth:    extension.PageSourceObject,
		kostaskmanager.ItemDetailVolumes:   extension.PageSourceObject,
		kostaskmanager.ItemDetailEndpoints: extension.PageSourceObject,
		kostaskmanager.ItemDetailBackup:    extension.PageSourceObject,
		kostaskmanager.ItemDetailDrift:     extension.PageSourceObject,
		kostaskmanager.ItemDetailEvents:    extension.PageSourceObject,
		kostaskmanager.ItemDetailLogs:      extension.PageSourceRead,
	})
	if page["readOnly"] != true || harness.String(page, "readOnlyReason") == "" {
		t.Fatalf("detail page = %v, want a stated read-only page", page["readOnlyReason"])
	}
	if actions := actionsOf(t, page); len(actions) != 0 {
		t.Fatalf("detail page offers actions %v, want none", actions)
	}

	if phase := harness.String(valueOf(t, page, kostaskmanager.ItemDetailHealth), "phase"); phase != "Ready" {
		t.Fatalf("health phase = %q, want the stored phase", phase)
	}
	volumes, _ := valueOf(t, page, kostaskmanager.ItemDetailVolumes)["volumes"].([]any)
	if len(volumes) != 1 {
		t.Fatalf("volumes = %v, want the declared volume", volumes)
	}
	if volume, _ := volumes[0].(map[string]any); volume["found"] != true || harness.String(volume, "name") != volumeName {
		t.Fatalf("volume = %v, want the stored volume", volume)
	}
	endpoints, _ := valueOf(t, page, kostaskmanager.ItemDetailEndpoints)["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %v, want the endpoint targeting the application", endpoints)
	}
	if endpoint, _ := endpoints[0].(map[string]any); harness.String(endpoint, "name") != endpointName {
		t.Fatalf("endpoint = %v, want %s", endpoint, endpointName)
	}
	policies, _ := valueOf(t, page, kostaskmanager.ItemDetailBackup)["policies"].([]any)
	if len(policies) != 1 {
		t.Fatalf("backup policies = %v, want the policy whose selector covers the application", policies)
	}
	if policy, _ := policies[0].(map[string]any); harness.String(policy, "name") != backupName {
		t.Fatalf("backup policy = %v, want %s", policy, backupName)
	}
	drift, _ := valueOf(t, page, kostaskmanager.ItemDetailDrift)["drift"].([]any)
	if len(drift) != 1 {
		t.Fatalf("drift = %v, want the stored entry", drift)
	}
	if names := eventNames(t, valueOf(t, page, kostaskmanager.ItemDetailEvents)); len(names) != 1 || names[0] != "shop-warning" {
		t.Fatalf("events = %v, want only the application's own event", names)
	}

	// The log item is the control plane's own per-application read (C-64): only
	// the application's pod lines, never the other application's in the same
	// namespace.
	lines := logMessages(t, valueOf(t, page, kostaskmanager.ItemDetailLogs))
	if len(lines) != 1 || lines[0] != shopLine {
		t.Fatalf("logs = %v, want only the application's own line", lines)
	}
	other := readPage(t, shell, token, detailPath(otherApp))
	if lines := logMessages(t, valueOf(t, other, kostaskmanager.ItemDetailLogs)); len(lines) != 1 || lines[0] != otherLine {
		t.Fatalf("other application's logs = %v, want only its own line", lines)
	}

	// Naming another object class is refused: the detail page renders an
	// application and never answers with an object the caller did not name
	// (C-52).
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+kostaskmanager.PageApplicationDetail+
		"?kind=Pod&namespace="+harness.Namespace+"&name="+application, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("detail read naming another class = %d %v, want 400", status, body)
	}
	if message := harness.String(body, "error", "message"); !strings.Contains(message, "Application") {
		t.Fatalf("other-class detail refusal = %q, want the application class named", message)
	}
}

// assertNativeView checks C-52's read-only native view: one native resource's raw
// fields and the objects related to it, with no write offered.
func assertNativeView(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	page := readPage(t, shell, token, "/api/v1/pages/"+kosdiagnostics.PageNativeView+
		"?kind=Deployment&namespace="+harness.Namespace+"&name="+application)
	assertSources(t, page, map[string]string{
		kosdiagnostics.ItemRawFields: extension.PageSourceObject,
		kosdiagnostics.ItemRelated:   extension.PageSourceObject,
	})
	if page["readOnly"] != true || harness.String(page, "readOnlyReason") == "" {
		t.Fatalf("native view = %v, want a stated read-only view", page["readOnlyReason"])
	}
	if actions := actionsOf(t, page); len(actions) != 0 {
		t.Fatalf("native view offers actions %v, want none", actions)
	}
	for _, item := range page["items"].([]any) {
		entry, _ := item.(map[string]any)
		if entry["entry"] != nil {
			t.Fatalf("native view item %v carries a modification entry", entry["id"])
		}
	}

	raw := valueOf(t, page, kosdiagnostics.ItemRawFields)
	if harness.String(raw, "kind") != "Deployment" || harness.String(raw, "metadata", "name") != application {
		t.Fatalf("raw fields = %v, want the native resource's own fields", raw)
	}
	if replicas := harness.Nested(raw, "spec", "replicas"); replicas == nil {
		t.Fatalf("raw fields = %v, want the stored spec", raw)
	}
	related, _ := valueOf(t, page, kosdiagnostics.ItemRelated)["related"].([]any)
	if !relatedHas(related, "rendered-by", application) {
		t.Fatalf("related = %v, want the application whose render wrote the resource", related)
	}

	// A view read that names no resource is refused rather than rendered for an
	// object the caller did not name (C-52).
	status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+kosdiagnostics.PageNativeView, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("targetless view read = %d %v, want 400", status, body)
	}
	status, body = shell.Call(t, http.MethodGet, detailPath(application), nil, token)
	if status != http.StatusOK {
		t.Fatalf("targeted detail read = %d %v, want 200", status, body)
	}

	// The view serves only the classes it declares: a credential resource is
	// refused rather than projected, so its value never reaches the browser
	// (C-14). The refusal names the classes the view does serve, and Secret is
	// not among them.
	status, body = shell.Call(t, http.MethodGet, "/api/v1/pages/"+kosdiagnostics.PageNativeView+
		"?kind=Secret&namespace="+harness.Namespace+"&name="+credential, nil, token)
	if status != http.StatusBadRequest {
		t.Fatalf("credential view read = %d %v, want 400", status, body)
	}
	message := harness.String(body, "error", "message")
	if !strings.Contains(message, "可选类") || !strings.Contains(message, "Deployment") {
		t.Fatalf("credential view refusal = %q, want the served classes listed", message)
	}
	if strings.Contains(message, "Secret") {
		t.Fatalf("credential view refusal = %q, want Secret outside the served classes", message)
	}
	if text := fmt.Sprint(body); strings.Contains(text, credentialVal) {
		t.Fatalf("credential view read answered with the secret value: %v", body)
	}
}

// detailPath is the application detail page read for one application.
func detailPath(name string) string {
	return "/api/v1/pages/" + kostaskmanager.PageApplicationDetail +
		"?kind=Application&namespace=" + harness.Namespace + "&name=" + url.QueryEscape(name)
}

// readPage reads one page and fails the case on a refusal.
func readPage(t *testing.T, shell *harness.Shell, token, path string) map[string]any {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, path, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read %s = %d %v", path, status, body)
	}
	return body
}

// itemOf resolves one rendered item.
func itemOf(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	items, _ := page["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if harness.String(item, "id") == id {
			return item
		}
	}
	t.Fatalf("page %q has no item %q", harness.String(page, "id"), id)
	return nil
}

// valueOf resolves one rendered item's value.
func valueOf(t *testing.T, page map[string]any, id string) map[string]any {
	t.Helper()
	item := itemOf(t, page, id)
	value, _ := item["value"].(map[string]any)
	if value == nil {
		t.Fatalf("item %q carries no object value: %v", id, item["value"])
	}
	return value
}

// listOf resolves one rendered item whose value is a list.
func listOf(t *testing.T, page map[string]any, id string) []any {
	t.Helper()
	item := itemOf(t, page, id)
	list, _ := item["value"].([]any)
	if list == nil {
		t.Fatalf("item %q carries no list value: %v", id, item["value"])
	}
	return list
}

// assertSources checks that every named item exists with the source type it
// declares (C-53).
func assertSources(t *testing.T, page map[string]any, want map[string]string) {
	t.Helper()
	for id, source := range want {
		item := itemOf(t, page, id)
		if got := harness.String(item, "source"); got != source {
			t.Fatalf("item %q source = %q, want %q", id, got, source)
		}
	}
}

// actionsOf reads the page's actions.
func actionsOf(t *testing.T, page map[string]any) []any {
	t.Helper()
	actions, _ := page["actions"].([]any)
	return actions
}

// hasName reports whether one list of named objects names name.
func hasName(list []any, name string) bool {
	for _, raw := range list {
		object, _ := raw.(map[string]any)
		if harness.String(object, "name") == name {
			return true
		}
	}
	return false
}

// entryNamed resolves one desktop entry by name.
func entryNamed(t *testing.T, list []any, name string) map[string]any {
	t.Helper()
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		if harness.String(entry, "name") == name {
			return entry
		}
	}
	t.Fatalf("entries = %v, want %s", list, name)
	return nil
}

// relatedHas reports whether the related list carries one relation.
func relatedHas(list []any, relation, name string) bool {
	for _, raw := range list {
		object, _ := raw.(map[string]any)
		if harness.String(object, "relation") == relation && harness.String(object, "name") == name {
			return true
		}
	}
	return false
}

// eventNames reads one event read's names in order.
func eventNames(t *testing.T, read map[string]any) []string {
	t.Helper()
	events, _ := read["events"].([]any)
	names := make([]string, 0, len(events))
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		names = append(names, harness.String(event, "name"))
	}
	return names
}

// logMessages reads one log read's messages in order.
func logMessages(t *testing.T, read map[string]any) []string {
	t.Helper()
	lines, _ := read["lines"].([]any)
	messages := make([]string, 0, len(lines))
	for _, raw := range lines {
		line, _ := raw.(map[string]any)
		messages = append(messages, harness.String(line, "message"))
	}
	return messages
}

// createApplication writes one installed application whose last render wrote its
// deployment.
func createApplication(t *testing.T, cluster *harness.Cluster, name string, volumes ...string) {
	t.Helper()
	// The shop application is not shown to guests or unlisted members, so the
	// case can check that a session which may not see it reads no detail of it
	// (C-59); the other application is visible to every signed-in account.
	visibility := map[string]any{"allowGuests": name != application}
	spec := map[string]any{
		"namespace":  harness.Namespace,
		"owner":      ownerName,
		"visibility": visibility,
	}
	if len(volumes) > 0 {
		spec["volumes"] = volumes
	}
	object := map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"managed-by": "acceptance", "app": name},
		},
		"spec": spec,
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": "Healthy"},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Applied",
				"message": "已应用", "lastTransitionTime": readTime.Format(time.RFC3339),
			}},
			"rendered": []any{map[string]any{
				"group": "apps", "version": "v1", "kind": "Deployment",
				"namespace": harness.Namespace, "name": name,
			}},
			"drift": driftOf(name),
		},
	}
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: object})
}

// driftOf is the stored drift entry of one application; the other application
// has none, so the detail page's drift item shows an application's own record.
func driftOf(name string) []any {
	if name != application {
		return nil
	}
	return []any{map[string]any{
		"object": map[string]any{
			"group": "apps", "kind": "Deployment", "namespace": harness.Namespace, "name": name,
		},
		"path":     driftPath,
		"external": "3",
		"expected": "2",
	}}
}

// deployment is the workload one application renders.
func deployment(name string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"replicas": int64(2),
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": name}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "web", "image": "nginx:1.27",
				}}},
			},
		},
	}
}

// pod is one instance of a workload, carrying the workload's label.
func pod(name, workload string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"app": workload},
		},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "web", "image": "nginx:1.27",
		}}},
	}
}

// createEntry writes the desktop entry through the extension class the package
// declares; the entry class belongs to the extension's own API group (C-19).
func createEntry(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resource := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: dock.APIGroup, Version: dock.EntryVersion, Resource: dock.EntryResource,
	}).Namespace(harness.Namespace)
	if _, err := resource.Create(ctx, &unstructured.Unstructured{Object: entry()}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the desktop entry: %v", err)
	}
}

// createBrokenEntry writes entries whose definitions do not decode: one without
// a spec and one whose order has the wrong type. The class admits both because
// the write paths do not require fields (C-30); the desktop must still show them.
func createBrokenEntry(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	resource := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: dock.APIGroup, Version: dock.EntryVersion, Resource: dock.EntryResource,
	}).Namespace(harness.Namespace)
	objects := []map[string]any{
		{
			"apiVersion": dock.APIGroup + "/" + dock.EntryVersion,
			"kind":       dock.EntryKind,
			"metadata":   map[string]any{"name": brokenEntry, "namespace": harness.Namespace},
		},
		{
			"apiVersion": dock.APIGroup + "/" + dock.EntryVersion,
			"kind":       dock.EntryKind,
			"metadata":   map[string]any{"name": mistypedEntry, "namespace": harness.Namespace},
			"spec":       map[string]any{"application": application, "order": "high"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, object := range objects {
		if _, err := resource.Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create the unreadable desktop entry: %v", err)
		}
	}
}

// entry is the desktop entry that opens the shop application.
func entry() map[string]any {
	return map[string]any{
		"apiVersion": dock.APIGroup + "/" + dock.EntryVersion,
		"kind":       dock.EntryKind,
		"metadata":   map[string]any{"name": entryName, "namespace": harness.Namespace},
		"spec": map[string]any{
			"application": application,
			"displayName": "商店",
			"icon":        "shop",
			"visibility":  dock.VisibilityEveryone,
			"order":       int64(10),
		},
	}
}

// event is one event about one application, stamped inside the window.
func event(name, kind, subject string, lastSeen time.Time) map[string]any {
	stamp := lastSeen.UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"involvedObject": map[string]any{
			"apiVersion": "kos.tsic.top/v1beta1", "kind": kind,
			"namespace": harness.Namespace, "name": subject,
		},
		"type":               "Warning",
		"reason":             "BackendUnavailable",
		"message":            "no ready backend",
		"count":              1,
		"firstTimestamp":     stamp,
		"lastTimestamp":      stamp,
		"reportingComponent": "kos-shell",
		"reportingInstance":  "kos-shell-1",
	}
}
