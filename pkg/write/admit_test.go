package write

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
)

// allSubjects is every subject the allocation tables know, written out so a
// subject that loses its table entry fails a test instead of disappearing.
var allSubjects = []Subject{
	SubjectUser,
	SubjectAPIProcess,
	SubjectInstaller,
	SubjectApplicationReconciler,
	SubjectEndpointReconciler,
	SubjectVolumeReconciler,
	SubjectBackupReconciler,
	SubjectMigrationReconciler,
	SubjectValidator,
	SubjectCatalogueSync,
	SubjectComponentRegistry,
	SubjectDiscovery,
	SubjectExtensionReconciler,
	SubjectProjection,
}

// member returns a signed-in member session.
func member() Session { return Session{Username: "alice", Role: RoleMember} }

// owner returns a signed-in global Owner session.
func owner() Session { return Session{Username: "root", Role: RoleOwner} }

// kernelRef names a kernel object in the default namespace.
func kernelRef(kind string) Ref {
	return Ref{Group: api.Group, Version: "v1beta1", Kind: kind, Namespace: "default", Name: "one"}
}

// stored returns a kernel object carrying the given labels.
func stored(kind string, labels map[string]string) *unstructured.Unstructured {
	raw := make(map[string]any, len(labels))
	for key, value := range labels {
		raw[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       kind,
		"metadata":   map[string]any{"name": "one", "namespace": "default", "labels": raw},
	}}
}

// managedLabels is a managed object's attribution (C-02).
var managedLabels = map[string]string{markers.ManagedBy: "kos-shell"}

// adoptedLabels is an adopted object's attribution (C-02).
var adoptedLabels = map[string]string{markers.AdoptedBy: "alice", markers.AdoptionReason: "existing"}

// creatorMarkers is what a creator's document carries: managed-by, plus the
// owner's UID for a class that has an owner. An empty ownerUID is the
// ownerless form C-02 allows for action records, definitions and device facts.
func creatorMarkers(ownerUID string) []MarkerWrite {
	writes := []MarkerWrite{{Key: markers.ManagedBy, Carrier: markers.CarrierLabel, Value: ManagedByValue, Present: true}}
	if ownerUID != "" {
		writes = append(writes, MarkerWrite{Key: markers.OwnerUID, Carrier: markers.CarrierLabel, Value: ownerUID, Present: true})
	}
	return writes
}

// TestStatusWritersMatchContract states C-03's status column literally.
func TestStatusWritersMatchContract(t *testing.T) {
	cases := map[string]Subject{
		"Application":     SubjectApplicationReconciler,
		"Endpoint":        SubjectEndpointReconciler,
		"Volume":          SubjectVolumeReconciler,
		"Provider":        SubjectValidator,
		"SystemComponent": SubjectBackupReconciler,
		"BackupPolicy":    SubjectBackupReconciler,
		"RestoreJob":      SubjectMigrationReconciler,
		"VolumeMigration": SubjectMigrationReconciler,
	}
	for _, kind := range api.Kinds() {
		kind := kind
		writer, hasWriter := cases[kind]
		t.Run(kind, func(t *testing.T) {
			for _, subject := range allSubjects {
				req := Request{
					Subject: subject,
					Session: member(),
					Object:  kernelRef(kind),
					Verb:    VerbApply,
					Parts:   []Part{PartStatus},
					Current: stored(kind, managedLabels),
				}
				err := Admit(req)
				wantAllowed := hasWriter && subject == writer
				if wantAllowed && err != nil {
					t.Fatalf("%s: %v", subject, err)
				}
				if !wantAllowed && err == nil {
					t.Fatalf("%s was allowed to write status", subject)
				}
			}
		})
	}
}

// TestSpecWritersMatchContract states C-03's spec column literally: three kinds
// are system owned, the rest are user owned.
func TestSpecWritersMatchContract(t *testing.T) {
	system := map[string]Subject{
		"ApplicationDefinition": SubjectCatalogueSync,
		"Provider":              SubjectComponentRegistry,
		"NodeDevice":            SubjectDiscovery,
	}
	for _, kind := range api.Kinds() {
		kind := kind
		writer, isSystem := system[kind]
		t.Run(kind, func(t *testing.T) {
			for _, subject := range allSubjects {
				req := Request{
					Subject: subject,
					Session: owner(),
					Object:  kernelRef(kind),
					Verb:    VerbApply,
					Parts:   []Part{PartSpec},
					Current: stored(kind, managedLabels),
				}
				err := Admit(req)
				var wantAllowed bool
				if isSystem {
					wantAllowed = subject == writer
				} else {
					wantAllowed = subject == SubjectUser
				}
				if wantAllowed && err != nil {
					t.Fatalf("%s: %v", subject, err)
				}
				if !wantAllowed && err == nil {
					t.Fatalf("%s was allowed to write the spec", subject)
				}
			}
		})
	}
}

