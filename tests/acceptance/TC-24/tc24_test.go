// Package tc24 verifies the write path's concurrency rules: one field set has
// one winner and a 409 for the loser, disjoint field sets both survive, and a
// user who does not own an object is refused (AC-12, TC-24).
package tc24

import (
	"context"
	"errors"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC24ConcurrentWrites races the object's owner against the global Owner and
// checks what each writer observes.
func TestTC24ConcurrentWrites(t *testing.T) {
	cluster := harness.Start(t)
	writer := cluster.Writer()
	ctx := context.Background()
	alice := write.Session{Username: "alice", Role: write.RoleMember}
	bob := write.Session{Username: "bob", Role: write.RoleOwner}
	carol := write.Session{Username: "carol", Role: write.RoleMember}

	shared, err := writer.Create(ctx, write.SubjectUser, alice, application("shared"))
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	if owner, _, _ := unstructured.NestedString(shared.Object, "spec", "owner"); owner != alice.Username {
		t.Fatalf("created object owner = %q, want %q", owner, alice.Username)
	}

	// Two writers of the same field set: one applies, the other is told the
	// object is not its to overwrite.
	members := race(t, ctx, writer, []write.Session{alice, bob}, func(session write.Session) *unstructured.Unstructured {
		return membersDocument("shared", session.Username)
	})
	winner := ""
	conflicts := 0
	for _, outcome := range members {
		switch {
		case outcome.err == nil:
			if winner != "" {
				t.Fatalf("%s and another writer both won the same field set", outcome.session.Username)
			}
			winner = outcome.session.Username
		default:
			var conflict *write.Conflict
			if !errors.As(outcome.err, &conflict) {
				t.Fatalf("%s failed with %v, want a 409 conflict", outcome.session.Username, outcome.err)
			}
			if conflict.Code() != 409 {
				t.Fatalf("conflict code = %d, want 409", conflict.Code())
			}
			if conflict.Current == nil {
				t.Fatalf("%s got a conflict without the current object", outcome.session.Username)
			}
			if conflict.Current.GetUID() != shared.GetUID() {
				t.Fatalf("conflict carries %s, want %s", conflict.Current.GetUID(), shared.GetUID())
			}
			conflicts++
		}
	}
	if winner == "" || conflicts != 1 {
		t.Fatalf("winners = %q, conflicts = %d; want one of each", winner, conflicts)
	}
	sharedStored := cluster.Get(t, "v1beta1", "Application", "shared")
	if got := membersOf(t, sharedStored); len(got) != 1 || got[0] != winner {
		t.Fatalf("stored members = %v, want the winner's [%s]", got, winner)
	}
	// The creator's own apply omits spec.owner; its field manager would delete
	// the field, and C-60 makes it immutable after creation.
	if owner, _, _ := unstructured.NestedString(sharedStored.Object, "spec", "owner"); owner != alice.Username {
		t.Fatalf("the merged apply erased spec.owner: %q, want %q", owner, alice.Username)
	}

	// Disjoint field sets are both kept.
	if _, err := writer.Create(ctx, write.SubjectUser, alice, application("separate")); err != nil {
		t.Fatalf("create second application: %v", err)
	}
	outcomes := race(t, ctx, writer, []write.Session{alice, bob}, func(session write.Session) *unstructured.Unstructured {
		if session.Username == alice.Username {
			return membersDocument("separate", alice.Username)
		}
		return guestsDocument("separate", true)
	})
	for _, outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("%s wrote a disjoint field set and failed: %v", outcome.session.Username, outcome.err)
		}
	}
	separate := cluster.Get(t, "v1beta1", "Application", "separate")
	if got := membersOf(t, separate); len(got) != 1 || got[0] != alice.Username {
		t.Fatalf("members = %v, want [%s]", got, alice.Username)
	}
	if guests, _, _ := unstructured.NestedBool(separate.Object, "spec", "visibility", "allowGuests"); !guests {
		t.Fatal("allowGuests was lost when the other writer applied a different field")
	}
	// The owner's own apply omits spec.owner; its field manager would delete
	// the field, and C-60 makes it immutable after creation.
	if owner, _, _ := unstructured.NestedString(separate.Object, "spec", "owner"); owner != alice.Username {
		t.Fatalf("the merged apply erased spec.owner: %q, want %q", owner, alice.Username)
	}

	// A merged apply can create the object as well: the API still writes the
	// owner from the session, and the next merged write keeps it.
	if _, err := writer.Apply(ctx, write.SubjectUser, alice, write.PartSpec, guestsDocument("applied", true)); err != nil {
		t.Fatalf("apply-create application: %v", err)
	}
	applied := cluster.Get(t, "v1beta1", "Application", "applied")
	if owner, _, _ := unstructured.NestedString(applied.Object, "spec", "owner"); owner != alice.Username {
		t.Fatalf("apply-created owner = %q, want %q", owner, alice.Username)
	}
	if _, err := writer.Apply(ctx, write.SubjectUser, alice, write.PartSpec, membersDocument("applied", alice.Username)); err != nil {
		t.Fatalf("alice writes its own members: %v", err)
	}
	applied = cluster.Get(t, "v1beta1", "Application", "applied")
	if owner, _, _ := unstructured.NestedString(applied.Object, "spec", "owner"); owner != alice.Username {
		t.Fatalf("the merged apply erased spec.owner: %q, want %q", owner, alice.Username)
	}
	spoofed := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "spoofed", "namespace": harness.Namespace},
		"spec":       map[string]any{"owner": bob.Username},
	}}
	var spoofDenied *write.Denied
	if _, err := writer.Apply(ctx, write.SubjectUser, alice, write.PartSpec, spoofed); !errors.As(err, &spoofDenied) || spoofDenied.Code() != 403 {
		t.Fatalf("apply-create accepted another user's owner: %v", err)
	}

	// A member who owns neither object is refused, and nothing changes.
	_, err = writer.Apply(ctx, write.SubjectUser, carol, write.PartSpec, membersDocument("shared", carol.Username))
	var denied *write.Denied
	if !errors.As(err, &denied) {
		t.Fatalf("a non-owner write was accepted: %v", err)
	}
	if denied.Code() != 403 {
		t.Fatalf("refusal code = %d, want 403", denied.Code())
	}
	after := cluster.Get(t, "v1beta1", "Application", "shared")
	if after.GetResourceVersion() != sharedStored.GetResourceVersion() {
		t.Fatal("the refused write changed the object")
	}

	// C-59: a member deletes only what it owns, and the kinds only the Owner
	// manages are the Owner's to delete.
	guarded, err := writer.Create(ctx, write.SubjectUser, alice, application("guarded"))
	if err != nil {
		t.Fatalf("create guarded application: %v", err)
	}
	err = writer.Delete(ctx, write.SubjectUser, carol, write.RefOf(guarded))
	if !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a member deleted another owner's application: %v", err)
	}
	provider := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Provider",
		"metadata": map[string]any{
			"name":   "guarded-provider",
			"labels": map[string]any{markers.ManagedBy: "kos-shell"},
		},
	}}
	cluster.Create(t, provider)
	providerRef := write.Ref{Group: api.Group, Version: "v1beta1", Kind: "Provider", Name: "guarded-provider"}
	err = writer.Delete(ctx, write.SubjectUser, alice, providerRef)
	if !errors.As(err, &denied) || denied.Code() != 403 {
		t.Fatalf("a member deleted a provider: %v", err)
	}
	if err := writer.Delete(ctx, write.SubjectUser, bob, providerRef); err != nil {
		t.Fatalf("the global Owner could not delete a provider: %v", err)
	}

	// C-05/C-60: a transfer forces spec.owner alone. Another writer's field
	// keeps its owner and the transfer reports the conflict.
	if _, err := writer.Create(ctx, write.SubjectUser, alice, application("transfer")); err != nil {
		t.Fatalf("create transfer application: %v", err)
	}
	if _, err := writer.Apply(ctx, write.SubjectUser, alice, write.PartSpec, membersDocument("transfer", alice.Username)); err != nil {
		t.Fatalf("alice writes her own members: %v", err)
	}
	mixed := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "transfer", "namespace": harness.Namespace},
		"spec": map[string]any{
			"owner":      carol.Username,
			"visibility": map[string]any{"members": []any{bob.Username}},
		},
	}}
	var conflict *write.Conflict
	if _, err := writer.Apply(ctx, write.SubjectUser, bob, write.PartSpec, mixed); !errors.As(err, &conflict) || conflict.Code() != 409 {
		t.Fatalf("a transfer overwrote another writer's field set: %v", err)
	}
	held := cluster.Get(t, "v1beta1", "Application", "transfer")
	if owner, _, _ := unstructured.NestedString(held.Object, "spec", "owner"); owner != alice.Username {
		t.Fatalf("a failed transfer changed the owner to %q", owner)
	}
	if got := membersOf(t, held); len(got) != 1 || got[0] != alice.Username {
		t.Fatalf("members = %v, want alice's [%s]", got, alice.Username)
	}

	// A transfer of the owner alone moves it, and the new owner keeps the
	// field through its own plain merged write.
	if _, err := writer.Apply(ctx, write.SubjectUser, bob, write.PartSpec, ownerDocument("transfer", carol.Username)); err != nil {
		t.Fatalf("transfer the application: %v", err)
	}
	if _, err := writer.Apply(ctx, write.SubjectUser, carol, write.PartSpec, guestsDocument("transfer", true)); err != nil {
		t.Fatalf("the new owner writes its own visibility: %v", err)
	}
	moved := cluster.Get(t, "v1beta1", "Application", "transfer")
	if owner, _, _ := unstructured.NestedString(moved.Object, "spec", "owner"); owner != carol.Username {
		t.Fatalf("owner after the transfer = %q, want %q", owner, carol.Username)
	}
	if guests, _, _ := unstructured.NestedBool(moved.Object, "spec", "visibility", "allowGuests"); !guests {
		t.Fatal("the new owner's write was lost")
	}
}

