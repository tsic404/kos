package controlplane

import (
	"context"
	"fmt"
	"maps"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// validateApplicationWrite rejects an application write the config structure or
// the field boundary of C-32 forbids, before anything reaches the cluster. A
// missing definition is not a rejection: the application stays Pending until
// it appears (C-33).
func (s *Server) validateApplicationWrite(ctx context.Context, object *unstructured.Unstructured) *Error {
	if object.GetKind() != "Application" {
		return nil
	}
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return invalidField("spec 无法读取: " + err.Error())
	}
	if !found {
		spec = map[string]any{}
	}
	definitionRef, _ := spec["definitionRef"].(string)
	config := map[string]any{}
	if raw, present := spec["config"]; present && raw != nil {
		values, ok := raw.(map[string]any)
		if !ok {
			return invalidField("spec.config 必须是一个对象")
		}
		config = values
	}
	// The field boundary of F3 holds whether or not a definition is named; the
	// structure check needs one.
	if err := app.CheckReserved(config); err != nil {
		return invalidApplication(err)
	}
	if failure := validateApplicationResources(object); failure != nil {
		return failure
	}
	if definitionRef == "" {
		return nil
	}
	definition, err := s.applicationDefinition(ctx, definitionRef)
	if err != nil {
		return s.readFailure(err, kernelRef("ApplicationDefinition", "", definitionRef))
	}
	if definition == nil {
		return nil
	}
	if err := app.ValidateDefinition(definition.Spec); err != nil {
		return invalidField(err.Error())
	}
	if _, err := app.Resolve(definition.Spec.Config, config); err != nil {
		return invalidApplication(err)
	}
	return nil
}

// applicationDefinition reads one definition; a missing one is nil.
func (s *Server) applicationDefinition(ctx context.Context, name string) (*v1beta1.ApplicationDefinition, error) {
	objectType, ok := kernelType("ApplicationDefinition")
	if !ok {
		return nil, fmt.Errorf("ApplicationDefinition is not a kernel kind")
	}
	stored, err := resourceOf(s.dynamic, objectType, "").Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	definition := &v1beta1.ApplicationDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, definition); err != nil {
		return nil, fmt.Errorf("decode definition %q: %w", name, err)
	}
	return definition, nil
}

// invalidApplication renders a config refusal as the field-level failure C-12
// requires.
func invalidApplication(err error) *Error {
	return (&Error{
		Code:               CodeInvalidField,
		Message:            fmt.Sprintf("spec.config 被拒绝: %v", err),
		Stage:              StageValidate,
		RequiresUserAction: true,
		SideEffects:        SideEffectsNone,
	})
}

// validateApplicationResources rejects a resource set whose quantities or
// request to limit relation C-67 forbids, naming the offending field.
func validateApplicationResources(object *unstructured.Unstructured) *Error {
	if object.GetKind() != "Application" {
		return nil
	}
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return invalidField("spec 无法读取: " + err.Error())
	}
	if !found {
		return nil
	}
	resources, failure := resourcesOfSpec(spec)
	if failure != nil {
		return failure
	}
	if err := app.ValidateResources(resources, "spec.resources"); err != nil {
		return invalidField(err.Error())
	}
	return nil
}

// materializeApplicationResources fills the resource fields a create leaves
// empty with the package defaults (C-67). The write happens once: the values
// belong to the application spec afterwards, so a later definition update does
// not change them (C-32).
func (s *Server) materializeApplicationResources(ctx context.Context, object *unstructured.Unstructured) *Error {
	if object.GetKind() != "Application" {
		return nil
	}
	spec, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return invalidField("spec 无法读取: " + err.Error())
	}
	if !found {
		return nil
	}
	definitionRef, _ := spec["definitionRef"].(string)
	if definitionRef == "" {
		return nil
	}
	definition, err := s.applicationDefinition(ctx, definitionRef)
	if err != nil {
		return s.readFailure(err, kernelRef("ApplicationDefinition", "", definitionRef))
	}
	if definition == nil || definition.Spec.Resources == nil {
		return nil
	}
	submitted, failure := resourcesOfSpec(spec)
	if failure != nil {
		return failure
	}
	merged := app.MergeResources(definition.Spec.Resources, submitted)
	if merged == nil {
		return nil
	}
	if err := app.ValidateResources(merged, "spec.resources"); err != nil {
		return invalidField(err.Error())
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(merged)
	if err != nil {
		return internal(StageValidate, "资源默认值无法编码")
	}
	spec["resources"] = encoded
	if err := unstructured.SetNestedMap(object.Object, spec, "spec"); err != nil {
		return internal(StageValidate, "资源默认值无法写入 spec")
	}
	return nil
}

