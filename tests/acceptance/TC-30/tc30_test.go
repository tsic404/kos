// Package tc30 verifies C-59's visibility predicate: the object's owner always
// sees an application, a member sees it only when the member list names the
// account, and a guest sees it only when guests are allowed (AC-16, TC-30).
package tc30

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// application builds a stored application whose owner and visibility the case
// controls. It is written directly, so the API's owner stamping is not part of
// what this case measures.
func application(name, owner string, members []string, allowGuests bool) *unstructured.Unstructured {
	object := harness.Application(name, nil)
	spec := map[string]any{"owner": owner}
	visibility := map[string]any{"allowGuests": allowGuests}
	if len(members) > 0 {
		listed := make([]any, 0, len(members))
		for _, member := range members {
			listed = append(listed, member)
		}
		visibility["members"] = listed
	}
	spec["visibility"] = visibility
	object.Object["spec"] = spec
	return object
}

// TestTC30VisibilityPredicate reads the application list and one application as
// the four identities C-59 names.
func TestTC30VisibilityPredicate(t *testing.T) {
	cluster := harness.Start(t)
	// "shared" is owned by alice and lists bob; "ghosted" also lists a name no
	// account carries, which C-59 ignores; "open" admits guests.
	cluster.Create(t, application("shared", "alice", []string{"bob", "ghost"}, false))
	cluster.Create(t, application("open", "alice", nil, true))

	system := cluster.StartAccess(t, nil, nil)
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	bob := system.Account(t, owner, "bob", "bob-password", write.RoleMember)
	carol := system.Account(t, owner, "carol", "carol-password", write.RoleMember)
	guest := system.Account(t, owner, "guest", "guest-password", write.RoleGuest)

	collection := "/api/v1/resources/Application?namespace=" + harness.Namespace
	cases := []struct {
		name  string
		token string
		want  []string
	}{
		{"the object's owner", alice, []string{"open", "shared"}},
		{"a member the list does not name", carol, nil},
		{"a member the list names", bob, []string{"shared"}},
		{"a guest without allowGuests", guest, []string{"open"}},
		{"the global Owner", owner, []string{"open", "shared"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := system.Call(t, testCase.token, http.MethodGet, collection, nil, nil)
			if status != http.StatusOK {
				t.Fatalf("list = %d %v", status, body)
			}
			got := names(t, body)
			if len(got) != len(testCase.want) {
				t.Fatalf("visible applications = %v, want %v", got, testCase.want)
			}
			for index := range got {
				if got[index] != testCase.want[index] {
					t.Fatalf("visible applications = %v, want %v", got, testCase.want)
				}
			}
		})
	}

	// A hidden application is absent, not refused: reading it is answered as if
	// it did not exist (C-59).
	status, body := system.Call(t, carol, http.MethodGet,
		"/api/v1/resources/Application/shared?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusNotFound {
		t.Fatalf("read a hidden application = %d %v, want 404", status, body)
	}
	if code := harness.String(body, "error", "code"); code != "not_found" {
		t.Fatalf("hidden read code = %q, want not_found", code)
	}
	status, body = system.Call(t, bob, http.MethodGet,
		"/api/v1/resources/Application/shared?namespace="+harness.Namespace, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("read a visible application = %d %v, want 200", status, body)
	}
}

