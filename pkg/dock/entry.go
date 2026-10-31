package dock

import (
	"errors"
	"fmt"
	"sort"
	"strconv"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/endpoint"
	"github.com/tsic404/kos/pkg/write"
)

// Spec is the desktop entry spec (C-30): the application the entry opens along
// with everything the desktop shows for it.
type Spec struct {
	// Application is the name of the Application in the entry's namespace.
	Application string `json:"application"`
	// DisplayName is the desktop label; empty means the application's name.
	DisplayName string `json:"displayName,omitempty"`
	// Icon is the display icon.
	Icon string `json:"icon,omitempty"`
	// Launch names the endpoint the entry opens first; empty means the
	// application's first endpoint.
	Launch string `json:"launch,omitempty"`
	// Visibility decides who the entry shows to.
	Visibility string `json:"visibility,omitempty"`
	// Order places the entry on the desktop; lower comes first.
	Order int64 `json:"order,omitempty"`
	// Source names where the entry came from.
	Source string `json:"source,omitempty"`
}

// SpecOf decodes one entry's spec. Missing display name and visibility take
// their defaults, so a bare entry still renders and shows.
func SpecOf(object *unstructured.Unstructured) (Spec, error) {
	var spec Spec
	raw, found, err := unstructured.NestedMap(object.Object, "spec")
	if err != nil {
		return spec, fmt.Errorf("read desktop entry spec: %w", err)
	}
	if !found {
		return spec, errors.New("desktop entry carries no spec")
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(raw, &spec); err != nil {
		return spec, fmt.Errorf("decode desktop entry spec: %w", err)
	}
	if spec.DisplayName == "" {
		spec.DisplayName = spec.Application
	}
	if spec.Visibility == "" {
		spec.Visibility = VisibilityEveryone
	}
	return spec, nil
}

// Launch is the entry an application is opened through (C-30). The endpoint is
// read at resolve time, so its address follows the endpoint and not the entry.
type Launch struct {
	// Endpoint is the endpoint object serving the application.
	Endpoint string
	// Address is that endpoint's current address.
	Address string
}

// View is one desktop entry resolved against the live projection (C-30): the
// entry's own spec, the application it points at, and whether the desktop can
// open it. An entry whose application is unavailable keeps its place and
// carries the reason it cannot open.
type View struct {
	// Ref is the entry object.
	Ref write.Ref
	// Spec is the entry's declared fields.
	Spec Spec
	// Launch is the resolved endpoint, present only when the entry opens.
	Launch Launch
	// IsAvailable reports that the application is usable and has an endpoint.
	IsAvailable bool
	// Reason explains an entry that does not open.
	Reason string
}

// Resolve reads one entry against the application it names and the endpoints in
// the entry's namespace. The entry points at the Application, never at a
// downstream object: renaming the workload or the endpoint leaves the entry on
// the same application and only moves the address it opens.
func Resolve(entry *unstructured.Unstructured, application *unstructured.Unstructured, endpoints []*unstructured.Unstructured) (View, error) {
	spec, err := SpecOf(entry)
	if err != nil {
		return View{}, err
	}
	view := View{Ref: write.RefOf(entry), Spec: spec}
	if spec.Application == "" {
		view.Reason = "条目未声明所属应用"
		return view, nil
	}
	if reason := unavailableReason(application, spec.Application); reason != "" {
		view.Reason = reason
		return view, nil
	}
	target := selectEndpoint(spec.Launch, spec.Application, endpoints)
	if target == nil {
		view.Reason = "应用 " + spec.Application + " 没有可用入口"
		return view, nil
	}
	address, err := addressOf(target)
	if err != nil {
		view.Reason = "入口 " + target.GetName() + " 的地址无法解析：" + err.Error()
		return view, nil
	}
	view.Launch = Launch{Endpoint: target.GetName(), Address: address}
	view.IsAvailable = true
	return view, nil
}

// unavailableReason answers why an entry cannot open its application. An entry
// survives every application state, so the empty answer means the application is
// usable and the caller resolves the endpoint.
func unavailableReason(application *unstructured.Unstructured, name string) string {
	if application == nil || application.GetName() != name {
		return "应用 " + name + " 未安装"
	}
	if application.GetDeletionTimestamp() != nil {
		return "应用 " + name + " 正在卸载"
	}
	phase, _, err := unstructured.NestedString(application.Object, "status", "phase")
	if err != nil {
		return "应用 " + name + " 的阶段无法读取"
	}
	switch phase {
	case app.PhaseReady, app.PhaseUpgrading:
		// An upgrade keeps the running application reachable, so its entries
		// still open while the phase moves through Upgrading.
		return ""
	default:
		return "应用 " + name + " 阶段为 " + phase + "，暂不可打开"
	}
}

// selectEndpoint picks the endpoint an entry opens: the named one when it still
// serves the application, otherwise the application's first endpoint. A renamed
// endpoint therefore moves the address and not the entry's identity (C-30).
func selectEndpoint(launch, application string, endpoints []*unstructured.Unstructured) *unstructured.Unstructured {
	candidates := make([]*unstructured.Unstructured, 0, len(endpoints))
	for _, candidate := range endpoints {
		if candidate.GetDeletionTimestamp() != nil || !endpoint.TargetsApp(candidate, application) {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].GetName() < candidates[right].GetName()
	})
	if launch != "" {
		for _, candidate := range candidates {
			if candidate.GetName() == launch {
				return candidate
			}
		}
	}
	return candidates[0]
}

// addressOf renders the address an endpoint currently serves. The shape decides
// the form, so a rewritten host, path or port changes the address the entry
// opens (C-43, C-30).
func addressOf(object *unstructured.Unstructured) (string, error) {
	spec, err := endpoint.SpecOf(object)
	if err != nil {
		return "", err
	}
	switch spec.Kind {
	case v1beta1.EndpointKindHTTPHost:
		return schemeOf(spec) + "://" + spec.Host + "/", nil
	case v1beta1.EndpointKindHTTPPath:
		return schemeOf(spec) + "://" + spec.Host + spec.Path, nil
	case v1beta1.EndpointKindTCP:
		return "tcp://" + spec.TargetApp + ":" + strconv.Itoa(int(spec.Port)), nil
	case v1beta1.EndpointKindUDP:
		return "udp://" + spec.TargetApp + ":" + strconv.Itoa(int(spec.Port)), nil
	case v1beta1.EndpointKindMDNS:
		return "mdns://" + spec.ServiceName, nil
	default:
		return "", fmt.Errorf("endpoint shape %q is not known", spec.Kind)
	}
}

// schemeOf is the scheme the endpoint serves: TLS makes the two HTTP shapes
// https (C-46).
func schemeOf(spec v1beta1.EndpointSpec) string {
	if spec.TLS != nil {
		return "https"
	}
	return "http"
}
