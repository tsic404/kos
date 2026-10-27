package controlplane

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// applyOwner writes the owner the authorization seam resolved onto an owned
// object (C-60). The seam decides which value may stand — the session's at
// creation, the stored one on an ordinary write, the submitted one on a global
// Owner's transfer — so this path never invents one.
func applyOwner(object *unstructured.Unstructured, grant Grant) *Error {
	if grant.Owner == "" {
		return nil
	}
	if err := unstructured.SetNestedField(object.Object, grant.Owner, "spec", "owner"); err != nil {
		return internal(StageValidate, "spec.owner 无法写入: "+err.Error())
	}
	return nil
}