// mergeStoredApplicationResources keeps every resource field an update leaves
// out: a submitted field overrides the stored value and the rest stays (C-67).
// The relation between requests and limits is judged on the set the workloads
// converge to, which is the package defaults plus the stored spec plus the
// submission. The defaults come from the snapshot an installation froze, and
// from the definition while no round has frozen one yet: an application that is
// still Pending carries its defaults nowhere else, and a limit below such a
// default would otherwise be accepted while the workload it produces is invalid.
func (s *Server) mergeStoredApplicationResources(ctx context.Context, desired, current map[string]any) *Error {
	if kind, _ := desired["kind"].(string); kind != "Application" {
		return nil
	}
	desiredObject := &unstructured.Unstructured{Object: desired}
	desiredSpec, found, err := unstructured.NestedMap(desired, "spec")
	if err != nil {
		return invalidField("spec 无法读取: " + err.Error())
	}
	if !found {
		desiredSpec = map[string]any{}
	}
	submitted, failure := resourcesOfSpec(desiredSpec)
	if failure != nil {
		return failure
	}
	currentSpec, _, _ := unstructured.NestedMap(current, "spec")
	stored, _ := resourcesOfSpec(currentSpec)
	merged := app.MergeResources(stored, submitted)
	if merged == nil {
		delete(desiredSpec, "resources")
	} else {
		encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(merged)
		if err != nil {
			return internal(StageValidate, "资源规格无法编码")
		}
		desiredSpec["resources"] = encoded
	}
	if err := unstructured.SetNestedMap(desired, desiredSpec, "spec"); err != nil {
		return internal(StageValidate, "资源规格无法写入 spec").about(refOfObject(desiredObject))
	}
	defaults, frozen, failure := resourcesOfStatus(current)
	if failure != nil {
		return failure
	}
	if !frozen {
		defaults, failure = s.packageDefaults(ctx, desiredSpec)
		if failure != nil {
			return failure
		}
	}
	if err := app.ValidateResources(app.MergeResources(defaults, merged), "spec.resources"); err != nil {
		return invalidField(err.Error()).about(refOfObject(desiredObject))
	}
	return nil
}

// resourcesOfStatus reads the package defaults an installation froze into the
// status snapshot; frozen reports whether the snapshot exists at all, so an empty
// one keeps its meaning and a missing one can fall back to the definition (C-32).
func resourcesOfStatus(object map[string]any) (*v1beta1.ResourceRequirements, bool, *Error) {
	raw, present, err := unstructured.NestedFieldNoCopy(object, "status", "resourceDefaults")
	if err != nil {
		return nil, false, internal(StageRead, "status.resourceDefaults 无法读取")
	}
	if !present || raw == nil {
		return nil, false, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, false, internal(StageRead, "status.resourceDefaults 不是合法的资源规格")
	}
	resources := &v1beta1.ResourceRequirements{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, resources); err != nil {
		return nil, false, internal(StageRead, "status.resourceDefaults 不是合法的资源规格")
	}
	return resources, true, nil
}

// packageDefaults reads the defaults the definition a write names declares. An
// application that has not reached its first round has no frozen snapshot yet,
// and the definition is the set its installation will converge to (C-32, C-67).
func (s *Server) packageDefaults(ctx context.Context, spec map[string]any) (*v1beta1.ResourceRequirements, *Error) {
	definitionRef, _ := spec["definitionRef"].(string)
	if definitionRef == "" {
		return nil, nil
	}
	definition, err := s.applicationDefinition(ctx, definitionRef)
	if err != nil {
		return nil, s.readFailure(err, kernelRef("ApplicationDefinition", "", definitionRef))
	}
	if definition == nil {
		return nil, nil
	}
	return definition.Spec.Resources, nil
}

