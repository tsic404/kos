package install

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apiextensionsclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/adoption"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/markers"
)

// Leftover categories of the uninstall manifest (C-54). Every uninstall reports
// the six categories as independent lists; a category with no entries stays
// empty rather than disappearing.
const (
	// LeftoverReleasedClaim is a claim the uninstall leaves in place.
	LeftoverReleasedClaim = "released-claim"
	// LeftoverPersistentVolume is a volume backing such a claim.
	LeftoverPersistentVolume = "persistent-volume"
	// LeftoverInstance is a kernel instance KOS does not own, such as an
	// adopted object.
	LeftoverInstance = "instance"
	// LeftoverClusterPermission is a cluster-scoped permission object that
	// grants someone outside KOS rights over the installation.
	LeftoverClusterPermission = "cluster-permission"
	// LeftoverAdoptionSnapshot is the immutable pre-adoption record.
	LeftoverAdoptionSnapshot = "adoption-snapshot"
	// LeftoverExtensionData is retained or released data an extension declared.
	LeftoverExtensionData = "extension-data"
)

// Dispositions of one leftover entry.
const (
	// DispositionRetained marks data the uninstall keeps in place.
	DispositionRetained = "retained"
	// DispositionReleased marks a resource whose reference was released without
	// deleting it.
	DispositionReleased = "released"
)

// Categories lists the six leftover categories in manifest order.
func Categories() []string {
	return []string{
		LeftoverReleasedClaim, LeftoverPersistentVolume, LeftoverInstance,
		LeftoverClusterPermission, LeftoverAdoptionSnapshot, LeftoverExtensionData,
	}
}

// Leftover is one entry of the uninstall manifest.
type Leftover struct {
	// Category is one of the six categories.
	Category string `json:"category"`
	// Object is the object the entry names.
	Object v1beta1.ObjectRef `json:"object"`
	// Disposition is retained or released.
	Disposition string `json:"disposition"`
}

// Manifest is what an uninstall leaves behind (C-54): the operator's record of
// the data and rights the cluster still holds.
type Manifest struct {
	// GeneratedAt is when the uninstall finished.
	GeneratedAt time.Time `json:"generatedAt"`
	// Entries lists the leftovers, ordered by category and object.
	Entries []Leftover `json:"leavings"`
}

// ByCategory returns the entries of one category, keeping the manifest order.
func (m Manifest) ByCategory(category string) []Leftover {
	entries := make([]Leftover, 0)
	for _, entry := range m.Entries {
		if entry.Category == category {
			entries = append(entries, entry)
		}
	}
	return entries
}

// DataDisposer removes the installation's data through the volume path when the
// uninstall does not keep it. The uninstaller never writes a claim itself
// (C-04); the owner of that write performs the removal.
type DataDisposer interface {
	Dispose(ctx context.Context) error
}

// UninstallConfig wires one uninstallation.
type UninstallConfig struct {
	// Namespace is the installation's own namespace; empty selects
	// DefaultNamespace.
	Namespace string
	// RetainData keeps the installation's claims and volumes. Without it the
	// uninstall needs a DataDisposer, because data removal is destructive.
	RetainData bool
	// Dispose removes the data when RetainData is false.
	Dispose DataDisposer
	// Dynamic executes the uninstall deletions as the installer identity.
	Dynamic dynamic.Interface
	// Kube reads claims, volumes, permissions and namespace-scoped records.
	Kube kubernetes.Interface
	// APIExtensions removes the object definitions.
	APIExtensions apiextensionsclientset.Interface
	// Discovery names the kernel resources to scan.
	Discovery discovery.DiscoveryInterface
	// Artifacts reads installed components' declarations, so the classes they
	// contribute are scanned without a hand-maintained list (C-19); nil uses
	// ExtensionClasses.
	Artifacts extension.ArtifactSource
	// ExtensionClasses is the fallback class list when Artifacts is nil or a
	// declaration cannot be read.
	ExtensionClasses []schema.GroupKind
	// Now supplies the manifest time; nil means the wall clock.
	Now func() time.Time
}

