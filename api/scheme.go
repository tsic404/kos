package api

import (
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tsic404/kos/api/v1alpha1"
	"github.com/tsic404/kos/api/v1beta1"
)

// AddToScheme registers both served versions of the KOS group into s.
func AddToScheme(s *runtime.Scheme) error {
	if err := v1alpha1.AddToScheme(s); err != nil {
		return err
	}
	return v1beta1.AddToScheme(s)
}

// Scheme returns a runtime scheme holding both served versions.
func Scheme() *runtime.Scheme {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		panic(err)
	}
	return s
}
