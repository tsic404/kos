// Package tc60 verifies that a desktop entry keeps pointing at its application
// when the application's workload and endpoint are renamed: the entry keeps its
// identity, visibility and order, the address it opens follows the endpoint,
// and an entry whose application becomes unavailable stays on the desktop with
// the reason it cannot open (AC-29, TC-60).
package tc60

import (
	"context"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/dock"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	applicationName = "media"
	definitionName  = "media-template"
	entryName       = "media-desktop"
	firstEndpoint   = "media-entry"
	secondEndpoint  = "media-entry-v2"
	firstWorkload   = "media"
	secondWorkload  = "media-v2"
)

// mediaTemplate renders the workload the application owns, so the case can
// rename a downstream object the entry never names.
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

// TestTC60EntryFollowsTheApplicationAndItsEndpoint adjusts one entry, renames
// the application's workload and endpoint, and resolves the entry after each
// step.
func TestTC60EntryFollowsTheApplicationAndItsEndpoint(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, dock.APIGroup, dock.EntryVersion, dock.EntryKind, dock.EntryResource, true)
	cluster.InstallDefinition(t, harness.DefinitionWithTemplate(t, definitionName, mediaTemplate, nil))
	cluster.CreateApplication(t, applicationName, definitionName, harness.Namespace)
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, applicationName, 6); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", phase, app.PhaseReady)
	}
	cluster.Create(t, harness.Endpoint(firstEndpoint, harness.ManagedLabels(), map[string]any{
		"kind":      string(v1beta1.EndpointKindHTTPHost),
		"targetApp": applicationName,
		"host":      "media.test",
	}))

	installation := cluster.InstallPackage(t, "dock", dock.Declaration(), extension.MemoryArtifacts{})
	shell := cluster.StartShell(t)
	shell.Use(installation)
	owner := shell.SignInOwner(t, "owner", "owner-password")

	status, body := shell.Call(t, http.MethodPost, entriesPath(), entry(dock.VisibilityOwner, 30), owner)
	if status != http.StatusAccepted {
		t.Fatalf("create the entry = %d %v", status, body)
	}
	adjust(t, shell, owner)

	stored, err := entries(t, cluster).Get(context.Background(), entryName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the entry: %v", err)
	}
	spec, err := dock.SpecOf(stored)
	if err != nil {
		t.Fatalf("decode the entry: %v", err)
	}
	if spec.Visibility != dock.VisibilityEveryone || spec.Order != 5 {
		t.Fatalf("visibility/order = %s/%d, want the adjusted %s/5", spec.Visibility, spec.Order, dock.VisibilityEveryone)
	}
	if view := resolve(t, cluster, stored); view.Launch.Address != "http://media.test/" || view.Launch.Endpoint != firstEndpoint {
		t.Fatalf("launch = %+v, want %s at http://media.test/", view.Launch, firstEndpoint)
	}

	// The workload is renamed. The entry names the application, so nothing it
	// resolved changes.
	renameWorkload(t, cluster)
	if view := resolve(t, cluster, stored); view.Launch.Endpoint != firstEndpoint || !view.IsAvailable {
		t.Fatalf("after renaming the workload launch = %+v, want the unchanged %s", view.Launch, firstEndpoint)
	}

	// The endpoint is renamed to an address of its own: the entry keeps its
	// identity (the stored object is untouched) and the address follows.
	renameEndpoint(t, cluster)
	after := entries(t, cluster)
	untouched, err := after.Get(context.Background(), entryName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the entry after the renames: %v", err)
	}
	if untouched.GetResourceVersion() != stored.GetResourceVersion() {
		t.Fatalf("the entry was rewritten by the renames: version %s -> %s",
			stored.GetResourceVersion(), untouched.GetResourceVersion())
	}
	if view := resolve(t, cluster, untouched); !view.IsAvailable || view.Launch.Endpoint != secondEndpoint || view.Launch.Address != "http://media-v2.test/" {
		t.Fatalf("after renaming the endpoint launch = %+v, want %s at http://media-v2.test/", view.Launch, secondEndpoint)
	}
	renamed, err := dock.SpecOf(untouched)
	if err != nil {
		t.Fatalf("decode the entry after the renames: %v", err)
	}
	if renamed.Application != applicationName || renamed.Visibility != dock.VisibilityEveryone || renamed.Order != 5 {
		t.Fatalf("entry after the renames = %+v, want the same application with the adjusted visibility and order", renamed)
	}

	// An unavailable application keeps its entry on the desktop and states why
	// the entry cannot open (C-30).
	failApplication(t, cluster, applicationName)
	if view := resolve(t, cluster, untouched); view.IsAvailable || !strings.Contains(view.Reason, app.PhaseFailed) {
		t.Fatalf("unavailable entry = %+v, want the entry kept with the %s phase in its reason", view, app.PhaseFailed)
	}
}

