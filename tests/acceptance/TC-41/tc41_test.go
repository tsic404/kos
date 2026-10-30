// Package tc41 verifies C-48's deletion gate: a provider a volume and an
// endpoint still depend on stays in place, lists every dependent, and leaves
// only once they are gone (AC-21, TC-41).
package tc41

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/conformance"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/provider"
	"github.com/tsic404/kos/pkg/volume"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	providerName = "builtin-storage"
	storageClass = "local-path"
	provisioner  = "kos.test/local-path"
	endpointName = "web"
	volumeName   = "data"
)

// TestTC41DeletionBlockedByDependents deletes a provider that is still the
// issuer of an endpoint and the supplier of a bound volume.
func TestTC41DeletionBlockedByDependents(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
		},
	}), extension.MemoryArtifacts{})
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	harness.RequireProviderCondition(t, cluster.ProviderStatusOf(t, providerName), provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	// An endpoint naming the provider as its certificate issuer (C-46).
	endpoint := &unstructured.Unstructured{Object: harness.Manifest("Endpoint", harness.Namespace, endpointName,
		map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)}
	endpoint.Object["spec"] = map[string]any{
		"kind":      string(v1beta1.EndpointKindHTTPHost),
		"targetApp": "application",
		"host":      "web.test",
		"tls":       map[string]any{"issuerRef": providerName},
	}
	cluster.Create(t, endpoint)

	// A volume whose bound claim carries the provider's storage class (C-38,
	// C-48).
	cluster.Create(t, &unstructured.Unstructured{Object: harness.Manifest("Volume", harness.Namespace, volumeName,
		map[string]string{markers.ManagedBy: write.ManagedByValue}, nil)})
	cluster.CreateClaim(t, harness.Namespace, volumeName, storageClass)

	cluster.DeleteProvider(t, providerName)
	cluster.ValidateProvider(t, providerName, distribution, targets)

	if !cluster.ProviderExists(t, providerName) {
		t.Fatal("the provider disappeared although a volume and an endpoint still reference it")
	}
	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionDeprovisionBlocked, metav1.ConditionTrue, provider.ReasonDependents)
	// A blocked deletion does not change what the provider serves (C-47).
	harness.RequireProviderCondition(t, status, provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	dependents := harness.ReferenceNames(status.Dependents)
	if !slices.Contains(dependents, "Endpoint "+harness.Namespace+"/"+endpointName) {
		t.Fatalf("dependents = %v, want the endpoint", dependents)
	}
	if !slices.Contains(dependents, "Volume "+harness.Namespace+"/"+volumeName) {
		t.Fatalf("dependents = %v, want the volume", dependents)
	}

	// Dropping every dependent releases the gate; the next round lets the
	// object go.
	cluster.RemoveClaim(t, harness.Namespace, volumeName)
	cluster.DeleteObject(t, "v1beta1", "Volume", harness.Namespace, volumeName)
	cluster.DeleteObject(t, "v1beta1", "Endpoint", harness.Namespace, endpointName)
	cluster.ValidateProvider(t, providerName, distribution, targets)
	if cluster.ProviderExists(t, providerName) {
		t.Fatal("the provider is still present after its dependents were removed")
	}
}

// TestTC41DeletionBlockedByAnApplicationRequirement deletes a provider that no
// volume or endpoint references and that only an application definition's
// capability requirement depends on (C-48). The definition and the application
// installed from it must appear as dependents.
func TestTC41DeletionBlockedByAnApplicationRequirement(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
		},
	}), extension.MemoryArtifacts{})
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	harness.RequireProviderCondition(t, cluster.ProviderStatusOf(t, providerName), provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	const (
		definitionName  = "needs-volume"
		applicationName = "uses-volume"
	)
	cluster.InstallDefinition(t, harness.Definition(t, definitionName, []v1beta1.CapabilityRequirement{{
		Name: "storage.volume",
	}}))
	cluster.CreateApplication(t, applicationName, definitionName, harness.Namespace)

	cluster.DeleteProvider(t, providerName)
	cluster.ValidateProvider(t, providerName, distribution, targets)

	if !cluster.ProviderExists(t, providerName) {
		t.Fatal("the provider disappeared although an application definition requires its capability")
	}
	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionDeprovisionBlocked, metav1.ConditionTrue, provider.ReasonDependents)
	dependents := harness.ReferenceNames(status.Dependents)
	if !slices.Contains(dependents, "ApplicationDefinition "+definitionName) {
		t.Fatalf("dependents = %v, want the application definition requiring the capability", dependents)
	}
	if !slices.Contains(dependents, "Application "+harness.Namespace+"/"+applicationName) {
		t.Fatalf("dependents = %v, want the application installed from that definition", dependents)
	}
}

