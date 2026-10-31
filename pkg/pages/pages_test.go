package pages

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// declarations is a fixed page projection.
type declarations []extension.Page

// Pages implements Declarations.
func (d declarations) Pages() []extension.Page { return d }

// PageFor implements Declarations.
func (d declarations) PageFor(id string, role write.Role) (extension.Page, bool) {
	for _, page := range d {
		if page.ID == id && page.Allows(role) {
			return page, true
		}
	}
	return extension.Page{}, false
}

// readingsFunc adapts a function to Readings.
type readingsFunc func(context.Context, identity.Session, extension.PageItem) (Reading, *controlplane.Error)

// Read implements Readings.
func (f readingsFunc) Read(ctx context.Context, session identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
	return f(ctx, session, item)
}

// role is the session the cases act as.
func sessionWith(role write.Role) identity.Session {
	return identity.Session{Username: "alice", Role: role}
}

// localItem is one valid browser-local display item.
func localItem(id string) extension.PageItem {
	return extension.PageItem{ID: id, Label: id, Source: extension.PageSourceLocal, Store: id, Reason: "只读展示"}
}

// TestListFollowsTheSessionRoleAndTheProjection checks that the registry lists
// only pages the role may see and that a withdrawn page disappears (C-22).
func TestListFollowsTheSessionRoleAndTheProjection(t *testing.T) {
	service := New(declarations{
		{ID: "everyone", Title: "Everyone", Location: "panel", Items: []extension.PageItem{localItem("value")}},
		{ID: "members", Title: "Members", Location: "panel", RequiredRole: string(write.RoleMember),
			Items: []extension.PageItem{localItem("value")}},
		{ID: "owners", Title: "Owners", Location: "panel", RequiredRole: string(write.RoleOwner),
			Items: []extension.PageItem{localItem("value")}},
	}, Config{})

	cases := []struct {
		role write.Role
		want []string
	}{
		{role: write.RoleGuest, want: []string{"everyone"}},
		{role: write.RoleMember, want: []string{"everyone", "members"}},
		{role: write.RoleOwner, want: []string{"everyone", "members", "owners"}},
		{role: "", want: nil},
	}
	for _, testCase := range cases {
		got := idsOf(service.List(sessionWith(testCase.role)))
		if !equalStrings(got, testCase.want) {
			t.Errorf("role %q sees %v, want %v", testCase.role, got, testCase.want)
		}
	}

	withdrawn := New(declarations{
		{ID: "everyone", Title: "Everyone", Location: "panel", Items: []extension.PageItem{localItem("value")}},
	}, Config{})
	if got := idsOf(withdrawn.List(sessionWith(write.RoleOwner))); !equalStrings(got, []string{"everyone"}) {
		t.Fatalf("a withdrawn page stays listed: %v", got)
	}
}

// TestReadAnswersHiddenAndUnknownPagesIdentically checks that a page the role
// may not see is refused exactly like one no component declares (C-22).
func TestReadAnswersHiddenAndUnknownPagesIdentically(t *testing.T) {
	service := New(declarations{{
		ID: "members", Title: "Members", Location: "panel", RequiredRole: string(write.RoleMember),
		Items: []extension.PageItem{localItem("value")},
	}}, Config{})

	_, hidden := service.Read(context.Background(), sessionWith(write.RoleGuest), "members")
	_, unknown := service.Read(context.Background(), sessionWith(write.RoleGuest), "absent")
	if hidden == nil || unknown == nil {
		t.Fatalf("hidden = %v, unknown = %v; both must be refused", hidden, unknown)
	}
	if hidden.Code != unknown.Code || hidden.Message != unknown.Message || hidden.Stage != unknown.Stage {
		t.Fatalf("hidden = %+v, unknown = %+v; refusals must be indistinguishable", hidden, unknown)
	}
	if hidden.Code != controlplane.CodeNotFound {
		t.Fatalf("code = %q, want %q", hidden.Code, controlplane.CodeNotFound)
	}
}