// Uninstaller removes one installation (C-54).
type Uninstaller struct {
	config UninstallConfig
	// instanceResources remembers the resource of every scanned kind, so the
	// post-deletion recheck does not depend on discovery that the deleted
	// definitions just shrank.
	instanceResources map[schema.GroupKind]schema.GroupVersionResource
	// classes are the extension classes this uninstall scans, derived from the
	// installed components with the configured list as fallback.
	classes []schema.GroupKind
}

// NewUninstaller validates the wiring of one uninstallation.
func NewUninstaller(config UninstallConfig) (*Uninstaller, error) {
	if config.Namespace == "" {
		config.Namespace = DefaultNamespace
	}
	switch {
	case config.Dynamic == nil:
		return nil, errors.New("uninstall needs a dynamic client")
	case config.Kube == nil:
		return nil, errors.New("uninstall needs a kubernetes client")
	case config.APIExtensions == nil:
		return nil, errors.New("uninstall needs an apiextensions client")
	case config.Discovery == nil:
		return nil, errors.New("uninstall needs discovery")
	case !config.RetainData && config.Dispose == nil:
		return nil, errors.New("an uninstall that removes data needs a data disposer")
	}
	return &Uninstaller{config: config}, nil
}

// Uninstall stops the control plane, releases finalizers, settles the data,
// removes the control plane and the object definitions, and returns the
// leftover manifest.
func (u *Uninstaller) Uninstall(ctx context.Context) (Manifest, error) {
	manifest := Manifest{GeneratedAt: u.now(), Entries: []Leftover{}}
	classes, err := u.extensionClasses(ctx)
	if err != nil {
		return manifest, err
	}
	u.classes = classes
	if err := u.stopControlPlane(ctx); err != nil {
		return manifest, err
	}
	if err := u.releaseFinalizers(ctx); err != nil {
		return manifest, err
	}
	entries, err := u.collect(ctx)
	if err != nil {
		return manifest, err
	}
	if !u.config.RetainData {
		if err := u.config.Dispose.Dispose(ctx); err != nil {
			return manifest, fmt.Errorf("dispose installation data: %w", err)
		}
	}
	// The data entries describe what the retained-data option leaves behind,
	// so they are read after the disposal ran.
	dataEntries, err := u.collectData(ctx)
	if err != nil {
		return manifest, err
	}
	entries = append(entries, dataEntries...)
	if err := u.deletePermissions(ctx); err != nil {
		return manifest, err
	}
	if err := u.deleteDefinitions(ctx); err != nil {
		return manifest, err
	}
	// Deleting a definition removes its instances. The manifest must name what
	// still exists afterwards, so an adopted kernel instance that disappeared
	// with its definition is not reported as retained (C-54).
	entries, err = u.pruneInstances(ctx, entries)
	if err != nil {
		return manifest, err
	}
	sortEntries(entries)
	manifest.Entries = entries
	return manifest, nil
}

// collectData lists the claims and volumes the uninstall leaves in place.
func (u *Uninstaller) collectData(ctx context.Context) ([]Leftover, error) {
	claims, err := u.claims(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]Leftover, 0)
	for index := range claims {
		claim := &claims[index]
		if !isReleased(claim) && claim.GetLabels()[markers.ManagedBy] == "" {
			continue
		}
		entries = append(entries, Leftover{
			Category:    LeftoverReleasedClaim,
			Object:      objectRef(claimResource, "PersistentVolumeClaim", claim),
			Disposition: DispositionRetained,
		})
	}
	volumes, err := u.volumes(ctx)
	if err != nil {
		return nil, err
	}
	volumesByName := make(map[string]*unstructured.Unstructured, len(volumes))
	for index := range volumes {
		volumesByName[volumes[index].GetName()] = &volumes[index]
	}
	for index := range claims {
		claim := &claims[index]
		if !isReleased(claim) {
			continue
		}
		volume, found := volumesByName[claimVolume(claim)]
		if !found {
			continue
		}
		entries = append(entries, Leftover{
			Category:    LeftoverPersistentVolume,
			Object:      objectRef(volumeResource, "PersistentVolume", volume),
			Disposition: DispositionRetained,
		})
	}
	for index := range volumes {
		volume := &volumes[index]
		if volume.GetLabels()[markers.ManagedBy] == "" || listed(entries, LeftoverPersistentVolume, volume.GetName()) {
			continue
		}
		entries = append(entries, Leftover{
			Category:    LeftoverPersistentVolume,
			Object:      objectRef(volumeResource, "PersistentVolume", volume),
			Disposition: DispositionRetained,
		})
	}
	return entries, nil
}

