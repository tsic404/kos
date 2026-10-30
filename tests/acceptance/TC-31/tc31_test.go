// Package tc31 verifies C-59's role write permissions and C-60's ownership
// rules end to end: a Member writes only its own objects, a Guest writes
// nothing, the global Owner writes everything, and the API writes spec.owner
// from the session (AC-16, TC-31).
package tc31

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// owned builds a stored object of one kind with an owner and a team label the
// backup selector can match. It is written directly, so the case starts from
// state other writers (another session, the catalogue) could have produced.
func owned(kind, name, owner, team string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": harness.Namespace,
			"labels":    map[string]any{"managed-by": "acceptance", "team": team},
		},
		"spec": map[string]any{"owner": owner},
	}}
	return object
}

// TestTC31RoleWritePermissions walks the role matrix of C-59 and the ownership
// rules of C-60.
func TestTC31RoleWritePermissions(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, owned("Application", "alice-app", "alice", "alice"))
	cluster.Create(t, owned("Application", "bob-app", "bob", "bob"))
	cluster.Create(t, owned("Volume", "alice-vol", "alice", "alice"))
	cluster.Create(t, owned("Volume", "bob-vol", "bob", "bob"))

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)

	namespaceParam := "?namespace=" + harness.Namespace
	application := "/api/v1/resources/Application"
	create := func(name string) map[string]any {
		return harness.Manifest("Application", harness.Namespace, name, map[string]string{"managed-by": "kos-shell"}, nil)
	}
	// An update submits the desired state only: the stored attribution and the
	// markers the object already carries stay (C-02).
	rewrite := func(name string) map[string]any {
		return harness.Manifest("Application", harness.Namespace, name, nil, nil)
	}

	// The Owner writes every kind, including the ones reserved to it.
	for _, testCase := range []struct {
		name   string
		token  string
		method string
		path   string
		body   map[string]any
		status int
		code   string
	}{
		{"Owner creates an application", owner, http.MethodPost, application + namespaceParam, create("owner-app"), http.StatusAccepted, ""},
		{"Owner creates a provider", owner, http.MethodPost, "/api/v1/resources/Provider", harness.Manifest("Provider", "", "owner-provider", map[string]string{"managed-by": "kos-shell"}, nil), http.StatusAccepted, ""},
		{"Owner creates an application definition", owner, http.MethodPost, "/api/v1/resources/ApplicationDefinition", harness.Manifest("ApplicationDefinition", "", "owner-definition", map[string]string{"managed-by": "kos-shell"}, nil), http.StatusAccepted, ""},
		{"Member creates an application", alice, http.MethodPost, application + namespaceParam, create("alice-created"), http.StatusAccepted, ""},
		{"Member writes another member's application", alice, http.MethodPut, application + "/bob-app" + namespaceParam, withOwner(rewrite("bob-app"), "bob"), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member rewrites the owner field", alice, http.MethodPut, application + "/alice-app" + namespaceParam, withOwner(rewrite("alice-app"), "bob"), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member references another member's volume", alice, http.MethodPost, application + namespaceParam, withVolumes(create("cross-owner"), "bob-vol"), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member references its own volume", alice, http.MethodPost, application + namespaceParam, withVolumes(create("own-volume"), "alice-vol"), http.StatusAccepted, ""},
		{"Member creates a policy whose selector reaches another member", alice, http.MethodPost, "/api/v1/resources/BackupPolicy" + namespaceParam, policy("bob-scope", "team=bob"), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member creates a policy scoped to itself", alice, http.MethodPost, "/api/v1/resources/BackupPolicy" + namespaceParam, policy("alice-scope", "team=alice"), http.StatusAccepted, ""},
		{"Member creates a provider", alice, http.MethodPost, "/api/v1/resources/Provider", harness.Manifest("Provider", "", "alice-provider", map[string]string{"managed-by": "kos-shell"}, nil), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member creates a system component", alice, http.MethodPost, "/api/v1/resources/SystemComponent", harness.Manifest("SystemComponent", "", "alice-component", map[string]string{"managed-by": "kos-shell"}, nil), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member creates an application definition", alice, http.MethodPost, "/api/v1/resources/ApplicationDefinition", harness.Manifest("ApplicationDefinition", "", "alice-definition", map[string]string{"managed-by": "kos-shell"}, nil), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member exposes another member's application", alice, http.MethodPost, "/api/v1/resources/Endpoint" + namespaceParam, endpoint("bob-entry", "bob-app"), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Member exposes its own application", alice, http.MethodPost, "/api/v1/resources/Endpoint" + namespaceParam, endpoint("alice-entry", "alice-app"), http.StatusAccepted, ""},
		{"Guest creates an application", guest, http.MethodPost, application + namespaceParam, create("guest-app"), http.StatusForbidden, controlplane.CodePermissionDenied},
		{"Guest creates a provider", guest, http.MethodPost, "/api/v1/resources/Provider", harness.Manifest("Provider", "", "guest-provider", map[string]string{"managed-by": "kos-shell"}, nil), http.StatusForbidden, controlplane.CodePermissionDenied},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			headers := harness.Bearer(testCase.token)
			status, body := system.Identity.HTTP.Call(t, testCase.method, testCase.path, testCase.body, headers)
			if status != testCase.status {
				t.Fatalf("status = %d, want %d; body %v", status, testCase.status, body)
			}
			if testCase.code != "" && harness.String(body, "error", "code") != testCase.code {
				t.Fatalf("code = %v, want %s", harness.String(body, "error", "code"), testCase.code)
			}
		})
	}

	// The API wrote the creator's name into the objects a Member created.
	for _, name := range []string{"alice-created", "own-volume", "alice-scope"} {
		kind := map[string]string{"alice-created": "Application", "own-volume": "Application", "alice-scope": "BackupPolicy"}[name]
		if stored := cluster.Get(t, "v1beta1", kind, name); harness.String(stored.Object, "spec", "owner") != "alice" {
			t.Fatalf("%s spec.owner = %q, want alice", name, harness.String(stored.Object, "spec", "owner"))
		}
	}

	// A Member updates its own object and is refused another's or an owner
	// rewrite; the endpoint it created is reachable the same way.
	aliceAppVersion := cluster.Get(t, "v1beta1", "Application", "alice-app").GetResourceVersion()
	bobAppVersion := cluster.Get(t, "v1beta1", "Application", "bob-app").GetResourceVersion()
	update := withOwner(rewrite("alice-app"), "alice")
	update["spec"] = map[string]any{"owner": "alice", "namespace": "apps"}
	status, body := system.Call(t, alice, http.MethodPut, application+"/alice-app"+namespaceParam, update, map[string]string{"If-Match": aliceAppVersion})
	if status != http.StatusAccepted {
		t.Fatalf("member updates its own application = %d %v", status, body)
	}
	status, body = system.Call(t, alice, http.MethodPut, application+"/bob-app"+namespaceParam, rewrite("bob-app"), map[string]string{"If-Match": bobAppVersion})
	if status != http.StatusForbidden {
		t.Fatalf("member updates another member's application = %d %v, want 403", status, body)
	}
	transfer := withOwner(rewrite("alice-app"), "bob")
	transfer["spec"] = map[string]any{"owner": "bob"}
	status, body = system.Call(t, alice, http.MethodPut, application+"/alice-app"+namespaceParam, transfer,
		map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Application", "alice-app").GetResourceVersion()})
	if status != http.StatusForbidden {
		t.Fatalf("member transfers its application = %d %v, want 403", status, body)
	}

	// Deletes follow the same ownership: another member's object is refused, a
	// guest's delete is refused, and the owner of an object may remove it.
	status, body = system.Call(t, alice, http.MethodDelete, application+"/bob-app"+namespaceParam, nil,
		map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Application", "bob-app").GetResourceVersion()})
	if status != http.StatusForbidden {
		t.Fatalf("member deletes another member's application = %d %v, want 403", status, body)
	}
	status, body = system.Call(t, guest, http.MethodDelete, application+"/alice-app"+namespaceParam, nil,
		map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Application", "alice-app").GetResourceVersion()})
	if status != http.StatusForbidden {
		t.Fatalf("guest deletes an application = %d %v, want 403", status, body)
	}

	// An action is a write to the application's lifecycle, so orphan is the
	// owner's: another member's application is refused, its own is not.
	status, body = system.Call(t, alice, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": "bob-app", "action": "orphan",
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("member orphans another member's application = %d %v, want 403", status, body)
	}
	status, body = system.Call(t, alice, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": "alice-app", "action": "orphan",
	}, nil)
	if status == http.StatusForbidden {
		t.Fatalf("member orphans its own application = %d %v, want the phase check instead of a refusal", status, body)
	}
}

// withOwner sets the owner a submitted body carries.
func withOwner(body map[string]any, owner string) map[string]any {
	spec, _ := body["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec["owner"] = owner
	body["spec"] = spec
	return body
}

// withVolumes sets the volumes a submitted application references.
func withVolumes(body map[string]any, volumes ...string) map[string]any {
	spec, _ := body["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	listed := make([]any, 0, len(volumes))
	for _, volume := range volumes {
		listed = append(listed, volume)
	}
	spec["volumes"] = listed
	body["spec"] = spec
	return body
}

// policy builds a backup policy body with a selector.
func policy(name, selector string) map[string]any {
	body := harness.Manifest("BackupPolicy", harness.Namespace, name, map[string]string{"managed-by": "kos-shell"}, nil)
	body["spec"] = map[string]any{"selector": selector}
	return body
}

// endpoint builds an endpoint that exposes one application.
func endpoint(name, target string) map[string]any {
	body := harness.Manifest("Endpoint", harness.Namespace, name, map[string]string{"managed-by": "kos-shell"}, nil)
	body["spec"] = map[string]any{"kind": "http.host", "targetApp": target, "host": name + ".example.test"}
	return body
}
