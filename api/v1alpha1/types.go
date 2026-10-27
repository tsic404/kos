// Package v1alpha1 contains the first served version of the kernel object
// model (C-01). It is a structural twin of v1beta1 (C-16).
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// HealthState is the availability axis of an object (C-08).
//
// +kubebuilder:validation:Enum=Healthy;Degraded;Unavailable;Unknown
type HealthState string

const (
	// HealthStateHealthy means every desired workload is ready.
	HealthStateHealthy HealthState = "Healthy"
	// HealthStateDegraded means part of the desired state is not ready.
	HealthStateDegraded HealthState = "Degraded"
	// HealthStateUnavailable means a required part of the desired state is gone.
	HealthStateUnavailable HealthState = "Unavailable"
	// HealthStateUnknown means the object cannot be observed right now.
	HealthStateUnknown HealthState = "Unknown"
)

// Health carries the availability axis; it moves independently of phase (C-07).
type Health struct {
	// State is the aggregate health of the object (C-08).
	// +optional
	State HealthState `json:"state,omitempty"`
}

// ObjectStatus is the status shape shared by the eight kinds that have a
// status writer (C-07).
type ObjectStatus struct {
	// Phase is the flow stage; it is orthogonal to health (C-07).
	// +optional
	Phase string `json:"phase,omitempty"`
	// Health is the availability axis of the object (C-08).
	// +optional
	Health Health `json:"health,omitempty"`
	// Conditions carries Reconciled and the object's failure conditions (C-07).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Application is a namespaced application instance.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=applications,scope=Namespaced
// +kubebuilder:subresource:status
type Application struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the user (C-03).
	// +optional
	Spec ApplicationSpec `json:"spec,omitempty"`
	// Status is written by the application reconciler (C-03, C-07, C-33).
	// +optional
	Status ApplicationStatus `json:"status,omitempty"`
}

// ApplicationStatus is the status of an application: the two shared axes plus
// the lifecycle facts C-33 makes observable.
type ApplicationStatus struct {
	ObjectStatus `json:",inline"`
	// RetryCount is the number of failed attempts in the current retry budget
	// (C-33).
	// +optional
	RetryCount int32 `json:"retryCount,omitempty"`
	// NextRetryAt is the earliest time the next automatic attempt may run
	// (C-33).
	// +optional
	NextRetryAt *metav1.Time `json:"nextRetryAt,omitempty"`
	// Uninstall carries the objects that block an uninstall (C-33).
	// +optional
	Uninstall *UninstallStatus `json:"uninstall,omitempty"`
	// Rendered lists the objects the last applied render wrote, so an uninstall
	// does not depend on the definition still existing (C-33).
	// +optional
	Rendered []ObjectRef `json:"rendered,omitempty"`
}

// UninstallStatus is the uninstall blocking state of C-33.
type UninstallStatus struct {
	// Blocked lists the objects that have not gone away yet.
	// +optional
	Blocked []ObjectRef `json:"blocked,omitempty"`
	// BlockedSince is when the first blocking object was observed.
	// +optional
	BlockedSince *metav1.Time `json:"blockedSince,omitempty"`
	// Stalled reports a block that has outlasted the stall window.
	// +optional
	Stalled bool `json:"stalled,omitempty"`
}

// ObjectRef identifies one object inside a status list.
type ObjectRef struct {
	// Group is the API group; empty for the core group.
	// +optional
	Group string `json:"group,omitempty"`
	// Version is the object version.
	// +optional
	Version string `json:"version,omitempty"`
	// Kind is the object kind.
	Kind string `json:"kind"`
	// Namespace is the object namespace; empty for cluster-scoped objects.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Name is the object name.
	Name string `json:"name"`
}

// Visibility decides who sees an application in the panel; it does not gate
// access to its endpoints (C-59).
type Visibility struct {
	// Members lists local usernames that may see the application (C-59).
	// +optional
	Members []string `json:"members,omitempty"`
	// AllowGuests lets users without a role see the application (C-59).
	// +optional
	AllowGuests bool `json:"allowGuests,omitempty"`
}

