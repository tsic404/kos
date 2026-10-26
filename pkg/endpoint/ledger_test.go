package endpoint

import (
	"testing"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// entry builds a managed endpoint entry.
func entry(name string, spec v1beta1.EndpointSpec) Entry {
	return Entry{
		Ref:         write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Endpoint", Namespace: "default", Name: name},
		Spec:        spec,
		Attribution: markers.AttributionManaged,
	}
}

// conflictKinds lists the kinds of a conflict list.
func conflictKinds(conflicts []Conflict) []string {
	kinds := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		kinds = append(kinds, conflict.Kind)
	}
	return kinds
}

func TestLedgerRefusesOneKeyHeldTwice(t *testing.T) {
	ledger := NewLedger()
	ledger.Put(entry("first", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 8080}))
	second := entry("second", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 8080})
	ledger.Put(second)

	for _, name := range []string{"first", "second"} {
		conflicts := ledger.Conflicts(entry(name, v1beta1.EndpointSpec{}).Ref)
		if len(conflicts) != 1 || conflicts[0].Kind != ConflictDuplicateKey || conflicts[0].Key != "tcp/8080" {
			t.Fatalf("%s conflicts = %v, want one duplicate key", name, conflicts)
		}
		if len(conflicts[0].With) != 1 {
			t.Fatalf("%s conflicts name %v, want the other endpoint", name, conflicts[0].With)
		}
	}

	// TCP and UDP are separate spaces: the same number coexists.
	ledger.Put(entry("udp", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindUDP, TargetApp: "web", Port: 8080}))
	if conflicts := ledger.Conflicts(entry("udp", v1beta1.EndpointSpec{}).Ref); len(conflicts) != 0 {
		t.Fatalf("a UDP port conflicted with a TCP port: %v", conflicts)
	}

	// Deleting one holder releases the key for the other.
	ledger.Remove(entry("first", v1beta1.EndpointSpec{}).Ref)
	if conflicts := ledger.Conflicts(entry("second", v1beta1.EndpointSpec{}).Ref); len(conflicts) != 0 {
		t.Fatalf("the key stayed taken after a deletion: %v", conflicts)
	}
}

func TestLedgerMakesHostAndPathExclusive(t *testing.T) {
	ledger := NewLedger()
	ledger.Put(entry("host", hostSpec()))
	ledger.Put(entry("path", pathSpec()))

	for name, want := range map[string]string{"host": "app.test", "path": "app.test"} {
		conflicts := ledger.Conflicts(entry(name, v1beta1.EndpointSpec{}).Ref)
		if len(conflicts) == 0 {
			t.Fatalf("%s sees no conflict next to the other HTTP shape", name)
		}
		found := false
		for _, conflict := range conflicts {
			if conflict.Kind == ConflictHostExclusive && conflict.Key == want && len(conflict.With) == 1 {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s conflicts = %v, want an exclusive host conflict", name, conflicts)
		}
	}

	// Two path endpoints on one host coexist as long as their paths differ: the
	// sibling path adds no conflict, only the http.host endpoint does.
	ledger.Put(entry("other", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "app.test", Path: "/other"}))
	if conflicts := ledger.Conflicts(entry("path", pathSpec()).Ref); len(conflicts) != 1 || conflicts[0].Kind != ConflictHostExclusive {
		t.Fatalf("path conflicts = %v, want the host conflict only", conflicts)
	}

	// A second host endpoint on a different host is fine.
	ledger.Put(entry("elsewhere", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "other.test"}))
	if conflicts := ledger.Conflicts(entry("elsewhere", v1beta1.EndpointSpec{}).Ref); len(conflicts) != 0 {
		t.Fatalf("an unrelated host conflicted: %v", conflicts)
	}
}

