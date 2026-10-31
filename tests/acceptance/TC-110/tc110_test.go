// Package tc110 disables an installed extension, observes that its controller,
// page, setting and capability stop while its object stays, then enables it
// again and observes reconciliation resume (AC-54, TC-110).
package tc110

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	vmGroup   = "kubevirt.example.test"
	namespace = "vms"
	component = "kubevirt"
)

// declaration contributes one class, its controller, a page, a setting and a
// capability.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "thirdparty.kubevirt",
		Version:    "1.0.0",
		APIGroup:   vmGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: vmGroup, Kind: "VirtualMachine", Resource: "virtualmachines",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Controllers: []extension.Controller{{
				ID:      "virtual-machine",
				Manages: []extension.GroupKind{{Group: vmGroup, Kind: "VirtualMachine"}},
			}},
			Capabilities: extension.CapabilityContributions{Items: []extension.Capability{{
				Name: "thirdparty.kubevirt/vm", Version: "v1",
			}}},
			Pages: []extension.Page{{
				ID: "virtual-machines", Title: "Virtual Machines", Location: "panel",
				Items: []extension.PageItem{{
					ID: "machines", Label: "Virtual Machines",
					Source: extension.PageSourceObject, Reference: "VirtualMachine",
					Reason: "acceptance",
				}},
			}},
			Settings:           []extension.Setting{{ID: "vm-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser}},
			CatalogueSources:   []extension.CatalogueSource{{ID: "vm-catalogue", Sync: "pull"}},
			DiscoveryAdapters:  []extension.DiscoveryAdapter{{ID: "vm-discovery", Covers: []extension.GroupKind{{Kind: "Node"}}}},
			DiagnosticAdapters: []extension.DiagnosticAdapter{{ID: "vm-diagnostics", Covers: []extension.GroupKind{{Group: vmGroup, Kind: "VirtualMachine"}}}},
			Preferences:        []extension.PreferenceSchema{{ID: "vm-view", Keys: []string{"layout"}}},
		},
	}
}

// TestTC110DisableAndEnableRoundTrip covers both halves of C-28.
func TestTC110DisableAndEnableRoundTrip(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, vmGroup, "v1", "VirtualMachine", "virtualmachines", true)
	cluster.EnsureNamespace(t, namespace)
	artifacts := extension.MemoryArtifacts{}

	var writes atomic.Int64
	controllers := extension.NewControllerSet(2)
	if err := controllers.Register(component, extension.Controller{
		ID:      "virtual-machine",
		Manages: []extension.GroupKind{{Group: vmGroup, Kind: "VirtualMachine"}},
	}, functionReconciler(func(context.Context, reconcile.Request) (reconcile.Result, error) {
		writes.Add(1)
		return reconcile.Result{}, nil
	})); err != nil {
		t.Fatalf("register the controller: %v", err)
	}
	runContext, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		controllers.Run(runContext)
	}()
	defer func() {
		stop()
		<-done
	}()

	// Install first, then wire the controller set into later passes.
	cluster.InstallPackage(t, component, declaration(), artifacts)
	installed := cluster.ExtensionStateUsing(t, artifacts, func(installer *extension.Installer) {
		installer.Controllers = controllers
	})
	if _, err := cluster.ExtensionResource(vmGroup, "v1", "virtualmachines", namespace).Create(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": vmGroup + "/v1",
		"kind":       "VirtualMachine",
		"metadata": map[string]any{
			"name": "vm-one", "namespace": namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the virtual machine: %v", err)
	}
	assertEnabled(t, installed.Registry(), write.RoleMember)
	waitForWrites(t, &writes, 0)
	if err := controllers.Enqueue(component, "virtual-machine", reconcile.Request{Kind: "VirtualMachine", Namespace: namespace, Name: "vm-one"}); err != nil {
		t.Fatalf("enqueue the enabled controller: %v", err)
	}
	waitForWrites(t, &writes, 1)

	// Disable: the contributions disappear and the controller stops.
	cluster.SetComponentDisabled(t, component, true)
	disabled := cluster.ExtensionStateUsing(t, artifacts, func(installer *extension.Installer) {
		installer.Controllers = controllers
	})
	result, found := disabled.Component(component)
	if !found || result.Phase != extension.PhaseDisabled {
		t.Fatalf("disabled component = %+v (found %v)", result, found)
	}
	assertDisabled(t, disabled.Registry())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := cluster.ExtensionResource(vmGroup, "v1", "virtualmachines", namespace).Get(ctx, "vm-one", metav1.GetOptions{}); err != nil {
		t.Fatalf("the disabled extension's object is gone: %v", err)
	}
	before := writes.Load()
	if err := controllers.Enqueue(component, "virtual-machine", reconcile.Request{Kind: "VirtualMachine", Namespace: namespace, Name: "vm-one"}); err != extension.ErrControllerDisabled {
		t.Fatalf("enqueue on a disabled controller = %v, want %v", err, extension.ErrControllerDisabled)
	}
	if writes.Load() != before {
		t.Fatalf("the disabled controller ran a round")
	}

	// Enable: the page returns and reconciliation resumes.
	cluster.SetComponentDisabled(t, component, false)
	enabled := cluster.ExtensionStateUsing(t, artifacts, func(installer *extension.Installer) {
		installer.Controllers = controllers
	})
	if result, found := enabled.Component(component); !found || result.Phase != extension.PhaseReady {
		t.Fatalf("enabled component = %+v (found %v)", result, found)
	}
	assertEnabled(t, enabled.Registry(), write.RoleMember)
	if err := controllers.Enqueue(component, "virtual-machine", reconcile.Request{Kind: "VirtualMachine", Namespace: namespace, Name: "vm-one"}); err != nil {
		t.Fatalf("enqueue the re-enabled controller: %v", err)
	}
	waitForWrites(t, &writes, before+1)
}

