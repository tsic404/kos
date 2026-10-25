// Package tc29 verifies that the four failure classes answer with the fields the
// contract requires and that a write accepted before a client disconnect still
// reaches its terminal state (AC-15, TC-29).
package tc29

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// batchSize is large enough that the writes of an apply are still running when
// the client drops the connection.
const batchSize = 30

// writeTimeout bounds the wait for an accepted operation to reach its terminal
// state.
const writeTimeout = 60 * time.Second

// denyWrites refuses every write to the named object.
type denyWrites struct {
	name string
}

// Decide implements controlplane.Authorizer.
func (d denyWrites) Decide(_ context.Context, request controlplane.Request) error {
	if request.Verb != controlplane.VerbRead && request.Ref.Name == d.name {
		return errors.New("not allowed")
	}
	return nil
}

// TestTC29FailureFields checks the envelope of each failure class.
func TestTC29FailureFields(t *testing.T) {
	cluster := harness.Start(t)
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Authorizer = denyWrites{name: "forbidden"}
	})

	cases := []struct {
		name       string
		method     string
		path       string
		body       any
		headers    map[string]string
		status     int
		code       string
		stage      string
		retryable  bool
		actionable bool
	}{
		{
			name:   "permission denied",
			method: http.MethodPost,
			path:   "/api/v1/resources/Application",
			body: harness.Manifest("Application", harness.Namespace, "forbidden",
				map[string]string{"managed-by": "kos-shell"}, nil),
			status: http.StatusForbidden, code: controlplane.CodePermissionDenied,
			stage: string(controlplane.StageAuthorize), retryable: false, actionable: true,
		},
		{
			name:   "invalid field",
			method: http.MethodPost,
			path:   "/api/v1/resources/Application",
			body: func() map[string]any {
				manifest := harness.Manifest("Application", harness.Namespace, "invalid",
					map[string]string{"managed-by": "kos-shell"}, nil)
				manifest["spec"] = map[string]any{"replicas": 3}
				return manifest
			}(),
			status: http.StatusBadRequest, code: controlplane.CodeInvalidField,
			stage: string(controlplane.StageValidate), retryable: false, actionable: true,
		},
		{
			name:   "capability missing",
			method: http.MethodPost,
			path:   "/api/v1/resources/Widget",
			body: map[string]any{
				"apiVersion": "example.test/v1",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "gpu"},
				"spec":       map[string]any{},
			},
			status: http.StatusUnprocessableEntity, code: controlplane.CodeCapabilityMissing,
			stage: string(controlplane.StageCapability), retryable: false, actionable: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := gateway.Call(t, testCase.method, testCase.path, testCase.body, testCase.headers)
			assertFailure(t, status, body, testCase.status, testCase.code, testCase.stage,
				testCase.retryable, testCase.actionable, "")
		})
	}

	t.Run("concurrent conflict", func(t *testing.T) {
		labels := map[string]string{"managed-by": "kos-shell"}
		if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
			harness.Manifest("Application", harness.Namespace, "contended", labels, nil), nil); status != http.StatusAccepted {
			t.Fatalf("create the contended application: status %d, body %v", status, body)
		}
		// A different desired state behind a stale precondition: the other writer
		// must be told to read again rather than be silently overwritten (C-05).
		contended := harness.Manifest("Application", harness.Namespace, "contended", labels,
			map[string]string{"example.test/version": "v2"})
		status, body := gateway.Call(t, http.MethodPut,
			"/api/v1/resources/Application/contended?namespace="+harness.Namespace,
			contended, map[string]string{"If-Match": "1"})
		assertFailure(t, status, body, http.StatusConflict, controlplane.CodeConflict,
			string(controlplane.StageApply), true, false, "Application/"+harness.Namespace+"/contended")
	})
}