// TestReadRendersEverySourceType checks C-53's seven source types: server items
// carry their value and observation, a local item carries its browser key and
// no server observation, and the page lists every declared source type.
func TestReadRendersEverySourceType(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	observed := now.Add(-30 * time.Second)
	items := []extension.PageItem{
		{ID: "object", Label: "Object", Source: extension.PageSourceObject, Reference: "nodes", Reason: "只读展示"},
		{ID: "computed", Label: "Computed", Source: extension.PageSourceComputed, Reference: "ready-count", Reason: "只读展示"},
		{ID: "metric", Label: "Metric", Source: extension.PageSourceMetric, Reference: "cpu", Reason: "只读展示"},
		{ID: "probe", Label: "Probe", Source: extension.PageSourceProbe, Reference: "dns", Reason: "只读展示"},
		{ID: "read", Label: "Read", Source: extension.PageSourceRead, Reference: "logs", Reason: "只读展示"},
		{ID: "local", Label: "Local", Source: extension.PageSourceLocal, Store: "view", Reason: "只读展示"},
		{ID: "database", Label: "Database", Source: extension.PageSourceDatabase, Reference: "members", Reason: "只读展示"},
	}
	service := New(declarations{{
		ID: "all", Title: "All", Location: "panel", Items: items,
		Actions: []extension.Action{{ID: "apply", Method: "POST", Path: "/api/v1/apply-requests"}},
	}}, Config{
		Readings: readingsFunc(func(_ context.Context, _ identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
			return Reading{Value: item.Source + ":" + item.Reference, ObservedAt: observed}, nil
		}),
		Now: func() time.Time { return now },
	})

	page, failure := service.Read(context.Background(), sessionWith(write.RoleOwner), "all")
	if failure != nil {
		t.Fatalf("read page: %v", failure)
	}
	wantSources := []string{
		extension.PageSourceObject, extension.PageSourceComputed, extension.PageSourceMetric,
		extension.PageSourceProbe, extension.PageSourceRead, extension.PageSourceLocal,
		extension.PageSourceDatabase,
	}
	if !equalStrings(page.Sources, wantSources) {
		t.Fatalf("sources = %v, want %v", page.Sources, wantSources)
	}
	references := map[string]string{}
	for _, item := range items {
		references[item.ID] = item.Reference
	}
	for _, item := range page.Items {
		if item.Source == extension.PageSourceLocal {
			if item.Value != nil || item.Store != "view" || item.ObservedAt != nil || item.Stale {
				t.Fatalf("local item = %+v, want the browser key and no server observation", item)
			}
			continue
		}
		want := item.Source + ":" + references[item.ID]
		if item.Value != want {
			t.Fatalf("%s value = %v, want %q", item.ID, item.Value, want)
		}
		if item.ObservedAt == nil || !item.ObservedAt.Equal(observed.UTC()) {
			t.Fatalf("%s observedAt = %v, want %s", item.ID, item.ObservedAt, observed.UTC())
		}
		if item.Stale {
			t.Fatalf("%s is stale at 30s, want fresh", item.ID)
		}
	}
}

// TestStaleUsesOneThresholdForEveryServerSource checks C-13's single bound: at
// exactly two minutes a server item is fresh, one nanosecond later it is stale,
// and no source type gets its own value.
func TestStaleUsesOneThresholdForEveryServerSource(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, source := range []string{
		extension.PageSourceObject, extension.PageSourceComputed, extension.PageSourceMetric,
		extension.PageSourceProbe, extension.PageSourceRead, extension.PageSourceDatabase,
	} {
		for _, testCase := range []struct {
			name    string
			age     time.Duration
			isStale bool
		}{
			{name: "exactly at the threshold", age: controlplane.StaleThreshold, isStale: false},
			{name: "one nanosecond beyond", age: controlplane.StaleThreshold + time.Nanosecond, isStale: true},
		} {
			t.Run(source+"/"+testCase.name, func(t *testing.T) {
				observedAt := now.Add(-testCase.age)
				service := New(declarations{{
					ID: "page", Title: "Page", Location: "panel",
					Items: []extension.PageItem{{ID: "value", Label: "Value", Source: source, Reference: "input", Reason: "只读展示"}},
				}}, Config{
					Readings: readingsFunc(func(context.Context, identity.Session, extension.PageItem) (Reading, *controlplane.Error) {
						return Reading{Value: "value", ObservedAt: observedAt}, nil
					}),
					Now: func() time.Time { return now },
				})

				page, failure := service.Read(context.Background(), sessionWith(write.RoleOwner), "page")
				if failure != nil {
					t.Fatalf("read page: %v", failure)
				}
				if got := page.Items[0].Stale; got != testCase.isStale {
					t.Fatalf("stale at age %s = %v, want %v", testCase.age, got, testCase.isStale)
				}
			})
		}
	}
}

// TestReadOnlyPageStatesWhyAndExposesNoModificationEntry checks C-53's read-only
// rule: the page carries its reason, no action and no item entry.
func TestReadOnlyPageStatesWhyAndExposesNoModificationEntry(t *testing.T) {
	service := New(declarations{{
		ID: "native", Title: "Native", Location: "native", ReadOnly: true,
		ReadOnlyReason: "原生视图只能查看，写操作必须经 kubectl 与 kubeconfig",
		Items:          []extension.PageItem{localItem("fields")},
	}}, Config{})

	page, failure := service.Read(context.Background(), sessionWith(write.RoleOwner), "native")
	if failure != nil {
		t.Fatalf("read page: %v", failure)
	}
	if !page.ReadOnly || page.ReadOnlyReason == "" {
		t.Fatalf("read-only marker = %v reason = %q", page.ReadOnly, page.ReadOnlyReason)
	}
	if len(page.Actions) != 0 {
		t.Fatalf("actions = %v, want none", page.Actions)
	}
	if page.Items[0].Entry != nil {
		t.Fatalf("entry = %+v, want none on a read-only page", page.Items[0].Entry)
	}
}

