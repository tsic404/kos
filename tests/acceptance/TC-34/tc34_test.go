// Package tc34 verifies C-59's rule for extension objects: an extension page's
// write is judged by the same role rules as any other write, so a Guest gets
// the same refusal and the object is untouched; the declared writer roles of
// C-18 are what a Member gets (AC-16, TC-34).
package tc34

import (
	"fmt"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const group = "example.test"

// declaration declares two extension classes: widgets, which a Member may
// write, and gadgets, which no permission entry names.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   group,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{
				{Group: group, Kind: "Widget", Resource: "widgets", Versions: []string{"v1"}, Namespaced: true},
				{Group: group, Kind: "Gadget", Resource: "gadgets", Versions: []string{"v1"}, Namespaced: true},
			},
			Pages: []extension.Page{{ID: "widgets", Title: "Widgets", RequiredRole: string(write.RoleMember)}},
		},
		Permissions: []extension.Permission{{
			Contribution:  "Widget",
			RequiredRole:  string(write.RoleMember),
			ObjectWriters: []write.Role{write.RoleOwner, write.RoleMember},
		}},
	}
}

// object builds one extension object body.
func object(kind, name string) map[string]any {
	return map[string]any{
		"apiVersion": group + "/v1",
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"size": "small"},
	}
}

// TestTC34ExtensionPageWriteFollowsRoles drives one extension class through the
// API as each role.
func TestTC34ExtensionPageWriteFollowsRoles(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, group, "v1", "Widget", "widgets", true)
	cluster.InstallCRD(t, group, "v1", "Gadget", "gadgets", true)
	installation := cluster.InstallPackage(t, "demo", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	system := cluster.StartAccess(t, registry, func(config *controlplane.Config) {
		config.Extensions = registry
	})
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)

	path := "/api/v1/resources/%s?namespace=" + harness.Namespace

	// A declared writer role may write the class: the Member's widget is stored.
	status, body := system.Call(t, alice, http.MethodPost, fmt.Sprintf(path, "Widget"), object("Widget", "member-widget"), nil)
	if status != http.StatusAccepted {
		t.Fatalf("member writes a declared class = %d %v, want 202", status, body)
	}
	widgets := cluster.ExtensionResource(group, "v1", "widgets", harness.Namespace)
	stored, err := widgets.Get(t.Context(), "member-widget", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the stored widget: %v", err)
	}

	// A Guest gets the same refusal a kernel write gets, field for field.
	kernelStatus, kernelBody := system.Call(t, guest, http.MethodPost,
		"/api/v1/resources/Application?namespace="+harness.Namespace,
		harness.Manifest("Application", harness.Namespace, "guest-app", nil, nil), nil)
	extensionStatus, extensionBody := system.Call(t, guest, http.MethodPost, fmt.Sprintf(path, "Widget"), object("Widget", "guest-widget"), nil)
	if extensionStatus != http.StatusForbidden || kernelStatus != http.StatusForbidden {
		t.Fatalf("guest writes: kernel %d %v, extension %d %v", kernelStatus, kernelBody, extensionStatus, extensionBody)
	}
	for _, field := range []string{"code", "stage", "sideEffects"} {
		if harness.String(extensionBody, "error", field) != harness.String(kernelBody, "error", field) {
			t.Fatalf("extension refusal %s = %v, kernel refusal %s = %v",
				field, harness.String(extensionBody, "error", field), field, harness.String(kernelBody, "error", field))
		}
	}

	// The refusal leaves the stored widget untouched.
	status, body = system.Call(t, guest, http.MethodPut, "/api/v1/resources/Widget/member-widget?namespace="+harness.Namespace, object("Widget", "member-widget"),
		map[string]string{"If-Match": stored.GetResourceVersion()})
	if status != http.StatusForbidden {
		t.Fatalf("guest updates the widget = %d %v, want 403", status, body)
	}
	after, err := widgets.Get(t.Context(), "member-widget", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reread the widget: %v", err)
	}
	if after.GetResourceVersion() != stored.GetResourceVersion() {
		t.Fatalf("the refused write changed the widget: %s -> %s", stored.GetResourceVersion(), after.GetResourceVersion())
	}

	// A class no declaration names writers for is the Owner's alone (C-59).
	status, body = system.Call(t, alice, http.MethodPost, fmt.Sprintf(path, "Gadget"), object("Gadget", "member-gadget"), nil)
	if status != http.StatusForbidden {
		t.Fatalf("member writes an undeclared-writer class = %d %v, want 403", status, body)
	}
	status, body = system.Call(t, owner, http.MethodPost, fmt.Sprintf(path, "Gadget"), object("Gadget", "owner-gadget"), nil)
	if status != http.StatusAccepted {
		t.Fatalf("Owner writes an undeclared-writer class = %d %v, want 202", status, body)
	}
}