func TestLedgerReportsAMissingTargetAndInvalidShapes(t *testing.T) {
	ledger := NewLedger()
	absent := entry("absent", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "gone", Host: "app.test"})
	absent.TargetMissing = true
	ledger.Put(absent)
	conflicts := ledger.Conflicts(absent.Ref)
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictTargetMissing || conflicts[0].Key != "default/gone" {
		t.Fatalf("conflicts = %v, want a missing target", conflicts)
	}

	invalid := entry("invalid", v1beta1.EndpointSpec{Kind: "http.socket"})
	invalid.Invalid = &FieldError{Field: "spec.kind", Reason: "unknown"}
	ledger.Put(invalid)
	stored, ok := ledger.Entry(invalid.Ref)
	if !ok {
		t.Fatal("the invalid endpoint is not in the ledger")
	}
	if keys := stored.Keys(); keys != nil {
		t.Fatalf("an invalid endpoint holds keys: %v", keys)
	}
	if conflicts := ledger.Conflicts(invalid.Ref); conflicts != nil {
		t.Fatalf("an invalid endpoint carries key conflicts: %v", conflicts)
	}
}

func TestProjectionListsUnmanagedAndSharedMedium(t *testing.T) {
	ledger := NewLedger()
	unmanaged := entry("unmanaged", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "gone", Host: "app.test"})
	unmanaged.Attribution = markers.AttributionUnmanaged
	unmanaged.TargetMissing = true
	ledger.Put(unmanaged)
	ledger.Put(entry("mdns", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindMDNS, TargetApp: "web", ServiceName: "web", Port: 5353}))

	rows := ledger.Projection()
	if len(rows) != 2 {
		t.Fatalf("projection has %d rows, want 2", len(rows))
	}
	if rows[0].Ref.Name != "mdns" || rows[1].Ref.Name != "unmanaged" {
		t.Fatalf("projection order = %s, %s", rows[0].Ref.Name, rows[1].Ref.Name)
	}
	if !rows[0].SharedMedium || rows[0].Note != SharedMediumNote {
		t.Fatalf("mdns row carries no shared-medium note: %+v", rows[0])
	}
	if rows[1].Attribution != markers.AttributionUnmanaged || len(rows[1].Conflicts) != 1 {
		t.Fatalf("unmanaged row = %+v, want an unmanaged conflict", rows[1])
	}
	if len(conflictKinds(rows[1].Conflicts)) != 1 || conflictKinds(rows[1].Conflicts)[0] != ConflictTargetMissing {
		t.Fatalf("unmanaged conflicts = %v", rows[1].Conflicts)
	}
}

// TestLedgerNamesEveryEndpointAMutationMoved states C-06's order independence:
// a shared key or an exclusive host makes a peer's admission depend on the
// mutation, so the ledger must report that peer for the caller to settle.
func TestLedgerNamesEveryEndpointAMutationMoved(t *testing.T) {
	ledger := NewLedger()
	first := entry("first", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 8080})
	if refs := ledger.Put(first); !equalRefs(refs, "first") {
		t.Fatalf("a fresh entry reported %v, want only itself", refs)
	}
	second := entry("second", v1beta1.EndpointSpec{Kind: v1beta1.EndpointKindTCP, TargetApp: "web", Port: 8080})
	if refs := ledger.Put(second); !equalRefs(refs, "first", "second") {
		t.Fatalf("the duplicate reported %v, want both holders", refs)
	}
	if refs := ledger.Remove(first.Ref); !equalRefs(refs, "second") {
		t.Fatalf("the release reported %v, want the survivor", refs)
	}

	// The host/path exclusion crosses shapes, so it propagates the same way.
	ledger = NewLedger()
	if refs := ledger.Put(entry("host", hostSpec())); !equalRefs(refs, "host") {
		t.Fatalf("a fresh host reported %v", refs)
	}
	if refs := ledger.Put(entry("path", pathSpec())); !equalRefs(refs, "host", "path") {
		t.Fatalf("the exclusive host reported %v, want both shapes", refs)
	}
	if refs := ledger.Remove(entry("host", v1beta1.EndpointSpec{}).Ref); !equalRefs(refs, "path") {
		t.Fatalf("the host release reported %v, want the path endpoint", refs)
	}
}

// equalRefs compares the names of a reference list.
func equalRefs(refs []write.Ref, names ...string) bool {
	if len(refs) != len(names) {
		return false
	}
	for index, ref := range refs {
		if ref.Name != names[index] {
			return false
		}
	}
	return true
}
