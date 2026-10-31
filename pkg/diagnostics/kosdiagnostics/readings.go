package kosdiagnostics

import (
	"context"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/diagnostics"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
	"github.com/tsic404/kos/pkg/pages/cluster"
	volumeapi "github.com/tsic404/kos/pkg/volume"
)

// chainLimit bounds how many unhealthy applications one read assembles a chain
// for: the page names reasons, it does not export every bundle at once.
const chainLimit = 25

// Readings resolves the items the diagnostics page declares (C-53).
type Readings struct {
	// Dynamic reads the cluster.
	Dynamic dynamic.Interface
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
	// Environment carries the extension state the chain reports; empty leaves
	// the extension segment unobservable, which C-26 allows.
	Environment diagnostics.Environment
}

// Read implements pages.Readings.
func (r *Readings) Read(ctx context.Context, session identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	value, failure := r.value(ctx, session, item)
	if failure != nil {
		return pages.Reading{}, failure
	}
	return pages.Reading{Value: value, ObservedAt: r.reader().Clock()}, nil
}

// value resolves one item from the source it declares.
func (r *Readings) value(ctx context.Context, session identity.Session, item extension.PageItem) (any, *controlplane.Error) {
	switch item.ID {
	case ItemDegradation:
		return r.degradation(ctx, session)
	case ItemBlocked:
		return r.blocked(ctx, session)
	case ItemEvidence:
		return r.evidence(ctx, session)
	case ItemExport:
		return r.exportTargets(ctx, session)
	default:
		return nil, unknownItem(item.ID)
	}
}

// reader is the cluster read seam for this call.
func (r *Readings) reader() *cluster.Reader {
	return &cluster.Reader{Dynamic: r.Dynamic, Now: r.Now}
}

// service assembles evidence chains over the same cluster client (C-26).
func (r *Readings) service() *diagnostics.Service {
	return &diagnostics.Service{
		Source:      diagnostics.ClusterSource{Dynamic: r.Dynamic, Now: r.Now},
		Environment: r.Environment,
		Now:         r.Now,
	}
}

// applications lists the applications the session may see, ordered by namespace
// and name.
func (r *Readings) applications(ctx context.Context, session identity.Session) ([]v1beta1.Application, *controlplane.Error) {
	objects, failure := r.reader().List(ctx, "Application", "")
	if failure != nil {
		return nil, failure
	}
	applications := make([]v1beta1.Application, 0, len(objects))
	for _, object := range objects {
		var application v1beta1.Application
		if failure := cluster.Decode(object, &application); failure != nil {
			return nil, failure
		}
		visible, failure := r.reader().Visible(ctx, session, "Application", object)
		if failure != nil {
			return nil, failure
		}
		if !visible {
			continue
		}
		applications = append(applications, application)
	}
	sort.Slice(applications, func(left, right int) bool {
		if applications[left].Namespace != applications[right].Namespace {
			return applications[left].Namespace < applications[right].Namespace
		}
		return applications[left].Name < applications[right].Name
	})
	return applications, nil
}

// unhealthy reports an object whose health is not Healthy (C-08).
func unhealthy(status v1beta1.ObjectStatus) bool {
	return status.Health.State != "" && status.Health.State != v1beta1.HealthStateHealthy
}

