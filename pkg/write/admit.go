package write

import (
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/markers"
)

// ownerField is the field C-60 lets only the global Owner transfer.
const ownerField = "spec.owner"

// Admit decides one write. A nil error means it may proceed; a *Denied names
// the subject, the object and the rule that refused it.
func Admit(req Request) error {
	if !knownSubjects[req.Subject] {
		return denyf(req, "subject is not in the allocation tables")
	}
	if req.Subject == SubjectUser && !req.Session.IsSignedIn() {
		return denyf(req, "a user write needs a session with a role")
	}

	class := ClassOf(req.Object, req.Scope)
	var err error
	switch class {
	case ClassKernel:
		err = admitKernel(req)
	case ClassExtension:
		err = admitExtension(req)
	case ClassSystem:
		err = admitClass(req, systemWriters, "KOS namespace objects", true)
	case ClassPVC:
		err = admitClaim(req)
	case ClassNode:
		err = admitClass(req, nodeWriters, "run-kernel node objects", false)
	case ClassDownstream:
		err = admitDownstream(req)
	default:
		return denyf(req, "object is outside every allocation table")
	}
	if err != nil {
		return err
	}
	return admitOwnerMarker(req, class)
}

// admitKernel checks every part of a kernel object a write touches.
func admitKernel(req Request) error {
	if req.Verb == VerbDelete {
		return admitKernelDelete(req)
	}
	if len(req.Parts) == 0 {
		return denyf(req, "a kernel write names no part")
	}
	for _, part := range req.Parts {
		switch part {
		case PartSpec:
			if err := admitKernelSpec(req); err != nil {
				return err
			}
		case PartStatus:
			if err := admitKernelStatus(req); err != nil {
				return err
			}
		case PartMarker:
			if err := admitMarkers(req, false); err != nil {
				return err
			}
		default:
			return denyf(req, "kernel objects are written by part, not as a whole")
		}
	}
	return nil
}

// admitKernelDelete applies the kernel delete paths: a user deletes what it
// owns, the application reconciler deletes its cascade endpoints (C-45), and
// nobody else deletes kernel objects.
func admitKernelDelete(req Request) error {
	switch req.Subject {
	case SubjectUser:
		return admitUserSpec(req)
	case SubjectApplicationReconciler:
		if req.Object.Kind != "Endpoint" {
			return denyf(req, "the application reconciler deletes only cascade endpoints")
		}
		return admitAttributed(req)
	default:
		return denyf(req, "subject does not delete kernel objects")
	}
}

// admitKernelSpec applies the spec column of C-03: the user writes a user spec,
// one named subject writes a system spec, nobody else writes either.
func admitKernelSpec(req Request) error {
	if writer, isSystem := systemSpecWriters[req.Object.Kind]; isSystem {
		if req.Subject != writer {
			return denyf(req, "the system spec is written by %q", writer)
		}
		return admitAttributed(req)
	}
	if req.Subject != SubjectUser {
		return denyf(req, "the user spec is written by the user")
	}
	return admitUserSpec(req)
}

// admitKernelStatus applies the status column of C-03: one subject per kind.
func admitKernelStatus(req Request) error {
	writer, hasWriter := statusWriters[req.Object.Kind]
	if !hasWriter {
		return denyf(req, "the kind has no status writer")
	}
	if req.Subject != writer {
		return denyf(req, "status is written by %q", writer)
	}
	return admitAttributed(req)
}

// admitAttributed refuses a KOS subject's write to an object without an
// attribution marker: KOS does not write what it neither created nor adopted
// (C-02).
func admitAttributed(req Request) error {
	if req.Current == nil {
		return nil
	}
	attribution, err := markers.AttributionOf(req.Current.GetLabels())
	if err != nil {
		return denyf(req, "attribution is unreadable: %v", err)
	}
	if attribution == markers.AttributionUnmanaged {
		return denyf(req, "object carries no attribution marker")
	}
	return nil
}

