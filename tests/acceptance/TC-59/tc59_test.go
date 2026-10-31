// Package tc59 verifies that deleting one of two desktop entries of an
// installed application removes that entry alone: the application keeps its
// phase and replica count, its downstream objects are unchanged, and the
// remaining entry still points at the same application (AC-29, TC-59).
package tc59

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/dock"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	applicationName = "media"
	definitionName  = "media-template"
	firstEntry      = "media-desktop"
	secondEntry     = "media-dock"
)

// mediaTemplate renders the workload the application owns, so the case has a
// downstream object that an entry deletion must not touch.
const mediaTemplate = `apiVersion: apps/v1
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
        - name: media
          image: example.test/media:1
`

// TestTC59DeletingOneEntryKeepsTheApplication installs kos-dock, puts two
// entries of one application on the desktop, deletes one and reads the
// application and its downstream objects back.
func TestTC59DeletingOneEntryKeepsTheApplication(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, dock.APIGroup, dock.EntryVersion, dock.EntryKind, dock.EntryResource, true)
	cluster.InstallDefinition(t, harness.DefinitionWithTemplate(t, definitionName, mediaTemplate, nil))
	cluster.CreateApplication(t, applicationName, definitionName, harness.Namespace)
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, applicationName, 6); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", phase, app.PhaseReady)
	}

	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackage(t, "dock", dock.Declaration(), artifacts)
	result, found := installation.Component("dock")
	if !found || result.Phase != extension.PhaseReady || len(result.Problems) > 0 {
		t.Fatalf("kos-dock component = %+v (found %v), want Ready with no contribution problem", result, found)
	}
	if _, found := installation.Registry().PageFor(dock.PageID, write.RoleMember); !found {
		t.Fatal("the installed kos-dock component serves no desktop page")
	}
	shell := cluster.StartShell(t, func(config *pages.Config) { config.Readings = readingShell{} })
	shell.Use(installation)
	owner := shell.SignInOwner(t, "owner", "owner-password")

	// The page answers while kos-dock is active, so the 404 below is the
	// component leaving and not a page that never worked.
	if status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+dock.PageID, nil, owner); status != http.StatusOK {
		t.Fatalf("enabled page = %d %v, want %d", status, body, http.StatusOK)
	}
	if _, found := installation.Registry().PageFor(dock.PageID, write.RoleGuest); !found {
		t.Fatal("the desktop page is not visible to a Guest")
	}

	for _, name := range []string{firstEntry, secondEntry} {
		addEntry(t, shell, name, owner)
	}

	// The declaration names the global Owner as the writer: the creates above
	// answered 202, and a Guest is refused with 403 (C-59).
	guest := guestAccount(t, shell, owner)
	guestMethod, guestPath := harness.PageAction(t, dock.DesktopPage(), dock.ActionAddEntry, "", harness.Namespace)
	if status, body := shell.Call(t, guestMethod, guestPath, entry("guest-entry"), guest); status != http.StatusForbidden {
		t.Fatalf("Guest create = %d %v, want %d", status, body, http.StatusForbidden)
	}

	// C-19: the class is served only while kos-dock is active. Disabling the
	// component withdraws the class, the page and the writes; the stored
	// entries stay (C-28).
	cluster.SetComponentDisabled(t, "dock", true)
	disabled := cluster.ExtensionState(t, artifacts)
	if result, found := disabled.Component("dock"); !found || result.Phase != extension.PhaseDisabled {
		t.Fatalf("disabled kos-dock component = %+v (found %v), want %q", result, found, extension.PhaseDisabled)
	}
	shell.Use(disabled)

	// An unserved class is an explicit refusal, never an empty list.
	status, body := shell.Call(t, http.MethodGet, entryListPath(), nil, owner)
	if status != http.StatusUnprocessableEntity || harness.String(body, "error", "code") != controlplane.CodeCapabilityMissing {
		t.Fatalf("read of the disabled class = %d %v, want %d %s", status, body, http.StatusUnprocessableEntity, controlplane.CodeCapabilityMissing)
	}
	// A write through the page's declared add action is refused the same way.
	method, path := harness.PageAction(t, dock.DesktopPage(), dock.ActionAddEntry, "", harness.Namespace)
	if status, body := shell.Call(t, method, path, entry("refused"), owner); status != http.StatusUnprocessableEntity ||
		harness.String(body, "error", "code") != controlplane.CodeCapabilityMissing {
		t.Fatalf("write to the disabled class = %d %v, want %d %s", status, body, http.StatusUnprocessableEntity, controlplane.CodeCapabilityMissing)
	}
	// The page entry disappears with the component.
	if status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+dock.PageID, nil, owner); status != http.StatusNotFound {
		t.Fatalf("disabled page = %d %v, want %d", status, body, http.StatusNotFound)
	}

	// Re-enabling restores the class and the page; both entries were kept.
	cluster.SetComponentDisabled(t, "dock", false)
	enabled := cluster.ExtensionState(t, artifacts)
	shell.Use(enabled)
	if _, found := enabled.Registry().PageFor(dock.PageID, write.RoleMember); !found {
		t.Fatal("the re-enabled kos-dock component serves no desktop page")
	}
	if status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+dock.PageID, nil, owner); status != http.StatusOK {
		t.Fatalf("re-enabled page = %d %v, want %d", status, body, http.StatusOK)
	}
	if status, body := shell.Call(t, http.MethodGet, entryListPath(), nil, owner); status != http.StatusOK {
		t.Fatalf("read of the re-enabled class = %d %v, want %d", status, body, http.StatusOK)
	}
	if count := countEntries(t, cluster); count != 2 {
		t.Fatalf("entries after re-enabling kos-dock = %d, want the two created", count)
	}

	// The application's phase, its rendered set and every workload are the
	// baseline the deletion must leave alone.
	before := cluster.ApplicationStatusOf(t, applicationName)
	workloadsBefore := workloads(t, cluster)

	remove(t, shell, firstEntry, owner)

	if _, err := entries(t, cluster).Get(context.Background(), firstEntry, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the deleted entry still exists (err = %v)", err)
	}

	after := cluster.ApplicationStatusOf(t, applicationName)
	if after.Phase != before.Phase || after.Phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", after.Phase, app.PhaseReady)
	}
	if len(after.Rendered) != len(before.Rendered) {
		t.Fatalf("rendered objects = %d, want the %d from before the deletion", len(after.Rendered), len(before.Rendered))
	}
	if current := workloads(t, cluster); current != workloadsBefore {
		t.Fatalf("workloads after the deletion = %q, want %q", current, workloadsBefore)
	}

	// The remaining entry still opens the same application, and it is the one
	// the desktop kept rather than a second object under another name.
	remaining, err := entries(t, cluster).Get(context.Background(), secondEntry, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the remaining entry: %v", err)
	}
	spec, err := dock.SpecOf(remaining)
	if err != nil {
		t.Fatalf("decode the remaining entry: %v", err)
	}
	if spec.Application != applicationName {
		t.Fatalf("the remaining entry points at %q, want %q", spec.Application, applicationName)
	}
	if count := countEntries(t, cluster); count != 1 {
		t.Fatalf("entries after the deletion = %d, want the one remaining", count)
	}

	// C-18/C-29: uninstalling kos-dock keeps the entries — the retained one is
	// listed in the residual manifest — and deletes its preferences.
	status, body = shell.Call(t, http.MethodPut, "/api/v1/preferences/"+dock.Identifier+"/"+dock.PreferenceLayout,
		map[string]any{"value": "dock-layout"}, owner)
	if status != http.StatusNoContent {
		t.Fatalf("write the dock preference = %d %v", status, body)
	}
	cluster.EnsureNamespace(t, api.SystemNamespace)
	// Record the declared preference fate while the component is installed;
	// the uninstall pass applies it.
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	uid := cluster.ComponentUID(t, "dock")
	cluster.DeleteComponent(t, "dock")
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))

	residuals := cluster.Residuals(t, uid)
	if len(residuals) != 1 || residuals[0].Disposition != extension.ResidualRetained ||
		residuals[0].Object.Kind != dock.EntryKind || residuals[0].Object.Name != secondEntry {
		t.Fatalf("uninstall residuals = %+v, want the remaining entry retained", residuals)
	}

	// Reinstalling shows the retained entry still on the desktop and the
	// preference gone with the component (C-27).
	restored := cluster.InstallPackage(t, "dock", dock.Declaration(), artifacts)
	shell.Use(restored)
	if count := countEntries(t, cluster); count != 1 {
		t.Fatalf("entries after reinstalling kos-dock = %d, want the retained entry", count)
	}
	status, body = shell.Call(t, http.MethodGet, "/api/v1/preferences/"+dock.Identifier, nil, owner)
	if status != http.StatusOK {
		t.Fatalf("read the dock preferences = %d %v", status, body)
	}
	if value := harness.String(body, "items", dock.PreferenceLayout); value != "" {
		t.Fatalf("dock preference after reinstalling = %q, want it deleted with the component", value)
	}
}

