package endpoint

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// The two downstream kinds C-43 maps the endpoint shapes to.
const (
	// RouteIngress is the route object of the two HTTP shapes (C-43).
	RouteIngress = "Ingress"
	// RouteService is the route object of the two L4 shapes (C-43).
	RouteService = "Service"
)

// routeKinds are every kind one endpoint can own. A round keeps the kind the
// shape names and deletes the other, so a shape change leaves nothing behind.
var routeKinds = []string{RouteIngress, RouteService}

// errRouteNamespace reports an entry in the KOS namespace: C-04 gives that
// namespace's downstream objects to the installer, so the endpoint has no route
// to write and the state is final.
var errRouteNamespace = fmt.Errorf("the KOS namespace belongs to the installer; an endpoint there has no route object")

// routeRef names one route object of an endpoint.
func routeRef(kind, namespace, name string) write.Ref {
	ref := write.Ref{Group: "", Version: "v1", Kind: kind, Namespace: namespace, Name: name}
	if kind == RouteIngress {
		ref.Group = "networking.k8s.io"
	}
	return ref
}

// routeWriter binds the write path to one endpoint's namespace. The scope names
// that namespace; admission fixes the kinds to C-43's route objects, so this
// subject cannot widen the row into another downstream kind.
func (r *Reconciler) routeWriter(namespace string) *write.Writer {
	scoped := *r.writer
	scoped.Scope = write.Scope{Namespaces: map[string]bool{namespace: true}}
	return &scoped
}

// syncRoute makes the endpoint's downstream object match its admission (C-43).
// An admitted entry owns exactly the kind its shape maps to and nothing else; an
// unadmitted entry, mdns and a deleted entry own no route object.
func (r *Reconciler) syncRoute(ctx context.Context, endpoint *unstructured.Unstructured, entry Entry, admitted bool) error {
	if entry.Ref.Namespace == api.SystemNamespace {
		return errRouteNamespace
	}
	writer := r.routeWriter(entry.Ref.Namespace)
	var desired *unstructured.Unstructured
	if admitted {
		var err error
		if desired, err = r.routeDocument(ctx, endpoint, entry); err != nil {
			return err
		}
	}
	for _, kind := range routeKinds {
		if desired != nil && desired.GetKind() == kind {
			if _, err := writer.Apply(ctx, write.SubjectEndpointReconciler, write.Session{}, write.PartObject, desired); err != nil {
				return fmt.Errorf("apply %s %s: %w", kind, entry.Ref.Name, err)
			}
			continue
		}
		if err := deleteRoute(ctx, writer, routeRef(kind, entry.Ref.Namespace, entry.Ref.Name)); err != nil {
			return err
		}
	}
	return nil
}

// deleteRoutes removes every route kind an endpoint could own; the round that
// observes an endpoint's deletion calls it.
func (r *Reconciler) deleteRoutes(ctx context.Context, ref write.Ref) error {
	writer := r.routeWriter(ref.Namespace)
	for _, kind := range routeKinds {
		if err := deleteRoute(ctx, writer, routeRef(kind, ref.Namespace, ref.Name)); err != nil {
			return err
		}
	}
	return nil
}

// deleteRoute removes one route object; a missing one is the desired state, and
// one the allocation tables refuse to delete (an object KOS does not own, or the
// installer's namespace) is not this subject's to remove.
func deleteRoute(ctx context.Context, writer *write.Writer, ref write.Ref) error {
	err := writer.Delete(ctx, write.SubjectEndpointReconciler, write.Session{}, ref)
	if err == nil || apierrors.IsNotFound(err) {
		return nil
	}
	var denied *write.Denied
	if errors.As(err, &denied) {
		return nil
	}
	return fmt.Errorf("delete %s %s: %w", ref.Kind, ref.Name, err)
}

// routeDocument builds the downstream object of one admitted endpoint (C-43).
// mdns maps to no object: the multicast provider observes the entry itself.
func (r *Reconciler) routeDocument(ctx context.Context, endpoint *unstructured.Unstructured, entry Entry) (*unstructured.Unstructured, error) {
	spec := entry.Spec
	switch spec.Kind {
	case v1beta1.EndpointKindHTTPHost, v1beta1.EndpointKindHTTPPath:
		path := spec.Path
		if path == "" {
			// http.host carries no path; the whole host is the route.
			path = "/"
		}
		return ingressRoute(endpoint, spec, path), nil
	case v1beta1.EndpointKindTCP, v1beta1.EndpointKindUDP:
		application, err := r.writer.Get(ctx, ApplicationRef(entry.Ref.Namespace, spec.TargetApp))
		if err != nil {
			return nil, fmt.Errorf("read target application %s: %w", spec.TargetApp, err)
		}
		protocol := "TCP"
		if spec.Kind == v1beta1.EndpointKindUDP {
			protocol = "UDP"
		}
		return serviceRoute(endpoint, spec.Port, protocol, string(application.GetUID())), nil
	default:
		return nil, nil
	}
}

// ingressRoute is the route of both HTTP shapes: the host and the path prefix of
// the entry, forwarding to the service named after the target application. An
// entry that enables TLS names the secret its issuer writes, so the route and
// the certificate state C-46 reports observe the same object.
func ingressRoute(endpoint *unstructured.Unstructured, spec v1beta1.EndpointSpec, path string) *unstructured.Unstructured {
	document := routeObject(endpoint, RouteIngress)
	route := map[string]any{
		"rules": []any{map[string]any{
			"host": spec.Host,
			"http": map[string]any{"paths": []any{map[string]any{
				"path":     path,
				"pathType": "Prefix",
				"backend": map[string]any{"service": map[string]any{
					"name": spec.TargetApp,
					"port": map[string]any{"name": "http"},
				}},
			}}},
		}},
	}
	if spec.TLS != nil {
		route["tls"] = []any{map[string]any{
			"hosts":      []any{spec.Host},
			"secretName": CertificateSecretName(endpoint.GetName()),
		}}
	}
	document.Object["spec"] = route
	return document
}

// serviceRoute is the route of both L4 shapes: a load-balanced service that
// exposes the entry's port on the target application's workloads.
func serviceRoute(endpoint *unstructured.Unstructured, port int32, protocol, appUID string) *unstructured.Unstructured {
	document := routeObject(endpoint, RouteService)
	document.Object["spec"] = map[string]any{
		"type":     "LoadBalancer",
		"selector": map[string]any{markers.OwnerUID: appUID},
		"ports": []any{map[string]any{
			"name":       strings.ToLower(protocol),
			"port":       int64(port),
			"targetPort": int64(port),
			"protocol":   protocol,
		}},
	}
	return document
}

// routeObject is the identity and attribution every route object carries: the
// entry's name in its namespace, managed-by and owner-uid equal to the entry's
// UID (C-02, C-43).
func routeObject(endpoint *unstructured.Unstructured, kind string) *unstructured.Unstructured {
	apiVersion := "v1"
	if kind == RouteIngress {
		apiVersion = "networking.k8s.io/v1"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":      endpoint.GetName(),
			"namespace": endpoint.GetNamespace(),
			"labels": map[string]any{
				markers.ManagedBy: write.ManagedByValue,
				markers.OwnerUID:  string(endpoint.GetUID()),
			},
		},
	}}
}
