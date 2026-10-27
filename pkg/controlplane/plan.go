package controlplane

import (
	"context"
	"log/slog"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/volume"
)

// Warning codes are stable like error codes: a client can branch on them.
const (
	// WarningUnattributed marks a desired object without an ownership marker;
	// the write still happens, but nothing claims the object (C-02).
	WarningUnattributed = "unattributed"
	// WarningRetained marks an object the plan keeps because deleting it would
	// release data (C-39, F5).
	WarningRetained = "retained"
	// WarningReclaimDelete marks a volume whose storage class reclaims the
	// backend data when its claim is deleted (C-39).
	WarningReclaimDelete = "reclaim-delete"
	// WarningReclaimUnknown marks a volume whose reclaim policy cannot be
	// resolved; the warning is conservative because an unknown policy may
	// reclaim data (C-39).
	WarningReclaimUnknown = "reclaim-unknown"
	// WarningExpansionPreflight carries C-38's three expansion answers before a
	// capacity change is applied.
	WarningExpansionPreflight = "expansion-preflight"
)

// maxSetObjects bounds one plan or apply request.
const maxSetObjects = 200

// volumeKind is the kind whose deletion would release data (C-39).
const volumeKind = "Volume"

// storageClassType is the native class whose reclaim policy C-39 requires the
// plan to read; it resolves through the same source as the domain objects.
var storageClassType = ObjectType{
	Group:    "storage.k8s.io",
	Version:  "v1",
	Kind:     "StorageClass",
	Resource: "storageclasses",
}

// VolumeExpansion answers C-38's expansion preflight for one volume; a nil
// implementation leaves capacity plans without a preflight warning.
type VolumeExpansion interface {
	ExpansionPreflight(ctx context.Context, namespace, name string) (volume.ExpansionPlan, error)
}

// Change is one field a plan would rewrite (C-62). A null from or to means the
// field is absent on that side.
type Change struct {
	Path string `json:"path"`
	From any    `json:"from"`
	To   any    `json:"to"`
}

// Warning is a condition a plan reports without failing (C-62).
type Warning struct {
	Code    string     `json:"code"`
	Message string     `json:"message"`
	Object  *ObjectRef `json:"object,omitempty"`
}

// PlannedObject is one object a plan creates or deletes.
type PlannedObject struct {
	Ref ObjectRef `json:"ref"`
	// objectType is the class the object belongs to; a plan answer does not
	// carry it, the execution uses it.
	objectType ObjectType
}

// PlannedUpdate is one object a plan rewrites, with the fields that differ.
type PlannedUpdate struct {
	Ref     ObjectRef `json:"ref"`
	Changes []Change  `json:"changes"`
}

// Plan is the answer to a plan request: the three deviation classes, a rendered
// summary and warnings (C-62).
type Plan struct {
	Create   []PlannedObject `json:"create"`
	Update   []PlannedUpdate `json:"update"`
	Delete   []PlannedObject `json:"delete"`
	Summary  string          `json:"summary"`
	Warnings []Warning       `json:"warnings"`
}

// Scope bounds what a plan may delete. Only objects carrying this scope's
// ownership label are candidates, so a request never removes objects it does
// not own (C-02).
type Scope struct {
	OwnerUID string `json:"ownerUID"`
}

// SetRequest is the body of a plan or apply request: the desired object set and
// the scope its deletions are bounded to (C-62).
type SetRequest struct {
	Scope   *Scope           `json:"scope,omitempty"`
	Objects []map[string]any `json:"objects"`
}