// TestTC29DisconnectedClientStillReachesTerminalState accepts an apply request and
// drops the connection before the writes can finish.
func TestTC29DisconnectedClientStillReachesTerminalState(t *testing.T) {
	cluster := harness.Start(t)
	gateway := cluster.StartControlPlane(t, nil)

	desired := make([]map[string]any, 0, batchSize)
	for index := range batchSize {
		desired = append(desired, harness.Manifest("SystemComponent", "",
			fmt.Sprintf("bulk-%02d", index),
			map[string]string{"managed-by": "kos-shell", "example.test/batch": "disconnect"}, nil))
	}

	address := strings.TrimPrefix(gateway.URL, "http://")
	connection, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatalf("dial %s: %v", address, err)
	}
	defer connection.Close()

	payload, err := json.Marshal(map[string]any{"objects": desired})
	if err != nil {
		t.Fatalf("encode the desired set: %v", err)
	}
	head := fmt.Sprintf("POST /api/v1/apply-requests HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", address, len(payload))
	if _, err := connection.Write([]byte(head)); err != nil {
		t.Fatalf("write the request head: %v", err)
	}
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write the request body: %v", err)
	}
	// Half-close: the server reads the whole request, then sees the client leave
	// while the accepted operation is still running.
	tcp, ok := connection.(*net.TCPConn)
	if !ok {
		t.Fatalf("connection is %T, want *net.TCPConn", connection)
	}
	if err := tcp.CloseWrite(); err != nil {
		t.Fatalf("close the request side: %v", err)
	}

	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read the acceptance response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("accepted apply: status %d, want %d", response.StatusCode, http.StatusAccepted)
	}

	harness.Eventually(t, writeTimeout, fmt.Sprintf("%d accepted objects to reach their terminal state", batchSize), func() bool {
		return len(cluster.List(t, "v1beta1", "SystemComponent")) == batchSize
	})
	for _, component := range cluster.List(t, "v1beta1", "SystemComponent") {
		if component.GetLabels()["example.test/batch"] != "disconnect" {
			t.Fatalf("%s carries labels %v", component.GetName(), component.GetLabels())
		}
	}
}

// competingWriter changes an object through the cluster right after the API read
// it for a precondition, the way a second writer would between the read and the
// write.
type competingWriter struct {
	base      controlplane.ObjectSource
	client    dynamic.Interface
	kind      string
	namespace string
	name      string
	once      sync.Once
	lock      sync.Mutex
	problem   error
}

// List implements controlplane.ObjectSource.
func (w *competingWriter) List(ctx context.Context, kind, namespace string) ([]controlplane.ObservedObject, error) {
	return w.base.List(ctx, kind, namespace)
}

// Get implements controlplane.ObjectSource.
func (w *competingWriter) Get(ctx context.Context, kind, namespace, name string) (controlplane.ObservedObject, error) {
	observed, err := w.base.Get(ctx, kind, namespace, name)
	if err == nil && kind == w.kind && namespace == w.namespace && name == w.name {
		w.once.Do(func() { w.compete(ctx) })
	}
	return observed, err
}

// compete writes one annotation as another writer would.
func (w *competingWriter) compete(ctx context.Context) {
	resource := w.client.Resource(schema.GroupVersionResource{
		Group: "kos.tsic.top", Version: "v1beta1", Resource: "applications",
	}).Namespace(w.namespace)
	current, err := resource.Get(ctx, w.name, metav1.GetOptions{})
	if err != nil {
		w.fail(err)
		return
	}
	annotations := current.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["example.test/competing"] = "writer"
	current.SetAnnotations(annotations)
	if _, err := resource.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		w.fail(err)
	}
}

// fail records the first problem of the competing write.
func (w *competingWriter) fail(err error) {
	w.lock.Lock()
	defer w.lock.Unlock()
	if w.problem == nil {
		w.problem = err
	}
}

// err returns the problem of the competing write, if any.
func (w *competingWriter) err() error {
	w.lock.Lock()
	defer w.lock.Unlock()
	return w.problem
}

