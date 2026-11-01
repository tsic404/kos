package controlplane

import (
	"context"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/tsic404/kos/api"
)

// NodeMaintenance is the node maintenance flow the API exposes (C-65). The four
// steps move the run kernel's node object and nothing else: the maintenance
// state is that object's unschedulable field, so the API carries no task record
// and the answer is acceptance rather than completion (C-12, C-65). A
// deployment without the flow serves no maintenance entry.
type NodeMaintenance interface {
	// Preflight lists what a maintenance of one node would affect and every
	// item the flow cannot judge (C-65). It writes nothing.
	Preflight(ctx context.Context, request MaintenanceRequest) (MaintenanceReport, *Error)
	// Cordon takes the node out of scheduling on the owner's behalf (C-65) and
	// reports whether this call wrote the field; a node already out of
	// scheduling takes no second write.
	Cordon(ctx context.Context, request MaintenanceRequest, actor string) (MaintenanceReport, bool, *Error)
	// Drain evicts what the run kernel still runs once the node-local
	// applications are stopped (C-65). It never deletes a pod the kernel
	// refuses to evict.
	Drain(ctx context.Context, node, actor string) (MaintenanceDrain, *Error)
	// Restore lifts the cordon (C-65).
	Restore(ctx context.Context, node, actor string) *Error
}

// MaintenanceRequest names the node and the declared downtime estimate (C-55,
// C-65). The estimate is an input because only the operator knows how long the
// node stays out; without it the preflight marks the item undetermined rather
// than guessing.
type MaintenanceRequest struct {
	// Node is the run-kernel node the maintenance applies to.
	Node string `json:"node"`
	// Downtime is the declared estimate, as a duration Go parses ("15m").
	Downtime string `json:"downtime,omitempty"`
}

// MaintenanceReport is the preflight output (C-65): the node-local applications
// and volumes bound to the node, the declared downtime, and the items the flow
// cannot judge.
type MaintenanceReport struct {
	// Node is the node the report is about.
	Node string `json:"node"`
	// Applications are the node-local applications maintenance would stop,
	// named "namespace/name".
	Applications []string `json:"applications"`
	// Volumes are the node-local volumes whose data sits on the node, named
	// "namespace/name".
	Volumes []string `json:"volumes"`
	// Downtime is the declared estimate, empty when none was declared.
	Downtime string `json:"downtime,omitempty"`
	// Undetermined lists what the preflight could not judge (C-65); a flow that
	// has any of them does not proceed.
	Undetermined []MaintenanceUndetermined `json:"undetermined,omitempty"`
}

// MaintenanceUndetermined is one item the preflight cannot judge (C-65).
type MaintenanceUndetermined struct {
	// Item names what could not be judged.
	Item string `json:"item"`
	// Reason states why it cannot be judged.
	Reason string `json:"reason"`
	// Ref is the object the item is about, when it is about one. A read filters
	// the item by the same visibility predicate as the object itself (C-59).
	Ref *ObjectRef `json:"ref,omitempty"`
}

// MaintenanceDrain is what the stop-and-drain step did (C-65).
type MaintenanceDrain struct {
	// Node is the node that was drained.
	Node string `json:"node"`
	// Evicted are the pods the run kernel took off the node.
	Evicted []string `json:"evicted"`
	// Retained are the pods the flow left in place, with the reason: node-local
	// applications keep their workloads on the node, and a daemon set is bound
	// to it by design.
	Retained []MaintenanceRetained `json:"retained,omitempty"`
}

// MaintenanceRetained is one pod the drain left on the node.
type MaintenanceRetained struct {
	// Pod is the retained pod, named "namespace/name".
	Pod string `json:"pod"`
	// Reason states why the pod stays.
	Reason string `json:"reason"`
}

// maintenanceResponse is the answer to one accepted maintenance write (C-12):
// acceptance plus the object the steps moved.
type maintenanceResponse struct {
	// Accepted is always true: a step that failed is an error response.
	Accepted bool `json:"accepted"`
	// Ref identifies the node whose state carries the progress.
	Ref ObjectRef `json:"ref"`
	// Preflight is the report the cordon was accepted with, when one ran.
	Preflight *MaintenanceReport `json:"preflight,omitempty"`
	// Drain is what the drain step did, when it ran.
	Drain *MaintenanceDrain `json:"drain,omitempty"`
}

// handleMaintenancePreflight answers the preflight step (C-65). It writes
// nothing, so a refused node leaves no record. The read goes through the same
// seam as every other one, and the objects it names follow C-59's visibility
// predicate: a role that may read the maintenance state does not thereby learn
// which applications it may not see.
func (s *Server) handleMaintenancePreflight(w http.ResponseWriter, r *http.Request) {
	if s.maintenance == nil {
		writeError(w, notFound("没有接入节点维护流程"))
		return
	}
	request, failure := decodeMaintenanceRequest(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if _, failure := s.authorize(r.Context(), VerbRead, maintenanceRef(request.Node), nil, nil); failure != nil {
		writeError(w, failure)
		return
	}
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()
	report, failure := s.maintenance.Preflight(ctx, request)
	if failure != nil {
		writeError(w, failure)
		return
	}
	report, failure = s.visibleMaintenanceReport(r.Context(), report)
	if failure != nil {
		writeError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"preflight": report})
}

