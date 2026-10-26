package write

// Subject is a principal from the writer allocation tables (C-03, C-04).
//
// The set is closed: a subject the tables do not name writes nothing.
type Subject string

const (
	// SubjectUser is a user writing through the API or kubectl (C-03).
	SubjectUser Subject = "user"
	// SubjectAPIProcess is the control plane acting on a user action: it
	// creates action records and writes node scheduling fields (C-04).
	SubjectAPIProcess Subject = "api-process"
	// SubjectInstaller installs and uninstalls KOS (C-04).
	SubjectInstaller Subject = "installer"
	// SubjectApplicationReconciler writes application status and rendered
	// downstream objects (C-03, C-04).
	SubjectApplicationReconciler Subject = "application-reconciler"
	// SubjectEndpointReconciler writes endpoint status and ingress routes.
	SubjectEndpointReconciler Subject = "endpoint-reconciler"
	// SubjectVolumeReconciler writes volume status and bound PVCs.
	SubjectVolumeReconciler Subject = "volume-reconciler"
	// SubjectBackupReconciler writes backup and component status.
	SubjectBackupReconciler Subject = "backup-reconciler"
	// SubjectMigrationReconciler writes migration and restore status.
	SubjectMigrationReconciler Subject = "migration-reconciler"
	// SubjectValidator writes provider status.
	SubjectValidator Subject = "validator"
	// SubjectCatalogueSync writes application definition specs.
	SubjectCatalogueSync Subject = "catalogue-sync"
	// SubjectComponentRegistry writes provider registration fields.
	SubjectComponentRegistry Subject = "component-registry"
	// SubjectDiscovery writes device facts.
	SubjectDiscovery Subject = "discovery"
	// SubjectExtensionReconciler writes extension objects it declared (C-20).
	SubjectExtensionReconciler Subject = "extension-reconciler"
	// SubjectProjection only reads (C-04).
	SubjectProjection Subject = "projection"
)

// knownSubjects is the closed subject set; an entry here is what keeps a typo
// from turning into an allowed write.
var knownSubjects = map[Subject]bool{
	SubjectUser:                  true,
	SubjectAPIProcess:            true,
	SubjectInstaller:             true,
	SubjectApplicationReconciler: true,
	SubjectEndpointReconciler:    true,
	SubjectVolumeReconciler:      true,
	SubjectBackupReconciler:      true,
	SubjectMigrationReconciler:   true,
	SubjectValidator:             true,
	SubjectCatalogueSync:         true,
	SubjectComponentRegistry:     true,
	SubjectDiscovery:             true,
	SubjectExtensionReconciler:   true,
	SubjectProjection:            true,
}

// Role is the session role of a user write (C-59).
type Role string

const (
	// RoleOwner writes everything and is the only role that may transfer an
	// object's owner (C-59, C-60).
	RoleOwner Role = "Owner"
	// RoleMember writes the objects it owns and reads the rest (C-59).
	RoleMember Role = "Member"
	// RoleGuest only reads (C-59).
	RoleGuest Role = "Guest"
)

// Session is the identity behind a user write (C-59, C-60).
type Session struct {
	Username string
	Role     Role
}

// IsSignedIn reports whether the session carries a usable identity.
func (s Session) IsSignedIn() bool {
	if s.Username == "" {
		return false
	}
	return s.Role == RoleOwner || s.Role == RoleMember || s.Role == RoleGuest
}
