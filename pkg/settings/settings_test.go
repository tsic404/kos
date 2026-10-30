package settings

import (
	"context"
	"strings"
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

// auditLog keeps the records the service writes.
type auditLog struct {
	records []controlplane.AuditRecord
}

// Record implements controlplane.Auditor.
func (a *auditLog) Record(_ context.Context, record controlplane.AuditRecord) error {
	a.records = append(a.records, record)
	return nil
}

// TestItemReportsWriterEntryAndReason checks the projection fields C-23 fixes.
func TestItemReportsWriterEntryAndReason(t *testing.T) {
	service := New(declarations{
		{ID: "writable", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
		{ID: "fixed", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterKernel},
	}, &memoryValues{stored: map[string]string{}}, Config{})

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
	}, &memoryValues{stored: map[string]string{}}, Config{})

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
	}, &memoryValues{stored: map[string]string{}}, Config{})

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
	}, &memoryValues{stored: map[string]string{}}, Config{})

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

// TestSetRefusesATargetTheTrailCannotHold checks that a write naming an item or
// node the audit record cannot hold is refused as invalid input before anything
// is stored or recorded (C-14).
func TestSetRefusesATargetTheTrailCannotHold(t *testing.T) {
	trail := &auditLog{}
	service := New(declarations{
		{ID: "writable", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser, Editable: true},
		{ID: "reserve", Scope: extension.SettingScopeNode, Writer: extension.SettingWriterUser, Editable: true},
	}, &memoryValues{stored: map[string]string{}}, Config{Auditor: trail})

	overLong := strings.Repeat("a", controlplane.MaxRecordNameLength+1)
	cases := []struct{ name, id, node string }{
		{"an item id over the bound", overLong, ""},
		{"an item id with a path separator", "re/serve", ""},
		{"an item id with a control character", "res\t", ""},
		{"a node over the bound", "reserve", overLong},
		{"a node that is not a DNS subdomain", "reserve", "Node_A"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, failure := service.Set(context.Background(), session, testCase.id, testCase.node, "1"); failure == nil ||
				failure.Code != controlplane.CodeInvalidField {
				t.Fatalf("write = %v, want %q", failure, controlplane.CodeInvalidField)
			}
		})
	}
	if len(trail.records) != 0 {
		t.Fatalf("audit = %+v, want an unrecordable target to leave no record", trail.records)
	}
	values := service.values.(*memoryValues)
	if len(values.stored) != 0 {
		t.Fatalf("stored values = %v, want nothing stored", values.stored)
	}

	// A well-formed refusal still reaches the trail: the bound narrows what is
	// recorded, not whether a refused attempt is.
	if _, failure := service.Set(context.Background(), session, "absent", "", "1"); failure == nil ||
		failure.Code != controlplane.CodeNotFound {
		t.Fatalf("write an undeclared item = %v, want not found", failure)
	}
	if len(trail.records) != 1 || trail.records[0].Result != controlplane.ResultRejected {
		t.Fatalf("audit = %+v, want one refused record", trail.records)
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
