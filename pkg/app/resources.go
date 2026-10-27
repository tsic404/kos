package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
)

// LimitRangeResource is the namespace resource whose defaults still apply to an
// application that declares no requests or limits (C-67).
var LimitRangeResource = schema.GroupVersionResource{Version: "v1", Resource: "limitranges"}

// ResourceError reports one refused resource field; Field names the spec path so
// the caller can point at it (C-67).
type ResourceError struct {
	// Field is the refused field path.
	Field string
	// Reason is what is wrong with the value.
	Reason string
}

// Error names the field and the reason.
func (e *ResourceError) Error() string {
	return fmt.Sprintf("resource field %s: %s", e.Field, e.Reason)
}

// ResourceErrors collects every refusal of one resource set.
type ResourceErrors struct {
	// Fields lists the refusals in field order.
	Fields []*ResourceError
}

// Error lists the refused fields.
func (e *ResourceErrors) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, field := range e.Fields {
		parts = append(parts, field.Error())
	}
	return strings.Join(parts, "; ")
}

// ValidateResources checks the quantities and the request to limit relation of
// one resource set (C-67). prefix names where the set lives in the submitted
// object, so a refusal points at the field.
func ValidateResources(resources *v1beta1.ResourceRequirements, prefix string) error {
	if resources == nil {
		return nil
	}
	pairs := []struct {
		name           string
		request, limit string
	}{
		{"cpu", resources.Requests.CPU, resources.Limits.CPU},
		{"memory", resources.Requests.Memory, resources.Limits.Memory},
	}
	var refused []*ResourceError
	for _, pair := range pairs {
		request, requestErr := parseQuantity(pair.request)
		limit, limitErr := parseQuantity(pair.limit)
		if requestErr != nil {
			refused = append(refused, &ResourceError{Field: prefix + ".requests." + pair.name, Reason: requestErr.Error()})
		}
		if limitErr != nil {
			refused = append(refused, &ResourceError{Field: prefix + ".limits." + pair.name, Reason: limitErr.Error()})
		}
		if request != nil && limit != nil && limit.Cmp(*request) < 0 {
			refused = append(refused, &ResourceError{
				Field:  prefix + ".limits." + pair.name,
				Reason: fmt.Sprintf("limit %s is below the request %s", limit.String(), request.String()),
			})
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return &ResourceErrors{Fields: refused}
}

// parseQuantity reads one declared quantity; an empty value means KOS sets
// nothing and a negative one is invalid (C-67).
func parseQuantity(value string) (*resource.Quantity, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return nil, fmt.Errorf("%q is not a Kubernetes quantity", value)
	}
	if quantity.Sign() < 0 {
		return nil, fmt.Errorf("%q is negative", value)
	}
	return &quantity, nil
}

// MergeResources overlays every submitted field over the base set (C-67): a
// field the submission leaves empty keeps the base value. It returns nil when
// neither side declares anything.
func MergeResources(base, submitted *v1beta1.ResourceRequirements) *v1beta1.ResourceRequirements {
	if base == nil && submitted == nil {
		return nil
	}
	merged := v1beta1.ResourceRequirements{}
	if base != nil {
		merged = *base
	}
	if submitted != nil {
		merged.Requests.CPU = firstDeclared(submitted.Requests.CPU, merged.Requests.CPU)
		merged.Requests.Memory = firstDeclared(submitted.Requests.Memory, merged.Requests.Memory)
		merged.Limits.CPU = firstDeclared(submitted.Limits.CPU, merged.Limits.CPU)
		merged.Limits.Memory = firstDeclared(submitted.Limits.Memory, merged.Limits.Memory)
	}
	if ResourcesEmpty(&merged) {
		return nil
	}
	return &merged
}

// firstDeclared returns primary when it declares a value and fallback otherwise.
func firstDeclared(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

// ResourcesEmpty reports a resource set that declares neither requests nor
// limits; it is the "unlimited" state of C-67.
func ResourcesEmpty(resources *v1beta1.ResourceRequirements) bool {
	if resources == nil {
		return true
	}
	return strings.TrimSpace(resources.Requests.CPU) == "" &&
		strings.TrimSpace(resources.Requests.Memory) == "" &&
		strings.TrimSpace(resources.Limits.CPU) == "" &&
		strings.TrimSpace(resources.Limits.Memory) == ""
}

// limitRangeName returns the namespace LimitRange that still supplies container
// defaults, sorted first so the projection is stable (C-67). Empty means the
// namespace has no such LimitRange.
func (c *Controller) limitRangeName(ctx context.Context, namespace string) (string, error) {
	if namespace == "" {
		return "", nil
	}
	records, err := c.store.List(ctx, LimitRangeResource, namespace, "")
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(records))
	for index := range records {
		if suppliesContainerDefaults(&records[index]) {
			names = append(names, records[index].GetName())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", nil
	}
	return names[0], nil
}

// suppliesContainerDefaults reports whether one LimitRange declares container
// defaults, so it is the source a projection names (C-67).
func suppliesContainerDefaults(limitRange *unstructured.Unstructured) bool {
	items, found, _ := unstructured.NestedSlice(limitRange.Object, "spec", "limits")
	if !found {
		return false
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if itemType, _ := item["type"].(string); itemType != "Container" {
			continue
		}
		for _, key := range []string{"default", "defaultRequest"} {
			if values, ok := item[key].(map[string]any); ok && len(values) > 0 {
				return true
			}
		}
	}
	return false
}
