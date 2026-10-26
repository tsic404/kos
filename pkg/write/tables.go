package write

import (
	"sort"

	"github.com/tsic404/kos/pkg/markers"
)

// This file is the writer allocation of C-03 and C-04 written as data: every
// entry says one subject, one part, one writer. Absence means forbidden, so a
// kind or subject added without its entry writes nothing.

// statusWriters is the single status writer of each kernel kind that has one
// (C-03). ApplicationDefinition and NodeDevice have none (C-07).
var statusWriters = map[string]Subject{
	"Application":     SubjectApplicationReconciler,
	"Endpoint":        SubjectEndpointReconciler,
	"Volume":          SubjectVolumeReconciler,
	"Provider":        SubjectValidator,
	"SystemComponent": SubjectBackupReconciler,
	"BackupPolicy":    SubjectBackupReconciler,
	"RestoreJob":      SubjectMigrationReconciler,
	"VolumeMigration": SubjectMigrationReconciler,
}

// systemSpecWriters is the single spec writer of the kinds whose spec is system
// owned (C-03). Every other kind's spec is user owned.
var systemSpecWriters = map[string]Subject{
	"ApplicationDefinition": SubjectCatalogueSync,
	"Provider":              SubjectComponentRegistry,
	"NodeDevice":            SubjectDiscovery,
}

// ownedKinds carry spec.owner, which the API writes from the session and only
// the global Owner may transfer (C-60). They are also the kinds a Member may
// write, and only when it owns them: an endpoint's owner comes from its target
// application (C-59, C-60) and is not resolvable from the endpoint yet.
var ownedKinds = map[string]bool{
	"Application":     true,
	"Volume":          true,
	"BackupPolicy":    true,
	"RestoreJob":      true,
	"VolumeMigration": true,
}

// OwnedKinds returns the kinds that carry spec.owner (C-60), sorted. It is the
// list an account's remaining ownership is checked against (C-63).
func OwnedKinds() []string {
	kinds := make([]string, 0, len(ownedKinds))
	for kind := range ownedKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// markerClasses is the marker class each subject may write (C-03). Attribution
// on a downstream object is the creator's, added at creation by C-02.
var markerClasses = map[Subject]map[markers.Class]bool{
	SubjectUser: {
		markers.ClassAttribution: true,
		markers.ClassDecline:     true,
	},
	SubjectApplicationReconciler: {markers.ClassAttribution: true},
	SubjectEndpointReconciler:    {markers.ClassAttribution: true},
	SubjectVolumeReconciler:      {markers.ClassRelease: true},
	SubjectBackupReconciler:      {markers.ClassAttribution: true},
	SubjectMigrationReconciler: {
		markers.ClassAttribution: true,
		markers.ClassRelease:     true,
	},
	SubjectCatalogueSync:     {markers.ClassAttribution: true},
	SubjectComponentRegistry: {markers.ClassAttribution: true},
	SubjectDiscovery:         {markers.ClassAttribution: true},
	SubjectValidator:         {markers.ClassValidity: true},
}

// downstreamWriters is the C-04 allocation for native objects below a KOS
// object. The application reconciler renders inside its declared namespaces, the
// extension controller inside its declared scope, and the installer in its own;
// the volume reconciler, the catalogue, discovery and the projection write none.
// The endpoint reconciler's ingress routes, the backup reconciler's CronJob and
// the migration flow's replication Job and target objects are object-level rows
// (C-43/C-44, C-41, C-40) that are not implemented yet: those subjects are
// refused rather than granted a class-wide write they were never given.
var downstreamWriters = map[Subject]map[Verb]bool{
	SubjectInstaller:             allVerbs(),
	SubjectApplicationReconciler: allVerbs(),
	SubjectExtensionReconciler:   allVerbs(),
}

// claimWriters is the C-04 allocation for claims, keyed by the role the write
// declares: a bound claim is created, expanded and deleted by the volume
// reconciler alone; a migration target is created and deleted by the migration
// and restore flow alone (C-40). A subject of one row writes nothing in the
// other, and a write that names no role is refused.
var claimWriters = map[Claim]map[Subject]map[Verb]bool{
	ClaimBound: {
		SubjectVolumeReconciler: allVerbs(),
	},
	ClaimMigrationTarget: {
		SubjectMigrationReconciler: {VerbCreate: true, VerbDelete: true},
	},
}

// nodeWriters is the C-04 allocation for run-kernel node objects: the control
// plane API process writes and clears unschedulable fields on a user action and
// nothing else; KOS never creates or deletes a node (C-65).
var nodeWriters = map[Subject]map[Verb]bool{
	SubjectAPIProcess: {VerbApply: true},
}

// systemWriters is the C-04 allocation for objects in the KOS namespace: the
// installer owns them, the API process creates action records, and the
// application reconciler deletes records it consumed.
var systemWriters = map[Subject]map[Verb]bool{
	SubjectInstaller:             allVerbs(),
	SubjectAPIProcess:            {VerbCreate: true},
	SubjectApplicationReconciler: {VerbDelete: true},
}

// extensionWriters is the C-20 allocation for extension objects: the extension
// controller that declared the group, within that declaration only.
var extensionWriters = map[Subject]bool{
	SubjectExtensionReconciler: true,
}

// allVerbs allows every write verb on one class.
func allVerbs() map[Verb]bool {
	return map[Verb]bool{VerbCreate: true, VerbApply: true, VerbDelete: true}
}
