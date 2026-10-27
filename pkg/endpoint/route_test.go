package endpoint

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
)

// endpointObject is a stored endpoint with the given UID and spec.
func endpointObject(name string, uid types.UID, spec v1beta1.EndpointSpec) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Endpoint",
		"metadata":   map[string]any{"name": name, "namespace": "default", "uid": string(uid)},
	}}
	object.Object["spec"] = specMap(spec)
	return object
}

// specMap renders one fixture spec as the map an object carries.
func specMap(spec v1beta1.EndpointSpec) map[string]any {
	raw := map[string]any{"kind": string(spec.Kind), "targetApp": spec.TargetApp}
	if spec.Host != "" {
		raw["host"] = spec.Host
	}
	if spec.Path != "" {
		raw["path"] = spec.Path
	}
	if spec.Port != 0 {
		raw["port"] = int64(spec.Port)
	}
	if spec.ServiceName != "" {
		raw["serviceName"] = spec.ServiceName
	}
	return raw
}

func TestIngressRouteCarriesTheHostPathAndBackend(t *testing.T) {
	spec := v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "site.test", Path: "/api"}
	endpoint := endpointObject("site", "uid-1", spec)
	ingress := ingressRoute(endpoint, spec, "/api")

	if ingress.GetAPIVersion() != "networking.k8s.io/v1" || ingress.GetKind() != RouteIngress {
		t.Fatalf("route identity = %s %s", ingress.GetAPIVersion(), ingress.GetKind())
	}
	if ingress.GetName() != "site" || ingress.GetNamespace() != "default" {
		t.Fatalf("route name = %s/%s", ingress.GetNamespace(), ingress.GetName())
	}
	if ingress.GetLabels()[markers.ManagedBy] != "kos-shell" || ingress.GetLabels()[markers.OwnerUID] != "uid-1" {
		t.Fatalf("route labels = %v, want attribution with the endpoint UID", ingress.GetLabels())
	}

	rule := firstOf(t, ingress.Object, "spec", "rules")
	if host := textOf(t, rule, "host"); host != "site.test" {
		t.Fatalf("host = %q", host)
	}
	path := firstOf(t, rule, "http", "paths")
	if got := textOf(t, path, "path"); got != "/api" {
		t.Fatalf("path = %q", got)
	}
	if got := textOf(t, path, "pathType"); got != "Prefix" {
		t.Fatalf("pathType = %q, want Prefix", got)
	}
	service := nestedOf(t, path, "backend", "service")
	if got := textOf(t, service, "name"); got != "web" {
		t.Fatalf("backend service = %q, want the target application", got)
	}
	if got := textOf(t, service, "port", "name"); got != "http" {
		t.Fatalf("backend port = %q, want http", got)
	}
}

func TestIngressRouteOfAHostEntryCoversTheWholeHost(t *testing.T) {
	spec := hostSpec()
	endpoint := endpointObject("site", "uid-2", spec)
	ingress := ingressRoute(endpoint, spec, "/")
	path := firstOf(t, firstOf(t, ingress.Object, "spec", "rules"), "http", "paths")
	if got := textOf(t, path, "path"); got != "/" {
		t.Fatalf("path = %q, want /", got)
	}
}

