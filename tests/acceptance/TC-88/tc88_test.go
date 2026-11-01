// Package tc88 verifies the component and storage failures of AC-43 and C-56:
// a crashed provider degrades its dependent applications without reinstalling
// them, a crashed extension controller makes only its own objects Unknown, a
// corrupted database does not stop reconciliation and is restorable from an
// export, a full disk fails the application and refuses new volumes, and an
// object definition that stops serving the required version makes the control
// plane refuse to start.
package tc88

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/app/kostaskmanager"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// template renders one Deployment, so health has observed replicas.
const template = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 1
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
          image: busybox
`

// configMapTemplate renders one object, which is all the disk-full flow needs
// before it refuses to install.
const configMapTemplate = `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
data:
  mode: acceptance
`

const (
	dependentApplication = "needs"
	plainApplication     = "plain"
)

func TestTC88ComponentAndStorageFailures(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)

	t.Run("provider crash", func(t *testing.T) {
		providerCrash(t, cluster)
	})
	t.Run("extension controller crash", func(t *testing.T) {
		controllerCrash(t, cluster)
	})
	t.Run("database corruption", func(t *testing.T) {
		databaseCorruption(t, cluster)
	})
	t.Run("disk full", func(t *testing.T) {
		diskFull(t, cluster)
	})
	t.Run("definition stops serving", func(t *testing.T) {
		definitionUnserved(t, cluster)
	})
}

// providerCrash marks a provider unavailable and checks its dependent
// application is Degraded while an unrelated one stays Healthy.
func providerCrash(t *testing.T, cluster *harness.Cluster) {
	ctx := context.Background()
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "dependent", template, nil,
		v1beta1.CapabilityRequirement{Name: "storage.volume", Version: "v1"}))
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "plain", template, nil))
	cluster.CreateClusterObject(t, api.Group, "v1beta1", "providers", "", map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Provider",
		"metadata": map[string]any{
			"name":   "local",
			"labels": harness.ManagedLabels(),
		},
		"spec": map[string]any{
			"version": "1.0.0",
			"capabilities": []any{map[string]any{
				"name": "storage.volume", "version": "v1",
				"caseSet": "storage.volume.v1", "caseSetDigest": "sha256:acceptance",
			}},
		},
	})
	if _, err := cluster.ApplyStatus(ctx, "v1beta1", "Provider", "", "local", map[string]any{
		"health": map[string]any{"state": string(v1beta1.HealthStateUnavailable)},
	}); err != nil {
		t.Fatalf("mark the provider unavailable: %v", err)
	}

	createApplication(t, cluster, dependentApplication, "dependent")
	createApplication(t, cluster, plainApplication, "plain")
	controller := cluster.ApplicationController(t, app.Options{
		Capabilities: app.StaticCapabilities{{
			Name: "storage.volume", Version: "v1",
			Scope: v1beta1.CapabilityScopeCluster, Verified: true,
		}},
	})
	for _, name := range []string{dependentApplication, plainApplication} {
		if phase := cluster.SettleApplication(t, controller, name, 10); phase != app.PhaseReady {
			t.Fatalf("%s phase = %s, want Ready", name, phase)
		}
		cluster.SetWorkloadStatus(t, "Deployment", harness.Namespace, name, 1, 1)
		cluster.ApplicationRound(t, controller, name)
	}
	if got := healthOf(t, cluster, dependentApplication); got != v1beta1.HealthStateDegraded {
		t.Fatalf("dependent health = %s, want Degraded", got)
	}
	if got := healthOf(t, cluster, plainApplication); got != v1beta1.HealthStateHealthy {
		t.Fatalf("unrelated health = %s, want Healthy", got)
	}
	applicationStatus := cluster.ApplicationStatusOf(t, dependentApplication)
	if !hasCondition(applicationStatus.Conditions, "CapabilityUnsatisfied") {
		t.Fatalf("conditions = %v, want the unsatisfied capability name", applicationStatus.Conditions)
	}

	// The dependent application is not reinstalled automatically: its workload
	// keeps its identity across rounds while the provider is down.
	before := cluster.Workload(t, "Deployment", harness.Namespace, dependentApplication)
	cluster.ApplicationRound(t, controller, dependentApplication)
	if after := cluster.Workload(t, "Deployment", harness.Namespace, dependentApplication); after.GetUID() != before.GetUID() {
		t.Fatalf("the crashed provider reinstalled the application: uid %s became %s", before.GetUID(), after.GetUID())
	}

	// The reinstall entry the page offers is executable: read the page, take
	// the action identity the item declares, resolve it to the value the API
	// accepts, submit that through the entry, and observe the action on the
	// object (C-22, C-56).
	page := applicationPage(t, cluster)
	assertPageActionsAccepted(t, page)
	item, action := pageAction(t, page, kostaskmanager.ItemReinstall)
	if item.Entry.Method != http.MethodPost || item.Entry.Path != "/api/v1/action-requests" {
		t.Fatalf("reinstall entry = %s %s, want POST /api/v1/action-requests", item.Entry.Method, item.Entry.Path)
	}
	gateway := cluster.StartControlPlane(t, nil)
	httpStatus, body := gateway.Call(t, item.Entry.Method, item.Entry.Path, map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": dependentApplication,
		"action": action,
	}, nil)
	if httpStatus != http.StatusAccepted {
		t.Fatalf("reinstall action: %d %v", httpStatus, body)
	}
	recordName := harness.String(body, "ref", "name")
	if recordName == "" {
		t.Fatalf("the accepted reinstall carries no record reference: %v", body)
	}
	cluster.ApplicationRound(t, controller, dependentApplication)
	if recordOf(t, cluster, recordName) != nil {
		t.Fatalf("the reinstall record %s was not consumed", recordName)
	}
	event := reinstallEvent(t, cluster)
	if event == nil {
		t.Fatalf("the reinstall produced no %s event", app.EventReinstallRequested)
	}
	if phase := cluster.ApplicationPhase(t, dependentApplication); phase != app.PhaseReady {
		t.Fatalf("phase after the reinstall = %s, want Ready", phase)
	}
	if got := healthOf(t, cluster, dependentApplication); got != v1beta1.HealthStateDegraded {
		t.Fatalf("health after the reinstall = %s, want Degraded while the provider is down", got)
	}
	if after := cluster.Workload(t, "Deployment", harness.Namespace, dependentApplication); after.GetUID() != before.GetUID() {
		t.Fatalf("the reinstall recreated the workload: uid %s became %s", before.GetUID(), after.GetUID())
	}
}

// applicationPage reads the application page the taskmanager declares, as the
// shell receives it (C-22).
func applicationPage(t *testing.T, cluster *harness.Cluster) pages.Page {
	t.Helper()
	installed := cluster.InstallPackage(t, "taskmanager", kostaskmanager.Declaration(), extension.MemoryArtifacts{})
	readings := &kostaskmanager.Readings{Dynamic: cluster.Dynamic}
	service := pages.New(installed.Registry(), pages.Config{
		Readings: pages.Router{kostaskmanager.Identifier: readings},
		Now:      time.Now,
	})
	page, failure := service.Read(context.Background(), identity.Session{Username: "owner", Role: write.RoleOwner}, kostaskmanager.PageApplications, pages.Target{})
	if failure != nil {
		t.Fatalf("read the application page: %v", failure)
	}
	return page
}

// pageAction returns one item and the control-plane action value its page
// action identity submits. It fails when the item offers no action, when the
// identity has no control-plane value, and when the API rejects that value, so
// a page entry the API cannot accept never passes unnoticed (C-22, C-56).
func pageAction(t *testing.T, page pages.Page, itemID string) (pages.Item, string) {
	t.Helper()
	for _, item := range page.Items {
		if item.ID != itemID {
			continue
		}
		if item.Action == "" || item.Entry == nil {
			t.Fatalf("item %q offers no action: %+v", itemID, item)
		}
		action, known := kostaskmanager.ControlPlaneAction(item.Action)
		if !known {
			t.Fatalf("page action %q has no control-plane action", item.Action)
		}
		if !controlplane.AcceptsAction(action) {
			t.Fatalf("page action %q submits %q, which the API rejects", item.Action, action)
		}
		return item, action
	}
	t.Fatalf("the application page offers no %q item", itemID)
	return pages.Item{}, ""
}

// assertPageActionsAccepted checks every entry that posts to the action
// endpoint: the page action identity must resolve to a value the API accepts,
// and the lifecycle entries must be offered at all (C-22, C-56).
func assertPageActionsAccepted(t *testing.T, page pages.Page) {
	t.Helper()
	posted := map[string]bool{}
	for _, action := range page.Actions {
		if action.Entry.Path != "/api/v1/action-requests" {
			continue
		}
		value, known := kostaskmanager.ControlPlaneAction(action.ID)
		if !known {
			t.Errorf("page action %q has no control-plane action", action.ID)
			continue
		}
		if !controlplane.AcceptsAction(value) {
			t.Errorf("page action %q submits %q, which the API rejects", action.ID, value)
		}
		posted[action.ID] = true
	}
	for _, id := range []string{
		kostaskmanager.ActionStop,
		kostaskmanager.ActionStart,
		kostaskmanager.ActionRestart,
		kostaskmanager.ActionScale,
		kostaskmanager.ActionReinstall,
	} {
		if !posted[id] {
			t.Errorf("the page does not offer %q through the action endpoint", id)
		}
	}
}

// recordOf reads one action record; nil when it is gone.
func recordOf(t *testing.T, cluster *harness.Cluster, name string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	object, err := cluster.Direct(t, "", "v1", "ConfigMap", api.SystemNamespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the action record %s: %v", name, err)
	}
	return object.Object
}

// reinstallEvent reads the event a reinstall wrote on the application.
func reinstallEvent(t *testing.T, cluster *harness.Cluster) *corev1.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events, err := cluster.Kube.CoreV1().Events(harness.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for index := range events.Items {
		event := &events.Items[index]
		if event.Reason == app.EventReinstallRequested && event.InvolvedObject.Name == dependentApplication {
			return event
		}
	}
	return nil
}

// controllerCrash marks one extension controller unavailable and checks only
// the objects it manages turn Unknown while the other controller keeps running.
func controllerCrash(t *testing.T, cluster *harness.Cluster) {
	const group = "demo.example.test"
	cluster.InstallCRD(t, group, "v1", "Widget", "widgets", true)
	cluster.InstallCRD(t, group, "v1", "Gadget", "gadgets", true)
	declaration := extension.Declaration{
		Identifier: "demo.extension",
		Version:    "1.0.0",
		APIGroup:   group,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{
				{Group: group, Kind: "Widget", Resource: "widgets", Versions: []string{"v1"}, Namespaced: true},
				{Group: group, Kind: "Gadget", Resource: "gadgets", Versions: []string{"v1"}, Namespaced: true},
			},
			Controllers: []extension.Controller{
				{ID: "widget-controller", Manages: []extension.GroupKind{{Group: group, Kind: "Widget"}}},
				{ID: "gadget-controller", Manages: []extension.GroupKind{{Group: group, Kind: "Gadget"}}},
			},
		},
	}

	var gadgetWrites atomic.Int64
	controllers := extension.NewControllerSet(1)
	if err := controllers.Register("demo", declaration.Contributions.Controllers[0], functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		return reconcile.Result{}, errors.New("widget controller crashed")
	})); err != nil {
		t.Fatalf("register the widget controller: %v", err)
	}
	if err := controllers.Register("demo", declaration.Contributions.Controllers[1], functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		gadgetWrites.Add(1)
		return reconcile.Result{}, nil
	})); err != nil {
		t.Fatalf("register the gadget controller: %v", err)
	}
	runContext, stop := context.WithCancel(context.Background())
	defer stop()
	go controllers.Run(runContext)

	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackage(t, "demo", declaration, artifacts)
	installed := cluster.ExtensionStateUsing(t, artifacts, func(installer *extension.Installer) {
		installer.Controllers = controllers
	})

	createExtensionObject(t, cluster, group, "widgets", "Widget", "w1")
	createExtensionObject(t, cluster, group, "gadgets", "Gadget", "g1")

	if err := controllers.Enqueue("demo", "widget-controller", reconcile.Request{Kind: "Widget", Namespace: harness.Namespace, Name: "w1"}); err != nil {
		t.Fatalf("enqueue the widget controller: %v", err)
	}
	eventually(t, 10*time.Second, "the widget controller to be unavailable", func() bool {
		state, found := controllers.State("demo", "widget-controller")
		return found && !state.IsAvailable
	})
	if err := controllers.Enqueue("demo", "gadget-controller", reconcile.Request{Kind: "Gadget", Namespace: harness.Namespace, Name: "g1"}); err != nil {
		t.Fatalf("enqueue the gadget controller: %v", err)
	}
	eventually(t, 10*time.Second, "the healthy controller to run", func() bool { return gadgetWrites.Load() > 0 })

	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Extensions = installed.Registry()
		config.Controllers = controllers
	})
	widget := extensionObject(t, gateway, group, "Widget", "w1")
	if health, _ := widget["status"].(map[string]any)["health"].(map[string]any)["state"].(string); health != string(v1beta1.HealthStateUnknown) {
		t.Fatalf("crashed controller object health = %q, want Unknown", health)
	}
	gadget := extensionObject(t, gateway, group, "Gadget", "g1")
	if health, _ := gadget["status"].(map[string]any)["health"].(map[string]any)["state"].(string); health != string(v1beta1.HealthStateHealthy) {
		t.Fatalf("unrelated controller object health = %q, want Healthy", health)
	}
}

// databaseCorruption corrupts the control-plane database, checks login is
// impossible while reconciliation continues, then restores the export.
func databaseCorruption(t *testing.T, cluster *harness.Cluster) {
	ctx := context.Background()
	surface := harness.OpenIdentity(t, nil, nil)
	password := surface.BootstrapOwner(t, "owner")

	exporter := identity.Exporter{Store: surface.Store, Target: t.TempDir(), Now: surface.Clock.Now}
	result, err := exporter.Export(ctx)
	if err != nil {
		t.Fatalf("export the database: %v", err)
	}
	if result.Path == "" {
		t.Fatal("the export carries no path")
	}

	if err := surface.Store.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}
	if err := os.WriteFile(surface.DatabasePath, []byte("not a database"), 0o600); err != nil {
		t.Fatalf("corrupt the database: %v", err)
	}
	if _, err := identity.Open(ctx, identity.OpenOptions{Path: surface.DatabasePath}); err == nil {
		t.Fatal("the corrupted database opened")
	}

	// The cluster and its reconciliation do not depend on the database.
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "offline-db", template, nil))
	createApplication(t, cluster, "offline", "offline-db")
	controller := cluster.ApplicationController(t, app.Options{})
	if phase := cluster.SettleApplication(t, controller, "offline", 10); phase != app.PhaseReady {
		t.Fatalf("phase with a corrupted database = %s, want Ready", phase)
	}

	// The export restores the account: login works again.
	if err := identity.Restore(ctx, result.Path, surface.DatabasePath); err != nil {
		t.Fatalf("restore the database: %v", err)
	}
	restored, err := identity.Open(ctx, identity.OpenOptions{Path: surface.DatabasePath})
	if err != nil {
		t.Fatalf("open the restored database: %v", err)
	}
	defer restored.Close()
	service := identity.New(restored, identity.Config{})
	login, err := service.Login(ctx, "owner", password)
	if err != nil {
		t.Fatalf("login on the restored database: %v", err)
	}
	if login.Session.Username != "owner" {
		t.Fatalf("login session = %+v, want the owner", login.Session)
	}
}

// diskFull makes the volume claim fail with a full disk, checks the volume and
// application carry the reason, and checks the API refuses a new volume.
func diskFull(t *testing.T, cluster *harness.Cluster) {
	ctx := context.Background()
	cluster.InstallDefinition(t, harness.TemplateDefinition(t, "full-disk", configMapTemplate, nil))
	cluster.Create(t, harness.Volume(harness.Namespace, "full", map[string]any{
		"mobility": string(v1beta1.VolumeMobilityClusterMovable),
		"capacity": "1Gi",
	}))
	manifest := harness.ApplicationManifest("fails", harness.Namespace, "full-disk", harness.Namespace, nil, []string{"full"})
	cluster.Create(t, &unstructured.Unstructured{Object: manifest})

	store := fullStore{Store: volume.NewWriterStore(cluster.Writer())}
	volumes := volume.NewController(store, volume.Options{})
	harness.ReconcileVolume(t, volumes, harness.Namespace, "full")
	condition := volumeCondition(t, cluster, "full", volume.ConditionStorageFull)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("volume conditions carry no %s: %v", volume.ConditionStorageFull, condition)
	}

	controller := cluster.ApplicationController(t, app.Options{})
	cluster.ApplicationRound(t, controller, "fails")
	if phase := cluster.ApplicationPhase(t, "fails"); phase != app.PhaseFailed {
		t.Fatalf("phase = %s, want Failed", phase)
	}
	failure := applicationCondition(t, cluster, "fails", "StorageFull")
	if failure == nil || !strings.Contains(failure.Message, "disk is full") {
		t.Fatalf("application failure = %+v, want the disk-full reason", failure)
	}

	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.StorageCapacity = fullDisk{}
	})
	body := harness.Volume(harness.Namespace, "refused", map[string]any{
		"mobility": string(v1beta1.VolumeMobilityClusterMovable),
		"capacity": "1Gi",
	}).Object
	status, response := gateway.Call(t, http.MethodPost, "/api/v1/resources/Volume", body, nil)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("new volume status = %d, want 503: %v", status, response)
	}
	if code := harness.String(response, "error", "code"); code != controlplane.CodeStorageFull {
		t.Fatalf("new volume code = %q, want %q", code, controlplane.CodeStorageFull)
	}
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Volume", harness.Namespace).Get(ctx, "refused", metav1.GetOptions{}); err == nil {
		t.Fatal("the refused volume was created")
	}
}

// definitionUnserved removes one object definition and checks the control plane
// refuses to start and names the required version.
func definitionUnserved(t *testing.T, cluster *harness.Cluster) {
	ctx := context.Background()
	resource := cluster.APIExtensions.ApiextensionsV1().CustomResourceDefinitions()
	crd, err := resource.Get(ctx, "applications."+api.Group, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the application definition: %v", err)
	}
	for index := range crd.Spec.Versions {
		version := &crd.Spec.Versions[index]
		switch version.Name {
		case controlplane.RequiredVersion:
			version.Served = false
			version.Storage = false
		default:
			version.Storage = true
		}
	}
	if _, err := resource.Update(ctx, crd, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("stop serving %s: %v", controlplane.RequiredVersion, err)
	}
	eventually(t, 10*time.Second, "the definitions to stop serving the required version", func() bool {
		stored, err := resource.Get(ctx, "applications."+api.Group, metav1.GetOptions{})
		if err != nil {
			return false
		}
		for _, version := range stored.Spec.Versions {
			if version.Name == controlplane.RequiredVersion && !version.Served {
				return true
			}
		}
		return false
	})
	_, err = controlplane.New(controlplane.Config{
		Dynamic:     cluster.Dynamic,
		Definitions: definitionVersions{cluster: cluster},
	})
	if err == nil {
		t.Fatal("the control plane started without the application definition")
	}
	if !strings.Contains(err.Error(), controlplane.RequiredVersion) {
		t.Fatalf("refusal %q does not name the required version %s", err, controlplane.RequiredVersion)
	}
	if !strings.Contains(err.Error(), "Application") {
		t.Fatalf("refusal %q does not name the missing definition", err)
	}
}

// definitionVersions reads which kernel kinds a group/version still serves.
type definitionVersions struct {
	cluster *harness.Cluster
}

func (d definitionVersions) MissingDefinitions(ctx context.Context, group, version string, kinds []string) ([]string, error) {
	crdList, err := d.cluster.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	served := map[string]bool{}
	for _, crd := range crdList.Items {
		if crd.Spec.Group != group {
			continue
		}
		for _, offered := range crd.Spec.Versions {
			if offered.Name == version && offered.Served {
				served[crd.Spec.Names.Kind] = true
			}
		}
	}
	var missing []string
	for _, kind := range kinds {
		if !served[kind] {
			missing = append(missing, kind)
		}
	}
	return missing, nil
}

// functionReconciler adapts a function to the reconcile loop.
type functionReconciler func(context.Context, reconcile.Request) (reconcile.Result, error)

func (f functionReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	return f(ctx, request)
}

// fullStore refuses every claim with the kernel's disk-full error.
type fullStore struct {
	volume.Store
}

func (s fullStore) CreateClaim(_ context.Context, _ *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return nil, errors.New(`create claim: no space left on device`)
}

// fullDisk reports a full backing disk to the API.
type fullDisk struct{}

func (fullDisk) IsFull(context.Context) (bool, string, error) {
	return true, "节点 node-a 的磁盘已满", nil
}

// createApplication writes one application through the native path.
func createApplication(t *testing.T, cluster *harness.Cluster, name, definition string) {
	t.Helper()
	manifest := harness.ApplicationManifest(name, harness.Namespace, definition, harness.Namespace, nil, nil)
	cluster.Create(t, &unstructured.Unstructured{Object: manifest})
}

// createExtensionObject writes one extension object with an observed status.
func createExtensionObject(t *testing.T, cluster *harness.Cluster, group, resource, kind, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": harness.Namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": string(v1beta1.HealthStateHealthy)},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Applied", "message": "observed",
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			}},
		},
	}}
	if _, err := cluster.ExtensionResource(group, "v1", resource, harness.Namespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s %s: %v", kind, name, err)
	}
}

// extensionObject reads one extension object through the API projection.
func extensionObject(t *testing.T, gateway *harness.ControlPlane, group, kind, name string) map[string]any {
	t.Helper()
	status, body := gateway.Call(t, http.MethodGet,
		"/api/v1/resources/"+kind+"?apiVersion="+group+"/v1&namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read %s: %d %v", kind, status, body)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("%s projections = %v, want one", kind, items)
	}
	item, _ := items[0].(map[string]any)
	object, _ := item["object"].(map[string]any)
	return object
}

// healthOf reads the stored application health.
func healthOf(t *testing.T, cluster *harness.Cluster, name string) v1beta1.HealthState {
	t.Helper()
	return cluster.ApplicationStatusOf(t, name).Health.State
}

// hasCondition reports one condition type in the list.
func hasCondition(conditions []metav1.Condition, conditionType string) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return true
		}
	}
	return false
}

// volumeCondition reads one condition of a volume.
func volumeCondition(t *testing.T, cluster *harness.Cluster, name, conditionType string) *metav1.Condition {
	t.Helper()
	status := cluster.Get(t, "v1beta1", "Volume", name)
	return conditionOf(t, status, conditionType)
}

// applicationCondition reads one condition of an application.
func applicationCondition(t *testing.T, cluster *harness.Cluster, name, conditionType string) *metav1.Condition {
	t.Helper()
	return conditionOf(t, cluster.Get(t, "v1beta1", "Application", name), conditionType)
}

// conditionOf decodes one condition from a stored object.
func conditionOf(t *testing.T, object *unstructured.Unstructured, conditionType string) *metav1.Condition {
	t.Helper()
	conditions, _, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil {
		t.Fatalf("read conditions of %s: %v", object.GetName(), err)
	}
	for _, raw := range conditions {
		encoded, _ := raw.(map[string]any)
		if encoded["type"] != conditionType {
			continue
		}
		condition := metav1.Condition{
			Type:    conditionType,
			Status:  metav1.ConditionStatus(text(encoded["status"])),
			Reason:  text(encoded["reason"]),
			Message: text(encoded["message"]),
		}
		return &condition
	}
	return nil
}

// text reads a string field.
func text(value any) string {
	text, _ := value.(string)
	return text
}

// eventually waits until check holds.
func eventually(t *testing.T, timeout time.Duration, description string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, description)
}
