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

// CatalogueVerdict is the catalogue's judgement of the entry behind one
// definition (C-24, C-31).
type CatalogueVerdict struct {
	// Installable tells whether the entry may be installed now.
	Installable bool
	// Reason explains a refusal in user-facing terms.
	Reason string
	// Missing lists the required capabilities no active registration satisfies
	// (C-11).
	Missing []string
}

// CatalogueInstall reports what the catalogue judges about the definition an
// application write names (C-24, C-31). A control plane without this seam
// admits every definition and leaves the C-33 pending rule to the reconciler.
type CatalogueInstall interface {
	// JudgeInstall reports the entry behind one definition. ok is false when no
	// active source offers it, so the definition is not an app-center install.
	JudgeInstall(definition string) (CatalogueVerdict, bool)
}