// TestActionEntriesNameControlPlaneWrites checks that an item's modification
// entry is the page action's control-plane request (C-22).
func TestActionEntriesNameControlPlaneWrites(t *testing.T) {
	service := New(declarations{{
		ID: "apps", Title: "Apps", Location: "panel",
		Items: []extension.PageItem{{
			ID: "apps", Label: "Apps", Source: extension.PageSourceLocal, Store: "apps", Action: "stop",
		}},
		Actions: []extension.Action{
			{ID: "stop", Method: "POST", Path: "/api/v1/action-requests"},
			{ID: "delete", Method: "DELETE", Path: "/api/v1/resources/Application/apps"},
		},
	}}, Config{})

	page, failure := service.Read(context.Background(), sessionWith(write.RoleOwner), "apps")
	if failure != nil {
		t.Fatalf("read page: %v", failure)
	}
	if len(page.Actions) != 2 || page.Actions[0].Entry.Path != "/api/v1/action-requests" ||
		page.Actions[1].Entry.Method != "DELETE" {
		t.Fatalf("actions = %+v", page.Actions)
	}
	entry := page.Items[0].Entry
	if entry == nil || entry.Method != "POST" || entry.Path != "/api/v1/action-requests" {
		t.Fatalf("item entry = %+v, want the stop action", entry)
	}
}

// TestServerItemNeedsAReaderAndAnObservation checks the two failures a shell
// cannot paper over: no reader for a server source, and a reader that returns no
// observation time (C-13, C-53).
func TestServerItemNeedsAReaderAndAnObservation(t *testing.T) {
	declared := declarations{{
		ID: "page", Title: "Page", Location: "panel",
		Items: []extension.PageItem{{ID: "value", Label: "Value", Source: extension.PageSourceMetric, Reference: "cpu", Reason: "只读展示"}},
	}}

	_, failure := New(declared, Config{}).Read(context.Background(), sessionWith(write.RoleOwner), "page")
	if failure == nil || failure.Code != controlplane.CodeCapabilityMissing {
		t.Fatalf("no reader failure = %+v, want capability missing", failure)
	}

	_, failure = New(declared, Config{
		Readings: readingsFunc(func(context.Context, identity.Session, extension.PageItem) (Reading, *controlplane.Error) {
			return Reading{Value: 1}, nil
		}),
	}).Read(context.Background(), sessionWith(write.RoleOwner), "page")
	if failure == nil || failure.Code != controlplane.CodeInternal {
		t.Fatalf("missing observation failure = %+v, want internal", failure)
	}
}

// TestLocalPageNeedsNoReader checks that browser-local pages render without any
// control-plane source, because their values never enter a projection (C-53).
func TestLocalPageNeedsNoReader(t *testing.T) {
	service := New(declarations{{
		ID: "desktop", Title: "Desktop", Location: "desktop",
		Items: []extension.PageItem{localItem("recent")},
	}}, Config{})

	page, failure := service.Read(context.Background(), sessionWith(write.RoleGuest), "desktop")
	if failure != nil {
		t.Fatalf("read page: %v", failure)
	}
	if len(page.Items) != 1 || page.Items[0].Store != "recent" {
		t.Fatalf("items = %+v", page.Items)
	}
}

// TestReadRejectsContradictoryItems checks the rendering guard C-53 needs even
// when a declaration did not pass loadPages: a writable item with neither an
// entry nor a reason, and an item declaring both.
func TestReadRejectsContradictoryItems(t *testing.T) {
	cases := []struct {
		name    string
		item    extension.PageItem
		actions []extension.Action
		want    string
	}{
		{
			name: "no entry and no reason",
			item: extension.PageItem{ID: "value", Label: "Value", Source: extension.PageSourceLocal, Store: "value"},
			want: "既没有修改入口也没有原因",
		},
		{
			name: "entry and reason",
			item: extension.PageItem{
				ID: "value", Label: "Value", Source: extension.PageSourceLocal, Store: "value",
				Action: "apply", Reason: "只读展示",
			},
			actions: []extension.Action{{ID: "apply", Method: "POST", Path: "/api/v1/apply-requests"}},
			want:    "同时声明修改入口与不可改原因",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			service := New(declarations{{
				ID: "page", Title: "Page", Location: "panel",
				Items:   []extension.PageItem{testCase.item},
				Actions: testCase.actions,
			}}, Config{})

			_, failure := service.Read(context.Background(), sessionWith(write.RoleOwner), "page")
			if failure == nil || failure.Code != controlplane.CodeInternal {
				t.Fatalf("failure = %+v, want an internal contradiction", failure)
			}
			if !strings.Contains(failure.Message, testCase.want) {
				t.Fatalf("message = %q, want %q", failure.Message, testCase.want)
			}
		})
	}
}

// idsOf names the summaries' identities in order.
func idsOf(summaries []Summary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	return ids
}

// equalStrings compares two string slices element by element.
func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
