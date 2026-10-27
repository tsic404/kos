package app

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/tsic404/kos/api/v1beta1"
)

// InvalidPackage marks a package that violates C-32: a storage class it may not
// declare, a render outside the declared namespace, a malformed template. The
// application fails without spending its retry budget, because the same package
// cannot start working by being retried.
type InvalidPackage struct {
	// Reason names the violated rule.
	Reason string
}

// Error reports the package rule the definition broke.
func (e *InvalidPackage) Error() string {
	return "invalid application package: " + e.Reason
}

// RenderInput is everything a translation template may read (C-32). It carries
// no clock, no discovery and no cluster access, so rendering stays a pure
// function.
type RenderInput struct {
	// Name is the application name; rendered objects take their names from it.
	Name string
	// Namespace is the declared namespace every rendered object lands in.
	Namespace string
	// Config is the resolved config, with the definition's defaults applied.
	Config map[string]any
	// Volumes maps a declared volume name to the claim name the volume object
	// creates (C-32, C-38).
	Volumes map[string]string
}

// Render runs the one-way translation template of a definition (C-32). The same
// definition and input always produce the same objects in the same order.
func Render(spec v1beta1.ApplicationDefinitionSpec, input RenderInput) ([]*unstructured.Unstructured, error) {
	if strings.TrimSpace(spec.Template) == "" {
		return nil, &InvalidPackage{Reason: "the definition carries no translation template"}
	}
	if err := ValidateSchema(spec.Config); err != nil {
		return nil, &InvalidPackage{Reason: "the config structure is invalid: " + err.Error()}
	}
	parsed, err := template.New("translation").Option("missingkey=error").Parse(spec.Template)
	if err != nil {
		return nil, &InvalidPackage{Reason: "the translation template does not parse: " + err.Error()}
	}
	var rendered bytes.Buffer
	if err := parsed.Execute(&rendered, input); err != nil {
		return nil, &InvalidPackage{Reason: "the translation template failed: " + err.Error()}
	}
	objects, err := decodeObjects(rendered.Bytes())
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, &InvalidPackage{Reason: "the translation template renders no object"}
	}
	for _, object := range objects {
		if err := confine(object, input.Namespace); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

// decodeObjects splits a rendered document set into objects. A document the
// template cannot express as an object is an invalid package, not a runtime
// failure to retry.
func decodeObjects(document []byte) ([]*unstructured.Unstructured, error) {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(document), 4096)
	objects := make([]*unstructured.Unstructured, 0, 2)
	for {
		var raw map[string]any
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return objects, nil
		}
		if err != nil {
			return nil, &InvalidPackage{Reason: "the rendered document is not parseable: " + err.Error()}
		}
		if len(raw) == 0 {
			continue
		}
		object := &unstructured.Unstructured{Object: raw}
		if object.GetAPIVersion() == "" || object.GetKind() == "" {
			return nil, &InvalidPackage{Reason: "a rendered document carries no apiVersion or kind"}
		}
		if object.GetKind() == "List" || strings.HasSuffix(object.GetKind(), "List") {
			return nil, &InvalidPackage{Reason: "a rendered list must be written as separate documents"}
		}
		if object.GetName() == "" {
			return nil, &InvalidPackage{Reason: fmt.Sprintf("the rendered %s carries no name", object.GetKind())}
		}
		objects = append(objects, object)
	}
}

// confine applies the namespace rule of C-32: every rendered object lands in the
// declared namespace, and an object that names another one is an illegal
// package. It also refuses the volume declarations a package may not carry.
func confine(object *unstructured.Unstructured, namespace string) error {
	if namespace == "" {
		return &InvalidPackage{Reason: "the application declares no namespace"}
	}
	// A cluster-scoped kind has no namespace to confine it to: filling one in
	// would defeat the rule and the write would fail only at the API server
	// (C-32). The check is a static table because rendering is a pure function
	// and cannot consult discovery.
	gvk := object.GroupVersionKind()
	if clusterScopedKinds[schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}] {
		return &InvalidPackage{Reason: fmt.Sprintf(
			"%s %s is cluster scoped; a package renders only inside the declared namespace (C-32)",
			object.GetKind(), object.GetName())}
	}
	switch current := object.GetNamespace(); current {
	case "":
		object.SetNamespace(namespace)
	case namespace:
	default:
		return &InvalidPackage{Reason: fmt.Sprintf(
			"%s %s renders into namespace %q, outside the declared %q",
			object.GetKind(), object.GetName(), current, namespace)}
	}
	if object.GetKind() == "PersistentVolumeClaim" {
		return &InvalidPackage{Reason: fmt.Sprintf(
			"%s %s is a volume template; the volume object creates the claim (C-32, C-38)",
			object.GetKind(), object.GetName())}
	}
	for _, key := range claimTemplateKeys {
		if path, declared := findField(object.Object, key, nil); declared {
			return &InvalidPackage{Reason: fmt.Sprintf(
				"%s %s declares a volume claim template at %s; a package may not carry them (C-32)",
				object.GetKind(), object.GetName(), path)}
		}
	}
	if path, found := findField(object.Object, "storageClassName", nil); found {
		return &InvalidPackage{Reason: fmt.Sprintf(
			"%s %s declares a storage class at %s; the volume object owns it (C-32, F4)",
			object.GetKind(), object.GetName(), path)}
	}
	return nil
}

