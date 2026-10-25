package abi_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/tsic404/kos/pkg/abi"
)

const crdDir = "../../config/crd"

func TestGeneratedCRDsSatisfyABI(t *testing.T) {
	if err := abi.CheckCRDs(crdDir); err != nil {
		t.Fatalf("generated CRDs violate the ABI rules: %v", err)
	}
}

func TestNoWebhookManifestsInRepository(t *testing.T) {
	if err := abi.CheckNoWebhookManifests("../../config"); err != nil {
		t.Fatalf("repository declares a blocking webhook: %v", err)
	}
}

func TestCheckCRDsRejectsConversionWebhook(t *testing.T) {
	dir := copyCRDs(t)
	mutateCRD(t, filepath.Join(dir, "kos.tsic.top_applications.yaml"), func(crd map[string]any) {
		spec := crd["spec"].(map[string]any)
		spec["conversion"] = map[string]any{
			"strategy": "Webhook",
			"webhook":  map[string]any{"conversionReviewVersions": []any{"v1"}},
		}
	})
	err := abi.CheckCRDs(dir)
	if err == nil || !strings.Contains(err.Error(), "conversion") {
		t.Fatalf("CheckCRDs() = %v, want a conversion violation", err)
	}
}

func TestCheckCRDsRejectsKindOutsideTheClosedSet(t *testing.T) {
	dir := copyCRDs(t)
	manifest := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.kos.tsic.top
spec:
  group: kos.tsic.top
  names:
    kind: Widget
    plural: widgets
    singular: widget
  scope: Namespaced
  versions:
    - name: v1alpha1
      served: true
      storage: false
      schema:
        openAPIV3Schema:
          type: object
    - name: v1beta1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`
	if err := os.WriteFile(filepath.Join(dir, "kos.tsic.top_widgets.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	err := abi.CheckCRDs(dir)
	if err == nil || !strings.Contains(err.Error(), `CRD "Widget" is outside the closed kernel kind set`) {
		t.Fatalf("CheckCRDs() = %v, want an eleventh kind to be rejected (C-01)", err)
	}
}

func TestCheckCRDsRejectsKernelKindInAnotherGroup(t *testing.T) {
	dir := copyCRDs(t)
	mutateCRD(t, filepath.Join(dir, "kos.tsic.top_applications.yaml"), func(crd map[string]any) {
		crd["spec"].(map[string]any)["group"] = "apps.example.com"
	})
	err := abi.CheckCRDs(dir)
	if err == nil || !strings.Contains(err.Error(), "is not the KOS group") {
		t.Fatalf("CheckCRDs() = %v, want a group violation (C-19)", err)
	}
}

func TestCheckCRDsRejectsSchemaDrift(t *testing.T) {
	dir := copyCRDs(t)
	mutateCRD(t, filepath.Join(dir, "kos.tsic.top_volumes.yaml"), func(crd map[string]any) {
		versions := crd["spec"].(map[string]any)["versions"].([]any)
		beta := versions[1].(map[string]any)
		schema := beta["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		properties["drifted"] = map[string]any{"type": "string"}
	})
	err := abi.CheckCRDs(dir)
	if err == nil || !strings.Contains(err.Error(), "not structurally identical") {
		t.Fatalf("CheckCRDs() = %v, want a schema parity violation", err)
	}
}

func TestCheckNoWebhookManifestsRejectsMutatingWebhook(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: kos-defaults
webhooks:
  - name: defaults.kos.tsic.top
    failurePolicy: Fail
`
	if err := os.WriteFile(filepath.Join(dir, "webhook.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := abi.CheckNoWebhookManifests(dir); err == nil {
		t.Fatal("CheckNoWebhookManifests() = nil, want a mutating webhook violation")
	}
}

func TestCheckNoWebhookManifestsAllowsFailureIgnoringValidation(t *testing.T) {
	dir := t.TempDir()
	manifest := `apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: kos-validation
webhooks:
  - name: validation.kos.tsic.top
    failurePolicy: Ignore
`
	if err := os.WriteFile(filepath.Join(dir, "webhook.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := abi.CheckNoWebhookManifests(dir); err != nil {
		t.Fatalf("CheckNoWebhookManifests() = %v, want nil", err)
	}
}

// copyCRDs copies the generated manifests into a scratch directory.
func copyCRDs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	entries, err := os.ReadDir(crdDir)
	if err != nil {
		t.Fatalf("read %s: %v", crdDir, err)
	}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(crdDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dir, entry.Name()), raw, 0o600); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
	return dir
}

// mutateCRD applies a YAML-level change to one manifest.
func mutateCRD(t *testing.T, path string, mutate func(map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var crd map[string]any
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	mutate(crd)
	patched, err := yaml.Marshal(crd)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, patched, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
