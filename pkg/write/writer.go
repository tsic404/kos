package write

import (
	"context"
	"fmt"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/markers"
)

// fieldManagerLimit is the longest field manager the API server accepts.
const fieldManagerLimit = 128

// ManagedByValue is what kos-shell writes in managed-by. C-02 fixes the key and
// its carrier, not a vocabulary for the value.
const ManagedByValue = "kos-shell"

// AbandonmentLabel marks the immutable abandonment manifest the application
// reconciler creates in the KOS namespace (C-04, C-33). It is a plain label, not
// a C-02 marker: it tells the write allocation which KOS-namespace record a
// creation writes.
const AbandonmentLabel = "kos.tsic.top/abandonment"

// ActionRecordLabel and ActionRecordPrefix identify the one-shot action records
// the control plane creates and the application reconciler consumes (C-04,
// C-33). The allocation uses them to scope the reconciler's KOS-namespace
// deletions to those records.
const (
	ActionRecordLabel  = "kos.tsic.top/action"
	ActionRecordPrefix = "kos-action-"
)

// nativeResources names the native kinds the allocation tables talk about. Any
// other native kind must come from discovery rather than a guess.
var nativeResources = map[schema.GroupKind]string{
	{Group: "", Kind: "ConfigMap"}:                "configmaps",
	{Group: "", Kind: "PersistentVolumeClaim"}:    "persistentvolumeclaims",
	{Group: "", Kind: "Node"}:                     "nodes",
	{Group: "", Kind: "Service"}:                  "services",
	{Group: "networking.k8s.io", Kind: "Ingress"}: "ingresses",
}

// Writer executes admitted writes against the API server.
type Writer struct {
	// Dynamic reads and writes every class the tables allocate.
	Dynamic dynamic.Interface
	// Resources maps a kind to the resource name discovery reported.
	Resources map[schema.GroupKind]string
	// Scope is what a controller subject is declared to write (C-20, C-32).
	Scope Scope
}

// Create brings a new object into existence for subject. A user's creation
// records the session as spec.owner (C-60).
func (w *Writer) Create(ctx context.Context, subject Subject, session Session, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return w.create(ctx, subject, session, "", object)
}

// CreateClaim creates a claim in the C-04 row the caller declares it to belong
// to: a bound claim of a volume or a migration target (C-04, C-40).
func (w *Writer) CreateClaim(ctx context.Context, subject Subject, session Session, claim Claim, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return w.create(ctx, subject, session, claim, object)
}

// create runs one admitted creation.
func (w *Writer) create(ctx context.Context, subject Subject, session Session, claim Claim, object *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ref := RefOf(object)
	object = writeCreationAttribution(object, ClassOf(ref, w.Scope))
	fields := fieldsOf(object)
	if subject == SubjectUser && ownedKinds[ref.Kind] {
		if err := assignOwner(object, session); err != nil {
			return nil, &Denied{Subject: subject, Object: ref, Reason: err.Error()}
		}
		fields = append(fields, ownerField)
	}
	request := Request{
		Subject: subject,
		Session: session,
		Object:  ref,
		Verb:    VerbCreate,
		Parts:   partsOf(object),
		Fields:  fields,
		Markers: markerWrites(object),
		Labels:  object.GetLabels(),
		Scope:   w.Scope,
		Claim:   claim,
	}
	if err := Admit(request); err != nil {
		return nil, err
	}
	resource, err := w.resource(ref)
	if err != nil {
		return nil, err
	}
	created, err := resource.Create(ctx, object, metav1.CreateOptions{FieldManager: manager(subject, session)})
	if err != nil {
		return nil, w.mapError(ctx, ref, err)
	}
	return created, nil
}

// Apply server-side applies one part of an object and returns what the server
// stored. A field set another writer owns comes back as *Conflict carrying the
// current object (C-05).
func (w *Writer) Apply(ctx context.Context, subject Subject, session Session, part Part, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return w.apply(ctx, subject, session, "", part, document, false)
}

