package write

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
)

// fakeWriter returns a writer over one fake cluster: a downstream Service and
// an extension group are declarable by resources and scope, the other test
// kinds are the built-in natives.
func fakeWriter(fake *dynamicfake.FakeDynamicClient) *Writer {
	return &Writer{
		Dynamic: fake,
		Resources: map[schema.GroupKind]string{
			{Kind: "Service"}: "services",
			{Group: "media.example.com", Kind: "Library"}: "libraries",
		},
		Scope: Scope{
			Namespaces: map[string]bool{"apps": true},
			Groups:     map[string]bool{"media.example.com": true},
		},
	}
}

// TestCreateStampsAttribution covers C-02's creator rule: a KOS subject creating
// an object in a class the unmanaged rule covers writes managed-by, so the
// object is not unmanaged to its creator's next write.
func TestCreateStampsAttribution(t *testing.T) {
	ctx := context.Background()
	claim := nativeStored("PersistentVolumeClaim", "apps", "data", map[string]string{markers.OwnerUID: "uid-volume"})
	claim.Object["spec"] = map[string]any{"accessModes": []any{"ReadWriteOnce"}}
	cases := []struct {
		name     string
		subject  Subject
		claim    Claim
		ownerUID string
		object   *unstructured.Unstructured
	}{
		{name: "downstream object", subject: SubjectApplicationReconciler, ownerUID: "uid-application", object: nativeStored("Service", "apps", "web", map[string]string{markers.OwnerUID: "uid-application"})},
		{name: "claim", subject: SubjectVolumeReconciler, claim: ClaimBound, ownerUID: "uid-volume", object: claim},
		{name: "KOS namespace object", subject: SubjectAPIProcess, object: nativeStored("ConfigMap", api.SystemNamespace, "record", nil)},
		{name: "extension object", subject: SubjectExtensionReconciler, object: objectStored("media.example.com/v1", "Library", "apps", "one", nil)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			writer := fakeWriter(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
			var created *unstructured.Unstructured
			var err error
			if test.claim == "" {
				created, err = writer.Create(ctx, test.subject, Session{}, test.object)
			} else {
				created, err = writer.CreateClaim(ctx, test.subject, Session{}, test.claim, test.object)
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if got := created.GetLabels()[markers.ManagedBy]; got != managedByValue {
				t.Fatalf("managed-by = %q, want %q", got, managedByValue)
			}
			if test.ownerUID != "" {
				if got := created.GetLabels()[markers.OwnerUID]; got != test.ownerUID {
					t.Fatalf("owner-uid = %q, want the caller's %q", got, test.ownerUID)
				}
			}
			if err := admitAttributed(Request{Subject: test.subject, Object: RefOf(created), Current: created}); err != nil {
				t.Fatalf("the creator's own object is still unmanaged: %v", err)
			}
		})
	}
}

// TestCreateDeniesOwnedObjectWithoutOwnerUID covers the other half of C-02's
// owner-uid rule: a creation that omits the marker is refused and no object is
// written, so a managed object cannot enter the cluster without an owner.
func TestCreateDeniesOwnedObjectWithoutOwnerUID(t *testing.T) {
	cases := []struct {
		name    string
		subject Subject
		claim   Claim
		object  *unstructured.Unstructured
	}{
		{name: "downstream object", subject: SubjectApplicationReconciler, object: nativeStored("Service", "apps", "web", nil)},
		{name: "claim", subject: SubjectVolumeReconciler, claim: ClaimBound, object: nativeStored("PersistentVolumeClaim", "apps", "data", nil)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fake := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
			writer := fakeWriter(fake)
			var err error
			if test.claim == "" {
				_, err = writer.Create(context.Background(), test.subject, Session{}, test.object)
			} else {
				_, err = writer.CreateClaim(context.Background(), test.subject, Session{}, test.claim, test.object)
			}
			var denied *Denied
			if !errors.As(err, &denied) || denied.Code() != 403 {
				t.Fatalf("create = %v, want a 403 refusal", err)
			}
			for _, action := range fake.Actions() {
				if action.GetVerb() == "create" {
					t.Fatal("an object was created without its owner UID")
				}
			}
		})
	}
}