// collect gathers everything the manifest reports except the data entries. It
// runs before the object definitions are removed, because the instances
// disappear with them.
func (u *Uninstaller) collect(ctx context.Context) ([]Leftover, error) {
	entries := make([]Leftover, 0)
	instances, err := u.instances(ctx)
	if err != nil {
		return nil, err
	}
	entries = append(entries, instances...)
	permissions, err := u.clusterPermissions(ctx)
	if err != nil {
		return nil, err
	}
	entries = append(entries, permissions...)
	snapshots, err := u.adoptionSnapshots(ctx)
	if err != nil {
		return nil, err
	}
	entries = append(entries, snapshots...)
	extensionData, err := u.extensionData(ctx)
	if err != nil {
		return nil, err
	}
	entries = append(entries, extensionData...)
	return entries, nil
}

var (
	claimResource  = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumeclaims"}
	volumeResource = schema.GroupVersionResource{Version: "v1", Resource: "persistentvolumes"}
)

// listed reports whether the manifest already names one object of a category.
func listed(entries []Leftover, category, name string) bool {
	for _, entry := range entries {
		if entry.Category == category && entry.Object.Name == name {
			return true
		}
	}
	return false
}

// sortEntries orders the manifest by category and object identity.
func sortEntries(entries []Leftover) {
	order := make(map[string]int, len(Categories()))
	for index, category := range Categories() {
		order[category] = index
	}
	sort.SliceStable(entries, func(left, right int) bool {
		if entries[left].Category != entries[right].Category {
			return order[entries[left].Category] < order[entries[right].Category]
		}
		if entries[left].Object.Namespace != entries[right].Object.Namespace {
			return entries[left].Object.Namespace < entries[right].Object.Namespace
		}
		if entries[left].Object.Kind != entries[right].Object.Kind {
			return entries[left].Object.Kind < entries[right].Object.Kind
		}
		return entries[left].Object.Name < entries[right].Object.Name
	})
}

// stopControlPlane removes the control-plane workload so nothing reconciles
// while the uninstall changes objects (C-54).
func (u *Uninstaller) stopControlPlane(ctx context.Context) error {
	objects := []struct {
		gvr  schema.GroupVersionResource
		name string
	}{
		{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, ControlPlaneName},
		{schema.GroupVersionResource{Version: "v1", Resource: "services"}, ControlPlaneName},
		{schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}, ControlPlaneServiceAccount},
	}
	for _, object := range objects {
		writable := u.config.Dynamic.Resource(object.gvr).Namespace(u.config.Namespace)
		err := writable.Delete(ctx, object.name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("stop %s %s: %w", object.gvr.Resource, object.name, err)
		}
	}
	return u.waitDeploymentGone(ctx)
}

// waitDeploymentGone waits until the control-plane deployment is gone.
func (u *Uninstaller) waitDeploymentGone(ctx context.Context) error {
	resource := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	writable := u.config.Dynamic.Resource(resource).Namespace(u.config.Namespace)
	deadline := time.Now().Add(operationTimeout)
	for {
		_, err := writable.Get(ctx, ControlPlaneName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read deployment %s: %w", ControlPlaneName, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deployment %s did not stop within %s", ControlPlaneName, operationTimeout)
		}
		if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
}