// readingShell answers every server-source item, so the desktop page renders
// and a 404 after the component is disabled proves withdrawal rather than a
// page that never worked.
type readingShell struct{}

// Read implements pages.Readings.
func (readingShell) Read(context.Context, identity.Session, extension.PageItem) (pages.Reading, *controlplane.Error) {
	return pages.Reading{ObservedAt: time.Now()}, nil
}

// entry builds one entry of the application.
func entry(name string) map[string]any {
	return map[string]any{
		"apiVersion": dock.APIGroup + "/" + dock.EntryVersion,
		"kind":       dock.EntryKind,
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"application": applicationName,
			"displayName": "媒体库",
			"icon":        "media",
			"visibility":  dock.VisibilityEveryone,
			"order":       10,
			"source":      "manual",
		},
	}
}

// addEntry creates one entry through the page's declared add action.
func addEntry(t *testing.T, shell *harness.Shell, name, token string) {
	t.Helper()
	method, path := harness.PageAction(t, dock.DesktopPage(), dock.ActionAddEntry, "", harness.Namespace)
	status, body := shell.Call(t, method, path, entry(name), token)
	if status != http.StatusAccepted {
		t.Fatalf("create entry %s = %d %v", name, status, body)
	}
}

// guestAccount creates a Guest account behind the owner session and returns a
// token carrying the changed password, so a write can be judged by its role.
func guestAccount(t *testing.T, shell *harness.Shell, owner string) string {
	t.Helper()
	status, body := shell.Call(t, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "guest", "role": string(write.RoleGuest),
	}, owner)
	if status != http.StatusCreated {
		t.Fatalf("create the Guest account = %d %v", status, body)
	}
	oneTime := harness.String(body, "oneTimePassword")
	if oneTime == "" {
		t.Fatalf("create the Guest account returned no one-time password: %v", body)
	}
	token, _ := shell.Accounts.SignIn(t, "guest", oneTime)
	shell.Accounts.ChangePassword(t, token, oneTime, "guest-password")
	token, _ = shell.Accounts.SignIn(t, "guest", "guest-password")
	return token
}

