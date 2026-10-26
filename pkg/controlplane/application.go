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
