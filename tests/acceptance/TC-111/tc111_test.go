// Package tc111 uninstalls two equivalent extensions, one declaring its data
// kept and one declaring it deleted: the kept instances stay and are listed in
// the residual manifest, the deleted ones disappear, and the kernel kind set
// and capability baseline stay unchanged (AC-54, TC-111).
package tc111

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	keepGroup = "keep.example.test"
	dropGroup = "drop.example.test"
	namespace = "data"
)

// requestTimeout bounds every direct cluster call of the case.
const requestTimeout = 30 * time.Second

// declaration builds one extension whose object class data follows keep.
func declaration(identifier, group, kind, plural string, keep bool) extension.Declaration {
	return extension.Declaration{
		Identifier: identifier,
		Version:    "1.0.0",
		APIGroup:   group,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: group, Kind: kind, Resource: plural,
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Capabilities: extension.CapabilityContributions{Items: []extension.Capability{{
				Name: identifier + "/data", Version: "v1",
			}}},
			Pages: []extension.Page{{
				ID: identifier, Title: identifier, Location: "panel",
				Items: []extension.PageItem{{
					ID: "data", Label: "Data",
					Source: extension.PageSourceObject, Reference: kind,
					Reason: "acceptance",
				}},
			}},
		},
		Retention: []extension.Retention{{Resource: extension.ClassObjectClasses, Keep: keep}},
	}
}

// TestTC111UninstallAppliesTheDeclaredDataFate covers both retention
// declarations.
func TestTC111UninstallAppliesTheDeclaredDataFate(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, keepGroup, "v1", "Widget", "widgets", true)
	cluster.InstallCRD(t, dropGroup, "v1", "Gadget", "gadgets", true)
	cluster.EnsureNamespace(t, namespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	artifacts := extension.MemoryArtifacts{}
	kindsBefore := append([]string(nil), api.Kinds()...)
	baselineBefore := extension.BaselineCapabilities()

	cluster.InstallPackage(t, "keep", declaration("demo.keep", keepGroup, "Widget", "widgets", true), artifacts)
	cluster.InstallPackage(t, "drop", declaration("demo.drop", dropGroup, "Gadget", "gadgets", false), artifacts)
	createObject(t, cluster, keepGroup, "widgets", "Widget", "kept-one")
	createObject(t, cluster, dropGroup, "gadgets", "Gadget", "dropped-one")

	keepUID := cluster.ComponentUID(t, "keep")
	dropUID := cluster.ComponentUID(t, "drop")
	// The registration exists while the components are installed.
	if spec := cluster.ProviderSpecOf(t, "keep"); len(spec.Capabilities) != 1 {
		t.Fatalf("keep registration = %v, want one capability", spec.Capabilities)
	}

	cluster.DeleteComponent(t, "keep")
	cluster.DeleteComponent(t, "drop")
	removed := cluster.ExtensionState(t, artifacts)

	// No contribution class survives the uninstall.
	registry := removed.Registry()
	if len(registry.ObjectClasses()) != 0 || len(registry.Pages()) != 0 || len(registry.ExtensionCapabilities()) != 0 {
		t.Fatalf("contributions survived: classes %v pages %v capabilities %v",
			registry.ObjectClasses(), registry.Pages(), registry.ExtensionCapabilities())
	}
	if got := registry.Capabilities(); !reflect.DeepEqual(got, baselineBefore) {
		t.Fatalf("capability catalogue = %v, want the baseline", got)
	}
	if !reflect.DeepEqual(api.Kinds(), kindsBefore) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}

	// The kept class instance stays; the deleted one is gone.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := cluster.ExtensionResource(keepGroup, "v1", "widgets", namespace).Get(ctx, "kept-one", metav1.GetOptions{}); err != nil {
		t.Fatalf("the kept instance is gone: %v", err)
	}
	if _, err := cluster.ExtensionResource(dropGroup, "v1", "gadgets", namespace).Get(ctx, "dropped-one", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the deleted instance still exists (err %v)", err)
	}

	// Only the retained data is listed.
	residuals := cluster.Residuals(t, keepUID)
	if len(residuals) != 1 || residuals[0].Disposition != extension.ResidualRetained ||
		residuals[0].Object.Kind != "Widget" || residuals[0].Object.Name != "kept-one" {
		t.Fatalf("keep residuals = %+v", residuals)
	}
	if dropped := cluster.Residuals(t, dropUID); len(dropped) != 0 {
		t.Fatalf("deleted data is listed as a residual: %+v", dropped)
	}

	// The capability registration is withdrawn from the Provider, which is a
	// kernel object and stays (C-29).
	if spec := cluster.ProviderSpecOf(t, "keep"); len(spec.Capabilities) != 0 || spec.ComponentRef != "" {
		t.Fatalf("keep registration = %+v, want it cleared", spec)
	}
}

