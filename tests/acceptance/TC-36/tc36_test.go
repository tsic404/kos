// Package tc36 verifies C-14's audit trail: each write of the control plane
// leaves one record with actor, object, verb, result and time; account writes
// through the identity surface land in the same stream; a mixed kernel and
// extension apply marks each record with its own class's extension; and no
// path — HTTP or SQL — rewrites or removes a record (AC-18, TC-36).
package tc36

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// declaration declares one extension object class the Owner may write.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   "example.test",
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{
				{Group: "example.test", Kind: "Widget", Resource: "widgets", Versions: []string{"v1"}, Namespaced: true},
			},
		},
	}
}

// record is the part of an audit entry this case judges.
type record struct {
	actor     string
	verb      string
	result    string
	object    string
	extension string
}

// assertRecords compares the recorded stream position by position.
func assertRecords(t *testing.T, what string, got []identity.AuditEntry, want []record, now func() time.Time) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: audit records = %d %+v, want %d", what, len(got), got, len(want))
	}
	for index, expected := range want {
		entry := got[index]
		actual := record{entry.Actor, entry.Verb, entry.Result, entry.Object, entry.Extension}
		if actual != expected {
			t.Fatalf("%s: record %d = %+v, want %+v", what, index, actual, expected)
		}
		if entry.Source != controlplane.SourceAPI {
			t.Fatalf("%s: record %d source = %q, want %q", what, index, entry.Source, controlplane.SourceAPI)
		}
		if !entry.OccurredAt.Equal(now()) {
			t.Fatalf("%s: record %d time = %v, want the write instant %v", what, index, entry.OccurredAt, now())
		}
	}
}

// account renders the audit object of one account write: the account is an
// object of its own class, like every other audited write names one.
func account(username string) string {
	return "kos.tsic.top/Account/" + username
}

// widget builds one extension object body.
func widget(name string) map[string]any {
	return map[string]any{
		"apiVersion": "example.test/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"size": "small"},
	}
}

