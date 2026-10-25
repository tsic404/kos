package api_test

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/abi"
)

func TestBothVersionsShareOneStructure(t *testing.T) {
	scheme := api.Scheme()
	for _, kind := range api.Kinds() {
		t.Run(kind, func(t *testing.T) {
			alpha := zeroType(t, scheme, "v1alpha1", kind)
			beta := zeroType(t, scheme, "v1beta1", kind)
			if err := abi.CheckTypeParity(alpha, beta); err != nil {
				t.Fatalf("C-16 violation: %v", err)
			}
		})
	}
}

// zeroType resolves a registered kind to its Go struct type.
func zeroType(t *testing.T, scheme *runtime.Scheme, version, kind string) reflect.Type {
	t.Helper()
	object, err := scheme.New(schema.GroupVersionKind{Group: api.Group, Version: version, Kind: kind})
	if err != nil {
		t.Fatalf("resolve %s %s: %v", version, kind, err)
	}
	return reflect.TypeOf(object).Elem()
}
