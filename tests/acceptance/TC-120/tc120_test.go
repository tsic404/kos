// Package tc120 verifies C-64's failure semantics and event window (AC-59,
// TC-120): a control plane cut off from the cluster API answers with a reason
// instead of a successful empty read, an unusable credential names itself, and
// once the read works the events of the last hour are readable newest first
// while an older one is not.
package tc120

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// readTime is the clock the case reads on, so the event fixtures and the
// one-hour window agree.
var readTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// Source states the case switches between.
const (
	sourceConnected int32 = iota
	sourceUnreachable
	sourceCredentialExpired
)

// switchSource is the cluster read seam with a switch the case throws: the
// control plane keeps its client while the cluster is made unreachable.
type switchSource struct {
	base controlplane.ObjectSource
	mode *atomic.Int32
}

// List implements controlplane.ObjectSource.
func (s switchSource) List(ctx context.Context, objectType controlplane.ObjectType, namespace string) ([]controlplane.ObservedObject, error) {
	if err := s.failure(); err != nil {
		return nil, err
	}
	return s.base.List(ctx, objectType, namespace)
}

// Get implements controlplane.ObjectSource.
func (s switchSource) Get(ctx context.Context, objectType controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	if err := s.failure(); err != nil {
		return controlplane.ObservedObject{}, err
	}
	return s.base.Get(ctx, objectType, namespace, name)
}

// failure is the reason the read fails in the current mode.
func (s switchSource) failure() error {
	switch s.mode.Load() {
	case sourceUnreachable:
		return &clusterUnreachableError{}
	case sourceCredentialExpired:
		return apierrors.NewUnauthorized("the control plane credential has expired")
	}
	return nil
}

// clusterUnreachableError is the failure a disconnected client reports.
type clusterUnreachableError struct{}

// Error implements error.
func (*clusterUnreachableError) Error() string {
	return "dial tcp 10.43.0.1:443: connect: connection refused"
}

// application is one stored application whose last render wrote one deployment.
func application(name, owner string) map[string]any {
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{
			"namespace": harness.Namespace,
			"owner":     owner,
			"visibility": map[string]any{
				"allowGuests": true,
			},
		},
		"status": map[string]any{"rendered": []any{map[string]any{
			"group": "apps", "version": "v1", "kind": "Deployment",
			"namespace": harness.Namespace, "name": name,
		}}},
	}
}

// deployment is the workload the application renders.
func deployment(name string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": name}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "web", "image": "nginx:1.27",
				}}},
			},
		},
	}
}

// pod is the workload's instance.
func pod(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"app": "shop"},
		},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "web", "image": "nginx:1.27",
		}}},
	}
}

// event is one event about the application, stamped at lastSeen.
func event(name string, lastSeen time.Time) map[string]any {
	stamp := lastSeen.UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"involvedObject": map[string]any{
			"apiVersion": "kos.tsic.top/v1beta1", "kind": "Application",
			"namespace": harness.Namespace, "name": "shop",
		},
		"type":               "Warning",
		"reason":             "BackendUnavailable",
		"message":            "no ready backend",
		"count":              1,
		"firstTimestamp":     stamp,
		"lastTimestamp":      stamp,
		"reportingComponent": "kos-shell",
		"reportingInstance":  "kos-shell-1",
	}
}