// assertEnabled checks the contributions of an active extension.
func assertEnabled(t *testing.T, registry *extension.Registry, role write.Role) {
	t.Helper()
	if !registry.IsServing(vmGroup, "VirtualMachine") {
		t.Fatal("the active extension's class is not served")
	}
	if _, found := registry.PageFor("virtual-machines", role); !found {
		t.Fatal("the active extension's page is not visible")
	}
	if _, found := registry.Setting("vm-endpoint"); !found {
		t.Fatal("the active extension's setting is missing")
	}
	if len(registry.ExtensionCapabilities()) != 1 {
		t.Fatal("the active extension's capability is not registered")
	}
	if len(registry.CatalogueSources()) != 1 || len(registry.DiscoveryAdapters()) != 1 ||
		len(registry.DiagnosticAdapters()) != 1 || len(registry.Preferences()) != 1 {
		t.Fatalf("the active extension's contributions = sources %d discovery %d diagnostics %d preferences %d",
			len(registry.CatalogueSources()), len(registry.DiscoveryAdapters()),
			len(registry.DiagnosticAdapters()), len(registry.Preferences()))
	}
	if _, found := registry.PreferenceKeys("thirdparty.kubevirt"); !found {
		t.Fatal("the active extension's preference domain is not declared")
	}
}

// assertDisabled checks that every contribution class is withdrawn.
func assertDisabled(t *testing.T, registry *extension.Registry) {
	t.Helper()
	if registry.IsServing(vmGroup, "VirtualMachine") {
		t.Fatal("a disabled extension still serves its class")
	}
	if len(registry.Pages()) != 0 {
		t.Fatal("a disabled extension still shows a page")
	}
	if len(registry.Settings()) != 0 {
		t.Fatal("a disabled extension still shows a setting")
	}
	if len(registry.ExtensionCapabilities()) != 0 {
		t.Fatal("a disabled extension still registers a capability")
	}
	if len(registry.Controllers()) != 0 {
		t.Fatal("a disabled extension still contributes a controller")
	}
	if len(registry.CatalogueSources()) != 0 || len(registry.DiscoveryAdapters()) != 0 ||
		len(registry.DiagnosticAdapters()) != 0 || len(registry.Preferences()) != 0 {
		t.Fatalf("a disabled extension still contributes sources %d discovery %d diagnostics %d preferences %d",
			len(registry.CatalogueSources()), len(registry.DiscoveryAdapters()),
			len(registry.DiagnosticAdapters()), len(registry.Preferences()))
	}
	if _, found := registry.PreferenceKeys("thirdparty.kubevirt"); found {
		t.Fatal("a disabled extension still declares its preference domain")
	}
}

// waitForWrites waits until the controller has run at least want rounds.
func waitForWrites(t *testing.T, writes *atomic.Int64, want int64) {
	t.Helper()
	harness.Eventually(t, 10*time.Second, "the controller to run", func() bool {
		return writes.Load() >= want
	})
}

// functionReconciler adapts a function to the reconcile loop.
type functionReconciler func(context.Context, reconcile.Request) (reconcile.Result, error)

// Reconcile implements reconcile.Reconciler.
func (f functionReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	return f(ctx, request)
}
