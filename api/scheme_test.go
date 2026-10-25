package api_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
)

// kernelPackage is the import prefix of the types that make up the object model.
const kernelPackage = "github.com/tsic404/kos/api/"

// contractKinds is the ten kinds C-01 names, spelled out. The expectation is the
// contract rather than api.Kinds(), so dropping a kind cannot shrink it too.
var contractKinds = []string{
	"Application",
	"ApplicationDefinition",
	"BackupPolicy",
	"Endpoint",
	"NodeDevice",
	"Provider",
	"RestoreJob",
	"SystemComponent",
	"Volume",
	"VolumeMigration",
}

// contractClusterScoped is the four cluster-scoped kinds C-01 names; every other
// kernel kind is namespaced.
var contractClusterScoped = []string{
	"ApplicationDefinition",
	"NodeDevice",
	"Provider",
	"SystemComponent",
}

func TestKindsMatchContract(t *testing.T) {
	if got := api.Kinds(); !reflect.DeepEqual(got, contractKinds) {
		t.Fatalf("api.Kinds() = %v, want %v (C-01)", got, contractKinds)
	}
}

func TestScopesMatchContract(t *testing.T) {
	cluster := make(map[string]bool, len(contractClusterScoped))
	for _, kind := range contractClusterScoped {
		cluster[kind] = true
	}
	for _, kind := range contractKinds {
		scope, ok := api.ScopeOf(kind)
		if !ok {
			t.Fatalf("ScopeOf(%q) reported no scope", kind)
		}
		want := api.ScopeNamespaced
		if cluster[kind] {
			want = api.ScopeCluster
		}
		if scope != want {
			t.Errorf("ScopeOf(%q) = %q, want %q", kind, scope, want)
		}
	}
}

func TestSchemeEnumeratesExactlyTheKernelKinds(t *testing.T) {
	scheme := api.Scheme()

	kinds := make(map[string]bool, len(contractKinds))
	for gvk := range scheme.AllKnownTypes() {
		if gvk.Group != api.Group || strings.HasSuffix(gvk.Kind, "List") {
			continue
		}
		object, err := scheme.New(gvk)
		if err != nil {
			t.Fatalf("scheme lists %s but cannot create it: %v", gvk, err)
		}
		// The builder also binds option and list types to the group version;
		// only the kernel packages define the object model.
		if !strings.HasPrefix(reflect.TypeOf(object).Elem().PkgPath(), kernelPackage) {
			continue
		}
		kinds[gvk.Kind] = true
	}
	got := make([]string, 0, len(kinds))
	for kind := range kinds {
		got = append(got, kind)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, contractKinds) {
		t.Fatalf("scheme kinds = %v, want %v", got, contractKinds)
	}

	for _, version := range []string{"v1alpha1", "v1beta1"} {
		for _, kind := range contractKinds {
			gvk := schema.GroupVersionKind{Group: api.Group, Version: version, Kind: kind}
			if !scheme.Recognizes(gvk) {
				t.Errorf("scheme does not recognize %s", gvk)
			}
		}
	}
}