// entry builds one entry of the application opened through the first endpoint.
func entry(visibility string, order int) map[string]any {
	return map[string]any{
		"apiVersion": dock.APIGroup + "/" + dock.EntryVersion,
		"kind":       dock.EntryKind,
		"metadata":   map[string]any{"name": entryName, "namespace": harness.Namespace},
		"spec": map[string]any{
			"application": applicationName,
			"displayName": "媒体库",
			"icon":        "media",
			"launch":      firstEndpoint,
			"visibility":  visibility,
			"order":       order,
			"source":      "manual",
		},
	}
}

// entriesPath addresses the entry class through the control plane.
func entriesPath() string {
	return "/api/v1/resources/" + dock.EntryKind + "?namespace=" + harness.Namespace
}

// adjust rewrites the entry's visibility and order through the API, behind the
// version precondition every update carries.
func adjust(t *testing.T, shell *harness.Shell, token string) {
	t.Helper()
	stored, err := entries(t, shell.Cluster).Get(context.Background(), entryName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the entry before adjusting it: %v", err)
	}
	headers := harness.Bearer(token)
	headers["If-Match"] = stored.GetResourceVersion()
	status, body := shell.Accounts.HTTP.Call(t, http.MethodPut,
		"/api/v1/resources/"+dock.EntryKind+"/"+entryName+"?apiVersion="+dock.APIGroup+"/"+dock.EntryVersion+"&namespace="+harness.Namespace,
		entry(dock.VisibilityEveryone, 5), headers)
	if status != http.StatusAccepted {
		t.Fatalf("adjust the entry = %d %v", status, body)
	}
}

// resolve reads the entry against the stored application and the endpoints of
// its namespace.
func resolve(t *testing.T, cluster *harness.Cluster, entry *unstructured.Unstructured) dock.View {
	t.Helper()
	application := cluster.Get(t, "v1beta1", "Application", applicationName)
	endpoints := cluster.List(t, "v1beta1", "Endpoint")
	view, err := dock.Resolve(entry, application, endpoints)
	if err != nil {
		t.Fatalf("resolve the entry: %v", err)
	}
	return view
}

// renameWorkload replaces the application's workload with one under another
// name, as an external rename would.
func renameWorkload(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": secondWorkload, "namespace": harness.Namespace},
		"spec": map[string]any{
			"replicas": 2,
			"selector": map[string]any{"matchLabels": map[string]any{"app": secondWorkload}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": secondWorkload}},
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": "media", "image": "example.test/media:1"},
				}},
			},
		},
	})
	if err := cluster.Kube.AppsV1().Deployments(harness.Namespace).Delete(
		context.Background(), firstWorkload, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the renamed workload: %v", err)
	}
}

// renameEndpoint replaces the application's endpoint with one under another
// name and another address.
func renameEndpoint(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	cluster.Create(t, harness.Endpoint(secondEndpoint, harness.ManagedLabels(), map[string]any{
		"kind":      string(v1beta1.EndpointKindHTTPHost),
		"targetApp": applicationName,
		"host":      "media-v2.test",
	}))
	resource := cluster.Direct(t, api.Group, "v1beta1", "Endpoint", harness.Namespace)
	if err := resource.Delete(context.Background(), firstEndpoint, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the renamed endpoint: %v", err)
	}
}

// failApplication rewrites the stored phase, so the case reads the entry of an
// application that cannot be opened.
func failApplication(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx := context.Background()
	stored, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the application: %v", err)
	}
	status, found, err := unstructured.NestedMap(stored.Object, "status")
	if err != nil || !found {
		t.Fatalf("application %s carries no status: found=%v err=%v", name, found, err)
	}
	status["phase"] = app.PhaseFailed
	if _, err := cluster.ApplyStatus(ctx, "v1beta1", "Application", harness.Namespace, name, status); err != nil {
		t.Fatalf("write the failed phase: %v", err)
	}
}

// entries resolves the entry class client.
func entries(t *testing.T, cluster *harness.Cluster) dynamic.ResourceInterface {
	t.Helper()
	return cluster.ExtensionResource(dock.APIGroup, dock.EntryVersion, dock.EntryResource, harness.Namespace)
}
