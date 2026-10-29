package harness

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/diagnostics"
	"github.com/tsic404/kos/pkg/extension"
)

// WidgetGroup is the extension group the diagnostics cases install (C-19).
const WidgetGroup = "widgets.example.test"

// Event names the diagnostics cases create: three inside the one-hour window
// (one about the node) and one outside it (C-64).
const (
	EventNewest        = "shop-backend-unavailable"
	EventNode          = "shop-node-not-ready"
	EventWithinWindow  = "shop-endpoint-warning"
	EventOutsideWindow = "shop-old-warning"
)

// DiagnosticNamespace holds the objects of the diagnostics world. It is not the
// acceptance namespace on purpose: a cluster-scoped object's events are stored
// in the default namespace, so only a read by object finds them (C-64).
const DiagnosticNamespace = "shop-apps"

// WidgetIdentifier is the extension identity the diagnostic adapter covers.
const WidgetIdentifier = "demo.widgets"

// DiagnosticWorld is a degraded application the diagnostics cases read: one
// endpoint, its route, a backend with no ready pod, a workload with no ready
// replica, a NotReady node and one extension object a diagnostic adapter
// observes (C-26).
type DiagnosticWorld struct {
	// Installation is the extension install pass the world starts with.
	Installation *extension.Installation
	// Artifacts resolve the extension package again after a component removal.
	Artifacts extension.MemoryArtifacts
	// Component is the system component name of the extension.
	Component string
	// Namespace is where the application and its objects live.
	Namespace string
	// Application is the application name.
	Application string
	// ConfigSecret is the sensitive config value the application submits.
	ConfigSecret string
	// CredentialSecret is the value stored in a Secret; the bundle must never
	// carry it (C-14).
	CredentialSecret string
}

// StartDiagnosticWorld installs the widget extension and shapes the objects of
// the degraded chain. The cases then read the chain or the bundle over them.
func StartDiagnosticWorld(t *testing.T, cluster *Cluster) *DiagnosticWorld {
	t.Helper()
	cluster.InstallCRD(t, WidgetGroup, "v1", "Widget", "widgets", true)
	declaration := extension.Declaration{
		Identifier: "demo.widgets",
		Version:    "1.0.0",
		APIGroup:   WidgetGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: WidgetGroup, Kind: "Widget", Resource: "widgets",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			DiagnosticAdapters: []extension.DiagnosticAdapter{{
				ID:         "widget-diagnostics",
				Covers:     []extension.GroupKind{{Group: WidgetGroup, Kind: "Widget"}},
				Components: []string{WidgetIdentifier},
				Segment:    string(diagnostics.LinkWorkload),
				Source:     "widgets.example.test diagnostics",
			}},
			Capabilities: []extension.Capability{{Name: "demo.widgets/diagnostics", Version: "v1"}},
		},
		FrameworkVersion: ">=1.0 <2.0",
	}
	artifacts := extension.MemoryArtifacts{}
	installation := cluster.InstallPackage(t, "widgets", declaration, artifacts)

	definition := diagnosticDefinition(t)
	cluster.InstallDefinition(t, definition)
	cluster.EnsureNamespace(t, DiagnosticNamespace)
	cluster.CreateIn(t, DiagnosticNamespace, diagnosticApplication())
	cluster.CreateIn(t, DiagnosticNamespace, diagnosticEndpoint())
	cluster.installObject(t, "networking.k8s.io", "v1", "ingresses", diagnosticIngress(), false)
	// The objects a package renders are server-side applied, so the render diff
	// reads the fields the applying manager owns (C-32).
	cluster.applyObject(t, "", "v1", "services", diagnosticService(), true)
	cluster.applyObject(t, "apps", "v1", "deployments", diagnosticDeployment(), true)
	cluster.installObject(t, "", "v1", "pods", diagnosticPod(), true)
	cluster.installObject(t, "", "v1", "nodes", diagnosticNode(), true)
	cluster.applyObject(t, WidgetGroup, "v1", "widgets", diagnosticWidget(), false)
	// The node's event sits in the default namespace the API server records
	// cluster-scoped objects in, outside the world's namespace, so only a read
	// by object finds it (C-64).
	cluster.installObject(t, "", "v1", "events", diagnosticEvent(EventNewest, time.Now()), false)
	cluster.installObject(t, "", "v1", "events", diagnosticNodeEvent(time.Now().Add(-20*time.Minute)), false)
	cluster.installObject(t, "", "v1", "events", diagnosticEvent(EventWithinWindow, time.Now().Add(-30*time.Minute)), false)
	cluster.installObject(t, "", "v1", "events", diagnosticEvent(EventOutsideWindow, time.Now().Add(-2*time.Hour)), false)
	cluster.installObject(t, "", "v1", "secrets", diagnosticSecret(), false)

	return &DiagnosticWorld{
		Installation:     installation,
		Artifacts:        artifacts,
		Component:        "widgets",
		Namespace:        DiagnosticNamespace,
		Application:      "shop",
		ConfigSecret:     "config-secret-7f3a",
		CredentialSecret: "bundle-credential-2b8e",
	}
}