// admitUserSpec applies C-59 and C-60 to a user's write: a guest reads only,
// the global Owner writes everything, and a member writes the five owned kinds,
// only when it owns the object. Ownerless objects and the kinds only the Owner
// manages are refused to members. A member may not write spec.owner either:
// that field is the global Owner's to transfer.
func admitUserSpec(req Request) error {
	if req.Session.Role == RoleGuest {
		return denyf(req, "guest is read-only")
	}
	if req.Session.Role == RoleOwner {
		return nil
	}
	if !ownedKinds[req.Object.Kind] {
		return denyf(req, "only the global Owner may write %s", req.Object.Kind)
	}
	if req.Current == nil {
		return nil
	}
	if contains(req.Fields, ownerField) {
		return denyf(req, "only the global Owner may transfer %s", ownerField)
	}
	owner, _, err := unstructured.NestedString(req.Current.Object, "spec", "owner")
	if err != nil {
		return denyf(req, "%s is unreadable: %v", ownerField, err)
	}
	if owner != req.Session.Username {
		return denyf(req, "the object is owned by %q", owner)
	}
	return nil
}

// admitClaim applies C-04 to claims. A claim write names the row it belongs to;
// a bound claim is the volume reconciler's, a migration target the migration
// flow's, and neither may write the other's claims. Release markers stay a
// marker-column write (C-03), so the migration flow can mark a source claim
// released without gaining the right to delete it.
func admitClaim(req Request) error {
	if len(req.Parts) == 1 && req.Parts[0] == PartMarker {
		if err := admitAttributed(req); err != nil {
			return err
		}
		return admitMarkers(req, false)
	}
	writers, roleKnown := claimWriters[req.Claim]
	if !roleKnown {
		return denyf(req, "a claim write names no claim role")
	}
	if !writers[req.Subject][req.Verb] {
		return denyf(req, "a %s claim has no %s writer of that subject", req.Claim, req.Verb)
	}
	if err := admitAttributed(req); err != nil {
		return err
	}
	return admitMarkers(req, creates(req))
}

// admitDownstream applies C-04 to native objects: the application reconciler
// writes inside the namespaces its application declared, an extension
// controller writes only the namespaces and kinds its declaration grants, the
// other named subjects write their own downstream kind, and the creator marks
// what it creates.
func admitDownstream(req Request) error {
	switch req.Subject {
	case SubjectInstaller:
		if !req.Scope.Namespaces[req.Object.Namespace] {
			return denyf(req, "namespace is not declared by the installer")
		}
	case SubjectApplicationReconciler:
		if !req.Scope.Namespaces[req.Object.Namespace] {
			return denyf(req, "namespace is not declared by the application")
		}
	case SubjectExtensionReconciler:
		if !req.Scope.Namespaces[req.Object.Namespace] {
			return denyf(req, "namespace is not declared by the extension")
		}
		if !req.Scope.Kinds[req.Object.GroupKind()] {
			return denyf(req, "kind is not declared by the extension")
		}
	}
	if !downstreamWriters[req.Subject][req.Verb] {
		return denyf(req, "downstream objects have no %s writer of that subject", req.Verb)
	}
	if err := admitAttributed(req); err != nil {
		return err
	}
	return admitMarkers(req, creates(req))
}

// admitExtension applies C-20 and C-19: one controller per declared group,
// nothing else, and extension objects follow the same attribution rule as
// kernel objects (C-02).
func admitExtension(req Request) error {
	if !extensionWriters[req.Subject] {
		return denyf(req, "extension objects are written by their extension controller")
	}
	if !req.Scope.Groups[req.Object.Group] {
		return denyf(req, "group is not declared by the controller")
	}
	if err := admitAttributed(req); err != nil {
		return err
	}
	return admitMarkers(req, creates(req))
}

// admitClass applies one C-04 verb table. attributed adds C-02's unmanaged
// rule for the classes KOS creates itself; run-kernel nodes are not KOS's and
// carry no attribution marker.
func admitClass(req Request, table map[Subject]map[Verb]bool, what string, attributed bool) error {
	if !table[req.Subject][req.Verb] {
		return denyf(req, "%s have no %s writer of that subject", what, req.Verb)
	}
	if attributed {
		if err := admitAttributed(req); err != nil {
			return err
		}
	}
	return admitMarkers(req, attributed && creates(req))
}

// creates reports whether req brings its object into existence, whether by an
// explicit create or by a merged apply that found nothing stored. A creator
// writes the attribution marker (C-02).
func creates(req Request) bool {
	return req.Verb != VerbDelete && (req.Verb == VerbCreate || req.Current == nil)
}

