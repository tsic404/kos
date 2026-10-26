// Package tc47 verifies that an annotation outside the marker keys changes
// nothing: adding and removing it leaves phase, health and the rendered objects
// identical (AC-23, TC-47, F15).
package tc47

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/app/builtin"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	applicationName = "web"
	// unknownAnnotation is outside the marker keys and carries no meaning (C-02).
	unknownAnnotation = "example.test/unrelated"
)

func TestTC47UnknownAnnotationChangesNothing(t *testing.T) {
	cluster := harness.Start(t)
	definition, err := builtin.Definition("nginx")
	if err != nil {
		t.Fatalf("load the nginx definition: %v", err)
	}
	cluster.InstallDefinition(t, definition)
	cluster.EnsureNamespace(t, targetNamespace)
	gateway := cluster.StartControlPlane(t, nil)
	if status, body := gateway.Call(t, "POST", "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, "nginx", targetNamespace, nil, nil), nil); status != 202 {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	if status, body := gateway.Call(t, "POST", "/api/v1/resources/Volume",
		harness.Manifest("Volume", harness.Namespace, "data", map[string]string{"managed-by": "kos-shell"}, nil), nil); status != 202 {
		t.Fatalf("create volume: status %d, body %v", status, body)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	install(t, cluster, controller)
	baseline := observe(t, cluster)

	annotate(t, cluster, "Application", harness.Namespace, applicationName, unknownAnnotation, "one")
	annotate(t, cluster, "Volume", harness.Namespace, "data", unknownAnnotation, "one")
	round(t, controller)
	withAnnotation := observe(t, cluster)

	annotate(t, cluster, "Application", harness.Namespace, applicationName, unknownAnnotation, "")
	annotate(t, cluster, "Volume", harness.Namespace, "data", unknownAnnotation, "")
	round(t, controller)
	withoutAnnotation := observe(t, cluster)

	for name, observed := range map[string]map[string]string{
		"with the annotation":    withAnnotation,
		"without the annotation": withoutAnnotation,
	} {
		for path, value := range baseline {
			if observed[path] != value {
				t.Errorf("%s: %s = %q, want %q", name, path, observed[path], value)
			}
		}
		// A field that only appears after the annotation is just as much a
		// behavior change as one that disappears; compare both directions.
		for path := range observed {
			if _, found := baseline[path]; !found {
				t.Errorf("%s: %s appeared outside the baseline", name, path)
			}
		}
	}
}

// install drives rounds until the application reaches Ready.
func install(t *testing.T, cluster *harness.Cluster, controller *app.Controller) {
	t.Helper()
	for range 5 {
		request(t, controller)
		phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
		if err != nil {
			t.Fatalf("read the phase: %v", err)
		}
		if phase == app.PhaseReady {
			return
		}
	}
	t.Fatal("the application did not reach Ready")
}

// round drives one reconcile round.
func round(t *testing.T, controller *app.Controller) {
	t.Helper()
	request(t, controller)
}

// request reconciles the case's application once.
func request(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// observe captures the phase, health and rendered objects of the application.
func observe(t *testing.T, cluster *harness.Cluster) map[string]string {
	t.Helper()
	fields := make(map[string]string)
	application := cluster.Get(t, "v1beta1", "Application", applicationName)
	for path, value := range harness.Flatten(application.Object) {
		if strings.HasPrefix(path, "metadata.annotations") || path == "metadata.resourceVersion" {
			continue
		}
		if strings.HasPrefix(path, "status.conditions") {
			continue
		}
		fields["Application."+path] = value
	}
	for _, kind := range []string{"Deployment", "Service"} {
		group, version := "apps", "v1"
		if kind == "Service" {
			group, version = "", "v1"
		}
		object, err := cluster.Direct(t, group, version, kind, targetNamespace).Get(context.Background(), applicationName, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("read the rendered %s: %v", kind, err)
		}
		for path, value := range harness.Flatten(object.Object) {
			if path == "metadata.resourceVersion" || path == "metadata.generation" {
				continue
			}
			fields[kind+"."+path] = value
		}
	}
	volume := cluster.Get(t, "v1beta1", "Volume", "data")
	for path, value := range harness.Flatten(volume.Object) {
		if strings.HasPrefix(path, "metadata.annotations") || path == "metadata.resourceVersion" {
			continue
		}
		fields["Volume."+path] = value
	}
	return fields
}

// annotate sets or removes one annotation directly, as a client outside KOS
// would.
func annotate(t *testing.T, cluster *harness.Cluster, kind, namespace, name, key, value string) {
	t.Helper()
	resource := cluster.Direct(t, api.Group, "v1beta1", kind, namespace)
	object, err := resource.Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read %s %s: %v", kind, name, err)
	}
	annotations := object.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if value == "" {
		delete(annotations, key)
	} else {
		annotations[key] = value
	}
	object.SetAnnotations(annotations)
	if _, err := resource.Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("annotate %s %s: %v", kind, name, err)
	}
}
