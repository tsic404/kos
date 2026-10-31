// Package tc61 verifies that window interaction stays inside the browser:
// opening two windows of one application, minimizing one and closing them all
// changes no cluster object, the application keeps its phase and replica count,
// and the window layout and default size the account kept read back in another
// browser of the same account (AC-30, TC-61).
package tc61

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/dock"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/wm"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	applicationName = "media"
	definitionName  = "media-template"
	ownerName       = "owner"
	ownerPassword   = "owner-password"
)

// mediaTemplate renders the workload the application owns, so the case can
// watch the replica count and the downstream objects across every close.
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

// TestTC61ClosingWindowsChangesNothing installs kos-dock and kos-wm, drives the
// browser window set and compares the cluster before and after.
func TestTC61ClosingWindowsChangesNothing(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallDefinition(t, harness.DefinitionWithTemplate(t, definitionName, mediaTemplate, nil))
	cluster.CreateApplication(t, applicationName, definitionName, harness.Namespace)
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, applicationName, 6); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", phase, app.PhaseReady)
	}

	// The desktop package and the window package both install; only the window
	// preferences matter to this case.
	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackage(t, "dock", dock.Declaration(), artifacts)
	installation := cluster.InstallPackage(t, "wm", wm.Declaration(), artifacts)
	result, found := installation.Component("wm")
	if !found || result.Phase != extension.PhaseReady || len(result.Problems) > 0 {
		t.Fatalf("kos-wm component = %+v (found %v), want Ready with no contribution problem", result, found)
	}
	shell := cluster.StartShell(t)
	shell.Use(installation)
	owner := shell.SignInOwner(t, ownerName, ownerPassword)
	otherBrowser, _ := shell.Accounts.SignIn(t, ownerName, ownerPassword)

	if keys, found := installation.Registry().PreferenceKeys(wm.Identifier); !found || len(keys) != 2 {
		t.Fatalf("kos-wm preference keys = %v (found %v), want the layout and default size", keys, found)
	}

	// The account keeps a default window size and reads it in another browser.
	size := wm.Size{Width: 1024, Height: 640}
	writePreference(t, shell, owner, wm.PreferenceDefaultSize, encode(t, size))
	storedSize := readPreference(t, shell, otherBrowser, wm.PreferenceDefaultSize)
	if decoded, err := wm.DecodeSize(storedSize); err != nil || decoded != size {
		t.Fatalf("default size in another browser = %q (%v), want %+v", storedSize, err, size)
	}

	desktop := wm.New(size)
	first := desktop.Open(applicationName)
	second := desktop.Open(applicationName)
	if desktop.Count() != 2 || first.Instance != 1 || second.Instance != 2 {
		t.Fatalf("open windows = %+v, want two instances of the application", desktop.Windows())
	}
	if !desktop.Minimize(first.ID) || desktop.Count() != 2 {
		t.Fatalf("minimize = %+v, want the window minimized and still open", desktop.Windows())
	}

	// The layout the browser holds is the account preference, so another
	// browser of the same account lays the windows out the same way.
	layout := desktop.Layout()
	writePreference(t, shell, owner, wm.PreferenceLayout, encode(t, layout))
	storedLayout := readPreference(t, shell, otherBrowser, wm.PreferenceLayout)
	decoded, err := wm.DecodeLayout(storedLayout)
	if err != nil {
		t.Fatalf("decode the layout in another browser: %v", err)
	}
	if !sameLayout(decoded, layout) {
		t.Fatalf("layout in another browser = %+v, want %+v", decoded, layout)
	}

	// Every close leaves the application and its downstream objects alone.
	before := clusterState(t, cluster)
	if !desktop.Close(first.ID) || desktop.Count() != 1 {
		t.Fatalf("close one window = %+v, want one window left", desktop.Windows())
	}
	if current := clusterState(t, cluster); current != before {
		t.Fatalf("cluster after closing one window = %q, want %q", current, before)
	}
	if closed := desktop.CloseAll(); closed != 1 || desktop.Count() != 0 {
		t.Fatalf("close all = %d (left %d), want the one remaining window closed", closed, desktop.Count())
	}
	if current := clusterState(t, cluster); current != before {
		t.Fatalf("cluster after closing every window = %q, want %q", current, before)
	}
}

// writePreference stores one kos-wm preference through the API.
func writePreference(t *testing.T, shell *harness.Shell, token, key, value string) {
	t.Helper()
	status, body := shell.Call(t, http.MethodPut, preferencePath(key), map[string]any{"value": value}, token)
	if status != http.StatusNoContent {
		t.Fatalf("write preference %s = %d %v", key, status, body)
	}
}

// readPreference reads one kos-wm preference.
func readPreference(t *testing.T, shell *harness.Shell, token, key string) string {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, preferencePath(""), nil, token)
	if status != http.StatusOK {
		t.Fatalf("read preferences = %d %v", status, body)
	}
	value := harness.String(body, "items", key)
	if value == "" {
		t.Fatalf("preference %s is absent from %v", key, body)
	}
	return value
}

// preferencePath addresses the kos-wm preference domain, optionally one key.
func preferencePath(key string) string {
	if key == "" {
		return "/api/v1/preferences/" + wm.Identifier
	}
	return "/api/v1/preferences/" + wm.Identifier + "/" + key
}

// encode renders one preference structure.
func encode(t *testing.T, value interface{ Encode() (string, error) }) string {
	t.Helper()
	encoded, err := value.Encode()
	if err != nil {
		t.Fatalf("encode the preference: %v", err)
	}
	return encoded
}

// sameLayout compares two layouts by their placement lists.
func sameLayout(left, right wm.Layout) bool {
	if len(left.Windows) != len(right.Windows) {
		return false
	}
	for index := range left.Windows {
		if left.Windows[index] != right.Windows[index] {
			return false
		}
	}
	return true
}

// clusterState renders the phase, the application version and every workload
// as one string, so any write the window interaction made shows up.
func clusterState(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	application := cluster.Get(t, "v1beta1", "Application", applicationName)
	phase := harness.String(application.Object, "status", "phase")
	list, err := cluster.Kube.AppsV1().Deployments(harness.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	state := phase + "/" + application.GetResourceVersion()
	for index := range list.Items {
		deployment := &list.Items[index]
		replicas := int32(0)
		if deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		state += ";" + deployment.Name + "/" + string(deployment.UID) + "/" +
			deployment.ResourceVersion + "/" + strconv.Itoa(int(replicas))
	}
	return state
}
