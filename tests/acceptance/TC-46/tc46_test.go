// Package tc46 verifies the private config boundary of F3: a value the Volume
// or Endpoint object owns is refused at write time with the conflicting field
// named, and neither object is touched (AC-23, TC-46).
package tc46

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tsic404/kos/pkg/app/builtin"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const targetNamespace = "apps"

func TestTC46PrivateConfigFieldBoundary(t *testing.T) {
	cluster := harness.Start(t)
	definition, err := builtin.Definition("nginx")
	if err != nil {
		t.Fatalf("load the nginx definition: %v", err)
	}
	cluster.InstallDefinition(t, definition)
	cluster.EnsureNamespace(t, targetNamespace)
	gateway := cluster.StartControlPlane(t, nil)

	// The endpoint is only the owner of the reserved config fields; it still
	// has to satisfy the endpoint shape C-43 requires.
	endpoint := harness.Manifest("Endpoint", harness.Namespace, "front", map[string]string{"managed-by": "kos-shell"}, nil)
	endpoint["spec"] = map[string]any{"kind": "http.host", "targetApp": "web", "host": "front.example.test"}
	for _, manifest := range []map[string]any{
		harness.Manifest("Volume", harness.Namespace, "data", map[string]string{"managed-by": "kos-shell"}, nil),
		endpoint,
	} {
		kind := harness.String(manifest, "kind")
		if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/"+kind, manifest, nil); status != http.StatusAccepted {
			t.Fatalf("create %s: status %d, body %v", kind, status, body)
		}
	}
	before := snapshot(t, cluster)

	cases := []struct {
		key   string
		owner string
	}{
		{"size", "Volume"},
		{"storagePolicy", "Volume"},
		{"hostname", "Endpoint"},
		{"port", "Endpoint"},
	}
	for index, collision := range cases {
		name := fmt.Sprintf("rejected-%d", index)
		body := harness.ApplicationManifest(name, harness.Namespace, "nginx", targetNamespace,
			map[string]any{collision.key: "value"}, nil)
		status, response := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", body, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400; body %v", collision.key, status, response)
		}
		if code := harness.String(response, "error", "code"); code != "invalid_field" {
			t.Errorf("%s: code %q, want invalid_field", collision.key, code)
		}
		message := harness.String(response, "error", "message")
		if !strings.Contains(message, collision.key) {
			t.Errorf("%s: the message does not name the field: %s", collision.key, message)
		}
		if !strings.Contains(message, collision.owner) {
			t.Errorf("%s: the message does not name the %s object: %s", collision.key, collision.owner, message)
		}
	}

	// An undeclared config key is refused by the same structure check; the four
	// write entries must agree on it, not just on the reserved keys.
	for index, key := range []string{"unknownKey", "unknown_field"} {
		body := harness.ApplicationManifest(fmt.Sprintf("declared-unknown-%d", index), harness.Namespace, "nginx", targetNamespace,
			map[string]any{key: "value"}, nil)
		status, response := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", body, nil)
		if status != http.StatusBadRequest {
			t.Fatalf("declared-unknown key %q: status %d, want 400; body %v", key, status, response)
		}
		if code := harness.String(response, "error", "code"); code != "invalid_field" {
			t.Errorf("declared-unknown key %q: code %q, want invalid_field", key, code)
		}
		if message := harness.String(response, "error", "message"); !strings.Contains(message, key) {
			t.Errorf("declared-unknown key %q: the message does not name the field: %s", key, message)
		}
	}

	if applications := cluster.List(t, "v1beta1", "Application"); len(applications) != 0 {
		t.Fatalf("a refused write created %d applications", len(applications))
	}
	after := snapshot(t, cluster)
	for key, value := range before {
		if after[key] != value {
			t.Errorf("%s changed from %q to %q", key, value, after[key])
		}
	}

	// The boundary is not create-only: an update that adds a reserved key is
	// refused with the same answer and the stored config stays as it was.
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest("update-target", harness.Namespace, "nginx", targetNamespace,
			map[string]any{"replicas": int64(1)}, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create a legal application: status %d, body %v", status, body)
	}
	path := "/api/v1/resources/Application/update-target?namespace=" + harness.Namespace
	update := harness.ApplicationManifest("update-target", harness.Namespace, "nginx", targetNamespace,
		map[string]any{"replicas": int64(1), "port": "8080"}, nil)
	precondition := map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Application", "update-target").GetResourceVersion()}
	status, response := gateway.Call(t, http.MethodPut, path, update, precondition)
	if status != http.StatusBadRequest {
		t.Fatalf("update with a reserved key: status %d, want 400; body %v", status, response)
	}
	if code := harness.String(response, "error", "code"); code != "invalid_field" {
		t.Errorf("update with a reserved key: code %q, want invalid_field", code)
	}
	if message := harness.String(response, "error", "message"); !strings.Contains(message, "port") || !strings.Contains(message, "Endpoint") {
		t.Errorf("update refusal does not name port and Endpoint: %s", message)
	}

	unknownUpdate := harness.ApplicationManifest("update-target", harness.Namespace, "nginx", targetNamespace,
		map[string]any{"replicas": int64(1), "unknownKey": "value"}, nil)
	status, response = gateway.Call(t, http.MethodPut, path, unknownUpdate,
		map[string]string{"If-Match": cluster.Get(t, "v1beta1", "Application", "update-target").GetResourceVersion()})
	if status != http.StatusBadRequest {
		t.Fatalf("update with an undeclared key: status %d, want 400; body %v", status, response)
	}
	if code := harness.String(response, "error", "code"); code != "invalid_field" {
		t.Errorf("update with an undeclared key: code %q, want invalid_field", code)
	}
	if message := harness.String(response, "error", "message"); !strings.Contains(message, "unknownKey") {
		t.Errorf("update refusal does not name the undeclared key: %s", message)
	}

	// The desired-set endpoints judge an Application the same way as a
	// single-object write, so neither plan nor apply is a way around F3 or the
	// config structure.
	for _, endpoint := range []string{"/api/v1/plan-requests", "/api/v1/apply-requests"} {
		for key, config := range map[string]map[string]any{
			"reserved":   {"hostname": "example.test"},
			"undeclared": {"unknownKey": "value"},
		} {
			status, response := gateway.Call(t, http.MethodPost, endpoint, map[string]any{"objects": []any{
				harness.ApplicationManifest("set-target", harness.Namespace, "nginx", targetNamespace, config, nil),
			}}, nil)
			if status != http.StatusBadRequest {
				t.Fatalf("%s with a %s key: status %d, want 400; body %v", endpoint, key, status, response)
			}
			if code := harness.String(response, "error", "code"); code != "invalid_field" {
				t.Errorf("%s with a %s key: code %q, want invalid_field", endpoint, key, code)
			}
		}
	}
}

// snapshot flattens the volume and endpoint the case must not disturb.
func snapshot(t *testing.T, cluster *harness.Cluster) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	for _, kind := range []string{"Volume", "Endpoint"} {
		for path, value := range harness.Flatten(cluster.Get(t, "v1beta1", kind, kindKey(kind)).Object) {
			fields[kind+"."+path] = value
		}
	}
	return fields
}

// kindKey maps the kind to the object name the case created.
func kindKey(kind string) string {
	if kind == "Volume" {
		return "data"
	}
	return "front"
}
