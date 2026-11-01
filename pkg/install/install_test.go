package install

import (
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/markers"
)

// objectState builds one snapshot entry with the given labels and fields.
func objectState(labels map[string]string, fields map[string]string) ObjectState {
	if fields == nil {
		fields = map[string]string{}
	}
	return ObjectState{Labels: labels, Fields: fields}
}

// snapshotOf indexes entries by their key.
func snapshotOf(objects map[ObjectKey]ObjectState) *Snapshot {
	return &Snapshot{At: time.Now(), Objects: objects}
}

// TestVerdictAppliesTheWriterWhitelist pins C-15: the judgment is per writer,
// a KOS subject may write the kernel kinds, its namespace, an installed
// extension class and its own attributed downstream objects, and any other
// KOS write is a violation naming object and requester.
func TestVerdictAppliesTheWriterWhitelist(t *testing.T) {
	installer := "system:serviceaccount:kos-system:kos-installer"
	controller := "system:serviceaccount:kos-system:kos-control-plane"
	extensionKind := schema.GroupKind{Group: "widgets.example.test", Kind: "Widget"}

	downstreamKey := ObjectKey{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "shop"}
	before := snapshotOf(map[ObjectKey]ObjectState{
		downstreamKey: objectState(nil, map[string]string{"spec.replicas": "1"}),
	})
	statefulKey := ObjectKey{Group: "apps", Kind: "StatefulSet", Namespace: "default", Name: "db"}
	roleBindingKey := ObjectKey{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding", Namespace: "default", Name: ControlPlaneName}
	renderedBindingKey := ObjectKey{Group: "rbac.authorization.k8s.io", Kind: "RoleBinding", Namespace: "shop", Name: ControlPlaneName}
	extensionRoleKey := ObjectKey{Group: "rbac.authorization.k8s.io", Kind: "Role", Namespace: "kube-system", Name: "widgets-reader"}
	after := snapshotOf(map[ObjectKey]ObjectState{
		downstreamKey: objectState(
			map[string]string{markers.ManagedBy: "kos-shell", markers.OwnerUID: "app-uid"},
			map[string]string{"spec.replicas": "2"},
		),
		statefulKey: objectState(map[string]string{markers.ManagedBy: "kos-shell"}, nil),
		roleBindingKey: objectState(nil, map[string]string{
			"roleRef.apiGroup": "rbac.authorization.k8s.io",
			"roleRef.kind":     "ClusterRole",
			"roleRef.name":     "cluster-admin",
		}),
		renderedBindingKey: objectState(nil, map[string]string{
			"roleRef.apiGroup": "rbac.authorization.k8s.io",
			"roleRef.kind":     "Role",
			"roleRef.name":     ControlPlaneName,
		}),
		extensionRoleKey: objectState(
			map[string]string{markers.ManagedBy: "kos-shell", markers.OwnerUID: "component-uid"}, nil,
		),
	})

	cases := []struct {
		name   string
		event  AuditEvent
		passed bool
	}{
		{
			name:   "kernel object",
			event:  AuditEvent{Verb: "update", User: installer, Object: v1beta1.ObjectRef{Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "shop"}},
			passed: true,
		},
		{
			name:   "installation namespace",
			event:  AuditEvent{Verb: "create", User: installer, Object: v1beta1.ObjectRef{Kind: "ConfigMap", Namespace: "kos-system", Name: "record"}},
			passed: true,
		},
		{
			name:   "extension class",
			event:  AuditEvent{Verb: "create", User: controller, Object: v1beta1.ObjectRef{Group: "widgets.example.test", Kind: "Widget", Namespace: "default", Name: "w"}},
			passed: true,
		},
		{
			name: "extension class definition",
			event: AuditEvent{Verb: "create", User: controller, Object: v1beta1.ObjectRef{
				Group: "apiextensions.k8s.io", Kind: "CustomResourceDefinition", Name: "widgets.widgets.example.test",
			}},
			passed: true,
		},
		{
			name: "rendered namespaced role",
			event: AuditEvent{Verb: "create", User: installer, Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Kind: "Role", Namespace: "default", Name: ControlPlaneName,
			}},
			passed: true,
		},
		{
			name: "rendered-name role outside the granted namespaces",
			event: AuditEvent{Verb: "create", User: installer, Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Kind: "Role", Namespace: "kube-system", Name: ControlPlaneName,
			}},
			passed: false,
		},
		{
			name: "binding that points at a cluster role",
			event: AuditEvent{Verb: "create", User: installer, Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Kind: "RoleBinding", Namespace: "default", Name: ControlPlaneName,
			}},
			passed: false,
		},
		{
			name: "rendered binding that points at the rendered role",
			event: AuditEvent{Verb: "create", User: installer, Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Kind: "RoleBinding", Namespace: "shop", Name: ControlPlaneName,
			}},
			passed: true,
		},
		{
			name: "extension role with attribution outside the granted namespaces",
			event: AuditEvent{Verb: "create", User: controller, Object: v1beta1.ObjectRef{
				Group: "rbac.authorization.k8s.io", Kind: "Role", Namespace: "kube-system", Name: "widgets-reader",
			}},
			passed: true,
		},
		{
			name:   "attributed downstream object",
			event:  AuditEvent{Verb: "update", User: controller, Object: v1beta1.ObjectRef{Group: "apps", Kind: "Deployment", Namespace: "default", Name: "shop"}},
			passed: true,
		},
		{
			name:   "unattributed native object",
			event:  AuditEvent{Verb: "create", User: installer, Object: v1beta1.ObjectRef{Kind: "ConfigMap", Namespace: "kube-system", Name: "leak"}},
			passed: false,
		},
		{
			name:   "attributed native object without an owner",
			event:  AuditEvent{Verb: "create", User: controller, Object: v1beta1.ObjectRef{Group: "apps", Kind: "StatefulSet", Namespace: "default", Name: "db"}},
			passed: false,
		},
		{
			name:   "a cluster controller's write is not judged",
			event:  AuditEvent{Verb: "create", User: "system:serviceaccount:kube-system:statefulset-controller", Object: v1beta1.ObjectRef{Kind: "ConfigMap", Namespace: "kube-system", Name: "leak"}},
			passed: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			verdict := Judgement{
				Before:          before,
				After:           after,
				Events:          []AuditEvent{testCase.event},
				Subjects:        []string{installer, controller},
				ExtensionKinds:  []schema.GroupKind{extensionKind},
				Namespace:       "kos-system",
				GrantNamespaces: []string{"default", "shop"},
			}.Verdict()
			if verdict.Passed != testCase.passed {
				t.Fatalf("passed = %v, want %v (%+v)", verdict.Passed, testCase.passed, verdict.Violations)
			}
			if !verdict.Measured {
				t.Fatalf("verdict = %+v, want a measured window", verdict)
			}
		})
	}
}

