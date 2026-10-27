package app

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
)

// WorkloadTarget is what one application round converges its rendered
// workloads to (C-66, C-67). It never changes the translation template's
// output; it writes the facts the application spec owns on top of it.
type WorkloadTarget struct {
	// Paused drives every scalable workload to zero replicas.
	Paused bool
	// Replicas is the written replica count; nil leaves the rendered count.
	Replicas *int32
	// Resources is the declared request and limit set; nil leaves the rendered
	// container resources.
	Resources *v1beta1.ResourceRequirements
}

// scalableKinds are the rendered kinds whose replica count the application
// converges (C-66). A DaemonSet has no replica count to converge.
var scalableKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"ReplicaSet":  true,
}

// workloadKinds are the rendered kinds a restart rebuilds (C-66).
var workloadKinds = map[string]bool{
	"Deployment":            true,
	"StatefulSet":           true,
	"ReplicaSet":            true,
	"ReplicationController": true,
	"DaemonSet":             true,
	"Job":                   true,
	"CronJob":               true,
	"Pod":                   true,
}

// Converge applies the pause, replica and resource facts of an application to
// its rendered objects (C-66, C-67). Volumes, services and routes are left
// untouched, so a stop never removes an entry or data.
func Converge(objects []*unstructured.Unstructured, target WorkloadTarget) {
	for _, object := range objects {
		pod := podTemplate(object)
		if pod == nil {
			continue
		}
		convergeReplicas(object, target)
		if !ResourcesEmpty(target.Resources) {
			convergeResources(pod, target.Resources)
		}
	}
}

// convergeReplicas writes the effective replica count on one scalable workload.
func convergeReplicas(object *unstructured.Unstructured, target WorkloadTarget) {
	if !scalableKinds[object.GetKind()] {
		return
	}
	switch {
	case target.Paused:
		_ = unstructured.SetNestedField(object.Object, int64(0), "spec", "replicas")
	case target.Replicas != nil:
		_ = unstructured.SetNestedField(object.Object, int64(*target.Replicas), "spec", "replicas")
	}
}

// podTemplate returns the pod spec of a workload object; nil means the object
// carries no containers to converge. It reads without copying, because the
// convergence writes through the returned map.
func podTemplate(object *unstructured.Unstructured) map[string]any {
	var path []string
	switch object.GetKind() {
	case "Deployment", "StatefulSet", "ReplicaSet", "DaemonSet", "Job", "ReplicationController":
		path = []string{"spec", "template", "spec"}
	case "CronJob":
		path = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	case "Pod":
		path = []string{"spec"}
	default:
		return nil
	}
	value, found, err := unstructured.NestedFieldNoCopy(object.Object, path...)
	if err != nil || !found {
		return nil
	}
	pod, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return pod
}

// convergeResources replaces every container's requests and limits with the
// declared set (C-67): a field the spec leaves empty is not set by KOS.
func convergeResources(pod map[string]any, resources *v1beta1.ResourceRequirements) {
	for _, key := range []string{"containers", "initContainers"} {
		containers, ok := pod[key].([]any)
		if !ok {
			continue
		}
		for _, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			declared := containerResources(resources)
			if len(declared) == 0 {
				delete(container, "resources")
				continue
			}
			container["resources"] = declared
		}
	}
}

// containerResources renders one resource set into the container shape; an
// empty side is omitted, and the two sides never share a map.
func containerResources(resources *v1beta1.ResourceRequirements) map[string]any {
	declared := map[string]any{}
	if requests := quantityMap(resources.Requests); len(requests) > 0 {
		declared["requests"] = requests
	}
	if limits := quantityMap(resources.Limits); len(limits) > 0 {
		declared["limits"] = limits
	}
	return declared
}

// quantityMap renders the non-empty fields of one request or limit list.
func quantityMap(list v1beta1.ResourceList) map[string]any {
	quantities := map[string]any{}
	if literal := strings.TrimSpace(list.CPU); literal != "" {
		quantities["cpu"] = literal
	}
	if literal := strings.TrimSpace(list.Memory); literal != "" {
		quantities["memory"] = literal
	}
	if len(quantities) == 0 {
		return nil
	}
	return quantities
}

// convergeManifests applies the operational facts of the application spec to one
// manifest set and returns the resource set the workloads converge to (C-66,
// C-67). The frozen package defaults form the base, so a spec that declares no
// resources still carries them on every apply round.
func convergeManifests(application *v1beta1.Application, defaults *v1beta1.ResourceRequirements, objects []*unstructured.Unstructured) *v1beta1.ResourceRequirements {
	resources := MergeResources(defaults, application.Spec.Resources)
	Converge(objects, WorkloadTarget{
		Paused:    application.Spec.Paused,
		Replicas:  application.Spec.Replicas,
		Resources: resources,
	})
	return resources
}

// WorkloadRefs lists the rendered objects a restart rebuilds, in status order
// (C-66). Volumes, services and routes are not workloads and stay in place.
func WorkloadRefs(refs []v1beta1.ObjectRef) []v1beta1.ObjectRef {
	workloads := make([]v1beta1.ObjectRef, 0, len(refs))
	for _, ref := range refs {
		if workloadKinds[ref.Kind] {
			workloads = append(workloads, ref)
		}
	}
	return workloads
}
