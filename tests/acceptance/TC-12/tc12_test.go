// Package tc12 verifies that one degradation reason resolves to a concrete
// object and evidence segment, that the chain is given segment by segment, that
// the extension object's segment sits where its adapter declares and carries the
// declared source, and that facts and inferences stay distinguishable (AC-6,
// TC-12).
package tc12

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/diagnostics"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC12EvidenceChain(t *testing.T) {
	cluster := harness.Start(t)
	world := harness.StartDiagnosticWorld(t, cluster)
	service := cluster.DiagnosticService(world.Installation)
	api := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Diagnostics = service
	})

	status, body, err := api.Raw(http.MethodGet, "/api/v1/diagnostics/"+world.Namespace+"/"+world.Application, nil, nil)
	if err != nil {
		t.Fatalf("read the evidence chain: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("read the evidence chain answered %d: %s", status, body)
	}
	chain := diagnostics.Chain{}
	if err := json.Unmarshal(body, &chain); err != nil {
		t.Fatalf("decode the evidence chain: %v", err)
	}

	if chain.Application.Kind != "Application" || chain.Application.Name != world.Application {
		t.Fatalf("chain application = %v", chain.Application)
	}

	// The kernel links keep C-26's order; extension segments sit at the link
	// their adapter declares and name extension objects instead.
	kernel := kernelLinks(chain)
	wantKernel := []diagnostics.Link{
		diagnostics.LinkApplication, diagnostics.LinkEndpoint, diagnostics.LinkRoute,
		diagnostics.LinkBackend, diagnostics.LinkWorkload, diagnostics.LinkNode,
	}
	if len(kernel) != len(wantKernel) {
		t.Fatalf("kernel links = %v, want %v", kernel, wantKernel)
	}
	for index, link := range wantKernel {
		if kernel[index] != link {
			t.Fatalf("kernel links = %v, want %v", kernel, wantKernel)
		}
	}

	// The backend segment follows the service the Ingress names, which differs
	// from the application name: the fallback can no longer hide a parse bug.
	backend := segmentOf(t, chain, func(segment diagnostics.Segment) bool {
		return segment.Link == diagnostics.LinkBackend
	})
	if backend.Object.Kind != "Service" || backend.Object.Name != "shop-backend" {
		t.Fatalf("backend segment object = %v, want the service the Ingress names", backend.Object)
	}

	// Every observable segment states the six elements and whether it is
	// observation or inference (C-26).
	bases := map[diagnostics.Basis]bool{}
	for _, segment := range chain.Segments {
		if !segment.Observable {
			t.Fatalf("segment %s is unobservable: %s", segment.Link, segment.Reason)
		}
		if len(segment.Evidence) == 0 {
			t.Fatalf("segment %s carries no evidence", segment.Link)
		}
		if segment.Object.Name == "" {
			t.Fatalf("segment %s names no object", segment.Link)
		}
		for _, entry := range segment.Evidence {
			if entry.Conclusion == "" || entry.Source == "" || entry.Object.Name == "" ||
				entry.ObservedAt.IsZero() || entry.RawState == "" || entry.Impact == "" || entry.NextAction == "" {
				t.Fatalf("segment %s carries an incomplete evidence entry: %+v", segment.Link, entry)
			}
			if entry.Basis != diagnostics.BasisFact && entry.Basis != diagnostics.BasisInference {
				t.Fatalf("segment %s evidence basis = %q", segment.Link, entry.Basis)
			}
			bases[entry.Basis] = true
		}
	}
	if !bases[diagnostics.BasisFact] || !bases[diagnostics.BasisInference] {
		t.Fatalf("evidence bases = %v, want both a fact and an inference", bases)
	}

	// The extension object inserts its segment at the declared link and names
	// the declared source (C-26).
	widget := segmentOf(t, chain, func(segment diagnostics.Segment) bool {
		return segment.Object.Group == harness.WidgetGroup && segment.Object.Kind == "Widget"
	})
	if widget.Object.Name != "shop-widget" {
		t.Fatalf("extension segment object = %v", widget.Object)
	}
	if widget.Link != diagnostics.LinkWorkload {
		t.Fatalf("extension segment link = %q, want the declared %q", widget.Link, diagnostics.LinkWorkload)
	}
	if widget.Evidence[0].Source != "widgets.example.test diagnostics" {
		t.Fatalf("extension evidence source = %q, want the declared source", widget.Evidence[0].Source)
	}
	if widget.Evidence[0].ObservedAt.IsZero() {
		t.Fatal("extension evidence carries no observation time")
	}

	// The adapter covers its own component, which becomes a segment of its own.
	component := segmentOf(t, chain, func(segment diagnostics.Segment) bool {
		return segment.Object.Kind == "SystemComponent"
	})
	if component.Object.Name != world.Component {
		t.Fatalf("component segment object = %v, want the system component", component.Object)
	}
	if component.Link != diagnostics.LinkWorkload {
		t.Fatalf("component segment link = %q, want the declared %q", component.Link, diagnostics.LinkWorkload)
	}
	if component.Evidence[0].Source != "widgets.example.test diagnostics" {
		t.Fatalf("component evidence source = %q", component.Evidence[0].Source)
	}

	// The reason points at the deepest observed object and segment.
	if chain.Conclusion.Basis != diagnostics.BasisFact {
		t.Fatalf("conclusion basis = %q, want a fact", chain.Conclusion.Basis)
	}
	if chain.Conclusion.Segment != diagnostics.LinkNode {
		t.Fatalf("conclusion segment = %q, want %q", chain.Conclusion.Segment, diagnostics.LinkNode)
	}
	if chain.Conclusion.Object.Kind != "Node" || chain.Conclusion.Object.Name != "node-a" {
		t.Fatalf("conclusion object = %v, want the NotReady node", chain.Conclusion.Object)
	}
	if chain.Conclusion.NextAction == "" {
		t.Fatal("conclusion names no next action")
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

// segmentOf returns the one segment a predicate selects.
func segmentOf(t *testing.T, chain diagnostics.Chain, match func(diagnostics.Segment) bool) diagnostics.Segment {
	t.Helper()
	for _, segment := range chain.Segments {
		if match(segment) {
			return segment
		}
	}
	t.Fatalf("no segment matched among %d segments", len(chain.Segments))
	return diagnostics.Segment{}
}