// TestTC41DeletionBlockedByAnInServiceCapability deletes a provider whose
// declaration no longer names a capability that a blocked withdrawal kept in
// service (C-48). The dependent list must come from the effective set, not from
// the declaration.
func TestTC41DeletionBlockedByAnInServiceCapability(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
		},
	}), artifacts)
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)
	harness.RequireProviderCondition(t, cluster.ProviderStatusOf(t, providerName), provider.ConditionReady, metav1.ConditionTrue, provider.ReasonReady)

	const (
		definitionName  = "needs-volume"
		applicationName = "uses-volume"
		appNamespace    = "apps"
	)
	cluster.EnsureNamespace(t, appNamespace)
	cluster.InstallDefinition(t, harness.Definition(t, definitionName, []v1beta1.CapabilityRequirement{{
		Name: "storage.volume",
	}}))
	cluster.CreateApplication(t, applicationName, definitionName, appNamespace)
	controller := cluster.ApplicationController(t, app.Options{Capabilities: provider.NewCatalogue(cluster.Writer())})
	if phase := cluster.SettleApplication(t, controller, applicationName, 5); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q", phase, app.PhaseReady)
	}

	// Withdrawing the capability is blocked: it stays in service although the
	// declaration no longer names it (C-48).
	cluster.UpgradePackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
	}), artifacts)
	cluster.ValidateProvider(t, providerName, distribution, targets)

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionUpgradeBlocked, metav1.ConditionTrue, provider.ReasonWithdrawn)
	if spec := cluster.ProviderSpecOf(t, providerName); len(spec.Capabilities) != 0 {
		t.Fatalf("declared capabilities = %v, want none after the withdrawal", spec.Capabilities)
	}
	if len(status.EffectiveCapabilities) != 1 || status.EffectiveCapabilities[0].Name != "storage.volume" {
		t.Fatalf("effective capabilities = %v, want the withdrawn storage.volume still in service", status.EffectiveCapabilities)
	}

	cluster.DeleteProvider(t, providerName)
	cluster.ValidateProvider(t, providerName, distribution, targets)

	if !cluster.ProviderExists(t, providerName) {
		t.Fatal("the provider disappeared although its in-service capability is still required")
	}
	status = cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionDeprovisionBlocked, metav1.ConditionTrue, provider.ReasonDependents)
	dependents := harness.ReferenceNames(status.Dependents)
	if !slices.Contains(dependents, "ApplicationDefinition "+definitionName) {
		t.Fatalf("dependents = %v, want the definition requiring the in-service capability", dependents)
	}
	if !slices.Contains(dependents, "Application "+harness.Namespace+"/"+applicationName) {
		t.Fatalf("dependents = %v, want the application consuming it", dependents)
	}
	if phase := cluster.ApplicationPhase(t, applicationName); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q after the blocked deletion", phase, app.PhaseReady)
	}
}

