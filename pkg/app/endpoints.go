package app

import (
	"context"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// EntryNeedsHost reports whether an entry form carries a hostname (C-43). A
// package never declares one, so the user has to give it with the application
// (C-32).
func EntryNeedsHost(kind v1beta1.EndpointKind) bool {
	return kind == v1beta1.EndpointKindHTTPHost || kind == v1beta1.EndpointKindHTTPPath
}

// endpointName is the name the cascade endpoint of one declaration carries: the
// application name and the declaration name. The composition is unique within
// one application, but not across the applications of a namespace; an object
// that already carries the name is therefore judged by materializedEntry before
// it counts as materialized (C-32).
func endpointName(application, declaration string) string {
	return application + "-" + declaration
}

// materializeEndpoints creates the cascade endpoints a first installation makes
// from the package's entry declarations (C-32). It runs once: an endpoint that
// already carries the declaration's name is left untouched, so a definition
// update never changes an installed object. A declaration whose form needs a
// hostname the application does not give is skipped, because its default value
// is unavailable.
func (c *Controller) materializeEndpoints(ctx context.Context, application *v1beta1.Application, definition *v1beta1.ApplicationDefinition) error {
	for _, declaration := range definition.Spec.Endpoints {
		if EntryNeedsHost(declaration.Kind) && application.Spec.EndpointHosts[declaration.Name] == "" {
			continue
		}
		document, err := cascadeEndpoint(application, declaration)
		if err != nil {
			return err
		}
		ref := write.RefOf(document)
		existing, err := c.store.Get(ctx, ref)
		switch {
		case err == nil:
			if failure := materializedEntry(application, existing, declaration.Name); failure != nil {
				return failure
			}
			continue
		case !apierrors.IsNotFound(err):
			return err
		}
		if _, err := c.store.CreateEndpoint(ctx, application.Namespace, document); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return err
			}
			// Another writer took the name between the read and the create:
			// judge the stored object the same way.
			stored, readErr := c.store.Get(ctx, ref)
			if readErr != nil {
				return readErr
			}
			if failure := materializedEntry(application, stored, declaration.Name); failure != nil {
				return failure
			}
		}
	}
	return nil
}

// materializedEntry judges one stored endpoint that carries a declaration's
// name. The installation skips it only when it targets this application and,
// when it names an owner, was created for this instance of the application
// (C-32). Treating a collision with another application, or a leftover of a
// previous instance, as materialized would leave the declaration without its
// entry while the installation reports Ready.
func materializedEntry(application *v1beta1.Application, stored *unstructured.Unstructured, declaration string) error {
	target, _, _ := unstructured.NestedString(stored.Object, "spec", "targetApp")
	if target != application.Name {
		return &InvalidPackage{Reason: fmt.Sprintf(
			"entry %q: the endpoint %q already exists and does not target this application", declaration, stored.GetName())}
	}
	if owner := stored.GetLabels()[markers.OwnerUID]; owner != "" && owner != string(application.UID) {
		return &InvalidPackage{Reason: fmt.Sprintf(
			"entry %q: the endpoint %q was created for a previous instance of the application", declaration, stored.GetName())}
	}
	return nil
}

