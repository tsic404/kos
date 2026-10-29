package controlplane

import (
	"context"
	"encoding/json"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// actionRequest is one user action on one application (C-33, C-66).
type actionRequest struct {
	// Kind is the object kind the action applies to; only Application acts.
	Kind string `json:"kind"`
	// Namespace is the application's namespace.
	Namespace string `json:"namespace"`
	// Name is the application's name.
	Name string `json:"name"`
	// Action is one of the application actions.
	Action string `json:"action"`
	// TargetDigest is the digest a confirmation accepts as the new baseline
	// (C-35); only confirm carries it.
	TargetDigest string `json:"targetDigest,omitempty"`
	// Replicas is the target count of a scale action (C-66).
	Replicas *int32 `json:"replicas,omitempty"`
}

// actionResponse accepts an action and names the records it wrote (C-12).
type actionResponse struct {
	// Accepted is always true: a failed action is an error response.
	Accepted bool `json:"accepted"`
	// Ref is the one-shot action record the reconciler consumes.
	Ref *ObjectRef `json:"ref"`
	// Idempotent tells that the record was already in place.
	Idempotent bool `json:"idempotent,omitempty"`
}

// handleAction accepts a user action on an application. A lifecycle action
// travels as a one-shot record in the KOS namespace; the application reconciler
// reads it and deletes it. Stop, start and scale are spec writes the API
// performs on the user's behalf. Neither path puts a restart or an action
// trigger into the user spec (C-04, C-33, C-66).
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request) {
	var request actionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, invalid("请求体不是合法的动作请求"))
		return
	}
	if request.Kind == "" {
		request.Kind = "Application"
	}
	if request.Kind != "Application" {
		writeError(w, invalidField("只有应用接受生命周期动作"))
		return
	}
	if request.Name == "" || request.Namespace == "" {
		writeError(w, invalidField("动作必须给出应用的名字与命名空间"))
		return
	}
	switch request.Action {
	case app.ActionRetry, app.ActionOrphan, app.ActionRestart,
		app.ActionStop, app.ActionStart, app.ActionScale:
	case app.ActionConfirm:
		if request.TargetDigest == "" {
			writeError(w, invalidField("确认必须给出目标摘要"))
			return
		}
	default:
		writeError(w, invalidField("未知动作 "+request.Action))
		return
	}
	applicationType, ok := kernelType("Application")
	if !ok {
		writeError(w, internal(StageRead, "应用不是内核对象类"))
		return
	}
	ref := kernelRef("Application", request.Namespace, request.Name)
	if failure := s.authorize(r.Context(), VerbUpdate, ref); failure != nil {
		writeError(w, failure)
		return
	}
	// The write outlives the client (C-12), but the actor belongs to the request:
	// resolve it before the detached context drops the session values (C-14).
	actor := s.actorOf(r.Context())
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()

	stored, err := s.source.Get(ctx, applicationType, ref.Namespace, ref.Name)
	if err != nil {
		writeError(w, s.readFailure(err, ref))
		return
	}
	application := &v1beta1.Application{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, application); err != nil {
		writeError(w, internal(StageRead, "读取应用状态失败").about(ref))
		return
	}
	if application.DeletionTimestamp != nil && request.Action != app.ActionOrphan {
		writeError(w, invalidField("应用正在卸载，不接受动作 "+request.Action).about(ref))
		return
	}
	if failure := acceptAction(request, application); failure != nil {
		writeError(w, failure)
		return
	}
	if recordAction(request.Action) {
		record, created, failure := s.writeActionRecord(ctx, request.Action, application, request.TargetDigest)
		if failure != nil {
			writeError(w, failure)
			return
		}
		if created {
			s.audit(ctx, actor, request.Action, ref)
		}
		writeJSON(w, http.StatusAccepted, actionResponse{Accepted: true, Ref: new(refOfObject(record)), Idempotent: !created})
		return
	}
	updated, changed, failure := s.writeApplicationAction(ctx, applicationType, request, stored.Object)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if changed {
		s.audit(ctx, actor, request.Action, ref)
	}
	response := actionResponse{Accepted: true, Ref: new(ref), Idempotent: !changed}
	if updated != nil {
		response.Ref = new(refOfObject(updated))
	}
	writeJSON(w, http.StatusAccepted, response)
}

// recordAction reports whether an action travels as a one-shot record instead of
// a spec write (C-33, C-66).
func recordAction(action string) bool {
	switch action {
	case app.ActionRetry, app.ActionOrphan, app.ActionRestart, app.ActionConfirm:
		return true
	default:
		return false
	}
}

