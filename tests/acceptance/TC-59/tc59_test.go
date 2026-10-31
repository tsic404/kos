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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/dock"
	"github.com/tsic404/kos/pkg/extension"
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

	installation := cluster.InstallPackage(t, "dock", dock.Declaration(), extension.MemoryArtifacts{})
	result, found := installation.Component("dock")
	if !found || result.Phase != extension.PhaseReady || len(result.Problems) > 0 {
		t.Fatalf("kos-dock component = %+v (found %v), want Ready with no contribution problem", result, found)
	}
	if _, found := installation.Registry().PageFor(dock.PageID, write.RoleMember); !found {
		t.Fatal("the installed kos-dock component serves no desktop page")
	}
	shell := cluster.StartShell(t)
	shell.Use(installation)
	owner := shell.SignInOwner(t, "owner", "owner-password")

	for _, name := range []string{firstEntry, secondEntry} {
		status, body := shell.Call(t, http.MethodPost, entryPath(), entry(name), owner)
		if status != http.StatusAccepted {
			t.Fatalf("create entry %s = %d %v", name, status, body)
		}
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

// entryPath addresses the entry class through the control plane.
func entryPath() string {
	return "/api/v1/resources/" + dock.EntryKind + "?namespace=" + harness.Namespace
}

// remove deletes one entry behind the version precondition the API requires.
func remove(t *testing.T, shell *harness.Shell, name, token string) {
	t.Helper()
	stored, err := entries(t, shell.Cluster).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read entry %s: %v", name, err)
	}
	headers := harness.Bearer(token)
	headers["If-Match"] = stored.GetResourceVersion()
	status, body := shell.Accounts.HTTP.Call(t, http.MethodDelete,
		"/api/v1/resources/"+dock.EntryKind+"/"+name+"?apiVersion="+dock.APIGroup+"/"+dock.EntryVersion+"&namespace="+harness.Namespace,
		nil, headers)
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
