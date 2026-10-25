package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/tsic404/kos/api"
)

// TestResourceNamesMatchTheGeneratedCRDs keeps the resource names the client uses
// from drifting away from the manifests the API server installs.
func TestResourceNamesMatchTheGeneratedCRDs(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "config", "crd"))
	if err != nil {
		t.Fatalf("read the CRD directory: %v", err)
	}
	declared := map[string]string{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		content, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		var manifest struct {
			Spec struct {
				Names struct {
					Kind   string `json:"kind"`
					Plural string `json:"plural"`
				} `json:"names"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(content, &manifest); err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		declared[manifest.Spec.Names.Kind] = manifest.Spec.Names.Plural
	}

	for _, kind := range api.Kinds() {
		plural, present := declared[kind]
		if !present {
			t.Errorf("no CRD declares kind %s", kind)
			continue
		}
		if resourceNames[kind] != plural {
			t.Errorf("%s resolves to resource %q, the CRD declares %q", kind, resourceNames[kind], plural)
		}
	}
	if len(declared) != len(resourceNames) {
		t.Errorf("the CRDs declare %d kinds, the control plane serves %d", len(declared), len(resourceNames))
	}
}
