// Package tc104 verifies that one object read through both served versions
// returns the same field set (AC-52, TC-104).
package tc104

import (
	"sort"
	"testing"

	"github.com/tsic404/kos/tests/acceptance/harness"
)

// TestTC104DualVersionFieldParity creates one object per kernel kind, reads it
// through v1alpha1 and v1beta1, and compares every field.
func TestTC104DualVersionFieldParity(t *testing.T) {
	cluster := harness.Start(t)
	for _, object := range harness.KernelObjects(t) {
		kind := object.GetKind()
		t.Run(kind, func(t *testing.T) {
			created := cluster.Create(t, object)
			name := created.GetName()

			alpha := cluster.Get(t, "v1alpha1", kind, name)
			beta := cluster.Get(t, "v1beta1", kind, name)

			if alpha.GetUID() != beta.GetUID() {
				t.Fatalf("uid %q (v1alpha1) != %q (v1beta1)", alpha.GetUID(), beta.GetUID())
			}
			if alpha.GetResourceVersion() != beta.GetResourceVersion() {
				t.Fatalf("resourceVersion %q != %q; reads must observe one object",
					alpha.GetResourceVersion(), beta.GetResourceVersion())
			}

			compareFields(t, harness.Flatten(alpha.Object), harness.Flatten(beta.Object))
		})
	}
}

// compareFields reports every field that exists in one read and not the other,
// or whose value or type differs.
func compareFields(t *testing.T, alpha, beta map[string]string) {
	t.Helper()
	var problems []string
	for path, alphaValue := range alpha {
		betaValue, ok := beta[path]
		if !ok {
			problems = append(problems, "missing in v1beta1: "+path)
			continue
		}
		if alphaValue != betaValue {
			problems = append(problems, path+": "+alphaValue+" vs "+betaValue)
		}
	}
	for path := range beta {
		if _, ok := alpha[path]; !ok {
			problems = append(problems, "missing in v1alpha1: "+path)
		}
	}
	if len(problems) == 0 {
		return
	}
	sort.Strings(problems)
	for _, problem := range problems {
		t.Error(problem)
	}
}
