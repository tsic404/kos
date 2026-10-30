package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/write"
)

// storageClassResource is the cluster's storage class kind: a provider declares
// classes by name, and the class object reports the backend that supplies them
// (C-47).
var storageClassResource = schema.GroupVersionResource{Group: "storage.k8s.io", Version: "v1", Resource: "storageclasses"}

// dependents lists every object C-48 makes a deletion edge from: an endpoint
// that names the provider as its certificate issuer or authentication
// provider, a claim carrying a storage class the provider declares (C-46,
// C-38), a volume whose resolved backend is one the provider supplies through
// its declared storage classes, and an application definition that requires a
// capability the provider still serves together with the applications installed
// from it (C-48).
func (r *Reconciler) dependents(ctx context.Context, provider string, spec v1beta1.ProviderSpec, previous v1beta1.ProviderStatus) ([]v1beta1.ObjectRef, error) {
	endpoints, err := listObjects(ctx, r.writer, api.Group, "Endpoint")
	if err != nil {
		return nil, err
	}
	var dependents []v1beta1.ObjectRef
	for _, endpoint := range endpoints {
		_, tlsDeclared, err := unstructured.NestedMap(endpoint.Object, "spec", "tls")
		if err != nil {
			return nil, fmt.Errorf("read endpoint %s/%s: %w", endpoint.GetNamespace(), endpoint.GetName(), err)
		}
		issuer, _, err := unstructured.NestedString(endpoint.Object, "spec", "tls", "issuerRef")
		if err != nil {
			return nil, fmt.Errorf("read endpoint %s/%s: %w", endpoint.GetNamespace(), endpoint.GetName(), err)
		}
		// The spec reference wins; an endpoint that omits it serves the issuer
		// its status resolved (C-46). A status left behind by a certificate the
		// endpoint no longer declares is not an edge.
		if issuer == "" && tlsDeclared {
			if issuer, _, err = unstructured.NestedString(endpoint.Object, "status", "certificate", "issuer"); err != nil {
				return nil, fmt.Errorf("read endpoint %s/%s: %w", endpoint.GetNamespace(), endpoint.GetName(), err)
			}
		}
		auth, _, err := unstructured.NestedString(endpoint.Object, "spec", "auth", "providerRef")
		if err != nil {
			return nil, fmt.Errorf("read endpoint %s/%s: %w", endpoint.GetNamespace(), endpoint.GetName(), err)
		}
		if issuer == provider || auth == provider {
			dependents = append(dependents, refOf("Endpoint", endpoint.GetNamespace(), endpoint.GetName()))
		}
	}
	// The volume edge reads what C-38 resolved into the volume's status: the
	// backend its binding landed on. A provider supplies a backend through the
	// storage classes it declares, and each class object reports that backend;
	// a volume bound through another class on the same backend is a dependent
	// too. The class-name edge below stays separate, and a volume both edges
	// match is still listed once (C-38, C-47, C-48).
	backends, err := r.backends(ctx, spec.StorageClasses)
	if err != nil {
		return nil, err
	}
	if len(backends) > 0 {
		volumes, err := listObjects(ctx, r.writer, api.Group, "Volume")
		if err != nil {
			return nil, err
		}
		for _, volume := range volumes {
			backend, _, err := unstructured.NestedString(volume.Object, "status", "provider", "name")
			if err != nil {
				return nil, fmt.Errorf("read volume %s/%s: %w", volume.GetNamespace(), volume.GetName(), err)
			}
			if backends[backend] {
				dependents = append(dependents, refOf("Volume", volume.GetNamespace(), volume.GetName()))
			}
		}
	}
	if len(spec.StorageClasses) > 0 {
		classes := make(map[string]bool, len(spec.StorageClasses))
		for _, class := range spec.StorageClasses {
			classes[class] = true
		}
		claims, err := listObjects(ctx, r.writer, "", "PersistentVolumeClaim")
		if err != nil {
			return nil, err
		}
		for _, claim := range claims {
			className, _, err := unstructured.NestedString(claim.Object, "spec", "storageClassName")
			if err != nil {
				return nil, fmt.Errorf("read claim %s/%s: %w", claim.GetNamespace(), claim.GetName(), err)
			}
			if !classes[className] {
				continue
			}
			dependents = append(dependents, r.claimDependent(ctx, claim))
		}
	}
	// The requirement edge reads what the provider serves, not what it declares
	// now: a blocked withdrawal leaves the old set in service after the
	// declaration drops it, and a refused registration never put its entries in
	// service at all (C-47, C-48).
	required, err := r.references(ctx, inService(spec, previous))
	if err != nil {
		return nil, err
	}
	dependents = append(dependents, required...)
	return sortRefs(dependents), nil
}

// backends returns the storage backends the provider's declared storage classes
// supply: each class object reports the provisioner a bound volume records as
// its resolved provider (C-38, C-47). A declared class the cluster does not
// serve contributes nothing; claims naming it are still dependents through the
// class-name edge.
func (r *Reconciler) backends(ctx context.Context, classes []string) (map[string]bool, error) {
	backends := make(map[string]bool, len(classes))
	for _, class := range classes {
		object, err := r.writer.Dynamic.Resource(storageClassResource).Get(ctx, class, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read storage class %s: %w", class, err)
		}
		provisioner, _, err := unstructured.NestedString(object.Object, "provisioner")
		if err != nil {
			return nil, fmt.Errorf("read storage class %s: %w", class, err)
		}
		if provisioner != "" {
			backends[provisioner] = true
		}
	}
	return backends, nil
}