// resourcesOfSpec reads one spec's resource set; a missing value is nil.
func resourcesOfSpec(spec map[string]any) (*v1beta1.ResourceRequirements, *Error) {
	raw, present := spec["resources"]
	if !present || raw == nil {
		return nil, nil
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		return nil, invalidField("spec.resources 必须是一个对象")
	}
	resources := &v1beta1.ResourceRequirements{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, resources); err != nil {
		return nil, invalidField("spec.resources 不是合法的资源规格：" + err.Error())
	}
	return resources, nil
}

// sensitiveKeys reads the config keys the application's definition marks
// sensitive (C-31, C-32). known is false when no definition resolves, so the
// caller cannot tell which values were sensitive and must mask all of them. A
// definition that cannot be read is an error: the caller must not serve the
// value unmasked.
func (s *Server) sensitiveKeys(ctx context.Context, definitionRef string) (keys map[string]bool, known bool, failure *Error) {
	if definitionRef == "" {
		return nil, false, nil
	}
	definition, err := s.applicationDefinition(ctx, definitionRef)
	if err != nil {
		return nil, false, s.readFailure(err, kernelRef("ApplicationDefinition", "", definitionRef))
	}
	if definition == nil {
		return nil, false, nil
	}
	keys = make(map[string]bool)
	for _, field := range definition.Spec.Config {
		if field.Sensitive {
			keys[field.Name] = true
		}
	}
	return keys, true, nil
}

// maskSensitive replaces the config values the definition marks sensitive with a
// mask before they reach a browser: the read path never returns them (C-31,
// C-32). A definition that cannot be read fails the projection rather than
// serving an unmasked value; a definition that no longer exists leaves the
// sensitive keys unknown, so every value is masked.
func (s *Server) maskSensitive(ctx context.Context, kind string, object map[string]any) (map[string]any, *Error) {
	if kind != "Application" || object == nil {
		return object, nil
	}
	config, found, err := unstructured.NestedMap(object, "spec", "config")
	if err != nil || !found || len(config) == 0 {
		return object, nil
	}
	keys, known, failure := s.sensitiveKeys(ctx, nestedText(object, "spec", "definitionRef"))
	if failure != nil {
		return nil, failure
	}
	if known && len(keys) == 0 {
		return object, nil
	}
	masked := maps.Clone(config)
	changed := false
	if !known {
		for key := range masked {
			masked[key] = "***"
			changed = true
		}
	} else {
		for key := range keys {
			if _, present := masked[key]; present {
				masked[key] = "***"
				changed = true
			}
		}
	}
	if !changed {
		return object, nil
	}
	view := maps.Clone(object)
	spec, _ := object["spec"].(map[string]any)
	view["spec"] = maps.Clone(spec)
	view["spec"].(map[string]any)["config"] = masked
	return view, nil
}

// maskChanges replaces the sensitive config sides of an application diff with a
// mask, so a plan or apply response never hands back a stored value (C-31).
func (s *Server) maskChanges(ctx context.Context, object desiredObject, differences []Change) ([]Change, *Error) {
	if object.Type.Kind != "Application" || len(differences) == 0 {
		return differences, nil
	}
	keys, known, failure := s.sensitiveKeys(ctx, nestedText(object.Object.Object, "spec", "definitionRef"))
	if failure != nil {
		return nil, failure
	}
	if known && len(keys) == 0 {
		return differences, nil
	}
	masked := make([]Change, len(differences))
	copy(masked, differences)
	for index := range masked {
		if !configPathIsSensitive(masked[index].Path, keys, known) {
			continue
		}
		if masked[index].From != nil {
			masked[index].From = "***"
		}
		if masked[index].To != nil {
			masked[index].To = "***"
		}
	}
	return masked, nil
}

// configPathIsSensitive reports whether one flattened diff path can carry a
// sensitive config value: the definition's marked keys decide when it is known,
// and every config value counts as sensitive when it is not (C-31).
func configPathIsSensitive(path string, keys map[string]bool, known bool) bool {
	if !known {
		return strings.HasPrefix(path, "spec.config.")
	}
	for key := range keys {
		if sensitiveConfigPath(path, key) {
			return true
		}
	}
	return false
}

// sensitiveConfigPath reports whether a flattened diff path names one sensitive
// config key, including a value below it.
func sensitiveConfigPath(path, key string) bool {
	prefix := "spec.config." + key
	return path == prefix || strings.HasPrefix(path, prefix+".") || strings.HasPrefix(path, prefix+"[")
}
