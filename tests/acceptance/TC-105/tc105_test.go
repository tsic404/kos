// Package tc105 verifies that the object definitions carry no conversion
// webhook and that the cluster holds no admission webhook on the write path
// (AC-52, TC-105).
package tc105

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/abi"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// requestTimeout bounds the admission listings.
const requestTimeout = 30 * time.Second

// TestTC105NoConversionWebhook checks the object definitions and the cluster
// the objects are written to.
func TestTC105NoConversionWebhook(t *testing.T) {
	root := harness.RepoRoot(t)

	t.Run("repository manifests declare no blocking webhook", func(t *testing.T) {
		if err := abi.CheckCRDs(filepath.Join(root, "config", "crd")); err != nil {
			t.Fatalf("CRD manifests violate the ABI rules: %v", err)
		}
		if err := abi.CheckNoWebhookManifests(filepath.Join(root, "config")); err != nil {
			t.Fatalf("a manifest puts a webhook on the write path: %v", err)
		}
	})

	cluster := harness.Start(t)
	checkConversion(t, cluster)

	t.Run("no blocking admission webhook exists in the cluster", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		defer cancel()

		mutating, err := cluster.Kube.AdmissionregistrationV1().MutatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list mutating webhook configurations: %v", err)
		}
		if len(mutating.Items) != 0 {
			names := make([]string, 0, len(mutating.Items))
			for _, item := range mutating.Items {
				names = append(names, item.Name)
			}
			sort.Strings(names)
			t.Errorf("mutating webhook configurations present: %v", names)
		}

		validating, err := cluster.Kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list validating webhook configurations: %v", err)
		}
		for _, configuration := range validating.Items {
			for _, webhook := range configuration.Webhooks {
				if webhook.FailurePolicy == nil || *webhook.FailurePolicy != admissionregistrationv1.Ignore {
					t.Errorf("validating webhook %q of %s cannot ignore failures", webhook.Name, configuration.Name)
				}
			}
		}
	})

	t.Run("write path changes no submitted field", func(t *testing.T) {
		for _, object := range harness.KernelObjects(t) {
			created := cluster.Create(t, object)
			stored := cluster.Get(t, "v1beta1", object.GetKind(), created.GetName())
			compareUserFields(t, object, stored)
		}
	})
}

// checkConversion verifies every kernel CRD is served with conversion None and
// no conversion webhook.
func checkConversion(t *testing.T, cluster *harness.Cluster) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	list, err := cluster.APIExtensions.ApiextensionsV1().CustomResourceDefinitions().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list CRDs: %v", err)
	}
	seen := make(map[string]bool, len(list.Items))
	for _, crd := range list.Items {
		if crd.Spec.Group != api.Group {
			t.Errorf("CRD %s serves group %q, want %q", crd.Name, crd.Spec.Group, api.Group)
			continue
		}
		seen[crd.Spec.Names.Kind] = true
		conversion := crd.Spec.Conversion
		if conversion == nil {
			continue
		}
		if conversion.Strategy != apiextensionsv1.NoneConverter {
			t.Errorf("CRD %s converts with strategy %q, want None", crd.Name, conversion.Strategy)
		}
		if conversion.Webhook != nil {
			t.Errorf("CRD %s carries a conversion webhook", crd.Name)
		}
	}
	for _, kind := range api.Kinds() {
		if !seen[kind] {
			t.Errorf("no CRD serves kernel kind %q", kind)
		}
	}
}

// compareUserFields checks the server stored exactly the submitted labels,
// annotations and spec, so no admission step rewrote them.
func compareUserFields(t *testing.T, submitted, stored *unstructured.Unstructured) {
	t.Helper()
	fields := []struct {
		path      string
		submitted any
		stored    any
	}{
		{"metadata.labels", submitted.GetLabels(), stored.GetLabels()},
		{"metadata.annotations", submitted.GetAnnotations(), stored.GetAnnotations()},
		{"spec", submitted.Object["spec"], stored.Object["spec"]},
		{"status", submitted.Object["status"], stored.Object["status"]},
	}
	for _, field := range fields {
		if !reflect.DeepEqual(field.submitted, field.stored) {
			t.Errorf("%s: stored %v, submitted %v", field.path, field.stored, field.submitted)
		}
	}
}
