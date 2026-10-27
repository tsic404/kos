package endpoint

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// DefaultIssuer is the reserved identity of the distribution's built-in
// certificate issuer (C-46). It is not a product name, so no extension takes
// it; an endpoint that enables TLS without naming an issuer resolves to it.
const DefaultIssuer = "internal-issuer.kos.tsic.top"

// CertificateWarningWindow is how long before expiry the panel warns (C-46).
const CertificateWarningWindow = 14 * 24 * time.Hour

// certificateSecretSuffix names the TLS secret of an endpoint: the issuer
// writes it at the name the route carries, so the kernel observes a standard
// kubernetes.io/tls secret instead of an issued format of its own (C-46).
const certificateSecretSuffix = "-tls"

// Certificate condition of an endpoint (C-46). It is the endpoint's own
// condition type, never a stand-in for Reconciled (C-07).
const (
	// ConditionCertificateReady reports the certificate state of an endpoint
	// that serves TLS (C-46).
	ConditionCertificateReady = "CertificateReady"
	// ReasonCertificateServed is the reason of a certificate that covers now.
	ReasonCertificateServed = "CertificateServed"
	// ReasonCertificateMissing is the reason of an endpoint whose issuer has
	// not written a certificate yet.
	ReasonCertificateMissing = "CertificateMissing"
	// ReasonCertificateUnreadable is the reason of a certificate that cannot
	// be read.
	ReasonCertificateUnreadable = "CertificateUnreadable"
	// ReasonCertificateExpired is the reason of a certificate that no longer
	// covers now.
	ReasonCertificateExpired = "CertificateExpired"
	// ReasonCertificateNotYetValid is the reason of a certificate whose
	// validity starts in the future.
	ReasonCertificateNotYetValid = "CertificateNotYetValid"
)

// Certificate is the observed certificate state of one endpoint (C-46).
type Certificate struct {
	// Issuer is the resolved issuer identity: the spec reference, or
	// DefaultIssuer when the spec omits it.
	Issuer string `json:"issuer"`
	// Ready reports that a certificate covering now serves the route.
	Ready bool `json:"ready"`
	// NotAfter is the expiry of the served certificate; nil means none was
	// observed.
	NotAfter *time.Time `json:"notAfter,omitempty"`
	// ExpiringSoon reports an expiry inside CertificateWarningWindow.
	ExpiringSoon bool `json:"expiringSoon,omitempty"`
	// Reason explains a certificate that is not ready.
	Reason string `json:"reason,omitempty"`
}

// certificateState observes the certificate behind one endpoint: the TLS
// secret its issuer writes, named by the route. An endpoint without TLS has no
// certificate, and one whose shape C-43 rejects is granted none.
func (r *Reconciler) certificateState(ctx context.Context, entry Entry) (*Certificate, error) {
	if entry.Spec.TLS == nil || entry.Invalid != nil || !servesCertificate(entry.Spec.Kind) {
		return nil, nil
	}
	certificate := &Certificate{Issuer: IssuerOf(entry.Spec)}
	secret, err := r.writer.Get(ctx, write.Ref{
		Version:   "v1",
		Kind:      "Secret",
		Namespace: entry.Ref.Namespace,
		Name:      entry.Ref.Name + certificateSecretSuffix,
	})
	if apierrors.IsNotFound(err) {
		certificate.Reason = ReasonCertificateMissing
		return certificate, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read certificate secret of %s: %w", entry.Ref.Name, err)
	}
	notBefore, notAfter, err := validityOf(secret)
	if err != nil {
		certificate.Reason = ReasonCertificateUnreadable
		return certificate, nil
	}
	now := r.clock()
	certificate.NotAfter = &notAfter
	switch {
	case !now.Before(notAfter):
		certificate.Reason = ReasonCertificateExpired
	case now.Before(notBefore):
		// A certificate only covers the route between its two validity bounds.
		certificate.Reason = ReasonCertificateNotYetValid
	default:
		certificate.Ready = true
		certificate.ExpiringSoon = notAfter.Sub(now) <= CertificateWarningWindow
	}
	return certificate, nil
}