// ApplicationSpec holds the user-owned desired state of an application.
type ApplicationSpec struct {
	// Owner is the local username of the creating session; the API writes it
	// and only the global Owner may transfer it (C-60).
	// +optional
	Owner string `json:"owner,omitempty"`
	// Visibility decides who sees the application in the panel (C-59).
	// +optional
	Visibility Visibility `json:"visibility,omitempty"`
	// DefinitionRef names the application definition the application was
	// installed from (C-32).
	// +optional
	DefinitionRef string `json:"definitionRef,omitempty"`
	// Namespace is the namespace the application declares for everything it
	// renders; it must already exist (C-32, C-54).
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// Config carries the values of the definition's config structure (C-32).
	// +optional
	Config runtime.RawExtension `json:"config,omitempty"`
	// Volumes names the volume objects whose claims the render references
	// (C-32, C-38).
	// +optional
	Volumes []string `json:"volumes,omitempty"`
}

// ApplicationList is the list form of Application.
//
// +kubebuilder:object:root=true
type ApplicationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Application `json:"items"`
}

// ApplicationDefinition is a cluster-scoped application definition.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=applicationdefinitions,scope=Cluster
type ApplicationDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the catalogue (C-03).
	// +optional
	Spec ApplicationDefinitionSpec `json:"spec,omitempty"`
}

// ApplicationDefinitionSpec holds the catalogue-owned definition: the package
// identity, the config structure that is the sole source of interface fields,
// the one-way translation template and the optional hooks (C-32).
type ApplicationDefinitionSpec struct {
	// Package identifies the packaged artifact the definition was installed
	// from (C-32).
	// +optional
	Package PackageIdentity `json:"package,omitempty"`
	// Config is the config structure the interface fields are generated from
	// (C-32).
	// +optional
	Config []ConfigField `json:"config,omitempty"`
	// Template is the one-way translation template; it is a pure function of
	// the application and its config (C-32).
	// +optional
	Template string `json:"template,omitempty"`
	// Hooks are optional lifecycle hooks; an exit status expresses the result
	// (C-32).
	// +optional
	Hooks []Hook `json:"hooks,omitempty"`
	// Chart references the chart the package ships (C-32).
	// +optional
	Chart string `json:"chart,omitempty"`
	// Requires lists the capability requirements of the definition (C-11).
	// +optional
	Requires []CapabilityRequirement `json:"requires,omitempty"`
}