// createObject stores one instance of a declared class.
func createObject(t *testing.T, cluster *harness.Cluster, group, plural, kind, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": group + "/v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name": name, "namespace": namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
	}}
	if _, err := cluster.ExtensionResource(group, "v1", plural, namespace).Create(ctx, object, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s %s: %v", kind, name, err)
	}
}

// TestTC111UninstallTraversesEveryDependencyEdge checks C-29's dependent walk:
// a component edge, an extension object reference, an application capability
// requirement and a Provider registration each hold the uninstall open and are
// named; removing them one at a time releases the component.
func TestTC111UninstallTraversesEveryDependencyEdge(t *testing.T) {
	const (
		baseGroup     = "base.example.test"
		referrerGroup = "referrer.example.test"
		definitionRef = "needs-base"
	)
	cluster := harness.Start(t)
	cluster.InstallCRD(t, baseGroup, "v1", "Widget", "widgets", true)
	cluster.InstallCRD(t, referrerGroup, "v1", "Gadget", "gadgets", true)
	cluster.EnsureNamespace(t, namespace)
	cluster.EnsureNamespace(t, api.SystemNamespace)
	artifacts := extension.MemoryArtifacts{}

	base := extension.Declaration{
		Identifier: "demo.base", Version: "1.0.0", APIGroup: baseGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: baseGroup, Kind: "Widget", Resource: "widgets",
				Versions: []string{"v1"}, Namespaced: true,
			}},
			Capabilities: extension.CapabilityContributions{Items: []extension.Capability{{
				Name: "demo.base/vm", Version: "v1", Scope: v1beta1.CapabilityScopeCluster,
			}}},
		},
		Retention: []extension.Retention{{Resource: extension.ClassObjectClasses, Keep: true}},
	}
	consumer := extension.Declaration{
		Identifier: "demo.consumer", Version: "1.0.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{{ID: "consumer-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser}},
		},
		Dependencies: extension.Dependencies{
			Components: []extension.ComponentRequirement{{Identifier: "demo.base"}},
		},
	}
	referrer := extension.Declaration{
		Identifier: "demo.referrer", Version: "1.0.0", APIGroup: referrerGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: referrerGroup, Kind: "Gadget", Resource: "gadgets",
				Versions: []string{"v1"}, Namespaced: true,
				References: []string{"spec.widgetRef"},
			}},
		},
	}
	cluster.InstallPackage(t, "base", base, artifacts)
	cluster.InstallPackage(t, "consumer", consumer, artifacts)
	cluster.InstallPackage(t, "referrer", referrer, artifacts)

	createObject(t, cluster, baseGroup, "widgets", "Widget", "widget-one")
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	gadget := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": referrerGroup + "/v1",
		"kind":       "Gadget",
		"metadata": map[string]any{
			"name": "gadget-one", "namespace": namespace,
			"labels": map[string]any{"managed-by": "acceptance"},
		},
		"spec": map[string]any{"widgetRef": map[string]any{
			"apiVersion": baseGroup + "/v1", "kind": "Widget", "name": "widget-one",
		}},
	}}
	if _, err := cluster.ExtensionResource(referrerGroup, "v1", "gadgets", namespace).Create(ctx, gadget, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create the referring object: %v", err)
	}

	cluster.InstallDefinition(t, harness.Definition(t, definitionRef, []v1beta1.CapabilityRequirement{{
		Name: "demo.base/vm", Version: ">=v1",
	}}))
	cluster.CreateApplication(t, "uses-base", definitionRef, harness.Namespace)

	// A second Provider registers the same capability: the registration is a
	// dependent edge too (C-29).
	provider := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Provider",
		"metadata": map[string]any{
			"name": "other-provider",
			"labels": map[string]any{
				"managed-by": write.ManagedByValue, "owner-uid": "other-uid",
			},
		},
		"spec": map[string]any{
			"componentRef": "other", "version": "1.0.0",
			"capabilities": []any{map[string]any{
				"name": "demo.base/vm", "version": "v1", "scope": string(v1beta1.CapabilityScopeCluster),
				"caseSet": "acceptance/case", "caseSetDigest": "acceptance-digest",
			}},
		},
	}}
	if _, err := cluster.Writer().Apply(ctx, write.SubjectComponentRegistry, write.Session{}, write.PartSpec, provider); err != nil {
		t.Fatalf("register the second provider: %v", err)
	}

	baseUID := cluster.ComponentUID(t, "base")
	cluster.DeleteComponent(t, "base")
	installation := cluster.ExtensionState(t, artifacts)
	result, found := installation.Component("base")
	if !found || result.Phase != extension.PhaseTerminating {
		t.Fatalf("base = %+v/%v, want the uninstall held", result, found)
	}
	// The held component keeps serving, so its registration stays.
	if spec := cluster.ProviderSpecOf(t, "base"); len(spec.Capabilities) != 1 {
		t.Fatalf("held registration = %+v, want the capability still listed", spec)
	}
	condition := findCondition(t, harness.Conditions(t, cluster.Get(t, "v1beta1", "SystemComponent", "base")), extension.ConditionDeprovisionBlocked)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("DeprovisionBlocked = %+v, want True", condition)
	}
	for _, dependent := range []string{"consumer", "gadget-one", "uses-base", "other-provider"} {
		if !strings.Contains(condition.Message, dependent) {
			t.Fatalf("blocked message %q does not list %q", condition.Message, dependent)
		}
	}

	// Removing the dependents one at a time keeps the uninstall blocked until
	// the last one is gone.
	cluster.DeleteComponent(t, "consumer")
	cluster.ExtensionState(t, artifacts)
	assertBlocked(t, cluster, "base")
	if err := cluster.ExtensionResource(referrerGroup, "v1", "gadgets", namespace).Delete(ctx, "gadget-one", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the referring object: %v", err)
	}
	cluster.ExtensionState(t, artifacts)
	assertBlocked(t, cluster, "base")
	if err := cluster.Direct(t, api.Group, "v1beta1", "Application", harness.Namespace).Delete(ctx, "uses-base", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the dependent application: %v", err)
	}
	cluster.ExtensionState(t, artifacts)
	assertBlocked(t, cluster, "base")
	if err := cluster.Direct(t, api.Group, "v1beta1", "Provider", "").Delete(ctx, "other-provider", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete the second provider: %v", err)
	}
	cluster.ExtensionState(t, artifacts)

	// With every edge gone the component uninstalls, keeps its declared data
	// and records it in the residual manifest (C-29).
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Get(ctx, "base", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("base still exists after the last dependent left (err %v)", err)
	}
	if _, err := cluster.ExtensionResource(baseGroup, "v1", "widgets", namespace).Get(ctx, "widget-one", metav1.GetOptions{}); err != nil {
		t.Fatalf("the retained object is gone: %v", err)
	}
	residuals := cluster.Residuals(t, baseUID)
	if len(residuals) != 1 || residuals[0].Object.Name != "widget-one" || residuals[0].Disposition != extension.ResidualRetained {
		t.Fatalf("residuals = %+v", residuals)
	}
}

