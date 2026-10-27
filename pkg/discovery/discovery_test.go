package discovery_test

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/tsic404/kos/pkg/discovery"
)

// TestRunKeepsReportsWhenOneAdapterFails covers C-25's multi-source rule under
// a failing adapter: the other adapter's reports survive the round.
func TestRunKeepsReportsWhenOneAdapterFails(t *testing.T) {
	ctx := context.Background()
	configMaps := schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{configMaps: "ConfigMapList"},
	)
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": "settings", "namespace": "default",
			"labels": map[string]any{"app": "web"},
		},
		"data": map[string]any{"key": "value"},
	}}
	if _, err := client.Resource(configMaps).Namespace("default").Create(ctx, object, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed the ConfigMap: %v", err)
	}

	runner := &discovery.Runner{
		Dynamic:   client,
		Resources: discovery.Resources{{Kind: "ConfigMap"}: configMaps},
		Adapters: []discovery.Adapter{
			{
				ID:       "working",
				Covers:   []schema.GroupKind{{Kind: "ConfigMap"}},
				Fields:   []string{"metadata.labels"},
				IsActive: true,
			},
			{
				ID:       "broken",
				Covers:   []schema.GroupKind{{Kind: "Secret"}},
				IsActive: true,
			},
		},
	}
	reports, err := runner.Run(ctx)
	if err == nil {
		t.Fatal("an adapter without a resource mapping did not fail the round")
	}
	if len(reports) != 1 {
		t.Fatalf("the working adapter reported %d objects, want 1", len(reports))
	}
	if reports[0].Adapter != "working" || reports[0].Ref.Name != "settings" {
		t.Fatalf("the surviving report is %+v", reports[0])
	}
	if reports[0].Fields["metadata.labels"] == nil {
		t.Fatalf("the surviving report lost its fields: %+v", reports[0].Fields)
	}
}
