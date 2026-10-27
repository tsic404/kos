// Package tc80 verifies that the certificate of an endpoint is a first-class
// status item: a certificate covering now, one that is missing, one whose
// validity starts later and one that has expired are distinguishable; the
// issuer in use is recorded; and a certificate inside the warning window shows
// up as an expiry alert in the network page projection (AC-40, TC-80; C-46,
// C-50).
package tc80

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC80CertificateStateAndExpiryAlert(t *testing.T) {
	cluster := harness.Start(t)
	cluster.Create(t, harness.Application("web", nil))
	reconciler := endpoint.NewReconciler(cluster.Writer(), nil)

	// Five endpoints of the same shape: a served certificate, one whose
	// issuer omitted the reference, one inside the warning window, one whose
	// validity starts later, and one that has expired.
	cluster.Create(t, harness.Endpoint("served", harness.ManagedLabels(), tlsShape("served.test", "internal")))
	cluster.Create(t, harness.Endpoint("pending", harness.ManagedLabels(), tlsShape("pending.test", "")))
	cluster.Create(t, harness.Endpoint("expiring", harness.ManagedLabels(), tlsShape("expiring.test", "")))
	cluster.Create(t, harness.Endpoint("future", harness.ManagedLabels(), tlsShape("future.test", "")))
	cluster.Create(t, harness.Endpoint("expired", harness.ManagedLabels(), tlsShape("expired.test", "")))
	for _, name := range []string{"served", "pending", "expiring", "future", "expired"} {
		harness.ReconcileEndpoint(t, reconciler, name)
	}

	// The issuer writes at the name the route declares, so the certificate
	// state and the route observe one object (C-46).
	writeCertificate(t, cluster, "served", -time.Hour, 90*24*time.Hour)
	writeCertificate(t, cluster, "expiring", -time.Hour, 10*24*time.Hour)
	writeCertificate(t, cluster, "future", time.Hour, 90*24*time.Hour)
	writeCertificate(t, cluster, "expired", -2*time.Hour, -time.Hour)
	for _, name := range []string{"served", "pending", "expiring", "future", "expired"} {
		harness.ReconcileEndpoint(t, reconciler, name)
	}

	// A served certificate is ready and records the issuer the spec named.
	served := certificateOf(t, cluster, "served")
	if !served.Ready || served.Issuer != "internal" {
		t.Fatalf("served certificate = %+v, want ready and issued by the spec issuer", served)
	}
	if served.NotAfter == nil {
		t.Fatal("served certificate carries no expiry")
	}
	requireCertificateCondition(t, cluster, "served", metav1.ConditionTrue, endpoint.ReasonCertificateServed)

	// Without a certificate the endpoint is not ready, and the status still
	// records the issuer the reconciler resolved (C-46).
	pending := certificateOf(t, cluster, "pending")
	if pending.Ready || pending.Issuer != endpoint.DefaultIssuer {
		t.Fatalf("pending certificate = %+v, want unready and issued by the built-in issuer", pending)
	}
	requireCertificateCondition(t, cluster, "pending", metav1.ConditionFalse, endpoint.ReasonCertificateMissing)

	// A certificate inside the window is ready and marked as expiring soon.
	expiring := certificateOf(t, cluster, "expiring")
	if !expiring.Ready || !expiring.ExpiringSoon {
		t.Fatalf("expiring certificate = %+v, want ready and expiring soon", expiring)
	}
	requireCertificateCondition(t, cluster, "expiring", metav1.ConditionTrue, endpoint.ReasonCertificateServed)

	// Both validity bounds decide readiness: a certificate starting later and
	// an expired one are not ready, each with its own reason.
	future := certificateOf(t, cluster, "future")
	if future.Ready {
		t.Fatalf("a certificate whose validity starts later reports ready: %+v", future)
	}
	requireCertificateCondition(t, cluster, "future", metav1.ConditionFalse, endpoint.ReasonCertificateNotYetValid)
	expired := certificateOf(t, cluster, "expired")
	if expired.Ready {
		t.Fatalf("an expired certificate reports ready: %+v", expired)
	}
	requireCertificateCondition(t, cluster, "expired", metav1.ConditionFalse, endpoint.ReasonCertificateExpired)

	// The network page projection carries the alert and points at the entry.
	rows := map[string]endpoint.Projection{}
	for _, row := range reconciler.Ledger().ProjectionAt(time.Now()) {
		rows[row.Ref.Name] = row
	}
	if alert := rows["expiring"].CertificateAlert; alert == "" {
		t.Fatalf("the projection of the expiring endpoint carries no alert: %+v", rows["expiring"])
	} else if expiring.NotAfter == nil {
		t.Fatal("expiring certificate carries no expiry")
	} else if want := expiring.NotAfter.UTC().Format(time.RFC3339); !strings.Contains(alert, want) {
		t.Fatalf("alert %q does not name the expiry %q", alert, want)
	}
	if alert := rows["expired"].CertificateAlert; !strings.Contains(alert, "过期") {
		t.Fatalf("an expired certificate carries the alert %q, want an expiry message", alert)
	}
	for _, name := range []string{"served", "future", "pending"} {
		if alert := rows[name].CertificateAlert; alert != "" {
			t.Fatalf("%s carries the expiry alert %q", name, alert)
		}
	}
}

