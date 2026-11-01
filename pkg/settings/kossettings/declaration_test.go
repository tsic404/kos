package kossettings

import (
	"context"
	"errors"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
)

// TestDeclarationLoads checks the product declares what C-36 and C-50 put in
// the panel and that the declaration loader accepts every class.
func TestDeclarationLoads(t *testing.T) {
	loaded, err := Declaration().Load()
	if err != nil {
		t.Fatalf("load declaration: %v", err)
	}
	if len(loaded.Problems) > 0 {
		t.Fatalf("declaration problems = %+v", loaded.Problems)
	}
	if len(loaded.Contributions.Settings) != 2 {
		t.Fatalf("settings = %+v, want the two modifiable items", loaded.Contributions.Settings)
	}
	if len(loaded.Contributions.Pages) != 2 {
		t.Fatalf("pages = %+v, want the settings and user pages", loaded.Contributions.Pages)
	}
	for _, page := range loaded.Contributions.Pages {
		if page.ID != PageSettings && page.ID != PageUsers {
			t.Fatalf("page %q is not one of the product's pages", page.ID)
		}
	}
}

// TestSettingsPageCoversBothDomains checks C-36's cluster and node coverage is
// declared, and that every fact item carries the source type it is judged by.
func TestSettingsPageCoversBothDomains(t *testing.T) {
	page := settingsPage()
	sources := map[string]string{}
	for _, item := range page.Items {
		sources[item.ID] = item.Source
	}
	expected := map[string]string{
		ClusterKubernetesVersion:  extension.PageSourceObject,
		ClusterNodesAndRoles:      extension.PageSourceObject,
		ClusterResourceUsage:      extension.PageSourceMetric,
		ClusterSystemComponents:   extension.PageSourceObject,
		ClusterNetworkAndStorage:  extension.PageSourceObject,
		ClusterHealth:             extension.PageSourceComputed,
		ClusterMaintenanceWindow:  extension.PageSourceDatabase,
		NodeIdentity:              extension.PageSourceObject,
		NodeStatus:                extension.PageSourceObject,
		NodeResources:             extension.PageSourceObject,
		NodeMaintenanceState:      extension.PageSourceObject,
		NodeMaintenanceOrigin:     extension.PageSourceDatabase,
		NodeMaintenanceAffected:   extension.PageSourceComputed,
		NodeDevices:               extension.PageSourceObject,
		NodeProviderStatus:        extension.PageSourceObject,
		NodeSchedulingConstraints: extension.PageSourceObject,
		NodeReservedResources:     extension.PageSourceDatabase,
		NodeHostConfig:            extension.PageSourceComputed,
	}
	for id, source := range expected {
		if sources[id] != source {
			t.Fatalf("item %q source = %q, want %q", id, sources[id], source)
		}
	}
	if len(sources) != len(expected) {
		t.Fatalf("settings page declares %d items, want %d", len(sources), len(expected))
	}
}

// trailReader answers the per-object maintenance read from one fixed trail,
// newest first, applying the object, verb, result and limit bounds the store
// applies.
type trailReader struct {
	entries []identity.AuditEntry
	err     error
}

// RecentAuditEntriesForObject implements AuditReader.
func (t trailReader) RecentAuditEntriesForObject(ctx context.Context, object string, verbs, results []string, limit int) ([]identity.AuditEntry, error) {
	if t.err != nil {
		return nil, t.err
	}
	matched := make([]identity.AuditEntry, 0, len(t.entries))
	for _, entry := range t.entries {
		if entry.Object != object || !slices.Contains(verbs, entry.Verb) {
			continue
		}
		if len(results) > 0 && !slices.Contains(results, entry.Result) {
			continue
		}
		matched = append(matched, entry)
	}
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

// TestOriginFollowsTheAuditTrail checks C-65's origin rule as the settings page
// applies it: a start newer than the last restore is planned, an unschedulable
// node with no maintenance record is unplanned, an unreadable trail is unknown,
// a schedulable node has no maintenance, and an attempt that wrote nothing never
// decides the answer.
func TestOriginFollowsTheAuditTrail(t *testing.T) {
	node := corev1.Node{}
	node.Name = "node-a"
	node.Spec.Unschedulable = true
	object := api.Group + "/Node/node-a"
	cases := []struct {
		name       string
		node       corev1.Node
		audit      AuditReader
		wantOrigin string
	}{
		{"planned", node, trailReader{entries: []identity.AuditEntry{
			{Object: object, Verb: controlplane.VerbMaintain, Result: controlplane.ResultAccepted},
			{Object: api.Group + "/Node/node-b", Verb: controlplane.VerbRestore, Result: controlplane.ResultAccepted},
		}}, MaintenancePlanned},
		{"unplanned", node, trailReader{}, MaintenanceUnplanned},
		{"restored then cordoned outside KOS", node, trailReader{entries: []identity.AuditEntry{
			{Object: object, Verb: controlplane.VerbRestore, Result: controlplane.ResultAccepted},
			{Object: object, Verb: controlplane.VerbMaintain, Result: controlplane.ResultAccepted},
		}}, MaintenanceUnplanned},
		// A refused start writes nothing, so the cordon it left in place stays
		// unplanned.
		{"refused start after an outside cordon", node, trailReader{entries: []identity.AuditEntry{
			{Object: object, Verb: controlplane.VerbMaintain, Result: controlplane.ResultRejected},
		}}, MaintenanceUnplanned},
		// A restore that failed wrote nothing either: the accepted start is still
		// the newest write that moved the field.
		{"failed restore after a planned start", node, trailReader{entries: []identity.AuditEntry{
			{Object: object, Verb: controlplane.VerbRestore, Result: controlplane.ResultFailed},
			{Object: object, Verb: controlplane.VerbMaintain, Result: controlplane.ResultAccepted},
		}}, MaintenancePlanned},
		{"audit unavailable", node, trailReader{err: errors.New("audit down")}, MaintenanceUnknown},
		{"no audit reader", node, nil, MaintenanceUnknown},
		{"schedulable", corev1.Node{}, trailReader{entries: []identity.AuditEntry{
			{Object: object, Verb: controlplane.VerbMaintain, Result: controlplane.ResultAccepted},
		}}, MaintenanceNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			readings := &Readings{Audit: testCase.audit}
			if origin := readings.originOf(context.Background(), testCase.node); origin != testCase.wantOrigin {
				t.Fatalf("origin = %q, want %q", origin, testCase.wantOrigin)
			}
		})
	}
}
