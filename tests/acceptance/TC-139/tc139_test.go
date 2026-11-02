// Package tc139 verifies that an installation writes the package's declared
// volume names into the application's volume set and its entry declarations
// into cascade endpoints (AC-67, TC-139).
package tc139

import (
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	appName         = "defaulted"
)

// volumeTemplate renders one Deployment whose claim reference comes from the
// declared volume set, so a declared volume name is observable on the workload.
const volumeTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: app
          image: example.test/app:1
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: {{ index .Volumes "data" }}
`

// declaredDefaults is what the package declares: two volume names and three
// entry forms. The HTTP form carries no hostname, because a package never does
// (C-32); the user gives it with the application.
func declaredDefaults() ([]string, []v1beta1.EndpointDefault) {
	return []string{"data", "cache"}, []v1beta1.EndpointDefault{
		{Name: "web", Kind: v1beta1.EndpointKindHTTPHost},
		{Name: "metrics", Kind: v1beta1.EndpointKindTCP, Port: 9090},
		{Name: "discovery", Kind: v1beta1.EndpointKindMDNS, ServiceName: "defaulted", Port: 5353},
	}
}

func TestTC139InstallMaterializesPackageDefaults(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	volumes, endpoints := declaredDefaults()
	cluster.InstallDefinition(t, harness.DefinitionWithDefaults(t, appName, volumeTemplate, volumes, endpoints))

	gateway := cluster.StartControlPlane(t, nil)
	manifest := harness.ApplicationManifest(appName, harness.Namespace, appName, targetNamespace, nil, []string{"extra"})
	manifest["spec"].(map[string]any)["endpointHosts"] = map[string]any{"web": "defaulted.example.test"}
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create %s: status %d, body %v", appName, status, body)
	}

	// The installation wrote the declared names into the spec, behind the name
	// the user wrote (C-32).
	if got := volumeNames(t, cluster, appName); strings.Join(got, ",") != "extra,data,cache" {
		t.Fatalf("spec.volumes = %v, want extra,data,cache", got)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, appName, 5); phase != app.PhaseReady {
		t.Fatalf("phase = %q, want %q; status %+v", phase, app.PhaseReady, cluster.ApplicationStatusOf(t, appName))
	}

	application := cluster.Get(t, "v1beta1", "Application", appName)
	ownerUID := string(application.GetUID())

	web := cluster.Get(t, "v1beta1", "Endpoint", "defaulted-web")
	assertEndpoint(t, web, v1beta1.EndpointKindHTTPHost, "defaulted.example.test", "", 0, "")
	metrics := cluster.Get(t, "v1beta1", "Endpoint", "defaulted-metrics")
	assertEndpoint(t, metrics, v1beta1.EndpointKindTCP, "", "", 9090, "")
	discovery := cluster.Get(t, "v1beta1", "Endpoint", "defaulted-discovery")
	assertEndpoint(t, discovery, v1beta1.EndpointKindMDNS, "", "", 5353, "defaulted")
	for _, endpoint := range []*unstructured.Unstructured{web, metrics, discovery} {
		if target, _, _ := unstructured.NestedString(endpoint.Object, "spec", "targetApp"); target != appName {
			t.Fatalf("endpoint %s targets %q, want %q", endpoint.GetName(), target, appName)
		}
		if endpoint.GetLabels()[markers.ManagedBy] == "" {
			t.Fatalf("endpoint %s carries no attribution", endpoint.GetName())
		}
		if endpoint.GetLabels()[markers.OwnerUID] != ownerUID {
			t.Fatalf("endpoint %s owner-uid = %q, want the application UID %q",
				endpoint.GetName(), endpoint.GetLabels()[markers.OwnerUID], ownerUID)
		}
	}

	// The installation never creates the volumes it names: capacity, mobility
	// and the storage class stay user decisions (C-32).
	if created := cluster.List(t, "v1beta1", "Volume"); len(created) != 0 {
		t.Fatalf("the install created volumes: %v", created)
	}

	// A hostname keyed to an entry the package does not declare is refused
	// before anything is written (C-32).
	unknown := harness.ApplicationManifest("unknown-entry", harness.Namespace, appName, targetNamespace, nil, nil)
	unknown["spec"].(map[string]any)["endpointHosts"] = map[string]any{"missing": "missing.example.test"}
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", unknown, nil); status != http.StatusBadRequest {
		t.Fatalf("unknown entry hostname: status %d, body %v", status, body)
	}
}

// assertEndpoint checks the shape fields one materialized entry carries.
func assertEndpoint(t *testing.T, endpoint *unstructured.Unstructured, kind v1beta1.EndpointKind, host, path string, port int32, serviceName string) {
	t.Helper()
	if got, _, _ := unstructured.NestedString(endpoint.Object, "spec", "kind"); got != string(kind) {
		t.Fatalf("endpoint %s kind = %q, want %s", endpoint.GetName(), got, kind)
	}
	if got, _, _ := unstructured.NestedString(endpoint.Object, "spec", "host"); got != host {
		t.Fatalf("endpoint %s host = %q, want %q", endpoint.GetName(), got, host)
	}
	if got, _, _ := unstructured.NestedString(endpoint.Object, "spec", "path"); got != path {
		t.Fatalf("endpoint %s path = %q, want %q", endpoint.GetName(), got, path)
	}
	if got, _, _ := unstructured.NestedString(endpoint.Object, "spec", "serviceName"); got != serviceName {
		t.Fatalf("endpoint %s serviceName = %q, want %q", endpoint.GetName(), got, serviceName)
	}
	if port == 0 {
		if _, present := endpoint.Object["spec"].(map[string]any)["port"]; present {
			t.Fatalf("endpoint %s carries a port it should not", endpoint.GetName())
		}
		return
	}
	if got, _, _ := unstructured.NestedInt64(endpoint.Object, "spec", "port"); got != int64(port) {
		t.Fatalf("endpoint %s port = %d, want %d", endpoint.GetName(), got, port)
	}
}

// volumeNames reads the application's stored volume set.
func volumeNames(t *testing.T, cluster *harness.Cluster, name string) []string {
	t.Helper()
	names, _, err := unstructured.NestedStringSlice(cluster.Get(t, "v1beta1", "Application", name).Object, "spec", "volumes")
	if err != nil {
		t.Fatalf("read spec.volumes of %s: %v", name, err)
	}
	return names
}

// deployTemplate renders one Deployment that names no claim, so an entry-only
// package can be installed.
const deployTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: app
          image: example.test/app:1
`