// buildPlan computes the three deviation classes without writing anything
// (C-62). verb is the permission the desired objects are judged under; deletes
// are judged as deletes.
func (s *Server) buildPlan(ctx context.Context, verb Verb, request SetRequest) (*Plan, []desiredObject, *Error) {
	if len(request.Objects) == 0 {
		return nil, nil, invalidField("请求必须至少给出一个对象")
	}
	if len(request.Objects) > maxSetObjects {
		return nil, nil, invalidField("一次请求最多 " + strconv.Itoa(maxSetObjects) + " 个对象")
	}

	desired := make([]desiredObject, 0, len(request.Objects))
	seen := make(map[string]bool, len(request.Objects))
	for _, raw := range request.Objects {
		kind := text(raw["kind"])
		if kind == "" {
			return nil, nil, invalidField("每个对象都必须给出 kind")
		}
		objectType, failure := resolveType(kind, text(raw["apiVersion"]), s.extensions)
		if failure != nil {
			return nil, nil, withIdentity(failure,
				nestedText(raw, "metadata", "namespace"), nestedText(raw, "metadata", "name"))
		}
		object, failure := decodeObject(objectType, "", raw)
		if failure != nil {
			return nil, nil, failure
		}
		if failure := s.validateDesired(ctx, object); failure != nil {
			return nil, nil, failure
		}
		// Attribution is not stamped here: only an object this request creates
		// gets the creation marker, and an existing object keeps its own (C-02).
		if seen[object.Ref.String()] {
			return nil, nil, invalidField("请求重复给出了对象 " + object.Ref.String()).about(object.Ref)
		}
		seen[object.Ref.String()] = true
		if failure := s.authorize(ctx, verb, object.Ref); failure != nil {
			return nil, nil, failure
		}
		// An Application in a desired set meets the same C-32/F3 judgement as a
		// single-object write; the set path must not be a way around it (C-12).
		if failure := s.validateApplicationWrite(ctx, object.Object); failure != nil {
			return nil, nil, failure
		}
		desired = append(desired, object)
	}

	plan := &Plan{Create: []PlannedObject{}, Update: []PlannedUpdate{}, Delete: []PlannedObject{}, Warnings: []Warning{}}
	// C-39's reclaim judgement reads the class set once for the whole plan: the
	// set does not change between two creates in the same request.
	classes := newStorageClassIndex(s.source, s.log)
	for index := range desired {
		object := desired[index]
		current, err := s.source.Get(ctx, object.Type, object.Ref.Namespace, object.Ref.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				if !hasLabel(object.Object, markers.ManagedBy) {
					plan.Warnings = append(plan.Warnings, Warning{
						Code:    WarningUnattributed,
						Message: "对象 " + object.Ref.String() + " 的请求体没有归属标记，控制面创建时会补创建者标记",
						Object:  new(object.Ref),
					})
				}
				object = withCreationAttribution(object)
				desired[index] = object
				plan.Create = append(plan.Create, PlannedObject{Ref: object.Ref, objectType: object.Type})
				if warning := reclaimWarning(ctx, classes, object); warning != nil {
					plan.Warnings = append(plan.Warnings, *warning)
				}
				continue
			}
			s.log.Error("plan read failed", "object", object.Ref.String(), "error", err)
			return nil, nil, internal(StagePlan, "读取对象 "+object.Ref.String()+" 失败").about(object.Ref)
		}
		if failure := validateEndpointUpdate(current.Object, object); failure != nil {
			return nil, nil, failure
		}
		// An update keeps the attribution and decline markers the object already
		// carries, so a body that omits labels neither drops them nor grants new
		// ones (C-02).
		object, failure := withStoredAttribution(object, current.Object)
		if failure != nil {
			return nil, nil, failure
		}
		desired[index] = object
		if differences := changes(current.Object, object.Object.Object); len(differences) > 0 {
			// A plan is an outbound payload too: a stored sensitive value must not
			// come back through the diff (C-31).
			differences, failure := s.maskChanges(ctx, object, differences)
			if failure != nil {
				return nil, nil, failure
			}
			plan.Update = append(plan.Update, PlannedUpdate{Ref: object.Ref, Changes: differences})
			if warning := s.expansionWarning(ctx, object, current.Object); warning != nil {
				plan.Warnings = append(plan.Warnings, *warning)
			}
		}
	}

	retained, failure := s.planDeletes(ctx, request.Scope, seen, plan)
	if failure != nil {
		return nil, nil, failure
	}
	plan.sort()
	plan.render(retained)
	return plan, desired, nil
}

