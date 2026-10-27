package settings

import (
	"context"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// memoryValues is a value store that answers from a map.
type memoryValues struct {
	stored map[string]string
}

// SettingValues implements Values.
func (m *memoryValues) SettingValues(_ context.Context, scope, target string) (map[string]string, error) {
	values := map[string]string{}
	prefix := scope + "\x00" + target + "\x00"
	for key, value := range m.stored {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			values[key[len(prefix):]] = value
		}
	}
	return values, nil
}

// SetSettingValue implements Values.
func (m *memoryValues) SetSettingValue(_ context.Context, scope, target, id, value string) error {
	m.stored[scope+"\x00"+target+"\x00"+id] = value
	return nil
}

// declarations is a fixed declaration set.
type declarations []extension.Setting

// Settings implements Declarations.
func (d declarations) Settings() []extension.Setting { return d }

// Setting implements Declarations.
func (d declarations) Setting(id string) (extension.Setting, bool) {
	for _, item := range d {
		if item.ID == id {
			return item, true
		}
	}
	return extension.Setting{}, false
}

// session is the owner the cases act as.
var session = identity.Session{Username: "owner", Role: write.RoleOwner}

// TestItemReportsWriterEntryAndReason checks the projection fields C-23 fixes.
func TestItemReportsWriterEntryAndReason(t *testing.T) {
	service := New(declarations{
		{ID: "writable", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
		{ID: "fixed", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterKernel},
	}, &memoryValues{stored: map[string]string{}})

	items, failure := service.Items(context.Background(), session, "")
	if failure != nil {
		t.Fatalf("items: %v", failure)
	}
	writable := itemByID(t, items, "writable")
	if writable.Source != extension.SettingWriterUser || writable.Entry == nil ||
		writable.Entry.Path != "/api/v1/settings/writable" || writable.Reason != "" {
		t.Fatalf("writable item = %+v", writable)
	}
	fixed := itemByID(t, items, "fixed")
	if fixed.Entry != nil || fixed.Reason == "" || fixed.Source != extension.SettingWriterKernel {
		t.Fatalf("fixed item = %+v, want no entry and a reason naming the writer", fixed)
	}
}

// TestWriteFollowsTheDeclaredWriter checks that only the user writer's editable
// item accepts a write (C-23).
func TestWriteFollowsTheDeclaredWriter(t *testing.T) {
	service := New(declarations{
		{ID: "writable", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
		{ID: "controller", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterController},
		{ID: "fixed", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser},
	}, &memoryValues{stored: map[string]string{}})

	item, failure := service.Set(context.Background(), session, "writable", "", "42")
	if failure != nil {
		t.Fatalf("write the editable item: %v", failure)
	}
	if item.Value != "42" {
		t.Fatalf("written value = %q, want 42", item.Value)
	}
	for _, id := range []string{"controller", "fixed"} {
		if _, failure := service.Set(context.Background(), session, id, "", "42"); failure == nil {
			t.Fatalf("setting %q accepted a user write", id)
		} else if failure.Code != controlplane.CodeInvalidField {
			t.Fatalf("setting %q failed with %q, want %q", id, failure.Code, controlplane.CodeInvalidField)
		}
	}
	if _, failure := service.Set(context.Background(), session, "absent", "", "42"); failure == nil ||
		failure.Code != controlplane.CodeNotFound {
		t.Fatalf("unknown setting = %v, want not found", failure)
	}
}

// TestNodeScopeNeedsTheNode checks a node-scoped item does not fall back to a
// cluster-wide value.
func TestNodeScopeNeedsTheNode(t *testing.T) {
	service := New(declarations{
		{ID: "reserve", Scope: extension.SettingScopeNode, Writer: extension.SettingWriterUser, Editable: true},
	}, &memoryValues{stored: map[string]string{}})

	items, failure := service.Items(context.Background(), session, "")
	if failure != nil || len(items) != 0 {
		t.Fatalf("items without a node = %v / %v, want the node item skipped", items, failure)
	}
	if _, failure := service.Set(context.Background(), session, "reserve", "", "1"); failure == nil {
		t.Fatal("a node-scoped item written without a node")
	}
	item, failure := service.Set(context.Background(), session, "reserve", "node-a", "2")
	if failure != nil || item.Value != "2" {
		t.Fatalf("write on node-a = %+v / %v", item, failure)
	}
	values := service.values.(*memoryValues)
	if _, written := values.stored["Node\x00node-a\x00reserve"]; !written {
		t.Fatalf("stored values = %v, want the value under node-a", values.stored)
	}
}

// TestNodeScopeRejectsAMalformedNode checks that a node target is a DNS
// subdomain before it becomes a stored value: a typo must not create a row no
// node will ever read, and a caller must not mint arbitrary targets.
func TestNodeScopeRejectsAMalformedNode(t *testing.T) {
	service := New(declarations{
		{ID: "reserve", Scope: extension.SettingScopeNode, Writer: extension.SettingWriterUser, Editable: true},
	}, &memoryValues{stored: map[string]string{}})

	if _, failure := service.Set(context.Background(), session, "reserve", "Not_A_Node", "2"); failure == nil ||
		failure.Code != controlplane.CodeInvalidField {
		t.Fatalf("write on a malformed node = %v, want %q", failure, controlplane.CodeInvalidField)
	}
	if _, failure := service.Items(context.Background(), session, "Not_A_Node"); failure == nil ||
		failure.Code != controlplane.CodeInvalidField {
		t.Fatalf("read on a malformed node = %v, want %q", failure, controlplane.CodeInvalidField)
	}
	values := service.values.(*memoryValues)
	if len(values.stored) != 0 {
		t.Fatalf("stored values = %v, want nothing stored for a malformed node", values.stored)
	}
	if _, failure := service.Set(context.Background(), session, "reserve", "node-a.example", "2"); failure != nil {
		t.Fatalf("write on a valid node: %v", failure)
	}
}

// itemByID returns one projected item.
func itemByID(t *testing.T, items []Item, id string) Item {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("item %q is absent from %v", id, items)
	return Item{}
}
