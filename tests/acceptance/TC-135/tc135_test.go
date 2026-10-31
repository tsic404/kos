// Package tc135 verifies the resource item's states: capacity is always
// readable, consumption appears only when the metric source samples every node,
// and an incomplete reading is unavailable with the uncovered nodes named rather
// than reported as zero or as a partial cluster total (AC-64, TC-135).
package tc135

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/settings/kossettings"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName = "owner"
	ownerPass = "owner-password"
)

// nodeSamples stands in for the distribution's metric source: one sample per
// node (C-49).
type nodeSamples []kossettings.NodeUsage

// NodeUsage implements kossettings.UsageSource.
func (s nodeSamples) NodeUsage(context.Context) ([]kossettings.NodeUsage, error) { return s, nil }

// TestTC135ResourceUsageStates drives the metric source through full coverage,
// an empty sample set, a missing node and no source at all, and judges the
// projected item in each state.
func TestTC135ResourceUsageStates(t *testing.T) {
	cluster := harness.Start(t)
	createNode(t, cluster, "node-a", "4", "8Gi")
	createNode(t, cluster, "node-b", "2", "4Gi")

	readings := &kossettings.Readings{
		Dynamic: cluster.Dynamic,
		Usage: nodeSamples{
			{Name: "node-a", CPU: "1", Memory: "2Gi"},
			{Name: "node-b", CPU: "2", Memory: "1Gi"},
		},
	}
	shell := cluster.StartShell(t, func(config *pages.Config) {
		config.Readings = pages.Router{kossettings.Identifier: readings}
	})
	readings.Settings = shell.Settings
	installation := cluster.InstallPackageWith(t, "settings", kossettings.Declaration(), extension.MemoryArtifacts{}, shell.Retention())
	shell.Use(installation)
	token := shell.SignInOwner(t, ownerName, ownerPass)

	// The item is the metric-source projection C-53 types; each read below
	// judges the shape the metric source currently produces.
	itemOf := func() map[string]any {
		status, body := shell.Call(t, http.MethodGet, "/api/v1/pages/"+kossettings.PageSettings, nil, token)
		if status != http.StatusOK {
			t.Fatalf("read the settings page = %d %v", status, body)
		}
		items, _ := body["items"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if harness.String(item, "id") == kossettings.ClusterResourceUsage {
				return item
			}
		}
		t.Fatalf("the page carries no resource item: %v", body["items"])
		return nil
	}

	// Full coverage: capacity and consumption are both readable, and the
	// consumption is the samples' sum, not the capacity (C-71).
	item := itemOf()
	if source := harness.String(item, "source"); source != "metric" {
		t.Fatalf("resource item source = %q, want metric", source)
	}
	value, _ := item["value"].(map[string]any)
	if value["available"] != true {
		t.Fatalf("resource item = %v, want a sampled reading", value)
	}
	if allocatable, _ := value["allocatable"].(map[string]any); allocatable["cpu"] != "6" || allocatable["memory"] != "12Gi" {
		t.Fatalf("allocatable = %v, want the sum of both nodes", value["allocatable"])
	}
	if used, _ := value["used"].(map[string]any); used["cpu"] != "3" || used["memory"] != "3Gi" {
		t.Fatalf("used = %v, want the sampled sum of both nodes", value["used"])
	}
	for _, node := range []string{"node-a", "node-b"} {
		if row := rowOf(value, node); row == nil || harness.Nested(row, "used") == nil {
			t.Fatalf("node %q row = %v, want the sample beside its capacity", node, value["nodes"])
		}
	}

	// An empty sample set has measured nothing: the item is unavailable and
	// names both nodes, not a zero-use cluster (C-71).
	readings.Usage = nodeSamples{}
	assertUnavailable(t, itemOf(), []string{"node-a", "node-b"}, nil)

	// A source that skips one node cannot give a cluster total: the item names
	// the uncovered node and publishes no figure for either node (C-71).
	readings.Usage = nodeSamples{{Name: "node-a", CPU: "1", Memory: "2Gi"}}
	assertUnavailable(t, itemOf(), []string{"node-b"}, []string{"node-a"})

	// With no metric source the item says so; capacity stays readable (C-71).
	readings.Usage = nil
	assertUnavailable(t, itemOf(), nil, []string{"node-a", "node-b"})
}

// assertUnavailable checks the unavailable shape of the resource item: the
// reason names the uncovered nodes it must name and none it must not, no level
// carries consumption, and capacity stays readable.
func assertUnavailable(t *testing.T, item map[string]any, wantNamed, wantAbsent []string) {
	t.Helper()
	value, _ := item["value"].(map[string]any)
	if value["available"] != false {
		t.Fatalf("resource item = %v, want the unavailable shape", value)
	}
	reason := harness.String(value, "reason")
	if reason == "" {
		t.Fatalf("resource item = %v, want a reason", value)
	}
	for _, node := range wantNamed {
		if !strings.Contains(reason, node) {
			t.Fatalf("reason = %q, want %s named", reason, node)
		}
	}
	for _, node := range wantAbsent {
		if strings.Contains(reason, node) {
			t.Fatalf("reason = %q, want %s not named", reason, node)
		}
	}
	if used, reported := value["used"]; reported && used != nil {
		t.Fatalf("resource item = %v, want no cluster consumption figure", value)
	}
	nodes, _ := value["nodes"].([]any)
	for _, raw := range nodes {
		row, _ := raw.(map[string]any)
		if _, reported := row["used"]; reported {
			t.Fatalf("node row = %v, want no per-node consumption figure", row)
		}
	}
	allocatable, _ := value["allocatable"].(map[string]any)
	if allocatable["cpu"] != "6" || allocatable["memory"] != "12Gi" {
		t.Fatalf("allocatable = %v, want the capacity kept while unavailable", value["allocatable"])
	}
}

// rowOf returns one node's row from the resource item's node list.
func rowOf(value map[string]any, node string) map[string]any {
	rows, _ := value["nodes"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["name"] == node {
			return row
		}
	}
	return nil
}

// createNode writes one node with the allocation the resource item reads.
func createNode(t *testing.T, cluster *harness.Cluster, name, cpu, memory string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nodes := cluster.Direct(t, "", "v1", "Node", "")
	node, err := nodes.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	node.Object["status"] = map[string]any{
		"conditions":  []any{map[string]any{"type": "Ready", "status": "True"}},
		"allocatable": map[string]any{"cpu": cpu, "memory": memory},
		"nodeInfo":    map[string]any{"kubeletVersion": "v1.37.0"},
	}
	if _, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the status of node %q: %v", name, err)
	}
}
