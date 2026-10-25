// Package v1alpha1 registers the first served version of the KOS API group.
//
// +kubebuilder:object:generate=true
// +groupName=kos.tsic.top
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is the group and version served by this package.
	GroupVersion = schema.GroupVersion{Group: "kos.tsic.top", Version: "v1alpha1"}

	// SchemeBuilder registers this version's types into a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds this version's types to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&Application{}, &ApplicationList{},
		&ApplicationDefinition{}, &ApplicationDefinitionList{},
		&Volume{}, &VolumeList{},
		&Endpoint{}, &EndpointList{},
		&Provider{}, &ProviderList{},
		&SystemComponent{}, &SystemComponentList{},
		&BackupPolicy{}, &BackupPolicyList{},
		&RestoreJob{}, &RestoreJobList{},
		&NodeDevice{}, &NodeDeviceList{},
		&VolumeMigration{}, &VolumeMigrationList{},
	)
}
