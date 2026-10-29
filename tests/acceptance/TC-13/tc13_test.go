// Package tc13 verifies that stopping the extension that provides a diagnostic
// adapter marks only that segment unobservable: the other segments are still
// given and the bundle still carries its content classes (AC-6, TC-13).
package tc13

import (
	"testing"

	"github.com/tsic404/kos/pkg/diagnostics"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC13AdapterUnavailable(t *testing.T) {
	cluster := harness.Start(t)
	world := harness.StartDiagnosticWorld(t, cluster)
	before := cluster.DiagnosticService(world.Installation)
	first, err := before.Chain(t.Context(), world.Namespace, world.Application)
	if err != nil {
		t.Fatalf("assemble the chain before stopping the extension: %v", err)
	}
	widget := widgetOf(t, first)
	if !widget.Observable {
		t.Fatal("the extension segment is unobservable while the adapter is active")
	}

	stopped := world.StopExtension(t, cluster)
	after := cluster.DiagnosticService(stopped)
	second, err := after.Chain(t.Context(), world.Namespace, world.Application)
	if err != nil {
		t.Fatalf("assemble the chain after stopping the extension: %v", err)
	}

	// The kernel links are unchanged: only the adapter's own segment changed.
	beforeKernel := kernelLinks(first)
	afterKernel := kernelLinks(second)
	if len(beforeKernel) != len(afterKernel) {
		t.Fatalf("kernel links = %v after the stop, want %v", afterKernel, beforeKernel)
	}
	for index, link := range beforeKernel {
		if afterKernel[index] != link {
			t.Fatalf("kernel links = %v after the stop, want %v", afterKernel, beforeKernel)
		}
	}
	for _, segment := range second.Segments {
		switch {
		case segment.Object.Kind == "Widget":
			if segment.Observable {
				t.Fatal("the extension segment is observable after its adapter stopped")
			}
			if segment.Reason == "" {
				t.Fatal("the unobservable segment names no reason")
			}
			if len(segment.Evidence) != 0 {
				t.Fatalf("the unobservable segment carries %d evidence entries", len(segment.Evidence))
			}
		case segment.Object.Kind == "SystemComponent":
			t.Fatalf("a component segment remains after its adapter stopped: %v", segment.Object)
		default:
			if !segment.Observable || len(segment.Evidence) == 0 {
				t.Fatalf("segment %s stopped being observable: %s", segment.Link, segment.Reason)
			}
		}
	}

	bundle, err := after.Bundle(t.Context(), world.Namespace, world.Application)
	if err != nil {
		t.Fatalf("assemble the bundle after stopping the extension: %v", err)
	}
	if len(bundle.ConditionSnapshot) == 0 {
		t.Fatal("the bundle carries no condition snapshot")
	}
	if !bundle.RecentEvents.Available || len(bundle.RecentEvents.Events) == 0 {
		t.Fatalf("the bundle carries no recent events: %+v", bundle.RecentEvents)
	}
	if !bundle.RenderDiff.Available {
		t.Fatalf("the bundle carries no render diff: %s", bundle.RenderDiff.Reason)
	}
	if len(bundle.Capabilities) == 0 {
		t.Fatal("the bundle carries no capability list")
	}
	if len(bundle.VersionMatrix.KernelAPIVersions) == 0 || len(bundle.VersionMatrix.Nodes) == 0 {
		t.Fatalf("the bundle carries no version matrix: %+v", bundle.VersionMatrix)
	}
	if len(bundle.Evidence.Segments) != len(second.Segments) {
		t.Fatalf("bundle chain segments = %d, want %d", len(bundle.Evidence.Segments), len(second.Segments))
	}
}

// kernelLinks returns the links of the segments that observed kernel objects,
// in chain order. Extension evidence shares a link with the kernel segment it
// attaches to, so the object decides which is which.
func kernelLinks(chain diagnostics.Chain) []diagnostics.Link {
	kinds := map[string]bool{
		"Application": true, "Endpoint": true, "Ingress": true,
		"Service": true, "Deployment": true, "Node": true,
	}
	links := make([]diagnostics.Link, 0, len(chain.Segments))
	for _, segment := range chain.Segments {
		if kinds[segment.Object.Kind] {
			links = append(links, segment.Link)
		}
	}
	return links
}

// widgetOf returns the segment of the extension object on the chain.
func widgetOf(t *testing.T, chain diagnostics.Chain) diagnostics.Segment {
	t.Helper()
	for _, segment := range chain.Segments {
		if segment.Object.Group == harness.WidgetGroup && segment.Object.Kind == "Widget" {
			return segment
		}
	}
	t.Fatalf("the chain carries no %s/Widget segment", harness.WidgetGroup)
	return diagnostics.Segment{}
}
