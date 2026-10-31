package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// fixedReadTime is the clock every log case reads on.
var fixedReadTime = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// fixtureLogs serves the lines a case declares per pod and container.
type fixtureLogs struct {
	lines map[string][]LogLine
	err   error
}

// Lines implements LogSource.
func (f fixtureLogs) Lines(_ context.Context, namespace, pod, container string, since time.Time, limit int) ([]LogLine, error) {
	if f.err != nil {
		return nil, f.err
	}
	key := namespace + "/" + pod + "/" + container
	owned := make([]LogLine, 0, len(f.lines[key]))
	for _, line := range f.lines[key] {
		if line.Timestamp.Before(since) {
			continue
		}
		owned = append(owned, line)
	}
	sortLogLines(owned)
	if len(owned) > limit {
		owned = owned[len(owned)-limit:]
	}
	return owned, nil
}

// untimedLogs serves its lines exactly as given, so a case can hand the read a
// line the source did not stamp.
type untimedLogs struct{ lines []LogLine }

// Lines implements LogSource.
func (u untimedLogs) Lines(context.Context, string, string, string, time.Time, int) ([]LogLine, error) {
	return u.lines, nil
}

// refusingAuthorizer refuses every read with one error.
type refusingAuthorizer struct{ err error }

// Decide implements Authorizer.
func (a refusingAuthorizer) Decide(context.Context, Request) (Grant, error) {
	return Grant{}, a.err
}

// logsServer serves one API over a fake source and log source.
func logsServer(t *testing.T, source ObjectSource, logs LogSource, authorizer Authorizer) *httptest.Server {
	t.Helper()
	server, err := New(Config{
		Dynamic:    dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		Source:     source,
		Logs:       logs,
		Authorizer: authorizer,
		Now:        func() time.Time { return fixedReadTime },
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

// readLogs calls the log route and decodes the answer.
func readLogs(t *testing.T, server *httptest.Server, path string) (int, map[string]any) {
	t.Helper()
	response, err := http.Get(server.URL + path)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	defer response.Body.Close()
	body := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode the log read: %v", err)
	}
	return response.StatusCode, body
}

// logApplication is a stored application whose last render wrote the named
// workloads.
func logApplication(name string, workloads ...map[string]any) map[string]any {
	rendered := make([]any, 0, len(workloads))
	for _, workload := range workloads {
		rendered = append(rendered, workload)
	}
	return map[string]any{
		"apiVersion": "kos.tsic.top/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec":       map[string]any{"namespace": "default"},
		"status":     map[string]any{"rendered": rendered},
	}
}

// renderedWorkload names one workload of a render.
func renderedWorkload(kind, name string) map[string]any {
	group, version := "apps", "v1"
	switch kind {
	case "Pod":
		group, version = "", "v1"
	case "Job":
		group = "batch"
	}
	return map[string]any{"group": group, "version": version, "kind": kind, "namespace": "default", "name": name}
}

// labelledDeployment is one app workload selecting pods by one label.
func labelledDeployment(name, application string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata":   map[string]any{"name": name, "namespace": "default"},
		"spec": map[string]any{
			"selector": map[string]any{"matchLabels": map[string]any{"app": application}},
		},
	}
}

// logPod is one pod of one application's workload, with one container.
func logPod(name, application string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": "default",
			"labels": map[string]any{"app": application},
		},
		"spec": map[string]any{"containers": []any{map[string]any{"name": "web"}}},
	}
}

// logWorld is the two applications the cases read: "shop" and "blog" run in
// the same namespace, and only their selectors tell their pods apart.
func logWorld() []map[string]any {
	shop := logApplication("shop", renderedWorkload("Deployment", "shop"))
	blog := logApplication("blog", renderedWorkload("Deployment", "blog"))
	return []map[string]any{
		shop,
		blog,
		labelledDeployment("shop", "shop"),
		labelledDeployment("blog", "blog"),
		logPod("shop-0", "shop"),
		logPod("blog-0", "blog"),
	}
}

// sourceWorld wraps the objects as one fake source.
func sourceWorld(objects []map[string]any) *fakeSource {
	source := &fakeSource{}
	for _, object := range objects {
		source.objects = append(source.objects, observe(object))
	}
	return source
}

