// Package tc33 verifies C-60's ownership transfer: only the global Owner moves
// spec.owner, the former owner loses management, and the new owner gains it
// (AC-16, TC-33).
package tc33

import (
	"fmt"
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// application builds a stored application with a fixed owner.
func application(name, owner string) *unstructured.Unstructured {
	object := harness.Application(name, nil)
	object.Object["spec"] = map[string]any{"owner": owner}
	return object
}

// TestTC33OwnerTransfer moves one application and refuses the same move from a
// member.
func TestTC33OwnerTransfer(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, application("first", "alice"))
	cluster.Create(t, application("second", "alice"))

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	bob := system.Account(t, owner, "bob", "bob-password", write.RoleMember)

	path := "/api/v1/resources/Application/%s?namespace=" + harness.Namespace
	transfer := func(token, name, to string) (int, map[string]any) {
		body := harness.Manifest("Application", harness.Namespace, name, nil, nil)
		body["spec"] = map[string]any{"owner": to}
		version := cluster.Get(t, "v1beta1", "Application", name).GetResourceVersion()
		return system.Call(t, token, http.MethodPut, fmt.Sprintf(path, name), body, map[string]string{"If-Match": version})
	}

	// The global Owner transfers alice's first application to bob.
	if status, body := transfer(owner, "first", "bob"); status != http.StatusAccepted {
		t.Fatalf("Owner transfer = %d %v", status, body)
	}
	if got := harness.String(cluster.Get(t, "v1beta1", "Application", "first").Object, "spec", "owner"); got != "bob" {
		t.Fatalf("spec.owner after transfer = %q, want bob", got)
	}

	// Alice lost management: every later write of hers is refused.
	status, body := transfer(alice, "first", "alice")
	if status != http.StatusForbidden {
		t.Fatalf("the former owner's write = %d %v, want 403", status, body)
	}

	// Bob gained it: the same write he was refused before is accepted now.
	if status, body := transfer(bob, "first", "bob"); status != http.StatusAccepted {
		t.Fatalf("the new owner's write = %d %v", status, body)
	}

	// A member cannot transfer its own application either.
	if status, body := transfer(alice, "second", "bob"); status != http.StatusForbidden {
		t.Fatalf("a member transfer = %d %v, want 403", status, body)
	}
	if got := harness.String(cluster.Get(t, "v1beta1", "Application", "second").Object, "spec", "owner"); got != "alice" {
		t.Fatalf("spec.owner after a refused transfer = %q, want alice", got)
	}
}