// visibleMaintenanceReport drops the applications one session may not see from
// a preflight report (C-59). Every other class carries no visibility predicate,
// so only the application names and the items about applications are filtered.
func (s *Server) visibleMaintenanceReport(ctx context.Context, report MaintenanceReport) (MaintenanceReport, *Error) {
	objectType, failure := resolveType("Application", "", s.extensions)
	if failure != nil {
		return MaintenanceReport{}, failure
	}
	applications := make([]string, 0, len(report.Applications))
	for _, name := range report.Applications {
		shown, failure := s.visibleApplication(ctx, objectType, name)
		if failure != nil {
			return MaintenanceReport{}, failure
		}
		if shown {
			applications = append(applications, name)
		}
	}
	report.Applications = applications
	undetermined := make([]MaintenanceUndetermined, 0, len(report.Undetermined))
	for _, item := range report.Undetermined {
		if item.Ref != nil && item.Ref.Kind == "Application" {
			shown, failure := s.visibleApplication(ctx, objectType, item.Ref.Namespace+"/"+item.Ref.Name)
			if failure != nil {
				return MaintenanceReport{}, failure
			}
			if !shown {
				continue
			}
		}
		undetermined = append(undetermined, item)
	}
	report.Undetermined = undetermined
	return report, nil
}

// visibleApplication reports whether one "namespace/name" application is
// visible to the session. An application the cluster no longer holds is not
// shown: the report names what maintenance would stop, and an object that is
// gone is not one of them.
func (s *Server) visibleApplication(ctx context.Context, objectType ObjectType, name string) (bool, *Error) {
	namespace, application, found := strings.Cut(name, "/")
	if !found {
		return false, nil
	}
	ref := ObjectRef{Group: api.Group, Kind: "Application", Namespace: namespace, Name: application}
	observed, err := s.source.Get(ctx, objectType, namespace, application)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, s.readFailure(err, ref)
	}
	return s.visible(ctx, ref, observed.Object)
}

// handleMaintenanceStart runs the cordon step (C-65): the preflight decides,
// and the answer is acceptance because the state it moved is the node field.
func (s *Server) handleMaintenanceStart(w http.ResponseWriter, r *http.Request) {
	if s.maintenance == nil {
		writeError(w, notFound("没有接入节点维护流程"))
		return
	}
	request, failure := decodeMaintenanceRequest(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	grant, failure := s.authorize(r.Context(), VerbUpdate, maintenanceRef(request.Node), nil, nil)
	if failure != nil {
		writeError(w, failure)
		return
	}
	actor := s.actorOfGrant(r.Context(), grant)
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()
	report, _, failure := s.maintenance.Cordon(ctx, request, actor)
	if failure != nil {
		writeError(w, failure)
		return
	}
	writeJSON(w, http.StatusAccepted, maintenanceResponse{Accepted: true, Ref: maintenanceRef(request.Node), Preflight: &report})
}

// handleMaintenanceDrain runs the stop-and-drain step (C-65). The flow refuses
// while a node-local application still runs, so the stop always precedes the
// eviction.
func (s *Server) handleMaintenanceDrain(w http.ResponseWriter, r *http.Request) {
	if s.maintenance == nil {
		writeError(w, notFound("没有接入节点维护流程"))
		return
	}
	request, failure := decodeMaintenanceRequest(r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	grant, failure := s.authorize(r.Context(), VerbUpdate, maintenanceRef(request.Node), nil, nil)
	if failure != nil {
		writeError(w, failure)
		return
	}
	actor := s.actorOfGrant(r.Context(), grant)
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()
	drain, failure := s.maintenance.Drain(ctx, request.Node, actor)
	if failure != nil {
		writeError(w, failure)
		return
	}
	writeJSON(w, http.StatusAccepted, maintenanceResponse{Accepted: true, Ref: maintenanceRef(request.Node), Drain: &drain})
}

// handleMaintenanceRestore runs the restore step (C-65).
func (s *Server) handleMaintenanceRestore(w http.ResponseWriter, r *http.Request) {
	if s.maintenance == nil {
		writeError(w, notFound("没有接入节点维护流程"))
		return
	}
	node := r.PathValue("name")
	if node == "" {
		writeError(w, invalidField("node 必填"))
		return
	}
	grant, failure := s.authorize(r.Context(), VerbUpdate, maintenanceRef(node), nil, nil)
	if failure != nil {
		writeError(w, failure)
		return
	}
	actor := s.actorOfGrant(r.Context(), grant)
	ctx, cancel := context.WithTimeout(s.operations, s.timeout)
	defer cancel()
	if failure := s.maintenance.Restore(ctx, node, actor); failure != nil {
		writeError(w, failure)
		return
	}
	writeJSON(w, http.StatusAccepted, maintenanceResponse{Accepted: true, Ref: maintenanceRef(node)})
}

// maintenanceRef names the run-kernel node as the object a maintenance write
// touches. The group is the kernel's, because the audit record the attribution
// rule reads is written under it (C-14, C-65).
func maintenanceRef(node string) ObjectRef {
	return ObjectRef{Group: api.Group, Kind: "Node", Name: node}
}

// decodeMaintenanceRequest reads one maintenance body. A field the model does
// not define is refused, so a caller cannot ask for a forced drain: no such
// path exists (C-55, C-65).
func decodeMaintenanceRequest(r *http.Request) (MaintenanceRequest, *Error) {
	var request MaintenanceRequest
	if r.ContentLength != 0 {
		if failure := decodeBody(r, true, &request); failure != nil {
			return MaintenanceRequest{}, failure
		}
	}
	// A page action names its target in the path template, so the same two
	// fields arrive as query parameters.
	if request.Node == "" {
		request.Node = r.URL.Query().Get("node")
	}
	if request.Downtime == "" {
		request.Downtime = r.URL.Query().Get("downtime")
	}
	if request.Node == "" {
		return MaintenanceRequest{}, invalidField("node 必填")
	}
	if request.Downtime != "" {
		downtime, err := time.ParseDuration(request.Downtime)
		if err != nil || downtime < 0 {
			return MaintenanceRequest{}, invalidField("downtime 必须是正时长，例如 15m")
		}
	}
	return request, nil
}