// entryListPath reads the entry class through the control plane.
func entryListPath() string {
	return "/api/v1/resources/" + dock.EntryKind + "?apiVersion=" + dock.APIGroup + "/" + dock.EntryVersion +
		"&namespace=" + harness.Namespace
}

// remove deletes one entry through the page's declared remove action, behind
// the version precondition the API requires.
func remove(t *testing.T, shell *harness.Shell, name, token string) {
	t.Helper()
	stored, err := entries(t, shell.Cluster).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read entry %s: %v", name, err)
	}
	headers := harness.Bearer(token)
	headers["If-Match"] = stored.GetResourceVersion()
	method, path := harness.PageAction(t, dock.DesktopPage(), dock.ActionRemoveEntry, name, harness.Namespace)
	status, body := shell.Accounts.HTTP.Call(t, method, path, nil, headers)
	if status != http.StatusAccepted {
		t.Fatalf("delete entry %s = %d %v", name, status, body)
	}
}

// entries resolves the entry class client.
func entries(t *testing.T, cluster *harness.Cluster) dynamic.ResourceInterface {
	t.Helper()
	return cluster.ExtensionResource(dock.APIGroup, dock.EntryVersion, dock.EntryResource, harness.Namespace)
}

// countEntries lists the entries the desktop holds.
func countEntries(t *testing.T, cluster *harness.Cluster) int {
	t.Helper()
	list, err := entries(t, cluster).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	return len(list.Items)
}

// workloads renders every workload the namespace holds as name, UID and replica
// count, so a recreate or a rescale shows up as a difference.
func workloads(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	list, err := cluster.Kube.AppsV1().Deployments(harness.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	rendered := ""
	for index := range list.Items {
		deployment := &list.Items[index]
		replicas := int32(0)
		if deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		rendered += deployment.Name + "/" + string(deployment.UID) + "/" + strconv.Itoa(int(replicas)) + ";"
	}
	return rendered
}