// TestTC41DeletionIgnoresNeverServedDeclarations deletes a provider whose
// registration was refused on its first round: the capability it declares never
// entered service, so a definition requiring it is not a dependent (C-48).
func TestTC41DeletionIgnoresNeverServedDeclarations(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	cluster.CreateStorageClass(t, storageClass, provisioner)

	// A storage capability without a storage class: the registration is
	// refused, so the entry never enters service.
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
		},
	}), extension.MemoryArtifacts{})
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)

	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionRegistered, metav1.ConditionFalse, provider.ReasonInvalidRegistration)
	if len(status.EffectiveCapabilities) != 0 {
		t.Fatalf("effective capabilities = %v, want none: the registration was refused", status.EffectiveCapabilities)
	}
	if spec := cluster.ProviderSpecOf(t, providerName); len(spec.Capabilities) != 1 {
		t.Fatalf("declared capabilities = %v, want the refused entry", spec.Capabilities)
	}
	cluster.InstallDefinition(t, harness.Definition(t, "needs-volume", []v1beta1.CapabilityRequirement{{
		Name: "storage.volume",
	}}))

	cluster.DeleteProvider(t, providerName)
	cluster.ValidateProvider(t, providerName, distribution, targets)

	if cluster.ProviderExists(t, providerName) {
		t.Fatalf("a never-served declaration blocked the deletion: %v", cluster.ProviderStatusOf(t, providerName).Dependents)
	}
}