// TestTC29ConcurrentWritesMeetTheAPIServer interleaves a competing write between
// the precondition read and the write: only the API server's own version check
// can refuse the second write, a pre-check cannot (C-05).
func TestTC29ConcurrentWritesMeetTheAPIServer(t *testing.T) {
	cluster := harness.Start(t)
	create := func(name string) *unstructured.Unstructured {
		return cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest(
			"Application", harness.Namespace, name, map[string]string{"managed-by": "kos-shell"}, nil)})
	}
	start := func(name string) (*harness.ControlPlane, *competingWriter) {
		writer := &competingWriter{
			base:      controlplane.NewClusterSource(cluster.Dynamic),
			client:    cluster.Dynamic,
			kind:      "Application",
			namespace: harness.Namespace,
			name:      name,
		}
		return cluster.StartControlPlane(t, func(config *controlplane.Config) { config.Source = writer }), writer
	}

	t.Run("update", func(t *testing.T) {
		const name = "raced-update"
		created := create(name)
		gateway, writer := start(name)
		desired := harness.Manifest("Application", harness.Namespace, name,
			map[string]string{"managed-by": "kos-shell"}, map[string]string{"example.test/version": "v2"})

		status, body := gateway.Call(t, http.MethodPut,
			"/api/v1/resources/Application/"+name+"?namespace="+harness.Namespace,
			desired, map[string]string{"If-Match": created.GetResourceVersion()})
		if err := writer.err(); err != nil {
			t.Fatalf("competing write: %v", err)
		}
		assertFailure(t, status, body, http.StatusConflict, controlplane.CodeConflict,
			string(controlplane.StageApply), true, false, "Application/"+harness.Namespace+"/"+name)
		assertCurrentObject(t, body)

		stored := cluster.Get(t, "v1beta1", "Application", name)
		if stored.GetAnnotations()["example.test/competing"] != "writer" {
			t.Fatalf("the competing write was overwritten: %v", stored.GetAnnotations())
		}
		if _, present := stored.GetAnnotations()["example.test/version"]; present {
			t.Fatalf("the conflicting update was applied anyway: %v", stored.GetAnnotations())
		}
	})

	t.Run("delete", func(t *testing.T) {
		const name = "raced-delete"
		created := create(name)
		gateway, writer := start(name)

		status, body := gateway.Call(t, http.MethodDelete,
			"/api/v1/resources/Application/"+name+"?namespace="+harness.Namespace,
			nil, map[string]string{"If-Match": created.GetResourceVersion()})
		if err := writer.err(); err != nil {
			t.Fatalf("competing write: %v", err)
		}
		assertFailure(t, status, body, http.StatusConflict, controlplane.CodeConflict,
			string(controlplane.StageApply), true, false, "Application/"+harness.Namespace+"/"+name)
		assertCurrentObject(t, body)

		stored := cluster.Get(t, "v1beta1", "Application", name)
		if stored.GetAnnotations()["example.test/competing"] != "writer" {
			t.Fatalf("the competing write was destroyed by the delete: %v", stored.GetAnnotations())
		}
	})
}

// assertCurrentObject checks that a conflict carries the object as it stands,
// which C-05 requires so the caller can reconcile.
func assertCurrentObject(t *testing.T, body map[string]any) {
	t.Helper()
	failure, _ := body["error"].(map[string]any)
	current, ok := failure["current"].(map[string]any)
	if !ok {
		t.Fatalf("the conflict carries no current object: %v", failure)
	}
	if got := harness.String(current, "metadata", "annotations", "example.test/competing"); got != "writer" {
		t.Fatalf("current object is not the one the other writer left: %v", current)
	}
	if harness.String(current, "metadata", "resourceVersion") == "" {
		t.Fatalf("current object carries no resourceVersion: %v", current)
	}
}

// assertFailure checks the envelope fields C-12 requires and the optional object
// reference.
func assertFailure(t *testing.T, status int, body map[string]any, wantStatus int, code, stage string, retryable, actionable bool, reference string) {
	t.Helper()
	if status != wantStatus {
		t.Fatalf("status = %d, want %d (body %v)", status, wantStatus, body)
	}
	failure, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("response carries no error envelope: %v", body)
	}
	if failure["code"] != code {
		t.Errorf("code = %v, want %s", failure["code"], code)
	}
	if failure["message"] == nil || failure["message"] == "" {
		t.Errorf("message is missing: %v", failure)
	}
	if failure["stage"] != stage {
		t.Errorf("stage = %v, want %s", failure["stage"], stage)
	}
	for _, field := range []string{"retryable", "requiresUserAction"} {
		if _, present := failure[field].(bool); !present {
			t.Errorf("%s is missing or not a boolean: %v", field, failure)
		}
	}
	if failure["retryable"] != retryable {
		t.Errorf("retryable = %v, want %v", failure["retryable"], retryable)
	}
	if failure["requiresUserAction"] != actionable {
		t.Errorf("requiresUserAction = %v, want %v", failure["requiresUserAction"], actionable)
	}
	if failure["sideEffects"] != string(controlplane.SideEffectsNone) {
		t.Errorf("sideEffects = %v, want %s", failure["sideEffects"], controlplane.SideEffectsNone)
	}
	object, ok := failure["object"].(map[string]any)
	if !ok {
		t.Fatalf("object reference is missing: %v", failure)
	}
	if harness.String(object, "kind") == "" || harness.String(object, "name") == "" {
		t.Errorf("object reference is incomplete: %v", object)
	}
	if reference != "" && !strings.HasSuffix(harness.Reference(object), reference) {
		t.Errorf("object reference = %v, want it to name %s", object, reference)
	}
}
