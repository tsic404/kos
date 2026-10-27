package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// Installer evaluates the live system components and writes their status: it
// is the install pipeline of C-17 read through C-28.
type Installer struct {
	// Dynamic reads the system components.
	Dynamic dynamic.Interface
	// Resources maps a kernel kind to its served resource name.
	Resources map[schema.GroupKind]string
	// Artifacts resolves a package reference to its bytes.
	Artifacts ArtifactSource
	// Status writes component status as the component reconciler (C-03).
	Status *write.Writer
	// Preferences applies the declared fate of preference data when a component
	// is withdrawn (C-27, C-29); nil keeps the store untouched.
	Preferences PreferenceRetention
	// Log records preference bookkeeping failures; nil discards them.
	Log *slog.Logger
}

// PreferenceRetention is where the install pipeline settles the preference data
// of a component that is no longer installed. An extension's preferences
// survive its withdrawal only when its declaration asked for that (C-27, C-29).
type PreferenceRetention interface {
	// Retain records the declared fate of one installed component's preference
	// data, keyed by the component object that declares it.
	Retain(ctx context.Context, component, identifier string, keep bool) error
	// Withdraw applies the recorded fates. present maps every live component
	// object to the extension identifier it declares now; an empty identifier
	// means this round could not read the declaration, so its domains stay.
	Withdraw(ctx context.Context, present map[string]string) error
}

// Installation is the result of one install pass.
type Installation struct {
	components []ComponentResult
	registry   *Registry
}

// Components returns every evaluated component in install order.
func (i *Installation) Components() []ComponentResult {
	return append([]ComponentResult(nil), i.components...)
}

// Component returns one component's result by object name.
func (i *Installation) Component(name string) (ComponentResult, bool) {
	for _, component := range i.components {
		if component.Name == name {
			return component, true
		}
	}
	return ComponentResult{}, false
}

// Registry returns the visibility projection of the active components.
func (i *Installation) Registry() *Registry {
	return i.registry
}

// Install reads every live component, verifies its package, evaluates the
// declarations and writes the resulting status. Components without an
// attribution marker are not KOS's to write and are left alone (C-02).
func (i *Installer) Install(ctx context.Context) (*Installation, error) {
	if i.Dynamic == nil || i.Artifacts == nil || i.Status == nil {
		return nil, errors.New("installer needs a dynamic client, an artifact source and a status writer")
	}
	// The gate runs first: a component whose deletion nothing blocks leaves the
	// install set in this same pass, and one a dependent holds stays installed
	// with the blocked condition already on it (C-17, C-48).
	blocked, err := (&Gate{Dynamic: i.Dynamic, Resources: i.Resources, Status: i.Status}).Reconcile(ctx)
	if err != nil {
		return nil, err
	}
	held := BlockedNames(blocked)
	resource, err := i.resource()
	if err != nil {
		return nil, err
	}
	list, err := resource.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list system components: %w", err)
	}

	requests := make([]Request, 0, len(list.Items))
	objects := make(map[string]*unstructured.Unstructured, len(list.Items))
	live := make(map[string]bool, len(list.Items))
	for index := range list.Items {
		object := &list.Items[index]
		if object.GetDeletionTimestamp() != nil && !held[object.GetName()] {
			continue
		}
		// A gate-released component has left the set, so it is not live here.
		live[object.GetName()] = true
		attribution, err := markers.AttributionOf(object.GetLabels())
		if err != nil {
			return nil, fmt.Errorf("component %s: %w", object.GetName(), err)
		}
		if attribution == markers.AttributionUnmanaged {
			continue
		}
		request := Request{
			Name:       object.GetName(),
			Generation: object.GetGeneration(),
			Created:    object.GetCreationTimestamp().Time,
			IsAdopted:  attribution == markers.AttributionAdopted,
		}
		if !request.IsAdopted {
			reference, _, err := unstructured.NestedString(object.Object, "spec", "package", "reference")
			if err != nil {
				return nil, fmt.Errorf("component %s: read spec.package.reference: %w", object.GetName(), err)
			}
			digest, _, err := unstructured.NestedString(object.Object, "spec", "package", "digest")
			if err != nil {
				return nil, fmt.Errorf("component %s: read spec.package.digest: %w", object.GetName(), err)
			}
			if reference != "" {
				content, fetchErr := i.Artifacts.Fetch(ctx, reference)
				if fetchErr != nil {
					request.FetchError = fetchErr
				} else {
					request.Package = &Package{Reference: reference, Digest: digest, Content: content}
				}
			}
		}
		requests = append(requests, request)
		objects[request.Name] = object
	}

	assessment := Evaluate(requests)
	for _, result := range assessment.Components {
		object := objects[result.Name]
		if object == nil {
			continue
		}
		status := StatusFor(result, existingConditions(object))
		if err := i.writeStatus(ctx, result.Name, status, object); err != nil {
			return nil, err
		}
		if !result.HasVerifiedPackage || !result.HasMetDependencies {
			continue
		}
		if err := i.registerProvider(ctx, result, object); err != nil {
			return nil, err
		}
	}
	// Preference bookkeeping records what it can and never fails the pass: the
	// control-plane database is not on the reconciliation path (C-27, C-61).
	i.retainPreferences(ctx, assessment, live)
	return &Installation{components: assessment.Components, registry: assessment.Registry}, nil
}

