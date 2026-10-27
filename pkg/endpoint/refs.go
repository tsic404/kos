package endpoint

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ComponentRefs returns the system components an endpoint depends on (C-46):
// the spec's issuer and authentication provider, and — when the spec omits the
// issuer — the issuer its status resolved. The two paths are what C-46 calls
// the spec reference and the status resolution; a deleted component lists the
// endpoints each path binds to it.
func ComponentRefs(object *unstructured.Unstructured) []string {
	spec, err := SpecOf(object)
	if err != nil {
		return nil
	}
	refs := make([]string, 0, 2)
	if spec.TLS != nil {
		issuer := spec.TLS.IssuerRef
		if issuer == "" {
			issuer = statusIssuer(object)
		}
		if issuer != "" {
			refs = append(refs, issuer)
		}
	}
	if spec.Auth != nil && spec.Auth.ProviderRef != "" {
		refs = append(refs, spec.Auth.ProviderRef)
	}
	return unique(refs)
}

// statusIssuer reads the issuer identity an endpoint's status records (C-46).
func statusIssuer(object *unstructured.Unstructured) string {
	issuer, _, err := unstructured.NestedString(object.Object, "status", "certificate", "issuer")
	if err != nil {
		return ""
	}
	return issuer
}

// unique drops repeated references while keeping their order.
func unique(values []string) []string {
	kept := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		kept = append(kept, value)
	}
	return kept
}