// reasonView is one degradation reason with its next action (C-51).
type reasonView struct {
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	Health     string `json:"health,omitempty"`
	Condition  string `json:"condition,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Message    string `json:"message,omitempty"`
	NextAction string `json:"nextAction"`
	Basis      string `json:"basis"`
}

// degradation aggregates the conditions of the unhealthy objects C-51 names:
// applications through their evidence chain, every other kind through the
// condition it carries.
func (r *Readings) degradation(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	reasons := make([]reasonView, 0)
	service := r.service()
	for _, application := range applications {
		if !unhealthy(application.Status.ObjectStatus) {
			continue
		}
		evidence := reasonView{
			Kind: "Application", Namespace: application.Namespace, Name: application.Name,
			Health:     string(application.Status.Health.State),
			Basis:      string(diagnostics.BasisInference),
			NextAction: "查看该应用的证据链定位失败来源",
		}
		chain, err := service.Chain(ctx, application.Namespace, application.Name)
		switch {
		case err == nil:
			evidence.Condition = string(chain.Conclusion.Segment)
			evidence.Reason = chain.Conclusion.Summary
			evidence.Basis = string(chain.Conclusion.Basis)
			if chain.Conclusion.NextAction != "" {
				evidence.NextAction = chain.Conclusion.NextAction
			}
		case apierrors.IsNotFound(err):
			evidence.Reason = "应用在读取证据链时已不存在"
		default:
			evidence.Reason = "证据链组装失败：" + err.Error()
		}
		reasons = append(reasons, evidence)
	}
	for _, class := range cluster.StatusKinds() {
		if class == "Application" {
			// Applications already contributed their evidence-chain reason.
			continue
		}
		objects, failure := r.reader().List(ctx, class, "")
		if failure != nil {
			return nil, failure
		}
		for _, object := range objects {
			var shape struct {
				Status v1beta1.ObjectStatus `json:"status"`
			}
			if failure := cluster.Decode(object, &shape); failure != nil {
				return nil, failure
			}
			if !unhealthy(shape.Status) {
				continue
			}
			visible, failure := r.reader().Visible(ctx, session, class, object)
			if failure != nil {
				return nil, failure
			}
			if !visible {
				continue
			}
			reasons = append(reasons, reasonView{
				Kind: class, Namespace: cluster.Text(object, "metadata", "namespace"),
				Name:       cluster.Text(object, "metadata", "name"),
				Health:     string(shape.Status.Health.State),
				Condition:  firstUnmetCondition(shape.Status.Conditions),
				Reason:     firstUnmetReason(shape.Status.Conditions),
				NextAction: nextActionOf(shape.Status.Conditions),
				Basis:      string(diagnostics.BasisFact),
			})
		}
	}
	sort.Slice(reasons, func(left, right int) bool {
		if reasons[left].Kind != reasons[right].Kind {
			return reasons[left].Kind < reasons[right].Kind
		}
		if reasons[left].Namespace != reasons[right].Namespace {
			return reasons[left].Namespace < reasons[right].Namespace
		}
		return reasons[left].Name < reasons[right].Name
	})
	return map[string]any{
		"rule":    "健康状态不是 Healthy 的对象按条件聚合，应用另取证据链结论（C-26、C-53）",
		"reasons": reasons,
	}, nil
}

// firstUnmetCondition names the first condition that is not met.
func firstUnmetCondition(conditions []metav1.Condition) string {
	for _, condition := range conditions {
		if condition.Status != metav1.ConditionTrue {
			return condition.Type
		}
	}
	return ""
}

// firstUnmetReason returns the reason of the first condition that is not met.
func firstUnmetReason(conditions []metav1.Condition) string {
	for _, condition := range conditions {
		if condition.Status != metav1.ConditionTrue {
			return condition.Message
		}
	}
	return ""
}

// nextActionOf suggests what resolves the first unmet condition. The suggestion
// is the condition type's known remedy; an unknown condition falls back to
// reading the evidence chain.
func nextActionOf(conditions []metav1.Condition) string {
	for _, condition := range conditions {
		if condition.Status == metav1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case app.ConditionCapabilityMissing, app.ConditionCapabilityUnsatisfied:
			return "安装提供所需能力的组件或 Provider，然后重试"
		case app.ConditionDefinitionMissing:
			return "安装或同步应用定义后重试"
		case app.ConditionNamespaceMissing:
			return "创建应用声明的命名空间后重试"
		case volumeapi.ConditionClaimBlocked:
			return "检查声明与存储类的绑定条件"
		case volumeapi.ConditionLost:
			return "检查绑定节点与后端数据是否恢复"
		case volumeapi.ConditionDeletionBlocked:
			return "先移除仍引用该对象的扩展对象"
		default:
			return "查看该对象的证据链定位失败来源"
		}
	}
	return "查看该对象的证据链定位失败来源"
}

// blockedObject names one uninstall-blocked object (C-51).
type blockedObject struct {
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace,omitempty"`
	Name      string   `json:"name"`
	Stalled   bool     `json:"stalled"`
	Since     string   `json:"since,omitempty"`
	Objects   []string `json:"objects,omitempty"`
	Condition string   `json:"condition,omitempty"`
}