// TestApplicationLogsStayInsideTheApplication pins the isolation rule of C-64:
// a read returns only the lines of the pods the application's own workloads
// select, even when another application of the same namespace writes too.
func TestApplicationLogsStayInsideTheApplication(t *testing.T) {
	logs := fixtureLogs{lines: map[string][]LogLine{
		"default/shop-0/web": {{Timestamp: fixedReadTime.Add(-2 * time.Minute), Message: "shop line"}},
		"default/blog-0/web": {{Timestamp: fixedReadTime.Add(-time.Minute), Message: "blog line"}},
	}}
	server := logsServer(t, sourceWorld(logWorld()), logs, AllowAll{})

	status, body := readLogs(t, server, "/api/v1/logs/default/shop")
	if status != http.StatusOK {
		t.Fatalf("read logs = %d %v", status, body)
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want only the application's own line", lines)
	}
	line, _ := lines[0].(map[string]any)
	if line["message"] != "shop line" || line["pod"] != "shop-0" {
		t.Fatalf("line = %v, want the shop pod's line", line)
	}
	if body["truncated"] != false {
		t.Fatalf("truncated = %v, want false", body["truncated"])
	}
}

// TestApplicationLogsTruncateAtTheLineBound pins C-64's bound: a read that
// exceeds the line bound keeps the newest lines and marks where it was cut.
func TestApplicationLogsTruncateAtTheLineBound(t *testing.T) {
	lines := make([]LogLine, 0, 5)
	for index := 1; index <= 5; index++ {
		lines = append(lines, LogLine{
			Timestamp: fixedReadTime.Add(-time.Duration(6-index) * time.Minute),
			Message:   "line " + string(rune('0'+index)),
		})
	}
	logs := fixtureLogs{lines: map[string][]LogLine{"default/shop-0/web": lines}}
	server := logsServer(t, sourceWorld(logWorld()), logs, AllowAll{})

	status, body := readLogs(t, server, "/api/v1/logs/default/shop?limit=3")
	if status != http.StatusOK {
		t.Fatalf("read logs = %d %v", status, body)
	}
	got, _ := body["lines"].([]any)
	if len(got) != 3 {
		t.Fatalf("lines = %v, want the bound of 3", got)
	}
	first, _ := got[0].(map[string]any)
	last, _ := got[2].(map[string]any)
	if first["message"] != "line 3" || last["message"] != "line 5" {
		t.Fatalf("lines = %v, want the newest three", got)
	}
	if body["truncated"] != true {
		t.Fatalf("truncated = %v, want true", body["truncated"])
	}
	wantCut := fixedReadTime.Add(-3 * time.Minute).Format(time.RFC3339Nano)
	if body["truncatedAt"] != wantCut {
		t.Fatalf("truncatedAt = %v, want %s", body["truncatedAt"], wantCut)
	}
}

// TestApplicationLogsRespectTheWindowAndClampTheRequest pins both bounds: a line
// older than the window is not readable, and a request above the window bound is
// clamped to it rather than widening the read.
func TestApplicationLogsRespectTheWindowAndClampTheRequest(t *testing.T) {
	logs := fixtureLogs{lines: map[string][]LogLine{"default/shop-0/web": {
		{Timestamp: fixedReadTime.Add(-time.Minute), Message: "recent"},
		{Timestamp: fixedReadTime.Add(-30 * time.Minute), Message: "outside a ten-minute window"},
		{Timestamp: fixedReadTime.Add(-2 * time.Hour), Message: "outside the one-hour bound"},
	}}}
	server := logsServer(t, sourceWorld(logWorld()), logs, AllowAll{})

	status, body := readLogs(t, server, "/api/v1/logs/default/shop?window=10m")
	if status != http.StatusOK {
		t.Fatalf("read logs = %d %v", status, body)
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want only the line inside the ten-minute window", lines)
	}

	status, body = readLogs(t, server, "/api/v1/logs/default/shop?window=5h")
	if status != http.StatusOK {
		t.Fatalf("read logs = %d %v", status, body)
	}
	if body["window"] != "1h0m0s" {
		t.Fatalf("window = %v, want the one-hour bound", body["window"])
	}
	lines, _ = body["lines"].([]any)
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want the two lines inside the one-hour bound", lines)
	}
}

