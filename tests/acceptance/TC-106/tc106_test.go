// Package tc106 installs a third-party product package shaped like a virtual
// machine manager: its object class, controller, capability and page appear
// without changing the kernel, its object references an existing volume, and
// the page carries the source its declaration names (AC-53, TC-106).
package tc106

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	vmGroup   = "kubevirt.example.test"
	namespace = "vms"
	volume    = "data"
)

// requestTimeout bounds every direct cluster call of the case.
const requestTimeout = 30 * time.Second

// declaration is the virtual machine manager package.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier:       "thirdparty.kubevirt",
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		APIGroup:         vmGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: vmGroup, Kind: "VirtualMachine", Resource: "virtualmachines",
				Versions: []string{"v1"}, Namespaced: true,
				References: []string{"spec.volumeRef"},
			}},
			Controllers: []extension.Controller{{
				ID:      "virtual-machine",
				Manages: []extension.GroupKind{{Group: vmGroup, Kind: "VirtualMachine"}},
				Scope: extension.WriteScope{
					Groups:     []string{vmGroup},
					Kinds:      []extension.GroupKind{{Kind: "Service"}},
					Namespaces: []string{namespace},
				},
			}},
			Capabilities: extension.CapabilityContributions{Items: []extension.Capability{{
				Name: "thirdparty.kubevirt/vm", Version: "v1", Scope: v1beta1.CapabilityScopeCluster,
			}}},
			Pages: []extension.Page{{
				ID: "virtual-machines", Title: "Virtual Machines", Location: "panel",
				RequiredRole: string(write.RoleMember),
				Items: []extension.PageItem{{
					ID: "machines", Label: "Virtual Machines",
					Source: extension.PageSourceObject, Reference: "VirtualMachine",
					Action: "start",
				}},
				Actions: []extension.Action{{
					ID: "start", Method: http.MethodPost,
					Path: "/api/v1/resources/VirtualMachine",
				}},
			}},
		},
	}
}

// TestTC106ProductInstallsWithoutChangingTheKernel checks the install, the
// resolved reference and the page source.
func TestTC106ProductInstallsWithoutChangingTheKernel(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, vmGroup, "v1", "VirtualMachine", "virtualmachines", true)
	cluster.EnsureNamespace(t, namespace)
	artifacts := extension.MemoryArtifacts{}
	kindsBefore := append([]string(nil), api.Kinds()...)
	cluster.CreateIn(t, namespace, harness.Volume(namespace, volume, map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": "standard",
	}))

	// Before the install the class definition exists, but the framework serves
	// neither the class nor a page.
	before := cluster.ExtensionState(t, artifacts)
	if len(before.Registry().ObjectClasses()) != 0 || len(before.Registry().Pages()) != 0 {
		t.Fatalf("before the install the framework serves %v and pages %v",
			before.Registry().ObjectClasses(), before.Registry().Pages())
	}
	beforeAPI := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Extensions = before.Registry()
	})
	status, body := beforeAPI.Call(t, http.MethodGet, readPath(), nil, nil)
	if status != http.StatusUnprocessableEntity || harness.String(body, "error", "code") != controlplane.CodeCapabilityMissing {
		t.Fatalf("read the uninstalled class: status %d, body %v", status, body)
	}

	installed := cluster.InstallPackage(t, "kubevirt", declaration(), artifacts)
	result, found := installed.Component("kubevirt")
	if !found || result.Phase != extension.PhaseReady {
		t.Fatalf("component = %+v (found %v), want ready", result, found)
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
	if class, ok := installed.Registry().ObjectClass(vmGroup, "VirtualMachine"); !ok {
		t.Fatal("the product object class is not served after the install")
	} else if !class.Namespaced {
		t.Fatal("the product object class lost its namespace scope")
	}
	page, visible := installed.Registry().PageFor("virtual-machines", write.RoleMember)
	if !visible {
		t.Fatal("the product page is not visible after the install")
	}
	if len(page.Items) != 1 || page.Items[0].Source != extension.PageSourceObject {
		t.Fatalf("page items = %+v, want one declared object source", page.Items)
	}

	// The product object is created through the control plane and names the
	// existing volume.
	control := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Extensions = installed.Registry()
	})
	manifest := map[string]any{
		"apiVersion": vmGroup + "/v1",
		"kind":       "VirtualMachine",
		"metadata":   map[string]any{"name": "vm-one", "namespace": namespace},
		"spec": map[string]any{"volumeRef": map[string]any{
			"apiVersion": api.Group + "/v1beta1", "kind": "Volume", "name": volume,
		}},
	}
	if status, body := control.Call(t, http.MethodPost, "/api/v1/resources/VirtualMachine?namespace="+namespace, manifest, nil); status != http.StatusAccepted {
		t.Fatalf("create the virtual machine: status %d, body %v", status, body)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	stored, err := cluster.ExtensionResource(vmGroup, "v1", "virtualmachines", namespace).Get(ctx, "vm-one", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the virtual machine: %v", err)
	}
	reference, found, err := unstructured.NestedString(stored.Object, "spec", "volumeRef", "name")
	if err != nil || !found || reference != volume {
		t.Fatalf("stored volume reference = %q (found %v, err %v)", reference, found, err)
	}

	// The declarative reference resolves to the volume it names (C-19).
	referrers, err := extension.Referrers(ctx, cluster.Dynamic, installed.Registry(), write.Ref{
		Group: api.Group, Version: "v1beta1", Kind: "Volume", Namespace: namespace, Name: volume,
	})
	if err != nil {
		t.Fatalf("resolve references to the volume: %v", err)
	}
	if len(referrers) != 1 || referrers[0].Kind != "VirtualMachine" || referrers[0].Name != "vm-one" {
		t.Fatalf("referrers = %v, want the virtual machine", referrers)
	}
}

// readPath reads one page of the product class.
func readPath() string {
	return "/api/v1/resources/VirtualMachine?apiVersion=" + vmGroup + "/v1&namespace=" + namespace
}