// clusterScopedKinds lists the native kinds a package may not render. Only the
// kinds a package could plausibly name are listed; a kind outside the table is
// confined by its namespace like any other (C-32).
var clusterScopedKinds = map[schema.GroupKind]bool{
	{Group: "", Kind: "Namespace"}:                                                    true,
	{Group: "", Kind: "Node"}:                                                         true,
	{Group: "", Kind: "PersistentVolume"}:                                             true,
	{Group: "", Kind: "ComponentStatus"}:                                              true,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:                         true,
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRoleBinding"}:                  true,
	{Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition"}:                 true,
	{Group: "storage.k8s.io", Kind: "StorageClass"}:                                   true,
	{Group: "storage.k8s.io", Kind: "VolumeAttachment"}:                               true,
	{Group: "storage.k8s.io", Kind: "CSIDriver"}:                                      true,
	{Group: "storage.k8s.io", Kind: "CSINode"}:                                        true,
	{Group: "scheduling.k8s.io", Kind: "PriorityClass"}:                               true,
	{Group: "node.k8s.io", Kind: "RuntimeClass"}:                                      true,
	{Group: "networking.k8s.io", Kind: "IngressClass"}:                                true,
	{Group: "networking.k8s.io", Kind: "IPAddress"}:                                   true,
	{Group: "networking.k8s.io", Kind: "ServiceCIDR"}:                                 true,
	{Group: "admissionregistration.k8s.io", Kind: "MutatingWebhookConfiguration"}:     true,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingWebhookConfiguration"}:   true,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicy"}:        true,
	{Group: "admissionregistration.k8s.io", Kind: "ValidatingAdmissionPolicyBinding"}: true,
	{Group: "apiregistration.k8s.io", Kind: "APIService"}:                             true,
	{Group: "certificates.k8s.io", Kind: "CertificateSigningRequest"}:                 true,
	{Group: "certificates.k8s.io", Kind: "ClusterTrustBundle"}:                        true,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "FlowSchema"}:                       true,
	{Group: "flowcontrol.apiserver.k8s.io", Kind: "PriorityLevelConfiguration"}:       true,
	{Group: "policy", Kind: "PodSecurityPolicy"}:                                      true,
}

// claimTemplateKeys are the keys a package could use to carry a volume claim
// template: StatefulSet spells it plural, an ephemeral volume singular (C-32).
var claimTemplateKeys = []string{"volumeClaimTemplates", "volumeClaimTemplate"}

// findField looks for a non-empty key anywhere below node and returns its path.
// Keys are visited in sorted order, so the same object always names the same
// offending path.
func findField(node any, key string, path []string) (string, bool) {
	switch typed := node.(type) {
	case map[string]any:
		names := make([]string, 0, len(typed))
		for name := range typed {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if len(path) == 1 && path[0] == "metadata" && (name == "labels" || name == "annotations") {
				// A label or annotation key is data, not a desired field: a package
				// may carry "storageClassName" on that carrier (C-32).
				continue
			}
			value := typed[name]
			child := append(append([]string{}, path...), name)
			if name == key {
				if text, isText := value.(string); !isText || text != "" {
					return strings.Join(child, "."), true
				}
				continue
			}
			if found, ok := findField(value, key, child); ok {
				return found, true
			}
		}
	case []any:
		for index, value := range typed {
			child := append(append([]string{}, path...), fmt.Sprintf("[%d]", index))
			if found, ok := findField(value, key, child); ok {
				return found, true
			}
		}
	}
	return "", false
}
