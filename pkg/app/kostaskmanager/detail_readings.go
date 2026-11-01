package kostaskmanager

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/backup"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// ApplicationLogs reads one application's log lines (C-64). The control plane's
// log route and the detail page's log item answer from this one read, so the
// page shows what the route serves. A nil seam leaves the item unreadable
// instead of presenting an empty log as a read with no lines.
type ApplicationLogs interface {
	// ReadApplicationLogs returns one application's bounded log read.
	ReadApplicationLogs(ctx context.Context, namespace, name string, bounds controlplane.LogBounds) (controlplane.LogRead, *controlplane.Error)
}

// detailValue resolves one item of the application detail page for the
// application the read names (C-52).
func (r *Readings) detailValue(ctx context.Context, session identity.Session, item string, target pages.Target) (any, *controlplane.Error) {
	application, failure := r.applicationOf(ctx, session, target)
	if failure != nil {
		return nil, failure
	}
	switch item {
	case ItemDetailHealth:
		return healthOf(*application), nil
	case ItemDetailVolumes:
		return r.volumesOf(ctx, *application)
	case ItemDetailEndpoints:
		return r.endpointsOf(ctx, *application)
	case ItemDetailBackup:
		return r.backupOf(ctx, *application)
	case ItemDetailDrift:
		return map[string]any{"drift": application.Status.Drift}, nil
	case ItemDetailEvents:
		return r.eventsOf(ctx, session, *application)
	case ItemDetailLogs:
		return r.logsOf(ctx, *application)
	default:
		return nil, unknownItem(item)
	}
}

// applicationOf resolves the application one detail read names. A read naming no
// object, naming another class, naming no namespace, or naming an application the
// session may not see is refused: the page shows one application, and answering
// it with another one would show a value the caller did not ask for (C-52, C-59).
func (r *Readings) applicationOf(ctx context.Context, session identity.Session, target pages.Target) (*v1beta1.Application, *controlplane.Error) {
	if !target.Complete() || target.Kind != "Application" || target.Namespace == "" {
		return nil, detailTargetRequired()
	}
	object, failure := r.reader().Get(ctx, "Application", target.Namespace, target.Name)
	if failure != nil {
		return nil, failure
	}
	visible, failure := r.reader().Visible(ctx, session, "Application", object)
	if failure != nil {
		return nil, failure
	}
	if !visible {
		// An application the session may not see is answered as absent, exactly
		// as the read path treats it (C-59).
		return nil, applicationAbsent(target)
	}
	var application v1beta1.Application
	if failure := cluster.Decode(object, &application); failure != nil {
		return nil, failure
	}
	return &application, nil
}

// detailTargetRequired answers a detail read that names no application.
func detailTargetRequired() *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInvalidField,
		Message:     "应用详情页必须指名一个应用：给出 kind=Application、namespace 与 name",
		Stage:       controlplane.StageValidate,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// applicationAbsent is the one answer for an application no longer there and for
// one the session may not see.
func applicationAbsent(target pages.Target) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeNotFound,
		Message:     "应用 " + target.Namespace + "/" + target.Name + " 不存在",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// healthOf renders the application's health item (C-52): the phase and health
// the object reports, the stop marker, the desired replicas, and every condition
// the reconciler wrote.
func healthOf(application v1beta1.Application) map[string]any {
	value := map[string]any{
		"phase":      application.Status.Phase,
		"health":     string(application.Status.Health.State),
		"paused":     application.Spec.Paused,
		"conditions": conditionsOf(application.Status.Conditions),
	}
	if application.Spec.Replicas != nil {
		value["replicas"] = *application.Spec.Replicas
	}
	if application.Spec.DriftPolicy != "" {
		value["driftPolicy"] = string(application.Spec.DriftPolicy)
	}
	return value
}

