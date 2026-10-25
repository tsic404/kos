// Package abi verifies the ABI rules of the KOS API group: a closed ten-kind
// object model, adjacent versions that share one structure, and no webhook on
// the correctness path (C-01, C-16).
package abi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/tsic404/kos/api"
)

// Versions are the two versions served at the same time; the newer one is the
// storage version (C-16).
var Versions = []string{"v1alpha1", "v1beta1"}

// StorageVersion is the version the API server stores.
const StorageVersion = "v1beta1"

// CheckCRDs verifies every CRD manifest in dir against the ABI rules.
//
// It checks the closed kind set, scope, served and storage versions, conversion
// strategy, status presence and the per-version schema equality required by
// C-01, C-07 and C-16. All violations are reported together.
func CheckCRDs(dir string) error {
	crds, problems := loadCRDs(dir)
	want := api.Kinds()

	for _, kind := range want {
		if _, ok := crds[kind]; !ok {
			problems = append(problems, fmt.Sprintf("no CRD for kernel kind %q", kind))
		}
	}
	for kind := range crds {
		if _, ok := api.ScopeOf(kind); !ok {
			problems = append(problems, fmt.Sprintf("CRD %q is outside the closed kernel kind set", kind))
		}
	}
	for kind, crd := range crds {
		problems = append(problems, checkCRD(kind, crd)...)
	}
	return joinProblems(problems)
}

// CheckTypeParity reports where two version types disagree on field name, JSON
// name, type or optionality. Types that are identical are compared by identity,
// so shared k8s types are not walked.
func CheckTypeParity(alpha, beta reflect.Type) error {
	var problems []string
	compareTypes(alpha.Name(), alpha, beta, &problems)
	return joinProblems(problems)
}

// CheckNoWebhookManifests reports blocking webhook manifests under dirs: a
// mutating or conversion webhook may not sit on the write path, and a
// validating webhook must ignore failures (C-16, ADR-012).
func CheckNoWebhookManifests(dirs ...string) error {
	var problems []string
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !isYAML(entry.Name()) {
				return nil
			}
			found, err := inspectManifest(path)
			if err != nil {
				return err
			}
			problems = append(problems, found...)
			return nil
		})
		if err != nil {
			return fmt.Errorf("scan %s: %w", dir, err)
		}
	}
	return joinProblems(problems)
}

// loadCRDs decodes every CRD manifest in dir, keyed by kind.
func loadCRDs(dir string) (map[string]*apiextensionsv1.CustomResourceDefinition, []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []string{fmt.Sprintf("read CRD directory %s: %v", dir, err)}
	}
	crds := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(entries))
	var problems []string
	for _, entry := range entries {
		if entry.IsDir() || !isYAML(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.Unmarshal(raw, crd); err != nil {
			problems = append(problems, fmt.Sprintf("%s: decode CRD: %v", path, err))
			continue
		}
		if crd.Spec.Names.Kind == "" {
			problems = append(problems, fmt.Sprintf("%s: manifest carries no CRD kind", path))
			continue
		}
		crds[crd.Spec.Names.Kind] = crd
	}
	return crds, problems
}

// checkCRD reports the ABI violations of a single CRD.
func checkCRD(kind string, crd *apiextensionsv1.CustomResourceDefinition) []string {
	var problems []string
	if crd.Spec.Group != api.Group {
		problems = append(problems, fmt.Sprintf("%s: group %q is not the KOS group %q (C-19)", kind, crd.Spec.Group, api.Group))
	}
	if want, ok := api.ScopeOf(kind); ok && string(want) != string(crd.Spec.Scope) {
		problems = append(problems, fmt.Sprintf("%s: scope %q, want %q", kind, crd.Spec.Scope, want))
	}
	problems = append(problems, checkVersions(kind, crd)...)
	problems = append(problems, checkConversion(kind, crd)...)
	problems = append(problems, checkStatus(kind, crd)...)
	problems = append(problems, checkSchemaParity(kind, crd)...)
	return problems
}

// checkVersions verifies the two served versions and the single storage version.
func checkVersions(kind string, crd *apiextensionsv1.CustomResourceDefinition) []string {
	var problems []string
	served := make(map[string]bool, len(crd.Spec.Versions))
	var storage []string
	for _, version := range crd.Spec.Versions {
		served[version.Name] = version.Served
		if version.Storage {
			storage = append(storage, version.Name)
		}
	}
	for _, want := range Versions {
		isServed, ok := served[want]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: version %s is not defined", kind, want))
			continue
		}
		if !isServed {
			problems = append(problems, fmt.Sprintf("%s: version %s is not served", kind, want))
		}
	}
	if len(served) != len(Versions) {
		problems = append(problems, fmt.Sprintf("%s: %d versions defined, want %d", kind, len(served), len(Versions)))
	}
	if len(storage) != 1 || storage[0] != StorageVersion {
		problems = append(problems, fmt.Sprintf("%s: storage versions %v, want [%s]", kind, storage, StorageVersion))
	}
	return problems
}