// ApplyClaim writes a claim in the C-04 row the caller declares it to belong
// to. Claims carry no kernel parts, so the whole object is the written part.
// The apply is forced: the declared subject is the sole writer of that claim
// row (C-04), and the claim it created carries the same manager under the
// create operation, which a plain apply reports as a conflict.
func (w *Writer) ApplyClaim(ctx context.Context, subject Subject, session Session, claim Claim, document *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return w.apply(ctx, subject, session, claim, PartObject, document, true)
}

// apply runs one admitted server-side apply.
func (w *Writer) apply(ctx context.Context, subject Subject, session Session, claim Claim, part Part, document *unstructured.Unstructured, force bool) (*unstructured.Unstructured, error) {
	ref := RefOf(document)
	current, err := w.current(ctx, ref)
	if err != nil {
		return nil, err
	}
	class := ClassOf(ref, w.Scope)
	if current == nil {
		document = writeCreationAttribution(document, class)
	}
	markers := markerWrites(document)
	parts := []Part{part}
	if len(markers) > 0 {
		parts = append(parts, PartMarker)
	}
	request := Request{
		Subject: subject,
		Session: session,
		Object:  ref,
		Verb:    VerbApply,
		Parts:   parts,
		Fields:  fieldsOf(document),
		Markers: markers,
		Labels:  document.GetLabels(),
		Current: current,
		Scope:   w.Scope,
		Claim:   claim,
	}
	if err := Admit(request); err != nil {
		return nil, err
	}
	if attributedClass(class) {
		document = keepsAttribution(document, subject, session, current)
	}
	if part == PartSpec {
		switch {
		case transfersOwner(subject, session, request):
			return w.transferOwner(ctx, subject, session, document, ref)
		case current == nil && subject == SubjectUser && ownedKinds[ref.Kind]:
			// A merged apply can create too; the owner is still the API's
			// to write from the session (C-60).
			document = document.DeepCopy()
			if err := assignOwner(document, session); err != nil {
				return nil, &Denied{Subject: subject, Object: ref, Reason: err.Error()}
			}
		default:
			document = keepsOwner(document, session, current)
		}
	}
	return w.applyDocument(ctx, ref, part, document, force, manager(subject, session))
}

// applyDocument runs one server-side apply and turns a field-set conflict into
// the object-carrying conflict C-05 requires.
func (w *Writer) applyDocument(ctx context.Context, ref Ref, part Part, document *unstructured.Unstructured, force bool, fieldManager string) (*unstructured.Unstructured, error) {
	resource, err := w.resource(ref)
	if err != nil {
		return nil, err
	}
	options := metav1.ApplyOptions{FieldManager: fieldManager, Force: force}
	var stored *unstructured.Unstructured
	if part == PartStatus {
		stored, err = resource.ApplyStatus(ctx, ref.Name, document, options)
	} else {
		stored, err = resource.Apply(ctx, ref.Name, document, options)
	}
	if err != nil {
		return nil, w.mapError(ctx, ref, err)
	}
	return stored, nil
}

// transferOwner writes C-60's owner change as a document of its own and writes
// the rest of the document first as an ordinary merged write, so the other
// fields still lose to their writer with a 409 instead of being silently taken
// over (C-05). Only spec.owner is forced, and it is written by the owner
// manager, which owns no other field.
func (w *Writer) transferOwner(ctx context.Context, subject Subject, session Session, document *unstructured.Unstructured, ref Ref) (*unstructured.Unstructured, error) {
	owner, _, err := unstructured.NestedString(document.Object, "spec", "owner")
	if err != nil {
		return nil, err
	}
	rest := document.DeepCopy()
	spec, _, err := unstructured.NestedMap(rest.Object, "spec")
	if err != nil {
		return nil, err
	}
	delete(spec, "owner")
	if len(spec) == 0 {
		unstructured.RemoveNestedField(rest.Object, "spec")
	} else if err := unstructured.SetNestedMap(rest.Object, spec, "spec"); err != nil {
		return nil, err
	}
	if carriesWrite(rest) {
		if _, err := w.applyDocument(ctx, ref, PartSpec, rest, false, manager(subject, session)); err != nil {
			return nil, err
		}
	}
	metadata := map[string]any{"name": ref.Name}
	if ref.Namespace != "" {
		metadata["namespace"] = ref.Namespace
	}
	ownerDocument := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": document.GetAPIVersion(),
		"kind":       document.GetKind(),
		"metadata":   metadata,
		"spec":       map[string]any{"owner": owner},
	}}
	return w.applyDocument(ctx, ref, PartSpec, ownerDocument, true, ownerManager)
}

