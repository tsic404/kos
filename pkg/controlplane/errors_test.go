package controlplane

import (
	"encoding/json"
	"testing"
)

// TestErrorEnvelopeCarriesEveryContractField pins the failure fields C-12
// requires for each failure class a write can produce.
func TestErrorEnvelopeCarriesEveryContractField(t *testing.T) {
	ref := ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "web"}
	failures := map[string]*Error{
		"permission denied": (&Error{
			Code:               CodePermissionDenied,
			Message:            "当前身份不能执行写操作",
			Stage:              StageAuthorize,
			RequiresUserAction: true,
			SideEffects:        SideEffectsNone,
		}).about(ref),
		"invalid field": invalidField("对象含模型未定义的字段").about(ref),
		"concurrent conflict": (&Error{
			Code:        CodeConflict,
			Message:     "对象已被其他写入修改",
			Stage:       StageApply,
			Retryable:   true,
			SideEffects: SideEffectsNone,
		}).about(ref),
		"capability missing": resolveKind("Widget", "example.test/v1").about(ref),
		"write failed mid way": (&Error{
			Code:        CodeWriteFailed,
			Message:     "写入失败",
			Stage:       StageApply,
			Retryable:   true,
			SideEffects: SideEffectsApplied,
			Applied:     []ObjectRef{ref},
		}).about(ref),
	}
	required := []string{"code", "message", "stage", "object", "retryable", "requiresUserAction", "sideEffects"}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(failure)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			fields := map[string]any{}
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, field := range required {
				if _, present := fields[field]; !present {
					t.Errorf("%s is missing from %s", field, encoded)
				}
			}
			object, ok := fields["object"].(map[string]any)
			if !ok {
				t.Fatalf("object is not a reference: %s", encoded)
			}
			for _, field := range []string{"group", "kind", "name"} {
				if _, present := object[field]; !present {
					t.Errorf("object.%s is missing from %s", field, encoded)
				}
			}
			if status := failure.status(); status < 400 || status > 599 {
				t.Errorf("status = %d, want a failure status", status)
			}
		})
	}
}