// inService returns the capabilities a provider actually serves: the effective
// set its status records. A provider whose status was never written has no
// record, so its declaration is the only set a deletion edge can read. The test
// is whether a condition exists, never whether the effective set is empty — a
// registration refused on its first round records an empty set on purpose.
func inService(spec v1beta1.ProviderSpec, previous v1beta1.ProviderStatus) []v1beta1.ProviderCapability {
	if len(previous.Conditions) == 0 {
		return spec.Capabilities
	}
	return previous.EffectiveCapabilities
}

// claimDependent renders a claim one of the provider's classes backs as the
// volume C-38 binds it to; a claim without a volume object is itself the
// dependent.
func (r *Reconciler) claimDependent(ctx context.Context, claim *unstructured.Unstructured) v1beta1.ObjectRef {
	volume, err := r.writer.Get(ctx, write.Ref{
		Group:     api.Group,
		Version:   v1beta1.GroupVersion.Version,
		Kind:      "Volume",
		Namespace: claim.GetNamespace(),
		Name:      claim.GetName(),
	})
	if err == nil && volume != nil {
		return refOf("Volume", claim.GetNamespace(), claim.GetName())
	}
	return v1beta1.ObjectRef{
		Group:     "",
		Version:   "v1",
		Kind:      "PersistentVolumeClaim",
		Namespace: claim.GetNamespace(),
		Name:      claim.GetName(),
	}
}

// references lists the objects that reference one of the given capabilities: an
// application definition whose requirements admit any of them, and every
// application installed from that definition (C-48). The caller decides which
// set to pass: the withdrawal path hands over the capabilities being withdrawn,
// the deletion path the set the provider still serves.
func (r *Reconciler) references(ctx context.Context, capabilities []v1beta1.ProviderCapability) ([]v1beta1.ObjectRef, error) {
	if len(capabilities) == 0 {
		return nil, nil
	}
	definitions, err := listObjects(ctx, r.writer, api.Group, "ApplicationDefinition")
	if err != nil {
		return nil, err
	}
	applications, err := listObjects(ctx, r.writer, api.Group, "Application")
	if err != nil {
		return nil, err
	}
	consumers := make(map[string]bool, len(definitions))
	references := make([]v1beta1.ObjectRef, 0, len(definitions))
	for _, definition := range definitions {
		referenced, err := requiresAny(definition, capabilities)
		if err != nil {
			return nil, err
		}
		if !referenced {
			continue
		}
		references = append(references, refOf("ApplicationDefinition", "", definition.GetName()))
		consumers[definition.GetName()] = true
	}
	for _, application := range applications {
		definitionRef, _, err := unstructured.NestedString(application.Object, "spec", "definitionRef")
		if err != nil {
			return nil, fmt.Errorf("read application %s/%s: %w", application.GetNamespace(), application.GetName(), err)
		}
		if consumers[definitionRef] {
			references = append(references, refOf("Application", application.GetNamespace(), application.GetName()))
		}
	}
	return sortRefs(references), nil
}

// requiresAny reports whether one definition requires any of the given
// capability versions.
func requiresAny(definition *unstructured.Unstructured, capabilities []v1beta1.ProviderCapability) (bool, error) {
	raw, found, err := unstructured.NestedSlice(definition.Object, "spec", "requires")
	if err != nil || !found {
		return false, err
	}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		requirement := v1beta1.CapabilityRequirement{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(entry, &requirement); err != nil {
			return false, fmt.Errorf("read requirement of %s: %w", definition.GetName(), err)
		}
		for _, capability := range capabilities {
			if requirement.Name != capability.Name {
				continue
			}
			// A range that does not parse is the definition's defect; it still
			// names the capability, so the dependency holds.
			matched, err := app.MatchesVersion(capability.Version, requirement.Version)
			if err != nil || matched {
				return true, nil
			}
		}
	}
	return false, nil
}

// listObjects reads every object of one kind; an empty group is the run
// kernel's core group (C-01).
func listObjects(ctx context.Context, writer *write.Writer, group, kind string) ([]*unstructured.Unstructured, error) {
	version := v1beta1.GroupVersion.Version
	if group == "" {
		version = "v1"
	}
	ref := write.Ref{Group: group, Version: version, Kind: kind}
	plural, err := writer.ResourceName(ref)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	list, err := writer.Dynamic.Resource(schema.GroupVersionResource{Group: group, Version: version, Resource: plural}).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", kind, err)
	}
	objects := make([]*unstructured.Unstructured, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, &list.Items[index])
	}
	return objects, nil
}

// refOf names one kernel object of the served version.
func refOf(kind, namespace, name string) v1beta1.ObjectRef {
	return v1beta1.ObjectRef{
		Group:     api.Group,
		Version:   v1beta1.GroupVersion.Version,
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
	}
}

// sortRefs deduplicates and orders references, so an unchanged state renders an
// unchanged condition message (C-06).
func sortRefs(refs []v1beta1.ObjectRef) []v1beta1.ObjectRef {
	seen := make(map[string]bool, len(refs))
	sorted := make([]v1beta1.ObjectRef, 0, len(refs))
	for _, ref := range refs {
		key := ref.Kind + "\x00" + ref.Namespace + "\x00" + ref.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		sorted = append(sorted, ref)
	}
	sort.SliceStable(sorted, func(left, right int) bool {
		if sorted[left].Kind != sorted[right].Kind {
			return sorted[left].Kind < sorted[right].Kind
		}
		if sorted[left].Namespace != sorted[right].Namespace {
			return sorted[left].Namespace < sorted[right].Namespace
		}
		return sorted[left].Name < sorted[right].Name
	})
	return sorted
}

// refsMessage renders a reference list for a condition message.
func refsMessage(refs []v1beta1.ObjectRef) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		name := ref.Name
		if ref.Namespace != "" {
			name = ref.Namespace + "/" + ref.Name
		}
		parts = append(parts, ref.Kind+" "+name)
	}
	return strings.Join(parts, ", ")
}
