// Package tc28 verifies that submitting one write request twice produces one
// object and the same reference (AC-15, TC-28).
package tc28

import (
	"net/http"
	"testing"
	"time"

	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC28RepeatedUpdateAndDeleteConverge submits one update body twice and one
// delete twice: a repeat must converge on the same state instead of failing or
// acting again (C-12).
func TestTC28RepeatedUpdateAndDeleteConverge(t *testing.T) {
	cluster := harness.Start(t)
	gateway := cluster.StartControlPlane(t, nil)
	path := "/api/v1/resources/Application/worker?namespace=" + harness.Namespace

	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.Manifest("Application", harness.Namespace, "worker", map[string]string{"managed-by": "kos-shell"}, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create: status %d, body %v", status, body)
	}
	update := harness.Manifest("Application", harness.Namespace, "worker",
		map[string]string{"managed-by": "kos-shell"}, map[string]string{"example.test/version": "v2"})
	precondition := map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Application", "worker").GetResourceVersion()}

	if status, body := gateway.Call(t, http.MethodPut, path, update, precondition); status != http.StatusAccepted {
		t.Fatalf("first update: status %d, body %v", status, body)
	}
	status, second := gateway.Call(t, http.MethodPut, path, update, precondition)
	if status != http.StatusAccepted {
		t.Fatalf("repeated update: status %d, body %v", status, second)
	}
	if idempotent, _ := second["idempotent"].(bool); !idempotent {
		t.Fatalf("the repeated update must report the desired state was already in place: %v", second)
	}
	updated := cluster.Get(t, "v1beta1", "Application", "worker")
	if updated.GetAnnotations()["example.test/version"] != "v2" {
		t.Fatalf("annotations = %v, want the submitted version", updated.GetAnnotations())
	}

	deletePrecondition := map[string]string{"If-Match": updated.GetResourceVersion()}
	if status, body := gateway.Call(t, http.MethodDelete, path, nil, deletePrecondition); status != http.StatusAccepted {
		t.Fatalf("first delete: status %d, body %v", status, body)
	}
	harness.Eventually(t, 30*time.Second, "the deleted application to be gone", func() bool {
		return len(cluster.List(t, "v1beta1", "Application")) == 0
	})
	status, repeated := gateway.Call(t, http.MethodDelete, path, nil, deletePrecondition)
	if status != http.StatusAccepted {
		t.Fatalf("repeated delete: status %d, body %v", status, repeated)
	}
	if idempotent, _ := repeated["idempotent"].(bool); !idempotent {
		t.Fatalf("the repeated delete must report the object was already gone: %v", repeated)
	}
}

// TestTC28RepeatedCreateProducesOneObject submits the same application body
// twice and compares the two answers with the cluster.
func TestTC28RepeatedCreateProducesOneObject(t *testing.T) {
	cluster := harness.Start(t)
	gateway := cluster.StartControlPlane(t, nil)
	manifest := harness.Manifest("Application", harness.Namespace, "web",
		map[string]string{"managed-by": "kos-shell"}, map[string]string{"example.test/note": "idempotency"})

	firstStatus, first := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil)
	if firstStatus != http.StatusAccepted {
		t.Fatalf("first create: status %d, body %v", firstStatus, first)
	}
	if accepted, _ := first["accepted"].(bool); !accepted {
		t.Fatalf("a write must answer with acceptance: %v", first)
	}
	firstRef, ok := first["ref"].(map[string]any)
	if !ok {
		t.Fatalf("a write must answer with an object reference: %v", first)
	}
	created := cluster.Get(t, "v1beta1", "Application", "web")

	secondStatus, second := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil)
	if secondStatus != http.StatusAccepted {
		t.Fatalf("second create: status %d, body %v", secondStatus, second)
	}
	secondRef, ok := second["ref"].(map[string]any)
	if !ok {
		t.Fatalf("the repeated write must answer with an object reference: %v", second)
	}
	if harness.Reference(firstRef) != harness.Reference(secondRef) {
		t.Fatalf("the two answers name different objects: %v vs %v", firstRef, secondRef)
	}
	if idempotent, _ := second["idempotent"].(bool); !idempotent {
		t.Fatalf("the repeated write must report that the desired state was already in place: %v", second)
	}

	applications := cluster.List(t, "v1beta1", "Application")
	if len(applications) != 1 {
		names := make([]string, 0, len(applications))
		for _, application := range applications {
			names = append(names, application.GetName())
		}
		t.Fatalf("applications = %v, want exactly one", names)
	}
	if version := applications[0].GetResourceVersion(); version != created.GetResourceVersion() {
		t.Fatalf("the repeated request rewrote the object: resourceVersion %s, want %s",
			version, created.GetResourceVersion())
	}
}