// TestTC30DiagnosticsVisibility reads the evidence chain and the diagnostic
// bundle as the identities C-59 separates. The two documents are reads of one
// application like any other, so the same visibility predicate decides them,
// and a reader who may not see the application must not learn it exists.
func TestTC30DiagnosticsVisibility(t *testing.T) {
	cluster := harness.Start(t)
	world := harness.StartDiagnosticWorld(t, cluster)
	// The world writes the application directly, so this case supplies the
	// owner and the member list the predicate reads.
	own(t, cluster, world)

	system := cluster.StartAccess(t, nil, func(config *controlplane.Config) {
		config.Diagnostics = cluster.DiagnosticService(world.Installation)
	})
	owner := system.Owner(t, "owner", "owner-password")
	alice := system.Account(t, owner, "alice", "alice-password", write.RoleMember)
	bob := system.Account(t, owner, "bob", "bob-password", write.RoleMember)
	carol := system.Account(t, owner, "carol", "carol-password", write.RoleMember)

	chain := "/api/v1/diagnostics/" + world.Namespace + "/" + world.Application
	documents := []string{chain, chain + "/bundle"}

	// (a) A member the list does not name gets absence for both documents, not
	// a refusal that would confirm the application exists (C-59).
	for _, document := range documents {
		status, body := system.Call(t, carol, http.MethodGet, document, nil, nil)
		if status != http.StatusNotFound {
			t.Fatalf("member the list does not name read %s = %d %v, want 404", document, status, body)
		}
		if code := harness.String(body, "error", "code"); code != "not_found" {
			t.Fatalf("member the list does not name read %s with code %q, want not_found", document, code)
		}
	}

	// (b) The application's owner and a member the list names read both.
	for _, identity := range []struct {
		name  string
		token string
	}{{"the object's owner", alice}, {"a member the list names", bob}} {
		for _, document := range documents {
			status, body := system.Call(t, identity.token, http.MethodGet, document, nil, nil)
			if status != http.StatusOK {
				t.Fatalf("%s read %s = %d %v, want 200", identity.name, document, status, body)
			}
		}
	}

	// (c) A name no application carries is answered exactly as the hidden one
	// is: the bodies match field for field once the requested name replaces the
	// other, so the answer never tells the two apart.
	const absent = "vendor"
	for _, suffix := range []string{"", "/bundle"} {
		hiddenStatus, hiddenBody := system.Call(t, carol, http.MethodGet, chain+suffix, nil, nil)
		absentStatus, absentBody := system.Call(t, carol, http.MethodGet,
			"/api/v1/diagnostics/"+world.Namespace+"/"+absent+suffix, nil, nil)
		if hiddenStatus != http.StatusNotFound || absentStatus != http.StatusNotFound {
			t.Fatalf("hidden %s = %d and absent %s = %d, want 404 both",
				world.Application, hiddenStatus, absent, absentStatus)
		}
		if got, want := renamed(hiddenBody, world.Namespace, world.Application), renamed(absentBody, world.Namespace, absent); !reflect.DeepEqual(got, want) {
			t.Fatalf("the answer to a hidden application differs from the one to an absent name: %v vs %v", got, want)
		}
	}
}

// own marks the world's application as alice's and lists bob, leaving carol
// unnamed.
func own(t *testing.T, cluster *harness.Cluster, world *harness.DiagnosticWorld) {
	t.Helper()
	resource, err := cluster.Resource("v1beta1", "Application", world.Namespace)
	if err != nil {
		t.Fatalf("resolve applications in %s: %v", world.Namespace, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	object, err := resource.Get(ctx, world.Application, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the application %s: %v", world.Application, err)
	}
	if err := unstructured.SetNestedField(object.Object, "alice", "spec", "owner"); err != nil {
		t.Fatalf("set the owner of %s: %v", world.Application, err)
	}
	if err := unstructured.SetNestedMap(object.Object, map[string]any{
		"allowGuests": false,
		"members":     []any{"bob"},
	}, "spec", "visibility"); err != nil {
		t.Fatalf("set the visibility of %s: %v", world.Application, err)
	}
	if _, err := resource.Update(ctx, object, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write the owner of %s: %v", world.Application, err)
	}
}

// renamed replaces the requested application, in the two places an answer names
// it, with a placeholder: the identity itself and the reference it appears in.
// Everything else stays untouched, so the two answers are compared field for
// field without the caller's chosen name separating them.
func renamed(value any, namespace, name string) any {
	switch typed := value.(type) {
	case map[string]any:
		compared := make(map[string]any, len(typed))
		for key, entry := range typed {
			compared[key] = renamed(entry, namespace, name)
		}
		return compared
	case []any:
		compared := make([]any, len(typed))
		for index, entry := range typed {
			compared[index] = renamed(entry, namespace, name)
		}
		return compared
	case string:
		if typed == name {
			return "<application>"
		}
		return strings.ReplaceAll(typed, namespace+"/"+name, namespace+"/<application>")
	default:
		return value
	}
}

// names returns the sorted names of a list response.

func names(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("list response has no items: %v", body)
	}
	found := make([]string, 0, len(items))
	for _, item := range items {
		object, _ := item.(map[string]any)
		found = append(found, harness.String(object, "object", "metadata", "name"))
	}
	sort.Strings(found)
	return found
}