// checkConversion verifies the conversion strategy is None and carries no webhook.
func checkConversion(kind string, crd *apiextensionsv1.CustomResourceDefinition) []string {
	conversion := crd.Spec.Conversion
	if conversion == nil {
		return nil
	}
	var problems []string
	if conversion.Strategy != apiextensionsv1.NoneConverter {
		problems = append(problems, fmt.Sprintf("%s: conversion strategy %q, want None", kind, conversion.Strategy))
	}
	if conversion.Webhook != nil {
		problems = append(problems, fmt.Sprintf("%s: conversion webhook configured", kind))
	}
	return problems
}

// checkStatus verifies the status subresource and field exist exactly for the
// kinds that have a status writer (C-07).
func checkStatus(kind string, crd *apiextensionsv1.CustomResourceDefinition) []string {
	wantStatus, _ := api.HasStatus(kind)
	var problems []string
	for _, version := range crd.Spec.Versions {
		hasSubresource := version.Subresources != nil && version.Subresources.Status != nil
		if hasSubresource != wantStatus {
			problems = append(problems, fmt.Sprintf("%s %s: status subresource present=%t, want %t", kind, version.Name, hasSubresource, wantStatus))
		}
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			problems = append(problems, fmt.Sprintf("%s %s: no structural schema", kind, version.Name))
			continue
		}
		_, hasField := version.Schema.OpenAPIV3Schema.Properties["status"]
		if hasField != wantStatus {
			problems = append(problems, fmt.Sprintf("%s %s: status field present=%t, want %t", kind, version.Name, hasField, wantStatus))
		}
	}
	return problems
}

// checkSchemaParity verifies the two version schemas differ in nothing but
// descriptions, which is what "adjacent versions share one structure" means.
func checkSchemaParity(kind string, crd *apiextensionsv1.CustomResourceDefinition) []string {
	schemas := make(map[string]any, len(crd.Spec.Versions))
	for _, version := range crd.Spec.Versions {
		if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
			continue
		}
		raw, err := json.Marshal(version.Schema.OpenAPIV3Schema)
		if err != nil {
			return []string{fmt.Sprintf("%s %s: marshal schema: %v", kind, version.Name, err)}
		}
		var tree any
		if err := json.Unmarshal(raw, &tree); err != nil {
			return []string{fmt.Sprintf("%s %s: unmarshal schema: %v", kind, version.Name, err)}
		}
		schemas[version.Name] = withoutDescriptions(tree)
	}
	if len(schemas) != len(Versions) {
		return []string{fmt.Sprintf("%s: %d schemas, want %d", kind, len(schemas), len(Versions))}
	}
	if !reflect.DeepEqual(schemas[Versions[0]], schemas[Versions[1]]) {
		return []string{fmt.Sprintf("%s: %s and %s schemas are not structurally identical", kind, Versions[0], Versions[1])}
	}
	return nil
}

// withoutDescriptions drops documentation-only keys so two generated schemas
// can be compared on structure alone.
func withoutDescriptions(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, value := range typed {
			if key == "description" {
				continue
			}
			cleaned[key] = withoutDescriptions(value)
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(typed))
		for index, value := range typed {
			cleaned[index] = withoutDescriptions(value)
		}
		return cleaned
	default:
		return node
	}
}

// inspectManifest reports blocking webhook configuration in one manifest file.
func inspectManifest(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(raw)), 4096)
	var problems []string
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		if len(document) == 0 {
			continue
		}
		problems = append(problems, inspectDocument(path, document)...)
	}
	return problems, nil
}

