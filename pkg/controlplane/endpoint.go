package controlplane

import (
	"context"

	"k8s.io/apimachinery/pkg/labels"

	"github.com/tsic404/kos/pkg/endpoint"
)

// TargetAppImmutable is the reason a rewrite of an endpoint's target
// application is refused (C-45). The CRD carries the same sentence as its
// transition rule, so a native write is refused with the same words.
const TargetAppImmutable = "spec.targetApp 创建后不可修改"

// validateDesired applies the rules a write must pass beyond the object model:
// C-43's shape matrix, the two path-prefix declarations and C-60's backup
// selector syntax. The API is the write path that refuses an illegal shape; the
// reconciler marks an object that bypassed the API instead of admitting it.
func (s *Server) validateDesired(ctx context.Context, object desiredObject) *Error {
	if object.Ref.Kind == "BackupPolicy" {
		raw := nestedText(object.Object.Object, "spec", "selector")
		if raw == "" {
			return nil
		}
		if _, err := labels.Parse(raw); err != nil {
			return invalidField("spec.selector 不是合法的标签选择器：" + err.Error()).about(object.Ref)
		}
		return nil
	}
	if object.Ref.Kind != "Endpoint" {
		return nil
	}
	spec, err := endpoint.SpecOf(object.Object)
	if err != nil {
		return invalidField(err.Error()).about(object.Ref)
	}
	if failure := endpoint.Validate(ctx, object.Ref.Namespace, spec, s.endpointFacts); failure != nil {
		return invalidField(failure.Error()).about(object.Ref)
	}
	return nil
}

// validateEndpointUpdate refuses a rewrite of C-45's fixed target application.
// The CRD's transition rule refuses the same write on the native path.
func validateEndpointUpdate(current map[string]any, object desiredObject) *Error {
	if object.Ref.Kind != "Endpoint" {
		return nil
	}
	if nestedText(current, "spec", "targetApp") != nestedText(object.Object.Object, "spec", "targetApp") {
		return invalidField(TargetAppImmutable).about(object.Ref)
	}
	return nil
}
