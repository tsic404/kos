// Package tc11 verifies that discovery only reads, that adoption is the
// explicit action which writes attribution and an immutable snapshot, and that
// one object carries every adapter that reported it (AC-5, TC-11).
package tc11

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/adoption"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/discovery"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC11DiscoveryDoesNotAdopt discovers an existing workload and an existing
// endpoint, reads them without confirmation, then adopts the endpoint.
func TestTC11DiscoveryDoesNotAdopt(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	ctx := context.Background()

	deployment := existingDeployment()
	created, err := cluster.Direct(t, "apps", "v1", "Deployment", harness.Namespace).Create(ctx, deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create the existing workload: %v", err)
	}
	deployment = created
	endpoint := existingEndpoint()
	cluster.Create(t, endpoint)
	before := cluster.Get(t, "v1beta1", "Endpoint", endpoint.GetName())

	runner := &discovery.Runner{
		Dynamic: cluster.Dynamic,
		Resources: discovery.Resources{
			{Group: "apps", Kind: "Deployment"}: {Group: "apps", Version: "v1", Resource: "deployments"},
			{Group: api.Group, Kind: "Endpoint"}: {
				Group: api.Group, Version: "v1beta1", Resource: "endpoints",
			},
		},
		Adapters: []discovery.Adapter{
			{
				ID:        "cluster-scan",
				Covers:    []schema.GroupKind{{Group: "apps", Kind: "Deployment"}, {Group: api.Group, Kind: "Endpoint"}},
				Scope:     discovery.Scope{Namespaces: []string{harness.Namespace}},
				Collector: "node-a",
				Fields:    []string{"metadata.labels", "spec"},
				IsActive:  true,
			},
			{
				ID:       "endpoint-scan",
				Covers:   []schema.GroupKind{{Group: api.Group, Kind: "Endpoint"}},
				Scope:    discovery.Scope{Namespaces: []string{harness.Namespace}},
				Fields:   []string{"spec"},
				IsActive: true,
			},
		},
	}
	reports, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("run discovery: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("discovery reported %d objects, want 3", len(reports))
	}
	endpointReports := reportsFor(reports, "Endpoint", endpoint.GetName())
	if len(endpointReports) != 2 {
		t.Fatalf("the endpoint has %d sources, want 2", len(endpointReports))
	}
	for _, report := range endpointReports {
		if report.Fields["spec"] == nil {
			t.Fatalf("adapter %s reported no spec field: %v", report.Adapter, report.Fields)
		}
		if report.Adapter == "cluster-scan" && report.Collector != "node-a" {
			t.Fatalf("adapter %s reported collector %q, want the declared identity", report.Adapter, report.Collector)
		}
	}

	index := discovery.NewIndex()
	index.Replace(reports)
	plane := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Discoveries = index
	})

	// Discovery is read-only: neither the endpoint nor the workload changed.
	assertUnadopted(t, cluster.Get(t, "v1beta1", "Endpoint", endpoint.GetName()))
	if after := cluster.Get(t, "v1beta1", "Endpoint", endpoint.GetName()); after.GetResourceVersion() != before.GetResourceVersion() {
		t.Fatalf("discovery changed the endpoint: resourceVersion %s -> %s", before.GetResourceVersion(), after.GetResourceVersion())
	}
	if got, want := specOf(t, cluster.Get(t, "v1beta1", "Endpoint", endpoint.GetName())), specOf(t, before); !reflect.DeepEqual(got, want) {
		t.Fatalf("discovery changed the endpoint spec: %v != %v", got, want)
	}
	if downstream := ingresses(t, cluster); downstream != 0 {
		t.Fatalf("discovery created %d downstream objects", downstream)
	}
	assertNoSourcesWritten(t, cluster, deployment)

	status, body := plane.Call(t, http.MethodGet,
		"/api/v1/resources/Endpoint/"+endpoint.GetName()+"?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the projection: status %d, body %v", status, body)
	}
	if got, want := sources(t, body), []string{"cluster-scan", "endpoint-scan"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("projection sources = %v, want %v", got, want)
	}

	// Adoption is the explicit action: it writes the attribution and the
	// snapshot, and the object keeps both discovery sources.
	actions := adoption.New(cluster.Writer())
	ref := write.Ref{
		Group: api.Group, Version: "v1beta1", Kind: "Endpoint",
		Namespace: harness.Namespace, Name: endpoint.GetName(),
	}
	session := write.Session{Username: "alice", Role: write.RoleOwner}
	adopted, err := actions.Adopt(ctx, session, ref, "pre-existing")
	if err != nil {
		t.Fatalf("adopt the endpoint: %v", err)
	}
	if got := adopted.GetLabels()[markers.AdoptedBy]; got != session.Username {
		t.Fatalf("adopted-by = %q, want %q", got, session.Username)
	}
	if got := adopted.GetLabels()[markers.AdoptionReason]; got != "pre-existing" {
		t.Fatalf("adoption-reason = %q", got)
	}
	snapshot, err := actions.SnapshotOf(ctx, ref)
	if err != nil {
		t.Fatalf("read the snapshot: %v", err)
	}
	if want := specOf(t, before); !reflect.DeepEqual(snapshot.Spec, want) {
		t.Fatalf("snapshot spec %v != pre-adoption spec %v", snapshot.Spec, want)
	}

	status, body = plane.Call(t, http.MethodGet,
		"/api/v1/resources/Endpoint/"+endpoint.GetName()+"?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the adopted projection: status %d, body %v", status, body)
	}
	if got, want := sources(t, body), []string{"cluster-scan", "endpoint-scan"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("adoption dropped a discovery source: %v, want %v", got, want)
	}

	// A disabled adapter stops reporting; the object it found is kept (C-28).
	runner.Adapters[1].IsActive = false
	reports, err = runner.Run(ctx)
	if err != nil {
		t.Fatalf("run discovery with one adapter disabled: %v", err)
	}
	index.Replace(reports)
	status, body = plane.Call(t, http.MethodGet,
		"/api/v1/resources/Endpoint/"+endpoint.GetName()+"?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the projection after disabling one adapter: status %d, body %v", status, body)
	}
	if got, want := sources(t, body), []string{"cluster-scan"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a disabled adapter still reports: sources %v, want %v", got, want)
	}

	runner.Adapters[0].IsActive = false
	reports, err = runner.Run(ctx)
	if err != nil {
		t.Fatalf("run discovery with every adapter disabled: %v", err)
	}
	if len(reports) != 0 {
		t.Fatalf("disabled adapters reported %d objects", len(reports))
	}
	index.Replace(reports)
	status, body = plane.Call(t, http.MethodGet,
		"/api/v1/resources/Endpoint/"+endpoint.GetName()+"?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the projection after disabling every adapter: status %d, body %v", status, body)
	}
	if got := sources(t, body); len(got) != 0 {
		t.Fatalf("a disabled adapter still reports: sources %v", got)
	}
	if kept := cluster.Get(t, "v1beta1", "Endpoint", endpoint.GetName()); kept.GetLabels()[markers.AdoptedBy] != session.Username {
		t.Fatalf("disabling discovery dropped the discovered object: %v", kept.GetLabels())
	}
}

