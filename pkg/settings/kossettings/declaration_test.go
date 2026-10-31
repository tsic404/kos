package kossettings

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/tsic404/kos/api"
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
		ClusterResourceUsage:      extension.PageSourceComputed,
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

// TestOriginFollowsTheAuditTrail checks C-65's origin rule as the settings page
// applies it: a start without a later restore is planned, an unschedulable node
// without a KOS write is unplanned, an unreadable trail is unknown, and a
// schedulable node has no maintenance.
func TestOriginFollowsTheAuditTrail(t *testing.T) {
	node := corev1.Node{}
	node.Name = "node-a"
	node.Spec.Unschedulable = true
	object := api.Group + "/Node/node-a"
	trail := []identity.AuditEntry{
		{Object: object, Verb: AuditVerbMaintain},
		{Object: api.Group + "/Node/node-b", Verb: AuditVerbRestore},
	}
	cases := []struct {
		name       string
		node       corev1.Node
		trail      []identity.AuditEntry
		readable   bool
		wantOrigin string
	}{
		{"planned", node, trail, true, MaintenancePlanned},
		{"unplanned", node, []identity.AuditEntry{}, true, MaintenanceUnplanned},
		{"restored then cordoned outside KOS", node, []identity.AuditEntry{
			{Object: object, Verb: AuditVerbMaintain},
			{Object: object, Verb: AuditVerbRestore},
		}, true, MaintenanceUnplanned},
		{"audit unavailable", node, nil, false, MaintenanceUnknown},
		{"schedulable", corev1.Node{}, trail, true, MaintenanceNone},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if origin := originOf(testCase.node, testCase.trail, testCase.readable); origin != testCase.wantOrigin {
				t.Fatalf("origin = %q, want %q", origin, testCase.wantOrigin)
			}
		})
	}
}
