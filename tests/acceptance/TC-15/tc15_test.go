// Package tc15 verifies the retention semantics of preferences: an extension
// that declares retention keeps its preferences across a withdrawal, one that
// does not is deleted, a transient failure of the component keeps its data, and
// no cluster object is removed in either case (AC-7, TC-15).
package tc15

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName = "owner"
	ownerPass = "owner-password"

	keepExtension = "demo.keep"
	keepDomain    = "/api/v1/preferences/" + keepExtension
	dropExtension = "demo.drop"
	dropDomain    = "/api/v1/preferences/" + dropExtension
)

// keepDeclaration declares a preference schema whose data survives a
// withdrawal.
func keepDeclaration() extension.Declaration {
	return extension.Declaration{
		Identifier: keepExtension,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Preferences: []extension.PreferenceSchema{{ID: "keep", Keys: []string{"layout"}}},
		},
		Retention: []extension.Retention{{Resource: extension.ClassPreferences, Keep: true}},
	}
}

// dropDeclaration declares a preference schema with the default fate: the data
// is deleted with the component.
func dropDeclaration() extension.Declaration {
	return extension.Declaration{
		Identifier: dropExtension,
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Preferences: []extension.PreferenceSchema{{ID: "drop", Keys: []string{"layout"}}},
		},
	}
}

// TestTC15PreferencesFollowTheDeclaredRetention writes preferences for two
// extensions, withdraws both and reinstalls them.
func TestTC15PreferencesFollowTheDeclaredRetention(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, ownerName, ownerPass)
	artifacts := extension.MemoryArtifacts{}

	installation := cluster.InstallPackageWith(t, "keep", keepDeclaration(), artifacts, shell.Retention())
	shell.Use(installation)
	installation = cluster.InstallPackageWith(t, "drop", dropDeclaration(), artifacts, shell.Retention())
	shell.Use(installation)

	status, body := shell.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest("shell", harness.Namespace, "", harness.Namespace, nil, nil), token)
	if status != http.StatusAccepted {
		t.Fatalf("create the application = %d %v", status, body)
	}

	// Both browsers write through the same account, so each domain holds a
	// value before the withdrawal.
	for domain, value := range map[string]string{keepDomain: "kept-layout", dropDomain: "dropped-layout"} {
		status, body = shell.Call(t, http.MethodPut, domain+"/layout", map[string]any{"value": value}, token)
		if status != http.StatusNoContent {
			t.Fatalf("write %s = %d %v", domain, status, body)
		}
	}

	// Both components are withdrawn; the install pass applies the declared fate
	// to the preference data.
	cluster.DeleteComponent(t, "keep")
	cluster.DeleteComponent(t, "drop")
	shell.Use(cluster.ExtensionStateWith(t, artifacts, shell.Retention()))
	cluster.Get(t, "v1beta1", "Application", "shell")

	// Reinstalling both extensions shows what survived.
	installation = cluster.InstallPackageWith(t, "keep", keepDeclaration(), artifacts, shell.Retention())
	shell.Use(installation)
	installation = cluster.InstallPackageWith(t, "drop", dropDeclaration(), artifacts, shell.Retention())
	shell.Use(installation)

	status, body = shell.Call(t, http.MethodGet, keepDomain, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the kept domain = %d %v", status, body)
	}
	if layout := harness.String(body, "items", "layout"); layout != "kept-layout" {
		t.Fatalf("kept layout = %q, want the value kept by the declaration", layout)
	}
	status, body = shell.Call(t, http.MethodGet, dropDomain, nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the dropped domain = %d %v", status, body)
	}
	if layout := harness.String(body, "items", "layout"); layout != "" {
		t.Fatalf("dropped layout = %q, want the value deleted with the component", layout)
	}
	cluster.Get(t, "v1beta1", "Application", "shell")
}

// failingRetention is a preference store that cannot answer, the way a
// control-plane database outage looks to an install pass (C-61).
type failingRetention struct{}

// Retain implements extension.PreferenceRetention.
func (failingRetention) Retain(context.Context, string, string, bool) error {
	return errors.New("database unavailable")
}

// Withdraw implements extension.PreferenceRetention.
func (failingRetention) Withdraw(context.Context, map[string]string) error {
	return errors.New("database unavailable")
}

