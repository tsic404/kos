package dock

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
)

// entry builds one stored entry.
func entry(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": APIGroup + "/" + EntryVersion,
		"kind":       EntryKind,
		"metadata":   map[string]any{"name": "desktop", "namespace": "apps"},
		"spec":       spec,
	}}
}

// application builds one stored application with a phase.
func application(name, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "apps"},
		"status":     map[string]any{"phase": phase},
	}}
}

// endpointObject builds one stored endpoint.
func endpointObject(name, shape string, fields map[string]any) *unstructured.Unstructured {
	spec := map[string]any{"kind": shape, "targetApp": "media"}
	for key, value := range fields {
		spec[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Endpoint",
		"metadata":   map[string]any{"name": name, "namespace": "apps"},
		"spec":       spec,
	}}
}

func TestSpecOfDefaults(t *testing.T) {
	spec, err := SpecOf(entry(map[string]any{"application": "media", "order": int64(4)}))
	if err != nil {
		t.Fatalf("decode the entry: %v", err)
	}
	if spec.DisplayName != "media" || spec.Visibility != VisibilityEveryone || spec.Order != 4 {
		t.Fatalf("spec = %+v, want the application name, everyone and order 4", spec)
	}
	if _, err := SpecOf(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "empty"}}}); err == nil {
		t.Fatal("an entry without a spec decoded")
	}
}

func TestResolveAddressesEveryShape(t *testing.T) {
	cases := []struct {
		name   string
		shape  string
		fields map[string]any
		want   string
	}{
		{"http.host", string(v1beta1.EndpointKindHTTPHost), map[string]any{"host": "media.test"}, "http://media.test/"},
		{"http.host with TLS", string(v1beta1.EndpointKindHTTPHost), map[string]any{"host": "media.test", "tls": map[string]any{}}, "https://media.test/"},
		{"http.path", string(v1beta1.EndpointKindHTTPPath), map[string]any{"host": "media.test", "path": "/media"}, "http://media.test/media"},
		{"l4.tcp", string(v1beta1.EndpointKindTCP), map[string]any{"port": int64(5432)}, "tcp://media:5432"},
		{"l4.udp", string(v1beta1.EndpointKindUDP), map[string]any{"port": int64(5353)}, "udp://media:5353"},
		{"mdns", string(v1beta1.EndpointKindMDNS), map[string]any{"serviceName": "media.local"}, "mdns://media.local"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			view, err := Resolve(entry(map[string]any{"application": "media"}),
				application("media", "Ready"), []*unstructured.Unstructured{endpointObject("media-entry", testCase.shape, testCase.fields)})
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if !view.IsAvailable || view.Launch.Address != testCase.want {
				t.Fatalf("view = %+v, want %s", view, testCase.want)
			}
		})
	}
}

func TestResolveKeepsTheApplicationIdentity(t *testing.T) {
	endpoints := []*unstructured.Unstructured{
		endpointObject("b-entry", string(v1beta1.EndpointKindHTTPHost), map[string]any{"host": "b.test"}),
		endpointObject("a-entry", string(v1beta1.EndpointKindHTTPHost), map[string]any{"host": "a.test"}),
	}
	// The first endpoint in name order opens the entry, and a named launch picks
	// its own endpoint out of the set.
	view, err := Resolve(entry(map[string]any{"application": "media"}), application("media", "Upgrading"), endpoints)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if view.Launch.Endpoint != "a-entry" || view.Launch.Address != "http://a.test/" {
		t.Fatalf("default launch = %+v, want the first endpoint", view.Launch)
	}
	view, err = Resolve(entry(map[string]any{"application": "media", "launch": "b-entry"}), application("media", "Ready"), endpoints)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if view.Launch.Endpoint != "b-entry" {
		t.Fatalf("named launch = %+v, want b-entry", view.Launch)
	}
	// A launch name the application no longer serves falls back to the endpoints
	// it does serve, so renaming an endpoint never strands the entry.
	view, err = Resolve(entry(map[string]any{"application": "media", "launch": "gone"}), application("media", "Ready"), endpoints)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if view.Launch.Endpoint != "a-entry" {
		t.Fatalf("fallen-back launch = %+v, want a-entry", view.Launch)
	}
}

func TestResolveKeepsAnUnavailableEntry(t *testing.T) {
	entries := entry(map[string]any{"application": "media"})
	cases := []struct {
		name        string
		application *unstructured.Unstructured
		endpoints   []*unstructured.Unstructured
		wantReason  string
	}{
		{"no application", nil, nil, "未安装"},
		{"another application", application("other", "Ready"), nil, "未安装"},
		{"deleting", deleting(application("media", "Ready")), nil, "正在卸载"},
		{"installing", application("media", "Installing"), nil, "Installing"},
		{"no endpoint", application("media", "Ready"), nil, "没有可用入口"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			view, err := Resolve(entries, testCase.application, testCase.endpoints)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if view.IsAvailable || !strings.Contains(view.Reason, testCase.wantReason) {
				t.Fatalf("view = %+v, want an unavailable entry stating %q", view, testCase.wantReason)
			}
		})
	}
}

// deleting marks one application as under deletion.
func deleting(object *unstructured.Unstructured) *unstructured.Unstructured {
	now := metav1.Now()
	object.SetDeletionTimestamp(&now)
	return object
}
