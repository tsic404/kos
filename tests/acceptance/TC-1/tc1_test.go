// Package tc1 verifies that installing a product package makes its declared
// object class and page appear outside the kernel group, and that removing the
// component withdraws them (AC-1, TC-1).
package tc1

import (
	"context"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// widgetsGroup is the product's own API group; it is never the kernel group.
const widgetsGroup = "widgets.example.test"

// requestTimeout bounds every API call of the case.
const requestTimeout = 30 * time.Second

// declaration is the product package the case installs.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   widgetsGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: widgetsGroup, Kind: "Widget", Resource: "widgets",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []extension.Controller{{
				ID: "widget", Manages: []extension.GroupKind{{Group: widgetsGroup, Kind: "Widget"}},
			}},
			Capabilities: []extension.Capability{{Name: "demo.widgets/panel", Version: "v1"}},
			Pages:        []extension.Page{{ID: "widgets", Title: "Widgets"}},
		},
	}
}

// TestTC1InstalledProductAppearsAndDisappears installs one product package,
// reads its contributions and removes it.
func TestTC1InstalledProductAppearsAndDisappears(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	artifacts := extension.MemoryArtifacts{}
	kindsBefore := api.Kinds()

	// The class definition exists in the cluster, but the framework serves it
	// only after a component declares it.
	before := cluster.ExtensionState(t, artifacts)
	if classes := before.Registry().ObjectClasses(); len(classes) != 0 {
		t.Fatalf("before the install the framework serves %v", classes)
	}
	if pages := before.Registry().Pages(); len(pages) != 0 {
		t.Fatalf("before the install the framework shows pages %v", pages)
	}
	beforeAPI := cluster.StartControlPlane(t, withExtensions(before.Registry()))
	assertCapabilityMissing(t, beforeAPI, http.MethodGet, readPath(), nil)

	installed := cluster.InstallPackage(t, "widgets", declaration(), artifacts)
	result, found := installed.Component("widgets")
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want ready", result, found)
	}
	classes := installed.Registry().ObjectClasses()
	wantClass := extension.GroupKind{Group: widgetsGroup, Kind: "Widget"}
	if len(classes) != 1 || classes[0].GroupKind() != wantClass {
		t.Fatalf("served classes = %v, want only %v", classes, wantClass)
	}
	if slices.Contains(api.Kinds(), "Widget") {
		t.Fatal("the product kind entered the kernel API group")
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
	if _, visible := installed.Registry().PageFor("widgets", write.RoleMember); !visible {
		t.Fatal("the declared page is not visible after the install")
	}

	afterAPI := cluster.StartControlPlane(t, withExtensions(installed.Registry()))
	if status, body := afterAPI.Call(t, http.MethodGet, readPath(), nil, nil); status != http.StatusOK {
		t.Fatalf("read the declared class: status %d, body %v", status, body)
	}
	manifest := map[string]any{
		"apiVersion": widgetsGroup + "/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "alpha", "namespace": harness.Namespace},
		"spec":       map[string]any{"size": 3},
	}
	if status, body := afterAPI.Call(t, http.MethodPost, "/api/v1/resources/Widget?namespace="+harness.Namespace, manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create a Widget: status %d, body %v", status, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	stored, err := cluster.ExtensionResource(widgetsGroup, "v1", "widgets", harness.Namespace).Get(ctx, "alpha", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the created Widget: %v", err)
	}
	if size, _, _ := unstructured.NestedInt64(stored.Object, "spec", "size"); size != 3 {
		t.Fatalf("stored spec.size = %d, want 3", size)
	}

	// Uninstall: without the component the class and the page are gone.
	cluster.DeleteComponent(t, "widgets")
	removed := cluster.ExtensionState(t, artifacts)
	if classes := removed.Registry().ObjectClasses(); len(classes) != 0 {
		t.Fatalf("after the uninstall the framework still serves %v", classes)
	}
	if pages := removed.Registry().Pages(); len(pages) != 0 {
		t.Fatalf("after the uninstall the framework still shows %v", pages)
	}
	goneAPI := cluster.StartControlPlane(t, withExtensions(removed.Registry()))
	assertCapabilityMissing(t, goneAPI, http.MethodGet, readPath(), nil)
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed after the uninstall: %v", api.Kinds())
	}
}

// readPath reads one page of the product class through the control plane.
func readPath() string {
	return "/api/v1/resources/Widget?apiVersion=" + widgetsGroup + "/v1&namespace=" + harness.Namespace
}

// withExtensions wires the control plane to one visibility projection.
func withExtensions(kinds controlplane.ExtensionKinds) func(*controlplane.Config) {
	return func(config *controlplane.Config) {
		config.Extensions = kinds
	}
}

// assertCapabilityMissing checks the stable answer for an unserved class.
func assertCapabilityMissing(t *testing.T, api *harness.ControlPlane, method, path string, body any) {
	t.Helper()
	status, response := api.Call(t, method, path, body, nil)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("%s %s: status %d, body %v", method, path, status, response)
	}
	if code := harness.String(response, "error", "code"); code != controlplane.CodeCapabilityMissing {
		t.Fatalf("%s %s: error code %q, want %s", method, path, code, controlplane.CodeCapabilityMissing)
	}
}
