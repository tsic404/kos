// Package tc5 verifies that undeclared contributions stay invisible and that a
// class is readable only where its component is installed (AC-2, TC-5).
package tc5

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// widgetsGroup is the product's API group.
const widgetsGroup = "widgets.example.test"

// requestTimeout bounds every API call of the case.
const requestTimeout = 30 * time.Second

// declaration declares one object class and one page, nothing else.
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
			Pages: []extension.Page{{
				ID: "widgets", Title: "Widgets", Location: "panel",
				Items: []extension.PageItem{{
					ID: "widgets", Label: "Widgets", Source: extension.PageSourceLocal, Store: "widgets", Reason: "只读展示",
				}},
			}},
		},
	}
}

// TestTC5UndeclaredAndUninstalledContributionsAreAbsent reads contributions the
// declaration does not name and classes no component installs.
func TestTC5UndeclaredAndUninstalledContributionsAreAbsent(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Gadget", "gadgets", true)
	artifacts := extension.MemoryArtifacts{}

	// Not installed: the class, the page, the setting and the capability are all
	// absent even though the cluster serves the definitions.
	before := cluster.ExtensionState(t, artifacts)
	beforeAPI := cluster.StartControlPlane(t, withExtensions(before.Registry()))
	assertCapabilityMissing(t, beforeAPI, http.MethodGet, readPath(), nil)
	if len(before.Registry().Pages()) != 0 {
		t.Fatalf("pages before the install = %v", before.Registry().Pages())
	}
	if _, found := before.Registry().Setting("widgets-endpoint"); found {
		t.Fatal("a setting is visible before the install")
	}
	if _, found := before.Registry().Capability("demo.widgets/panel", "v1"); found {
		t.Fatal("a capability is registered before the install")
	}

	installed := cluster.InstallPackage(t, "widgets", declaration(), artifacts)
	registry := installed.Registry()
	afterAPI := cluster.StartControlPlane(t, withExtensions(registry))
	if status, body := afterAPI.Call(t, http.MethodGet, readPath(), nil, nil); status != http.StatusOK {
		t.Fatalf("read the declared class: status %d, body %v", status, body)
	}

	// The declaration names no setting and no capability, so neither is
	// readable.
	if _, found := registry.Setting("widgets-endpoint"); found {
		t.Fatal("an undeclared setting became visible")
	}
	if _, found := registry.Capability("demo.widgets/panel", "v1"); found {
		t.Fatal("an undeclared capability was registered")
	}

	// Writing a class the declaration does not name is refused and leaves no
	// object behind.
	manifest := map[string]any{
		"apiVersion": widgetsGroup + "/v1",
		"kind":       "Gadget",
		"metadata":   map[string]any{"name": "beta", "namespace": harness.Namespace},
		"spec":       map[string]any{"size": 1},
	}
	assertCapabilityMissing(t, afterAPI, http.MethodPost, "/api/v1/resources/Gadget?namespace="+harness.Namespace, manifest)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := cluster.ExtensionResource(widgetsGroup, "v1", "gadgets", harness.Namespace).Get(ctx, "beta", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a refused write created the object: %v", err)
	}
	assertCapabilityMissing(t, afterAPI, http.MethodGet,
		"/api/v1/resources/Gadget?apiVersion="+widgetsGroup+"/v1&namespace="+harness.Namespace, nil)
}

// readPath reads one page of the class the component declares.
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