// TestTC36AuditRecord runs the three write classes, the account writes with
// their refusals, an extension write, a refused object write and a mixed apply,
// then checks the stream and its immutability.
func TestTC36AuditRecord(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, "example.test", "v1", "Widget", "widgets", true)
	installation := cluster.InstallPackage(t, "demo", declaration(), extension.MemoryArtifacts{})
	registry := installation.Registry()

	system := cluster.StartAccess(t, registry, func(config *controlplane.Config) {
		config.Extensions = registry
	})
	ctx := context.Background()
	stream := func() []identity.AuditEntry {
		t.Helper()
		entries, err := system.Identity.Store.AuditEntries(ctx, 500)
		if err != nil {
			t.Fatalf("read the audit stream: %v", err)
		}
		return entries
	}

	owner := system.Owner(t, "owner", "owner-password")
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)

	// The account surface is a write surface too: the Owner's password change,
	// the Guest's creation and the Guest's password change each left a record
	// naming the account the write touched (C-14).
	assertRecords(t, "the account writes", stream(), []record{
		{"owner", "update", controlplane.ResultAccepted, account("owner"), ""},
		{"owner", "create", controlplane.ResultAccepted, account("guest"), ""},
		{"guest", "update", controlplane.ResultAccepted, account("guest"), ""},
	}, system.Identity.Clock.Now)
	accountWrites := len(stream())

	// Refusals belong to the same stream: a Member may neither create, change
	// nor delete an account, nor change its password with a wrong current one,
	// and the Owner's role change and deletion each leave the record of the
	// account they touched (C-14).
	member := system.Account(t, owner, "member", "member-password", write.RoleMember)
	if status, body := system.Call(t, member, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "intruder", "role": string(write.RoleGuest),
	}, nil); status != http.StatusForbidden {
		t.Fatalf("member create account = %d %v, want 403", status, body)
	}
	if status, body := system.Call(t, member, http.MethodPut, "/api/v1/accounts/member",
		map[string]any{"disabled": true}, nil); status != http.StatusForbidden {
		t.Fatalf("member disable account = %d %v, want 403", status, body)
	}
	if status, body := system.Call(t, member, http.MethodDelete, "/api/v1/accounts/owner", nil, nil); status != http.StatusForbidden {
		t.Fatalf("member delete account = %d %v, want 403", status, body)
	}
	// A wrong current password is refused like any other write and recorded
	// against the account; it runs before the deletion that ends the session.
	if status, body := system.Call(t, member, http.MethodPut, "/api/v1/auth/password", map[string]any{
		"currentPassword": "not-the-member-password", "newPassword": "replacement-member-password",
	}, nil); status != http.StatusBadRequest {
		t.Fatalf("member password change with a wrong current password = %d %v, want 400", status, body)
	}
	if status, body := system.Call(t, owner, http.MethodPut, "/api/v1/accounts/member",
		map[string]any{"role": string(write.RoleGuest)}, nil); status != http.StatusOK {
		t.Fatalf("change the member's role = %d %v", status, body)
	}
	if status, body := system.Call(t, owner, http.MethodDelete, "/api/v1/accounts/member", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete the account = %d %v", status, body)
	}
	assertRecords(t, "the further account writes", stream()[accountWrites:], []record{
		{"owner", "create", controlplane.ResultAccepted, account("member"), ""},
		{"member", "update", controlplane.ResultAccepted, account("member"), ""},
		{"member", "create", controlplane.ResultRejected, account("intruder"), ""},
		{"member", "update", controlplane.ResultRejected, account("member"), ""},
		{"member", "delete", controlplane.ResultRejected, account("owner"), ""},
		{"member", "update", controlplane.ResultRejected, account("member"), ""},
		{"owner", "update", controlplane.ResultAccepted, account("member"), ""},
		{"owner", "delete", controlplane.ResultAccepted, account("member"), ""},
	}, system.Identity.Clock.Now)
	written := len(stream())

	namespaceParam := "?namespace=" + harness.Namespace
	application := "/api/v1/resources/Application"

	// Create, modify, delete: one record each.
	if status, body := system.Call(t, owner, http.MethodPost, application+namespaceParam,
		harness.Manifest("Application", harness.Namespace, "audited", nil, nil), nil); status != http.StatusAccepted {
		t.Fatalf("create the application = %d %v", status, body)
	}
	update := harness.Manifest("Application", harness.Namespace, "audited", nil, nil)
	update["spec"] = map[string]any{"namespace": "apps"}
	version := cluster.Get(t, "v1beta1", "Application", "audited").GetResourceVersion()
	if status, body := system.Call(t, owner, http.MethodPut, application+"/audited"+namespaceParam, update,
		map[string]string{"If-Match": version}); status != http.StatusAccepted {
		t.Fatalf("update the application = %d %v", status, body)
	}
	entry := harness.Manifest("Endpoint", harness.Namespace, "audited-entry", nil, nil)
	entry["spec"] = map[string]any{"kind": "http.host", "targetApp": "audited", "host": "audited.example.test"}
	if status, body := system.Call(t, owner, http.MethodPost, "/api/v1/resources/Endpoint"+namespaceParam, entry, nil); status != http.StatusAccepted {
		t.Fatalf("create the endpoint = %d %v", status, body)
	}
	version = cluster.Get(t, "v1beta1", "Endpoint", "audited-entry").GetResourceVersion()
	if status, body := system.Call(t, owner, http.MethodDelete, "/api/v1/resources/Endpoint/audited-entry"+namespaceParam, nil,
		map[string]string{"If-Match": version}); status != http.StatusAccepted {
		t.Fatalf("delete the endpoint = %d %v", status, body)
	}

	// An extension class writes into the same stream, marked with its
	// extension (C-14).
	if status, body := system.Call(t, owner, http.MethodPost, "/api/v1/resources/Widget"+namespaceParam,
		widget("audited-widget"), nil); status != http.StatusAccepted {
		t.Fatalf("create the widget = %d %v", status, body)
	}

	// A refused write is recorded too, with the role that was refused.
	if status, body := system.Call(t, guest, http.MethodPost, application+namespaceParam,
		harness.Manifest("Application", harness.Namespace, "refused", nil, nil), nil); status != http.StatusForbidden {
		t.Fatalf("guest write = %d %v, want 403", status, body)
	}

	assertRecords(t, "the object writes", stream()[written:], []record{
		{"owner", "create", controlplane.ResultAccepted, "kos.tsic.top/Application/default/audited", ""},
		{"owner", "update", controlplane.ResultAccepted, "kos.tsic.top/Application/default/audited", ""},
		{"owner", "create", controlplane.ResultAccepted, "kos.tsic.top/Endpoint/default/audited-entry", ""},
		{"owner", "delete", controlplane.ResultAccepted, "kos.tsic.top/Endpoint/default/audited-entry", ""},
		{"owner", "create", controlplane.ResultAccepted, "example.test/Widget/default/audited-widget", "demo.widgets"},
		{"guest", "create", controlplane.ResultRejected, "kos.tsic.top/Application/default/refused", ""},
	}, system.Identity.Clock.Now)
	written = len(stream())

	// A set request may mix a kernel class with a declared extension class:
	// each write carries its own class's extension, never the batch's last one
	// (C-14).
	batch := map[string]any{"objects": []any{
		harness.Manifest("Application", harness.Namespace, "batched", nil, nil),
		widget("batched-widget"),
	}}
	if status, body := system.Call(t, owner, http.MethodPost, "/api/v1/apply-requests", batch, nil); status != http.StatusAccepted {
		t.Fatalf("mixed apply = %d %v", status, body)
	}
	harness.Eventually(t, 20*time.Second, "the applied writes to be audited", func() bool {
		return len(stream()) >= written+2
	})
	batched := stream()[written:]
	assertRecords(t, "the mixed apply", batched, []record{
		{"owner", "create", controlplane.ResultAccepted, "example.test/Widget/default/batched-widget", "demo.widgets"},
		{"owner", "create", controlplane.ResultAccepted, "kos.tsic.top/Application/default/batched", ""},
	}, system.Identity.Clock.Now)

	entries := stream()

	// No HTTP path rewrites or removes a record: there is no audit route at all.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		status, body := system.Call(t, owner, method, "/api/v1/audit", nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("%s /api/v1/audit = %d %v, want 404", method, status, body)
		}
	}

	// The table itself refuses the two mutations, so not even the store can
	// rewrite what it appended (C-14).
	database, err := sql.Open("sqlite", system.Identity.DatabasePath)
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(`UPDATE audit SET result = 'rewritten'`); err == nil {
		t.Fatal("an audit record was rewritten")
	}
	if _, err := database.Exec(`DELETE FROM audit`); err == nil {
		t.Fatal("an audit record was deleted")
	}
	after := stream()
	if len(after) != len(entries) {
		t.Fatalf("the stream changed under the refused mutations: %d -> %d", len(entries), len(after))
	}
}

