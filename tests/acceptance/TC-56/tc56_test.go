// Package tc56 verifies the uninstall escape of C-33: a blocked uninstall
// becomes visible as stalled with its blocking objects listed, an early orphan
// request is refused, and an orphan gives up the downstream objects, keeps the
// data, and records both in the application's abandonment manifest instead of
// the disappearing application status (AC-27, TC-56).
package tc56

import (
	"context"
	"net/http"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	targetNamespace = "apps"
	applicationName = "web"
	// holdFinalizer keeps the rendered ConfigMap alive so the uninstall blocks.
	holdFinalizer = "acceptance.kos.tsic.top/hold"
)

func TestTC56UninstallStallAndOrphan(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, targetNamespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, "plain", nil))
	gateway := cluster.StartControlPlane(t, nil)

	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Volume",
		harness.Manifest("Volume", harness.Namespace, "data", map[string]string{"managed-by": "kos-shell"}, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create volume: status %d, body %v", status, body)
	}
	if status, body := gateway.Call(t, http.MethodPost, "/api/v1/resources/Application",
		harness.ApplicationManifest(applicationName, harness.Namespace, "plain", targetNamespace, nil, []string{"data"}), nil); status != http.StatusAccepted {
		t.Fatalf("create application: status %d, body %v", status, body)
	}
	uid := string(cluster.Get(t, "v1beta1", "Application", applicationName).GetUID())

	clock := harness.NewClock(time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC))
	controller := cluster.ApplicationController(t, app.Options{Clock: clock.Now})
	install(t, cluster, controller)
	if !configMapExists(t, cluster) {
		t.Fatal("the installed application has no rendered object")
	}

	hold(t, cluster)
	current := cluster.Get(t, "v1beta1", "Application", applicationName)
	path := "/api/v1/resources/Application/" + applicationName + "?namespace=" + harness.Namespace
	if status, body := gateway.Call(t, http.MethodDelete, path, nil, map[string]string{"If-Match": current.GetResourceVersion()}); status != http.StatusAccepted {
		t.Fatalf("delete application: status %d, body %v", status, body)
	}

	reconcileOnce(t, controller)
	if phase := phaseOf(t, cluster); phase != app.PhaseUninstalling {
		t.Fatalf("phase = %q, want %q", phase, app.PhaseUninstalling)
	}
	blocked := blockedObjects(t, cluster)
	if len(blocked) != 1 || blocked[0].Kind != "ConfigMap" || blocked[0].Name != applicationName {
		t.Fatalf("blocked = %v, want the rendered ConfigMap", blocked)
	}
	if stalled := stalledFlag(t, cluster); stalled {
		t.Fatal("the uninstall reports stalled before the stall window passed")
	}

	// The orphan escape belongs to the stuck condition of C-33: before the
	// window passes, the request is refused and writes nothing.
	status, early := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind":      "Application",
		"namespace": harness.Namespace,
		"name":      applicationName,
		"action":    app.ActionOrphan,
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("orphan before the stall window: status %d, body %v", status, early)
	}
	if code := harness.String(early, "error", "code"); code != "invalid_field" {
		t.Fatalf("orphan before the stall window: code %q, want invalid_field", code)
	}
	if records, err := actionRecords(t, cluster, uid); err != nil || len(records) != 0 {
		t.Fatalf("a refused orphan left %d action records: %v", len(records), err)
	}

	clock.Advance(app.UninstallStallWindow + time.Minute)
	reconcileOnce(t, controller)
	status, projection := gateway.Call(t, http.MethodGet, path, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read the projection: status %d, body %v", status, projection)
	}
	uninstall := harness.Nested(projection, "object", "status", "uninstall")
	if stalled, _ := harness.Nested(projection, "object", "status", "uninstall", "stalled").(bool); !stalled {
		t.Fatalf("the projection does not show the stalled uninstall: %v", uninstall)
	}
	blockedInProjection, _ := harness.Nested(projection, "object", "status", "uninstall", "blocked").([]any)
	if len(blockedInProjection) != 1 {
		t.Fatalf("the projection lists %d blocking objects, want 1", len(blockedInProjection))
	}

	status, action := gateway.Call(t, http.MethodPost, "/api/v1/action-requests", map[string]any{
		"kind":      "Application",
		"namespace": harness.Namespace,
		"name":      applicationName,
		"action":    app.ActionOrphan,
	}, nil)
	if status != http.StatusAccepted {
		t.Fatalf("orphan action: status %d, body %v", status, action)
	}
	// The abandonment manifest is the reconciler's write, not an API side effect:
	// the action names only its one-shot record.
	if _, present := action["leftovers"]; present {
		t.Fatalf("the ordinary action response must not name a manifest: %v", action)
	}

	// Until the gate is released the application stays observable; the reconcile
	// round writes the manifest and releases it.
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Get(context.Background(), applicationName, metav1.GetOptions{}); err != nil {
		t.Fatalf("the application disappeared before the uninstall gate was released: %v", err)
	}
	reconcileOnce(t, controller)
	harness.Eventually(t, 30*time.Second, "the application to be gone", func() bool {
		_, err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Get(context.Background(), applicationName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	if !configMapExists(t, cluster) {
		t.Fatal("orphan deleted the abandoned downstream object")
	}
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", harness.Namespace).Get(context.Background(), "data", metav1.GetOptions{}); err != nil {
		t.Fatalf("orphan deleted the retained volume: %v", err)
	}

	list, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Get(context.Background(), app.AbandonmentName(uid), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the abandonment manifest: %v", err)
	}
	if immutable, _, _ := unstructured.NestedBool(list.Object, "immutable"); !immutable {
		t.Fatal("the abandonment manifest is not immutable")
	}
	if action := list.GetLabels()[app.ActionLabel]; action != "" {
		t.Fatalf("the abandonment manifest carries action %q and would be consumed as a record", action)
	}
	entries, err := app.DecodeAbandonment(harness.String(list.Object, "data", app.AbandonmentKey))
	if err != nil {
		t.Fatalf("decode the abandonment manifest: %v", err)
	}
	dispositions := make(map[string]string, len(entries))
	for _, entry := range entries {
		dispositions[entry.Object.Kind+"/"+entry.Object.Name] = entry.Disposition
	}
	if got := dispositions["ConfigMap/"+applicationName]; got != app.DispositionAbandoned {
		t.Errorf("the rendered ConfigMap is listed as %q, want %q", got, app.DispositionAbandoned)
	}
	if got := dispositions["Volume/data"]; got != app.DispositionRetained {
		t.Errorf("the volume is listed as %q, want %q", got, app.DispositionRetained)
	}

	// The consumed action record is gone; the manifest stays.
	records, err := actionRecords(t, cluster, uid)
	if err != nil {
		t.Fatalf("list the action records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("the consumed action record is still there: %v", records)
	}
}

// install drives rounds until the application reaches Ready.
func install(t *testing.T, cluster *harness.Cluster, controller *app.Controller) {
	t.Helper()
	for range 5 {
		reconcileOnce(t, controller)
		if phaseOf(t, cluster) == app.PhaseReady {
			return
		}
	}
	t.Fatal("the application did not reach Ready")
}

// reconcileOnce drives one round.
func reconcileOnce(t *testing.T, controller *app.Controller) {
	t.Helper()
	if _, err := controller.Reconcile(context.Background(), reconcile.Request{
		Kind: "Application", Namespace: harness.Namespace, Name: applicationName,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// actionRecords lists the unconsumed action records of one application.
func actionRecords(t *testing.T, cluster *harness.Cluster, uid string) ([]unstructured.Unstructured, error) {
	t.Helper()
	list, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: app.ActionOwnerLabel + "=" + uid,
	})
	if err != nil {
		return nil, err
	}
	records := make([]unstructured.Unstructured, 0, len(list.Items))
	for _, item := range list.Items {
		if item.GetLabels()[app.ActionLabel] != "" {
			records = append(records, item)
		}
	}
	return records, nil
}

// hold puts the blocking finalizer on the rendered ConfigMap.
func hold(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	resource := cluster.Direct(t, "", "v1", "ConfigMap", targetNamespace)
	object, err := resource.Get(context.Background(), applicationName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the rendered ConfigMap: %v", err)
	}
	object.SetFinalizers(append(object.GetFinalizers(), holdFinalizer))
	if _, err := resource.Update(context.Background(), object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("hold the rendered ConfigMap: %v", err)
	}
}

// configMapExists reports whether the rendered object is still there.
func configMapExists(t *testing.T, cluster *harness.Cluster) bool {
	t.Helper()
	_, err := cluster.Direct(t, "", "v1", "ConfigMap", targetNamespace).Get(context.Background(), applicationName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatalf("read the rendered ConfigMap: %v", err)
	}
	return true
}

// phaseOf reads the stored phase.
func phaseOf(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read the phase: %v", err)
	}
	return phase
}

// blockedObjects reads the blocking list of the uninstall.
func blockedObjects(t *testing.T, cluster *harness.Cluster) []struct {
	Kind string
	Name string
} {
	t.Helper()
	status, found, err := unstructured.NestedSlice(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "uninstall", "blocked")
	if err != nil || !found {
		t.Fatalf("read the blocking list: found=%v err=%v", found, err)
	}
	objects := make([]struct {
		Kind string
		Name string
	}, 0, len(status))
	for _, item := range status {
		entry, _ := item.(map[string]any)
		kind, _ := entry["kind"].(string)
		name, _ := entry["name"].(string)
		objects = append(objects, struct {
			Kind string
			Name string
		}{Kind: kind, Name: name})
	}
	return objects
}

// stalledFlag reads the stalled marker.
func stalledFlag(t *testing.T, cluster *harness.Cluster) bool {
	t.Helper()
	stalled, _, err := unstructured.NestedBool(cluster.Get(t, "v1beta1", "Application", applicationName).Object, "status", "uninstall", "stalled")
	if err != nil {
		t.Fatalf("read the stalled marker: %v", err)
	}
	return stalled
}
