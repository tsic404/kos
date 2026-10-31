// Package tc102 verifies AC-51's credential boundary: a browser session can
// read the page shell, the object API, settings and the catalogue, but obtains
// no cluster credentials and has no interface that writes native resources
// (TC-102).
package tc102

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// widgetsGroup is the product's own API group.
const widgetsGroup = "widgets.example.test"

// declaration declares one page so the shell has something to render; its item
// is browser-local, so the case exercises the shell without needing a domain
// reader.
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
					ID: "recent", Label: "Recent", Source: extension.PageSourceLocal, Store: "widgets.recent", Reason: "只读展示",
				}},
				Actions: []extension.Action{{
					ID: "install", Method: http.MethodPost, Path: "/api/v1/action-requests",
				}},
			}},
		},
	}
}

// TestTC102BrowserSessionGetsNoClusterCredentials inspects every surface the
// signed-in browser can reach and tries the native write paths.
func TestTC102BrowserSessionGetsNoClusterCredentials(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, widgetsGroup, "v1", "Widget", "widgets", true)
	shell := cluster.StartShell(t)
	installation := cluster.InstallPackage(t, "widgets", declaration(), extension.MemoryArtifacts{})
	shell.Use(installation)
	owner := shell.SignInOwner(t, "root", "root-password")

	// Every surface a browser session can read: the account session, the page
	// registry and one page, one kernel collection, settings, the catalogue and
	// the account list the Owner may see.
	surfaces := []string{
		"/api/v1/auth/session",
		"/api/v1/pages",
		"/api/v1/pages/widgets",
		"/api/v1/resources/Application?namespace=" + harness.Namespace,
		"/api/v1/settings",
		"/api/v1/catalogue/entries",
		"/api/v1/accounts",
	}
	for _, path := range surfaces {
		status, payload, err := shell.Accounts.HTTP.Raw(http.MethodGet, path, nil, harness.Bearer(owner))
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, status, payload)
		}
		assertNoClusterCredentials(t, path, payload)
	}

	// The page shell offers actions as control-plane write entries; none names an
	// external URL or a native resource path.
	status, payload, err := shell.Accounts.HTTP.Raw(http.MethodGet, "/api/v1/pages/widgets", nil, harness.Bearer(owner))
	if err != nil || status != http.StatusOK {
		t.Fatalf("GET page = %d %v (%v)", status, payload, err)
	}
	page := decode(t, payload)
	actions, _ := page["actions"].([]any)
	if len(actions) == 0 {
		t.Fatalf("page actions = %v, want the declared action", page["actions"])
	}
	for _, raw := range actions {
		action, _ := raw.(map[string]any)
		entry, _ := action["entry"].(map[string]any)
		method, _ := entry["method"].(string)
		path, _ := entry["path"].(string)
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodDelete:
		default:
			t.Fatalf("action %v method %q is not a control-plane write", action["id"], method)
		}
		if !strings.HasPrefix(path, "/api/v1/") {
			t.Fatalf("action %v path %q is not a control-plane API path", action["id"], path)
		}
	}

	// There is no proxy for native resources: a native kind is refused by the
	// one resource route, and no object is written.
	pod := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": "probe", "namespace": harness.Namespace},
		"spec": map[string]any{
			"containers": []any{map[string]any{"name": "probe", "image": "busybox"}},
		},
	}
	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "probe", "namespace": harness.Namespace},
		"spec":       map[string]any{},
	}
	nativeWrites := []struct {
		method string
		path   string
		body   map[string]any
	}{
		{http.MethodPost, "/api/v1/resources/Pod?namespace=" + harness.Namespace, pod},
		{http.MethodPut, "/api/v1/resources/Pod/probe?apiVersion=v1&namespace=" + harness.Namespace, pod},
		{http.MethodDelete, "/api/v1/resources/Pod/probe?apiVersion=v1&namespace=" + harness.Namespace, nil},
		{http.MethodPost, "/api/v1/resources/Deployment?apiVersion=apps/v1&namespace=" + harness.Namespace, deployment},
	}
	for _, write := range nativeWrites {
		status, body := shell.Call(t, write.method, write.path, write.body, owner)
		if status < http.StatusBadRequest || status >= http.StatusInternalServerError {
			t.Fatalf("%s %s = %d %v, want a client refusal", write.method, write.path, status, body)
		}
		if harness.String(body, "error", "code") == "" {
			t.Fatalf("%s %s refused without a stable error code: %v", write.method, write.path, body)
		}
	}
	if _, err := cluster.Kube.CoreV1().Pods(harness.Namespace).Get(t.Context(), "probe", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the browser wrote a native Pod: %v", err)
	}
	if _, err := cluster.Kube.AppsV1().Deployments(harness.Namespace).Get(t.Context(), "probe", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the browser wrote a native Deployment: %v", err)
	}

	// No raw or proxy route exists beside the domain API.
	for _, path := range []string{
		"/api/v1/proxy/namespaces/" + harness.Namespace + "/pods/probe",
		"/api/v1/raw/Pod/probe",
		"/api/v1/resources/Pod/probe/proxy",
	} {
		status, _ := shell.Call(t, http.MethodGet, path, nil, owner)
		if status != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want the catch-all 404", path, status)
		}
	}
}

// assertNoClusterCredentials fails when one browser-visible body carries a
// kubeconfig, a client certificate, a private key or a service-account token.
func assertNoClusterCredentials(t *testing.T, surface string, payload []byte) {
	t.Helper()
	lower := strings.ToLower(string(payload))
	for _, marker := range []string{
		"kubeconfig",
		"client-key-data",
		"client-certificate-data",
		"certificate-authority-data",
		"begin rsa private key",
		"begin ec private key",
		"begin private key",
		"begin openssh private key",
		"kubernetes.default.svc",
		"system:serviceaccount",
	} {
		if strings.Contains(lower, marker) {
			t.Fatalf("%s carries cluster credentials (%q): %s", surface, marker, payload)
		}
	}
}

// decode parses one JSON object answer.
func decode(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	decoded := map[string]any{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return decoded
}