// PackageIdentity is the identity of a packaged artifact (C-32).
type PackageIdentity struct {
	// Version is the package version.
	// +optional
	Version string `json:"version,omitempty"`
	// Digest is the package digest; the catalogue locks it instead of a tag
	// (C-32).
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ConfigField is one entry of a config structure. Type decides the value
// shape: string, integer, number, boolean or enum (C-32).
type ConfigField struct {
	// Name is the field name a submitted config uses.
	Name string `json:"name"`
	// Type is one of string, integer, number, boolean or enum (C-32).
	// +kubebuilder:validation:Enum=string;integer;number;boolean;enum
	Type string `json:"type"`
	// Required rejects a submission that omits the field.
	// +optional
	Required bool `json:"required,omitempty"`
	// Default is the textual default, interpreted by Type.
	// +optional
	Default *string `json:"default,omitempty"`
	// Minimum is the textual inclusive lower bound of an integer or number
	// field, interpreted by Type.
	// +optional
	Minimum *string `json:"minimum,omitempty"`
	// Maximum is the textual inclusive upper bound of an integer or number
	// field, interpreted by Type.
	// +optional
	Maximum *string `json:"maximum,omitempty"`
	// Enum lists the accepted values of an enum field.
	// +optional
	Enum []string `json:"enum,omitempty"`
	// Sensitive marks a value the read path masks; it is never returned to a
	// browser (C-31).
	// +optional
	Sensitive bool `json:"sensitive,omitempty"`
	// Description is the help text the interface shows.
	// +optional
	Description string `json:"description,omitempty"`
}

// Hook is one package hook; a zero exit status succeeds and any other value
// fails (C-32).
type Hook struct {
	// Name identifies the hook.
	Name string `json:"name"`
	// Command is the command the hook runs.
	// +optional
	Command []string `json:"command,omitempty"`
	// TimeoutSeconds bounds one run.
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// CapabilityRequirement is one capability an application needs (C-11).
type CapabilityRequirement struct {
	// Name is the capability name.
	Name string `json:"name"`
	// Version is the optional version range; empty matches any version.
	// +optional
	Version string `json:"version,omitempty"`
	// Guarantees are the guarantee values the capability must hold (C-11).
	// +optional
	Guarantees []GuaranteeRequirement `json:"guarantees,omitempty"`
	// Optional makes a failed requirement degrade the application instead of
	// keeping it Pending (C-11).
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// GuaranteeRequirement declares one guarantee value: a boolean must be equal,
// an enum must be equal or inside the declared set (C-11).
type GuaranteeRequirement struct {
	// Key is the guarantee key.
	Key string `json:"key"`
	// Bool is the required boolean value.
	// +optional
	Bool *bool `json:"bool,omitempty"`
	// Enum is the required enum value.
	// +optional
	Enum string `json:"enum,omitempty"`
	// OneOf is the set an enum value may come from.
	// +optional
	OneOf []string `json:"oneOf,omitempty"`
}

// ApplicationDefinitionList is the list form of ApplicationDefinition.
//
// +kubebuilder:object:root=true
type ApplicationDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ApplicationDefinition `json:"items"`
}

// Volume is a namespaced storage claim owned by KOS.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=volumes,scope=Namespaced
// +kubebuilder:subresource:status
type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the user (C-03).
	// +optional
	Spec VolumeSpec `json:"spec,omitempty"`
	// Status is written by the volume reconciler (C-03, C-07).
	// +optional
	Status ObjectStatus `json:"status,omitempty"`
}

// VolumeSpec holds the user-owned desired state of a volume.
type VolumeSpec struct {
	// Owner is the local username of the creating session; the API writes it
	// and only the global Owner may transfer it (C-60).
	// +optional
	Owner string `json:"owner,omitempty"`
}

// VolumeList is the list form of Volume.
//
// +kubebuilder:object:root=true
type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}

// Endpoint is a namespaced ingress into an application.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=endpoints,scope=Namespaced
// +kubebuilder:subresource:status
type Endpoint struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the user (C-03).
	// +optional
	Spec EndpointSpec `json:"spec,omitempty"`
	// Status is written by the endpoint reconciler (C-03, C-07, C-46).
	// +optional
	Status EndpointStatus `json:"status,omitempty"`
}

// EndpointStatus is the status of an endpoint: the two shared axes plus the
// certificate state C-46 makes a first-class item.
type EndpointStatus struct {
	ObjectStatus `json:",inline"`
	// Certificate is the certificate state of an endpoint that serves TLS
	// (C-46); it is absent when the endpoint serves no certificate.
	// +optional
	Certificate *EndpointCertificate `json:"certificate,omitempty"`
}

// EndpointCertificate is the observed certificate state of one endpoint
// (C-46): the issuer in use, whether a certificate covers the route now, and
// when it expires.
type EndpointCertificate struct {
	// Issuer is the resolved issuer identity: the spec's issuerRef, or the
	// distribution's reserved built-in issuer when the spec omits it (C-46).
	Issuer string `json:"issuer"`
	// Ready reports that a certificate covering now serves the route (C-46).
	Ready bool `json:"ready"`
	// NotAfter is the expiry of the served certificate (C-46).
	// +optional
	NotAfter *metav1.Time `json:"notAfter,omitempty"`
	// ExpiringSoon reports an expiry inside the warning window (C-46).
	// +optional
	ExpiringSoon bool `json:"expiringSoon,omitempty"`
	// Reason explains a certificate that is not ready (C-46).
	// +optional
	Reason string `json:"reason,omitempty"`
}