// Service builds the diagnostics service over this cluster from one
// installation's registry (C-26).
func (c *Cluster) DiagnosticService(installation *extension.Installation) *diagnostics.Service {
	environment := diagnostics.Environment{
		Adapters:     diagnosticAdapters(installation.Registry()),
		Capabilities: diagnosticCapabilities(installation.Registry()),
		Components:   diagnosticComponents(installation),
	}
	return &diagnostics.Service{
		Source:      diagnostics.ClusterSource{Dynamic: c.Dynamic},
		Environment: environment,
	}
}

// StopExtension removes the component and runs another install pass, which
// withdraws its contributions (C-28).
func (w *DiagnosticWorld) StopExtension(t *testing.T, cluster *Cluster) *extension.Installation {
	t.Helper()
	cluster.DeleteComponent(t, w.Component)
	return cluster.ExtensionState(t, w.Artifacts)
}

// diagnosticAdapters projects the active diagnostic adapters into the class
// bindings the chain reads.
func diagnosticAdapters(registry *extension.Registry) []diagnostics.Adapter {
	classes := registry.ObjectClasses()
	adapters := make([]diagnostics.Adapter, 0)
	for _, adapter := range registry.DiagnosticAdapters() {
		for _, cover := range adapter.Covers {
			for _, class := range classes {
				if class.GroupKind() != cover || len(class.Versions) == 0 {
					continue
				}
				adapters = append(adapters, diagnostics.Adapter{
					ID: adapter.ID, Group: cover.Group, Kind: cover.Kind,
					Version: class.Versions[len(class.Versions)-1], Resource: class.Resource,
					Segment: diagnostics.Link(adapter.Segment), Source: adapter.Source,
					Components: append([]string(nil), adapter.Components...),
				})
			}
		}
	}
	return adapters
}

// diagnosticCapabilities projects the capability catalogue, attributing each
// extension entry to the identifier its name is prefixed with (C-09).
func diagnosticCapabilities(registry *extension.Registry) []diagnostics.Capability {
	entries := make([]diagnostics.Capability, 0)
	for _, capability := range registry.Capabilities() {
		entries = append(entries, diagnostics.Capability{
			Name: capability.Name, Version: capability.Version,
			Source: capabilitySource(capability.Name),
		})
	}
	return entries
}

// capabilitySource returns the component a capability name names.
func capabilitySource(name string) string {
	if index := strings.Index(name, "/"); index > 0 {
		return name[:index]
	}
	return diagnostics.KernelSource
}

// diagnosticComponents projects the install results into the version matrix.
func diagnosticComponents(installation *extension.Installation) []diagnostics.Component {
	components := make([]diagnostics.Component, 0)
	for _, result := range installation.Components() {
		if result.Identifier == "" {
			continue
		}
		components = append(components, diagnostics.Component{
			Name: result.Name, Identifier: result.Identifier, Version: result.Version,
			FrameworkVersion: result.FrameworkVersion,
			Phase:            result.Phase, Health: string(result.Health),
		})
	}
	return components
}

// diagnosticDefinition renders the service, the workload and the widget the
// world installs, and declares one sensitive config field (C-32).
func diagnosticDefinition(t *testing.T) *v1beta1.ApplicationDefinition {
	t.Helper()
	definition := &v1beta1.ApplicationDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: api.Group + "/v1beta1", Kind: "ApplicationDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: "shop-definition"},
		Spec: v1beta1.ApplicationDefinitionSpec{
			Package: v1beta1.PackageIdentity{Version: "1.0.0"},
			Config: []v1beta1.ConfigField{{
				Name: "password", Type: "string", Sensitive: true,
			}},
			Template: diagnosticTemplate,
		},
	}
	digest, err := app.DefinitionDigest(definition.Spec)
	if err != nil {
		t.Fatalf("digest the diagnostic definition: %v", err)
	}
	definition.Spec.Package.Digest = digest
	return definition
}

