package harness

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/pkg/write"
)

// Endpoint returns a namespaced endpoint with the given attribution labels and
// spec and no status, so the reconciler is its only status writer.
func Endpoint(name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	return EndpointIn(Namespace, name, labels, spec)
}

// EndpointIn is Endpoint in an explicit namespace.
func EndpointIn(namespace, name string, labels map[string]string, spec map[string]any) *unstructured.Unstructured {
	manifest := Manifest("Endpoint", namespace, name, labels, nil)
	manifest["spec"] = spec
	return &unstructured.Unstructured{Object: manifest}
}

// ManagedLabels is the attribution a case uses for the objects KOS owns.
func ManagedLabels() map[string]string {
	return map[string]string{"managed-by": "acceptance"}
}

// Declarations is the C-43 declaration seam the endpoint cases configure: the
// path-prefix capability is registered and the named applications declare path
// safety. The kernel objects that will hold these facts — the Provider
// registration (C-47) and the application package (C-32) — are other contracts'
// fields; the endpoint cases supply the same answers through this seam.
type Declarations struct {
	// Capable reports whether network.path-prefix is registered.
	Capable bool
	// Safe names the applications whose package declares path safety.
	Safe []string
}

// PathSafe implements endpoint.Facts.
func (d Declarations) PathSafe(_ context.Context, app write.Ref) (bool, error) {
	for _, name := range d.Safe {
		if name == app.Name {
			return true, nil
		}
	}
	return false, nil
}

// CapabilityRegistered implements endpoint.Facts.
func (d Declarations) CapabilityRegistered(_ context.Context, capability string) (bool, error) {
	return d.Capable && capability == endpoint.PathPrefixCapability, nil
}

// ReconcileEndpoint runs one endpoint round and fails the case on error.
func ReconcileEndpoint(t *testing.T, r *endpoint.Reconciler, name string) {
	t.Helper()
	ReconcileEndpointIn(t, r, Namespace, name)
}

// ReconcileEndpointIn is ReconcileEndpoint in an explicit namespace.
func ReconcileEndpointIn(t *testing.T, r *endpoint.Reconciler, namespace, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{Kind: "Endpoint", Namespace: namespace, Name: name}); err != nil {
		t.Fatalf("reconcile endpoint %s/%s: %v", namespace, name, err)
	}
}

// ReconcileApplication runs one application round and fails the case on error.
func ReconcileApplication(t *testing.T, r *endpoint.Reconciler, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), reconcile.Request{Kind: "Application", Namespace: Namespace, Name: name}); err != nil {
		t.Fatalf("reconcile application %s: %v", name, err)
	}
}

// FindCondition reads one condition of a stored object.
func FindCondition(t *testing.T, cluster *Cluster, kind, name, conditionType string) (metav1.Condition, bool) {
	t.Helper()
	object := cluster.Get(t, "v1beta1", kind, name)
	return state.Find(Conditions(t, object), conditionType)
}

// RequireCondition returns one condition and fails when it is missing.
func RequireCondition(t *testing.T, cluster *Cluster, kind, name, conditionType string) metav1.Condition {
	t.Helper()
	condition, ok := FindCondition(t, cluster, kind, name, conditionType)
	if !ok {
		t.Fatalf("%s carries no %s condition", name, conditionType)
	}
	return condition
}

// RequireReconciled requires one value of the Reconciled condition.
func RequireReconciled(t *testing.T, cluster *Cluster, kind, name string, want metav1.ConditionStatus) {
	t.Helper()
	condition := RequireCondition(t, cluster, kind, name, api.ConditionReconciled)
	if condition.Status != want {
		t.Fatalf("%s Reconciled = %s (%s), want %s", name, condition.Status, condition.Message, want)
	}
}

// Delete removes an object as the user's native write would, into the cluster
// that holds it.
func Delete(t *testing.T, cluster *Cluster, kind, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cluster.Direct(t, api.Group, "v1beta1", kind, Namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete %s %s: %v", kind, name, err)
	}
}

// HealthOf reads the health state of a stored object.
func HealthOf(t *testing.T, cluster *Cluster, kind, name string) string {
	t.Helper()
	object := cluster.Get(t, "v1beta1", kind, name)
	state, _ := Nested(object.Object, "status", "health", "state").(string)
	return state
}

// IngressOf returns one ingress of the acceptance namespace, or nil when it does
// not exist.
func IngressOf(t *testing.T, cluster *Cluster, name string) *networkingv1.Ingress {
	t.Helper()
	return IngressOfIn(t, cluster, Namespace, name)
}

// IngressOfIn is IngressOf in an explicit namespace.
func IngressOfIn(t *testing.T, cluster *Cluster, namespace, name string) *networkingv1.Ingress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ingress, err := cluster.Kube.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read ingress %s/%s: %v", namespace, name, err)
	}
	return ingress
}

// CreateUnmanagedIngress writes an ingress no KOS subject owns, so a case can
// put an object in the route's place.
func CreateUnmanagedIngress(t *testing.T, cluster *Cluster, namespace, name string) *networkingv1.Ingress {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ingress, err := cluster.Kube.NetworkingV1().Ingresses(namespace).Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{
			Service: &networkingv1.IngressServiceBackend{
				Name: "elsewhere",
				Port: networkingv1.ServiceBackendPort{Number: 80},
			},
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create unmanaged ingress %s/%s: %v", namespace, name, err)
	}
	return ingress
}

// RemoveIngress deletes one ingress of a namespace.
func RemoveIngress(t *testing.T, cluster *Cluster, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := cluster.Kube.NetworkingV1().Ingresses(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete ingress %s/%s: %v", namespace, name, err)
	}
}

// ServiceOf returns one service of the acceptance namespace, or nil when it does
// not exist.
func ServiceOf(t *testing.T, cluster *Cluster, name string) *corev1.Service {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	service, err := cluster.Kube.CoreV1().Services(Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read service %s: %v", name, err)
	}
	return service
}
