// Package tc96 verifies that a cluster upgrade has no path that force-drains a
// node holding a node-local application: the request that asks for one is
// refused, the drain refuses while the application still runs, and the
// workload and its data stay where they are (AC-48, TC-96).
package tc96

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/maintenance"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	appsNamespace = "apps"
	application   = "shop"
	nodeName      = "node-a"
	volumeName    = "data"
	storageClass  = "local"
	claimName     = "data"
	ownerName     = "owner"
	downtime      = "15m"
)

// shopTemplate renders the Deployment of the node-local application.
const shopTemplate = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 2
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
          image: nginx:1.27
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: {{ index .Volumes "data" }}
`

// TestTC96NoForceDrain checks that neither the maintenance requests nor the
// drain offer a forced path (C-55, C-65).
func TestTC96NoForceDrain(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, appsNamespace)
	cluster.StorageClass(t, storageClass, "local.example", false, "Delete", nil)
	createNode(t, cluster, nodeName)
	cluster.CreateClaim(t, appsNamespace, claimName, storageClass)
	cluster.CreateIn(t, appsNamespace, boundVolume())
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, application, shopTemplate, nil))

	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Maintenance = &maintenance.Service{
			Dynamic: cluster.Dynamic,
			Writer:  cluster.Writer(),
			Pods:    maintenance.Cluster{Kube: cluster.Kube},
			Auditor: config.Auditor,
		}
	})
	manifest := harness.ApplicationManifest(application, harness.Namespace, application, appsNamespace, nil, []string{volumeName})
	manifest["spec"].(map[string]any)["replicas"] = int64(2)
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application", manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create the application: status %d, body %v", status, body)
	}
	controller := cluster.ApplicationController(t, app.Options{})
	harness.Eventually(t, 30*time.Second, "the application reaches Ready", func() bool {
		cluster.ApplicationRound(t, controller, application)
		return cluster.ApplicationPhase(t, application) == app.PhaseReady
	})
	createPod(t, cluster, "shop-abc-1")
	claimBefore := claimVersion(t, cluster)

	// A maintenance start that asks for a forced drain names a field the model
	// does not define, so it is refused before anything moves (C-55).
	status, body := gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance", map[string]any{
		"node": nodeName, "downtime": downtime, "force": true,
	}, nil)
	if status != http.StatusBadRequest || failureCode(t, body) != controlplane.CodeInvalidRequest {
		t.Fatalf("a forced maintenance start: status %d, body %v, want 400 %s", status, body, controlplane.CodeInvalidRequest)
	}
	if schedulable(t, cluster, nodeName) == false {
		t.Fatal("the refused request cordoned the node")
	}

	// The cordon itself is accepted; the node-local application has not stopped
	// yet, so the drain waits for it instead of forcing it out (C-65).
	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance", map[string]any{
		"node": nodeName, "downtime": downtime,
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start the maintenance: status %d, body %v", status, body)
	}

	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance/drain", map[string]any{
		"node": nodeName, "force": true,
	}, nil)
	if status != http.StatusBadRequest || failureCode(t, body) != controlplane.CodeInvalidRequest {
		t.Fatalf("a forced drain: status %d, body %v, want 400 %s", status, body, controlplane.CodeInvalidRequest)
	}

	status, body = gateway.Call(t, http.MethodPost, "/api/v1/node-maintenance/drain", map[string]any{"node": nodeName}, nil)
	if status != http.StatusConflict || failureCode(t, body) != controlplane.CodeConflict {
		t.Fatalf("a drain before the stop: status %d, body %v, want 409 %s", status, body, controlplane.CodeConflict)
	}
	if message := failureMessage(t, body); !contains(message, harness.Namespace+"/"+application) {
		t.Fatalf("the refusal names %q, want the application %s/%s", message, harness.Namespace, application)
	}

	// Nothing moved: the workload still runs, its pod is still on the node, and
	// the claim holding the data is untouched (C-55).
	if replicas := cluster.WorkloadReplicas(t, "Deployment", appsNamespace, application); replicas != 2 {
		t.Fatalf("workload replicas = %d after the refused drain, want 2", replicas)
	}
	if !podExists(t, cluster, "shop-abc-1") {
		t.Fatal("the refused drain removed the node-local application's pod")
	}
	if got := claimVersion(t, cluster); got != claimBefore {
		t.Fatalf("the claim holding the data changed: %s -> %s", claimBefore, got)
	}
	if node := volumeNode(t, cluster); node != nodeName {
		t.Fatalf("the data moved to node %q, want %q", node, nodeName)
	}
}

// boundVolume is the node-local volume whose data sits on the case's node.
func boundVolume() *unstructured.Unstructured {
	object := harness.Volume(appsNamespace, volumeName, map[string]any{
		"owner":        ownerName,
		"mobility":     "node-local",
		"capacity":     "1Gi",
		"storageClass": storageClass,
	})
	object.Object["status"] = map[string]any{"node": nodeName}
	return object
}

// volumeNode reads where the volume's data sits.
func volumeNode(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	volume := cluster.GetIn(t, appsNamespace, "v1beta1", "Volume", volumeName)
	node, _, _ := unstructured.NestedString(volume.Object, "status", "node")
	return node
}

// claimVersion reads the resource version of the claim that holds the data.
func claimVersion(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	return cluster.Claim(t, appsNamespace, claimName).GetResourceVersion()
}

// createPod writes the pod the workload runs on the case's node.
func createPod(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "pods", appsNamespace, map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": appsNamespace,
			"ownerReferences": []any{map[string]any{
				"apiVersion": "apps/v1", "kind": "ReplicaSet", "name": "shop-abc",
				"uid": "uid-shop-abc", "controller": true,
			}},
		},
		"spec": map[string]any{
			"nodeName":                      nodeName,
			"terminationGracePeriodSeconds": int64(0),
			"containers":                    []any{map[string]any{"name": "app", "image": "nginx:1.27"}},
		},
	})
}

// podExists reports whether the cluster still holds one pod.
func podExists(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, "", "v1", "Pod", appsNamespace).Get(ctx, name, metav1.GetOptions{})
	return err == nil
}

// schedulable reports whether the cluster schedules on one node.
func schedulable(t *testing.T, cluster *harness.Cluster, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node, err := cluster.Direct(t, "", "v1", "Node", "").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read node %q: %v", name, err)
	}
	cordoned, _, _ := unstructured.NestedBool(node.Object, "spec", "unschedulable")
	return !cordoned
}

// createNode writes one ready, schedulable node.
func createNode(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	cluster.CreateClusterObject(t, "", "v1", "nodes", "", map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": name},
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
		"allocatable": map[string]any{"cpu": "4", "memory": "8Gi"},
		"nodeInfo":    map[string]any{"kubeletVersion": "v1.37.0"},
	}
	if _, err := nodes.UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the status of node %q: %v", name, err)
	}
}

// failureCode reads the stable code of one error response.
func failureCode(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	code, _ := failure["code"].(string)
	return code
}

// failureMessage reads the user-facing message of one error response.
func failureMessage(t *testing.T, body map[string]any) string {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	message, _ := failure["message"].(string)
	return message
}

// contains reports whether value names want.
func contains(value, want string) bool {
	return strings.Contains(value, want)
}