// planDeletes adds the objects the scope owns but the desired set dropped. A
// volume is never deleted this way: releasing data needs its own confirmation
// (C-39, F5).
func (s *Server) planDeletes(ctx context.Context, scope *Scope, desired map[string]bool, plan *Plan) (int, *Error) {
	if scope == nil || scope.OwnerUID == "" {
		return 0, nil
	}
	retained := 0
	for _, kind := range api.Kinds() {
		objectType, known := kernelType(kind)
		if !known {
			continue
		}
		observed, err := s.source.List(ctx, objectType, "")
		if err != nil {
			s.log.Error("plan read failed", "kind", kind, "error", err)
			return 0, internal(StagePlan, "读取 "+kind+" 失败").about(kernelRef(kind, "", ""))
		}
		for _, item := range observed {
			if text(labelOf(item.Object, markers.OwnerUID)) != scope.OwnerUID {
				continue
			}
			ref := refOf(item.Object, objectType)
			if desired[ref.String()] {
				continue
			}
			if kind == volumeKind {
				retained++
				plan.Warnings = append(plan.Warnings, Warning{
					Code:    WarningRetained,
					Message: "卷 " + ref.String() + " 保留数据，计划不删除它",
					Object:  new(ref),
				})
				continue
			}
			if failure := s.authorize(ctx, VerbDelete, ref); failure != nil {
				return 0, failure
			}
			plan.Delete = append(plan.Delete, PlannedObject{Ref: ref, objectType: objectType})
		}
	}
	return retained, nil
}

// storageClassIndex answers C-39's reclaim questions from one read of the
// class set per plan: however many volumes a request creates, the classes are
// listed once and both the named and the unnamed branch read that index.
type storageClassIndex struct {
	source ObjectSource
	log    *slog.Logger
	loaded bool
	err    error
	names  []string
	byName map[string]map[string]any
}

// newStorageClassIndex returns an index over a plan's source.
func newStorageClassIndex(source ObjectSource, log *slog.Logger) *storageClassIndex {
	return &storageClassIndex{source: source, log: log}
}

// read loads the class set on first use and remembers a failed read, so every
// volume it affects still gets the conservative warning without another list.
// The failed read is logged here, once per plan, because every later volume
// shares the same error.
func (i *storageClassIndex) read(ctx context.Context) error {
	if i.loaded {
		return i.err
	}
	i.loaded = true
	classes, err := i.source.List(ctx, storageClassType, "")
	if err != nil {
		i.err = err
		if i.log != nil {
			i.log.Warn("storage class read failed", "error", err)
		}
		return err
	}
	i.byName = make(map[string]map[string]any, len(classes))
	for _, class := range classes {
		name := nestedText(class.Object, "metadata", "name")
		i.names = append(i.names, name)
		i.byName[name] = class.Object
	}
	return nil
}

// defaultName is the class the cluster provisions an unnamed claim with, empty
// when no class is marked default; the first marked class wins, as before.
func (i *storageClassIndex) defaultName() string {
	for _, name := range i.names {
		if defaultStorageClass(i.byName[name]) {
			return name
		}
	}
	return ""
}