// EndpointKind is the form of an endpoint (C-43).
//
// +kubebuilder:validation:Enum=http.host;http.path;l4.tcp;l4.udp;mdns
type EndpointKind string

const (
	// EndpointKindHTTPHost exposes an application under a hostname (C-43).
	EndpointKindHTTPHost EndpointKind = "http.host"
	// EndpointKindHTTPPath exposes an application under a hostname and path (C-43).
	EndpointKindHTTPPath EndpointKind = "http.path"
	// EndpointKindTCP exposes a TCP port (C-43).
	EndpointKindTCP EndpointKind = "l4.tcp"
	// EndpointKindUDP exposes a UDP port (C-43).
	EndpointKindUDP EndpointKind = "l4.udp"
	// EndpointKindMDNS advertises an application over multicast DNS (C-43).
	EndpointKindMDNS EndpointKind = "mdns"
)

// EndpointSpec holds the user-owned desired state of an endpoint. The five
// shapes and the fields each accepts are C-43's matrix: every shape takes kind
// and targetApp, and a field the shape does not list is invalid.
//
// +kubebuilder:validation:XValidation:rule="self.targetApp == oldSelf.targetApp",message="spec.targetApp 创建后不可修改"
type EndpointSpec struct {
	// Kind selects the shape (C-43).
	Kind EndpointKind `json:"kind"`
	// TargetApp names the application the endpoint exposes; the reference is
	// fixed at creation and resolved in the endpoint's namespace (C-43, C-45).
	TargetApp string `json:"targetApp"`
	// Host is the hostname of an http.host or http.path endpoint (C-43).
	// +optional
	Host string `json:"host,omitempty"`
	// Path is the path prefix of an http.path endpoint (C-43).
	// +optional
	Path string `json:"path,omitempty"`
	// Port is the port of an l4.tcp, l4.udp or mdns endpoint (C-43).
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// ServiceName is the advertised name of an mdns endpoint (C-43).
	// +optional
	ServiceName string `json:"serviceName,omitempty"`
	// TLS names the certificate issuer of an http.host or http.path endpoint
	// (C-43, C-46).
	// +optional
	TLS *EndpointTLS `json:"tls,omitempty"`
	// Auth names the authentication provider of an http.host or http.path
	// endpoint (C-43, C-46).
	// +optional
	Auth *EndpointAuth `json:"auth,omitempty"`
}

// EndpointTLS is the certificate provider reference of an endpoint (C-46).
type EndpointTLS struct {
	// IssuerRef names the issuer component; empty means the distribution's
	// built-in issuer (C-46).
	// +optional
	IssuerRef string `json:"issuerRef,omitempty"`
}

// EndpointAuth is the authentication provider reference of an endpoint (C-46).
type EndpointAuth struct {
	// ProviderRef names the forward-auth provider (C-46).
	// +optional
	ProviderRef string `json:"providerRef,omitempty"`
}

// EndpointList is the list form of Endpoint.
//
// +kubebuilder:object:root=true
type EndpointList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Endpoint `json:"items"`
}

// Provider is a cluster-scoped capability provider registration.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=providers,scope=Cluster
// +kubebuilder:subresource:status
type Provider struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the registering component (C-03).
	// +optional
	Spec ProviderSpec `json:"spec,omitempty"`
	// Status is written by the validator (C-03, C-07).
	// +optional
	Status ObjectStatus `json:"status,omitempty"`
}

// ProviderSpec holds the registration declared by a system component.
type ProviderSpec struct{}

// ProviderList is the list form of Provider.
//
// +kubebuilder:object:root=true
type ProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Provider `json:"items"`
}

// SystemComponent is a cluster-scoped installed extension instance.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=systemcomponents,scope=Cluster
// +kubebuilder:subresource:status
type SystemComponent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the installer (C-03).
	// +optional
	Spec SystemComponentSpec `json:"spec,omitempty"`
	// Status is written by the component reconciler (C-03, C-07).
	// +optional
	Status ObjectStatus `json:"status,omitempty"`
}