// certificateOf reads the certificate status of one endpoint as the API
// serves it.
func certificateOf(t *testing.T, cluster *harness.Cluster, name string) *v1beta1.EndpointCertificate {
	t.Helper()
	object := cluster.Get(t, "v1beta1", "Endpoint", name)
	raw, found, err := unstructured.NestedMap(object.Object, "status", "certificate")
	if err != nil || !found {
		t.Fatalf("%s has no certificate status: found=%v err=%v", name, found, err)
	}
	certificate := &v1beta1.EndpointCertificate{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, certificate); err != nil {
		t.Fatalf("decode certificate status of %s: %v", name, err)
	}
	return certificate
}

// requireCertificateCondition requires one value and reason of the certificate
// condition.
func requireCertificateCondition(t *testing.T, cluster *harness.Cluster, name string, want metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := harness.RequireCondition(t, cluster, "Endpoint", name, endpoint.ConditionCertificateReady)
	if condition.Status != want {
		t.Fatalf("%s CertificateReady = %s (%s), want %s", name, condition.Status, condition.Message, want)
	}
	if condition.Reason != reason {
		t.Fatalf("%s CertificateReady reason = %q, want %q", name, condition.Reason, reason)
	}
	if condition.Message == "" {
		t.Fatalf("%s CertificateReady carries no message", name)
	}
}

// tlsShape builds an HTTP host endpoint with TLS; an empty issuer omits the
// reference so the reconciler resolves the built-in issuer.
func tlsShape(host, issuer string) map[string]any {
	tls := map[string]any{}
	if issuer != "" {
		tls["issuerRef"] = issuer
	}
	return map[string]any{
		"kind": "http.host", "targetApp": "web", "host": host, "tls": tls,
	}
}

// writeCertificate stores a self-signed certificate at the secret name the
// endpoint's route declares, so the kernel observes a standard TLS secret.
func writeCertificate(t *testing.T, cluster *harness.Cluster, name string, notBefore, notAfter time.Duration) {
	t.Helper()
	secretName := routeSecretName(t, cluster, name)
	certificate, key := selfSigned(t, name, notBefore, notAfter)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: harness.Namespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"tls.crt": certificate, "tls.key": key},
	}
	if _, err := cluster.Kube.CoreV1().Secrets(harness.Namespace).Create(context.Background(), secret, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create certificate secret %s: %v", secretName, err)
	}
}

// routeSecretName reads the secret name the endpoint's route declares.
func routeSecretName(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	ingress := harness.IngressOf(t, cluster, name)
	if ingress == nil {
		t.Fatalf("%s owns no ingress route", name)
	}
	if len(ingress.Spec.TLS) != 1 || ingress.Spec.TLS[0].SecretName == "" {
		t.Fatalf("the route of %s declares no TLS secret: %+v", name, ingress.Spec.TLS)
	}
	return ingress.Spec.TLS[0].SecretName
}

// selfSigned mints a certificate with the given validity bounds, relative to
// now.
func selfSigned(t *testing.T, name string, notBefore, notAfter time.Duration) ([]byte, []byte) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: name + ".test"},
		NotBefore:    time.Now().Add(notBefore),
		NotAfter:     time.Now().Add(notAfter),
		DNSNames:     []string{name + ".test"},
	}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	key, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: encoded}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})
}