// TestTC111UnreadableDeclarationBlocksUninstall checks C-29's other half: when
// the declaration cannot be read the gate must not guess, so the pass fails
// and the component stays until the artifact is readable again.
func TestTC111UnreadableDeclarationBlocksUninstall(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, keepGroup, "v1", "Widget", "widgets", true)
	artifacts := extension.MemoryArtifacts{}
	cluster.InstallPackage(t, "keep", declaration("demo.keep", keepGroup, "Widget", "widgets", true), artifacts)
	content := artifacts["test://keep"]
	if len(content) == 0 {
		t.Fatal("the package artifact is missing from the source")
	}
	cluster.DeleteComponent(t, "keep")
	delete(artifacts, "test://keep")

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	installer := cluster.ExtensionInstaller(artifacts)
	if _, err := installer.Install(ctx); err == nil {
		t.Fatal("an unreadable declaration released the uninstall")
	} else if !strings.Contains(err.Error(), "read package") {
		t.Fatalf("uninstall error = %v, want the unreadable package named", err)
	}
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Get(ctx, "keep", metav1.GetOptions{}); err != nil {
		t.Fatalf("the component left while its declaration was unreadable: %v", err)
	}

	artifacts["test://keep"] = content
	cluster.ExtensionState(t, artifacts)
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Get(ctx, "keep", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the component survived a readable uninstall (err %v)", err)
	}
}