// TestTC120UnreachableClusterGivesAReasonThenTheWindow pins the case end to
// end: disconnected reads answer with a reason, restored reads give the
// one-hour window newest first and leave the older event unreadable.
func TestTC120UnreachableClusterGivesAReasonThenTheWindow(t *testing.T) {
	cluster := harness.Start(t)
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: application("shop", "alice")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, deployment("shop"))
	cluster.CreateClusterObject(t, "", "v1", "pods", harness.Namespace, pod("shop-0"))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace, event("shop-newest", readTime.Add(-5*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace, event("shop-older", readTime.Add(-50*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace, event("shop-before-window", readTime.Add(-2*time.Hour)))

	logs := harness.NewFixtureLogs()
	logs.Add(harness.Namespace, "shop-0", "web", readTime.Add(-time.Minute), "line inside the window")
	logs.Add(harness.Namespace, "shop-0", "web", readTime.Add(-2*time.Hour), "line before the window")

	mode := &atomic.Int32{}
	api := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Source = switchSource{base: controlplane.NewClusterSource(cluster.Dynamic), mode: mode}
		config.Logs = logs
		config.Now = func() time.Time { return readTime }
	})
	logPath := "/api/v1/logs/" + harness.Namespace + "/shop"
	eventPath := "/api/v1/events?namespace=" + harness.Namespace

	// Cut off from the cluster API: both reads name the cluster as the reason
	// instead of returning an empty result.
	mode.Store(sourceUnreachable)
	for _, read := range []struct {
		name string
		path string
		key  string
	}{
		{name: "logs", path: logPath, key: "lines"},
		{name: "events", path: eventPath, key: "events"},
	} {
		t.Run("unreachable "+read.name, func(t *testing.T) {
			status, body := api.Call(t, http.MethodGet, read.path, nil, nil)
			if status != http.StatusBadGateway {
				t.Fatalf("read = %d %v, want 502", status, body)
			}
			errorBody, _ := body["error"].(map[string]any)
			if errorBody["code"] != controlplane.CodeClusterUnavailable {
				t.Fatalf("code = %v, want %s", errorBody["code"], controlplane.CodeClusterUnavailable)
			}
			if errorBody["retryable"] != true {
				t.Fatalf("retryable = %v, want true", errorBody["retryable"])
			}
			if text(errorBody["message"]) == "" {
				t.Fatalf("the failure carries no reason: %v", errorBody)
			}
			if _, empty := body[read.key]; empty {
				t.Fatalf("an unreachable read answered with a result: %v", body)
			}
		})
	}

	// An unusable credential is named as its own reason: a retry alone cannot
	// repair it.
	mode.Store(sourceCredentialExpired)
	status, body := api.Call(t, http.MethodGet, eventPath, nil, nil)
	if status != http.StatusBadGateway {
		t.Fatalf("expired credential = %d %v, want 502", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != controlplane.CodeClusterCredentialsInvalid {
		t.Fatalf("code = %v, want %s", errorBody["code"], controlplane.CodeClusterCredentialsInvalid)
	}
	if errorBody["requiresUserAction"] != true {
		t.Fatalf("requiresUserAction = %v, want true", errorBody["requiresUserAction"])
	}

	// Restored: the event read gives the window newest first and leaves the
	// event older than one hour unreadable.
	mode.Store(sourceConnected)
	status, body = api.Call(t, http.MethodGet, eventPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("restored event read = %d %v", status, body)
	}
	if body["window"] != "1h0m0s" {
		t.Fatalf("window = %v, want the one-hour window", body["window"])
	}
	if body["observedAt"] != readTime.Format(time.RFC3339Nano) {
		t.Fatalf("observedAt = %v, want the read time", body["observedAt"])
	}
	names := eventNames(t, body)
	if len(names) != 2 || names[0] != "shop-newest" || names[1] != "shop-older" {
		t.Fatalf("events = %v, want the two in-window events newest first", names)
	}

	// The log read works again and drops the line older than its window.
	status, body = api.Call(t, http.MethodGet, logPath, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("restored log read = %d %v", status, body)
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want only the line inside the window", lines)
	}
}

// eventNames reads the event names of one response in order.
func eventNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["events"].([]any)
	if !ok {
		t.Fatalf("response has no events: %v", body)
	}
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		item, _ := entry.(map[string]any)
		if name := text(item["name"]); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// text reads one string value.
func text(value any) string {
	parsed, _ := value.(string)
	return parsed
}