// TestApplicationLogsDropUntimedLines pins the window at the read: a line the
// source handed over without a timestamp cannot be placed inside the window, so
// the read does not return it, while the timed line of the window still comes
// back.
func TestApplicationLogsDropUntimedLines(t *testing.T) {
	logs := untimedLogs{lines: []LogLine{
		{Timestamp: fixedReadTime.Add(-time.Minute), Message: "timed"},
		{Message: "untimed"},
	}}
	server := logsServer(t, sourceWorld(logWorld()), logs, AllowAll{})

	status, body := readLogs(t, server, "/api/v1/logs/default/shop")
	if status != http.StatusOK {
		t.Fatalf("read logs = %d %v", status, body)
	}
	lines, _ := body["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("lines = %v, want only the timed line", lines)
	}
	line, _ := lines[0].(map[string]any)
	if line["message"] != "timed" {
		t.Fatalf("line = %v, want the timed line", line)
	}
}

// TestApplicationLogsAreHiddenWithTheApplication pins C-59's read rule: a
// session that may not see the application receives no logs of it.
func TestApplicationLogsAreHiddenWithTheApplication(t *testing.T) {
	logs := fixtureLogs{lines: map[string][]LogLine{
		"default/shop-0/web": {{Timestamp: fixedReadTime.Add(-time.Minute), Message: "shop line"}},
	}}
	for _, testCase := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{name: "hidden application", err: ErrHidden, status: http.StatusNotFound, code: CodeNotFound},
		{name: "refused read", err: errors.New("access: refused"), status: http.StatusForbidden, code: CodePermissionDenied},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := logsServer(t, sourceWorld(logWorld()), logs, refusingAuthorizer{err: testCase.err})
			status, body := readLogs(t, server, "/api/v1/logs/default/shop")
			if status != testCase.status {
				t.Fatalf("read logs = %d %v, want %d", status, body, testCase.status)
			}
			errorBody, _ := body["error"].(map[string]any)
			if errorBody["code"] != testCase.code {
				t.Fatalf("code = %v, want %s", errorBody["code"], testCase.code)
			}
		})
	}
}

// TestApplicationLogsNameTheClusterAsTheReason pins C-64's failure semantics:
// an unreachable API and an unusable credential both answer with a reason, and
// neither answers with a successful empty read.
func TestApplicationLogsNameTheClusterAsTheReason(t *testing.T) {
	source := sourceWorld(logWorld())
	source.getErr = errors.New("dial tcp 10.43.0.1:443: connect: connection refused")
	server := logsServer(t, source, fixtureLogs{}, AllowAll{})

	status, body := readLogs(t, server, "/api/v1/logs/default/shop")
	if status != http.StatusBadGateway {
		t.Fatalf("unreachable read = %d %v, want 502", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != CodeClusterUnavailable {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeClusterUnavailable)
	}
	if errorBody["retryable"] != true {
		t.Fatalf("retryable = %v, want true", errorBody["retryable"])
	}
	if _, empty := body["lines"]; empty {
		t.Fatalf("an unreachable read answered with a result: %v", body)
	}

	expired := sourceWorld(logWorld())
	expired.getErr = apierrors.NewUnauthorized("the token has expired")
	server = logsServer(t, expired, fixtureLogs{}, AllowAll{})
	status, body = readLogs(t, server, "/api/v1/logs/default/shop")
	if status != http.StatusBadGateway {
		t.Fatalf("credential read = %d %v, want 502", status, body)
	}
	errorBody, _ = body["error"].(map[string]any)
	if errorBody["code"] != CodeClusterCredentialsInvalid {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeClusterCredentialsInvalid)
	}
	if errorBody["requiresUserAction"] != true {
		t.Fatalf("requiresUserAction = %v, want true", errorBody["requiresUserAction"])
	}
}