// TestUserSpecWriteRoles covers C-59's write-only-own rule and C-60's owner
// transfer.
func TestUserSpecWriteRoles(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		session Session
		current *unstructured.Unstructured
		fields  []string
		want    bool
	}{
		{name: "owner writes any object", session: owner(), current: ownedBy("alice"), want: true},
		{name: "member writes its own object", session: member(), current: ownedBy("alice"), want: true},
		{name: "member is refused another owner's object", session: member(), current: ownedBy("bob")},
		{name: "member creates without an owner yet", session: member(), want: true},
		{name: "member creates no system component", session: member(), kind: "SystemComponent"},
		{name: "member writes no endpoint without a resolvable owner", session: member(), kind: "Endpoint", current: stored("Endpoint", managedLabels)},
		{name: "owner writes a system component", session: owner(), kind: "SystemComponent", want: true},
		{name: "member cannot transfer the owner", session: member(), current: ownedBy("alice"), fields: []string{"spec.owner"}},
		{name: "global owner transfers the owner", session: owner(), current: ownedBy("alice"), fields: []string{"spec.owner"}, want: true},
		{name: "guest is read-only", session: Session{Username: "guest", Role: RoleGuest}, current: ownedBy("guest")},
		{name: "user write without a session", session: Session{}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			kind := test.kind
			if kind == "" {
				kind = "Application"
			}
			err := Admit(Request{
				Subject: SubjectUser,
				Session: test.session,
				Object:  kernelRef(kind),
				Verb:    VerbApply,
				Parts:   []Part{PartSpec},
				Fields:  test.fields,
				Current: test.current,
			})
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestKernelDeleteOwnership covers C-59's delete path: a member deletes only
// what it owns, and the kinds only the Owner manages are refused to it.
func TestKernelDeleteOwnership(t *testing.T) {
	cases := []struct {
		name    string
		subject Subject
		kind    string
		session Session
		current *unstructured.Unstructured
		want    bool
	}{
		{name: "member deletes its own application", subject: SubjectUser, kind: "Application", session: member(), current: ownedBy("alice"), want: true},
		{name: "member is refused another owner's application", subject: SubjectUser, kind: "Application", session: member(), current: ownedBy("bob")},
		{name: "member is refused an ownerless application", subject: SubjectUser, kind: "Application", session: member(), current: stored("Application", managedLabels)},
		{name: "member is refused a provider", subject: SubjectUser, kind: "Provider", session: member(), current: stored("Provider", managedLabels)},
		{name: "member is refused a system component", subject: SubjectUser, kind: "SystemComponent", session: member(), current: stored("SystemComponent", managedLabels)},
		{name: "member is refused an application definition", subject: SubjectUser, kind: "ApplicationDefinition", session: member(), current: stored("ApplicationDefinition", managedLabels)},
		{name: "member is refused a node device", subject: SubjectUser, kind: "NodeDevice", session: member(), current: stored("NodeDevice", managedLabels)},
		{name: "owner deletes a provider", subject: SubjectUser, kind: "Provider", session: owner(), current: stored("Provider", managedLabels), want: true},
		{name: "guest deletes nothing", subject: SubjectUser, kind: "Application", session: Session{Username: "guest", Role: RoleGuest}, current: ownedBy("guest")},
		{name: "application reconciler deletes only cascade endpoints", subject: SubjectApplicationReconciler, kind: "Application", session: Session{}, current: ownedBy("alice")},
		{name: "application reconciler deletes a cascade endpoint", subject: SubjectApplicationReconciler, kind: "Endpoint", session: Session{}, current: stored("Endpoint", managedLabels), want: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(Request{
				Subject: test.subject,
				Session: test.session,
				Object:  kernelRef(test.kind),
				Verb:    VerbDelete,
				Parts:   []Part{PartObject},
				Current: test.current,
			})
			if test.want && err != nil {
				t.Fatalf("delete refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("delete allowed")
			}
		})
	}
}

// TestDeclineAndValidityMarkers covers C-03's marker column.
func TestDeclineAndValidityMarkers(t *testing.T) {
	cases := []struct {
		name    string
		subject Subject
		key     string
		want    bool
	}{
		{name: "user declines", subject: SubjectUser, key: markers.Declined, want: true},
		{name: "user cannot release", subject: SubjectUser, key: markers.Released},
		{name: "volume reconciler releases", subject: SubjectVolumeReconciler, key: markers.Released, want: true},
		{name: "migration releases", subject: SubjectMigrationReconciler, key: markers.Released, want: true},
		{name: "validator marks invalid", subject: SubjectValidator, key: markers.Invalid, want: true},
		{name: "application reconciler cannot mark invalid", subject: SubjectApplicationReconciler, key: markers.Invalid},
		{name: "projection writes nothing", subject: SubjectProjection, key: markers.Declined},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			carrier, _ := markers.CarrierOf(test.key)
			err := Admit(Request{
				Subject: test.subject,
				Session: owner(),
				Object:  kernelRef("Volume"),
				Verb:    VerbApply,
				Parts:   []Part{PartMarker},
				Markers: []MarkerWrite{{Key: test.key, Carrier: carrier, Value: "true", Present: true}},
				Current: stored("Volume", managedLabels),
			})
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestAttributionChanges covers C-02's rule that the three states are written
// once and only a release removes adoption markers.
func TestAttributionChanges(t *testing.T) {
	cases := []struct {
		name    string
		current *unstructured.Unstructured
		key     string
		value   string
		present bool
		want    bool
	}{
		{name: "unmanaged object is adopted", current: stored("Application", nil), key: markers.AdoptedBy, value: "alice", present: true, want: true},
		{name: "unmanaged object cannot claim management", current: stored("Application", nil), key: markers.ManagedBy, value: "kos-shell", present: true},
		{name: "adoption markers are written once", current: stored("Application", adoptedLabels), key: markers.AdoptedBy, value: "bob", present: true},
		{name: "release removes adoption markers", current: stored("Application", adoptedLabels), key: markers.AdoptedBy, present: false, want: true},
		{name: "managed attribution is fixed", current: stored("Application", managedLabels), key: markers.ManagedBy, value: "other", present: true},
		{name: "managed attribution rewrite is a no-op", current: stored("Application", managedLabels), key: markers.ManagedBy, value: "kos-shell", present: true, want: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			carrier, _ := markers.CarrierOf(test.key)
			err := Admit(Request{
				Subject: SubjectUser,
				Session: owner(),
				Object:  kernelRef("Application"),
				Verb:    VerbApply,
				Parts:   []Part{PartMarker},
				Markers: []MarkerWrite{{Key: test.key, Carrier: carrier, Value: test.value, Present: test.present}},
				Current: test.current,
			})
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestMarkerCarrier covers C-02's one-carrier-per-key rule: a marker written on
// the other carrier is refused, not ignored.
func TestMarkerCarrier(t *testing.T) {
	err := Admit(Request{
		Subject: SubjectUser,
		Session: owner(),
		Object:  kernelRef("Application"),
		Verb:    VerbApply,
		Parts:   []Part{PartMarker},
		Markers: []MarkerWrite{{Key: markers.ManagedBy, Carrier: markers.CarrierAnnotation, Value: "kos-shell", Present: true}},
		Current: stored("Application", nil),
	})
	var denied *Denied
	if !errors.As(err, &denied) {
		t.Fatalf("misplaced marker was not refused: %v", err)
	}
	if !strings.Contains(denied.Reason, markers.ManagedBy) {
		t.Fatalf("refusal does not name the key: %s", denied.Reason)
	}
}

// TestKOSSubjectsDoNotWriteUnmanagedObjects covers C-02 in every class KOS
// creates itself: kernel status, downstream objects, claims and KOS-namespace
// objects are all refused without an attribution marker.
func TestKOSSubjectsDoNotWriteUnmanagedObjects(t *testing.T) {
	cases := []struct {
		name string
		req  Request
	}{
		{
			name: "application reconciler writes an unmanaged kernel status",
			req: Request{
				Subject: SubjectApplicationReconciler,
				Object:  kernelRef("Application"),
				Verb:    VerbApply,
				Parts:   []Part{PartStatus},
				Current: stored("Application", nil),
			},
		},
		{
			name: "application reconciler applies an unmanaged downstream object",
			req: Request{
				Subject: SubjectApplicationReconciler,
				Object:  Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject},
				Scope:   namespaces("apps"),
				Current: nativeStored("Service", "apps", "web", nil),
			},
		},
		{
			name: "application reconciler deletes an unmanaged downstream object",
			req: Request{
				Subject: SubjectApplicationReconciler,
				Object:  Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"},
				Verb:    VerbDelete,
				Parts:   []Part{PartObject},
				Scope:   namespaces("apps"),
				Current: nativeStored("Service", "apps", "web", nil),
			},
		},
		{
			name: "volume reconciler applies an unmanaged claim",
			req: Request{
				Subject: SubjectVolumeReconciler,
				Object:  Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject},
				Claim:   ClaimBound,
				Current: nativeStored("PersistentVolumeClaim", "apps", "data", nil),
			},
		},
		{
			name: "installer applies an unmanaged object outside its namespace",
			req: Request{
				Subject: SubjectInstaller,
				Object:  Ref{Version: "v1", Kind: "ConfigMap", Namespace: "kube-system", Name: "foreign"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject},
				Scope:   namespaces("kube-system"),
				Current: nativeStored("ConfigMap", "kube-system", "foreign", nil),
			},
		},
		{
			name: "installer applies an unmanaged KOS namespace object",
			req: Request{
				Subject: SubjectInstaller,
				Object:  Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "install"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject},
				Scope:   namespaces(api.SystemNamespace),
				Current: nativeStored("ConfigMap", api.SystemNamespace, "install", nil),
			},
		},
		{
			name: "extension controller applies an unmanaged extension object",
			req: Request{
				Subject: SubjectExtensionReconciler,
				Object:  Ref{Group: "media.example.com", Version: "v1", Kind: "Library", Namespace: "apps", Name: "one"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject},
				Scope:   Scope{Groups: map[string]bool{"media.example.com": true}},
				Current: objectStored("media.example.com/v1", "Library", "apps", "one", nil),
			},
		},
		{
			name: "validator writes an unmanaged provider status",
			req: Request{
				Subject: SubjectValidator,
				Object:  kernelRef("Provider"),
				Verb:    VerbApply,
				Parts:   []Part{PartStatus},
				Current: stored("Provider", nil),
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(test.req)
			var denied *Denied
			if !errors.As(err, &denied) {
				t.Fatalf("write was not refused: %v", err)
			}
			if denied.Code() != 403 {
				t.Fatalf("refusal code = %d, want 403", denied.Code())
			}
		})
	}
}

// TestApplyCreateMayWriteAttribution covers the merged apply that creates: the
// writer is the creator, so it may write managed-by (C-02).
func TestApplyCreateMayWriteAttribution(t *testing.T) {
	marker := MarkerWrite{Key: markers.ManagedBy, Carrier: markers.CarrierLabel, Value: ManagedByValue, Present: true}
	cases := []struct {
		name string
		req  Request
	}{
		{
			name: "downstream object",
			req: Request{
				Subject: SubjectApplicationReconciler,
				Object:  Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject, PartMarker},
				Scope:   namespaces("apps"),
				Markers: creatorMarkers("uid-application"),
			},
		},
		{
			name: "claim",
			req: Request{
				Subject: SubjectVolumeReconciler,
				Object:  Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject, PartMarker},
				Claim:   ClaimBound,
				Markers: creatorMarkers("uid-volume"),
			},
		},
		{
			name: "KOS namespace object",
			req: Request{
				Subject: SubjectInstaller,
				Object:  Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "record"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject, PartMarker},
				Markers: []MarkerWrite{marker},
			},
		},
		{
			name: "extension object",
			req: Request{
				Subject: SubjectExtensionReconciler,
				Object:  Ref{Group: "media.example.com", Version: "v1", Kind: "Library", Namespace: "apps", Name: "one"},
				Verb:    VerbApply,
				Parts:   []Part{PartObject, PartMarker},
				Scope:   Scope{Groups: map[string]bool{"media.example.com": true}},
				Markers: []MarkerWrite{marker},
			},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := Admit(test.req); err != nil {
				t.Fatalf("apply-create could not write its attribution: %v", err)
			}
		})
	}
}

