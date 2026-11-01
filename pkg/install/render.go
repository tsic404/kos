package install

import (
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/markers"
	"github.com/tsic404/kos/pkg/write"
)

// Document is one rendered object and the resource the install writes it
// through.
type Document struct {
	// GVR is the resource the object is served under.
	GVR schema.GroupVersionResource
	// Object is the manifest.
	Object *unstructured.Unstructured
}

// Render turns a plan into the control-plane manifests an installation writes
// (C-54). Object definitions are not rendered here: the release ships them as
// generated CRD manifests and install applies those unchanged, so the
// definitions cannot drift from the API types they describe.
func Render(plan Plan) ([]Document, error) {
	plan, err := plan.Normalize()
	if err != nil {
		return nil, err
	}
	labels := map[string]any{markers.ManagedBy: write.ManagedByValue}
	documents := []Document{
		{
			GVR: schema.GroupVersionResource{Version: "v1", Resource: "namespaces"},
			Object: object("v1", "Namespace", plan.Namespace, "", map[string]any{
				"metadata": map[string]any{"labels": labels},
			}),
		},
		{
			GVR: schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"},
			Object: object("v1", "ServiceAccount", ControlPlaneServiceAccount, plan.Namespace, map[string]any{
				"metadata": map[string]any{"labels": labels},
			}),
		},
		{
			GVR:    schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			Object: clusterRole(labels),
		},
		{
			GVR:    schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
			Object: clusterRoleBinding(plan, labels),
		},
	}
	for _, namespace := range grantNamespaces(plan) {
		documents = append(documents, Document{
			GVR:    schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"},
			Object: namespaceRole(plan, namespace, labels),
		})
		documents = append(documents, Document{
			GVR:    schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"},
			Object: namespaceRoleBinding(plan, namespace, labels),
		})
	}
	documents = append(documents,
		Document{
			GVR:    schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"},
			Object: controlPlaneDeployment(plan, labels),
		},
		Document{
			GVR:    schema.GroupVersionResource{Version: "v1", Resource: "services"},
			Object: controlPlaneService(plan, labels),
		})
	documents = sortedDocuments(documents)
	return documents, nil
}

// grantNamespaces lists the namespaces the installation may write namespaced
// permissions in: its own and the namespaces the applications declared. C-04
// allocates that write, and the granted role is bound to the control-plane
// identity only, so it never carries cluster-scoped rights.
func grantNamespaces(plan Plan) []string {
	namespaces := append([]string{plan.Namespace}, plan.Applications...)
	sort.Strings(namespaces)
	return namespaces
}

// sortedDocuments orders by render order, then namespace and name, so the
// output is deterministic.
func sortedDocuments(documents []Document) []Document {
	sort.SliceStable(documents, func(left, right int) bool {
		if renderOrder(documents[left]) != renderOrder(documents[right]) {
			return renderOrder(documents[left]) < renderOrder(documents[right])
		}
		if documents[left].Object.GetNamespace() != documents[right].Object.GetNamespace() {
			return documents[left].Object.GetNamespace() < documents[right].Object.GetNamespace()
		}
		return documents[left].Object.GetName() < documents[right].Object.GetName()
	})
	return documents
}

// renderOrder keeps namespaces and permissions ahead of the workload, so an
// interrupted install never leaves a deployment whose namespace is missing.
func renderOrder(document Document) int {
	switch document.GVR.Resource {
	case "namespaces":
		return 0
	case "serviceaccounts":
		return 1
	case "clusterroles":
		return 2
	case "clusterrolebindings":
		return 3
	case "roles":
		return 4
	case "rolebindings":
		return 5
	case "deployments":
		return 6
	default:
		return 7
	}
}

// object builds a document around metadata and the fields a caller adds.
func object(apiVersion, kind, name, namespace string, fields map[string]any) *unstructured.Unstructured {
	manifest := map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
	}
	metadata := map[string]any{"name": name}
	if namespace != "" {
		metadata["namespace"] = namespace
	}
	manifest["metadata"] = metadata
	for key, value := range fields {
		if key == "metadata" {
			for metadataKey, metadataValue := range value.(map[string]any) {
				metadata[metadataKey] = metadataValue
			}
			continue
		}
		manifest[key] = value
	}
	return &unstructured.Unstructured{Object: manifest}
}

// clusterRole grants the control plane its cluster-scoped reach: the kernel
// objects it reconciles and read access to the cluster facts a projection
// shows. Cluster-scoped writes belong to the install path, never to the
// control plane (C-04), so namespaces, nodes and volumes stay read-only here.
func clusterRole(labels map[string]any) *unstructured.Unstructured {
	return object("rbac.authorization.k8s.io/v1", "ClusterRole", ControlPlaneName, "", map[string]any{
		"metadata": map[string]any{"labels": labels},
		"rules": []any{
			rule([]string{api.Group}, []string{"*"}, []string{"*"}),
			rule([]string{""}, []string{
				"namespaces", "nodes", "persistentvolumes", "serviceaccounts",
			}, []string{"get", "list", "watch"}),
			rule([]string{"apiextensions.k8s.io"}, []string{"customresourcedefinitions"}, []string{"get", "list", "watch"}),
			rule([]string{"storage.k8s.io"}, []string{"storageclasses"}, []string{"get", "list", "watch"}),
			rule([]string{"networking.k8s.io"}, []string{"ingressclasses"}, []string{"get", "list", "watch"}),
			rule([]string{"rbac.authorization.k8s.io"}, []string{"clusterroles", "clusterrolebindings"}, []string{"get", "list", "watch"}),
		},
	})
}