// SetFinalizers writes the finalizer list of one object as the reconciler that
// also writes its status. Finalizers are the uninstall gate of C-33/C-54: the
// object stays until its own reconciler says the work is done.
func (w *Writer) SetFinalizers(ctx context.Context, subject Subject, ref Ref, finalizers []string) (*unstructured.Unstructured, error) {
	current, err := w.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	request := Request{
		Subject: subject,
		Object:  ref,
		Verb:    VerbApply,
		Parts:   []Part{PartMeta},
		Current: current,
		Scope:   w.Scope,
	}
	if err := Admit(request); err != nil {
		return nil, err
	}
	updated := current.DeepCopy()
	updated.SetFinalizers(finalizers)
	resource, err := w.resource(ref)
	if err != nil {
		return nil, err
	}
	stored, err := resource.Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return nil, w.mapError(ctx, ref, err)
	}
	return stored, nil
}

// Markers sets and removes marker keys on an existing object, reading it
// first. A change by another writer becomes a *Conflict.
func (w *Writer) Markers(ctx context.Context, subject Subject, session Session, ref Ref, set map[string]string, remove []string) (*unstructured.Unstructured, error) {
	current, err := w.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	return w.MarkOn(ctx, subject, session, current, set, remove)
}

// MarkOn sets and removes marker keys on exactly the object the caller read,
// so a change in between becomes a *Conflict instead of a wrong update.
func (w *Writer) MarkOn(ctx context.Context, subject Subject, session Session, current *unstructured.Unstructured, set map[string]string, remove []string) (*unstructured.Unstructured, error) {
	ref := RefOf(current)
	writes, err := markerUpdates(subject, ref, set, remove)
	if err != nil {
		return nil, err
	}
	request := Request{
		Subject: subject,
		Session: session,
		Object:  ref,
		Verb:    VerbApply,
		Parts:   []Part{PartMarker},
		Markers: writes,
		Current: current,
		Scope:   w.Scope,
	}
	if err := Admit(request); err != nil {
		return nil, err
	}
	updated := current.DeepCopy()
	for key, value := range set {
		carrier, _ := markers.CarrierOf(key)
		writeMarker(updated, key, carrier, value)
	}
	for _, key := range remove {
		carrier, _ := markers.CarrierOf(key)
		writeMarker(updated, key, carrier, "")
	}
	resource, err := w.resource(ref)
	if err != nil {
		return nil, err
	}
	stored, err := resource.Update(ctx, updated, metav1.UpdateOptions{FieldManager: manager(subject, session)})
	if err != nil {
		return nil, w.mapError(ctx, ref, err)
	}
	return stored, nil
}

// Delete removes an object for subject.
func (w *Writer) Delete(ctx context.Context, subject Subject, session Session, ref Ref) error {
	return w.delete(ctx, subject, session, "", ref)
}

// DeleteClaim removes a claim in the C-04 row the caller declares it to belong
// to, so the migration flow cannot delete a volume's bound claim (C-04, F6).
func (w *Writer) DeleteClaim(ctx context.Context, subject Subject, session Session, claim Claim, ref Ref) error {
	return w.delete(ctx, subject, session, claim, ref)
}