// cascadeEndpoint renders the endpoint object of one entry declaration with the
// attribution C-02 requires of a KOS-created object: managed-by, plus the
// application UID as the owner when the application has one. The installation
// only creates the object; its removal follows C-45 and is not performed here.
func cascadeEndpoint(application *v1beta1.Application, declaration v1beta1.EndpointDefault) (*unstructured.Unstructured, error) {
	name := endpointName(application.Name, declaration.Name)
	if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
		return nil, &InvalidPackage{Reason: fmt.Sprintf("entry %q: %s", declaration.Name, strings.Join(problems, "; "))}
	}
	labels := map[string]string{markers.ManagedBy: write.ManagedByValue}
	if application.UID != "" {
		labels[markers.OwnerUID] = string(application.UID)
	}
	endpoint := &v1beta1.Endpoint{
		TypeMeta: metav1.TypeMeta{APIVersion: api.Group + "/" + v1beta1.GroupVersion.Version, Kind: "Endpoint"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: application.Namespace,
			Labels:    labels,
		},
		Spec: v1beta1.EndpointSpec{
			Kind:        declaration.Kind,
			TargetApp:   application.Name,
			Host:        application.Spec.EndpointHosts[declaration.Name],
			Path:        declaration.Path,
			Port:        declaration.Port,
			ServiceName: declaration.ServiceName,
		},
	}
	encoded, err := runtime.DefaultUnstructuredConverter.ToUnstructured(endpoint)
	if err != nil {
		return nil, fmt.Errorf("encode the entry default %q: %w", declaration.Name, err)
	}
	// A created endpoint carries no status; the endpoint reconciler writes it
	// (C-03), so the installation must not present an empty one.
	unstructured.RemoveNestedField(encoded, "status")
	return &unstructured.Unstructured{Object: encoded}, nil
}

// ValidateDefaults checks the volume names and entry forms a package declares
// (C-32): a volume declaration is a name, an entry declaration is a name plus
// the shape fields C-43 allows, and neither carries a hostname.
func ValidateDefaults(spec v1beta1.ApplicationDefinitionSpec) error {
	volumes := make(map[string]bool, len(spec.Volumes))
	for _, name := range spec.Volumes {
		if problems := validation.IsDNS1123Label(name); len(problems) > 0 {
			return &InvalidPackage{Reason: fmt.Sprintf("volume %q: %s", name, strings.Join(problems, "; "))}
		}
		if volumes[name] {
			return &InvalidPackage{Reason: fmt.Sprintf("volume %q is declared twice", name)}
		}
		volumes[name] = true
	}
	entries := make(map[string]bool, len(spec.Endpoints))
	for _, entry := range spec.Endpoints {
		if problems := validation.IsDNS1123Label(entry.Name); len(problems) > 0 {
			return &InvalidPackage{Reason: fmt.Sprintf("entry %q: %s", entry.Name, strings.Join(problems, "; "))}
		}
		if entries[entry.Name] {
			return &InvalidPackage{Reason: fmt.Sprintf("entry %q is declared twice", entry.Name)}
		}
		entries[entry.Name] = true
		if err := validateEntryShape(entry); err != nil {
			return err
		}
	}
	return nil
}

// validateEntryShape checks one declaration against the C-43 field matrix. It
// leaves out the hostname C-43 requires of the HTTP shapes: a package never
// declares one (C-32).
func validateEntryShape(entry v1beta1.EndpointDefault) error {
	hasPath := entry.Path != ""
	hasPort := entry.Port != 0
	hasService := entry.ServiceName != ""
	problem := func(reason string) error {
		return &InvalidPackage{Reason: fmt.Sprintf("entry %q: %s", entry.Name, reason)}
	}
	if entry.Port < 0 || entry.Port > 65535 {
		return problem(fmt.Sprintf("port %d is outside 1-65535", entry.Port))
	}
	switch entry.Kind {
	case v1beta1.EndpointKindHTTPHost, v1beta1.EndpointKindHTTPPath:
		if hasPort || hasService {
			return problem("an http entry carries no port and no service name")
		}
		if entry.Kind == v1beta1.EndpointKindHTTPPath && !hasPath {
			return problem("an http.path entry needs a path")
		}
		if entry.Kind == v1beta1.EndpointKindHTTPHost && hasPath {
			return problem("an http.host entry carries no path")
		}
	case v1beta1.EndpointKindTCP, v1beta1.EndpointKindUDP:
		if !hasPort {
			return problem("a tcp or udp entry needs a port")
		}
		if hasPath || hasService {
			return problem("a tcp or udp entry carries no path and no service name")
		}
	case v1beta1.EndpointKindMDNS:
		if !hasPort || !hasService {
			return problem("an mdns entry needs a service name and a port")
		}
		if hasPath {
			return problem("an mdns entry carries no path")
		}
	default:
		return problem(fmt.Sprintf("unknown kind %q", entry.Kind))
	}
	return nil
}