// releaseFinalizers strips the finalizers KOS puts on kernel objects, so their
// definitions can be removed (C-33, C-54).
func (u *Uninstaller) releaseFinalizers(ctx context.Context) error {
	resources, err := reconcilableResources(u.config.Discovery, nil)
	if err != nil {
		return err
	}
	for _, resource := range resources {
		items, err := listResource(ctx, u.config.Dynamic, resource.GVR, resource.Namespaced, "")
		if err != nil {
			return err
		}
		for index := range items {
			item := &items[index]
			finalizers := item.GetFinalizers()
			if len(finalizers) == 0 {
				continue
			}
			kept := make([]string, 0, len(finalizers))
			for _, finalizer := range finalizers {
				if strings.HasPrefix(finalizer, "kos.tsic.top/") {
					continue
				}
				kept = append(kept, finalizer)
			}
			if len(kept) == len(finalizers) {
				continue
			}
			item.SetFinalizers(kept)
			var writable dynamic.ResourceInterface = u.config.Dynamic.Resource(resource.GVR)
			if item.GetNamespace() != "" {
				writable = u.config.Dynamic.Resource(resource.GVR).Namespace(item.GetNamespace())
			}
			if _, err := writable.Update(ctx, item, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("release finalizers of %s %s: %w", resource.Kind, item.GetName(), err)
			}
		}
	}
	return nil
}

// claims lists every claim in the cluster.
func (u *Uninstaller) claims(ctx context.Context) ([]unstructured.Unstructured, error) {
	return listResource(ctx, u.config.Dynamic, claimResource, true, "")
}

// volumes lists every persistent volume.
func (u *Uninstaller) volumes(ctx context.Context) ([]unstructured.Unstructured, error) {
	return listResource(ctx, u.config.Dynamic, volumeResource, false, "")
}

// isReleased reports the release marker on one claim (C-02, C-39).
func isReleased(claim *unstructured.Unstructured) bool {
	_, found := claim.GetLabels()[markers.Released]
	return found
}

// claimVolume reads the volume a claim is bound to.
func claimVolume(claim *unstructured.Unstructured) string {
	volume, _, _ := unstructured.NestedString(claim.Object, "spec", "volumeName")
	return volume
}

// instances lists the objects KOS does not own, across the kernel and the
// installed extension classes: an adopted object keeps its record while the
// kernel definitions go (C-54, C-19).
func (u *Uninstaller) instances(ctx context.Context) ([]Leftover, error) {
	resources, err := reconcilableResources(u.config.Discovery, u.classes)
	if err != nil {
		return nil, err
	}
	entries := make([]Leftover, 0)
	u.instanceResources = make(map[schema.GroupKind]schema.GroupVersionResource, len(resources))
	for _, resource := range resources {
		u.instanceResources[schema.GroupKind{Group: resource.GVR.Group, Kind: resource.Kind}] = resource.GVR
		items, err := listResource(ctx, u.config.Dynamic, resource.GVR, resource.Namespaced, "")
		if err != nil {
			return nil, err
		}
		for index := range items {
			item := &items[index]
			attribution, err := markers.AttributionOf(item.GetLabels())
			if err != nil || attribution != markers.AttributionAdopted {
				continue
			}
			entries = append(entries, Leftover{
				Category:    LeftoverInstance,
				Object:      objectRef(resource.GVR, resource.Kind, item),
				Disposition: DispositionRetained,
			})
		}
	}
	return entries, nil
}

// clusterPermissions lists the cluster-scoped permission objects that grant
// someone outside KOS rights over the installation (C-54). A role an outside
// binding still names is kept, so the binding never points at a missing role.
func (u *Uninstaller) clusterPermissions(ctx context.Context) ([]Leftover, error) {
	managedRoles, externalBindings, retainedRoles, err := u.permissionState(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]Leftover, 0)
	for name := range externalBindings {
		entries = append(entries, Leftover{
			Category: LeftoverClusterPermission,
			Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding", Name: name,
			},
			Disposition: DispositionRetained,
		})
	}
	for role := range managedRoles {
		if !retainedRoles[role] {
			continue
		}
		entries = append(entries, Leftover{
			Category: LeftoverClusterPermission,
			Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole", Name: role,
			},
			Disposition: DispositionRetained,
		})
	}
	return entries, nil
}