// TestDeleteBindsTheReadObject covers the read-admit-delete window: the delete
// carries the UID of the object admission decided on, so a replacement under
// the same name is not removed under a stale decision.
func TestDeleteBindsTheReadObject(t *testing.T) {
	object := nativeStored("ConfigMap", api.SystemNamespace, "record", managedLabels)
	object.SetUID(types.UID("uid-record"))
	fake := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), object)
	writer := fakeWriter(fake)
	ref := Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "record"}
	if err := writer.Delete(context.Background(), SubjectInstaller, Session{}, ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var action clienttesting.DeleteAction
	for _, candidate := range fake.Actions() {
		if candidate, ok := candidate.(clienttesting.DeleteAction); ok {
			action = candidate
		}
	}
	if action == nil {
		t.Fatal("no delete was issued")
	}
	preconditions := action.GetDeleteOptions().Preconditions
	if preconditions == nil || preconditions.UID == nil || *preconditions.UID != types.UID("uid-record") {
		t.Fatalf("delete preconditions = %+v, want the read UID", preconditions)
	}
}

// TestDeleteDoesNotRemoveUnreadObject covers the other half of the window: when
// the read finds nothing, the delete returns NotFound and sends no DELETE, so an
// object created in between survives.
func TestDeleteDoesNotRemoveUnreadObject(t *testing.T) {
	object := nativeStored("ConfigMap", api.SystemNamespace, "record", managedLabels)
	object.SetUID(types.UID("uid-late"))
	fake := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), object)
	writer := fakeWriter(fake)
	read := false
	fake.PrependReactor("get", "configmaps", func(clienttesting.Action) (bool, runtime.Object, error) {
		if read {
			return false, nil, nil
		}
		read = true
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, "record")
	})
	ref := Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "record"}
	err := writer.Delete(context.Background(), SubjectInstaller, Session{}, ref)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("delete of an unread object = %v, want NotFound", err)
	}
	for _, action := range fake.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatal("a delete was issued for an object admission never read")
		}
	}
	if _, err := writer.Get(context.Background(), ref); err != nil {
		t.Fatalf("the object created after the read was removed: %v", err)
	}
}

// TestCreateKeepsOwnerUID covers C-02's owner-uid: the caller supplies the
// owner's UID in the creation document and the write path preserves it.
func TestCreateKeepsOwnerUID(t *testing.T) {
	ownerUID := "00000000-0000-0000-0000-0000000000aa"
	service := nativeStored("Service", "apps", "web", map[string]string{markers.OwnerUID: ownerUID})
	writer := fakeWriter(dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
	created, err := writer.Create(context.Background(), SubjectApplicationReconciler, Session{}, service)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := created.GetLabels()[markers.OwnerUID]; got != ownerUID {
		t.Fatalf("owner-uid = %q, want the caller's %q", got, ownerUID)
	}
	if got := created.GetLabels()[markers.ManagedBy]; got != managedByValue {
		t.Fatalf("managed-by = %q, want %q", got, managedByValue)
	}
}

// TestKeepsAttribution covers the marker half of keepsOwner: the manager that
// wrote the attribution refills it when a later render omits it.
func TestKeepsAttribution(t *testing.T) {
	current := nativeStored("Service", "apps", "web", map[string]string{
		markers.ManagedBy: managedByValue,
		markers.OwnerUID:  "uid-application",
	})
	metadata := current.Object["metadata"].(map[string]any)
	metadata["managedFields"] = []any{map[string]any{
		"manager":   manager(SubjectApplicationReconciler, Session{}),
		"operation": "Apply",
		"fieldsV1": map[string]any{
			"f:metadata": map[string]any{"f:labels": map[string]any{
				"f:managed-by": map[string]any{},
				"f:owner-uid":  map[string]any{},
			}},
		},
	}}
	document := nativeStored("Service", "apps", "web", nil)

	kept := keepsAttribution(document, SubjectApplicationReconciler, Session{}, current)
	if got := kept.GetLabels()[markers.ManagedBy]; got != managedByValue {
		t.Fatalf("the render dropped managed-by: %q", got)
	}
	if got := kept.GetLabels()[markers.OwnerUID]; got != "uid-application" {
		t.Fatalf("the render dropped owner-uid: %q", got)
	}

	already := nativeStored("Service", "apps", "web", map[string]string{markers.ManagedBy: "caller"})
	if got := keepsAttribution(already, SubjectApplicationReconciler, Session{}, current).GetLabels()[markers.ManagedBy]; got != "caller" {
		t.Fatalf("an explicit marker was overwritten: %q", got)
	}

	other := keepsAttribution(document, SubjectInstaller, Session{}, current)
	if _, found := other.GetLabels()[markers.ManagedBy]; found {
		t.Fatal("a manager that owns no attribution refilled it")
	}
}
