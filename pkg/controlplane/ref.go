package controlplane

import (
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
)

// ObjectRef identifies one domain object. It is the identifier a client keeps
// across reads and writes; storage details stay on this side (C-12).
type ObjectRef struct {
	// Group is the API group; the kernel group names an object of this model.
	Group string `json:"group"`
	// Kind is the object's kind.
	Kind string `json:"kind"`
	// Namespace is empty for cluster-scoped kinds.
	Namespace string `json:"namespace,omitempty"`
	// Name is the object's name inside its scope.
	Name string `json:"name"`
}

// String renders the reference for messages.
func (r ObjectRef) String() string {
	if r.Namespace == "" {
		return r.Group + "/" + r.Kind + "/" + r.Name
	}
	return r.Group + "/" + r.Kind + "/" + r.Namespace + "/" + r.Name
}

// ObjectType is one object class the control plane serves: a kernel kind (C-01)
// or an extension object class an active component declares (C-19).
type ObjectType struct {
	// Group is the class's API group.
	Group string
	// Version is the served version this request uses.
	Version string
	// Kind is the class's kind.
	Kind string
	// Resource is the plural resource name the cluster serves.
	Resource string
	// Namespaced marks a class whose objects live in a namespace.
	Namespaced bool
}

// GVR returns the resource identity of the class.
func (t ObjectType) GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: t.Group, Version: t.Version, Resource: t.Resource}
}

// IsKernel reports whether the class is one of the ten kernel kinds.
func (t ObjectType) IsKernel() bool {
	return t.Group == api.Group
}

// Key is the query identity a page cursor binds to.
func (t ObjectType) Key() string {
	return t.Group + "/" + t.Version + "/" + t.Kind
}

// kernelType returns the type of a kernel kind in the storage version.
func kernelType(kind string) (ObjectType, bool) {
	scope, known := api.ScopeOf(kind)
	resource, hasResource := api.ResourceName(kind)
	if !known || !hasResource {
		return ObjectType{}, false
	}
	return ObjectType{
		Group:      api.Group,
		Version:    v1beta1.GroupVersion.Version,
		Kind:       kind,
		Resource:   resource,
		Namespaced: scope == api.ScopeNamespaced,
	}, true
}

// kernelRef builds the reference of a kernel object, dropping the namespace of a
// cluster-scoped kind.
func kernelRef(kind, namespace, name string) ObjectRef {
	if scope, ok := api.ScopeOf(kind); ok && scope == api.ScopeCluster {
		namespace = ""
	}
	return ObjectRef{Group: api.Group, Kind: kind, Namespace: namespace, Name: name}
}

// splitAPIVersion splits an apiVersion into group and version.
func splitAPIVersion(apiVersion string) (group, version string, ok bool) {
	group, version, found := strings.Cut(apiVersion, "/")
	if !found || group == "" || version == "" {
		return "", "", false
	}
	return group, version, true
}

// resolveType decides whether this API can serve the requested kind. A kind of
// another group belongs to an extension object class: it is served only while
// the declaring component is active (C-19), and an unserved class is a missing
// capability rather than a malformed request (F16). A kind inside the kernel
// group that is not one of the ten is invalid, because the model is closed
// (C-01).
func resolveType(kind, apiVersion string, extensions ExtensionKinds) (ObjectType, *Error) {
	if kind == "" {
		return ObjectType{}, invalidField("对象必须给出 kind")
	}
	group, version := api.Group, ""
	if apiVersion != "" {
		parsedGroup, parsedVersion, ok := splitAPIVersion(apiVersion)
		if !ok {
			return ObjectType{}, invalidField("apiVersion 必须是 group/version 形式")
		}
		group, version = parsedGroup, parsedVersion
	}
	if group == api.Group {
		objectType, known := kernelType(kind)
		if !known {
			message := "kind " + kind + " 不在内核对象模型内，新增对象类属于框架变更"
			if extensions != nil {
				message += "；扩展对象类必须用 apiVersion 指名其 API 组"
			}
			return ObjectType{}, invalidField(message)
		}
		return objectType, nil
	}
	if extensions != nil {
		if class, ok := extensions.ObjectClass(group, kind); ok {
			if !servesVersion(class.Versions, version) {
				return ObjectType{}, capabilityMissing(group, kind, "版本 "+version+" 未被服务")
			}
			return ObjectType{
				Group:      group,
				Version:    version,
				Kind:       kind,
				Resource:   class.Resource,
				Namespaced: class.Namespaced,
			}, nil
		}
	}
	return ObjectType{}, capabilityMissing(group, kind, "")
}

// capabilityMissing builds the failure for a class this control plane does not
// serve (C-19, F16).
func capabilityMissing(group, kind, detail string) *Error {
	message := "对象类 " + group + "/" + kind + " 所属的扩展尚未安装，控制面无法读写它"
	if detail != "" {
		message += "（" + detail + "）"
	}
	return (&Error{
		Code:               CodeCapabilityMissing,
		Message:            message,
		Stage:              StageCapability,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	}).about(ObjectRef{Group: group, Kind: kind})
}

// servesVersion reports whether an extension class serves version.
func servesVersion(versions []string, version string) bool {
	for _, served := range versions {
		if served == version {
			return true
		}
	}
	return false
}
