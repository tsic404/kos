// Package cluster reads the cluster objects, nodes and events a page source
// judges, and applies the session visibility rules C-59 and C-64 fix. Every
// read goes through the control plane's client (C-12); a product package never
// holds a credential of its own.
package cluster

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/access"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
)

// nodes is the native node class; nodes are not a kernel kind, so the resource
// API cannot serve them (C-49).
var nodes = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}

// Reader reads cluster state for one page source.
type Reader struct {
	// Dynamic is the client every read goes through.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
}

// Clock returns the moment a read observes its values (C-13).
func (r *Reader) Clock() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// KernelType resolves a kernel kind to the class it is served under. An unknown
// kind is a declaration the control plane cannot answer (C-01).
func KernelType(kind string) (controlplane.ObjectType, *controlplane.Error) {
	resource, known := api.ResourceName(kind)
	if !known {
		return controlplane.ObjectType{}, internal("未知的内核对象类 " + kind)
	}
	scope, _ := api.ScopeOf(kind)
	return controlplane.ObjectType{
		Group:      api.Group,
		Version:    v1beta1.GroupVersion.Version,
		Kind:       kind,
		Resource:   resource,
		Namespaced: scope == api.ScopeNamespaced,
	}, nil
}

// StatusKinds returns every kernel kind that carries the shared status axes,
// which is the set C-07 and C-08 let a scan judge. Membership is derived from
// the closed kind set, never restated (C-01).
func StatusKinds() []string {
	kinds := make([]string, 0, len(api.Kinds()))
	for _, kind := range api.Kinds() {
		if hasStatus, _ := api.HasStatus(kind); hasStatus {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// List returns every object of one kernel class; an empty namespace reads every
// namespace.
func (r *Reader) List(ctx context.Context, kind, namespace string) ([]map[string]any, *controlplane.Error) {
	objectType, failure := KernelType(kind)
	if failure != nil {
		return nil, failure
	}
	list, err := r.resource(objectType, namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, readFailure(err, objectType, namespace, "")
	}
	objects := make([]map[string]any, 0, len(list.Items))
	for index := range list.Items {
		objects = append(objects, list.Items[index].Object)
	}
	return objects, nil
}

// Get returns one object of a kernel class.
func (r *Reader) Get(ctx context.Context, kind, namespace, name string) (map[string]any, *controlplane.Error) {
	objectType, failure := KernelType(kind)
	if failure != nil {
		return nil, failure
	}
	object, err := r.resource(objectType, namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, readFailure(err, objectType, namespace, name)
	}
	return object.Object, nil
}

// Nodes returns the cluster nodes and the moment the read observed them.
func (r *Reader) Nodes(ctx context.Context) ([]corev1.Node, *controlplane.Error) {
	list, err := r.Dynamic.Resource(nodes).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, readFailure(err, controlplane.ObjectType{Version: "v1", Kind: "Node", Resource: "nodes"}, "", "")
	}
	result := make([]corev1.Node, 0, len(list.Items))
	for index := range list.Items {
		var node corev1.Node
		if failure := Decode(list.Items[index].Object, &node); failure != nil {
			return nil, failure
		}
		result = append(result, node)
	}
	return result, nil
}

// Events returns the events of C-64's window, newest first, trimmed to what the
// session may read. The read goes through the control plane's one window
// implementation, so the page and the events route report the same events.
func (r *Reader) Events(ctx context.Context, session identity.Session, namespace string, limit int) ([]controlplane.Event, *controlplane.Error) {
	read, failure := (controlplane.EventReader{
		Source:     controlplane.NewClusterSource(r.Dynamic),
		Authorizer: r.authorizer(session),
		Now:        r.Now,
	}).Read(ctx, namespace, nil)
	if failure != nil {
		return nil, failure
	}
	events := read.Events
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// Visible applies C-59 to one kernel object: an application the session may not
// see is treated as absent, every other class is visible to a signed-in role.
func (r *Reader) Visible(ctx context.Context, session identity.Session, kind string, object map[string]any) (bool, *controlplane.Error) {
	if kind != "Application" {
		return true, nil
	}
	_, err := r.authorizer(session).Decide(ctx, controlplane.Request{
		Verb: controlplane.VerbRead, Ref: RefOf(kind, object), Object: object,
	})
	switch {
	case err == nil:
		return true, nil
	default:
		// The rule cannot judge this subject, so the page hides it rather than
		// showing an object the session might not own (C-59). A hidden object
		// and one the rule cannot resolve look the same to the caller.
		return false, nil
	}
}

// authorizer binds one session to the C-59 and C-64 policy.
func (r *Reader) authorizer(session identity.Session) controlplane.Authorizer {
	return sessionAuthorizer{
		policy:  &access.Policy{Objects: controlplane.NewClusterSource(r.Dynamic)},
		session: session,
	}
}

// sessionAuthorizer judges a request as the session a page was rendered for.
type sessionAuthorizer struct {
	policy  *access.Policy
	session identity.Session
}

// Decide implements controlplane.Authorizer.
func (a sessionAuthorizer) Decide(ctx context.Context, request controlplane.Request) (controlplane.Grant, error) {
	return a.policy.Decide(ctx, access.Session{Username: a.session.Username, Role: a.session.Role}, request)
}

// Decode reads one stored object into its typed form.
func Decode(object map[string]any, target any) *controlplane.Error {
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object, target); err != nil {
		return internal("对象无法解码：" + err.Error())
	}
	return nil
}

// RefOf builds the reference of a stored object.
func RefOf(kind string, object map[string]any) controlplane.ObjectRef {
	group := api.Group
	if _, known := api.ResourceName(kind); !known {
		group = subjectGroup(Text(object, "apiVersion"))
	}
	return controlplane.ObjectRef{
		Group:     group,
		Kind:      kind,
		Namespace: Text(object, "metadata", "namespace"),
		Name:      Text(object, "metadata", "name"),
	}
}

// subjectGroup reads the API group out of an apiVersion; the core group is
// empty.
func subjectGroup(apiVersion string) string {
	group, _, found := strings.Cut(apiVersion, "/")
	if !found {
		return ""
	}
	return group
}

// Text reads one string field of a stored object.
func Text(object map[string]any, path ...string) string {
	value, _, _ := unstructured.NestedString(object, path...)
	return value
}

// resource returns the interface one class is read through.
func (r *Reader) resource(objectType controlplane.ObjectType, namespace string) dynamic.ResourceInterface {
	resource := r.Dynamic.Resource(objectType.GVR())
	if objectType.Namespaced {
		return resource.Namespace(namespace)
	}
	return resource
}

// readFailure reports a failed cluster read. An absent object is a not-found
// failure; any other error is an unavailable cluster, never an empty result
// (C-64).
func readFailure(err error, objectType controlplane.ObjectType, namespace, name string) *controlplane.Error {
	return controlplane.ClusterReadFailure(err, controlplane.ObjectRef{
		Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: name,
	}, nil)
}

// internal reports a failure on this side of the read.
func internal(message string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     message,
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}