// ownerDocument applies only the owner, the one field C-60 lets the global
// Owner transfer.
func ownerDocument(name, owner string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"owner": owner},
	}}
}

// outcome is what one racer observed.
type outcome struct {
	session write.Session
	err     error
}

// race runs the applies of sessions at the same time.
func race(t *testing.T, ctx context.Context, writer *write.Writer, sessions []write.Session, document func(write.Session) *unstructured.Unstructured) []outcome {
	t.Helper()
	start := make(chan struct{})
	outcomes := make([]outcome, len(sessions))
	var wait sync.WaitGroup
	for index := range sessions {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := writer.Apply(ctx, write.SubjectUser, sessions[index], write.PartSpec, document(sessions[index]))
			outcomes[index] = outcome{session: sessions[index], err: err}
		}(index)
	}
	close(start)
	wait.Wait()
	return outcomes
}

// application builds the object a user creates, attribution included.
func application(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name":      name,
			"namespace": harness.Namespace,
			"labels":    map[string]any{markers.ManagedBy: "kos-shell"},
		},
	}}
}

// membersDocument applies the visibility member list.
func membersDocument(name string, members ...string) *unstructured.Unstructured {
	list := make([]any, 0, len(members))
	for _, member := range members {
		list = append(list, member)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"visibility": map[string]any{"members": list}},
	}}
}

// guestsDocument applies the guest visibility flag.
func guestsDocument(name string, allow bool) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": name, "namespace": harness.Namespace},
		"spec":       map[string]any{"visibility": map[string]any{"allowGuests": allow}},
	}}
}

// membersOf reads the stored visibility member list.
func membersOf(t *testing.T, object *unstructured.Unstructured) []string {
	t.Helper()
	raw, found, err := unstructured.NestedStringSlice(object.Object, "spec", "visibility", "members")
	if err != nil || !found {
		t.Fatalf("read members of %s: found=%v err=%v", object.GetName(), found, err)
	}
	return raw
}
