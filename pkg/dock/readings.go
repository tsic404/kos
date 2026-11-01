package dock

import (
	"context"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
)

// entries is the class the desktop reads its entries from; the class belongs to
// this extension's own API group (C-19).
var entries = schema.GroupVersionResource{Group: APIGroup, Version: EntryVersion, Resource: EntryResource}

// Readings resolves the items the desktop page declares (C-52, C-53): the
// entries with the address they open, the installed applications, and the
// notification centre's recent events. The recent list is browser state and
// never reaches this reader.
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
}

// Read implements pages.Readings. The desktop is a panel page, so it takes no
// target.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem, _ pages.Target) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, session, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.reader().Clock()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, session identity.Session, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ItemEntries:
		return r.entryViews(ctx, session)
	case ItemApplications:
		return r.applications(ctx, session)
	case ItemNotifications:
		return r.notifications(ctx, session)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
}

// entryView is one desktop entry resolved against the live projection (C-30):
// what the desktop shows, the address it opens and why it cannot open. An entry
// whose application is unavailable keeps its place and carries the reason.
type entryView struct {
	// Namespace is where the entry is stored.
	Namespace string `json:"namespace"`
	// Name is the entry's name.
	Name string `json:"name"`
	// DisplayName is the desktop label.
	DisplayName string `json:"displayName"`
	// Icon is the display icon.
	Icon string `json:"icon,omitempty"`
	// Application is the application the entry opens.
	Application string `json:"application"`
	// Visibility decides who the entry shows to.
	Visibility string `json:"visibility"`
	// Order places the entry on the desktop.
	Order int64 `json:"order"`
	// Source names where the entry came from.
	Source string `json:"source,omitempty"`
	// Launch is the endpoint the entry opens, present only when it opens.
	Launch *Launch `json:"launch,omitempty"`
	// Available reports that the desktop can open the entry.
	Available bool `json:"available"`
	// Reason explains an entry that does not open.
	Reason string `json:"reason,omitempty"`
}

// entryViews reads every entry the session may see and resolves it against its
// application and endpoints (C-30). The entries are ordered by the order they
// declare, then by name, so the desktop lays out the same way on every read.
func (r *Readings) entryViews(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	listed, err := r.Dynamic.Resource(entries).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, controlplane.ClusterReadFailure(err, controlplane.ObjectRef{
			Group: APIGroup, Kind: EntryKind,
		}, nil)
	}
	applications, failure := r.reader().List(ctx, "Application", "")
	if failure != nil {
		return nil, failure
	}
	byKey := make(map[string]*unstructured.Unstructured, len(applications))
	for _, application := range applications {
		byKey[cluster.Text(application, "metadata", "namespace")+"/"+cluster.Text(application, "metadata", "name")] =
			&unstructured.Unstructured{Object: application}
	}
	endpointsByNamespace := map[string][]*unstructured.Unstructured{}
	views := make([]entryView, 0, len(listed.Items))
	for index := range listed.Items {
		entry := &listed.Items[index]
		spec, err := SpecOf(entry)
		if err != nil {
			view := unreadableView(entry, err)
			if !showsTo(Spec{Visibility: view.Visibility}, entry, session) {
				continue
			}
			views = append(views, view)
			continue
		}
		if !showsTo(spec, entry, session) {
			continue
		}
		namespace := entry.GetNamespace()
		endpoints, read := endpointsByNamespace[namespace]
		if !read {
			objects, failure := r.reader().List(ctx, "Endpoint", namespace)
			if failure != nil {
				return nil, failure
			}
			endpoints = make([]*unstructured.Unstructured, 0, len(objects))
			for _, object := range objects {
				endpoints = append(endpoints, &unstructured.Unstructured{Object: object})
			}
			endpointsByNamespace[namespace] = endpoints
		}
		view, err := Resolve(entry, byKey[namespace+"/"+spec.Application], endpoints)
		if err != nil {
			unreadable := unreadableView(entry, err)
			if !showsTo(Spec{Visibility: unreadable.Visibility}, entry, session) {
				continue
			}
			views = append(views, unreadable)
			continue
		}
		views = append(views, entryView{
			Namespace: namespace, Name: entry.GetName(),
			DisplayName: spec.DisplayName, Icon: spec.Icon,
			Application: spec.Application, Visibility: spec.Visibility, Order: spec.Order, Source: spec.Source,
			Launch:    launchOf(view),
			Available: view.IsAvailable, Reason: view.Reason,
		})
	}
	sort.Slice(views, func(left, right int) bool {
		if views[left].Order != views[right].Order {
			return views[left].Order < views[right].Order
		}
		if views[left].Namespace != views[right].Namespace {
			return views[left].Namespace < views[right].Namespace
		}
		return views[left].Name < views[right].Name
	})
	return map[string]any{"entries": views}, nil
}

