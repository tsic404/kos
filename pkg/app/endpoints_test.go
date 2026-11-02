package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// TestValidateDefaultsAcceptsShapes covers the declarations a package may
// carry: volume names and the five entry forms, each with the fields C-43
// allows of it (C-32).
func TestValidateDefaultsAcceptsShapes(t *testing.T) {
	spec := v1beta1.ApplicationDefinitionSpec{
		Volumes: []string{"data", "cache"},
		Endpoints: []v1beta1.EndpointDefault{
			{Name: "web", Kind: v1beta1.EndpointKindHTTPHost},
			{Name: "docs", Kind: v1beta1.EndpointKindHTTPPath, Path: "/docs"},
			{Name: "metrics", Kind: v1beta1.EndpointKindTCP, Port: 9090},
			{Name: "dns", Kind: v1beta1.EndpointKindMDNS, ServiceName: "app", Port: 5353},
			{Name: "syslog", Kind: v1beta1.EndpointKindUDP, Port: 514},
		},
	}
	if err := ValidateDefaults(spec); err != nil {
		t.Fatalf("valid defaults were refused: %v", err)
	}
}

// TestValidateDefaultsRefusesMalformedDeclarations covers the declarations
// C-32 cannot materialize: a name that is not a DNS label, a duplicate, and an
// entry whose fields its shape does not carry.
func TestValidateDefaultsRefusesMalformedDeclarations(t *testing.T) {
	cases := map[string]v1beta1.ApplicationDefinitionSpec{
		"volume name is not a label": {Volumes: []string{"Data"}},
		"volume declared twice":      {Volumes: []string{"data", "data"}},
		"entry name is not a label":  {Endpoints: []v1beta1.EndpointDefault{{Name: "Web", Kind: v1beta1.EndpointKindTCP, Port: 80}}},
		"entry declared twice": {
			Endpoints: []v1beta1.EndpointDefault{
				{Name: "metrics", Kind: v1beta1.EndpointKindTCP, Port: 9090},
				{Name: "metrics", Kind: v1beta1.EndpointKindUDP, Port: 9091},
			},
		},
		"http.host carries a path": {
			Endpoints: []v1beta1.EndpointDefault{{Name: "web", Kind: v1beta1.EndpointKindHTTPHost, Path: "/web"}},
		},
		"http.path carries no path": {
			Endpoints: []v1beta1.EndpointDefault{{Name: "docs", Kind: v1beta1.EndpointKindHTTPPath}},
		},
		"tcp carries no port": {
			Endpoints: []v1beta1.EndpointDefault{{Name: "metrics", Kind: v1beta1.EndpointKindTCP}},
		},
		"mdns carries no service name": {
			Endpoints: []v1beta1.EndpointDefault{{Name: "dns", Kind: v1beta1.EndpointKindMDNS, Port: 5353}},
		},
		"unknown kind": {
			Endpoints: []v1beta1.EndpointDefault{{Name: "odd", Kind: "smtp"}},
		},
		"port out of range": {
			Endpoints: []v1beta1.EndpointDefault{{Name: "metrics", Kind: v1beta1.EndpointKindTCP, Port: 70000}},
		},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateDefaults(spec)
			if err == nil {
				t.Fatal("the declaration was accepted")
			}
			var invalid *InvalidPackage
			if !errors.As(err, &invalid) {
				t.Fatalf("error %v is not an InvalidPackage", err)
			}
		})
	}
}