// TestApplicationLogsReportALogEndpointFailure pins the same reason when the
// application reads but its logs do not.
func TestApplicationLogsReportALogEndpointFailure(t *testing.T) {
	server := logsServer(t, sourceWorld(logWorld()), fixtureLogs{err: errors.New("the log stream closed")}, AllowAll{})
	status, body := readLogs(t, server, "/api/v1/logs/default/shop")
	if status != http.StatusBadGateway {
		t.Fatalf("log read = %d %v, want 502", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != CodeClusterUnavailable {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeClusterUnavailable)
	}
}

// TestApplicationLogsNeedAResolvableApplication keeps a missing application from
// reading as an application with no logs.
func TestApplicationLogsNeedAResolvableApplication(t *testing.T) {
	server := logsServer(t, sourceWorld(logWorld()), fixtureLogs{}, AllowAll{})
	status, body := readLogs(t, server, "/api/v1/logs/default/missing")
	if status != http.StatusNotFound {
		t.Fatalf("missing application = %d %v, want 404", status, body)
	}
	errorBody, _ := body["error"].(map[string]any)
	if errorBody["code"] != CodeNotFound {
		t.Fatalf("code = %v, want %s", errorBody["code"], CodeNotFound)
	}
}

// TestApplicationLogRouteIsUnservedWithoutASource keeps a control plane that
// holds no log client from pretending every application writes nothing.
func TestApplicationLogRouteIsUnservedWithoutASource(t *testing.T) {
	server, err := New(Config{Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), Source: sourceWorld(logWorld())})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)

	status, _ := readLogs(t, httpServer, "/api/v1/logs/default/shop")
	if status != http.StatusNotFound {
		t.Fatalf("unserved route = %d, want 404", status)
	}
}

// TestLogBoundsClampAndReject pins the request bounds directly, including the
// values the route refuses.
func TestLogBoundsClampAndReject(t *testing.T) {
	limit, window, failure := logBoundsFromQuery("", "")
	if failure != nil || limit != defaultLogLines || window != defaultLogWindow {
		t.Fatalf("defaults = %d %s %v, want %d %s", limit, window, failure, defaultLogLines, defaultLogWindow)
	}
	limit, window, failure = logBoundsFromQuery("5000", "5h")
	if failure != nil || limit != maxLogLines || window != maxLogWindow {
		t.Fatalf("clamped = %d %s %v, want %d %s", limit, window, failure, maxLogLines, maxLogWindow)
	}
	for _, raw := range []string{"0", "-2", "many"} {
		if _, _, failure := logBoundsFromQuery(raw, ""); failure == nil {
			t.Fatalf("limit %q was accepted", raw)
		}
	}
	for _, raw := range []string{"0s", "-1m", "soon"} {
		if _, _, failure := logBoundsFromQuery("", raw); failure == nil {
			t.Fatalf("window %q was accepted", raw)
		}
	}
}

// TestSplitLogLineSeparatesTheKubeletTimestamp pins the line shape the cluster
// adapter parses.
func TestSplitLogLineSeparatesTheKubeletTimestamp(t *testing.T) {
	stamp, message := splitLogLine("2026-10-25T12:00:00.123456789Z hello world")
	if message != "hello world" || !stamp.Equal(time.Date(2026, 10, 25, 12, 0, 0, 123456789, time.UTC)) {
		t.Fatalf("parsed = %s %q", stamp, message)
	}
	stamp, message = splitLogLine("no timestamp here")
	if !stamp.IsZero() || message != "no timestamp here" {
		t.Fatalf("parsed = %s %q", stamp, message)
	}
}

// TestWorkloadSelectorReadsEveryDeclaredShape covers the selector shapes the
// workload kinds carry.
func TestWorkloadSelectorReadsEveryDeclaredShape(t *testing.T) {
	standard, err := workloadSelector("Deployment", labelledDeployment("shop", "shop"))
	if err != nil || !standard.Matches(labels.Set{"app": "shop"}) || standard.Matches(labels.Set{"app": "blog"}) {
		t.Fatalf("deployment selector = %v, %v", standard, err)
	}
	controller, err := workloadSelector("ReplicationController", map[string]any{
		"spec": map[string]any{"selector": map[string]any{"app": "shop"}},
	})
	if err != nil || !controller.Matches(labels.Set{"app": "shop"}) {
		t.Fatalf("replication controller selector = %v, %v", controller, err)
	}
	cron, err := workloadSelector("CronJob", map[string]any{
		"spec": map[string]any{"jobTemplate": map[string]any{"spec": map[string]any{
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"app": "shop"}}},
		}}},
	})
	if err != nil || !cron.Matches(labels.Set{"app": "shop"}) {
		t.Fatalf("cron job selector = %v, %v", cron, err)
	}
	expression, err := workloadSelector("Deployment", map[string]any{
		"spec": map[string]any{"selector": map[string]any{"matchExpressions": []any{
			map[string]any{"key": "app", "operator": "In", "values": []any{"shop", "blog"}},
		}}},
	})
	if err != nil || !expression.Matches(labels.Set{"app": "blog"}) {
		t.Fatalf("expression selector = %v, %v", expression, err)
	}
}
