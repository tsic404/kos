package controlplane

import (
	"strings"

	"github.com/tsic404/kos/api"
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

// resolveKind decides whether this API can serve the requested kind. A kind of
// another group belongs to an extension object class: until the extension is
// installed the kind is not served, which is a missing capability rather than a
// malformed request (C-19, C-21, F16). A kind inside the kernel group that is
// not one of the ten is invalid, because the model is closed (C-01).
func resolveKind(kind, apiVersion string) *Error {
	if kind == "" {
		return invalidField("对象必须给出 kind")
	}
	group := api.Group
	if apiVersion != "" {
		parsed, _, ok := splitAPIVersion(apiVersion)
		if !ok {
			return invalidField("apiVersion 必须是 group/version 形式")
		}
		group = parsed
	}
	if group != api.Group {
		return (&Error{
			Code:               CodeCapabilityMissing,
			Message:            "对象类 " + group + "/" + kind + " 所属的扩展尚未安装，控制面无法读写它",
			Stage:              StageCapability,
			RequiresUserAction: true,
			SideEffects:        SideEffectsNone,
		}).about(ObjectRef{Group: group, Kind: kind})
	}
	if _, ok := api.ScopeOf(kind); !ok {
		return invalidField("kind " + kind + " 不在内核对象模型内，新增对象类属于框架变更")
	}
	return nil
}