// existingDeployment is a workload that already runs in the cluster before KOS
// installs.
func existingDeployment() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": "legacy-web", "namespace": harness.Namespace},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{"app": "legacy-web"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "legacy-web"}},
				"spec": map[string]any{"containers": []any{
					map[string]any{"name": "web", "image": "nginx"},
				}},
			},
		},
	}}
}

// existingEndpoint is an endpoint object written before adoption.
func existingEndpoint() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Endpoint",
		"metadata":   map[string]any{"name": "legacy-site", "namespace": harness.Namespace},
		"spec": map[string]any{
			"kind":      "http.host",
			"targetApp": "legacy-web",
			"host":      "legacy.example.test",
		},
	}}
}

// assertUnadopted checks that an object still carries no attribution marker.
func assertUnadopted(t *testing.T, object *unstructured.Unstructured) {
	t.Helper()
	for _, key := range []string{markers.ManagedBy, markers.AdoptedBy, markers.AdoptionReason} {
		if value := object.GetLabels()[key]; value != "" {
			t.Fatalf("%s carries %s=%s before adoption", object.GetName(), key, value)
		}
	}
}

// ingresses counts the downstream route objects in the acceptance namespace.
func ingresses(t *testing.T, cluster *harness.Cluster) int {
	t.Helper()
	list, err := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: "networking.k8s.io", Version: "v1", Resource: "ingresses",
	}).Namespace(harness.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list ingresses: %v", err)
	}
	return len(list.Items)
}

// assertNoSourcesWritten checks that discovery did not touch the existing
// workload.
func assertNoSourcesWritten(t *testing.T, cluster *harness.Cluster, deployment *unstructured.Unstructured) {
	t.Helper()
	current, err := cluster.Dynamic.Resource(schema.GroupVersionResource{
		Group: "apps", Version: "v1", Resource: "deployments",
	}).Namespace(harness.Namespace).Get(context.Background(), deployment.GetName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the workload: %v", err)
	}
	if current.GetResourceVersion() != deployment.GetResourceVersion() {
		t.Fatalf("discovery changed the workload: resourceVersion %s -> %s",
			deployment.GetResourceVersion(), current.GetResourceVersion())
	}
}

// reportsFor returns the reports of one object in a kind.
func reportsFor(reports []discovery.Report, kind, name string) []discovery.Report {
	var found []discovery.Report
	for _, report := range reports {
		if report.Ref.Kind == kind && report.Ref.Name == name {
			found = append(found, report)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Adapter < found[j].Adapter })
	return found
}

// sources reads the projection's source list.
func sources(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["sources"].([]any)
	sources := make([]string, 0, len(raw))
	for _, value := range raw {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("source is not a string: %v", value)
		}
		sources = append(sources, text)
	}
	sort.Strings(sources)
	return sources
}

// specOf returns a copy of the object's spec.
func specOf(t *testing.T, object *unstructured.Unstructured) map[string]any {
	t.Helper()
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil || !found {
		t.Fatalf("read spec of %s: %v", object.GetName(), err)
	}
	return spec
}