// retainPreferences records the declared fate of every active extension's
// preference data and then applies the recorded fates to the domains whose owner
// is gone (C-27, C-29). A database failure is logged and changes nothing else:
// the control-plane database is not on the reconciliation path (C-61).
func (i *Installer) retainPreferences(ctx context.Context, assessment Assessment, live map[string]bool) {
	if i.Preferences == nil {
		return
	}
	present := make(map[string]string, len(live))
	for name := range live {
		// Every component object that still exists keeps its domains, whatever
		// this round could read from it: only an object that is gone withdraws
		// data.
		present[name] = ""
	}
	recorded := true
	for _, component := range assessment.Components {
		present[component.Name] = component.Identifier
		if component.Identifier == "" || !component.HasMetDependencies || len(component.Contributions.Preferences) == 0 {
			continue
		}
		if err := i.Preferences.Retain(ctx, component.Name, component.Identifier, keepsContribution(component.Retention, ClassPreferences)); err != nil {
			recorded = false
			i.logger().Error("record preference domain", "component", component.Name, "identifier", component.Identifier, "error", err)
		}
	}
	if !recorded {
		// Acting on a partly recorded round could delete data whose declaration
		// did not reach the store; the next round settles it.
		return
	}
	if err := i.Preferences.Withdraw(ctx, present); err != nil {
		i.logger().Error("settle preference domains", "error", err)
	}
}

// logger resolves the install log.
func (i *Installer) logger() *slog.Logger {
	if i.Log != nil {
		return i.Log
	}
	return slog.New(slog.DiscardHandler)
}

// keepsContribution reports whether the declaration kept the data of one
// contribution class. An undeclared class is deleted, which is C-29's default.
func keepsContribution(retention []Retention, class string) bool {
	for _, entry := range retention {
		if entry.Resource == class {
			return entry.Keep
		}
	}
	return false
}

// registerProvider writes the registration a component declares (C-47). The
// component path owns Provider.spec and the attribution markers; the validator
// owns status, so the two writers never touch one field (C-03).
func (i *Installer) registerProvider(ctx context.Context, result ComponentResult, component *unstructured.Unstructured) error {
	contribution := result.Contributions.Capabilities
	capabilities := make([]v1beta1.ProviderCapability, 0, len(contribution.Items))
	for _, entry := range contribution.Items {
		// A baseline capability without a declared scope takes the value the
		// kernel fixes for it; an extension capability must declare one and is
		// left empty for the validator to refuse (C-09).
		scope := entry.Scope
		if scope == "" {
			if baseline, found := baselineCapability(entry.Name); found {
				scope = baseline.Scope
			}
		}
		capabilities = append(capabilities, v1beta1.ProviderCapability{
			Name:          entry.Name,
			Version:       entry.Version,
			Scope:         scope,
			Parameters:    append([]v1beta1.CapabilityParameter(nil), entry.Parameters...),
			Guarantees:    append([]v1beta1.CapabilityGuarantee(nil), entry.Guarantees...),
			CaseSet:       entry.CaseSet,
			CaseSetDigest: entry.CaseSetDigest,
		})
	}
	reports := make([]v1beta1.ProviderSelfReport, 0, len(contribution.SelfReported))
	for _, report := range contribution.SelfReported {
		reports = append(reports, v1beta1.ProviderSelfReport{
			Capability:  report.Capability,
			Version:     report.Version,
			FailedCases: append([]string(nil), report.FailedCases...),
		})
	}
	spec := v1beta1.ProviderSpec{
		ComponentRef:   result.Name,
		Version:        result.Version,
		StorageClasses: append([]string(nil), contribution.StorageClasses...),
		Capabilities:   capabilities,
		SelfReported:   reports,
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&spec)
	if err != nil {
		return fmt.Errorf("provider %s: encode spec: %w", result.Name, err)
	}
	metadata := map[string]any{"name": result.Name}
	labels, err := i.registrationAttribution(ctx, result.Name, component)
	if err != nil {
		return err
	}
	if len(labels) > 0 {
		metadata["labels"] = labels
	}
	document := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "Provider",
		"metadata":   metadata,
		"spec":       raw,
	}}
	if _, err := i.Status.Apply(ctx, write.SubjectComponentRegistry, write.Session{}, write.PartSpec, document); err != nil {
		return fmt.Errorf("register provider %s: %w", result.Name, err)
	}
	return nil
}