// TestCascadeEndpointCarriesTheShapeAndTheOwner covers one materialized entry:
// the name composes the application and the declaration, the spec carries the
// declaration's fields plus the user's hostname, and the attribution names the
// application (C-02, C-32).
func TestCascadeEndpointCarriesTheShapeAndTheOwner(t *testing.T) {
	application := &v1beta1.Application{}
	application.Name = "demo"
	application.Namespace = "default"
	application.UID = "uid-demo"
	application.Spec.EndpointHosts = map[string]string{"web": "demo.example.test"}
	declaration := v1beta1.EndpointDefault{Name: "web", Kind: v1beta1.EndpointKindHTTPPath, Path: "/ui"}

	document, err := cascadeEndpoint(application, declaration)
	if err != nil {
		t.Fatalf("render the cascade endpoint: %v", err)
	}
	if document.GetName() != "demo-web" {
		t.Fatalf("name = %q, want demo-web", document.GetName())
	}
	if document.GetNamespace() != "default" {
		t.Fatalf("namespace = %q, want default", document.GetNamespace())
	}
	if got := document.GetLabels()["owner-uid"]; got != "uid-demo" {
		t.Fatalf("owner-uid = %q, want uid-demo", got)
	}
	if host, _, _ := unstructured.NestedString(document.Object, "spec", "host"); host != "demo.example.test" {
		t.Fatalf("host = %q, want demo.example.test", host)
	}
	if path, _, _ := unstructured.NestedString(document.Object, "spec", "path"); path != "/ui" {
		t.Fatalf("path = %q, want /ui", path)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(document.Object, "status"); found {
		t.Fatal("a created endpoint carries a status")
	}
}

// TestCascadeEndpointRefusesAComposedNameThatIsNoLabel covers a declaration
// whose materialized name cannot exist.
func TestCascadeEndpointRefusesAComposedNameThatIsNoLabel(t *testing.T) {
	application := &v1beta1.Application{}
	application.Name = strings.Repeat("a", 60)
	application.Namespace = "default"
	declaration := v1beta1.EndpointDefault{Name: "web", Kind: v1beta1.EndpointKindTCP, Port: 80}
	if _, err := cascadeEndpoint(application, declaration); err == nil {
		t.Fatal("an over-long composed name was accepted")
	}
}

// TestMaterializeEndpointsJudgesAnExistingName covers the name collision the
// composition can produce: application a-b's entry web and application a's
// entry b-web both name a-b-web. A stored endpoint of another application, or a
// leftover of a previous instance of the same application, fails the install
// instead of being counted as materialized; the application's own entry and a
// user-created one are skipped (C-32).
func TestMaterializeEndpointsJudgesAnExistingName(t *testing.T) {
	application := &v1beta1.Application{}
	application.Name = "a"
	application.Namespace = "default"
	application.UID = "uid-a"
	definition := &v1beta1.ApplicationDefinition{}
	definition.Spec.Endpoints = []v1beta1.EndpointDefault{
		{Name: "b-web", Kind: v1beta1.EndpointKindMDNS, ServiceName: "app", Port: 5353},
	}
	cases := []struct {
		name    string
		stored  *unstructured.Unstructured
		wantErr bool
	}{
		{
			name:    "name belongs to another application",
			stored:  storedEndpoint("a-b-web", "a-b", "uid-b"),
			wantErr: true,
		},
		{
			name:    "name was left by a previous instance",
			stored:  storedEndpoint("a-b-web", "a", "uid-old"),
			wantErr: true,
		},
		{
			name:   "name is this application's own entry",
			stored: storedEndpoint("a-b-web", "a", "uid-a"),
		},
		{
			name:   "name is a user-created entry of this application",
			stored: storedEndpoint("a-b-web", "a", ""),
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := &maintenanceStore{objects: map[string]*unstructured.Unstructured{
				keyOf(write.RefOf(testCase.stored)): testCase.stored,
			}}
			controller := NewController(store, Options{})
			err := controller.materializeEndpoints(context.Background(), application, definition)
			if testCase.wantErr {
				var invalid *InvalidPackage
				if !errors.As(err, &invalid) {
					t.Fatalf("error = %v, want an InvalidPackage", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("an existing entry of this application was refused: %v", err)
			}
		})
	}
}

// storedEndpoint builds one endpoint object as the cluster stores it.
func storedEndpoint(name, target, owner string) *unstructured.Unstructured {
	labels := map[string]any{markers.ManagedBy: write.ManagedByValue}
	if owner != "" {
		labels[markers.OwnerUID] = owner
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Endpoint",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "default",
			"labels":    labels,
		},
		"spec": map[string]any{
			"kind":        string(v1beta1.EndpointKindMDNS),
			"targetApp":   target,
			"serviceName": "app",
			"port":        int64(5353),
		},
	}}
}