// TestOwnedCreationNeedsOwnerUID covers C-02's existence rule: creating an
// object that has an owner without the owner's UID is refused, while a class
// with no owner is created with managed-by alone.
func TestOwnedCreationNeedsOwnerUID(t *testing.T) {
	downstream := Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"}
	claim := Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data"}
	provider := kernelRef("Provider")
	cases := []struct {
		name string
		req  Request
		want bool
	}{
		{
			name: "application reconciler creates a downstream object without owner-uid",
			req: Request{
				Subject: SubjectApplicationReconciler, Object: downstream, Verb: VerbCreate,
				Parts: []Part{PartObject, PartMarker}, Scope: namespaces("apps"),
				Markers: creatorMarkers(""),
			},
		},
		{
			name: "application reconciler creates a downstream object with an empty owner-uid",
			req: Request{
				Subject: SubjectApplicationReconciler, Object: downstream, Verb: VerbCreate,
				Parts: []Part{PartObject, PartMarker}, Scope: namespaces("apps"),
				Markers: []MarkerWrite{
					{Key: markers.ManagedBy, Carrier: markers.CarrierLabel, Value: ManagedByValue, Present: true},
					{Key: markers.OwnerUID, Carrier: markers.CarrierLabel, Value: "", Present: true},
				},
			},
		},
		{
			name: "application reconciler creates a downstream object with owner-uid",
			req: Request{
				Subject: SubjectApplicationReconciler, Object: downstream, Verb: VerbCreate,
				Parts: []Part{PartObject, PartMarker}, Scope: namespaces("apps"),
				Markers: creatorMarkers("uid-application"),
			},
			want: true,
		},
		{
			name: "merged apply that creates a downstream object needs owner-uid",
			req: Request{
				Subject: SubjectApplicationReconciler, Object: downstream, Verb: VerbApply,
				Parts: []Part{PartObject, PartMarker}, Scope: namespaces("apps"),
				Markers: creatorMarkers(""),
			},
		},
		{
			name: "volume reconciler creates a bound claim without owner-uid",
			req: Request{
				Subject: SubjectVolumeReconciler, Object: claim, Verb: VerbCreate,
				Parts: []Part{PartObject, PartMarker}, Claim: ClaimBound,
				Markers: creatorMarkers(""),
			},
		},
		{
			name: "migration flow creates a target claim with owner-uid",
			req: Request{
				Subject: SubjectMigrationReconciler, Object: claim, Verb: VerbCreate,
				Parts: []Part{PartObject, PartMarker}, Claim: ClaimMigrationTarget,
				Markers: creatorMarkers("uid-migration"),
			},
			want: true,
		},
		{
			name: "component registry creates a Provider without owner-uid",
			req: Request{
				Subject: SubjectComponentRegistry, Object: provider, Verb: VerbCreate,
				Parts: []Part{PartSpec, PartMarker}, Markers: creatorMarkers(""),
			},
		},
		{
			name: "catalogue sync creates a definition with managed-by alone",
			req: Request{
				Subject: SubjectCatalogueSync, Object: kernelRef("ApplicationDefinition"), Verb: VerbCreate,
				Parts: []Part{PartSpec, PartMarker}, Markers: creatorMarkers(""),
			},
			want: true,
		},
		{
			name: "discovery creates a device fact with managed-by alone",
			req: Request{
				Subject: SubjectDiscovery, Object: kernelRef("NodeDevice"), Verb: VerbCreate,
				Parts: []Part{PartSpec, PartMarker}, Markers: creatorMarkers(""),
			},
			want: true,
		},
		{
			name: "API process creates an action record with managed-by alone",
			req: Request{
				Subject: SubjectAPIProcess,
				Object:  Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "record"},
				Verb:    VerbCreate, Parts: []Part{PartObject, PartMarker},
				Markers: creatorMarkers(""),
			},
			want: true,
		},
		{
			name: "extension controller creates an extension object with managed-by alone",
			req: Request{
				Subject: SubjectExtensionReconciler,
				Object:  Ref{Group: "media.example.com", Version: "v1", Kind: "Library", Namespace: "apps", Name: "one"},
				Verb:    VerbCreate, Parts: []Part{PartObject, PartMarker},
				Scope:   Scope{Groups: map[string]bool{"media.example.com": true}},
				Markers: creatorMarkers(""),
			},
			want: true,
		},
		{
			name: "a render of a stored object may omit owner-uid",
			req: Request{
				Subject: SubjectApplicationReconciler, Object: downstream, Verb: VerbApply,
				Parts: []Part{PartObject}, Scope: namespaces("apps"),
				Current: nativeStored("Service", "apps", "web", map[string]string{
					markers.ManagedBy: ManagedByValue, markers.OwnerUID: "uid-application",
				}),
			},
			want: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(test.req)
			if test.want {
				if err != nil {
					t.Fatalf("write refused: %v", err)
				}
				return
			}
			var denied *Denied
			if !errors.As(err, &denied) {
				t.Fatalf("write was not refused: %v", err)
			}
			if denied.Code() != 403 {
				t.Fatalf("refusal code = %d, want 403", denied.Code())
			}
			if !strings.Contains(denied.Reason, markers.OwnerUID) {
				t.Fatalf("refusal does not name %s: %s", markers.OwnerUID, denied.Reason)
			}
		})
	}
}

