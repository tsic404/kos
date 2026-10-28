// Package write is the KOS write path. It decides which subject may write
// which part of which object (C-03, C-04) and applies user writes as server
// side merges, so two writers of one field set cannot silently overwrite each
// other (C-05).
//
// Admission is a lookup over the contracts' writer allocations. A refusal
// names the subject and the object; a merge conflict carries the object the
// server currently holds.
package write

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
)

// Ref identifies an object without loading it.
type Ref struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
}

// RefOf reads the identity of an object.
func RefOf(object *unstructured.Unstructured) Ref {
	return Ref{
		Group:     object.GroupVersionKind().Group,
		Version:   object.GroupVersionKind().Version,
		Kind:      object.GetKind(),
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
	}
}

// GroupKind is the key under which a kind resolves to one resource.
func (r Ref) GroupKind() schema.GroupKind {
	return schema.GroupKind{Group: r.Group, Kind: r.Kind}
}

// Key is the serialization key of the object (C-05).
func (r Ref) Key() string {
	return strings.Join([]string{r.Group, r.Kind, r.Namespace, r.Name}, "/")
}

// String is the form errors use to name the object.
func (r Ref) String() string {
	name := r.Name
	if r.Namespace != "" {
		name = r.Namespace + "/" + name
	}
	group := r.Group
	if group == "" {
		group = "core"
	}
	return fmt.Sprintf("%s %s %s", group, r.Kind, name)
}

// Class is what the allocation tables call the kind of object being written.
type Class string

const (
	// ClassKernel is one of the ten kernel kinds (C-01).
	ClassKernel Class = "kernel"
	// ClassSystem is an object in the KOS namespace (C-04).
	ClassSystem Class = "system"
	// ClassDownstream is a native object below a KOS object (C-04).
	ClassDownstream Class = "downstream"
	// ClassPVC is a PersistentVolumeClaim (C-04, C-39).
	ClassPVC Class = "pvc"
	// ClassNode is a run-kernel node object (C-04, C-65).
	ClassNode Class = "node"
	// ClassExtension is an object of a group an extension declared (C-20).
	ClassExtension Class = "extension"
	// ClassForeign is anything the allocation tables do not name.
	ClassForeign Class = "foreign"
)

// ClassOf classifies a reference against the declared extension scope.
func ClassOf(ref Ref, scope Scope) Class {
	switch {
	case ref.Group == api.Group:
		return ClassKernel
	// Claims and nodes are classified by kind before the namespace rule: a
	// claim in the KOS namespace is still a claim (C-04).
	case ref.Group == "" && ref.Kind == "PersistentVolumeClaim":
		return ClassPVC
	case ref.Group == "" && ref.Kind == "Node":
		return ClassNode
	case ref.Namespace == api.SystemNamespace:
		return ClassSystem
	// The route objects C-43 maps the endpoint shapes to are native, not an
	// extension class, even though one of them lives outside the core group.
	case routeKinds[ref.GroupKind()]:
		return ClassDownstream
	case scope.Groups[ref.Group]:
		return ClassExtension
	case privilegedGroups[ref.Group] && !scope.Kinds[ref.GroupKind()]:
		// A declared namespace admits a render's workloads, not objects that
		// grant or extend rights: a RoleBinding to a cluster role must not ride
		// in as a downstream object (C-04). A subject that declared the kind in
		// its own scope is judged by that declaration instead.
		return ClassForeign
	case ref.Namespace != "" && scope.Namespaces[ref.Namespace]:
		// A declared namespace admits the render's native objects whatever
		// their group: apps/v1 and batch/v1 objects are downstream exactly like
		// core ones (C-04, C-32).
		return ClassDownstream
	case ref.Group == "":
		return ClassDownstream
	default:
		return ClassForeign
	}
}

// privilegedGroups are the API groups whose objects grant or extend rights over
// the cluster. A declared namespace alone does not admit them (C-04).
var privilegedGroups = map[string]bool{
	"rbac.authorization.k8s.io":    true,
	"apiextensions.k8s.io":         true,
	"admissionregistration.k8s.io": true,
	"apiregistration.k8s.io":       true,
	"certificates.k8s.io":          true,
	"flowcontrol.apiserver.k8s.io": true,
	"node.k8s.io":                  true,
	"policy":                       true,
	"scheduling.k8s.io":            true,
}

// Scope is what a controller is declared to write: the extension groups open to
// it (C-20) and the namespaces and kinds an application or extension renders
// into (C-04, C-18, C-32).
type Scope struct {
	Groups     map[string]bool
	Namespaces map[string]bool
	Kinds      map[schema.GroupKind]bool
}

// Claim is the C-04 row a claim write belongs to: a bound claim of a volume, or
// a migration target the migration flow creates. A claim write names its role;
// without one it is refused, so a subject never inherits the other row's verbs.
type Claim string

const (
	// ClaimBound is a volume's claim: created, expanded and deleted by the
	// volume reconciler alone (C-04, C-39).
	ClaimBound Claim = "bound"
	// ClaimMigrationTarget is a claim the migration flow creates for a move;
	// it belongs to no volume until cutover (C-04, C-40).
	ClaimMigrationTarget Claim = "migration-target"
)

// Part is the part of an object a write touches. The kernel tables allocate
// writers per part; other classes allocate the object as a whole.
type Part string

const (
	// PartSpec is the object's spec.
	PartSpec Part = "spec"
	// PartStatus is the object's status.
	PartStatus Part = "status"
	// PartMarker is one or more attribution, decline, release or validity keys.
	PartMarker Part = "marker"
	// PartMeta is the object's own metadata outside spec, status and markers:
	// the finalizers its reconciler sets and releases (C-33, C-54).
	PartMeta Part = "meta"
	// PartObject is the whole object of a non-kernel class.
	PartObject Part = "object"
)

// Verb is what a write does to its target.
type Verb string

const (
	// VerbCreate brings a new object into existence.
	VerbCreate Verb = "create"
	// VerbApply writes fields of an existing object.
	VerbApply Verb = "apply"
	// VerbDelete removes an object.
	VerbDelete Verb = "delete"
)

// MarkerWrite is one marker key a write sets or removes.
type MarkerWrite struct {
	Key     string
	Carrier markers.Carrier
	// Value is the value being written; empty when the write removes the key.
	Value   string
	Present bool
}

// Request is one write to admit.
type Request struct {
	Subject Subject
	Session Session
	Object  Ref
	Verb    Verb
	Parts   []Part
	// Fields lists the field paths the write touches, such as spec.owner.
	Fields  []string
	Markers []MarkerWrite
	// Current is the stored object, nil when the write creates it.
	Current *unstructured.Unstructured
	// Labels is the label set the submitted document carries. Record rows are
	// recognized by label, so the allocation can name one record kind without
	// granting a subject every object of its class (C-04).
	Labels map[string]string
	Scope  Scope
	// Claim is the claim row a claim write belongs to (C-04).
	Claim Claim
	// Force takes over the fields another writer holds. It is a field-set
	// takeover, so the allocation grants it per subject rather than per verb
	// (C-05): only a subject that is the writer of the object may force it.
	Force bool
}
