package controlplane

import "github.com/tsic404/kos/api/v1beta1"

// ExtensionKinds reports the object classes active extension components serve
// (C-19). A control plane without loaded extensions serves kernel kinds only.
type ExtensionKinds interface {
	// ObjectClass resolves one declared class. ok is false when no active
	// component declares it, which is a missing capability (F16).
	ObjectClass(group, kind string) (ExtensionObjectClass, bool)
}

// ExtensionObjectClass is one object class an extension declares (C-19).
type ExtensionObjectClass struct {
	// Versions are the versions the class is served in.
	Versions []string
	// Resource is the plural resource name the cluster serves.
	Resource string
	// Namespaced marks a class whose objects live in a namespace.
	Namespaced bool
}

// ControllerAvailability reports what a reader observes of an extension object
// (C-20). A control plane without extension controllers shows every stored
// status as observed.
type ControllerAvailability interface {
	// ProjectedStatus returns the status a reader sees of one object: when the
	// controller serving its class is unavailable the object is Unknown, and
	// every other object keeps its stored status (C-08).
	ProjectedStatus(group, kind string, stored v1beta1.ObjectStatus) v1beta1.ObjectStatus
}