// TestDownstreamAllocation covers C-04's downstream column.
func TestDownstreamAllocation(t *testing.T) {
	ref := Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"}
	cases := []struct {
		name    string
		subject Subject
		scope   Scope
		verb    Verb
		want    bool
	}{
		{name: "application reconciler renders into its namespace", subject: SubjectApplicationReconciler, scope: namespaces("apps"), verb: VerbApply, want: true},
		{name: "application reconciler stays inside its namespaces", subject: SubjectApplicationReconciler, scope: namespaces("other"), verb: VerbApply},
		{name: "extension writes a declared kind in a declared namespace", subject: SubjectExtensionReconciler, scope: downstreamScope([]string{"apps"}, schema.GroupKind{Kind: "Service"}), verb: VerbCreate, want: true},
		{name: "extension stays inside its declared namespaces", subject: SubjectExtensionReconciler, scope: downstreamScope([]string{"other"}, schema.GroupKind{Kind: "Service"}), verb: VerbCreate},
		{name: "extension writes only declared kinds", subject: SubjectExtensionReconciler, scope: downstreamScope([]string{"apps"}, schema.GroupKind{Kind: "ConfigMap"}), verb: VerbCreate},
		{name: "extension without a declaration writes nothing", subject: SubjectExtensionReconciler, verb: VerbCreate},
		{name: "backup reconciler writes no downstream object until its CronJob row lands", subject: SubjectBackupReconciler, verb: VerbCreate},
		{name: "migration flow writes no downstream object until its Job row lands", subject: SubjectMigrationReconciler, verb: VerbCreate},
		{name: "volume reconciler writes no downstream object", subject: SubjectVolumeReconciler, verb: VerbApply},
		{name: "user writes no downstream object", subject: SubjectUser, verb: VerbApply},
		{name: "projection writes nothing", subject: SubjectProjection, verb: VerbApply},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(Request{
				Subject: test.subject,
				Session: owner(),
				Object:  ref,
				Verb:    test.verb,
				Parts:   []Part{PartObject},
				Markers: creatorMarkers("uid-owner"),
				Scope:   test.scope,
			})
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestUnrenderedDownstreamKindsStayDenied covers the C-04 rows this change does
// not implement: the backup reconciler's CronJob (C-41) and the migration flow's
// replication Job and target objects (C-40) are refused, including their own
// contract objects, rather than allowed by a class-wide row.
func TestUnrenderedDownstreamKindsStayDenied(t *testing.T) {
	cases := []struct {
		name    string
		subject Subject
		object  Ref
	}{
		{name: "backup reconciler writes no CronJob", subject: SubjectBackupReconciler, object: Ref{Group: "batch", Version: "v1", Kind: "CronJob", Namespace: "apps", Name: "nightly"}},
		{name: "migration flow writes no replication Job", subject: SubjectMigrationReconciler, object: Ref{Group: "batch", Version: "v1", Kind: "Job", Namespace: "apps", Name: "copy"}},
		{name: "migration flow writes no target object", subject: SubjectMigrationReconciler, object: Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "target"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(Request{
				Subject: test.subject,
				Object:  test.object,
				Verb:    VerbCreate,
				Parts:   []Part{PartObject},
				Markers: creatorMarkers("uid-owner"),
				Scope:   namespaces("apps"),
			})
			var denied *Denied
			if !errors.As(err, &denied) {
				t.Fatalf("write was not refused: %v", err)
			}
			if denied.Code() != 403 {
				t.Fatalf("refusal code = %d, want 403", denied.Code())
			}
		})
	}
}

// TestClaimAllocation covers C-04's claim column: a claim write names its row,
// and the volume reconciler owns bound claims while the migration flow owns its
// targets. A bound claim must stay out of the migration flow's reach (F6).
func TestClaimAllocation(t *testing.T) {
	ref := Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data"}
	for _, test := range []struct {
		name    string
		subject Subject
		verb    Verb
		claim   Claim
		want    bool
	}{
		{name: "volume reconciler creates a bound claim", subject: SubjectVolumeReconciler, verb: VerbCreate, claim: ClaimBound, want: true},
		{name: "volume reconciler expands a bound claim", subject: SubjectVolumeReconciler, verb: VerbApply, claim: ClaimBound, want: true},
		{name: "volume reconciler deletes a bound claim", subject: SubjectVolumeReconciler, verb: VerbDelete, claim: ClaimBound, want: true},
		{name: "migration flow creates a target claim", subject: SubjectMigrationReconciler, verb: VerbCreate, claim: ClaimMigrationTarget, want: true},
		{name: "migration flow deletes a target claim", subject: SubjectMigrationReconciler, verb: VerbDelete, claim: ClaimMigrationTarget, want: true},
		{name: "migration flow does not expand a target claim", subject: SubjectMigrationReconciler, verb: VerbApply, claim: ClaimMigrationTarget},
		{name: "migration flow does not delete a source claim", subject: SubjectMigrationReconciler, verb: VerbDelete, claim: ClaimBound},
		{name: "migration flow does not create a source claim", subject: SubjectMigrationReconciler, verb: VerbCreate, claim: ClaimBound},
		{name: "volume reconciler does not delete a migration target", subject: SubjectVolumeReconciler, verb: VerbDelete, claim: ClaimMigrationTarget},
		{name: "a claim write without a role is refused", subject: SubjectVolumeReconciler, verb: VerbDelete},
		{name: "application reconciler writes no claim", subject: SubjectApplicationReconciler, verb: VerbCreate, claim: ClaimBound},
		{name: "user writes no claim", subject: SubjectUser, verb: VerbCreate, claim: ClaimBound},
		{name: "installer writes no claim", subject: SubjectInstaller, verb: VerbApply, claim: ClaimBound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := Request{Subject: test.subject, Session: owner(), Object: ref, Verb: test.verb, Parts: []Part{PartObject}, Claim: test.claim}
			if test.verb != VerbDelete {
				request.Markers = creatorMarkers("uid-owner")
			}
			err := Admit(request)
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestClaimReleaseMarker covers C-03's marker column on a claim: the volume
// reconciler and the migration flow may mark a bound claim released, which is
// how a migration unbinds a source claim without deleting it (C-39).
func TestClaimReleaseMarker(t *testing.T) {
	carrier, _ := markers.CarrierOf(markers.Released)
	ref := Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: "apps", Name: "data"}
	for _, subject := range []Subject{SubjectVolumeReconciler, SubjectMigrationReconciler} {
		t.Run(string(subject), func(t *testing.T) {
			err := Admit(Request{
				Subject: subject,
				Session: owner(),
				Object:  ref,
				Verb:    VerbApply,
				Parts:   []Part{PartMarker},
				Claim:   ClaimBound,
				Markers: []MarkerWrite{{Key: markers.Released, Carrier: carrier, Value: "true", Present: true}},
				Current: nativeStored("PersistentVolumeClaim", "apps", "data", map[string]string{
					markers.ManagedBy: ManagedByValue, markers.OwnerUID: "uid-volume",
				}),
			})
			if err != nil {
				t.Fatalf("release refused: %v", err)
			}
		})
	}
}

// TestNodeAllocation covers C-04's node rule: the API process writes the
// run-kernel node objects and never creates or deletes them.
func TestNodeAllocation(t *testing.T) {
	ref := Ref{Version: "v1", Kind: "Node", Name: "node-1"}
	for _, test := range []struct {
		subject Subject
		verb    Verb
		want    bool
	}{
		{subject: SubjectAPIProcess, verb: VerbApply, want: true},
		{subject: SubjectAPIProcess, verb: VerbCreate},
		{subject: SubjectAPIProcess, verb: VerbDelete},
		{subject: SubjectUser, verb: VerbApply},
		{subject: SubjectApplicationReconciler, verb: VerbApply},
		{subject: SubjectVolumeReconciler, verb: VerbApply},
	} {
		t.Run(fmt.Sprintf("%s-%s", test.subject, test.verb), func(t *testing.T) {
			request := Request{Subject: test.subject, Session: owner(), Object: ref, Verb: test.verb, Parts: []Part{PartObject}}
			if test.subject == SubjectApplicationReconciler && test.verb == VerbDelete {
				// C-04 grants the delete of consumed action records only.
				request.Object = Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: ActionRecordPrefix + "uid-1-retry"}
				request.Current = nativeStored("ConfigMap", api.SystemNamespace, request.Object.Name,
					map[string]string{markers.ManagedBy: ManagedByValue, ActionRecordLabel: "retry"})
			}
			err := Admit(request)
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestSystemNamespaceAllocation covers C-04's own-namespace column: the installer
// owns it, the API process creates action records, the application reconciler
// deletes what it consumed.
func TestSystemNamespaceAllocation(t *testing.T) {
	ref := Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "record"}
	for _, test := range []struct {
		subject Subject
		verb    Verb
		want    bool
	}{
		{subject: SubjectInstaller, verb: VerbDelete, want: true},
		{subject: SubjectAPIProcess, verb: VerbCreate, want: true},
		{subject: SubjectAPIProcess, verb: VerbApply},
		{subject: SubjectApplicationReconciler, verb: VerbDelete, want: true},
		{subject: SubjectApplicationReconciler, verb: VerbCreate},
		{subject: SubjectUser, verb: VerbCreate},
	} {
		t.Run(fmt.Sprintf("%s-%s", test.subject, test.verb), func(t *testing.T) {
			request := Request{Subject: test.subject, Session: owner(), Object: ref, Verb: test.verb, Parts: []Part{PartObject}}
			if test.subject == SubjectApplicationReconciler && test.verb == VerbDelete {
				// C-04 grants the delete of consumed action records only.
				request.Object = Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: ActionRecordPrefix + "uid-1-retry"}
				request.Current = nativeStored("ConfigMap", api.SystemNamespace, request.Object.Name,
					map[string]string{markers.ManagedBy: ManagedByValue, ActionRecordLabel: "retry"})
			}
			err := Admit(request)
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestInstallerDownstreamScope covers C-04's installer row: the installer
// writes downstream objects only in the namespaces it declares, even when the
// object already carries attribution.
func TestInstallerDownstreamScope(t *testing.T) {
	ref := Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"}
	current := nativeStored("Service", "apps", "web", managedLabels)
	err := Admit(Request{
		Subject: SubjectInstaller,
		Object:  ref,
		Verb:    VerbApply,
		Parts:   []Part{PartObject},
		Scope:   namespaces("kube-system"),
		Current: current,
	})
	var denied *Denied
	if !errors.As(err, &denied) {
		t.Fatalf("installer wrote an undeclared namespace: %v", err)
	}
	if denied.Code() != 403 {
		t.Fatalf("refusal code = %d, want 403", denied.Code())
	}
	err = Admit(Request{
		Subject: SubjectInstaller,
		Object:  ref,
		Verb:    VerbApply,
		Parts:   []Part{PartObject},
		Scope:   namespaces("apps"),
		Current: current,
	})
	if err != nil {
		t.Fatalf("installer was refused inside its declared namespace: %v", err)
	}
}

// TestExtensionAllocation covers C-20: one controller per declared group.
func TestExtensionAllocation(t *testing.T) {
	ref := Ref{Group: "media.example.com", Version: "v1", Kind: "Library", Namespace: "apps", Name: "one"}
	declared := Scope{Groups: map[string]bool{"media.example.com": true}}
	for _, test := range []struct {
		name    string
		subject Subject
		scope   Scope
		object  Ref
		want    bool
	}{
		{name: "declared controller writes its objects", subject: SubjectExtensionReconciler, scope: declared, object: ref, want: true},
		{name: "undeclared group is refused", subject: SubjectExtensionReconciler, scope: Scope{}, object: ref},
		{name: "kernel controller writes no extension object", subject: SubjectApplicationReconciler, scope: declared, object: ref},
		{name: "extension controller writes no kernel object", subject: SubjectExtensionReconciler, scope: declared, object: kernelRef("Application")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(Request{Subject: test.subject, Session: owner(), Object: test.object, Verb: VerbApply, Parts: []Part{PartObject}, Scope: test.scope})
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestUnlistedSubjectAndForeignObject covers the tables' default: absence is
// refusal.
func TestUnlistedSubjectAndForeignObject(t *testing.T) {
	if err := Admit(Request{Subject: Subject("typo"), Object: kernelRef("Application"), Verb: VerbApply, Parts: []Part{PartSpec}, Current: stored("Application", nil)}); err == nil {
		t.Fatal("an unknown subject was allowed to write")
	}
	if err := Admit(Request{Subject: SubjectApplicationReconciler, Object: Ref{Group: "other.example.com", Version: "v1", Kind: "Thing", Name: "x"}, Verb: VerbApply, Parts: []Part{PartObject}}); err == nil {
		t.Fatal("a foreign object was allowed to be written")
	}
}

// TestDenialNamesSubjectAndObject covers C-03's and C-20's criterion that a
// refusal names both.
func TestDenialNamesSubjectAndObject(t *testing.T) {
	err := Admit(Request{
		Subject: SubjectApplicationReconciler,
		Object:  kernelRef("Volume"),
		Verb:    VerbApply,
		Parts:   []Part{PartSpec},
		Current: stored("Volume", managedLabels),
	})
	var denied *Denied
	if !errors.As(err, &denied) {
		t.Fatalf("write was not refused: %v", err)
	}
	message := denied.Error()
	for _, want := range []string{string(SubjectApplicationReconciler), "Volume", "default/one"} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal %q does not name %q", message, want)
		}
	}
	if denied.Code() != 403 {
		t.Fatalf("refusal code = %d, want 403", denied.Code())
	}
}

// ownedBy returns a stored application owned by username.
func ownedBy(username string) *unstructured.Unstructured {
	object := stored("Application", managedLabels)
	object.Object["spec"] = map[string]any{"owner": username}
	return object
}

// nativeStored returns an object outside the kernel API group carrying the
// given labels.
func nativeStored(kind, namespace, name string, labels map[string]string) *unstructured.Unstructured {
	return objectStored("v1", kind, namespace, name, labels)
}

// objectStored returns an object of any group and version carrying the given
// labels.
func objectStored(apiVersion, kind, namespace, name string, labels map[string]string) *unstructured.Unstructured {
	raw := make(map[string]any, len(labels))
	for key, value := range labels {
		raw[key] = value
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   map[string]any{"name": name, "namespace": namespace, "labels": raw},
	}}
}

// namespaces builds a declared namespace scope.
func namespaces(names ...string) Scope {
	scope := Scope{Namespaces: make(map[string]bool, len(names))}
	for _, name := range names {
		scope.Namespaces[name] = true
	}
	return scope
}

// downstreamScope builds the downstream scope an extension declares: the
// namespaces and the native kinds it may write (C-18).
func downstreamScope(namespaceNames []string, kinds ...schema.GroupKind) Scope {
	scope := Scope{Namespaces: make(map[string]bool, len(namespaceNames)), Kinds: make(map[schema.GroupKind]bool, len(kinds))}
	for _, name := range namespaceNames {
		scope.Namespaces[name] = true
	}
	for _, kind := range kinds {
		scope.Kinds[kind] = true
	}
	return scope
}

// TestClassOfClaimBeforeNamespace covers C-04's classification: a claim in the
// KOS namespace is still a claim, not a system-namespace object.
func TestClassOfClaimBeforeNamespace(t *testing.T) {
	ref := Ref{Version: "v1", Kind: "PersistentVolumeClaim", Namespace: api.SystemNamespace, Name: "data"}
	if got := ClassOf(ref, Scope{}); got != ClassPVC {
		t.Fatalf("a claim in the KOS namespace is %q, want %q", got, ClassPVC)
	}
}

// TestEndpointRouteAllocation covers C-04's route row the way C-43 fixes it: the
// endpoint reconciler writes the two route kinds in one entry's namespace and
// nothing else, whatever the scope claims.
func TestEndpointRouteAllocation(t *testing.T) {
	cases := []struct {
		name   string
		object Ref
		scope  Scope
		verb   Verb
		want   bool
	}{
		{name: "ingress into the entry namespace", object: Ref{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Namespace: "apps", Name: "web"}, scope: namespaces("apps"), verb: VerbCreate, want: true},
		{name: "service into the entry namespace", object: Ref{Version: "v1", Kind: "Service", Namespace: "apps", Name: "web"}, scope: namespaces("apps"), verb: VerbApply, want: true},
		{name: "route outside the entry namespace", object: Ref{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Namespace: "other", Name: "web"}, scope: namespaces("apps"), verb: VerbCreate},
		{name: "another kind in the entry namespace", object: Ref{Version: "v1", Kind: "ConfigMap", Namespace: "apps", Name: "web"}, scope: namespaces("apps"), verb: VerbCreate},
		{name: "deleting the route", object: Ref{Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Namespace: "apps", Name: "web"}, scope: namespaces("apps"), verb: VerbDelete, want: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(Request{
				Subject: SubjectEndpointReconciler,
				Object:  test.object,
				Verb:    test.verb,
				Parts:   []Part{PartObject},
				Markers: creatorMarkers("uid-endpoint"),
				Scope:   test.scope,
			})
			if test.want && err != nil {
				t.Fatalf("write refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("write allowed")
			}
		})
	}
}

// TestClassOfNativeGroupInDeclaredNamespace covers C-32's render rule: an
// application writes its native downstream objects whatever group they belong
// to, but only inside the namespace it declared.
func TestClassOfNativeGroupInDeclaredNamespace(t *testing.T) {
	scope := Scope{Namespaces: map[string]bool{"apps": true}}
	inside := Ref{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "apps", Name: "web"}
	if got := ClassOf(inside, scope); got != ClassDownstream {
		t.Fatalf("a Deployment in the declared namespace is %q, want %q", got, ClassDownstream)
	}
	outside := Ref{Group: "apps", Version: "v1", Kind: "Deployment", Namespace: "other", Name: "web"}
	if got := ClassOf(outside, scope); got != ClassForeign {
		t.Fatalf("a Deployment outside the declared namespace is %q, want %q", got, ClassForeign)
	}
}

// TestKOSNamespaceRecordAllocation covers C-04's KOS-namespace row: the
// application reconciler creates only abandonment manifests, and no other
// KOS-namespace object, through the label that names the record.
func TestKOSNamespaceRecordAllocation(t *testing.T) {
	record := func(kind string, labels map[string]string) Request {
		return Request{
			Subject: SubjectApplicationReconciler,
			Object:  Ref{Version: "v1", Kind: kind, Namespace: api.SystemNamespace, Name: "record"},
			Verb:    VerbCreate,
			Parts:   []Part{PartObject},
			Labels:  labels,
			Markers: creatorMarkers(""),
		}
	}
	if err := Admit(record("ConfigMap", map[string]string{AbandonmentLabel: "uid-1"})); err != nil {
		t.Fatalf("the abandonment manifest was refused: %v", err)
	}
	if err := Admit(record("ConfigMap", nil)); err == nil {
		t.Fatal("the reconciler created a KOS-namespace object without the abandonment label")
	}
	if err := Admit(record("Secret", map[string]string{AbandonmentLabel: "uid-1"})); err == nil {
		t.Fatal("the reconciler created a non-ConfigMap KOS-namespace object")
	}
}

// TestPrivilegedGroupsStayForeign covers C-04: a declared namespace admits a
// render's workloads, not objects that grant or extend rights.
func TestPrivilegedGroupsStayForeign(t *testing.T) {
	for _, object := range []Ref{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding", Namespace: "apps", Name: "grant"},
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition", Namespace: "apps", Name: "class"},
	} {
		err := Admit(Request{
			Subject: SubjectApplicationReconciler,
			Object:  object,
			Verb:    VerbCreate,
			Parts:   []Part{PartObject},
			Markers: creatorMarkers("uid-1"),
			Scope:   namespaces("apps"),
		})
		if err == nil {
			t.Errorf("%s was admitted as a downstream object", object.String())
		}
	}
}

// TestApplicationReconcilerDeletesOnlyActionRecords covers C-04's delete row:
// the reconciler removes the records it consumed, not the rest of the KOS
// namespace and never the immutable abandonment manifest.
func TestApplicationReconcilerDeletesOnlyActionRecords(t *testing.T) {
	recordRef := Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: ActionRecordPrefix + "uid-1-orphan"}
	installerRef := Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "installer"}
	manifestRef := Ref{Version: "v1", Kind: "ConfigMap", Namespace: api.SystemNamespace, Name: "kos-app-abandonment-uid-1"}
	cases := []struct {
		name    string
		ref     Ref
		current *unstructured.Unstructured
		want    bool
	}{
		{
			name: "consumed action record",
			ref:  recordRef,
			current: nativeStored("ConfigMap", api.SystemNamespace, recordRef.Name,
				map[string]string{markers.ManagedBy: ManagedByValue, ActionRecordLabel: "orphan"}),
			want: true,
		},
		{
			name:    "installer object",
			ref:     installerRef,
			current: nativeStored("ConfigMap", api.SystemNamespace, installerRef.Name, map[string]string{markers.ManagedBy: ManagedByValue}),
		},
		{
			name: "abandonment manifest",
			ref:  manifestRef,
			current: nativeStored("ConfigMap", api.SystemNamespace, manifestRef.Name,
				map[string]string{markers.ManagedBy: ManagedByValue, AbandonmentLabel: "uid-1"}),
		},
		{
			name: "already gone",
			ref:  recordRef,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := Admit(Request{
				Subject: SubjectApplicationReconciler,
				Object:  test.ref,
				Verb:    VerbDelete,
				Parts:   []Part{PartObject},
				Current: test.current,
			})
			if test.name == "already gone" {
				// A missing object passes admission; the delete itself reports NotFound.
				if err != nil {
					t.Fatalf("a missing record was refused: %v", err)
				}
				return
			}
			if test.want && err != nil {
				t.Fatalf("delete refused: %v", err)
			}
			if !test.want && err == nil {
				t.Fatal("delete allowed")
			}
		})
	}
}
