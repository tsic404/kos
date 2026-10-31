// Package tc119 verifies C-64's log read (AC-59, TC-119): a read is locked to
// the application's own workloads even when another application of the same
// namespace writes too, an over-bound read is truncated and names the position,
// and a session that may not see the application reads no logs of it.
package tc119

import (
	"net/http"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// readTime is the clock the case reads on, so the fixtures and the window agree.
var readTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// application is one application whose last render wrote one deployment.
func application(name, owner string, members []string, allowGuests bool, deployment string) map[string]any {
	visibility := map[string]any{"allowGuests": allowGuests}
	if len(members) > 0 {
		listed := make([]any, 0, len(members))
		for _, member := range members {
			listed = append(listed, member)
		}
		visibility["members"] = listed
	}
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{
			"namespace":  harness.Namespace,
			"owner":      owner,
			"visibility": visibility,
		},
		"status": map[string]any{"rendered": []any{map[string]any{
			"group": "apps", "version": "v1", "kind": "Deployment",
			"namespace": harness.Namespace, "name": deployment,
		}}},
	}
}

// deployment is the workload one application renders: it selects the pods that
// carry its own label.
func deployment(name, label string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": label}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": label}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "web", "image": "nginx:1.27",
				}}},
			},
		},
	}
}

// pod is one instance of one application's workload.
func pod(name, label string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"app": label},
		},
		"spec": map[string]any{"containers": []any{map[string]any{
			"name": "web", "image": "nginx:1.27",
		}}},
	}
}

// startWorld installs two applications of one namespace, each with its own
// workload and pod, and returns the log source the read will use.
func startWorld(t *testing.T, cluster *harness.Cluster) *harness.FixtureLogs {
	t.Helper()
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: application("shop", "alice", []string{"bob"}, false, "shop")})
	cluster.CreateIn(t, harness.Namespace, &unstructured.Unstructured{Object: application("blog", "carol", nil, false, "blog")})
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, deployment("shop", "shop"))
	cluster.CreateClusterObject(t, "apps", "v1", "deployments", harness.Namespace, deployment("blog", "blog"))
	cluster.CreateClusterObject(t, "", "v1", "pods", harness.Namespace, pod("shop-0", "shop"))
	cluster.CreateClusterObject(t, "", "v1", "pods", harness.Namespace, pod("blog-0", "blog"))

	logs := harness.NewFixtureLogs()
	for index := 1; index <= 6; index++ {
		logs.Add(harness.Namespace, "shop-0", "web", readTime.Add(-time.Duration(7-index)*time.Minute), "shop line "+string(rune('0'+index)))
	}
	logs.Add(harness.Namespace, "blog-0", "web", readTime.Add(-time.Minute), "blog line")
	// An old line of the application's own pod: the read window is what keeps
	// it out, not the workload selection.
	logs.Add(harness.Namespace, "shop-0", "web", readTime.Add(-2*time.Hour), "shop line from before the window")
	return logs
}

// TestTC119LogsStayInsideTheApplicationAndAreBounded reads one application's
// logs as its owner, with and without a line bound, and reads an application the
// session may not see.
func TestTC119LogsStayInsideTheApplicationAndAreBounded(t *testing.T) {
	cluster := harness.Start(t)
	logs := startWorld(t, cluster)
	system := cluster.StartAccess(t, nil, func(config *controlplane.Config) {
		config.Logs = logs
		config.Now = func() time.Time { return readTime }
	})
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	bob := system.Account(t, owner, "bob", "bob-password", write.RoleMember)
	carol := system.Account(t, owner, "carol", "carol-password", write.RoleMember)
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)
	path := "/api/v1/logs/" + harness.Namespace + "/shop"

	// The read returns only the application's own pod's lines.
	status, body := system.Call(t, alice, http.MethodGet, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read logs = %d %v", status, body)
	}
	lines := logLines(t, body)
	if len(lines) != 6 {
		t.Fatalf("lines = %d, want the six lines inside the window", len(lines))
	}
	for _, line := range lines {
		if line["pod"] != "shop-0" || line["container"] != "web" {
			t.Fatalf("line = %v, want a line of this application's pod", line)
		}
		if text(line["message"]) == "blog line" {
			t.Fatalf("the read returned another application's line: %v", line)
		}
	}
	if body["truncated"] != false {
		t.Fatalf("truncated = %v, want false below the bound", body["truncated"])
	}
	if body["window"] != "15m0s" {
		t.Fatalf("window = %v, want the default log window", body["window"])
	}
	if body["observedAt"] != readTime.Format(time.RFC3339Nano) {
		t.Fatalf("observedAt = %v, want the read time", body["observedAt"])
	}

	// A request above the line bound is truncated, keeps the newest lines and
	// names the position it was cut at.
	status, body = system.Call(t, alice, http.MethodGet, path+"?limit=3", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("bounded read = %d %v", status, body)
	}
	lines = logLines(t, body)
	if len(lines) != 3 {
		t.Fatalf("bounded lines = %d, want 3", len(lines))
	}
	if text(lines[0]["message"]) != "shop line 4" || text(lines[2]["message"]) != "shop line 6" {
		t.Fatalf("bounded lines = %v, want the newest three", lines)
	}
	if body["truncated"] != true {
		t.Fatalf("truncated = %v, want true", body["truncated"])
	}
	wantCut := readTime.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	if body["truncatedAt"] != wantCut {
		t.Fatalf("truncatedAt = %v, want %s", body["truncatedAt"], wantCut)
	}

	// A member the application does not list, and a guest the application does
	// not admit, read no logs of it; a listed member does.
	for _, testCase := range []struct {
		name  string
		token string
		want  int
	}{
		{name: "a member the application names", token: bob, want: http.StatusOK},
		{name: "a member the application does not name", token: carol, want: http.StatusNotFound},
		{name: "a guest the application does not admit", token: guest, want: http.StatusNotFound},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := system.Call(t, testCase.token, http.MethodGet, path, nil, nil)
			if status != testCase.want {
				t.Fatalf("read = %d %v, want %d", status, body, testCase.want)
			}
			if testCase.want == http.StatusNotFound {
				if _, carried := body["lines"]; carried {
					t.Fatalf("a hidden application answered with lines: %v", body)
				}
			}
		})
	}
}

// logLines reads the lines of one log response.
func logLines(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["lines"].([]any)
	if !ok {
		t.Fatalf("response has no lines: %v", body)
	}
	lines := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		line, _ := entry.(map[string]any)
		lines = append(lines, line)
	}
	return lines
}

// text reads one string value.
func text(value any) string {
	parsed, _ := value.(string)
	return parsed
}