// namespaceRole grants the control plane the namespaced writes C-04 allocates
// to its reconcilers inside one granted namespace: rendered workloads, the
// route objects, claims, action records and backups. It never grants
// cluster-scoped kinds, because a Role cannot, and it is bound only to the
// control-plane identity.
func namespaceRole(plan Plan, namespace string, labels map[string]any) *unstructured.Unstructured {
	return object("rbac.authorization.k8s.io/v1", "Role", ControlPlaneName, namespace, map[string]any{
		"metadata": map[string]any{"labels": labels},
		"rules": []any{
			rule([]string{""}, []string{
				"configmaps", "endpoints", "events", "persistentvolumeclaims", "pods", "pods/log",
				"secrets", "services",
			}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
			rule([]string{"apps"}, []string{"deployments", "statefulsets", "daemonsets", "replicasets"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
			rule([]string{"batch"}, []string{"jobs", "cronjobs"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
			rule([]string{"networking.k8s.io"}, []string{"ingresses"}, []string{"get", "list", "watch", "create", "update", "patch", "delete"}),
			rule([]string{"policy"}, []string{"poddisruptionbudgets"}, []string{"get", "list", "watch"}),
			rule([]string{"rbac.authorization.k8s.io"}, []string{"roles", "rolebindings"}, []string{"get", "list", "watch"}),
		},
	})
}

// namespaceRoleBinding binds the control plane identity to its namespaced role.
func namespaceRoleBinding(plan Plan, namespace string, labels map[string]any) *unstructured.Unstructured {
	return object("rbac.authorization.k8s.io/v1", "RoleBinding", ControlPlaneName, namespace, map[string]any{
		"metadata": map[string]any{"labels": labels},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     ControlPlaneName,
		},
		"subjects": []any{map[string]any{
			"kind":      "ServiceAccount",
			"name":      ControlPlaneServiceAccount,
			"namespace": plan.Namespace,
		}},
	})
}

// rule builds one policy rule.
func rule(groups, resources, verbs []string) map[string]any {
	return map[string]any{"apiGroups": anySlice(groups), "resources": anySlice(resources), "verbs": anySlice(verbs)}
}

// anySlice converts a string slice to the []any an unstructured object holds.
func anySlice(values []string) []any {
	converted := make([]any, 0, len(values))
	for _, value := range values {
		converted = append(converted, value)
	}
	return converted
}

// clusterRoleBinding binds the control plane identity to its role.
func clusterRoleBinding(plan Plan, labels map[string]any) *unstructured.Unstructured {
	return object("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", ControlPlaneName, "", map[string]any{
		"metadata": map[string]any{"labels": labels},
		"roleRef": map[string]any{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "ClusterRole",
			"name":     ControlPlaneName,
		},
		"subjects": []any{map[string]any{
			"kind":      "ServiceAccount",
			"name":      ControlPlaneServiceAccount,
			"namespace": plan.Namespace,
		}},
	})
}

// controlPlaneDeployment renders the single control-plane replica. C-54 and
// F13 permit no second replica, so the count is not a plan value.
func controlPlaneDeployment(plan Plan, labels map[string]any) *unstructured.Unstructured {
	selector := map[string]any{"app": ControlPlaneName}
	return object("apps/v1", "Deployment", ControlPlaneName, plan.Namespace, map[string]any{
		"metadata": map[string]any{"labels": labels},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": selector},
			"strategy": map[string]any{"type": "Recreate"},
			"template": map[string]any{
				"metadata": map[string]any{"labels": selector},
				"spec": map[string]any{
					"serviceAccountName": ControlPlaneServiceAccount,
					"containers": []any{map[string]any{
						"name":  ControlPlaneName,
						"image": plan.Image,
						"ports": []any{map[string]any{"containerPort": int64(8080)}},
					}},
				},
			},
		},
	})
}

// controlPlaneService exposes the control-plane API inside the cluster.
func controlPlaneService(plan Plan, labels map[string]any) *unstructured.Unstructured {
	return object("v1", "Service", ControlPlaneName, plan.Namespace, map[string]any{
		"metadata": map[string]any{"labels": labels},
		"spec": map[string]any{
			"selector": map[string]any{"app": ControlPlaneName},
			"ports": []any{map[string]any{
				"name":       "http",
				"port":       int64(8080),
				"targetPort": int64(8080),
			}},
		},
	})
}
