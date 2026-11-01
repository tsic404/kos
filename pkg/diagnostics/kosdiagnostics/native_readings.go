package kosdiagnostics

import (
	"context"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// nativeClasses is the set of native classes the read-only view serves. It is a
// product table, not a kernel one: the view names the classes it can show, and a
// class outside it is refused rather than guessed. Secret is deliberately absent:
// its values are credentials, and C-14 keeps credentials inside the control
// plane, so no page may project them.
var nativeClasses = map[string]controlplane.ObjectType{
	"Deployment":  {Group: "apps", Version: "v1", Kind: "Deployment", Resource: "deployments", Namespaced: true},
	"StatefulSet": {Group: "apps", Version: "v1", Kind: "StatefulSet", Resource: "statefulsets", Namespaced: true},
	"DaemonSet":   {Group: "apps", Version: "v1", Kind: "DaemonSet", Resource: "daemonsets", Namespaced: true},
	"ReplicaSet":  {Group: "apps", Version: "v1", Kind: "ReplicaSet", Resource: "replicasets", Namespaced: true},
	"Job":         {Group: "batch", Version: "v1", Kind: "Job", Resource: "jobs", Namespaced: true},
	"CronJob":     {Group: "batch", Version: "v1", Kind: "CronJob", Resource: "cronjobs", Namespaced: true},
	"Pod":         {Version: "v1", Kind: "Pod", Resource: "pods", Namespaced: true},
	"Service":     {Version: "v1", Kind: "Service", Resource: "services", Namespaced: true},
	"ConfigMap":   {Version: "v1", Kind: "ConfigMap", Resource: "configmaps", Namespaced: true},
	"PersistentVolumeClaim": {Version: "v1", Kind: "PersistentVolumeClaim",
		Resource: "persistentvolumeclaims", Namespaced: true},
	"Ingress": {Group: "networking.k8s.io", Version: "v1", Kind: "Ingress", Resource: "ingresses", Namespaced: true},
	"Node":    {Version: "v1", Kind: "Node", Resource: "nodes"},
}

// NativeClasses lists the kinds the read-only view serves, ordered by kind. A
// caller picks one of these as the view's target.
func NativeClasses() []string {
	kinds := make([]string, 0, len(nativeClasses))
	for kind := range nativeClasses {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// nativeValue resolves one item of the native view for the object the read names
// (C-52).
func (r *Readings) nativeValue(ctx context.Context, session identity.Session, item string, target pages.Target) (any, *controlplane.Error) {
	objectType, object, failure := r.nativeObject(ctx, target)
	if failure != nil {
		return nil, failure
	}
	switch item {
	case ItemRawFields:
		return object.Object, nil
	case ItemRelated:
		return r.relatedOf(ctx, session, objectType, object)
	default:
		return nil, unknownItem(item)
	}
}

// nativeObject resolves the native object one view read names. A read naming no
// object, a class the view does not serve, or a namespace the class does not use
// is refused: the view shows one object, and showing another would answer a
// question the caller did not ask (C-52).
func (r *Readings) nativeObject(ctx context.Context, target pages.Target) (controlplane.ObjectType, *unstructured.Unstructured, *controlplane.Error) {
	objectType, known := nativeClasses[target.Kind]
	if !known {
		return controlplane.ObjectType{}, nil, nativeTargetRequired()
	}
	if !target.Complete() || (objectType.Namespaced && target.Namespace == "") {
		return controlplane.ObjectType{}, nil, nativeTargetRequired()
	}
	namespace := target.Namespace
	if !objectType.Namespaced {
		namespace = ""
	}
	object, err := r.Dynamic.Resource(schema.GroupVersionResource{
		Group: objectType.Group, Version: objectType.Version, Resource: objectType.Resource,
	}).Namespace(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return controlplane.ObjectType{}, nil, controlplane.ClusterReadFailure(err, controlplane.ObjectRef{
			Group: objectType.Group, Kind: objectType.Kind, Namespace: namespace, Name: target.Name,
		}, nil)
	}
	return objectType, object, nil
}

// nativeTargetRequired answers a view read that names no object the view serves.
func nativeTargetRequired() *controlplane.Error {
	return &controlplane.Error{
		Code: controlplane.CodeInvalidField,
		Message: "原生视图必须指名一个它服务的原生资源：给出 kind、name，命名空间作用域的资源还要给出 namespace" +
			"（可选类：" + strings.Join(NativeClasses(), "、") + "）",
		Stage:       controlplane.StageValidate,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// relatedView is one object related to the view's target: an owner the target
// names, or a KOS application whose render wrote it.
type relatedView struct {
	// Relation is how the object relates to the target: owner or rendered-by.
	Relation string `json:"relation"`
	// Group is the related object's API group.
	Group string `json:"group,omitempty"`
	// Kind is the related object's kind.
	Kind string `json:"kind"`
	// Namespace is the related object's namespace.
	Namespace string `json:"namespace,omitempty"`
	// Name is the related object's name.
	Name string `json:"name"`
	// Found reports whether the object was read; an owner the class table does
	// not serve is related but unreadable.
	Found bool `json:"found"`
	// Reason explains an owner that cannot be read.
	Reason string `json:"reason,omitempty"`
}

// relatedOf reads the objects related to one native resource (C-52): the owners
// it names in metadata.ownerReferences, and the applications whose last render
// wrote it. The reverse edge is the one KOS side of the relation, so the view
// answers which application owns a native object without reading the cluster
// from the browser (C-12).
func (r *Readings) relatedOf(ctx context.Context, session identity.Session, objectType controlplane.ObjectType, object *unstructured.Unstructured) (any, *controlplane.Error) {
	related := make([]relatedView, 0)
	owners, _, err := unstructured.NestedSlice(object.Object, "metadata", "ownerReferences")
	if err != nil {
		return nil, controlplane.ClusterReadFailure(err, cluster.RefOf(objectType.Kind, object.Object), nil)
	}
	for _, entry := range owners {
		owner, _ := entry.(map[string]any)
		kind := cluster.Text(owner, "kind")
		name := cluster.Text(owner, "name")
		if kind == "" || name == "" {
			continue
		}
		view := relatedView{Relation: "owner", Kind: kind, Name: name, Namespace: object.GetNamespace()}
		ownerType, known := nativeClasses[kind]
		if !known {
			view.Reason = "视图不服务 " + kind + " 类，无法读取该属主"
			related = append(related, view)
			continue
		}
		ownerNamespace := view.Namespace
		if !ownerType.Namespaced {
			ownerNamespace = ""
			view.Namespace = ""
		}
		view.Group = ownerType.Group
		if _, err := r.Dynamic.Resource(schema.GroupVersionResource{
			Group: ownerType.Group, Version: ownerType.Version, Resource: ownerType.Resource,
		}).Namespace(ownerNamespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
			view.Reason = "属主 " + kind + "/" + name + " 不存在"
		} else {
			view.Found = true
		}
		related = append(related, view)
	}
	applications, failure := r.applicationsOf(ctx, session, object.GetNamespace())
	if failure != nil {
		return nil, failure
	}
	for _, application := range applications {
		if !renders(application, objectType, object) {
			continue
		}
		related = append(related, relatedView{
			Relation: "rendered-by", Group: "kos.tsic.top", Kind: "Application",
			Namespace: application.Namespace, Name: application.Name, Found: true,
		})
	}
	sort.Slice(related, func(left, right int) bool {
		if related[left].Relation != related[right].Relation {
			return related[left].Relation < related[right].Relation
		}
		if related[left].Namespace != related[right].Namespace {
			return related[left].Namespace < related[right].Namespace
		}
		return related[left].Name < related[right].Name
	})
	return map[string]any{"related": related}, nil
}

// applicationsOf lists the applications of one namespace the session may see; an
// empty namespace reads every namespace (C-59).
func (r *Readings) applicationsOf(ctx context.Context, session identity.Session, namespace string) ([]v1beta1.Application, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Application", namespace)
	if failure != nil {
		return nil, failure
	}
	applications := make([]v1beta1.Application, 0, len(objects))
	for _, object := range objects {
		visible, failure := r.reader().Visible(ctx, session, "Application", object)
		if failure != nil {
			return nil, failure
		}
		if !visible {
			continue
		}
		var application v1beta1.Application
		if failure := cluster.Decode(object, &application); failure != nil {
			return nil, failure
		}
		applications = append(applications, application)
	}
	return applications, nil
}

// renders reports whether one application's last applied render wrote the
// target, which is the reference edge the application keeps (C-33).
func renders(application v1beta1.Application, objectType controlplane.ObjectType, object *unstructured.Unstructured) bool {
	for _, ref := range application.Status.Rendered {
		if ref.Group != objectType.Group || ref.Kind != objectType.Kind || ref.Name != object.GetName() {
			continue
		}
		if ref.Namespace != "" && ref.Namespace != object.GetNamespace() {
			continue
		}
		return true
	}
	return false
}