// registrationAttribution returns the attribution labels one Provider write
// carries (C-02, C-47). A first registration takes this component's identity;
// an existing registration keeps what was written at creation, because
// attribution does not change after the object exists.
func (i *Installer) registrationAttribution(ctx context.Context, name string, component *unstructured.Unstructured) (map[string]any, error) {
	resource, err := i.providerResource()
	if err != nil {
		return nil, err
	}
	current, err := resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return map[string]any{
			markers.ManagedBy: write.ManagedByValue,
			markers.OwnerUID:  string(component.GetUID()),
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read provider %s: %w", name, err)
	}
	labels := map[string]any{}
	for _, key := range []string{markers.ManagedBy, markers.OwnerUID} {
		if value := current.GetLabels()[key]; value != "" {
			labels[key] = value
		}
	}
	return labels, nil
}

// providerResource resolves the Provider client.
func (i *Installer) providerResource() (dynamic.ResourceInterface, error) {
	plural, ok := i.Resources[schema.GroupKind{Group: api.Group, Kind: "Provider"}]
	if !ok {
		return nil, errors.New("installer has no resource name for Provider")
	}
	return i.Dynamic.Resource(schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: plural,
	}), nil
}

// resource resolves the system component client.
func (i *Installer) resource() (dynamic.ResourceInterface, error) {
	plural, ok := i.Resources[schema.GroupKind{Group: api.Group, Kind: "SystemComponent"}]
	if !ok {
		return nil, errors.New("installer has no resource name for SystemComponent")
	}
	return i.Dynamic.Resource(schema.GroupVersionResource{
		Group:    api.Group,
		Version:  v1beta1.GroupVersion.Version,
		Resource: plural,
	}), nil
}

// writeStatus applies the component status only when it differs from the stored
// one, so an unchanged round writes nothing (C-06).
func (i *Installer) writeStatus(ctx context.Context, name string, status v1beta1.ObjectStatus, current *unstructured.Unstructured) error {
	document, err := statusDocument(status)
	if err != nil {
		return fmt.Errorf("component %s: render status: %w", name, err)
	}
	stored, _, err := unstructured.NestedMap(current.Object, "status")
	if err != nil {
		return fmt.Errorf("component %s: read status: %w", name, err)
	}
	if sameJSON(stored, document) {
		return nil
	}
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/" + v1beta1.GroupVersion.Version,
		"kind":       "SystemComponent",
		"metadata":   map[string]any{"name": name},
		"status":     document,
	}}
	if _, err := i.Status.Apply(ctx, write.SubjectBackupReconciler, write.Session{}, write.PartStatus, object); err != nil {
		return fmt.Errorf("component %s: write status: %w", name, err)
	}
	return nil
}

// statusDocument renders the status as the map the dynamic client writes.
func statusDocument(status v1beta1.ObjectStatus) (map[string]any, error) {
	encoded, err := json.Marshal(status)
	if err != nil {
		return nil, err
	}
	document := map[string]any{}
	if err := json.Unmarshal(encoded, &document); err != nil {
		return nil, err
	}
	if _, present := document["conditions"]; !present {
		document["conditions"] = []any{}
	}
	return document, nil
}

// existingConditions reads the stored condition list.
func existingConditions(object *unstructured.Unstructured) []metav1.Condition {
	raw, found, err := unstructured.NestedSlice(object.Object, "status", "conditions")
	if err != nil || !found {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var conditions []metav1.Condition
	if err := json.Unmarshal(encoded, &conditions); err != nil {
		return nil
	}
	return conditions
}

// sameJSON compares two decoded JSON values by their encoding.
func sameJSON(left, right any) bool {
	leftEncoded, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightEncoded, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(leftEncoded) == string(rightEncoded)
}