// TestTC15PreferenceStoreFailureDoesNotBlockTheInstallPass checks C-27 and
// C-61: a database that cannot record the declared fate must not stop the
// component from being installed and reconciled.
func TestTC15PreferenceStoreFailureDoesNotBlockTheInstallPass(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	artifacts := extension.MemoryArtifacts{}

	installation := cluster.InstallPackageWith(t, "drop", dropDeclaration(), artifacts, failingRetention{})
	shell.Use(installation)

	result, found := installation.Component("drop")
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v, want it ready despite the failed preference store", result)
	}
	if keys, ok := installation.Registry().PreferenceKeys(dropExtension); !ok || len(keys) != 1 {
		t.Fatalf("preference keys = %v/%v, want the loaded declaration kept", keys, ok)
	}
}

// breakComponent points the component named drop at a package the install pass
// cannot use, one failure mode at a time.
func breakComponent(t *testing.T, scenario string, cluster *harness.Cluster, artifacts extension.MemoryArtifacts) {
	t.Helper()
	switch scenario {
	case "package-unreadable":
		cluster.RepointComponent(t, "drop", "test://missing-artifact", extension.DigestOf(artifacts["test://drop"]))
	case "digest-mismatch":
		cluster.RepointComponent(t, "drop", "test://drop", "sha256:"+strings.Repeat("0", 64))
	case "invalid-declaration":
		content := []byte("this is not a declaration")
		artifacts["test://broken"] = content
		cluster.RepointComponent(t, "drop", "test://broken", extension.DigestOf(content))
	case "dependency-missing":
		dependent := dropDeclaration()
		dependent.Dependencies = extension.Dependencies{
			Capabilities: []extension.CapabilityRequirement{{Name: "demo.absent/api", Versions: "v1"}},
		}
		content, err := extension.Marshal(dependent)
		if err != nil {
			t.Fatalf("marshal the dependent declaration: %v", err)
		}
		artifacts["test://dependent"] = content
		cluster.RepointComponent(t, "drop", "test://dependent", extension.DigestOf(content))
	default:
		t.Fatalf("unknown scenario %q", scenario)
	}
}

// TestTC15TransientComponentFailureKeepsPreferences checks that only a component
// object that is gone withdraws its preference data (C-27, C-29): an unreadable
// package, a digest mismatch, an invalid declaration and an unmet dependency
// all leave the stored preferences in place.
func TestTC15TransientComponentFailureKeepsPreferences(t *testing.T) {
	for _, scenario := range []string{"package-unreadable", "digest-mismatch", "invalid-declaration", "dependency-missing"} {
		t.Run(scenario, func(t *testing.T) {
			cluster := harness.Start(t)
			shell := cluster.StartShell(t)
			token := shell.SignInOwner(t, ownerName, ownerPass)
			artifacts := extension.MemoryArtifacts{}

			installation := cluster.InstallPackageWith(t, "drop", dropDeclaration(), artifacts, shell.Retention())
			shell.Use(installation)
			status, body := shell.Call(t, http.MethodPut, dropDomain+"/layout", map[string]any{"value": "kept-through-failure"}, token)
			if status != http.StatusNoContent {
				t.Fatalf("write the preference = %d %v", status, body)
			}

			// The component stops contributing this round, but its object stays.
			breakComponent(t, scenario, cluster, artifacts)
			installation = cluster.ExtensionStateWith(t, artifacts, shell.Retention())
			shell.Use(installation)
			if result, found := installation.Component("drop"); !found || result.Phase == extension.PhaseReady {
				t.Fatalf("component = %+v, want a component that contributes nothing this round", result)
			}
			status, _ = shell.Call(t, http.MethodGet, dropDomain, nil, token)
			if status != http.StatusNotFound {
				t.Fatalf("read the domain of an inactive component = %d, want %d", status, http.StatusNotFound)
			}
			stored, err := shell.Accounts.Store.PreferencesOf(context.Background(), ownerName, dropExtension)
			if err != nil {
				t.Fatalf("read the stored preference: %v", err)
			}
			if stored["layout"] != "kept-through-failure" {
				t.Fatalf("stored preference = %v, want the value a transient failure must not delete", stored)
			}

			// Recovering the package makes the same data readable again.
			cluster.RepointComponent(t, "drop", "test://drop", extension.DigestOf(artifacts["test://drop"]))
			installation = cluster.ExtensionStateWith(t, artifacts, shell.Retention())
			shell.Use(installation)
			status, body = shell.Call(t, http.MethodGet, dropDomain, nil, token)
			if status != http.StatusOK {
				t.Fatalf("read the recovered domain = %d %v", status, body)
			}
			if layout := harness.String(body, "items", "layout"); layout != "kept-through-failure" {
				t.Fatalf("recovered layout = %q, want the preference the failure did not delete", layout)
			}
		})
	}
}