// volumeView is one volume a detail page shows: the volume the application
// declares, with its stored fields. A declared volume that no longer exists
// keeps its place and carries the reason it cannot be read.
type volumeView struct {
	// Namespace is the volume's namespace.
	Namespace string `json:"namespace"`
	// Name is the volume's name.
	Name string `json:"name"`
	// Found reports that the volume object exists.
	Found bool `json:"found"`
	// Capacity is the size the claim requests.
	Capacity string `json:"capacity,omitempty"`
	// StorageClass is the backend policy.
	StorageClass string `json:"storageClass,omitempty"`
	// Mobility is how far the volume moves with its data.
	Mobility string `json:"mobility,omitempty"`
	// Phase is the volume's lifecycle phase.
	Phase string `json:"phase,omitempty"`
	// Health is the volume's health state.
	Health string `json:"health,omitempty"`
	// Reason explains a declared volume that cannot be read.
	Reason string `json:"reason,omitempty"`
}

// volumesOf reads the volumes the application declares (C-52): the names are the
// application spec's own volume list, so the item follows the application rather
// than a selector.
func (r *Readings) volumesOf(ctx context.Context, application v1beta1.Application) (any, *controlplane.Error) {
	views := make([]volumeView, 0, len(application.Spec.Volumes))
	for _, name := range application.Spec.Volumes {
		view := volumeView{Namespace: application.Namespace, Name: name}
		object, failure := r.reader().Get(ctx, "Volume", application.Namespace, name)
		if failure != nil {
			if failure.Code != controlplane.CodeNotFound {
				return nil, failure
			}
			view.Reason = "卷 " + name + " 不存在"
			views = append(views, view)
			continue
		}
		var volume v1beta1.Volume
		if failure := cluster.Decode(object, &volume); failure != nil {
			return nil, failure
		}
		view.Found = true
		view.Capacity = volume.Spec.Capacity
		view.StorageClass = volume.Spec.StorageClass
		view.Mobility = string(volume.Spec.Mobility)
		view.Phase = volume.Status.Phase
		view.Health = string(volume.Status.Health.State)
		views = append(views, view)
	}
	return map[string]any{"volumes": views}, nil
}

// endpointView is one endpoint the application is exposed through (C-52, C-45).
type endpointView struct {
	// Namespace is the endpoint's namespace.
	Namespace string `json:"namespace"`
	// Name is the endpoint's name.
	Name string `json:"name"`
	// Kind is the endpoint's shape.
	Kind string `json:"kind"`
	// Host is the hostname of an HTTP endpoint.
	Host string `json:"host,omitempty"`
	// Path is the path prefix of an HTTP endpoint.
	Path string `json:"path,omitempty"`
	// Port is the port of an L4 or mDNS endpoint.
	Port int32 `json:"port,omitempty"`
	// ServiceName is the advertised name of an mDNS endpoint.
	ServiceName string `json:"serviceName,omitempty"`
	// Phase is the endpoint's lifecycle phase.
	Phase string `json:"phase,omitempty"`
	// Health is the endpoint's health state.
	Health string `json:"health,omitempty"`
}

// endpointsOf reads the endpoints whose target is the application (C-52, C-45):
// the reverse lookup is the rule the endpoint contract fixes, so the page and the
// endpoint's own cascade name the same endpoints.
func (r *Readings) endpointsOf(ctx context.Context, application v1beta1.Application) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Endpoint", application.Namespace)
	if failure != nil {
		return nil, failure
	}
	views := make([]endpointView, 0, len(objects))
	for _, object := range objects {
		if !endpoint.TargetsApp(&unstructured.Unstructured{Object: object}, application.Name) {
			continue
		}
		var stored v1beta1.Endpoint
		if failure := cluster.Decode(object, &stored); failure != nil {
			return nil, failure
		}
		views = append(views, endpointView{
			Namespace: stored.Namespace, Name: stored.Name,
			Kind: string(stored.Spec.Kind), Host: stored.Spec.Host, Path: stored.Spec.Path,
			Port: stored.Spec.Port, ServiceName: stored.Spec.ServiceName,
			Phase: stored.Status.Phase, Health: string(stored.Status.Health.State),
		})
	}
	return map[string]any{"endpoints": views}, nil
}