// TestTC139NameCollisionRefusesTheInstall covers the name composition the
// materialized entry uses: application a-b's entry web and application a's entry
// b-web both name a-b-web. The second installation fails and names the
// collision instead of counting the other application's entry as its own
// (AC-67, TC-139).
func TestTC139NameCollisionRefusesTheInstall(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.InstallDefinition(t, harness.DefinitionWithDefaults(t, "outer", deployTemplate, nil,
		[]v1beta1.EndpointDefault{{Name: "web", Kind: v1beta1.EndpointKindTCP, Port: 9090}}))
	cluster.InstallDefinition(t, harness.DefinitionWithDefaults(t, "inner", deployTemplate, nil,
		[]v1beta1.EndpointDefault{{Name: "b-web", Kind: v1beta1.EndpointKindTCP, Port: 9091}}))

	gateway := cluster.StartControlPlane(t, nil)
	for _, installed := range []struct{ name, definition string }{{"a-b", "outer"}, {"a", "inner"}} {
		manifest := harness.ApplicationManifest(installed.name, harness.Namespace, installed.definition, targetNamespace, nil, nil)
		if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
			t.Fatalf("create %s: status %d, body %v", installed.name, status, body)
		}
	}

	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, "a-b", 5); phase != app.PhaseReady {
		t.Fatalf("first application phase = %q, want %q", phase, app.PhaseReady)
	}
	if target, _, _ := unstructured.NestedString(cluster.Get(t, "v1beta1", "Endpoint", "a-b-web").Object, "spec", "targetApp"); target != "a-b" {
		t.Fatalf("a-b-web targets %q, want a-b", target)
	}

	// The second application composes the same name: the install fails and the
	// reason names the endpoint (C-32).
	if phase := cluster.SettleApplication(t, controller, "a", 5); phase != app.PhaseFailed {
		t.Fatalf("second application phase = %q, want %q", phase, app.PhaseFailed)
	}
	condition, present := state.Find(harness.Conditions(t, cluster.Get(t, "v1beta1", "Application", "a")), app.ConditionInvalidPackage)
	if !present || condition.Status != metav1.ConditionTrue {
		t.Fatalf("the invalid-package condition is missing or not True: %+v", condition)
	}
	if !strings.Contains(condition.Message, "a-b-web") {
		t.Fatalf("the refusal does not name the colliding endpoint: %s", condition.Message)
	}
	if target, _, _ := unstructured.NestedString(cluster.Get(t, "v1beta1", "Endpoint", "a-b-web").Object, "spec", "targetApp"); target != "a-b" {
		t.Fatalf("the refused install changed the first application's entry: %q", target)
	}
}