// delete runs one admitted deletion. The stored object is read first: a
// delete is a write too, so ownership decides who may remove what (C-59, C-60).
func (w *Writer) delete(ctx context.Context, subject Subject, session Session, claim Claim, ref Ref) error {
	current, err := w.current(ctx, ref)
	if err != nil {
		return err
	}
	request := Request{
		Subject: subject,
		Session: session,
		Object:  ref,
		Verb:    VerbDelete,
		Parts:   []Part{PartObject},
		Current: current,
		Scope:   w.Scope,
		Claim:   claim,
	}
	if err := Admit(request); err != nil {
		return err
	}
	if current == nil {
		return apierrors.NewNotFound(schema.GroupResource{Group: ref.Group, Resource: ref.Kind}, ref.Name)
	}
	resource, err := w.resource(ref)
	if err != nil {
		return err
	}
	uid := current.GetUID()
	return w.mapError(ctx, ref, resource.Delete(ctx, ref.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}))
}

// Get reads an object through the served version named by ref.
func (w *Writer) Get(ctx context.Context, ref Ref) (*unstructured.Unstructured, error) {
	resource, err := w.resource(ref)
	if err != nil {
		return nil, err
	}
	return resource.Get(ctx, ref.Name, metav1.GetOptions{})
}

// current reads the stored object; a missing one is not an error because a
// create is a write too.
func (w *Writer) current(ctx context.Context, ref Ref) (*unstructured.Unstructured, error) {
	stored, err := w.Get(ctx, ref)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return stored, err
}

// resource resolves the client for a reference.
func (w *Writer) resource(ref Ref) (dynamic.ResourceInterface, error) {
	plural, known := w.Resources[ref.GroupKind()]
	if !known {
		plural, known = nativeResources[ref.GroupKind()]
	}
	if !known {
		return nil, &UnknownResource{Ref: ref}
	}
	resource := w.Dynamic.Resource(schema.GroupVersionResource{
		Group:    ref.Group,
		Version:  ref.Version,
		Resource: plural,
	})
	if ref.Namespace == "" {
		return resource, nil
	}
	return resource.Namespace(ref.Namespace), nil
}

// UnknownResource reports a kind or version the write path cannot map to a
// cluster resource. Repeating the write cannot make it succeed (C-33).
type UnknownResource struct {
	// Ref is the object the write addressed.
	Ref Ref
}

// Error names the object the write path could not resolve.
func (e *UnknownResource) Error() string {
	return fmt.Sprintf("no resource known for %s", e.Ref)
}

// mapError turns a server conflict into the object-carrying conflict C-05
// requires; every other error passes through.
func (w *Writer) mapError(ctx context.Context, ref Ref, err error) error {
	if err == nil || !apierrors.IsConflict(err) {
		return err
	}
	current, getErr := w.Get(ctx, ref)
	if getErr != nil {
		current = nil
	}
	return &Conflict{Object: ref, Current: current, Err: err}
}

// assignOwner writes C-60's spec.owner at creation. A different submitted owner
// is refused rather than overwritten.
func assignOwner(object *unstructured.Unstructured, session Session) error {
	owner, _, err := unstructured.NestedString(object.Object, "spec", "owner")
	if err != nil {
		return fmt.Errorf("%s is unreadable: %w", ownerField, err)
	}
	if owner != "" && owner != session.Username {
		return fmt.Errorf("%s is written by the API from the session", ownerField)
	}
	return unstructured.SetNestedField(object.Object, session.Username, "spec", "owner")
}

// ownerManager is the field manager an owner transfer writes under. It owns
// spec.owner and nothing else, so a transfer's owner-only apply cannot drop a
// field some other manager owns.
const ownerManager = "kos-owner"

// transfersOwner reports the one write C-60 lets the global Owner force: a
// change of spec.owner that another manager currently owns.
func transfersOwner(subject Subject, session Session, req Request) bool {
	return subject == SubjectUser && session.Role == RoleOwner && contains(req.Fields, ownerField)
}