// writeApplicationAction performs the spec write of stop, start or scale. It
// updates the stored object under its resourceVersion, so a concurrent write
// loses with a 409 instead of being silently overwritten (C-05, C-66).
func (s *Server) writeApplicationAction(ctx context.Context, applicationType ObjectType, request actionRequest, live map[string]any) (*unstructured.Unstructured, bool, *Error) {
	before := &unstructured.Unstructured{Object: live}
	ref := refOfObject(before)
	updated := before.DeepCopy()
	spec, found, err := unstructured.NestedMap(updated.Object, "spec")
	if err != nil {
		return nil, false, invalidField("spec 无法读取: " + err.Error()).about(ref)
	}
	if !found {
		spec = map[string]any{}
	}
	changed := false
	switch request.Action {
	case app.ActionStop:
		if paused, _, _ := unstructured.NestedBool(spec, "paused"); !paused {
			spec["paused"] = true
			changed = true
		}
	case app.ActionStart:
		if _, present := spec["paused"]; present {
			delete(spec, "paused")
			changed = true
		}
	case app.ActionScale:
		if request.Replicas == nil {
			return nil, false, invalidField("scale 必须给出 replicas").about(ref)
		}
		storedReplicas, present, _ := unstructured.NestedInt64(spec, "replicas")
		if !present || storedReplicas != int64(*request.Replicas) {
			spec["replicas"] = int64(*request.Replicas)
			changed = true
		}
	}
	if !changed {
		return before, false, nil
	}
	if err := unstructured.SetNestedMap(updated.Object, spec, "spec"); err != nil {
		return nil, false, internal(StageApply, "动作无法写入应用").about(ref)
	}
	resource := s.dynamic.Resource(applicationType.GVR()).Namespace(ref.Namespace)
	result, err := resource.Update(ctx, updated, metav1.UpdateOptions{FieldManager: "kos-api"})
	if err != nil {
		return nil, false, s.writeFailure(ctx, applicationType, err, ref)
	}
	return result, true, nil
}

// acceptAction checks the phase an action may leave (C-33, C-66).
func acceptAction(request actionRequest, application *v1beta1.Application) *Error {
	switch request.Action {
	case app.ActionRetry:
		if application.Status.Phase != app.PhaseFailed {
			// A rolled-back upgrade is retried the same way: the application
			// stays Ready and the record clears the failed attempt (C-34).
			if application.Status.Upgrade == nil || !application.Status.Upgrade.RolledBack {
				return invalidField("retry 只对 Failed 或已回滚升级的应用有效")
			}
		}
	case app.ActionOrphan:
		if application.Status.Phase != app.PhaseUninstalling {
			return invalidField("orphan 只对 Uninstalling 的应用有效")
		}
		if application.Status.Uninstall == nil || len(application.Status.Uninstall.Blocked) == 0 {
			return invalidField("orphan 需要非空的卸载阻塞清单")
		}
		if !application.Status.Uninstall.Stalled {
			return invalidField("orphan 只在卸载阻塞超过十分钟后可用；先修复阻塞对象")
		}
	case app.ActionConfirm:
		condition, present := state.Find(application.Status.Conditions, api.ConditionReconciled)
		if !present || condition.Status != metav1.ConditionFalse || condition.Reason != app.ReasonExternalChangePending {
			return invalidField("confirm 只对外部改动待确认的应用有效")
		}
	case app.ActionRestart:
		if application.Status.Phase != app.PhaseReady && application.Status.Phase != app.PhaseUpgrading {
			return invalidField("restart 只对已安装的应用有效")
		}
	case app.ActionScale:
		if request.Replicas == nil || *request.Replicas < 0 {
			return invalidField("scale 必须给出非负的 replicas")
		}
	}
	return nil
}

// writeActionRecord writes the one-shot record of one action. A record already
// in place makes the request idempotent (C-12). A confirmation also carries the
// digest of the spec that was stored when the user submitted it, so the
// reconciler judges the three-way agreement of C-35.
func (s *Server) writeActionRecord(ctx context.Context, action string, application *v1beta1.Application, targetDigest string) (*unstructured.Unstructured, bool, *Error) {
	uid := string(application.UID)
	data := map[string]any{
		app.ActionDataKey: application.Namespace + "/" + application.Name,
	}
	if action == app.ActionConfirm {
		digest, err := app.SpecDigest(application.Spec)
		if err != nil {
			return nil, false, internal(StageApply, "计算应用 spec 摘要失败").about(kernelRef("Application", application.Namespace, application.Name))
		}
		data[app.ActionTargetDigestKey] = targetDigest
		data[app.ActionSpecDigestKey] = digest
	}
	name := app.ActionRecordName(uid, action)
	if action == app.ActionConfirm {
		name = app.ConfirmRecordName(uid, targetDigest)
	}
	record := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": api.SystemNamespace,
			"labels": map[string]any{
				markers.ManagedBy:    write.ManagedByValue,
				app.ActionLabel:      action,
				app.ActionOwnerLabel: uid,
			},
		},
		"data": data,
	}}
	resource := s.dynamic.Resource(configMapType.GVR()).Namespace(api.SystemNamespace)
	created, err := resource.Create(ctx, record, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			stored, readErr := resource.Get(ctx, record.GetName(), metav1.GetOptions{})
			if readErr != nil {
				return nil, false, s.writeFailure(ctx, configMapType, readErr, refOfObject(record))
			}
			return stored, false, nil
		}
		return nil, false, s.writeFailure(ctx, configMapType, err, refOfObject(record))
	}
	return created, true, nil
}

// refOfObject reads the identity of an object into the API's reference shape.
func refOfObject(object *unstructured.Unstructured) ObjectRef {
	return ObjectRef{
		Group:     object.GroupVersionKind().Group,
		Kind:      object.GetKind(),
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
	}
}

// configMapType is the class the action records live in (C-04).
var configMapType = ObjectType{Group: "", Version: "v1", Kind: "ConfigMap", Resource: "configmaps", Namespaced: true}