// ownedClass reports whether C-02 gives the class's objects an owner: a
// downstream object belongs to the KOS object that renders it, a claim to the
// volume or migration that created it, and a Provider to its SystemComponent.
// Every other managed object — action records and adoption snapshots,
// application definitions, device facts, extension objects — carries
// managed-by alone.
func ownedClass(class Class, ref Ref) bool {
	switch class {
	case ClassDownstream, ClassPVC:
		return true
	case ClassKernel:
		return ref.Kind == "Provider"
	}
	return false
}

// admitOwnerMarker applies C-02's existence rule for owner-uid: an object with
// an owner is created with the owner's UID, so a creation that omits it is
// refused instead of stored as a managed object no ownership filter can find.
// The value is the caller's; the write path can judge that it is there, not who
// it names.
func admitOwnerMarker(req Request, class Class) error {
	if !creates(req) || !ownedClass(class, req.Object) {
		return nil
	}
	for _, marker := range req.Markers {
		if marker.Key == markers.OwnerUID && marker.Present && marker.Value != "" {
			return nil
		}
	}
	return denyf(req, "the object has an owner but no %s", markers.OwnerUID)
}

// admitMarkers checks carrier, class and attribution stability of every marker
// key a write touches (C-02). creator allows the implicit attribution a
// creator writes on the object it creates.
func admitMarkers(req Request, creator bool) error {
	if req.Subject == SubjectUser && req.Session.Role == RoleGuest {
		return denyf(req, "guest is read-only")
	}
	if len(req.Markers) == 0 {
		if !hasPart(req.Parts, PartMarker) {
			return nil
		}
		return denyf(req, "a marker write names no key")
	}
	allowed := markerClasses[req.Subject]
	for _, marker := range req.Markers {
		carrier, isMarker := markers.CarrierOf(marker.Key)
		if !isMarker {
			return denyf(req, "unknown marker key %q", marker.Key)
		}
		if carrier != marker.Carrier {
			return denyf(req, "marker key %q belongs on %s, not %s", marker.Key, carrier, marker.Carrier)
		}
		class, _ := markers.ClassOf(marker.Key)
		if !allowed[class] && !(creator && class == markers.ClassAttribution) {
			return denyf(req, "subject may not write %s markers", class)
		}
		if class == markers.ClassAttribution {
			if err := admitAttributionChange(req, marker); err != nil {
				return err
			}
		}
	}
	return nil
}

// admitAttributionChange keeps the attribution stable: a managed object keeps
// its markers, an adopted object keeps them until a release removes them, and
// managed-by is only ever written by the creator.
func admitAttributionChange(req Request, marker MarkerWrite) error {
	if req.Current == nil {
		return nil
	}
	attribution, err := markers.AttributionOf(req.Current.GetLabels())
	if err != nil {
		return denyf(req, "attribution is unreadable: %v", err)
	}
	current := markerValue(req.Current, marker)
	switch attribution {
	case markers.AttributionManaged:
		if !marker.Present || marker.Value != current {
			return denyf(req, "attribution of a managed object is fixed at creation")
		}
	case markers.AttributionAdopted:
		if marker.Present && marker.Value != current {
			return denyf(req, "adoption markers are written once, at adoption")
		}
	case markers.AttributionUnmanaged:
		if marker.Present && (marker.Key == markers.ManagedBy || marker.Key == markers.OwnerUID) {
			return denyf(req, "%q is written by the creator at creation", marker.Key)
		}
	}
	return nil
}

// markerValue reads the current value of a marker key from its carrier.
func markerValue(object *unstructured.Unstructured, marker MarkerWrite) string {
	if marker.Carrier == markers.CarrierAnnotation {
		return object.GetAnnotations()[marker.Key]
	}
	return object.GetLabels()[marker.Key]
}

// denyf builds the refusal for req with a formatted reason.
func denyf(req Request, format string, args ...any) error {
	return &Denied{Subject: req.Subject, Object: req.Object, Reason: fmt.Sprintf(format, args...)}
}

// hasPart reports whether parts names part.
func hasPart(parts []Part, part Part) bool {
	for _, candidate := range parts {
		if candidate == part {
			return true
		}
	}
	return false
}

// contains reports whether paths holds path.
func contains(paths []string, path string) bool {
	for _, candidate := range paths {
		if candidate == path {
			return true
		}
	}
	return false
}