// backupView is one backup policy that covers the application (C-41).
type backupView struct {
	// Namespace is the policy's namespace.
	Namespace string `json:"namespace"`
	// Name is the policy's name.
	Name string `json:"name"`
	// Schedule is the policy's cron expression.
	Schedule string `json:"schedule,omitempty"`
	// Consistency is the level the copies reach.
	Consistency string `json:"consistency,omitempty"`
	// Phase is the policy's lifecycle phase.
	Phase string `json:"phase,omitempty"`
	// Health is the policy's health state.
	Health string `json:"health,omitempty"`
	// LastBackup is the last completed run, absent when none ran.
	LastBackup *backupRunView `json:"lastBackup,omitempty"`
}

// backupRunView is the last completed backup run of one policy.
type backupRunView struct {
	// CompletedAt is when the run finished; empty when the cluster recorded no
	// time.
	CompletedAt string `json:"completedAt,omitempty"`
	// Consistency is the level the copy reached.
	Consistency string `json:"consistency,omitempty"`
	// Succeeded reports whether the run completed.
	Succeeded bool `json:"succeeded"`
	// Reason explains a run that did not succeed.
	Reason string `json:"reason,omitempty"`
}

// backupOf reads the backup policies that cover the application (C-52): a policy
// covers an application when its selector matches the application's labels and
// the policy's owner is exactly the application's owner. The check is the backup
// controller's own (CoversOwner), so the page and the reconciler cannot disagree;
// a policy without an owner is not a wildcard (C-41, C-60).
func (r *Readings) backupOf(ctx context.Context, application v1beta1.Application) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "BackupPolicy", application.Namespace)
	if failure != nil {
		return nil, failure
	}
	set := labels.Set(application.Labels)
	views := make([]backupView, 0, len(objects))
	for _, object := range objects {
		var policy v1beta1.BackupPolicy
		if failure := cluster.Decode(object, &policy); failure != nil {
			return nil, failure
		}
		selector, err := labels.Parse(policy.Spec.Selector)
		if err != nil || selector.Empty() || !selector.Matches(set) {
			continue
		}
		if !backup.CoversOwner(&policy, application.Spec.Owner) {
			continue
		}
		view := backupView{
			Namespace: policy.Namespace, Name: policy.Name,
			Schedule: policy.Spec.Schedule, Consistency: string(policy.Spec.Consistency),
			Phase: policy.Status.Phase, Health: string(policy.Status.Health.State),
		}
		if record := policy.Status.LastBackup; record != nil {
			run := &backupRunView{Consistency: string(record.Consistency), Succeeded: record.Succeeded, Reason: record.Reason}
			if record.CompletedAt != nil {
				run.CompletedAt = record.CompletedAt.UTC().Format(time.RFC3339)
			}
			view.LastBackup = run
		}
		views = append(views, view)
	}
	return map[string]any{"policies": views}, nil
}

// eventsOf reads the events about the application (C-52, C-64): the same window,
// order and visibility rule the events route serves, filtered to the
// application's own subject.
func (r *Readings) eventsOf(ctx context.Context, session identity.Session, application v1beta1.Application) (any, *controlplane.Error) {
	ref := controlplane.ObjectRef{
		Group: api.Group, Kind: "Application",
		Namespace: application.Namespace, Name: application.Name,
	}
	read, failure := r.reader().EventsAbout(ctx, session, ref)
	if failure != nil {
		return nil, failure
	}
	return read, nil
}

// logsOf reads the application's log lines through the control plane's own read
// (C-52, C-64), so the detail page and the log route answer with the same lines
// and neither widens the window the other supports.
func (r *Readings) logsOf(ctx context.Context, application v1beta1.Application) (any, *controlplane.Error) {
	if r.Logs == nil {
		return nil, &controlplane.Error{
			Code:        controlplane.CodeCapabilityMissing,
			Message:     "日志来源尚未接入控制面",
			Stage:       controlplane.StageCapability,
			SideEffects: controlplane.SideEffectsNone,
		}
	}
	read, failure := r.Logs.ReadApplicationLogs(ctx, application.Namespace, application.Name, controlplane.LogBounds{})
	if failure != nil {
		return nil, failure
	}
	return read, nil
}