// TestTC15DeletionGateDecidesWhichComponentsAreLive checks the seam between the
// deletion gate and preference retention: a component that leaves the install
// set before its data is settled loses the domains it does not keep, while one
// the gate holds open stays installed and keeps them (C-27, C-29, C-48).
func TestTC15DeletionGateDecidesWhichComponentsAreLive(t *testing.T) {
	const foreignFinalizer = "acceptance.kos.tsic.top/hold"

	t.Run("left the install set", func(t *testing.T) {
		cluster := harness.Start(t)
		shell := cluster.StartShell(t)
		token := shell.SignInOwner(t, ownerName, ownerPass)
		artifacts := extension.MemoryArtifacts{}

		installation := cluster.InstallPackageWith(t, "drop", dropDeclaration(), artifacts, shell.Retention())
		shell.Use(installation)
		status, body := shell.Call(t, http.MethodPut, dropDomain+"/layout", map[string]any{"value": "gone-with-the-component"}, token)
		if status != http.StatusNoContent {
			t.Fatalf("write the preference = %d %v", status, body)
		}

		// The deletion starts before the gate ever observes the component, so
		// the gate cannot hold it and the install pass skips it. A foreign
		// finalizer keeps the object visible for that pass.
		object := cluster.Get(t, "v1beta1", "SystemComponent", "drop")
		object.SetFinalizers([]string{foreignFinalizer})
		if _, err := cluster.Direct(t, api.Group, v1beta1.GroupVersion.Version, "SystemComponent", "").Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("replace the gate finalizer: %v", err)
		}
		cluster.DeleteComponent(t, "drop")

		installation = cluster.ExtensionStateWith(t, artifacts, shell.Retention())
		shell.Use(installation)
		if _, present := installation.Component("drop"); present {
			t.Fatal("a component the gate cannot hold stayed in the install set")
		}
		stored, err := shell.Accounts.Store.PreferencesOf(context.Background(), ownerName, dropExtension)
		if err != nil {
			t.Fatalf("read the stored preference: %v", err)
		}
		if len(stored) != 0 {
			t.Fatalf("stored preference = %v, want the data deleted with the component", stored)
		}
	})

	t.Run("held open", func(t *testing.T) {
		cluster := harness.Start(t)
		shell := cluster.StartShell(t)
		token := shell.SignInOwner(t, ownerName, ownerPass)
		artifacts := extension.MemoryArtifacts{}

		installation := cluster.InstallPackageWith(t, "drop", dropDeclaration(), artifacts, shell.Retention())
		shell.Use(installation)
		status, body := shell.Call(t, http.MethodPut, dropDomain+"/layout", map[string]any{"value": "kept-while-held"}, token)
		if status != http.StatusNoContent {
			t.Fatalf("write the preference = %d %v", status, body)
		}

		cluster.Create(t, harness.Application("web", nil))
		cluster.Create(t, harness.Endpoint("held-site", harness.ManagedLabels(), map[string]any{
			"kind": "http.host", "targetApp": "web", "host": "held.test",
			"tls": map[string]any{"issuerRef": "drop"},
		}))
		cluster.DeleteComponent(t, "drop")

		installation = cluster.ExtensionStateWith(t, artifacts, shell.Retention())
		shell.Use(installation)
		result, present := installation.Component("drop")
		if !present || result.Phase != extension.PhaseReady {
			t.Fatalf("component = %+v/%v, want the held component still installed", result, present)
		}
		stored, err := shell.Accounts.Store.PreferencesOf(context.Background(), ownerName, dropExtension)
		if err != nil {
			t.Fatalf("read the stored preference: %v", err)
		}
		if stored["layout"] != "kept-while-held" {
			t.Fatalf("stored preference = %v, want the data of a held component kept", stored)
		}
	})
}
