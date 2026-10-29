package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/state"
)

// KernelSource is the capability source of the kernel baseline (C-09).
const KernelSource = "kos-shell"

// Adapter binds one served extension object class to the active diagnostic
// adapter that observes it (C-26). Version and Resource come from the
// extension's object class declaration, so the chain reads the class through
// the same identity the extension gave it (C-19).
type Adapter struct {
	// ID is the adapter's declared identifier.
	ID string
	// Group is the extension API group it covers.
	Group string
	// Kind is the object class it covers.
	Kind string
	// Version is the served version the class is read in.
	Version string
	// Resource is the plural resource name.
	Resource string
	// Segment is the chain link the adapter's evidence is inserted at; empty
	// means the dependency segment (C-26).
	Segment Link
	// Source is what its evidence names as the data source; empty means the
	// adapter identifier (C-26).
	Source string
	// Components lists the component identifiers the adapter reports on
	// (C-26).
	Components []string
}

// Capability is one entry of the capability catalogue a bundle reports (C-09).
type Capability struct {
	// Name is the capability name.
	Name string `json:"name"`
	// Version is the version the catalogue holds.
	Version string `json:"version"`
	// Source names the component that registered it.
	Source string `json:"source"`
}

// Component is one installed extension component and its versions (C-17).
type Component struct {
	// Name is the system component object's name.
	Name string `json:"name,omitempty"`
	// Identifier is the extension identity across versions.
	Identifier string `json:"identifier"`
	// Version is the installed package version.
	Version string `json:"version"`
	// FrameworkVersion is the framework range the declaration targets.
	FrameworkVersion string `json:"frameworkVersion,omitempty"`
	// Phase is the component's install phase.
	Phase string `json:"phase,omitempty"`
	// Health is the component's health state.
	Health string `json:"health,omitempty"`
}

// Environment is the extension state the chain and the bundle report: the
// active diagnostic adapters, the capability catalogue and the installed
// components. It carries no cluster access; the caller builds it from the
// extension registry and the install results.
type Environment struct {
	Adapters     []Adapter
	Capabilities []Capability
	Components   []Component
}

// Service assembles evidence chains and diagnostic bundles over one source.
type Service struct {
	// Source reads the objects the chain walks; required.
	Source Source
	// Environment is the extension state reported by the chain and the bundle.
	Environment Environment
	// Now supplies the export clock; defaults to time.Now.
	Now func() time.Time
	// EventWindow is how far back recent events are read; defaults to one hour
	// (C-64).
	EventWindow time.Duration
}

// Chain assembles the evidence chain of one application (C-26).
func (s *Service) Chain(ctx context.Context, namespace, name string) (Chain, error) {
	assembly, err := s.assemble(ctx, namespace, name)
	if err != nil {
		return Chain{}, err
	}
	return assembly.chain(), nil
}

// failure is one degradation reason found on the path, kept with the link it
// was found in so the deepest cause can be reported.
type failure struct {
	link       Link
	object     ObjectRef
	summary    string
	nextAction string
}

// assembly is the state of one chain walk.
type assembly struct {
	service       *Service
	now           time.Time
	application   *v1beta1.Application
	ref           ObjectRef
	observed      []Observed
	observedIndex map[ObjectRef]bool
	// podLists caches one namespace's pod read, so every backend and workload
	// selector of this walk reuses it (C-26).
	podLists map[string][]Observed
	pods     []Observed
	podNodes map[ObjectRef]string
	segments []Segment
	failures []failure
	// hasEndpoint records that at least one endpoint targets the application.
	hasEndpoint bool
	// workloadUnready records a workload that has no ready replica, so a node
	// fault can be related to it as an inference instead of a second fact.
	workloadUnready bool
}