// assertBlocked requires the component to still exist while a dependent holds
// its uninstall open.
func assertBlocked(t *testing.T, cluster *harness.Cluster, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Get(ctx, name, metav1.GetOptions{}); err != nil {
		t.Fatalf("component %s left while a dependent still exists: %v", name, err)
	}
}

// findCondition returns one condition or nil.
func findCondition(t *testing.T, conditions []metav1.Condition, conditionType string) *metav1.Condition {
	t.Helper()
	for index := range conditions {
		if conditions[index].Type == conditionType {
			return &conditions[index]
		}
	}
	return nil
}

// TestTC111MalformedDeclarationBlocksOnlyItsOwnUninstall checks the error split:
// a structurally broken declaration is permanent, so it holds only its own
// deletion while another component uninstalls in the same round and the install
// pass keeps running (C-29).
func TestTC111MalformedDeclarationBlocksOnlyItsOwnUninstall(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}
	declarationWithPreferences := func(identifier string) extension.Declaration {
		return extension.Declaration{
			Identifier: identifier, Version: "1.0.0",
			Contributions: extension.Contributions{
				Settings: []extension.Setting{{
					ID: identifier + "-endpoint", Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterUser,
				}},
				Preferences: []extension.PreferenceSchema{{ID: "view", Keys: []string{"layout"}}},
			},
		}
	}
	installation := cluster.InstallPackageWith(t, "healthy", declarationWithPreferences("demo.healthy"), artifacts, shell.Retention())
	shell.Use(installation)
	installation = cluster.InstallPackageWith(t, "broken", declarationWithPreferences("demo.broken"), artifacts, shell.Retention())
	shell.Use(installation)
	status, body := shell.Call(t, http.MethodPut, "/api/v1/preferences/demo.broken/layout",
		map[string]any{"value": "kept-while-held"}, token)
	if status != http.StatusNoContent {
		t.Fatalf("write the preference = %d %v", status, body)
	}
	readable := artifacts["test://broken"]
	if len(readable) == 0 {
		t.Fatal("the healthy package artifact is missing")
	}

	// Repoint the component at a package that parses but loads into no
	// declaration: identifier is empty.
	malformed := []byte(`{"version":"1.0.0"}`)
	artifacts["test://malformed"] = malformed
	cluster.RepointComponent(t, "broken", "test://malformed", extension.DigestOf(malformed))

	cluster.DeleteComponent(t, "healthy")
	cluster.DeleteComponent(t, "broken")
	installation = cluster.ExtensionStateWith(t, artifacts, shell.Retention())
	shell.Use(installation)

	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Get(ctx, "healthy", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the healthy component did not uninstall in the same round (err %v)", err)
	}
	if _, present := installation.Component("healthy"); present {
		t.Fatal("the released component stayed in the install set")
	}
	// The malformed component is a held item, so the pass keeps it live and
	// writes the held phase instead of treating it as released.
	broken, found := installation.Component("broken")
	if !found || broken.Phase != extension.PhaseTerminating {
		t.Fatalf("broken = %+v/%v, want it held as %s", broken, found, extension.PhaseTerminating)
	}
	stored := cluster.Get(t, "v1beta1", "SystemComponent", "broken")
	condition := findCondition(t, harness.Conditions(t, stored), extension.ConditionDeprovisionBlocked)
	if condition == nil || condition.Status != metav1.ConditionTrue ||
		condition.Reason != extension.ReasonDeclarationUnreadable {
		t.Fatalf("broken declaration condition = %+v", condition)
	}
	// The object still exists, so its preference domain must not be withdrawn.
	preferences, err := shell.Accounts.Store.PreferencesOf(ctx, "owner", "demo.broken")
	if err != nil {
		t.Fatalf("read the stored preference: %v", err)
	}
	if preferences["layout"] != "kept-while-held" {
		t.Fatalf("preferences = %v, want the domain kept while the uninstall is held", preferences)
	}

	// A readable package releases the held deletion.
	cluster.RepointComponent(t, "broken", "test://broken", extension.DigestOf(readable))
	cluster.ExtensionStateWith(t, artifacts, shell.Retention())
	if _, err := cluster.Direct(t, api.Group, "v1beta1", "SystemComponent", "").Get(ctx, "broken", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the repaired component did not uninstall (err %v)", err)
	}
}