// keepsOwner returns the document a user's merged write needs so that its own
// manager does not delete the owner it owns: a server-side apply drops the
// fields its manager owns and the document omits, and C-60 makes the owner
// immutable after creation. Only the manager that owns spec.owner refills it;
// another writer's document carries no owner and must not claim one.
func keepsOwner(document *unstructured.Unstructured, session Session, current *unstructured.Unstructured) *unstructured.Unstructured {
	if current == nil || session.Username == "" {
		return document
	}
	if !ownsField(current, manager(SubjectUser, session), "spec", "owner") {
		return document
	}
	owner, found, err := unstructured.NestedString(current.Object, "spec", "owner")
	if err != nil || !found || owner != session.Username {
		return document
	}
	if _, present, err := unstructured.NestedFieldNoCopy(document.Object, "spec", "owner"); err != nil || present {
		return document
	}
	kept := document.DeepCopy()
	if err := unstructured.SetNestedField(kept.Object, owner, "spec", "owner"); err != nil {
		return document
	}
	return kept
}

// ownsField reports whether fieldManager owns a field path on the stored
// object. Ownership is what decides whether an apply that omits the field
// removes it.
func ownsField(current *unstructured.Unstructured, fieldManager string, path ...string) bool {
	entries, found, err := unstructured.NestedSlice(current.Object, "metadata", "managedFields")
	if err != nil || !found {
		return false
	}
	for _, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok || entry["manager"] != fieldManager {
			continue
		}
		if _, isSubresource := entry["subresource"]; isSubresource {
			continue
		}
		tree, ok := entry["fieldsV1"].(map[string]any)
		if !ok {
			continue
		}
		for _, field := range path {
			tree, ok = tree["f:"+field].(map[string]any)
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// carriesWrite reports whether a document holds data beyond the object's
// identity, so an owner-only transfer does not send an empty merged write.
func carriesWrite(object *unstructured.Unstructured) bool {
	if len(object.GetLabels()) > 0 || len(object.GetAnnotations()) > 0 {
		return true
	}
	for _, field := range []string{"spec", "status"} {
		value, found, err := unstructured.NestedFieldNoCopy(object.Object, field)
		if err != nil || !found {
			continue
		}
		if content, ok := value.(map[string]any); !ok || len(content) > 0 {
			return true
		}
	}
	return false
}

// partsOf lists the kernel parts a document writes.
func partsOf(object *unstructured.Unstructured) []Part {
	parts := make([]Part, 0, 3)
	if _, hasSpec := object.Object["spec"]; hasSpec {
		parts = append(parts, PartSpec)
	}
	if _, hasStatus := object.Object["status"]; hasStatus {
		parts = append(parts, PartStatus)
	}
	if len(markerWrites(object)) > 0 {
		parts = append(parts, PartMarker)
	}
	return parts
}

// fieldsOf lists the field paths a document sets that the tables reason about.
func fieldsOf(object *unstructured.Unstructured) []string {
	var fields []string
	if _, found, _ := unstructured.NestedFieldNoCopy(object.Object, "spec", "owner"); found {
		fields = append(fields, ownerField)
	}
	return fields
}

// markerWrites reads every marker key a document carries together with the
// carrier it actually sits on, so a misplaced key is refused, not ignored.
func markerWrites(object *unstructured.Unstructured) []MarkerWrite {
	writes := make([]MarkerWrite, 0, len(object.GetLabels())+len(object.GetAnnotations()))
	for key, value := range object.GetLabels() {
		if _, isMarker := markers.CarrierOf(key); isMarker {
			writes = append(writes, MarkerWrite{Key: key, Carrier: markers.CarrierLabel, Value: value, Present: true})
		}
	}
	for key, value := range object.GetAnnotations() {
		if _, isMarker := markers.CarrierOf(key); isMarker {
			writes = append(writes, MarkerWrite{Key: key, Carrier: markers.CarrierAnnotation, Value: value, Present: true})
		}
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].Key < writes[j].Key })
	return writes
}