// TestVerdictNamesObjectRequesterAndFields pins the judgment output C-15
// requires.
func TestVerdictNamesObjectRequesterAndFields(t *testing.T) {
	installer := "system:serviceaccount:kos-system:kos-installer"
	key := ObjectKey{Kind: "ConfigMap", Namespace: "kube-system", Name: "leak"}
	verdict := Judgement{
		Before: snapshotOf(map[ObjectKey]ObjectState{key: objectState(nil, map[string]string{"data.value": "old"})}),
		After:  snapshotOf(map[ObjectKey]ObjectState{key: objectState(nil, map[string]string{"data.value": "new"})}),
		Events: []AuditEvent{{
			Verb: "update", User: installer,
			Object: v1beta1.ObjectRef{Kind: "ConfigMap", Namespace: "kube-system", Name: "leak"},
		}},
		Subjects:  []string{installer},
		Namespace: "kos-system",
	}.Verdict()
	if len(verdict.Violations) != 1 {
		t.Fatalf("violations = %+v, want one", verdict.Violations)
	}
	violation := verdict.Violations[0]
	if violation.Object.Name != "leak" || violation.Object.Namespace != "kube-system" ||
		violation.Object.Kind != "ConfigMap" || violation.Requester != installer {
		t.Fatalf("violation = %+v, want the object and requester named", violation)
	}
	if len(violation.Fields) != 1 || violation.Fields[0] != "data.value" {
		t.Fatalf("fields = %v, want the changed path", violation.Fields)
	}
}

// TestVerdictIsUnmeasuredWithoutAudit pins ADR-017: a window that could not be
// measured is not a pass.
func TestVerdictIsUnmeasuredWithoutAudit(t *testing.T) {
	for name, judgement := range map[string]Judgement{
		"no audit":      {AuditError: ErrAuditUnavailable},
		"unknown scope": {ScopeError: errors.New("extension declaration unreadable")},
	} {
		t.Run(name, func(t *testing.T) {
			verdict := judgement.Verdict()
			if verdict.Measured || verdict.Passed {
				t.Fatalf("verdict = %+v, want unmeasured and not passed", verdict)
			}
			if verdict.Reason == "" {
				t.Fatal("an unmeasured verdict carries no reason")
			}
		})
	}
}