// diagnosticTemplate is the package the world installs.
const diagnosticTemplate = `apiVersion: v1
kind: Service
metadata:
  name: {{ .Name }}-backend
  namespace: {{ .Namespace }}
spec:
  selector:
    app: {{ .Name }}
  ports:
  - port: 80
    targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  replicas: 2
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
      - name: app
        image: nginx:1.27
        env:
        - name: PASSWORD
          value: {{ .Config.password }}
---
apiVersion: widgets.example.test/v1
kind: Widget
metadata:
  name: {{ .Name }}-widget
  namespace: {{ .Namespace }}
---
apiVersion: v1
kind: Secret
metadata:
  name: {{ .Name }}-credential
  namespace: {{ .Namespace }}
type: Opaque
stringData:
  password: {{ .Config.password }}
`

// diagnosticApplication is the degraded application: Ready with health
// Degraded and the three rendered objects.
func diagnosticApplication() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata": map[string]any{
			"name": "shop", "namespace": DiagnosticNamespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{
			"definitionRef": "shop-definition",
			"namespace":     DiagnosticNamespace,
			"config":        map[string]any{"password": "config-secret-7f3a"},
		},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": "Degraded"},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Applied",
				"message": "desired state applied", "lastTransitionTime": "2026-10-25T00:00:00Z",
			}},
			"rendered": []any{
				map[string]any{"version": "v1", "kind": "Service", "namespace": DiagnosticNamespace, "name": "shop-backend"},
				map[string]any{"group": "apps", "version": "v1", "kind": "Deployment", "namespace": DiagnosticNamespace, "name": "shop"},
				map[string]any{"group": WidgetGroup, "version": "v1", "kind": "Widget", "namespace": DiagnosticNamespace, "name": "shop-widget"},
				map[string]any{"version": "v1", "kind": "Secret", "namespace": DiagnosticNamespace, "name": "shop-credential"},
			},
		},
	}}
}

// diagnosticEndpoint is the admitted http.host endpoint of the application.
func diagnosticEndpoint() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Endpoint",
		"metadata": map[string]any{
			"name": "shop-entry", "namespace": DiagnosticNamespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{"kind": "http.host", "targetApp": "shop", "host": "shop.example.test"},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": "Healthy"},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Applied",
				"message": "route object applied", "lastTransitionTime": "2026-10-25T00:00:00Z",
			}},
		},
	}}
}

// diagnosticIngress is the route object the endpoint owns (C-43).
func diagnosticIngress() map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "Ingress",
		"metadata":   map[string]any{"name": "shop-entry", "namespace": DiagnosticNamespace},
		"spec": map[string]any{
			"rules": []any{map[string]any{
				"host": "shop.example.test",
				"http": map[string]any{"paths": []any{map[string]any{
					"path":     "/",
					"pathType": "Prefix",
					"backend": map[string]any{"service": map[string]any{
						"name": "shop-backend",
						"port": map[string]any{"number": 80},
					}},
				}}},
			}},
		},
	}
}

// diagnosticService is the backend service the route points at.
func diagnosticService() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata":   map[string]any{"name": "shop-backend", "namespace": DiagnosticNamespace},
		"spec": map[string]any{
			"selector": map[string]any{"app": "shop"},
			"ports":    []any{map[string]any{"port": 80, "targetPort": 8080}},
		},
	}
}

// diagnosticDeployment is the workload the definition renders, with no ready
// replica and an env value that differs from the render.
func diagnosticDeployment() map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": "shop", "namespace": DiagnosticNamespace,
			"labels": map[string]any{"legacy": "old"},
		},
		"spec": map[string]any{
			"replicas": 2,
			"selector": map[string]any{"matchLabels": map[string]any{"app": "shop"}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "shop"}},
				"spec": map[string]any{"containers": []any{map[string]any{
					"name": "app", "image": "nginx:1.27",
					"env": []any{map[string]any{"name": "PASSWORD", "value": "placeholder"}},
				}}},
			},
		},
		"status": map[string]any{"replicas": 2, "readyReplicas": 0, "unavailableReplicas": 2},
	}
}

// diagnosticPod is the workload's only instance, scheduled on the NotReady
// node.
func diagnosticPod() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": "shop-9d7c", "namespace": DiagnosticNamespace,
			"labels": map[string]any{"app": "shop"},
		},
		"spec": map[string]any{
			"nodeName":   "node-a",
			"containers": []any{map[string]any{"name": "app", "image": "nginx:1.27"}},
		},
		"status": map[string]any{
			"phase": "Running",
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "ContainersNotReady",
				"message": "containers with unready status: [app]",
			}},
		},
	}
}

// diagnosticNode is the node the pod runs on.
func diagnosticNode() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": "node-a"},
		"status": map[string]any{
			"conditions": []any{map[string]any{
				"type": "Ready", "status": "False", "reason": "NodeNotReady",
				"message": "node is not ready",
			}},
			"nodeInfo": map[string]any{"kubeletVersion": "v1.37.0", "osImage": "kind"},
		},
	}
}