// TestTC41DeletionBlockedByResolvedVolumeAndEndpoint deletes providers whose
// only dependents are resolved edges: volumes the volume reconciler resolved to
// a backend a provider supplies, and an endpoint whose status resolved the
// issuer its spec omitted. The volume bound through the provider's own class is
// matched by the class edge and the backend edge at once and is still listed
// once (C-38, C-46, C-48).
func TestTC41DeletionBlockedByResolvedVolumeAndEndpoint(t *testing.T) {
	cluster := harness.Start(t)
	distribution := conformance.Standard()
	// The volume status records the backend the binding landed on: the
	// persistent volume's driver wins over the class provisioner (C-38).
	const backend = "storage.example"
	cluster.CreateStorageClass(t, storageClass, backend)
	// A second class on the same backend that the provider does not declare:
	// only the resolved-backend edge can see a volume bound through it.
	const copyClass = "local-copy"
	cluster.CreateStorageClass(t, copyClass, backend)

	const (
		archiveVolume   = "archive"
		mirrorVolume    = "mirror"
		apiEndpoint     = "api"
		definitionName  = "needs-resolved-storage"
		applicationName = "uses-resolved-storage"
	)
	cluster.InstallPackage(t, providerName, harness.ProviderDeclaration(providerName, "1.0.0", extension.CapabilityContributions{
		StorageClasses: []string{storageClass},
		Items: []extension.Capability{
			harness.CapabilityEntry(t, distribution, "storage.volume", "v1", "kos/conformance/storage-volume-v1"),
		},
	}), extension.MemoryArtifacts{})
	targets := harness.ProviderTargets(map[string]conformance.Target{providerName: harness.CaseTarget()})
	cluster.ValidateProvider(t, providerName, distribution, targets)

	controller := cluster.VolumeController(t, volume.Options{})
	installBoundVolume(t, cluster, controller, archiveVolume, storageClass, backend, "pv-archive")
	installBoundVolume(t, cluster, controller, mirrorVolume, copyClass, backend, "pv-mirror")

	cluster.InstallDefinition(t, harness.Definition(t, definitionName, []v1beta1.CapabilityRequirement{{
		Name: "storage.volume",
	}}))
	cluster.CreateApplication(t, applicationName, definitionName, harness.Namespace)

	// An endpoint that declares TLS without an issuer reference: only the
	// issuer the reconciler resolves into its status binds it (C-46).
	cluster.Create(t, harness.Application("application", nil))
	cluster.Create(t, harness.Endpoint(apiEndpoint, harness.ManagedLabels(), map[string]any{
		"kind":      string(v1beta1.EndpointKindHTTPHost),
		"targetApp": "application",
		"host":      "api.test",
		"tls":       map[string]any{},
	}))
	harness.ReconcileEndpoint(t, endpoint.NewReconciler(cluster.Writer(), nil), apiEndpoint)
	resolved := cluster.Get(t, "v1beta1", "Endpoint", apiEndpoint)
	issuer, _, _ := unstructured.NestedString(resolved.Object, "status", "certificate", "issuer")
	if issuer != endpoint.DefaultIssuer {
		t.Fatalf("endpoint resolved issuer %q, want %q", issuer, endpoint.DefaultIssuer)
	}

	// The component behind that resolved issuer registers a provider of its
	// own: it contributes no capability and still is a registration (C-47).
	cluster.InstallPackage(t, endpoint.DefaultIssuer, harness.ProviderDeclaration(endpoint.DefaultIssuer, "1.0.0", extension.CapabilityContributions{}), extension.MemoryArtifacts{})
	issuerTargets := harness.ProviderTargets(map[string]conformance.Target{endpoint.DefaultIssuer: harness.CaseTarget()})
	cluster.ValidateProvider(t, endpoint.DefaultIssuer, distribution, issuerTargets)

	cluster.DeleteProvider(t, providerName)
	cluster.ValidateProvider(t, providerName, distribution, targets)
	if !cluster.ProviderExists(t, providerName) {
		t.Fatal("the provider disappeared although the backend it supplies still backs two bound volumes")
	}
	status := cluster.ProviderStatusOf(t, providerName)
	harness.RequireProviderCondition(t, status, provider.ConditionDeprovisionBlocked, metav1.ConditionTrue, provider.ReasonDependents)
	dependents := harness.ReferenceNames(status.Dependents)
	want := []string{
		"Application " + harness.Namespace + "/" + applicationName,
		"ApplicationDefinition " + definitionName,
		"Volume " + harness.Namespace + "/" + archiveVolume,
		"Volume " + harness.Namespace + "/" + mirrorVolume,
	}
	if !slices.Equal(dependents, want) {
		t.Fatalf("dependents = %v, want %v", dependents, want)
	}
	listed := 0
	for _, dependent := range dependents {
		if dependent == "Volume "+harness.Namespace+"/"+archiveVolume {
			listed++
		}
	}
	if listed != 1 {
		t.Fatalf("the volume matched by the class edge and the backend edge is listed %d times: %v", listed, dependents)
	}

	cluster.DeleteProvider(t, endpoint.DefaultIssuer)
	cluster.ValidateProvider(t, endpoint.DefaultIssuer, distribution, issuerTargets)
	if !cluster.ProviderExists(t, endpoint.DefaultIssuer) {
		t.Fatal("the provider disappeared although an endpoint's resolved issuer names it")
	}
	issuerStatus := cluster.ProviderStatusOf(t, endpoint.DefaultIssuer)
	harness.RequireProviderCondition(t, issuerStatus, provider.ConditionDeprovisionBlocked, metav1.ConditionTrue, provider.ReasonDependents)
	if got := harness.ReferenceNames(issuerStatus.Dependents); !slices.Equal(got, []string{"Endpoint " + harness.Namespace + "/" + apiEndpoint}) {
		t.Fatalf("dependents = %v, want the endpoint whose status resolved the issuer", got)
	}
}

// installBoundVolume drives one volume through the real reconciler until its
// claim is bound to a persistent volume on driver, then checks the backend its
// status records (C-38).
func installBoundVolume(t *testing.T, cluster *harness.Cluster, controller *volume.Controller, name, class, driver, persistentVolume string) {
	t.Helper()
	cluster.Create(t, harness.Volume(harness.Namespace, name, map[string]any{
		"mobility":     string(v1beta1.VolumeMobilityClusterMovable),
		"capacity":     "1Gi",
		"storageClass": class,
	}))
	harness.ReconcileVolume(t, controller, harness.Namespace, name)
	cluster.PersistentVolume(t, persistentVolume, class, driver, "Retain")
	cluster.BindClaim(t, harness.Namespace, name, persistentVolume)
	harness.ReconcileVolume(t, controller, harness.Namespace, name)

	observed := cluster.Get(t, "v1beta1", "Volume", name)
	backend, _, _ := unstructured.NestedString(observed.Object, "status", "provider", "name")
	if backend != driver {
		t.Fatalf("volume %s resolved backend %q, want %q", name, backend, driver)
	}
}