// SystemComponentSpec holds the installation request of an extension.
type SystemComponentSpec struct {
	// Package is the artifact this component installs (C-17). An adopted
	// component installs no package and leaves it empty.
	// +optional
	Package ExtensionPackage `json:"package,omitempty"`
}

// ExtensionPackage identifies one extension package artifact and the digest it
// must match; a mismatch rejects the install (C-17).
type ExtensionPackage struct {
	// Reference locates the artifact.
	// +optional
	Reference string `json:"reference,omitempty"`
	// Digest is the artifact's sha256 in the form sha256:<hex>.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// SystemComponentList is the list form of SystemComponent.
//
// +kubebuilder:object:root=true
type SystemComponentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SystemComponent `json:"items"`
}

// BackupPolicy is a namespaced backup schedule.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=backuppolicies,scope=Namespaced
// +kubebuilder:subresource:status
type BackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the user (C-03).
	// +optional
	Spec BackupPolicySpec `json:"spec,omitempty"`
	// Status is written by the backup reconciler (C-03, C-07).
	// +optional
	Status ObjectStatus `json:"status,omitempty"`
}

// BackupPolicySpec holds the user-owned backup schedule.
type BackupPolicySpec struct {
	// Owner is the local username of the creating session; the API writes it
	// and only the global Owner may transfer it (C-60).
	// +optional
	Owner string `json:"owner,omitempty"`
}

// BackupPolicyList is the list form of BackupPolicy.
//
// +kubebuilder:object:root=true
type BackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupPolicy `json:"items"`
}

// RestoreJob is a namespaced restore run.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=restorejobs,scope=Namespaced
// +kubebuilder:subresource:status
type RestoreJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the user (C-03).
	// +optional
	Spec RestoreJobSpec `json:"spec,omitempty"`
	// Status is written by the restore reconciler (C-03, C-07).
	// +optional
	Status ObjectStatus `json:"status,omitempty"`
}

// RestoreJobSpec holds the user-owned restore request.
type RestoreJobSpec struct {
	// Owner is the local username of the creating session; the API writes it
	// and only the global Owner may transfer it (C-60).
	// +optional
	Owner string `json:"owner,omitempty"`
}

// RestoreJobList is the list form of RestoreJob.
//
// +kubebuilder:object:root=true
type RestoreJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RestoreJob `json:"items"`
}

// NodeDevice is a cluster-scoped hardware fact reported by discovery.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=nodedevices,scope=Cluster
type NodeDevice struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the fact owned by discovery (C-03).
	// +optional
	Spec NodeDeviceSpec `json:"spec,omitempty"`
}

// NodeDeviceSpec holds the device fact; it has no status writer (C-07).
type NodeDeviceSpec struct{}

// NodeDeviceList is the list form of NodeDevice.
//
// +kubebuilder:object:root=true
type NodeDeviceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []NodeDevice `json:"items"`
}

// VolumeMigration is a namespaced move of a volume to another backend.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:path=volumemigrations,scope=Namespaced
// +kubebuilder:subresource:status
type VolumeMigration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	// Spec is the desired state owned by the user (C-03).
	// +optional
	Spec VolumeMigrationSpec `json:"spec,omitempty"`
	// Status is written by the migration reconciler (C-03, C-07).
	// +optional
	Status ObjectStatus `json:"status,omitempty"`
}

// VolumeMigrationSpec holds the user-owned migration request.
type VolumeMigrationSpec struct {
	// Owner is the local username of the creating session; the API writes it
	// and only the global Owner may transfer it (C-60).
	// +optional
	Owner string `json:"owner,omitempty"`
}

// VolumeMigrationList is the list form of VolumeMigration.
//
// +kubebuilder:object:root=true
type VolumeMigrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeMigration `json:"items"`
}
