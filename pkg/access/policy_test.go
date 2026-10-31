package access_test

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/access"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// fixedSource answers kernel-object reads from a map keyed by kind, namespace
// and name.
type fixedSource struct {
	objects map[string]map[string]any
}

func sourceKey(object controlplane.ObjectType, namespace, name string) string {
	return object.Kind + "/" + namespace + "/" + name
}

// Get implements controlplane.ObjectSource.
func (s fixedSource) Get(_ context.Context, object controlplane.ObjectType, namespace, name string) (controlplane.ObservedObject, error) {
	found, ok := s.objects[sourceKey(object, namespace, name)]
	if !ok {
		return controlplane.ObservedObject{}, apierrors.NewNotFound(schema.GroupResource{Group: object.Group, Resource: object.Resource}, name)
	}
	return controlplane.ObservedObject{Object: found}, nil
}

// List implements controlplane.ObjectSource.
func (s fixedSource) List(context.Context, controlplane.ObjectType, string) ([]controlplane.ObservedObject, error) {
	return nil, nil
}

// TestMemberRestoreJobNeedsItsOwnPolicy covers C-41's restore read path: a
// member may restore only the run of a policy it owns, while the global Owner is
// not limited by ownership (C-59, C-60).
func TestMemberRestoreJobNeedsItsOwnPolicy(t *testing.T) {
	source := fixedSource{objects: map[string]map[string]any{
		"BackupPolicy/apps/mine":   {"spec": map[string]any{"owner": "alice"}},
		"BackupPolicy/apps/theirs": {"spec": map[string]any{"owner": "bob"}},
	}}
	policy := &access.Policy{Objects: source}
	request := func(policyName string) controlplane.Request {
		return controlplane.Request{
			Verb: controlplane.VerbCreate,
			Ref:  controlplane.ObjectRef{Group: api.Group, Kind: "RestoreJob", Namespace: "apps", Name: "restore"},
			Object: map[string]any{
				"metadata": map[string]any{"name": "restore", "namespace": "apps"},
				"spec":     map[string]any{"policy": policyName, "targetApplication": "restored"},
			},
		}
	}
	member := access.Session{Username: "alice", Role: write.RoleMember}
	if _, err := policy.Decide(context.Background(), member, request("mine")); err != nil {
		t.Fatalf("a member's own policy was refused: %v", err)
	}
	if _, err := policy.Decide(context.Background(), member, request("theirs")); err == nil {
		t.Fatal("a member restored another account's policy")
	}
	owner := access.Session{Username: "root", Role: write.RoleOwner}
	if _, err := policy.Decide(context.Background(), owner, request("theirs")); err != nil {
		t.Fatalf("the global Owner was refused: %v", err)
	}
}