// inspectDocument reports webhook use that can block a write.
func inspectDocument(path string, document map[string]any) []string {
	kind, _ := document["kind"].(string)
	switch kind {
	case "MutatingWebhookConfiguration":
		return []string{fmt.Sprintf("%s: %s puts a mutating webhook on the write path", path, kind)}
	case "ValidatingWebhookConfiguration":
		var problems []string
		for index, webhook := range sliceOfMaps(document, "webhooks") {
			if policy, _ := webhook["failurePolicy"].(string); policy != "Ignore" {
				problems = append(problems, fmt.Sprintf("%s: %s webhook %d must ignore failures, failurePolicy=%q", path, kind, index, policy))
			}
		}
		return problems
	case "CustomResourceDefinition":
		spec, _ := document["spec"].(map[string]any)
		conversion, _ := spec["conversion"].(map[string]any)
		if _, ok := conversion["webhook"]; ok {
			return []string{fmt.Sprintf("%s: CRD %v configures a conversion webhook", path, nestedString(spec, "names", "kind"))}
		}
	}
	return nil
}

// sliceOfMaps reads a list of objects from an unstructured document.
func sliceOfMaps(document map[string]any, field string) []map[string]any {
	raw, _ := document[field].([]any)
	objects := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if object, ok := item.(map[string]any); ok {
			objects = append(objects, object)
		}
	}
	return objects
}

// nestedString reads a string nested under the given keys.
func nestedString(node map[string]any, keys ...string) string {
	var current any = node
	for _, key := range keys {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[key]
	}
	value, _ := current.(string)
	return value
}

// compareTypes reports every difference between two version types.
func compareTypes(path string, alpha, beta reflect.Type, problems *[]string) {
	if alpha == beta {
		return
	}
	if alpha.Kind() != beta.Kind() {
		*problems = append(*problems, fmt.Sprintf("%s: kind %s vs %s", path, alpha.Kind(), beta.Kind()))
		return
	}
	switch alpha.Kind() {
	case reflect.Pointer:
		compareTypes(path, alpha.Elem(), beta.Elem(), problems)
	case reflect.Slice, reflect.Array:
		compareTypes(path+"[]", alpha.Elem(), beta.Elem(), problems)
	case reflect.Map:
		compareTypes(path+"{}", alpha.Elem(), beta.Elem(), problems)
	case reflect.Struct:
		compareStructs(path, alpha, beta, problems)
	case reflect.Interface:
		if alpha.NumMethod() != beta.NumMethod() {
			*problems = append(*problems, fmt.Sprintf("%s: interface %s vs %s", path, alpha, beta))
		}
	default:
		if alpha.Name() != beta.Name() {
			*problems = append(*problems, fmt.Sprintf("%s: %s vs %s", path, alpha, beta))
		}
	}
}

// compareStructs reports fields that differ by name, JSON name or optionality.
func compareStructs(path string, alpha, beta reflect.Type, problems *[]string) {
	alphaFields := exportedFields(alpha)
	betaFields := exportedFields(beta)
	for name, alphaField := range alphaFields {
		betaField, ok := betaFields[name]
		if !ok {
			*problems = append(*problems, fmt.Sprintf("%s.%s: missing from %s", path, name, beta))
			continue
		}
		alphaName, alphaOptional := jsonShape(alphaField)
		betaName, betaOptional := jsonShape(betaField)
		if alphaName != betaName {
			*problems = append(*problems, fmt.Sprintf("%s.%s: JSON name %q vs %q", path, name, alphaName, betaName))
		}
		if alphaOptional != betaOptional {
			*problems = append(*problems, fmt.Sprintf("%s.%s: optional %t vs %t", path, name, alphaOptional, betaOptional))
		}
		fieldPath := path + "." + alphaName
		if alphaName == "" {
			fieldPath = path
		}
		compareTypes(fieldPath, alphaField.Type, betaField.Type, problems)
	}
	for name := range betaFields {
		if _, ok := alphaFields[name]; !ok {
			*problems = append(*problems, fmt.Sprintf("%s.%s: missing from %s", path, name, alpha))
		}
	}
}

// exportedFields indexes a struct's exported fields by Go field name.
func exportedFields(structType reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField, structType.NumField())
	for index := range structType.NumField() {
		field := structType.Field(index)
		if field.PkgPath != "" {
			continue
		}
		fields[field.Name] = field
	}
	return fields
}

// jsonShape returns the JSON name and the optionality declared by the tag.
func jsonShape(field reflect.StructField) (name string, optional bool) {
	tag := field.Tag.Get("json")
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, option := range parts[1:] {
		if option == "omitempty" {
			optional = true
		}
	}
	if name == "" && len(parts) == 1 && tag == "" {
		name = field.Name
	}
	return name, optional
}

// isYAML reports whether the file may carry a Kubernetes manifest.
func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// joinProblems renders violations in stable order.
func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	sort.Strings(problems)
	return fmt.Errorf("ABI violations:\n%s", strings.Join(problems, "\n"))
}
