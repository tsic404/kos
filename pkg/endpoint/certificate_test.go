package endpoint

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
)

// TestCertificateAlertCoversExactlyTheWarningWindow pins C-46's warning
// boundary: a certificate with the full window left still warns, one second
// more does not.
func TestCertificateAlertCoversExactlyTheWarningWindow(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	window := now.Add(CertificateWarningWindow)
	certificate := Certificate{Issuer: DefaultIssuer, Ready: true, NotAfter: &window}
	if alert := certificateExpiryAlert(certificate, now); alert == "" {
		t.Fatalf("a certificate with exactly %v left carries no alert", CertificateWarningWindow)
	}
	outside := now.Add(CertificateWarningWindow + time.Second)
	certificate.NotAfter = &outside
	if alert := certificateExpiryAlert(certificate, now); alert != "" {
		t.Fatalf("a certificate outside the window carries the alert %q", alert)
	}
	certificate.NotAfter = nil
	if alert := certificateExpiryAlert(certificate, now); alert != "" {
		t.Fatalf("an unobserved certificate carries the alert %q", alert)
	}
}

// TestComponentRefsBindsTheSpecReferenceAndTheStatusResolution states the two
// dependency-edge paths of C-46: a named issuer comes from the spec, an omitted
// one from the issuer the status resolved, and the authentication provider is
// an edge of its own.
func TestComponentRefsBindsTheSpecReferenceAndTheStatusResolution(t *testing.T) {
	named := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"kind": "http.host", "targetApp": "web", "host": "site.test",
			"tls": map[string]any{"issuerRef": "issuer"},
		},
	}}
	if refs := ComponentRefs(named); len(refs) != 1 || refs[0] != "issuer" {
		t.Fatalf("named issuer refs = %v, want the spec reference", refs)
	}

	resolved := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"kind": "http.host", "targetApp": "web", "host": "site.test",
			"tls":  map[string]any{},
			"auth": map[string]any{"providerRef": "sso"},
		},
		"status": map[string]any{
			"certificate": map[string]any{"issuer": DefaultIssuer, "ready": false},
		},
	}}
	if refs := ComponentRefs(resolved); len(refs) != 2 || refs[0] != DefaultIssuer || refs[1] != "sso" {
		t.Fatalf("resolved refs = %v, want the status issuer and the auth provider", refs)
	}

	// The status resolution is not a second edge for a spec that names one.
	both := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{
			"kind": "http.host", "targetApp": "web", "host": "site.test",
			"tls": map[string]any{"issuerRef": "issuer"},
		},
		"status": map[string]any{
			"certificate": map[string]any{"issuer": DefaultIssuer},
		},
	}}
	if refs := ComponentRefs(both); len(refs) != 1 || refs[0] != "issuer" {
		t.Fatalf("refs = %v, want only the spec reference", refs)
	}
}

// TestValidateRequiresAProviderForForwardAuth pins the one rule C-46 gives the
// optional authentication mode: choosing it binds a provider component in both
// HTTP shapes.
func TestValidateRequiresAProviderForForwardAuth(t *testing.T) {
	for _, spec := range []v1beta1.EndpointSpec{
		{Kind: v1beta1.EndpointKindHTTPHost, TargetApp: "web", Host: "site.test"},
		{Kind: v1beta1.EndpointKindHTTPPath, TargetApp: "web", Host: "site.test", Path: "/site"},
	} {
		spec.Auth = &v1beta1.EndpointAuth{}
		failure := Validate(t.Context(), "default", spec, nil)
		if failure == nil || failure.Field != "spec.auth.providerRef" {
			t.Fatalf("%s: failure = %v, want spec.auth.providerRef", spec.Kind, failure)
		}
	}
}
