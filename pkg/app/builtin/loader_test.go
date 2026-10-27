package builtin

import (
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
)

// pinnedYAML builds one shipped definition with its correct pin.
func pinnedYAML(t *testing.T) []byte {
	t.Helper()
	spec := v1beta1.ApplicationDefinitionSpec{
		Package:  v1beta1.PackageIdentity{Version: "1.0.0"},
		Template: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Name }}\n  namespace: {{ .Namespace }}\n",
	}
	digest, err := app.DefinitionDigest(spec)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	spec.Package.Digest = digest
	definition := &v1beta1.ApplicationDefinition{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kos.tsic.top/v1beta1", Kind: "ApplicationDefinition"},
		ObjectMeta: metav1.ObjectMeta{Name: "one"},
		Spec:       spec,
	}
	raw, err := yaml.Marshal(definition)
	if err != nil {
		t.Fatalf("encode the definition: %v", err)
	}
	return raw
}

// flipDigest changes the last hex digit of the pinned digest so the format is
// still valid and only the content comparison can reject it.
func flipDigest(t *testing.T, raw []byte) []byte {
	t.Helper()
	pattern := regexp.MustCompile(`sha256:([0-9a-f]{64})`)
	match := pattern.FindSubmatchIndex(raw)
	if match == nil {
		t.Fatal("the fixture carries no package digest")
	}
	digest := string(raw[match[2]:match[3]])
	last := digest[len(digest)-1]
	replacement := byte('0')
	if last == '0' {
		replacement = '1'
	}
	flipped := digest[:len(digest)-1] + string(replacement)
	return []byte(strings.Replace(string(raw), digest, flipped, 1))
}

// TestLoadDefinitionsRequiresAMatchingPin covers C-32's pinned digest: a
// definition whose content no longer matches its pin must not load.
func TestLoadDefinitionsRequiresAMatchingPin(t *testing.T) {
	raw := pinnedYAML(t)
	fsys := fstest.MapFS{"definitions/one.yaml": {Data: raw}}
	if _, err := loadDefinitions(fsys); err != nil {
		t.Fatalf("a matching pin was refused: %v", err)
	}

	tampered := flipDigest(t, raw)
	fsys = fstest.MapFS{"definitions/one.yaml": {Data: tampered}}
	if _, err := loadDefinitions(fsys); err == nil {
		t.Fatal("a definition whose pin does not match its content loaded")
	}
}

// TestLoadDefinitionsRejectsAnUnknownField covers the strict decode: a
// misspelled field must fail at load time instead of being dropped before the
// digest is computed.
func TestLoadDefinitionsRejectsAnUnknownField(t *testing.T) {
	raw := pinnedYAML(t)
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the fixture: %v", err)
	}
	spec, _ := document["spec"].(map[string]any)
	spec["templates"] = "apiVersion: v1"
	tampered, err := yaml.Marshal(document)
	if err != nil {
		t.Fatalf("encode the tampered definition: %v", err)
	}
	fsys := fstest.MapFS{"definitions/one.yaml": {Data: tampered}}
	if _, err := loadDefinitions(fsys); err == nil || !strings.Contains(err.Error(), "templates") {
		t.Fatalf("error = %v, want the unknown field named", err)
	}
}