// reclaimWarning answers C-39 before a volume is created: the storage class it
// names reclaims the backend data when the claim is deleted, so the plan says
// so rather than leaving the user to discover it on deletion. An unnamed class
// resolves through the cluster default class, and a class set the plan cannot
// read warns conservatively (C-39).
func reclaimWarning(ctx context.Context, classes *storageClassIndex, object desiredObject) *Warning {
	if object.Ref.Kind != volumeKind {
		return nil
	}
	if err := classes.read(ctx); err != nil {
		return unknownReclaimWarning(object.Ref)
	}
	className := nestedText(object.Object.Object, "spec", "storageClass")
	unnamed := className == ""
	if unnamed {
		className = classes.defaultName()
	}
	class, found := classes.byName[className]
	if className == "" || !found {
		// No class will provision the claim, so there is no backend data to
		// reclaim.
		return nil
	}
	policy, _, _ := unstructured.NestedString(class, "reclaimPolicy")
	if !reclaimsOnDelete(policy) {
		return nil
	}
	ref := object.Ref
	if unnamed {
		return &Warning{
			Code:    WarningReclaimDelete,
			Message: "卷 " + ref.String() + " 的集群默认存储类 " + className + " 回收策略为 Delete，删除 PVC 会回收后端数据",
			Object:  &ref,
		}
	}
	return &Warning{
		Code:    WarningReclaimDelete,
		Message: "卷 " + ref.String() + " 的存储类 " + className + " 回收策略为 Delete，删除 PVC 会回收后端数据",
		Object:  &ref,
	}
}

// unknownReclaimWarning is the conservative answer: a policy the plan cannot
// resolve must not read as safe (C-39).
func unknownReclaimWarning(ref ObjectRef) *Warning {
	return &Warning{
		Code:    WarningReclaimUnknown,
		Message: "卷 " + ref.String() + " 的回收策略无法解析，删除 PVC 可能回收后端数据",
		Object:  &ref,
	}
}

// reclaimsOnDelete reports a policy that keeps its data: only Delete, or the
// empty value Kubernetes defaults dynamically provisioned volumes to, reclaims
// it (C-39).
func reclaimsOnDelete(policy string) bool {
	return policy == "" || policy == "Delete"
}

// defaultStorageClass reports the class a cluster provisions an unnamed claim
// with.
func defaultStorageClass(object map[string]any) bool {
	for _, key := range []string{
		"storageclass.kubernetes.io/is-default-class",
		"storageclass.beta.kubernetes.io/is-default-class",
	} {
		if nestedText(object, "metadata", "annotations", key) == "true" {
			return true
		}
	}
	return false
}

// expansionWarning answers C-38's three expansion questions before a plan
// applies a capacity increase. A nil VolumeExpansion leaves the plan without a
// preflight; a failed preflight is not a plan failure.
func (s *Server) expansionWarning(ctx context.Context, object desiredObject, current map[string]any) *Warning {
	if object.Ref.Kind != volumeKind || s.volumeExpansion == nil {
		return nil
	}
	wanted, ok := quantityOf(object.Object.Object, "spec", "capacity")
	if !ok {
		return nil
	}
	stored, ok := quantityOf(current, "spec", "capacity")
	if !ok || wanted.Cmp(stored) <= 0 {
		return nil
	}
	plan, err := s.volumeExpansion.ExpansionPreflight(ctx, object.Ref.Namespace, object.Ref.Name)
	if err != nil {
		if s.log != nil {
			s.log.Warn("expansion preflight failed", "volume", object.Ref.String(), "error", err)
		}
		return nil
	}
	ref := object.Ref
	return &Warning{Code: WarningExpansionPreflight, Message: expansionMessage(ref, plan), Object: &ref}
}

// expansionMessage renders C-38's three answers; an unanswerable duration is
// what makes the expansion require downtime.
func expansionMessage(ref ObjectRef, plan volume.ExpansionPlan) string {
	online := func(value bool) string {
		if value {
			return "支持"
		}
		return "不支持"
	}
	estimate := plan.EstimatedDuration.String()
	if estimate == "0s" {
		estimate = "不可判定"
	}
	downtime := "无需停机"
	if plan.DowntimeRequired {
		downtime = "需要停机"
	}
	return "卷 " + ref.String() + " 扩容预检：后端在线扩容=" + online(plan.BackendOnline) +
		"，文件系统在线扩容=" + online(plan.FilesystemOnline) + "，预计耗时=" + estimate + "，" + downtime
}

