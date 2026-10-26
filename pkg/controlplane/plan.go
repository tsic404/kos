package controlplane

import (
	"context"
	"sort"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
)

// Warning codes are stable like error codes: a client can branch on them.
const (
	// WarningUnattributed marks a desired object without an ownership marker;
	// the write still happens, but nothing claims the object (C-02).
	WarningUnattributed = "unattributed"
	// WarningRetained marks an object the plan keeps because deleting it would
	// release data (C-39, F5).
	WarningRetained = "retained"
)

// maxSetObjects bounds one plan or apply request.
const maxSetObjects = 200

// volumeKind is the kind whose deletion would release data (C-39).
const volumeKind = "Volume"

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
		if seen[object.Ref.String()] {
			return nil, nil, invalidField("请求重复给出了对象 " + object.Ref.String()).about(object.Ref)
		}
		seen[object.Ref.String()] = true
		if failure := s.authorize(ctx, verb, object.Ref); failure != nil {
			return nil, nil, failure
		}
		desired = append(desired, object)
	}

	plan := &Plan{Create: []PlannedObject{}, Update: []PlannedUpdate{}, Delete: []PlannedObject{}, Warnings: []Warning{}}
	for _, object := range desired {
		if !hasLabel(object.Object, markers.ManagedBy) {
			plan.Warnings = append(plan.Warnings, Warning{
				Code:    WarningUnattributed,
				Message: "对象 " + object.Ref.String() + " 没有归属标记，写入后不归属任何安装",
				Object:  new(object.Ref),
			})
		}
		current, err := s.source.Get(ctx, object.Type, object.Ref.Namespace, object.Ref.Name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				plan.Create = append(plan.Create, PlannedObject{Ref: object.Ref, objectType: object.Type})
				continue
			}
			s.log.Error("plan read failed", "object", object.Ref.String(), "error", err)
			return nil, nil, internal(StagePlan, "读取对象 "+object.Ref.String()+" 失败").about(object.Ref)
		}
		if failure := validateEndpointUpdate(current.Object, object); failure != nil {
			return nil, nil, failure
		}
		if differences := changes(current.Object, object.Object.Object); len(differences) > 0 {
			plan.Update = append(plan.Update, PlannedUpdate{Ref: object.Ref, Changes: differences})
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