func TestServiceRouteSelectsTheTargetApplication(t *testing.T) {
	endpoint := endpointObject("game", "uid-3", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindUDP, TargetApp: "game", Port: 27015})
	service := serviceRoute(endpoint, 27015, "UDP", "app-uid")

	if service.GetKind() != RouteService || service.GetAPIVersion() != "v1" {
		t.Fatalf("route identity = %s %s", service.GetAPIVersion(), service.GetKind())
	}
	if got := textOf(t, service.Object, "spec", "type"); got != "LoadBalancer" {
		t.Fatalf("type = %q, want LoadBalancer", got)
	}
	if got := textOf(t, service.Object, "spec", "selector", markers.OwnerUID); got != "app-uid" {
		t.Fatalf("selector owner-uid = %q, want the target application UID", got)
	}
	port := firstOf(t, service.Object, "spec", "ports")
	if got := textOf(t, port, "protocol"); got != "UDP" {
		t.Fatalf("protocol = %q", got)
	}
	if got := number(t, port, "port"); got != 27015 {
		t.Fatalf("port = %v, want 27015", got)
	}
	if got := number(t, port, "targetPort"); got != 27015 {
		t.Fatalf("targetPort = %v, want 27015", got)
	}
	if got := textOf(t, service.Object, "metadata", "labels", markers.OwnerUID); got != "uid-3" {
		t.Fatalf("route owner-uid = %q, want the endpoint UID", got)
	}
}

// TestIngressRouteCarriesTheCertificateItServes states the TLS branch of the
// HTTP shapes: an entry that enables TLS names the secret C-46 observes, and an
// entry without TLS gets no tls section at all.
func TestIngressRouteCarriesTheCertificateItServes(t *testing.T) {
	spec := v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "site.test", TLS: &v1beta1.EndpointTLS{}}
	endpoint := endpointObject("site", "uid-5", spec)
	ingress := ingressRoute(endpoint, spec, "/")

	tls := firstOf(t, ingress.Object, "spec", "tls")
	if got := textOf(t, tls, "secretName"); got != CertificateSecretName("site") {
		t.Fatalf("secretName = %q, want %q", got, CertificateSecretName("site"))
	}
	hosts, ok := tls["hosts"].([]any)
	if !ok || len(hosts) != 1 || hosts[0] != "site.test" {
		t.Fatalf("hosts = %v, want the entry's host", tls["hosts"])
	}

	plain := endpointObject("plain", "uid-6", hostSpec())
	route := ingressRoute(plain, hostSpec(), "/")
	if _, found := route.Object["spec"].(map[string]any)["tls"]; found {
		t.Fatalf("an entry without TLS carries a tls section: %v", route.Object["spec"])
	}
}

func TestMulticastEntryMapsToNoObject(t *testing.T) {
	endpoint := endpointObject("cast", "uid-4", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", ServiceName: "cast", Port: 5353})
	spec, err := SpecOf(endpoint)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	document, err := (&Reconciler{}).routeDocument(context.Background(), endpoint, Entry{Spec: spec})
	if err != nil {
		t.Fatalf("mdns route: %v", err)
	}
	if document != nil {
		t.Fatalf("mdns produced %s", document.GetKind())
	}
}

// firstOf reads the first entry of a list below a path.
func firstOf(t *testing.T, object map[string]any, path ...string) map[string]any {
	t.Helper()
	value := nested(t, object, path...)
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("%v is not a non-empty list: %v", path, value)
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("%v[0] is not an object: %v", path, list[0])
	}
	return entry
}

// nestedOf reads a map below a path.
func nestedOf(t *testing.T, object map[string]any, path ...string) map[string]any {
	t.Helper()
	value := nested(t, object, path...)
	entry, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is not an object: %v", path, value)
	}
	return entry
}

// textOf reads a string below a path.
func textOf(t *testing.T, object map[string]any, path ...string) string {
	t.Helper()
	value, _ := nested(t, object, path...).(string)
	return value
}

// number reads an integer below a path.
func number(t *testing.T, object map[string]any, path ...string) int64 {
	t.Helper()
	switch value := nested(t, object, path...).(type) {
	case int64:
		return value
	case int32:
		return int64(value)
	case int:
		return int64(value)
	case float64:
		return int64(value)
	default:
		t.Fatalf("%v is not a number: %v", path, value)
		return 0
	}
}

// nested walks a chain of map keys.
func nested(t *testing.T, object map[string]any, path ...string) any {
	t.Helper()
	var value any = object
	for _, key := range path {
		parent, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("%v is not reachable below %v", path, key)
		}
		value = parent[key]
	}
	return value
}