// quantityOf reads one resource quantity from an object.
func quantityOf(object map[string]any, path ...string) (resource.Quantity, bool) {
	value, found, err := unstructured.NestedString(object, path...)
	if err != nil || !found || value == "" {
		return resource.Quantity{}, false
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, false
	}
	return quantity, true
}

// sort orders the three classes and the warnings so two identical requests
// render the same plan.
func (p *Plan) sort() {
	sort.Slice(p.Create, func(left, right int) bool { return p.Create[left].Ref.String() < p.Create[right].Ref.String() })
	sort.Slice(p.Update, func(left, right int) bool { return p.Update[left].Ref.String() < p.Update[right].Ref.String() })
	sort.Slice(p.Delete, func(left, right int) bool { return p.Delete[left].Ref.String() < p.Delete[right].Ref.String() })
	sort.Slice(p.Warnings, func(left, right int) bool {
		leftRef, rightRef := "", ""
		if p.Warnings[left].Object != nil {
			leftRef = p.Warnings[left].Object.String()
		}
		if p.Warnings[right].Object != nil {
			rightRef = p.Warnings[right].Object.String()
		}
		if p.Warnings[left].Code != p.Warnings[right].Code {
			return p.Warnings[left].Code < p.Warnings[right].Code
		}
		return leftRef < rightRef
	})
}

// render writes the summary a page shows before confirmation (C-62).
func (p *Plan) render(retained int) {
	p.Summary = "计划创建 " + strconv.Itoa(len(p.Create)) + " 个对象，更新 " + strconv.Itoa(len(p.Update)) +
		" 个对象，删除 " + strconv.Itoa(len(p.Delete)) + " 个对象"
	if retained > 0 {
		p.Summary += "，保留 " + strconv.Itoa(retained) + " 个对象"
	}
}

// execute carries out a plan under the operation context: a client that
// disconnected cannot stop it (C-12). The first failure ends the run; the
// objects written before it are the accepted side effects.
func (s *Server) execute(ctx context.Context, plan *Plan, desired []desiredObject) {
	byRef := make(map[string]desiredObject, len(desired))
	for _, object := range desired {
		byRef[object.Ref.String()] = object
	}
	for _, planned := range plan.Create {
		if failure := s.create(ctx, byRef[planned.Ref.String()]); failure != nil {
			return
		}
	}
	for _, planned := range plan.Update {
		if failure := s.update(ctx, byRef[planned.Ref.String()], ""); failure != nil {
			return
		}
	}
	for _, planned := range plan.Delete {
		if failure := s.delete(ctx, planned.objectType, planned.Ref, ""); failure != nil {
			return
		}
	}
}

// applyDesired replaces the client-owned fields of an object with the desired
// state, leaving status and server-assigned metadata untouched (C-03).
func applyDesired(current *unstructured.Unstructured, desired *unstructured.Unstructured) *unstructured.Unstructured {
	updated := current.DeepCopy()
	metadata, _ := desired.Object["metadata"].(map[string]any)
	for _, key := range []string{"labels", "annotations"} {
		if value, ok := metadata[key].(map[string]any); ok && len(value) > 0 {
			_ = unstructured.SetNestedMap(updated.Object, value, "metadata", key)
			continue
		}
		unstructured.RemoveNestedField(updated.Object, "metadata", key)
	}
	if spec, ok := desired.Object["spec"]; ok {
		updated.Object["spec"] = spec
	} else {
		delete(updated.Object, "spec")
	}
	return updated
}

// hasLabel reports whether an object carries a non-empty label.
func hasLabel(object *unstructured.Unstructured, key string) bool {
	return object.GetLabels()[key] != ""
}

// labelOf reads one label of a raw object.
func labelOf(object map[string]any, key string) any {
	metadata, _ := object["metadata"].(map[string]any)
	labels, _ := metadata["labels"].(map[string]any)
	return labels[key]
}