// validityOf reads both validity bounds of the leaf certificate in a TLS
// secret. A secret without a readable tls.crt carries no certificate.
func validityOf(secret *unstructured.Unstructured) (time.Time, time.Time, error) {
	encoded, found, err := unstructured.NestedString(secret.Object, "data", "tls.crt")
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !found || encoded == "" {
		return time.Time{}, time.Time{}, errors.New("the secret carries no tls.crt")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	return leafValidity(raw)
}

// leafValidity parses the first certificate of a PEM chain. The leaf comes
// first in a kubernetes.io/tls secret, so the chain needs no ordering of its
// own.
func leafValidity(raw []byte) (time.Time, time.Time, error) {
	for rest := raw; len(rest) > 0; {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return time.Time{}, time.Time{}, errors.New("the certificate is not PEM encoded")
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		return certificate.NotBefore.UTC(), certificate.NotAfter.UTC(), nil
	}
	return time.Time{}, time.Time{}, errors.New("the certificate chain carries no certificate")
}

// IssuerOf resolves the issuer identity an endpoint names: the spec reference
// when it is set, the built-in issuer otherwise (C-46).
func IssuerOf(spec v1beta1.EndpointSpec) string {
	if spec.TLS == nil {
		return ""
	}
	if spec.TLS.IssuerRef != "" {
		return spec.TLS.IssuerRef
	}
	return DefaultIssuer
}

// servesCertificate reports the shapes that can carry a certificate: only the
// two HTTP forms accept tls (C-43).
func servesCertificate(kind v1beta1.EndpointKind) bool {
	return kind == v1beta1.EndpointKindHTTPHost || kind == v1beta1.EndpointKindHTTPPath
}

// CertificateSecretName is the name of the TLS secret an endpoint's issuer
// writes: the endpoint's name in its namespace, as its route names it.
func CertificateSecretName(endpoint string) string {
	return endpoint + certificateSecretSuffix
}

// certificateCondition renders the certificate state as the endpoint's
// CertificateReady condition (C-46).
func certificateCondition(certificate Certificate, generation int64) metav1.Condition {
	if certificate.Ready {
		message := "证书由 " + certificate.Issuer + " 签发"
		if certificate.NotAfter != nil {
			message += "，将于 " + certificate.NotAfter.Format(time.RFC3339) + " 到期"
			if certificate.ExpiringSoon {
				message += "（已进入到期告警窗口）"
			}
		}
		return state.Condition(ConditionCertificateReady, metav1.ConditionTrue, generation, ReasonCertificateServed, message)
	}
	return state.Condition(ConditionCertificateReady, metav1.ConditionFalse, generation, certificateReason(certificate),
		"证书未就绪："+certificateReason(certificate)+"（签发者 "+certificate.Issuer+"）")
}

// certificateReason names why a certificate is not ready.
func certificateReason(certificate Certificate) string {
	if certificate.Reason == "" {
		return ReasonCertificateMissing
	}
	return certificate.Reason
}

// certificateExpiryAlert renders the panel warning of an expiring certificate
// (C-46, C-50): the certificate's validity is the computed source, so the text
// is derived from the expiry, not copied from a status field.
func certificateExpiryAlert(certificate Certificate, now time.Time) string {
	if certificate.NotAfter == nil {
		return ""
	}
	remaining := certificate.NotAfter.Sub(now)
	if remaining <= 0 {
		return fmt.Sprintf("证书已于 %s 过期", certificate.NotAfter.Format(time.RFC3339))
	}
	if remaining > CertificateWarningWindow {
		return ""
	}
	days := int(remaining.Hours() / 24)
	if remaining > time.Duration(days)*24*time.Hour {
		days++
	}
	return fmt.Sprintf("证书将在 %d 天内到期（%s）", days, certificate.NotAfter.Format(time.RFC3339))
}