// writeCreationAttribution adds the managed-by marker a creator writes at
// creation (C-02) for the classes where the unmanaged rule applies. Without it
// the object just created would be unmanaged to its creator's next write. The
// owner UID is the caller's: admission refuses a creation of an owned class
// that omits it, so it is never derived here.
func writeCreationAttribution(object *unstructured.Unstructured, class Class) *unstructured.Unstructured {
	if !attributedClass(class) {
		return object
	}
	if _, managed := object.GetLabels()[markers.ManagedBy]; managed {
		return object
	}
	stamped := object.DeepCopy()
	writeMarker(stamped, markers.ManagedBy, markers.CarrierLabel, ManagedByValue)
	return stamped
}

// attributedClass reports whether the class's objects carry KOS attribution, so
// a creator writes managed-by and its later renders must keep it (C-02).
func attributedClass(class Class) bool {
	switch class {
	case ClassSystem, ClassDownstream, ClassPVC, ClassExtension:
		return true
	}
	return false
}

// keepsAttribution refills the attribution keys a writer owns and the document
// omits: a server-side apply drops the fields its manager owns and the document
// leaves out, while C-02 writes attribution once at creation. The owner UID is
// the caller's value, required at creation and kept rather than derived here.
func keepsAttribution(document *unstructured.Unstructured, subject Subject, session Session, current *unstructured.Unstructured) *unstructured.Unstructured {
	if current == nil {
		return document
	}
	fields := []string{markers.ManagedBy, markers.OwnerUID}
	kept := document.DeepCopy()
	changed := false
	for _, key := range fields {
		if _, present := document.GetLabels()[key]; present {
			continue
		}
		value := current.GetLabels()[key]
		if value == "" || !ownsField(current, manager(subject, session), "metadata", "labels", key) {
			continue
		}
		writeMarker(kept, key, markers.CarrierLabel, value)
		changed = true
	}
	if !changed {
		return document
	}
	return kept
}

// markerUpdates turns the keys a caller sets and removes into admitted writes,
// in key order so a refusal is reproducible.
func markerUpdates(subject Subject, ref Ref, set map[string]string, remove []string) ([]MarkerWrite, error) {
	writes := make([]MarkerWrite, 0, len(set)+len(remove))
	for key, value := range set {
		carrier, isMarker := markers.CarrierOf(key)
		if !isMarker {
			return nil, &Denied{Subject: subject, Object: ref, Reason: fmt.Sprintf("unknown marker key %q", key)}
		}
		writes = append(writes, MarkerWrite{Key: key, Carrier: carrier, Value: value, Present: true})
	}
	for _, key := range remove {
		carrier, isMarker := markers.CarrierOf(key)
		if !isMarker {
			return nil, &Denied{Subject: subject, Object: ref, Reason: fmt.Sprintf("unknown marker key %q", key)}
		}
		writes = append(writes, MarkerWrite{Key: key, Carrier: carrier, Present: false})
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].Key < writes[j].Key })
	return writes, nil
}

// writeMarker sets or, for an empty value, removes a marker on its carrier.
func writeMarker(object *unstructured.Unstructured, key string, carrier markers.Carrier, value string) {
	if carrier == markers.CarrierAnnotation {
		annotations := object.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		if value == "" {
			delete(annotations, key)
		} else {
			annotations[key] = value
		}
		object.SetAnnotations(annotations)
		return
	}
	labels := object.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	if value == "" {
		delete(labels, key)
	} else {
		labels[key] = value
	}
	object.SetLabels(labels)
}

// manager is the field manager a subject writes under. Two subjects never share
// one, which is what turns a shared field set into a 409 instead of a silent
// overwrite (C-05).
func manager(subject Subject, session Session) string {
	name := "kos-" + string(subject)
	if subject == SubjectUser {
		name = "kos-user-" + session.Username
	}
	if len(name) > fieldManagerLimit {
		name = name[:fieldManagerLimit]
	}
	return name
}