// TestQuietWindowIsTheContractValue pins the C-15 observation point while the
// acceptance cases shorten it for run time.
func TestQuietWindowIsTheContractValue(t *testing.T) {
	if DefaultQuietWindow != 60*time.Second {
		t.Fatalf("quiet window = %s, want 60s", DefaultQuietWindow)
	}
}

// TestPlanNormalizeRefusesFormsItCannotHonour pins the installation forms of
// C-54: the external-control-plane form is pending ADR-023, an existing
// cluster declares no nodes, and a bootstrap install declares at least one.
func TestPlanNormalizeRefusesFormsItCannotHonour(t *testing.T) {
	cases := []struct {
		name    string
		plan    Plan
		wantErr string
	}{
		{name: "external form", plan: Plan{Mode: ModeExternal, Owner: "owner"}, wantErr: "ADR-023"},
		{name: "missing owner", plan: Plan{Mode: ModeExisting}, wantErr: "first Owner"},
		{name: "existing with nodes", plan: Plan{Mode: ModeExisting, Owner: "owner", Nodes: 3}, wantErr: "declares no nodes"},
		{name: "bootstrap without nodes", plan: Plan{Mode: ModeBootstrap, Owner: "owner"}, wantErr: "nodes"},
		{name: "application namespace equals own", plan: Plan{Mode: ModeExisting, Owner: "owner", Namespace: "kos-system", Applications: []string{"kos-system"}}, wantErr: "application namespace"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := testCase.plan.Normalize()
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("Normalize() error = %v, want one naming %q", err, testCase.wantErr)
			}
		})
	}
}

// TestRenderKeepsTheSingleReplicaPromise pins C-54 and F13: the control plane
// has one replica and the single-node notice claims no high availability.
func TestRenderKeepsTheSingleReplicaPromise(t *testing.T) {
	plan := Plan{Mode: ModeBootstrap, Owner: "owner", Nodes: 1}
	documents, err := Render(plan)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var deployment *unstructured.Unstructured
	for _, document := range documents {
		if document.GVR.Resource == "deployments" {
			deployment = document.Object
		}
		if document.GVR.Resource == "poddisruptionbudgets" {
			t.Fatalf("the render carries a disruption budget, which would promise availability")
		}
	}
	if deployment == nil {
		t.Fatal("the render carries no control-plane deployment")
	}
	replicas, found, err := unstructured.NestedInt64(deployment.Object, "spec", "replicas")
	if err != nil || !found || replicas != 1 {
		t.Fatalf("replicas = %d (found %v, err %v), want one", replicas, found, err)
	}
	single := Plan{Mode: ModeBootstrap, Owner: "owner", Nodes: 1}.Commitment()
	if single.Replicas != 1 || single.AutomaticMigration {
		t.Fatalf("commitment = %+v, want one replica and no migration", single)
	}
	if !strings.Contains(single.Notice, "不承诺高可用") || !strings.Contains(single.Notice, "单节点") {
		t.Fatalf("notice = %q, want the single-node degradation stated", single.Notice)
	}
	multi := Plan{Mode: ModeBootstrap, Owner: "owner", Nodes: 3}.Commitment()
	if !strings.Contains(multi.Notice, "3 个节点") || !strings.Contains(multi.Notice, "不承诺高可用") {
		t.Fatalf("notice = %q, want the declared node count and no availability promise", multi.Notice)
	}
	existing := Plan{Mode: ModeExisting, Owner: "owner"}.Commitment()
	if strings.Contains(existing.Notice, "单节点") || !strings.Contains(existing.Notice, "不承诺高可用") {
		t.Fatalf("notice = %q, want no node-count claim for an existing cluster", existing.Notice)
	}
}