// assemble walks the reference chain of one application (C-26).
func (s *Service) assemble(ctx context.Context, namespace, name string) (*assembly, error) {
	if s.Source == nil {
		return nil, errors.New("diagnostics: a source is required")
	}
	current := &assembly{
		service: s, now: s.now(),
		observedIndex: map[ObjectRef]bool{},
		podLists:      map[string][]Observed{},
		podNodes:      map[ObjectRef]string{},
	}
	application, err := s.Source.Get(ctx, applicationTarget(namespace, name))
	if err != nil {
		return nil, fmt.Errorf("read application %s/%s: %w", namespace, name, err)
	}
	current.remember(application)
	current.ref = application.Ref
	typed := &v1beta1.Application{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(application.Object.Object, typed); err != nil {
		return nil, fmt.Errorf("decode application %s/%s: %w", namespace, name, err)
	}
	current.application = typed
	current.segments = append(current.segments, current.applicationSegment(application))
	routes, err := current.endpointSegments(ctx)
	if err != nil {
		return nil, err
	}
	if err := current.backendSegment(ctx, routes); err != nil {
		return nil, err
	}
	workloads, err := current.workloadSegment(ctx)
	if err != nil {
		return nil, err
	}
	if err := current.nodeSegment(ctx, workloads); err != nil {
		return nil, err
	}
	if err := current.extensionSegments(ctx); err != nil {
		return nil, err
	}
	sortSegments(current.segments)
	return current, nil
}

// chain renders the assembled state as the chain a caller reads.
func (a *assembly) chain() Chain {
	return Chain{
		Application: a.ref,
		ObservedAt:  a.now,
		Segments:    a.segments,
		Conclusion:  a.conclusion(),
	}
}

// conclusion picks the deepest observed failure. When no segment failed but the
// application reports ill health, the reason lies outside the walked path and is
// reported as an inference (C-26).
func (a *assembly) conclusion() Conclusion {
	if len(a.failures) > 0 {
		deepest := a.failures[0]
		for _, candidate := range a.failures[1:] {
			if linkRank(candidate.link) > linkRank(deepest.link) {
				deepest = candidate
			}
		}
		return Conclusion{
			Summary:    deepest.summary,
			Basis:      BasisFact,
			Segment:    deepest.link,
			Object:     deepest.object,
			NextAction: deepest.nextAction,
		}
	}
	health := a.application.Status.Health.State
	if !a.hasEndpoint && (health == "" || health == v1beta1.HealthStateHealthy) {
		return Conclusion{
			Summary:    "应用没有入口，链路止于工作负载",
			Basis:      BasisFact,
			Segment:    LinkEndpoint,
			Object:     a.ref,
			NextAction: "需要对外暴露时创建入口",
		}
	}
	if health == "" || health == v1beta1.HealthStateHealthy {
		return Conclusion{
			Summary: "链路完整，入口可达",
			Basis:   BasisFact,
			Segment: LinkApplication,
			Object:  a.ref,
		}
	}
	return Conclusion{
		Summary:    fmt.Sprintf("应用报告 %s，链路上没有单段失败", health),
		Basis:      BasisInference,
		Segment:    LinkApplication,
		Object:     a.ref,
		NextAction: "检查应用条件与最近事件",
	}
}

// applicationSegment reports the application's own state (C-26).
func (a *assembly) applicationSegment(observed Observed) Segment {
	status := a.application.Status
	reconciled := state.ReconciledOf(status.Conditions)
	conclusion := fmt.Sprintf("应用处于 %s，健康 %s", valueOr(status.Phase, "未知"), valueOr(string(status.Health.State), "未知"))
	impact := "应用可用"
	nextAction := "无需处理"
	if status.Health.State != v1beta1.HealthStateHealthy || reconciled.Status != "True" {
		conclusion = fmt.Sprintf("应用健康为 %s：%s", valueOr(string(status.Health.State), "未知"), reconciled.Message)
		impact = "用户可能无法打开该应用或其入口"
		nextAction = "查看后续证据段定位失败对象"
		a.failures = append(a.failures, failure{
			link: LinkApplication, object: observed.Ref, summary: conclusion, nextAction: nextAction,
		})
	}
	return Segment{
		Link: LinkApplication, Object: observed.Ref, Observable: true,
		Evidence: []Evidence{{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState: fmt.Sprintf("phase=%s health=%s reconciled=%s reason=%s",
				valueOr(status.Phase, "-"), valueOr(string(status.Health.State), "-"),
				reconciled.Status, valueOr(reconciled.Reason, "-")),
			Impact:     impact,
			NextAction: nextAction,
			Basis:      BasisFact,
		}},
	}
}

// endpointSegments reports every endpoint that exposes the application and the
// route object each one owns (C-43, C-45). It returns the route objects it
// observed, so the backend segment follows the same references.
func (a *assembly) endpointSegments(ctx context.Context) ([]Observed, error) {
	listed, err := a.service.Source.List(ctx, endpointTarget(a.ref.Namespace))
	if err != nil {
		return nil, fmt.Errorf("list endpoints in %s: %w", a.ref.Namespace, err)
	}
	selected := make([]Observed, 0, len(listed))
	for _, observed := range listed {
		targetApp, _, err := unstructured.NestedString(observed.Object.Object, "spec", "targetApp")
		if err != nil || targetApp != a.ref.Name {
			continue
		}
		selected = append(selected, observed)
	}
	sort.Slice(selected, func(left, right int) bool {
		return selected[left].Ref.Name < selected[right].Ref.Name
	})
	if len(selected) == 0 {
		a.segments = append(a.segments, Segment{
			Link: LinkEndpoint, Object: a.ref, Observable: true,
			Evidence: []Evidence{{
				Conclusion: "应用没有入口",
				Object:     a.ref,
				Source:     sourceName(ObjectRef{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Endpoint"}),
				ObservedAt: a.now,
				RawState:   "没有 Endpoint 的 spec.targetApp 指向该应用",
				Impact:     "应用只能在集群内访问",
				NextAction: "需要对外暴露时创建入口",
				Basis:      BasisFact,
			}},
		})
		return nil, nil
	}
	a.hasEndpoint = true
	routes := make([]Observed, 0, len(selected))
	for _, endpoint := range selected {
		a.remember(endpoint)
		a.segments = append(a.segments, a.endpointSegment(endpoint))
		route, failure, err := a.routeOf(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		if route != nil {
			routes = append(routes, *route)
		}
		if failure != nil {
			a.failures = append(a.failures, *failure)
		}
	}
	return routes, nil
}

// endpointSegment reports one endpoint's shape and status.
func (a *assembly) endpointSegment(observed Observed) Segment {
	spec, _, _ := unstructured.NestedMap(observed.Object.Object, "spec")
	kind, _, _ := unstructured.NestedString(observed.Object.Object, "spec", "kind")
	targetApp, _, _ := unstructured.NestedString(observed.Object.Object, "spec", "targetApp")
	reconciled, reason, message := reconciledStatus(observed.Object)
	conclusion := fmt.Sprintf("入口 %s 指向应用 %s，形态 %s", observed.Ref.Name, targetApp, kind)
	impact := "入口已建立"
	nextAction := "无需处理"
	if reconciled != "True" {
		conclusion = fmt.Sprintf("入口 %s 未就绪：%s %s", observed.Ref.Name, reason, message)
		impact = "入口不可用"
		nextAction = "修正入口规格或等待调和完成"
		a.failures = append(a.failures, failure{
			link: LinkEndpoint, object: observed.Ref, summary: conclusion, nextAction: nextAction,
		})
	}
	return Segment{
		Link: LinkEndpoint, Object: observed.Ref, Observable: true,
		Evidence: []Evidence{{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState:   fmt.Sprintf("spec=%v reconciled=%s reason=%s", spec, reconciled, valueOr(reason, "-")),
			Impact:     impact,
			NextAction: nextAction,
			Basis:      BasisFact,
		}},
	}
}

// routeOf reads the route object one endpoint owns (C-43). It returns a nil
// observation and a failure when the object is missing.
func (a *assembly) routeOf(ctx context.Context, endpoint Observed) (*Observed, *failure, error) {
	kind, _, _ := unstructured.NestedString(endpoint.Object.Object, "spec", "kind")
	route, expects := routeClass(v1beta1.EndpointKind(kind))
	if !expects {
		a.segments = append(a.segments, Segment{
			Link: LinkRoute, Object: endpoint.Ref, Observable: true,
			Evidence: []Evidence{{
				Conclusion: "mdns 入口没有下游路由对象",
				Object:     endpoint.Ref,
				Source:     sourceName(endpoint.Ref),
				ObservedAt: endpoint.ObservedAt,
				RawState:   "spec.kind=" + kind,
				Impact:     "入口由多播 Provider 观察",
				NextAction: "无需处理",
				Basis:      BasisFact,
			}},
		})
		return nil, nil, nil
	}
	target := Target{
		Group: route.group, Version: route.version, Kind: route.kind, Resource: route.resource,
		Namespace: endpoint.Ref.Namespace, Name: endpoint.Ref.Name,
	}
	observed, err := a.service.Source.Get(ctx, target)
	if apierrors.IsNotFound(err) {
		missing := ObjectRef{Group: target.Group, Version: target.Version, Kind: target.Kind, Namespace: target.Namespace, Name: target.Name}
		conclusion := fmt.Sprintf("入口 %s 的路由对象 %s 不存在", endpoint.Ref.Name, target.Kind)
		a.segments = append(a.segments, Segment{
			Link: LinkRoute, Object: missing, Observable: true,
			Evidence: []Evidence{{
				Conclusion: conclusion,
				Object:     missing,
				Source:     sourceName(missing),
				ObservedAt: a.now,
				RawState:   err.Error(),
				Impact:     "入口没有路由对象，流量无法到达后端",
				NextAction: "等待入口调和器写入路由对象",
				Basis:      BasisFact,
			}},
		})
		return nil, &failure{
			link: LinkRoute, object: missing, summary: conclusion, nextAction: "等待入口调和器写入路由对象",
		}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read route %s %s: %w", target.Kind, target.Name, err)
	}
	a.remember(observed)
	reconciled, reason, message := reconciledStatus(observed.Object)
	conclusion := fmt.Sprintf("路由对象 %s %s 存在", target.Kind, target.Name)
	nextAction := "无需处理"
	if reconciled != "True" && reconciled != "" {
		conclusion = fmt.Sprintf("路由对象 %s %s 未就绪：%s %s", target.Kind, target.Name, reason, message)
		nextAction = "检查入口调和器与路由对象"
	}
	a.segments = append(a.segments, Segment{
		Link: LinkRoute, Object: observed.Ref, Observable: true,
		Evidence: []Evidence{{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState:   fmt.Sprintf("reconciled=%s reason=%s", valueOr(reconciled, "-"), valueOr(reason, "-")),
			Impact:     "入口流量经该对象转发",
			NextAction: nextAction,
			Basis:      BasisFact,
		}},
	})
	return &observed, nil, nil
}

// backendSegment reports every backend the route objects name (C-43). A route
// that exists but names no backend is reported as that fact; the chain never
// guesses an object.
func (a *assembly) backendSegment(ctx context.Context, routes []Observed) error {
	refs := make([]backendRef, 0, len(routes))
	for _, route := range routes {
		refs = append(refs, backendsOf(route)...)
	}
	if len(routes) == 0 {
		// No route object exists (mdns, or no endpoint at all): the
		// application's own service is the only backend the chain can name.
		refs = append(refs, backendRef{
			Kind: "Service", Name: a.ref.Name, Namespace: a.deploymentNamespace(),
		})
	}
	if len(refs) == 0 {
		route := routes[0]
		conclusion := fmt.Sprintf("路由对象 %s %s 没有声明后端", route.Ref.Kind, route.Ref.Name)
		a.segments = append(a.segments, Segment{
			Link: LinkBackend, Object: route.Ref, Observable: true,
			Evidence: []Evidence{{
				Conclusion: conclusion,
				Object:     route.Ref,
				Source:     sourceName(route.Ref),
				ObservedAt: route.ObservedAt,
				RawState:   "spec 没有 defaultBackend.service 或 rules[].http.paths[].backend",
				Impact:     "入口没有可到达的后端",
				NextAction: "修正入口规格或等待入口调和器补写后端",
				Basis:      BasisFact,
			}},
		})
		a.failures = append(a.failures, failure{
			link: LinkBackend, object: route.Ref, summary: conclusion, nextAction: "修正入口规格",
		})
		return nil
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		key := ref.Group + "/" + ref.Kind + "/" + ref.Name
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := a.backendFor(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// backendFor reports one backend object. A service backend also reports the
// instances its selector reaches; a resource backend reports the object state.
func (a *assembly) backendFor(ctx context.Context, ref backendRef) error {
	if ref.Kind == "Service" {
		return a.serviceBackend(ctx, ref)
	}
	target, readable := targetForRendered(ObjectRef{
		Group: ref.Group, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name,
	}, a.service.Environment)
	if !readable {
		missing := ObjectRef{Group: ref.Group, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
		conclusion := fmt.Sprintf("后端对象 %s %s 不在诊断读取范围内", ref.Kind, ref.Name)
		a.segments = append(a.segments, Segment{
			Link: LinkBackend, Object: missing, Observable: true,
			Evidence: []Evidence{{
				Conclusion: conclusion,
				Object:     missing,
				Source:     sourceName(missing),
				ObservedAt: a.now,
				RawState:   "没有可读的类定义",
				Impact:     "入口指向的后端无法被证据核对",
				NextAction: "确认该对象类由已安装的扩展声明",
				Basis:      BasisFact,
			}},
		})
		return nil
	}
	observed, err := a.service.Source.Get(ctx, target)
	if apierrors.IsNotFound(err) {
		missing := ObjectRef{Group: ref.Group, Version: target.Version, Kind: ref.Kind, Namespace: ref.Namespace, Name: ref.Name}
		conclusion := fmt.Sprintf("后端对象 %s %s 不存在", ref.Kind, ref.Name)
		a.segments = append(a.segments, Segment{
			Link: LinkBackend, Object: missing, Observable: true,
			Evidence: []Evidence{{
				Conclusion: conclusion,
				Object:     missing,
				Source:     sourceName(missing),
				ObservedAt: a.now,
				RawState:   err.Error(),
				Impact:     "入口指向的后端不存在",
				NextAction: "重新调和应用以写入该对象",
				Basis:      BasisFact,
			}},
		})
		a.failures = append(a.failures, failure{
			link: LinkBackend, object: missing, summary: conclusion, nextAction: "重新调和应用",
		})
		return nil
	}
	if err != nil {
		return fmt.Errorf("read backend %s: %w", ref.Name, err)
	}
	a.remember(observed)
	_, _, raw := stateOf(observed.Object)
	conclusion := fmt.Sprintf("后端对象 %s %s 存在", ref.Kind, ref.Name)
	a.segments = append(a.segments, Segment{
		Link: LinkBackend, Object: observed.Ref, Observable: true,
		Evidence: []Evidence{{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState:   raw,
			Impact:     "入口流量转发到该对象",
			NextAction: "无需处理",
			Basis:      BasisFact,
		}},
	})
	return nil
}

// serviceBackend reports a service backend and the pods its selector reaches.
func (a *assembly) serviceBackend(ctx context.Context, ref backendRef) error {
	target := Target{
		Group: ref.Group, Version: "v1", Kind: "Service", Resource: "services",
		Namespace: ref.Namespace, Name: ref.Name,
	}
	observed, err := a.service.Source.Get(ctx, target)
	if apierrors.IsNotFound(err) {
		missing := ObjectRef{Group: ref.Group, Version: target.Version, Kind: target.Kind, Namespace: ref.Namespace, Name: ref.Name}
		conclusion := fmt.Sprintf("后端服务 %s 不存在", ref.Name)
		a.segments = append(a.segments, Segment{
			Link: LinkBackend, Object: missing, Observable: true,
			Evidence: []Evidence{{
				Conclusion: conclusion,
				Object:     missing,
				Source:     sourceName(missing),
				ObservedAt: a.now,
				RawState:   err.Error(),
				Impact:     "入口指向的服务不存在，请求无法到达工作负载",
				NextAction: "重新调和应用以写入服务对象",
				Basis:      BasisFact,
			}},
		})
		a.failures = append(a.failures, failure{
			link: LinkBackend, object: missing, summary: conclusion, nextAction: "重新调和应用以写入服务对象",
		})
		return nil
	}
	if err != nil {
		return fmt.Errorf("read backend %s: %w", ref.Name, err)
	}
	a.remember(observed)
	selector, _, _ := unstructured.NestedStringMap(observed.Object.Object, "spec", "selector")
	pods, err := a.podsMatching(ctx, ref.Namespace, selector)
	if err != nil {
		return err
	}
	ready := 0
	for _, pod := range pods {
		if podReady(pod.Object) {
			ready++
		}
	}
	conclusion := fmt.Sprintf("后端服务 %s 选中 %d 个实例，%d 个就绪", ref.Name, len(pods), ready)
	impact := "后端可用"
	nextAction := "无需处理"
	if len(pods) == 0 || ready == 0 {
		impact = "入口没有可用后端，请求失败"
		nextAction = "检查工作负载与实例状态"
		a.failures = append(a.failures, failure{
			link: LinkBackend, object: observed.Ref, summary: conclusion, nextAction: nextAction,
		})
	}
	port := ref.Port
	if port == "" {
		port = "-"
	}
	a.segments = append(a.segments, Segment{
		Link: LinkBackend, Object: observed.Ref, Observable: true,
		Evidence: []Evidence{{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState:   fmt.Sprintf("port=%s selector=%v pods=%d ready=%d", port, selector, len(pods), ready),
			Impact:     impact,
			NextAction: nextAction,
			Basis:      BasisFact,
		}},
	})
	return nil
}

// workloadSegment reports the workload objects the application rendered (C-32).
func (a *assembly) workloadSegment(ctx context.Context) ([]Observed, error) {
	workloads := make([]Observed, 0, 2)
	for _, rendered := range a.application.Status.Rendered {
		if !isWorkload(rendered.Kind) {
			continue
		}
		target, ok := targetOf(rendered)
		if !ok {
			continue
		}
		observed, err := a.service.Source.Get(ctx, target)
		if apierrors.IsNotFound(err) {
			missing := ObjectRef{Group: rendered.Group, Version: rendered.Version, Kind: rendered.Kind, Namespace: rendered.Namespace, Name: rendered.Name}
			a.segments = append(a.segments, Segment{
				Link: LinkWorkload, Object: missing, Observable: true,
				Evidence: []Evidence{{
					Conclusion: fmt.Sprintf("工作负载 %s %s 不存在", rendered.Kind, rendered.Name),
					Object:     missing,
					Source:     sourceName(missing),
					ObservedAt: a.now,
					RawState:   err.Error(),
					Impact:     "应用声明的副本没有被创建",
					NextAction: "重新调和应用",
					Basis:      BasisFact,
				}},
			})
			a.failures = append(a.failures, failure{
				link: LinkWorkload, object: missing, summary: "应用声明的工作负载不存在", nextAction: "重新调和应用",
			})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read workload %s: %w", rendered.Name, err)
		}
		a.remember(observed)
		workloads = append(workloads, observed)
		a.segments = append(a.segments, a.workloadEvidence(observed))
		selector, _, _ := unstructured.NestedStringMap(observed.Object.Object, "spec", "selector", "matchLabels")
		if _, err := a.podsMatching(ctx, observed.Ref.Namespace, selector); err != nil {
			return nil, err
		}
	}
	if len(workloads) == 0 {
		a.segments = append(a.segments, Segment{
			Link: LinkWorkload, Object: a.ref, Observable: true,
			Evidence: []Evidence{{
				Conclusion: "应用没有渲染工作负载对象",
				Object:     a.ref,
				Source:     sourceName(a.ref),
				ObservedAt: a.now,
				RawState:   fmt.Sprintf("status.rendered=%d", len(a.application.Status.Rendered)),
				Impact:     "应用可能只渲染了配置类对象",
				NextAction: "确认应用定义是否需要工作负载",
				Basis:      BasisFact,
			}},
		})
	}
	return workloads, nil
}

// workloadEvidence reports one workload's replicas and conditions.
func (a *assembly) workloadEvidence(observed Observed) Segment {
	desired, ready, hasReplicas := replicaFacts(observed.Object)
	health, _, _ := unstructured.NestedString(observed.Object.Object, "status", "health", "state")
	conclusion := fmt.Sprintf("工作负载 %s %s 已创建", observed.Ref.Kind, observed.Ref.Name)
	impact := "工作负载可用"
	nextAction := "无需处理"
	failed := false
	if hasReplicas {
		conclusion = fmt.Sprintf("%s %s 副本 %d/%d 就绪", observed.Ref.Kind, observed.Ref.Name, ready, desired)
		failed = ready < desired
	} else if health != "" && health != string(v1beta1.HealthStateHealthy) {
		conclusion = fmt.Sprintf("工作负载 %s %s 健康 %s", observed.Ref.Kind, observed.Ref.Name, health)
		failed = true
	}
	if failed {
		impact = "入口没有足够的可用实例"
		nextAction = "检查实例调度与就绪状态"
		a.failures = append(a.failures, failure{
			link: LinkWorkload, object: observed.Ref, summary: conclusion, nextAction: nextAction,
		})
		a.workloadUnready = true
	}
	return Segment{
		Link: LinkWorkload, Object: observed.Ref, Observable: true,
		Evidence: []Evidence{{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState:   workloadState(observed.Object),
			Impact:     impact,
			NextAction: nextAction,
			Basis:      BasisFact,
		}},
	}
}

// nodeSegment reports the nodes the workload's pods run on. A node fault
// related to an unready workload is reported as an inference, so the entry never
// claims more than it observed (C-26).
func (a *assembly) nodeSegment(ctx context.Context, workloads []Observed) error {
	nodes := map[string]bool{}
	for _, node := range a.podNodes {
		nodes[node] = true
	}
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		subject := a.ref
		if len(workloads) > 0 {
			subject = workloads[0].Ref
		}
		a.segments = append(a.segments, Segment{
			Link: LinkNode, Object: subject, Observable: true,
			Evidence: []Evidence{{
				Conclusion: "没有已调度的实例，节点段不可判定",
				Object:     subject,
				Source:     "core/v1 Pod",
				ObservedAt: a.now,
				RawState:   "没有 Pod 声明 spec.nodeName",
				Impact:     "无法据此判断节点故障",
				NextAction: "确认实例是否已被调度",
				Basis:      BasisInference,
			}},
		})
		return nil
	}
	for _, name := range names {
		target := Target{Version: "v1", Kind: "Node", Resource: "nodes", Name: name}
		observed, err := a.service.Source.Get(ctx, target)
		if apierrors.IsNotFound(err) {
			missing := ObjectRef{Version: "v1", Kind: "Node", Name: name}
			conclusion := fmt.Sprintf("节点 %s 不存在", name)
			a.segments = append(a.segments, Segment{
				Link: LinkNode, Object: missing, Observable: true,
				Evidence: []Evidence{{
					Conclusion: conclusion,
					Object:     missing,
					Source:     sourceName(missing),
					ObservedAt: a.now,
					RawState:   err.Error(),
					Impact:     "实例所在节点已离开集群",
					NextAction: "检查节点状态",
					Basis:      BasisFact,
				}},
			})
			a.failures = append(a.failures, failure{
				link: LinkNode, object: missing, summary: conclusion, nextAction: "检查节点状态",
			})
			continue
		}
		if err != nil {
			return fmt.Errorf("read node %s: %w", name, err)
		}
		a.remember(observed)
		ready, reason, message := nodeReady(observed.Object)
		conclusion := fmt.Sprintf("节点 %s 就绪", name)
		impact := "节点可承载工作负载"
		nextAction := "无需处理"
		entries := make([]Evidence, 0, 2)
		if !ready {
			conclusion = fmt.Sprintf("节点 %s 未就绪：%s %s", name, reason, message)
			impact = "该节点上的实例不可用"
			nextAction = "恢复节点或迁移工作负载"
			a.failures = append(a.failures, failure{
				link: LinkNode, object: observed.Ref, summary: conclusion, nextAction: nextAction,
			})
		}
		entries = append(entries, Evidence{
			Conclusion: conclusion,
			Object:     observed.Ref,
			Source:     sourceName(observed.Ref),
			ObservedAt: observed.ObservedAt,
			RawState:   nodeState(observed.Object),
			Impact:     impact,
			NextAction: nextAction,
			Basis:      BasisFact,
		})
		if !ready && a.workloadUnready {
			entries = append(entries, Evidence{
				Conclusion: fmt.Sprintf("工作负载没有可用副本，可能由节点 %s 未就绪导致", name),
				Object:     observed.Ref,
				Source:     sourceName(observed.Ref),
				ObservedAt: observed.ObservedAt,
				RawState:   "工作负载就绪副本不足，且其 Pod 位于未就绪节点",
				Impact:     "入口保持不可达",
				NextAction: "恢复节点后观察工作负载是否回到就绪",
				Basis:      BasisInference,
			})
		}
		a.segments = append(a.segments, Segment{
			Link: LinkNode, Object: observed.Ref, Observable: true, Evidence: entries,
		})
	}
	return nil
}

// extensionSegments reports the extension classes the application rendered and
// the components the active adapters cover. A class no active adapter covers
// keeps its segment marked unobservable; a read failure is a chain error, like
// every other segment (C-26).
func (a *assembly) extensionSegments(ctx context.Context) error {
	for _, rendered := range a.application.Status.Rendered {
		if !isExtensionClass(rendered.Group) {
			continue
		}
		ref := ObjectRef{
			Group: rendered.Group, Version: rendered.Version, Kind: rendered.Kind,
			Namespace: rendered.Namespace, Name: rendered.Name,
		}
		adapter, found := a.service.Environment.adapterFor(rendered.Group, rendered.Kind)
		if !found {
			a.segments = append(a.segments, Segment{
				Link: LinkExtension, Object: ref, Observable: false,
				Reason: fmt.Sprintf("没有活动的诊断适配器覆盖 %s/%s", rendered.Group, rendered.Kind),
			})
			continue
		}
		target := Target{
			Group: rendered.Group, Version: adapter.Version, Kind: rendered.Kind,
			Resource: adapter.Resource, Namespace: rendered.Namespace, Name: rendered.Name,
		}
		observed, err := a.service.Source.Get(ctx, target)
		if apierrors.IsNotFound(err) {
			missing := ObjectRef{Group: rendered.Group, Version: adapter.Version, Kind: rendered.Kind, Namespace: rendered.Namespace, Name: rendered.Name}
			conclusion := fmt.Sprintf("扩展对象 %s %s 不存在", rendered.Kind, rendered.Name)
			a.segments = append(a.segments, Segment{
				Link: adapter.link(), Object: missing, Observable: true,
				Evidence: []Evidence{{
					Conclusion: conclusion,
					Object:     missing,
					Source:     adapter.source(),
					ObservedAt: a.now,
					RawState:   err.Error(),
					Impact:     "应用声明的扩展对象没有被写入",
					NextAction: "重新调和应用以写入该对象",
					Basis:      BasisFact,
				}},
			})
			a.failures = append(a.failures, failure{
				link: adapter.link(), object: missing, summary: conclusion, nextAction: "重新调和应用",
			})
			continue
		}
		if err != nil {
			return fmt.Errorf("read extension object %s: %w", ref.String(), err)
		}
		a.remember(observed)
		phase, health, raw := stateOf(observed.Object)
		conclusion := fmt.Sprintf("%s %s 处于 %s，健康 %s", ref.Kind, ref.Name, valueOr(phase, "未知"), valueOr(health, "未知"))
		a.segments = append(a.segments, Segment{
			Link: adapter.link(), Object: observed.Ref, Observable: true,
			Evidence: []Evidence{{
				Conclusion: conclusion,
				Object:     observed.Ref,
				Source:     adapter.source(),
				ObservedAt: observed.ObservedAt,
				RawState:   raw,
				Impact:     "扩展对象参与该应用的运行",
				NextAction: "无需处理",
				Basis:      BasisFact,
			}},
		})
	}
	return a.componentSegments()
}

// componentSegments reports the components active adapters cover, at the
// segment each adapter declares (C-26).
func (a *assembly) componentSegments() error {
	seen := map[string]bool{}
	for _, adapter := range a.service.Environment.Adapters {
		for _, identifier := range adapter.Components {
			key := adapter.ID + "|" + identifier
			if seen[key] {
				continue
			}
			seen[key] = true
			link := adapter.link()
			component, found := a.service.Environment.component(identifier)
			if !found {
				a.segments = append(a.segments, Segment{
					Link: link, Object: componentRef(identifier), Observable: false,
					Reason: fmt.Sprintf("适配器 %s 覆盖的组件 %s 不在安装结果中", adapter.ID, identifier),
				})
				continue
			}
			ref := componentRef(component.Name)
			conclusion := fmt.Sprintf("组件 %s 处于 %s，健康 %s", identifier, valueOr(component.Phase, "未知"), valueOr(component.Health, "未知"))
			impact := "组件贡献按声明生效"
			nextAction := "无需处理"
			if component.Phase != "" && component.Phase != "Ready" {
				impact = "组件贡献可能未全部生效"
				nextAction = "检查组件条件与依赖"
				a.failures = append(a.failures, failure{
					link: link, object: ref, summary: conclusion, nextAction: nextAction,
				})
			}
			a.segments = append(a.segments, Segment{
				Link: link, Object: ref, Observable: true,
				Evidence: []Evidence{{
					Conclusion: conclusion,
					Object:     ref,
					Source:     adapter.source(),
					ObservedAt: a.now,
					RawState:   fmt.Sprintf("identifier=%s version=%s phase=%s health=%s", identifier, component.Version, valueOr(component.Phase, "-"), valueOr(component.Health, "-")),
					Impact:     impact,
					NextAction: nextAction,
					Basis:      BasisFact,
				}},
			})
		}
	}
	return nil
}

// link is the chain link the adapter's evidence is inserted at; the dependency
// segment is the default (C-26).
func (a Adapter) link() Link {
	if a.Segment == "" {
		return LinkExtension
	}
	return a.Segment
}

// source is the data source the adapter's evidence names; the adapter
// identifier is the default (C-26).
func (a Adapter) source() string {
	if a.Source == "" {
		return "diagnostic adapter " + a.ID
	}
	return a.Source
}

// component resolves one covered component by identifier.
func (e Environment) component(identifier string) (Component, bool) {
	for _, component := range e.Components {
		if component.Identifier == identifier {
			return component, true
		}
	}
	return Component{}, false
}

// componentRef names the system component object an extension identifier is
// installed as.
func componentRef(name string) ObjectRef {
	return ObjectRef{Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "SystemComponent", Name: name}
}

// remember records one observation for the bundle's condition snapshot, once
// per object, in chain order.
func (a *assembly) remember(observed Observed) {
	if a.observedIndex[observed.Ref] {
		return
	}
	a.observedIndex[observed.Ref] = true
	a.observed = append(a.observed, observed)
}

// podsMatching lists the pods one selector matches and records them for the node
// segment.
func (a *assembly) podsMatching(ctx context.Context, namespace string, selector map[string]string) ([]Observed, error) {
	if len(selector) == 0 {
		return nil, nil
	}
	listed, cached := a.podLists[namespace]
	if !cached {
		var err error
		listed, err = a.service.Source.List(ctx, Target{
			Version: "v1", Kind: "Pod", Resource: "pods", Namespace: namespace,
		})
		if err != nil {
			return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
		}
		a.podLists[namespace] = listed
	}
	matched := make([]Observed, 0, len(listed))
	for _, pod := range listed {
		if !matchesSelector(pod.Object.GetLabels(), selector) {
			continue
		}
		matched = append(matched, pod)
		a.addPod(pod)
	}
	sort.Slice(matched, func(left, right int) bool { return matched[left].Ref.Name < matched[right].Ref.Name })
	return matched, nil
}

// addPod records one pod once and the node it runs on.
func (a *assembly) addPod(observed Observed) {
	for _, existing := range a.pods {
		if existing.Ref == observed.Ref {
			return
		}
	}
	a.pods = append(a.pods, observed)
	a.observed = append(a.observed, observed)
	if node, _, _ := unstructured.NestedString(observed.Object.Object, "spec", "nodeName"); node != "" {
		a.podNodes[observed.Ref] = node
	}
}

// matchesSelector reports whether labels carry every pair of selector.
func matchesSelector(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// adapterFor returns the first active adapter covering one class.
func (e Environment) adapterFor(group, kind string) (Adapter, bool) {
	for _, adapter := range e.Adapters {
		if adapter.Group == group && adapter.Kind == kind {
			return adapter, true
		}
	}
	return Adapter{}, false
}

// applicationTarget names the application class.
func applicationTarget(namespace, name string) Target {
	resource, _ := api.ResourceName("Application")
	return Target{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Application",
		Resource: resource, Namespace: namespace, Name: name,
	}
}

// endpointTarget names the endpoint class.
func endpointTarget(namespace string) Target {
	resource, _ := api.ResourceName("Endpoint")
	return Target{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Kind: "Endpoint",
		Resource: resource, Namespace: namespace,
	}
}

// routeClass returns the route object class an endpoint kind owns (C-43).
func routeClass(kind v1beta1.EndpointKind) (routeObject, bool) {
	switch kind {
	case v1beta1.EndpointKindHTTPHost, v1beta1.EndpointKindHTTPPath:
		return routeObject{group: "networking.k8s.io", version: "v1", kind: "Ingress", resource: "ingresses"}, true
	case v1beta1.EndpointKindTCP, v1beta1.EndpointKindUDP:
		return routeObject{version: "v1", kind: "Service", resource: "services"}, true
	default:
		return routeObject{}, false
	}
}

// routeObject is the downstream class one endpoint shape owns (C-43).
type routeObject struct {
	group    string
	version  string
	kind     string
	resource string
}

// backendRef is one backend a route object names (C-43): a service or any
// other object class.
type backendRef struct {
	// Group is the backend object's API group; empty for the core group.
	Group string
	// Kind is the backend object kind.
	Kind string
	// Name is the backend object name.
	Name string
	// Namespace is the route object's namespace: a backend is same-namespace.
	Namespace string
	// Port is the service port the route names; empty for a resource backend.
	Port string
}

// backendsOf returns every backend a route object names, in object order.
func backendsOf(route Observed) []backendRef {
	switch route.Ref.Kind {
	case "Service":
		return []backendRef{{Kind: "Service", Name: route.Ref.Name, Namespace: route.Ref.Namespace}}
	case "Ingress":
		return ingressBackends(route)
	default:
		return nil
	}
}

// ingressBackends reads both backend forms of an Ingress: a service backend and
// a resource backend, from the default backend and every rule path (C-43). The
// two forms are read from maps and slices directly; nested field helpers reject
// the array indices this shape needs.
func ingressBackends(route Observed) []backendRef {
	refs := make([]backendRef, 0, 2)
	seen := map[string]bool{}
	add := func(document map[string]any) {
		ref, ok := backendOf(document, route.Ref.Namespace)
		if !ok {
			return
		}
		key := ref.Group + "/" + ref.Kind + "/" + ref.Name
		if seen[key] {
			return
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	if backend, found, err := unstructured.NestedMap(route.Object.Object, "spec", "defaultBackend"); err == nil && found {
		add(backend)
	}
	rules, found, err := unstructured.NestedSlice(route.Object.Object, "spec", "rules")
	if err != nil || !found {
		return refs
	}
	for _, entry := range rules {
		rule, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		httpRule, ok := rule["http"].(map[string]any)
		if !ok {
			continue
		}
		paths, ok := httpRule["paths"].([]any)
		if !ok {
			continue
		}
		for _, item := range paths {
			path, ok := item.(map[string]any)
			if !ok {
				continue
			}
			backend, ok := path["backend"].(map[string]any)
			if !ok {
				continue
			}
			add(backend)
		}
	}
	return refs
}

// backendOf reads one backend document: service.name with its port, or
// resource.apiGroup/kind/name.
func backendOf(document map[string]any, namespace string) (backendRef, bool) {
	if service, ok := document["service"].(map[string]any); ok {
		name, _ := service["name"].(string)
		if name == "" {
			return backendRef{}, false
		}
		return backendRef{Kind: "Service", Name: name, Namespace: namespace, Port: backendPort(service["port"])}, true
	}
	if resource, ok := document["resource"].(map[string]any); ok {
		kind, _ := resource["kind"].(string)
		name, _ := resource["name"].(string)
		group, _ := resource["apiGroup"].(string)
		if kind == "" || name == "" {
			return backendRef{}, false
		}
		return backendRef{Group: group, Kind: kind, Name: name, Namespace: namespace}, true
	}
	return backendRef{}, false
}

// backendPort renders the service port a backend names; both the number and the
// name form exist (C-43).
func backendPort(value any) string {
	port, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	if number, found := port["number"]; found {
		return fmt.Sprintf("%v", number)
	}
	name, _ := port["name"].(string)
	return name
}

// targetOf resolves the read target of a rendered object the chain knows
// natively.
func targetOf(ref v1beta1.ObjectRef) (Target, bool) {
	if ref.Group == api.Group {
		resource, ok := api.ResourceName(ref.Kind)
		if !ok {
			return Target{}, false
		}
		return Target{
			Group: ref.Group, Version: v1beta1.GroupVersion.Version, Kind: ref.Kind,
			Resource: resource, Namespace: ref.Namespace, Name: ref.Name,
		}, true
	}
	class, ok := nativeClasses[schema.GroupKind{Group: ref.Group, Kind: ref.Kind}]
	if !ok {
		return Target{}, false
	}
	return Target{
		Group: ref.Group, Version: class.version, Kind: ref.Kind,
		Resource: class.resource, Namespace: ref.Namespace, Name: ref.Name,
	}, true
}

// isExtensionClass reports whether a group belongs to an extension class: any
// group that is neither the kernel group nor a native one (C-19).
func isExtensionClass(group string) bool {
	return group != "" && group != api.Group && !nativeGroups[group]
}

// isWorkload reports whether a rendered kind is a workload object.
func isWorkload(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "Job", "CronJob":
		return true
	default:
		return false
	}
}

// sourceName renders the read source of one object.
func sourceName(ref ObjectRef) string {
	group := ref.Group
	if group == "" {
		group = "core"
	}
	if ref.Version == "" {
		return group + " " + ref.Kind
	}
	return group + "/" + ref.Version + " " + ref.Kind
}

// valueOr returns the value, or the fallback when it is empty.
func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// reconciledStatus reads the Reconciled condition of a stored object.
func reconciledStatus(object *unstructured.Unstructured) (status, reason, message string) {
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != api.ConditionReconciled {
			continue
		}
		status, _ = condition["status"].(string)
		reason, _ = condition["reason"].(string)
		message, _ = condition["message"].(string)
		return status, reason, message
	}
	return "", "", ""
}

// stateOf renders an object's phase, health and conditions as the raw state of
// an evidence entry.
func stateOf(object *unstructured.Unstructured) (phase, health, raw string) {
	phase, _, _ = unstructured.NestedString(object.Object, "status", "phase")
	health, _, _ = unstructured.NestedString(object.Object, "status", "health", "state")
	conditions, _, _ := unstructured.NestedSlice(object.Object, "status", "conditions")
	parts := make([]string, 0, len(conditions))
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		text := fmt.Sprintf("%v=%v", condition["type"], condition["status"])
		if reason, ok := condition["reason"].(string); ok && reason != "" {
			text += "(" + reason + ")"
		}
		if message, ok := condition["message"].(string); ok && message != "" {
			text += " " + message
		}
		parts = append(parts, text)
	}
	raw = fmt.Sprintf("phase=%s health=%s", valueOr(phase, "-"), valueOr(health, "-"))
	if len(parts) > 0 {
		raw += " conditions[" + strings.Join(parts, "; ") + "]"
	}
	return phase, health, raw
}

// workloadState renders a workload's replica facts.
func workloadState(object *unstructured.Unstructured) string {
	desired, ready, hasReplicas := replicaFacts(object)
	if !hasReplicas {
		_, _, raw := stateOf(object)
		return raw
	}
	return fmt.Sprintf("replicas=%d readyReplicas=%d", desired, ready)
}

// replicaFacts reads the desired and ready replica counts of a workload.
func replicaFacts(object *unstructured.Unstructured) (desired, ready int64, hasReplicas bool) {
	raw, found, err := unstructured.NestedFieldNoCopy(object.Object, "spec", "replicas")
	if err != nil || !found {
		return 0, 0, false
	}
	number, ok := raw.(int64)
	if !ok {
		return 0, 0, false
	}
	ready, _, _ = unstructured.NestedInt64(object.Object, "status", "readyReplicas")
	return number, ready, true
}

// podReady reports whether a pod's Ready condition is true.
func podReady(pod *unstructured.Unstructured) bool {
	conditions, _, _ := unstructured.NestedSlice(pod.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		status, _ := condition["status"].(string)
		return status == "True"
	}
	return false
}

// nodeReady reports whether a node's Ready condition is true.
func nodeReady(node *unstructured.Unstructured) (ready bool, reason, message string) {
	conditions, _, _ := unstructured.NestedSlice(node.Object, "status", "conditions")
	for _, entry := range conditions {
		condition, ok := entry.(map[string]any)
		if !ok || condition["type"] != "Ready" {
			continue
		}
		status, _ := condition["status"].(string)
		reason, _ = condition["reason"].(string)
		message, _ = condition["message"].(string)
		return status == "True", reason, message
	}
	return false, "ReadyConditionMissing", "节点没有 Ready 条件"
}

// nodeState renders a node's readiness conditions.
func nodeState(node *unstructured.Unstructured) string {
	ready, reason, message := nodeReady(node)
	return fmt.Sprintf("ready=%t reason=%s message=%s", ready, valueOr(reason, "-"), valueOr(message, "-"))
}

// deploymentNamespace is where the application renders its objects (C-32).
func (a *assembly) deploymentNamespace() string {
	if namespace := strings.TrimSpace(a.application.Spec.Namespace); namespace != "" {
		return namespace
	}
	return a.ref.Namespace
}

// now reads the service clock, defaulting to the wall clock.
func (s *Service) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}