// showsTo applies the entry's visibility (C-30): an entry shown to everyone is
// shown to every signed-in session, and one shown to its owner is shown to the
// account the entry records. An entry that records no owner has nothing that
// attributes it to an account, so it stays visible rather than disappearing
// from the desktop of the account that created it.
func showsTo(spec Spec, entry *unstructured.Unstructured, session identity.Session) bool {
	if spec.Visibility != VisibilityOwner {
		return true
	}
	owner := cluster.Text(entry.Object, "spec", "owner")
	return owner == "" || owner == session.Username
}

// launchOf renders the resolved launch, absent when the entry does not open.
func launchOf(view View) *Launch {
	if !view.IsAvailable {
		return nil
	}
	launch := view.Launch
	return &launch
}

// applicationView is one installed application as the desktop lists it (C-30,
// C-52): the desktop opens applications, it does not manage them.
type applicationView struct {
	// Namespace is the application's namespace.
	Namespace string `json:"namespace"`
	// Name is the application's name.
	Name string `json:"name"`
	// Phase is the lifecycle phase the application reports.
	Phase string `json:"phase"`
	// Health is the health state the application reports.
	Health string `json:"health"`
}

// applications lists the installed applications the session may see (C-59).
func (r *Readings) applications(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Application", "")
	if failure != nil {
		return nil, failure
	}
	views := make([]applicationView, 0, len(objects))
	for _, object := range objects {
		visible, failure := r.reader().Visible(ctx, session, "Application", object)
		if failure != nil {
			return nil, failure
		}
		if !visible {
			continue
		}
		views = append(views, applicationView{
			Namespace: cluster.Text(object, "metadata", "namespace"),
			Name:      cluster.Text(object, "metadata", "name"),
			Phase:     cluster.Text(object, "status", "phase"),
			Health:    cluster.Text(object, "status", "health", "state"),
		})
	}
	sort.Slice(views, func(left, right int) bool {
		if views[left].Namespace != views[right].Namespace {
			return views[left].Namespace < views[right].Namespace
		}
		return views[left].Name < views[right].Name
	})
	return map[string]any{"applications": views}, nil
}

// notifications reads the notification centre's recent events (C-52): the same
// window, order and visibility rule the events route serves (C-64), so the
// centre and the route cannot report the same event differently.
func (r *Readings) notifications(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	events, failure := r.reader().Events(ctx, session, "", 0)
	if failure != nil {
		return nil, failure
	}
	return map[string]any{"window": controlplane.EventWindow.String(), "events": events}, nil
}

// unreadableView keeps an entry whose definition does not decode (C-30): the
// desktop still shows it with the reason it cannot open, and the fields that do
// read still label and place it. One bad definition never fails the page.
func unreadableView(entry *unstructured.Unstructured, err error) entryView {
	view := entryView{
		Namespace:   entry.GetNamespace(),
		Name:        entry.GetName(),
		DisplayName: cluster.Text(entry.Object, "spec", "displayName"),
		Application: cluster.Text(entry.Object, "spec", "application"),
		Visibility:  cluster.Text(entry.Object, "spec", "visibility"),
		Source:      cluster.Text(entry.Object, "spec", "source"),
		Reason:      "条目定义无法解析：" + err.Error(),
	}
	if order, found, readErr := unstructured.NestedInt64(entry.Object, "spec", "order"); readErr == nil && found {
		view.Order = order
	}
	if view.DisplayName == "" {
		view.DisplayName = view.Application
	}
	return view
}

// unknownItem names a page item this reader does not serve.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "桌面页的来源 " + id + " 无法解析",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}