// TestTC36PlanRefusalRecordsNothing sends a delete-shaped plan request as a
// Member naming another owner's scope: the deletion is refused, and the trail
// gains nothing, because a plan changes no object and no row (AC-17, C-62).
func TestTC36PlanRefusalRecordsNothing(t *testing.T) {
	cluster := harness.Start(t)
	system := cluster.StartAccess(t, nil, nil)
	ctx := context.Background()

	owner := system.Owner(t, "owner", "owner-password")
	system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	bob := system.Account(t, owner, "bob", "bob-password", write.RoleMember)

	// An application alice owns, inside the install scope the plan names; a
	// Member may not delete it, so the plan cannot be answered (C-59).
	theirs := &unstructured.Unstructured{Object: harness.Manifest(
		"Application", harness.Namespace, "theirs",
		map[string]string{"managed-by": "kos-shell", "owner-uid": "install-uid"}, nil)}
	theirs.Object["spec"] = map[string]any{"owner": "alice"}
	cluster.Create(t, theirs)

	trail := func() int {
		t.Helper()
		entries, err := system.Identity.Store.AuditEntries(ctx, 500)
		if err != nil {
			t.Fatalf("read the audit stream: %v", err)
		}
		return len(entries)
	}
	before := trail()

	status, body := system.Call(t, bob, http.MethodPost, "/api/v1/plan-requests", map[string]any{
		"scope":   map[string]any{"ownerUID": "install-uid"},
		"objects": []any{harness.Manifest("Application", harness.Namespace, "mine", nil, nil)},
	}, nil)
	if status != http.StatusForbidden {
		t.Fatalf("plan naming another owner's scope = %d %v, want 403", status, body)
	}
	if after := trail(); after != before {
		t.Fatalf("a refused plan changed the audit trail: %d -> %d", before, after)
	}
}

