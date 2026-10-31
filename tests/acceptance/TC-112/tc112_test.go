// Package tc112 installs a package whose target framework version this kernel
// does not serve: the install is refused, the incompatible item is named, and
// no object class, controller, capability or page appears (AC-55, TC-112).
package tc112

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	vmGroup   = "kubevirt.example.test"
	component = "kubevirt"
)

// requestTimeout bounds every direct cluster call of the case.
const requestTimeout = 30 * time.Second

// declaration targets a framework version this kernel does not serve.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier:       "thirdparty.kubevirt",
		Version:          "1.0.0",
		FrameworkVersion: ">=2.0",
		APIGroup:         vmGroup,
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
		},
	}
}

// TestTC112IncompatibleFrameworkIsRefused checks the refusal and its evidence.
func TestTC112IncompatibleFrameworkIsRefused(t *testing.T) {
	cluster := harness.Start(t)
	artifacts := extension.MemoryArtifacts{}
	kindsBefore := append([]string(nil), api.Kinds()...)

	installed := cluster.InstallPackage(t, component, declaration(), artifacts)
	result, found := installed.Component(component)
	if !found {
		t.Fatal("the refused component is missing from the pass")
	}
	if result.Phase != extension.PhaseIncompatible || result.Reason != extension.ReasonIncompatibleFramework {
		t.Fatalf("component = %s/%s, want incompatible", result.Phase, result.Reason)
	}
	if len(result.Incompatible) == 0 || !strings.Contains(result.Incompatible[0], ">=2.0") {
		t.Fatalf("incompatible items = %v", result.Incompatible)
	}

	registry := installed.Registry()
	if len(registry.ObjectClasses()) != 0 || len(registry.Controllers()) != 0 ||
		len(registry.ExtensionCapabilities()) != 0 || len(registry.Pages()) != 0 {
		t.Fatalf("an incompatible extension contributed: classes %v controllers %v capabilities %v pages %v",
			registry.ObjectClasses(), registry.Controllers(), registry.ExtensionCapabilities(), registry.Pages())
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}

	// No capability registration is written.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "Provider", "").Get(ctx, component, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the refused install registered a provider (err %v)", err)
	}

	// The stored status names the incompatible target range.
	status := cluster.ComponentStatus(t, component)
	if phase := harness.String(status, "phase"); phase != extension.PhaseIncompatible {
		t.Fatalf("stored phase = %q", phase)
	}
	conditions := harness.Conditions(t, cluster.Get(t, "v1beta1", "SystemComponent", component))
	foundCondition := false
	for _, condition := range conditions {
		if condition.Type != extension.ConditionFrameworkCompatible {
			continue
		}
		foundCondition = condition.Status == metav1.ConditionFalse &&
			condition.Reason == extension.ReasonIncompatibleFramework &&
			strings.Contains(condition.Message, ">=2.0")
	}
	if !foundCondition {
		t.Fatalf("no FrameworkCompatible=False condition naming the target: %+v", conditions)
	}
}