// TestRenderKeepsClusterScopedWritesWithTheInstallPath pins C-04: the rendered
// cluster role grants the kernel objects and read-only cluster facts, while
// namespaced writes are roles in the namespaces the plan declares.
func TestRenderKeepsClusterScopedWritesWithTheInstallPath(t *testing.T) {
	plan := Plan{Mode: ModeExisting, Owner: "owner", Applications: []string{"shop"}}
	documents, err := Render(plan)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	clusterRole := documentByKind(t, documents, "ClusterRole")
	for _, raw := range nestedSlice(t, clusterRole, "rules") {
		rule := raw.(map[string]any)
		groups := stringsOf(rule["apiGroups"])
		verbs := stringsOf(rule["verbs"])
		if containsString(groups, api.Group) {
			continue
		}
		for _, verb := range verbs {
			if verb == "*" || verb == "create" || verb == "update" || verb == "patch" || verb == "delete" {
				t.Fatalf("the cluster role grants %q on %v; cluster-scoped writes belong to the install path", verb, groups)
			}
		}
	}
	roles := documentsOfKind(documents, "Role")
	if len(roles) != 2 {
		t.Fatalf("rendered %d roles, want one for the installation and one for the declared namespace", len(roles))
	}
	namespaces := make(map[string]bool)
	for _, role := range roles {
		namespaces[role.Object.GetNamespace()] = true
	}
	if !namespaces[DefaultNamespace] || !namespaces["shop"] {
		t.Fatalf("roles live in %v, want the installation and the declared namespace", namespaces)
	}
	bindings := documentsOfKind(documents, "RoleBinding")
	if len(bindings) != 2 {
		t.Fatalf("rendered %d role bindings, want one per granted namespace", len(bindings))
	}
	for _, binding := range bindings {
		roleKind, _, _ := unstructured.NestedString(binding.Object.Object, "roleRef", "kind")
		roleName, _, _ := unstructured.NestedString(binding.Object.Object, "roleRef", "name")
		if roleKind != "Role" || roleName != ControlPlaneName {
			t.Fatalf("binding %s/%s points at %s/%s, want Role/%s",
				binding.Object.GetNamespace(), binding.Object.GetName(), roleKind, roleName, ControlPlaneName)
		}
		subjects := nestedSlice(t, binding.Object, "subjects")
		if len(subjects) != 1 {
			t.Fatalf("binding %s/%s has %d subjects, want the control-plane identity only",
				binding.Object.GetNamespace(), binding.Object.GetName(), len(subjects))
		}
		subject, ok := subjects[0].(map[string]any)
		if !ok {
			t.Fatalf("binding %s/%s carries no subject object", binding.Object.GetNamespace(), binding.Object.GetName())
		}
		if subject["kind"] != "ServiceAccount" || subject["name"] != ControlPlaneServiceAccount ||
			subject["namespace"] != DefaultNamespace {
			t.Fatalf("binding %s/%s binds %v, want ServiceAccount %s/%s",
				binding.Object.GetNamespace(), binding.Object.GetName(), subject, DefaultNamespace, ControlPlaneServiceAccount)
		}
	}
}

// documentByKind returns the one document of a kind.
func documentByKind(t *testing.T, documents []Document, kind string) *unstructured.Unstructured {
	t.Helper()
	found := documentsOfKind(documents, kind)
	if len(found) != 1 {
		t.Fatalf("rendered %d %s documents, want one", len(found), kind)
	}
	return found[0].Object
}

// documentsOfKind returns every document of a kind.
func documentsOfKind(documents []Document, kind string) []Document {
	found := make([]Document, 0)
	for _, document := range documents {
		if document.Object.GetKind() == kind {
			found = append(found, document)
		}
	}
	return found
}

// nestedSlice reads one slice field.
func nestedSlice(t *testing.T, object *unstructured.Unstructured, path ...string) []any {
	t.Helper()
	value, found, err := unstructured.NestedSlice(object.Object, path...)
	if err != nil || !found {
		t.Fatalf("read %v: found=%v err=%v", path, found, err)
	}
	return value
}

// stringsOf converts one unstructured string slice.
func stringsOf(value any) []string {
	raw, _ := value.([]any)
	converted := make([]string, 0, len(raw))
	for _, entry := range raw {
		if text, ok := entry.(string); ok {
			converted = append(converted, text)
		}
	}
	return converted
}

// containsString reports one element in a string slice.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestInjectionListStopsTwiceAtTheSameStep pins C-58: the same list fails at
// the same step, and a list naming no step or the same step twice is refused.
func TestInjectionListStopsTwiceAtTheSameStep(t *testing.T) {
	injections, err := ParseInjections("control-plane=1,definitions=2")
	if err != nil {
		t.Fatalf("parse injections: %v", err)
	}
	stop := func() string {
		injector, err := NewInjector(injections)
		if err != nil {
			t.Fatalf("build injector: %v", err)
		}
		for _, step := range Steps() {
			if err := injector.At(step); err != nil {
				var injected *InjectedFailure
				if !errors.As(err, &injected) {
					t.Fatalf("step %s failed with %v, want an injected failure", step, err)
				}
				return injected.Point
			}
		}
		return ""
	}
	first, second := stop(), stop()
	if first != StepControlPlane || second != first {
		t.Fatalf("stops = %q and %q, want %q twice", first, second, StepControlPlane)
	}
	for _, text := range []string{"nowhere=1", "control-plane=0", "control-plane=1,control-plane=2"} {
		if _, err := ParseInjections(text); err == nil {
			t.Fatalf("injections %q were accepted", text)
		}
	}
}