// TestTC36UnrecordableAccountName refuses a name the audit record cannot carry
// before the attempt is recorded, so no trail entry reads as two accounts
// (C-14).
func TestTC36UnrecordableAccountName(t *testing.T) {
	cluster := harness.Start(t)
	system := cluster.StartAccess(t, nil, nil)
	ctx := context.Background()
	owner := system.Owner(t, "owner", "owner-password")

	trail := func() int {
		t.Helper()
		entries, err := system.Identity.Store.AuditEntries(ctx, 500)
		if err != nil {
			t.Fatalf("read the audit stream: %v", err)
		}
		return len(entries)
	}
	before := trail()

	status, body := system.Call(t, owner, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "intruder/extra", "role": string(write.RoleGuest),
	}, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("create an account whose name carries two segments = %d %v, want 400", status, body)
	}
	if after := trail(); after != before {
		t.Fatalf("an unrecordable account name grew the trail: %d -> %d", before, after)
	}
}

// gatedTrail holds one audit write until the case releases it, then reports the
// error a database driver reports when its context is already done.
type gatedTrail struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	got     []controlplane.AuditRecord
}

// Record implements controlplane.Auditor.
func (g *gatedTrail) Record(ctx context.Context, record controlplane.AuditRecord) error {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, record)
	return nil
}

// records returns the records the case let through.
func (g *gatedTrail) records() []controlplane.AuditRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]controlplane.AuditRecord(nil), g.got...)
}

// TestTC36AuditOutlivesADisconnectedClient cancels the request context while the
// write is in flight: the write runs on the control plane's own context (C-12),
// so the object and its record both survive the caller going away (C-14).
func TestTC36AuditOutlivesADisconnectedClient(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	trail := &gatedTrail{entered: make(chan struct{}), release: make(chan struct{})}
	gateway := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Auditor = trail
	})
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest(
		"Application", harness.Namespace, "cut", nil, nil)})

	body, err := json.Marshal(map[string]any{
		"kind": "Application", "namespace": harness.Namespace, "name": "cut", "action": app.ActionStop,
	})
	if err != nil {
		t.Fatalf("encode the action: %v", err)
	}
	// The request context is cancelled while the handler runs; the write itself
	// runs on the server's own context, so only what is bound to the request can
	// be lost (C-12).
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/api/v1/action-requests", bytes.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	answer := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		gateway.Server.Handler().ServeHTTP(answer, request)
	}()

	// The write has landed and its record is being appended; the caller now
	// goes away before the record is stored.
	<-trail.entered
	cancel()
	close(trail.release)
	<-served
	if answer.Code != http.StatusAccepted {
		t.Fatalf("stop = %d %s, want 202", answer.Code, answer.Body.String())
	}

	stored := cluster.Get(t, "v1beta1", "Application", "cut")
	if paused, _, _ := unstructured.NestedBool(stored.Object, "spec", "paused"); !paused {
		t.Fatalf("the stop did not reach the application: %v", stored.Object["spec"])
	}
	records := trail.records()
	if len(records) != 1 || records[0].Verb != app.ActionStop || records[0].Result != controlplane.ResultAccepted {
		t.Fatalf("audit after the caller disconnected = %+v, want one accepted %s record", records, app.ActionStop)
	}
}
