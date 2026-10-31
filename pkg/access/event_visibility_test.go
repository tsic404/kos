package access

import (
	"context"
	"errors"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// TestEventVisibilityFollowsTheSubject pins C-64's event rule: an application's
// events follow C-59's visibility, while every other object's events reach the
// global Owner and the member owning the object, and never a Guest.
func TestEventVisibilityFollowsTheSubject(t *testing.T) {
	policy := &Policy{}
	application := map[string]any{"spec": map[string]any{
		"owner":      "alice",
		"visibility": map[string]any{"members": []any{"bob"}},
	}}
	volume := map[string]any{"spec": map[string]any{"owner": "alice"}}
	applicationRef := controlplane.ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "shop"}
	volumeRef := controlplane.ObjectRef{Group: "kos.tsic.top", Kind: "Volume", Namespace: "default", Name: "data"}

	cases := []struct {
		name    string
		session Session
		ref     controlplane.ObjectRef
		object  map[string]any
		hidden  bool
	}{
		{name: "the global Owner reads any event", session: Session{Username: "root", Role: write.RoleOwner}, ref: volumeRef, object: volume},
		{name: "the owning member reads its object's event", session: Session{Username: "alice", Role: write.RoleMember}, ref: volumeRef, object: volume},
		{name: "another member does not read it", session: Session{Username: "bob", Role: write.RoleMember}, ref: volumeRef, object: volume, hidden: true},
		{name: "a guest does not read it", session: Session{Username: "guest", Role: write.RoleGuest}, ref: volumeRef, object: volume, hidden: true},
		{name: "an object without an owner reaches the Owner only", session: Session{Username: "alice", Role: write.RoleMember}, ref: volumeRef, object: nil, hidden: true},
		{name: "the application's owner reads its event", session: Session{Username: "alice", Role: write.RoleMember}, ref: applicationRef, object: application},
		{name: "a listed member reads it", session: Session{Username: "bob", Role: write.RoleMember}, ref: applicationRef, object: application},
		{name: "an unlisted member does not", session: Session{Username: "carol", Role: write.RoleMember}, ref: applicationRef, object: application, hidden: true},
		{name: "a guest does not read it", session: Session{Username: "guest", Role: write.RoleGuest}, ref: applicationRef, object: application, hidden: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := policy.Decide(context.Background(), testCase.session, controlplane.Request{
				Verb: controlplane.VerbEventRead, Ref: testCase.ref, Object: testCase.object,
			})
			if testCase.hidden {
				if !errors.Is(err, controlplane.ErrHidden) {
					t.Fatalf("decision = %v, want the event hidden", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decision = %v, want the event visible", err)
			}
		})
	}
}
