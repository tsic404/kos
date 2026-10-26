// Package tc118 verifies that a role change, a password change and a disable
// take effect on the next request, and that an account owning objects cannot be
// deleted until ownership moves (AC-58, TC-118).
package tc118

import (
	"context"
	"net/http"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName     = "owner"
	ownerPassword = "owner-password"
	alicePassword = "alice-password"
	aliceNext     = "alice-password-2"
)

// TestTC118RoleChangePasswordAndDelete runs the scenario against the account
// API over a real cluster, so ownership and the write path are the real ones.
func TestTC118RoleChangePasswordAndDelete(t *testing.T) {
	cluster := harness.Start(t)
	controlPlane := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Authorizer = identity.Authorizer{}
	})
	surface := harness.StartIdentity(t, controlPlane.Server.Handler(), identity.ClusterOwnedObjects{Dynamic: cluster.Dynamic})

	oneTime := surface.BootstrapOwner(t, ownerName)
	ownerToken, _ := surface.SignIn(t, ownerName, oneTime)
	surface.ChangePassword(t, ownerToken, oneTime, ownerPassword)

	aliceOneTime := createAccount(t, surface, ownerToken, "alice", "Member")
	aliceToken, _ := surface.SignIn(t, "alice", aliceOneTime)
	surface.ChangePassword(t, aliceToken, aliceOneTime, alicePassword)
	aliceSecond, _ := surface.SignIn(t, "alice", alicePassword)

	bobOneTime := createAccount(t, surface, ownerToken, "bob", "Member")
	bobToken, _ := surface.SignIn(t, "bob", bobOneTime)
	cluster.Create(t, ownedApplication("web", "bob"))
	cluster.Create(t, ownedVolume("data", "bob"))

	// The Owner demotes the signed-in Member; the next request is judged as a
	// Guest, which reads only.
	status, body := surface.HTTP.Call(t, http.MethodPut, "/api/v1/accounts/alice", map[string]any{
		"role": "Guest",
	}, bearer(ownerToken))
	if status != http.StatusOK {
		t.Fatalf("demote alice = %d %v", status, body)
	}
	status, body = surface.HTTP.Call(t, http.MethodPost,
		"/api/v1/resources/Application?namespace="+harness.Namespace,
		harness.Manifest("Application", harness.Namespace, "guest-write", nil, nil),
		bearer(aliceToken))
	if status != http.StatusForbidden {
		t.Fatalf("a Guest write = %d %v, want 403", status, body)
	}
	if code := body["error"].(map[string]any)["code"]; code != controlplane.CodePermissionDenied {
		t.Fatalf("Guest write code = %v, want %s", code, controlplane.CodePermissionDenied)
	}

	// A password change ends the account's other sessions and keeps the current
	// one usable.
	surface.ChangePassword(t, aliceToken, alicePassword, aliceNext)
	if status, body := surface.HTTP.Call(t, http.MethodGet, "/api/v1/auth/session", nil, bearer(aliceSecond)); status != http.StatusUnauthorized {
		t.Fatalf("the other session after the password change = %d %v, want 401", status, body)
	}
	if status, body := surface.HTTP.Call(t, http.MethodGet, "/api/v1/auth/session", nil, bearer(aliceToken)); status != http.StatusOK {
		t.Fatalf("the session that changed the password = %d %v, want 200", status, body)
	}

	// Disabling ends every session and refuses a new login.
	status, body = surface.HTTP.Call(t, http.MethodPut, "/api/v1/accounts/alice", map[string]any{
		"disabled": true,
	}, bearer(ownerToken))
	if status != http.StatusOK {
		t.Fatalf("disable alice = %d %v", status, body)
	}
	if status, body := surface.HTTP.Call(t, http.MethodGet, "/api/v1/auth/session", nil, bearer(aliceToken)); status != http.StatusUnauthorized {
		t.Fatalf("a session after disable = %d %v, want 401", status, body)
	}
	status, body = surface.HTTP.Call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "alice", "password": aliceNext,
	}, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("login after disable = %d %v, want 401", status, body)
	}

	// Bob still owns an application and a volume: the delete lists them and
	// waits for a transfer.
	status, body = surface.HTTP.Call(t, http.MethodDelete, "/api/v1/accounts/bob", nil, bearer(ownerToken))
	if status != http.StatusConflict {
		t.Fatalf("delete with owned objects = %d %v, want 409", status, body)
	}
	failure := body["error"].(map[string]any)
	if failure["code"] != controlplane.CodeAccountHasObjects {
		t.Fatalf("delete code = %v, want %s", failure["code"], controlplane.CodeAccountHasObjects)
	}
	objects, _ := failure["ownedObjects"].([]any)
	if len(objects) != 2 {
		t.Fatalf("ownedObjects = %v, want the application and the volume", failure["ownedObjects"])
	}
	transferOwner(t, cluster, "Application", "web", ownerName)
	transferOwner(t, cluster, "Volume", "data", ownerName)
	if status, body := surface.HTTP.Call(t, http.MethodDelete, "/api/v1/accounts/bob", nil, bearer(ownerToken)); status != http.StatusNoContent {
		t.Fatalf("delete after transfer = %d %v", status, body)
	}
	if status, body := surface.HTTP.Call(t, http.MethodGet, "/api/v1/auth/session", nil, bearer(bobToken)); status != http.StatusUnauthorized {
		t.Fatalf("the deleted account's session = %d %v, want 401", status, body)
	}
}

// createAccount creates an account and returns its one-time password.
func createAccount(t *testing.T, surface *harness.Identity, token, username, role string) string {
	t.Helper()
	status, body := surface.HTTP.Call(t, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": username, "role": role,
	}, bearer(token))
	if status != http.StatusCreated {
		t.Fatalf("create %s = %d %v", username, status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	if oneTime == "" {
		t.Fatalf("create %s returned no one-time password: %v", username, body)
	}
	return oneTime
}

// ownedApplication is an application whose spec.owner names the account.
func ownedApplication(name, owner string) *unstructured.Unstructured {
	object := harness.Application(name, nil)
	_ = unstructured.SetNestedField(object.Object, owner, "spec", "owner")
	return object
}

// ownedVolume is a volume whose spec.owner names the account.
func ownedVolume(name, owner string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Volume",
		"metadata": map[string]any{
			"name":      name,
			"namespace": harness.Namespace,
			"labels":    map[string]any{markers.ManagedBy: "acceptance"},
		},
		"spec": map[string]any{"owner": owner},
	}}
}

// transferOwner moves an object's spec.owner through the cluster client, which
// is the transfer C-60 describes.
func transferOwner(t *testing.T, cluster *harness.Cluster, kind, name, owner string) {
	t.Helper()
	ctx := context.Background()
	resource := cluster.Direct(t, api.Group, v1beta1.GroupVersion.Version, kind, harness.Namespace)
	object, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read %s %s: %v", kind, name, err)
	}
	if err := unstructured.SetNestedField(object.Object, owner, "spec", "owner"); err != nil {
		t.Fatalf("set owner of %s %s: %v", kind, name, err)
	}
	if _, err := resource.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("transfer %s %s: %v", kind, name, err)
	}
}

// bearer returns the header carrying a token.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}