// permissionState names the installation's cluster roles, the outside bindings
// over them and the roles those bindings keep alive.
func (u *Uninstaller) permissionState(ctx context.Context) (map[string]bool, map[string]bool, map[string]bool, error) {
	roles, err := u.config.Kube.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list cluster roles: %w", err)
	}
	managedRoles := make(map[string]bool)
	for index := range roles.Items {
		role := &roles.Items[index]
		if role.Labels[markers.ManagedBy] != "" || role.Name == ControlPlaneName {
			managedRoles[role.Name] = true
		}
	}
	bindings, err := u.config.Kube.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("list cluster role bindings: %w", err)
	}
	externalBindings := make(map[string]bool)
	retainedRoles := make(map[string]bool)
	for index := range bindings.Items {
		binding := &bindings.Items[index]
		if binding.Labels[markers.ManagedBy] != "" || binding.Name == ControlPlaneName {
			continue
		}
		if !managedRoles[binding.RoleRef.Name] {
			continue
		}
		externalBindings[binding.Name] = true
		retainedRoles[binding.RoleRef.Name] = true
	}
	return managedRoles, externalBindings, retainedRoles, nil
}

// pruneInstances drops the instance entries whose object no longer exists after
// the definitions were removed (C-54).
func (u *Uninstaller) pruneInstances(ctx context.Context, entries []Leftover) ([]Leftover, error) {
	kept := make([]Leftover, 0, len(entries))
	for _, entry := range entries {
		if entry.Category != LeftoverInstance {
			kept = append(kept, entry)
			continue
		}
		resource, known := u.instanceResources[schema.GroupKind{Group: entry.Object.Group, Kind: entry.Object.Kind}]
		if !known {
			// The object was never readable as a resource; it cannot be claimed
			// as a leftover either.
			continue
		}
		var readable dynamic.ResourceInterface = u.config.Dynamic.Resource(resource)
		if entry.Object.Namespace != "" {
			readable = u.config.Dynamic.Resource(resource).Namespace(entry.Object.Namespace)
		}
		if _, err := readable.Get(ctx, entry.Object.Name, metav1.GetOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read leftover %s %s: %w", entry.Object.Kind, entry.Object.Name, err)
		}
		kept = append(kept, entry)
	}
	return kept, nil
}

// adoptionSnapshots lists the immutable pre-adoption records (C-02, C-54).
func (u *Uninstaller) adoptionSnapshots(ctx context.Context) ([]Leftover, error) {
	records, err := u.config.Kube.CoreV1().ConfigMaps(u.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: adoption.RecordLabel + "=" + adoption.RecordValue,
	})
	if err != nil {
		return nil, fmt.Errorf("list adoption snapshots: %w", err)
	}
	entries := make([]Leftover, 0, len(records.Items))
	for index := range records.Items {
		record := &records.Items[index]
		entries = append(entries, Leftover{
			Category: LeftoverAdoptionSnapshot,
			Object: v1beta1.ObjectRef{
				Version: "v1", Kind: "ConfigMap", Namespace: record.Namespace, Name: record.Name,
			},
			Disposition: DispositionRetained,
		})
	}
	return entries, nil
}

// extensionData lists the retained and released data extension declarations
// recorded on uninstall (C-29, C-54).
func (u *Uninstaller) extensionData(ctx context.Context) ([]Leftover, error) {
	records, err := u.config.Kube.CoreV1().ConfigMaps(u.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: extension.ResidualLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("list extension residual manifests: %w", err)
	}
	entries := make([]Leftover, 0)
	for index := range records.Items {
		record := &records.Items[index]
		residuals, err := extension.DecodeResiduals(record.Data[extension.ResidualKey])
		if err != nil {
			return nil, fmt.Errorf("read residual manifest %s: %w", record.Name, err)
		}
		for _, residual := range residuals {
			entries = append(entries, Leftover{
				Category:    LeftoverExtensionData,
				Object:      residual.Object,
				Disposition: residual.Disposition,
			})
		}
	}
	return entries, nil
}