// diagnosticWidget is the extension object on the chain.
func diagnosticWidget() map[string]any {
	return map[string]any{
		"apiVersion": WidgetGroup + "/v1",
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "shop-widget", "namespace": DiagnosticNamespace},
		"spec":       map[string]any{},
		"status": map[string]any{
			"phase":  "Ready",
			"health": map[string]any{"state": "Healthy"},
			"conditions": []any{map[string]any{
				"type": "Reconciled", "status": "True", "reason": "Applied", "message": "widget accepted",
			}},
		},
	}
}

// diagnosticEvent is one warning event for the application, stamped at when.
func diagnosticEvent(name string, when time.Time) map[string]any {
	stamp := when.UTC().Format(time.RFC3339)
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": name, "namespace": DiagnosticNamespace},
		"involvedObject": map[string]any{
			"kind": "Application", "namespace": DiagnosticNamespace, "name": "shop",
		},
		"type":               "Warning",
		"reason":             "BackendUnavailable",
		"message":            "no ready backend for the endpoint",
		"count":              1,
		"firstTimestamp":     stamp,
		"lastTimestamp":      stamp,
		"reportingComponent": "kos-shell",
		"reportingInstance":  "kos-shell-1",
	}
}

// diagnosticNodeEvent is a cluster-scoped object's event: the API server keeps
// it in the default namespace, outside the world's namespace, and it must still
// be read (C-64).
func diagnosticNodeEvent(when time.Time) map[string]any {
	stamp := when.UTC().Format(time.RFC3339)
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]any{"name": EventNode, "namespace": Namespace},
		"involvedObject": map[string]any{
			"kind": "Node", "name": "node-a",
		},
		"type":               "Warning",
		"reason":             "NodeNotReady",
		"message":            "node is not ready",
		"count":              1,
		"firstTimestamp":     stamp,
		"lastTimestamp":      stamp,
		"reportingComponent": "kos-shell",
		"reportingInstance":  "kos-shell-1",
	}
}

// diagnosticSecret is a credential store the bundle must never read (C-14).
func diagnosticSecret() map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata":   map[string]any{"name": "shop-credentials", "namespace": DiagnosticNamespace},
		"type":       "Opaque",
		"data": map[string]any{
			"password": base64.StdEncoding.EncodeToString([]byte("bundle-credential-2b8e")),
		},
	}
}

// installObject creates one object and writes its status when it carries one, so
// the chain reads the state a controller would have written. hasStatusSubresource
// tells whether the class serves status through its own subresource; a class
// without one keeps status in the object body.
func (c *Cluster) installObject(t *testing.T, group, version, resource string, object map[string]any, hasStatusSubresource bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	client := c.Dynamic.Resource(schema.GroupVersionResource{Group: group, Version: version, Resource: resource})
	var writable dynamic.ResourceInterface = client
	metadata, _ := object["metadata"].(map[string]any)
	namespace, _ := metadata["namespace"].(string)
	if namespace != "" {
		writable = client.Namespace(namespace)
	}
	created, err := writable.Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create %s %s: %v", resource, metadata["name"], err)
	}
	status, carries := object["status"]
	if !carries || !hasStatusSubresource {
		return
	}
	created.Object["status"] = status
	if _, err := writable.UpdateStatus(ctx, created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write status of %s %s: %v", resource, metadata["name"], err)
	}
}

// applyObject server-side applies one object as the application reconciler
// would, so its managedFields record the field ownership a render diff reads.
// A class with a status subresource keeps its status out of the apply and gets
// it written separately.
func (c *Cluster) applyObject(t *testing.T, group, version, resource string, object map[string]any, hasStatusSubresource bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	client := c.Dynamic.Resource(schema.GroupVersionResource{Group: group, Version: version, Resource: resource})
	metadata, _ := object["metadata"].(map[string]any)
	namespace, _ := metadata["namespace"].(string)
	name, _ := metadata["name"].(string)
	var writable dynamic.ResourceInterface = client
	if namespace != "" {
		writable = client.Namespace(namespace)
	}
	status, carriesStatus := object["status"]
	document := &unstructured.Unstructured{Object: object}
	if hasStatusSubresource {
		delete(document.Object, "status")
	}
	applied, err := writable.Apply(ctx, name, document, metav1.ApplyOptions{FieldManager: "application-reconciler"})
	if err != nil {
		t.Fatalf("apply %s %s: %v", resource, name, err)
	}
	if !carriesStatus || !hasStatusSubresource {
		return
	}
	applied.Object["status"] = status
	if _, err := writable.UpdateStatus(ctx, applied, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write status of %s %s: %v", resource, name, err)
	}
}
