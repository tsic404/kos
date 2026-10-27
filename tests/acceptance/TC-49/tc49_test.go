// Package tc49 installs packages that carry a storage class or their own
// volume template and requires the install to be refused before anything is
// written: the volume object owns the claim and its policy (AC-24, TC-49).
package tc49

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/state"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

// claimTemplatePackage renders a StatefulSet that brings its own volume claim
// template, which F4 forbids (C-32).
const claimTemplatePackage = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  serviceName: {{ .Name }}
  replicas: 1
  selector:
    matchLabels:
      app: {{ .Name }}
  template:
    metadata:
      labels:
        app: {{ .Name }}
    spec:
      containers:
        - name: app
          image: example.test/app:1
          volumeMounts:
            - name: data
              mountPath: /data
  volumeClaimTemplates:
    - metadata:
        name: data
      spec:
        accessModes: ["ReadWriteOnce"]
        resources:
          requests:
            storage: 1Gi
`

// storageClassPackage renders a workload that names a storage class, which the
// volume object owns instead (C-32, F4).
const storageClassPackage = `apiVersion: v1
kind: Pod
metadata:
  name: {{ .Name }}
  namespace: {{ .Namespace }}
spec:
  containers:
    - name: app
      image: example.test/app:1
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
        storageClassName: fast
`

func TestTC49PackagesMayNotDeclareStorage(t *testing.T) {
	cluster := harness.Start(t)
	cluster.EnsureNamespace(t, harness.Namespace)
	controller := cluster.ApplicationController(t, app.Options{})

	cases := []struct {
		name       string
		definition string
		template   string
		workload   string
		want       string
	}{
		{
			name:       "volume claim template",
			definition: "stateful",
			template:   claimTemplatePackage,
			workload:   "StatefulSet",
			want:       "volume claim template",
		},
		{
			name:       "storage class",
			definition: "classy",
			template:   storageClassPackage,
			workload:   "Pod",
			want:       "storage class",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cluster.InstallDefinition(t, harness.DefinitionWithTemplate(t, testCase.definition, testCase.template, nil))
			applicationName := testCase.definition + "-app"
			application := harness.Application(applicationName, nil)
			application.Object["spec"] = map[string]any{
				"definitionRef": testCase.definition,
				"namespace":     harness.Namespace,
			}
			cluster.Create(t, application)

			for range 3 {
				harness.ReconcileApplicationRound(t, controller, harness.Namespace, applicationName)
			}
			stored := cluster.Get(t, "v1beta1", "Application", applicationName)
			phase, _, _ := unstructured.NestedString(stored.Object, "status", "phase")
			if phase != app.PhaseFailed {
				t.Fatalf("phase = %q, want %q: the package must be refused", phase, app.PhaseFailed)
			}
			condition, found := state.Find(harness.Conditions(t, stored), app.ConditionInvalidPackage)
			if !found || condition.Status != metav1.ConditionTrue {
				t.Fatalf("%s is not reported as a failure: %+v", app.ConditionInvalidPackage, condition)
			}
			if !strings.Contains(condition.Message, testCase.want) {
				t.Fatalf("reason %q does not point at the %s declaration", condition.Message, testCase.want)
			}
			if workloadExists(t, cluster, testCase.workload, harness.Namespace, applicationName) {
				t.Fatalf("%s %s was written although the package is invalid", testCase.workload, applicationName)
			}
			if workloadExists(t, cluster, "PersistentVolumeClaim", harness.Namespace, "data") {
				t.Fatal("the invalid package created a claim")
			}
		})
	}
}

// workloadExists reports whether the object the invalid package would have
// rendered is in the cluster.
func workloadExists(t *testing.T, cluster *harness.Cluster, kind, namespace, name string) bool {
	t.Helper()
	group := ""
	if kind == "StatefulSet" {
		group = "apps"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := cluster.Direct(t, group, "v1", kind, namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return true
	}
	if !apierrors.IsNotFound(err) {
		t.Fatalf("read %s %s/%s: %v", kind, namespace, name, err)
	}
	return false
}