// deletePermissions removes the cluster-scoped permission objects the
// installation owns. A binding that grants an outside subject is left and
// reported instead.
func (u *Uninstaller) deletePermissions(ctx context.Context) error {
	bindings, err := u.config.Kube.RbacV1().ClusterRoleBindings().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list cluster role bindings: %w", err)
	}
	for index := range bindings.Items {
		binding := &bindings.Items[index]
		if binding.Labels[markers.ManagedBy] == "" && binding.Name != ControlPlaneName {
			continue
		}
		if err := u.config.Kube.RbacV1().ClusterRoleBindings().Delete(ctx, binding.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete cluster role binding %s: %w", binding.Name, err)
		}
	}
	roles, err := u.config.Kube.RbacV1().Roles(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list roles: %w", err)
	}
	for index := range roles.Items {
		role := &roles.Items[index]
		if role.Labels[markers.ManagedBy] == "" && role.Name != ControlPlaneName {
			continue
		}
		if err := u.config.Kube.RbacV1().Roles(role.Namespace).Delete(ctx, role.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete role %s/%s: %w", role.Namespace, role.Name, err)
		}
	}
	bindingsInNamespaces, err := u.config.Kube.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list role bindings: %w", err)
	}
	for index := range bindingsInNamespaces.Items {
		binding := &bindingsInNamespaces.Items[index]
		if binding.Labels[markers.ManagedBy] == "" && binding.Name != ControlPlaneName {
			continue
		}
		if err := u.config.Kube.RbacV1().RoleBindings(binding.Namespace).Delete(ctx, binding.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete role binding %s/%s: %w", binding.Namespace, binding.Name, err)
		}
	}
	managedRoles, _, retained, err := u.permissionState(ctx)
	if err != nil {
		return err
	}
	for role := range managedRoles {
		// A role an outside binding still names stays, so the reported binding
		// keeps its grant instead of dangling (C-54).
		if retained[role] {
			continue
		}
		if err := u.config.Kube.RbacV1().ClusterRoles().Delete(ctx, role, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete cluster role %s: %w", role, err)
		}
	}
	return nil
}

// deleteDefinitions removes the object definitions of the kernel group.
func (u *Uninstaller) deleteDefinitions(ctx context.Context) error {
	definitions, err := u.config.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list object definitions: %w", err)
	}
	for index := range definitions.Items {
		definition := &definitions.Items[index]
		if definition.Spec.Group != api.Group {
			continue
		}
		err := u.config.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().Delete(ctx, definition.Name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete object definition %s: %w", definition.Name, err)
		}
		if err := u.waitDefinitionGone(ctx, definition.Name); err != nil {
			return err
		}
	}
	return nil
}

// waitDefinitionGone waits until one definition is removed.
func (u *Uninstaller) waitDefinitionGone(ctx context.Context, name string) error {
	deadline := time.Now().Add(operationTimeout)
	for {
		_, err := u.config.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read object definition %s: %w", name, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("object definition %s did not disappear within %s", name, operationTimeout)
		}
		if err := sleepContext(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
}

// objectRef builds the reference of one listed object.
func objectRef(resource schema.GroupVersionResource, kind string, object *unstructured.Unstructured) v1beta1.ObjectRef {
	return v1beta1.ObjectRef{
		Group:     resource.Group,
		Version:   resource.Version,
		Kind:      kind,
		Namespace: object.GetNamespace(),
		Name:      object.GetName(),
	}
}

// extensionClasses derives the classes from the installed components; the
// configured list is the fallback. A derivation that fails without a fallback
// fails the uninstall: the leftover list cannot silently drop extension data.
func (u *Uninstaller) extensionClasses(ctx context.Context) ([]schema.GroupKind, error) {
	configured := append([]schema.GroupKind{}, u.config.ExtensionClasses...)
	if u.config.Artifacts == nil {
		return configured, nil
	}
	discovered, err := ComponentClasses(ctx, u.config.Dynamic, u.config.Artifacts)
	if err != nil {
		if len(configured) > 0 {
			return configured, nil
		}
		return nil, fmt.Errorf("read installed extension classes: %w", err)
	}
	seen := make(map[schema.GroupKind]bool, len(configured)+len(discovered))
	merged := make([]schema.GroupKind, 0, len(configured)+len(discovered))
	for _, class := range append(discovered, configured...) {
		if seen[class] {
			continue
		}
		seen[class] = true
		merged = append(merged, class)
	}
	return merged, nil
}

// now returns the manifest time.
func (u *Uninstaller) now() time.Time {
	if u.config.Now == nil {
		return time.Now().UTC()
	}
	return u.config.Now().UTC()
}