// blocked lists the stuck uninstalls and the blocked volumes (C-51).
func (r *Readings) blocked(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	objects := make([]blockedObject, 0)
	for _, application := range applications {
		uninstall := application.Status.Uninstall
		if uninstall == nil || len(uninstall.Blocked) == 0 {
			continue
		}
		entry := blockedObject{
			Kind: "Application", Namespace: application.Namespace, Name: application.Name,
			Stalled: uninstall.Stalled,
		}
		if uninstall.BlockedSince != nil {
			entry.Since = uninstall.BlockedSince.UTC().Format(time.RFC3339)
		}
		for _, ref := range uninstall.Blocked {
			entry.Objects = append(entry.Objects, ref.Kind+"/"+ref.Namespace+"/"+ref.Name)
		}
		objects = append(objects, entry)
	}
	volumes, failure := r.reader().List(ctx, "Volume", "")
	if failure != nil {
		return nil, failure
	}
	for _, object := range volumes {
		var volume v1beta1.Volume
		if failure := cluster.Decode(object, &volume); failure != nil {
			return nil, failure
		}
		condition, found := blockedCondition(volume.Status.Conditions)
		if !found {
			continue
		}
		objects = append(objects, blockedObject{
			Kind: "Volume", Namespace: volume.Namespace, Name: volume.Name,
			Condition: condition.Type, Objects: []string{condition.Message},
		})
	}
	return map[string]any{"objects": objects}, nil
}

// blockedCondition finds the first blocking or lost condition of a volume.
func blockedCondition(conditions []metav1.Condition) (metav1.Condition, bool) {
	for _, condition := range conditions {
		if condition.Status != metav1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case volumeapi.ConditionClaimBlocked, volumeapi.ConditionLost, volumeapi.ConditionDeletionBlocked:
			return condition, true
		}
	}
	return metav1.Condition{}, false
}

// evidenceEntry is one application's evidence chain (C-26).
type evidenceEntry struct {
	Namespace string             `json:"namespace"`
	Name      string             `json:"name"`
	Health    string             `json:"health,omitempty"`
	Chain     *diagnostics.Chain `json:"chain,omitempty"`
	Error     string             `json:"error,omitempty"`
}

// evidence assembles the evidence chain of every unhealthy application the
// session may see (C-26, C-51).
func (r *Readings) evidence(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	service := r.service()
	entries := make([]evidenceEntry, 0)
	for _, application := range applications {
		if !unhealthy(application.Status.ObjectStatus) {
			continue
		}
		if len(entries) >= chainLimit {
			break
		}
		entry := evidenceEntry{
			Namespace: application.Namespace, Name: application.Name,
			Health: string(application.Status.Health.State),
		}
		chain, err := service.Chain(ctx, application.Namespace, application.Name)
		switch {
		case err == nil:
			entry.Chain = &chain
		case apierrors.IsNotFound(err):
			entry.Error = "应用在读取证据链时已不存在"
		default:
			entry.Error = "证据链组装失败：" + err.Error()
		}
		entries = append(entries, entry)
	}
	return map[string]any{"applications": entries}, nil
}

// exportTargets names the applications a diagnostic bundle can be exported for
// (C-26, C-51).
func (r *Readings) exportTargets(ctx context.Context, session identity.Session) (any, *controlplane.Error) {
	applications, failure := r.applications(ctx, session)
	if failure != nil {
		return nil, failure
	}
	targets := make([]map[string]string, 0, len(applications))
	for _, application := range applications {
		targets = append(targets, map[string]string{
			"namespace": application.Namespace, "name": application.Name,
			"health": string(application.Status.Health.State),
		})
	}
	return map[string]any{"applications": targets}, nil
}

// unknownItem marks an item this product does not declare.
func unknownItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "kos-diagnostics 没有声明页面条目 " + id,
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
