// Package tc66 moves a volume from the local policy to the network policy: the
// six phases run to completion, the binding points at the new claim, the
// application object is unchanged and comes back Ready, and the source claim is
// released but still there (AC-33, TC-66).
package tc66

import (
	"context"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/migration"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	namespace   = "data"
	volumeName  = "data"
	sourceClass = "local"
	targetClass = "network"
)

func TestTC66SuccessfulMigration(t *testing.T) {
	cluster := harness.Start(t)
	world := harness.StartMigrationWorld(t, cluster, namespace, volumeName, sourceClass, targetClass)
	before := harness.ShapeOf(cluster.GetIn(t, namespace, "v1beta1", "Application", world.Application))
	world.RequestMigration(t, cluster, targetClass)

	// Each round advances the flow and plays the facts envtest does not
	// provision: the target claim binds and the step Jobs report their result.
	for range 12 {
		world.Round(t)
		if cluster.ClaimExists(t, namespace, world.TargetClaim) {
			world.BindTarget(t, cluster, targetClass)
		}
		requireRunnerJob(t, cluster, world, migration.StepCopy)
		requireRunnerJob(t, cluster, world, migration.StepVerify)
		world.CompleteStep(t, cluster, migration.StepCopy)
		world.CompleteStep(t, cluster, migration.StepVerify)
		if world.Phase(t, cluster) == migration.PhaseSucceeded {
			break
		}
	}

	stored := cluster.MigrationOf(t, namespace, volumeName)
	if stored.Status.Phase != migration.PhaseSucceeded {
		t.Fatalf("migration phase = %q, want %q; status %+v", stored.Status.Phase, migration.PhaseSucceeded, stored.Status)
	}
	if stored.Status.RollbackPoint != "" {
		t.Fatalf("rollback point = %q, want it cleared after the attempt", stored.Status.RollbackPoint)
	}
	if stored.Status.Failure != nil {
		t.Fatalf("a completed migration reports a failure: %+v", stored.Status.Failure)
	}
	if stored.Status.SourceClaim != world.SourceClaim || stored.Status.TargetClaim != world.TargetClaim {
		t.Fatalf("claims = %q -> %q, want %q -> %q",
			stored.Status.SourceClaim, stored.Status.TargetClaim, world.SourceClaim, world.TargetClaim)
	}

	// The binding points at the new claim with the policy it resolved.
	volume := cluster.GetIn(t, namespace, "v1beta1", "Volume", volumeName)
	bound, _, _ := unstructured.NestedString(volume.Object, "status", "boundClaim")
	if bound != world.TargetClaim {
		t.Fatalf("bound claim = %q, want %q", bound, world.TargetClaim)
	}
	class, _, _ := unstructured.NestedString(volume.Object, "status", "storageClass")
	if class != targetClass {
		t.Fatalf("actual policy = %q, want %q", class, targetClass)
	}

	// The application came back Ready and its workloads mount the new claim.
	if phase := phaseOf(t, cluster, world.Application); phase != app.PhaseReady {
		t.Fatalf("application phase = %q, want %q after the migration", phase, app.PhaseReady)
	}
	workload := cluster.Workload(t, "Deployment", namespace, world.Application)
	if claim := claimOf(t, workload); claim != world.TargetClaim {
		t.Fatalf("workload mounts claim %q, want %q", claim, world.TargetClaim)
	}
	replicas, _, _ := unstructured.NestedInt64(workload.Object, "spec", "replicas")
	if replicas != 1 {
		t.Fatalf("workload replicas = %d, want the declared 1 after the migration", replicas)
	}

	// The application object itself is unchanged (C-40).
	after := harness.ShapeOf(cluster.GetIn(t, namespace, "v1beta1", "Application", world.Application))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the application object changed across the migration:\nbefore %v\nafter  %v", before, after)
	}

	// The source claim is released and still there; the target claim binds it.
	source := cluster.Claim(t, namespace, world.SourceClaim)
	if source.GetLabels()["released"] == "" {
		t.Fatalf("the source claim carries no release marker: %v", source.GetLabels())
	}
	if source.GetDeletionTimestamp() != nil {
		t.Fatal("the source claim is being deleted; the migration may not delete it")
	}
	target := cluster.Claim(t, namespace, world.TargetClaim)
	if phase, _, _ := unstructured.NestedString(target.Object, "status", "phase"); phase != "Bound" {
		t.Fatalf("the target claim is %q, want Bound", phase)
	}

	// Cleanup removed the step Jobs of the flow.
	for _, step := range []string{migration.StepCopy, migration.StepVerify} {
		if !cluster.JobGone(t, namespace, world.TargetClaim+"-"+step) {
			t.Fatalf("the %s Job survived Cleanup", step)
		}
	}
}

// requireRunnerJob requires a step Job to give the runner both claims as
// mounts, the source read-only, and to carry the pod baseline the flow runs an
// external image under (C-40): no API token, no privileges, bounded resources.
func requireRunnerJob(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld, step string) {
	t.Helper()
	name := world.TargetClaim + "-" + step
	if cluster.JobGone(t, world.Namespace, name) {
		return
	}
	job, err := cluster.Direct(t, "batch", "v1", "Job", world.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the %s Job: %v", step, err)
	}
	pod, _, _ := unstructured.NestedMap(job.Object, "spec", "template", "spec")
	// The field has no API-side default on a Job template, so an absent value
	// is not the same as "off": the flow has to write it (C-40).
	if token, found, _ := unstructured.NestedBool(pod, "automountServiceAccountToken"); !found || token {
		t.Fatalf("the %s Job leaves the service account token setting open (found=%v value=%v)", step, found, token)
	}
	if value, found, _ := unstructured.NestedBool(pod, "securityContext", "runAsNonRoot"); !found || !value {
		t.Fatalf("the %s Job does not run as non-root: %v", step, pod["securityContext"])
	}
	if profile, found, _ := unstructured.NestedString(pod, "securityContext", "seccompProfile", "type"); !found || profile != "RuntimeDefault" {
		t.Fatalf("the %s Job seccomp profile = %q (found=%v), want RuntimeDefault", step, profile, found)
	}
	volumes, _, _ := unstructured.NestedSlice(pod, "volumes")
	claims := map[string]map[string]any{}
	for _, raw := range volumes {
		volume, _ := raw.(map[string]any)
		name, _, _ := unstructured.NestedString(volume, "name")
		claim, _, _ := unstructured.NestedMap(volume, "persistentVolumeClaim")
		if claim != nil {
			claims[name] = claim
		}
	}
	for _, want := range []struct {
		volume string
		claim  string
		read   bool
	}{
		{volume: "source", claim: world.SourceClaim, read: true},
		{volume: "target", claim: world.TargetClaim},
	} {
		claim, found := claims[want.volume]
		if !found {
			t.Fatalf("the %s Job declares no %s claim volume: %v", step, want.volume, volumes)
		}
		if claim["claimName"] != want.claim {
			t.Fatalf("the %s Job mounts claim %v for %s, want %q", step, claim["claimName"], want.volume, want.claim)
		}
		if readOnly, _, _ := unstructured.NestedBool(claim, "readOnly"); readOnly != want.read {
			t.Fatalf("the %s Job declares readOnly=%v for %s, want %v", step, readOnly, want.volume, want.read)
		}
	}
	containers, _, _ := unstructured.NestedSlice(pod, "containers")
	if len(containers) != 1 {
		t.Fatalf("the %s Job runs %d containers, want one", step, len(containers))
	}
	container, _ := containers[0].(map[string]any)
	mounts, _, _ := unstructured.NestedSlice(container, "volumeMounts")
	paths := map[string]string{}
	for _, raw := range mounts {
		mount, _ := raw.(map[string]any)
		name, _, _ := unstructured.NestedString(mount, "name")
		path, _, _ := unstructured.NestedString(mount, "mountPath")
		if path == "" {
			t.Fatalf("the %s Job mounts %q without a path", step, name)
		}
		readOnly, _, _ := unstructured.NestedBool(mount, "readOnly")
		paths[name] = path
		if name == "source" && !readOnly {
			t.Fatalf("the %s Job mounts the source claim writable", step)
		}
	}
	if paths["source"] == "" || paths["target"] == "" {
		t.Fatalf("the %s Job mounts %v, want both claims", step, paths)
	}
	// The runner reaches the claims only through these mounts, so the step and
	// the two paths are what its arguments carry (C-40).
	args, _, _ := unstructured.NestedStringSlice(container, "args")
	if len(args) != 3 || args[0] != step || args[1] != paths["source"] || args[2] != paths["target"] {
		t.Fatalf("the %s Job args = %v, want [%s %s %s]", step, args, step, paths["source"], paths["target"])
	}
	if value, found, _ := unstructured.NestedBool(container, "securityContext", "allowPrivilegeEscalation"); !found || value {
		t.Fatalf("the %s Job allows privilege escalation", step)
	}
	if value, found, _ := unstructured.NestedBool(container, "securityContext", "readOnlyRootFilesystem"); !found || !value {
		t.Fatalf("the %s Job runs a writable root filesystem", step)
	}
	drop, _, _ := unstructured.NestedStringSlice(container, "securityContext", "capabilities", "drop")
	if len(drop) != 1 || drop[0] != "ALL" {
		t.Fatalf("the %s Job drops %v capabilities, want ALL", step, drop)
	}
	for _, side := range []string{"requests", "limits"} {
		values, _, _ := unstructured.NestedMap(container, "resources", side)
		if len(values) == 0 {
			t.Fatalf("the %s Job declares no resource %s", step, side)
		}
	}
}

// claimOf reads the claim the rendered workload mounts.
func claimOf(t *testing.T, workload *unstructured.Unstructured) string {
	t.Helper()
	volumes, _, _ := unstructured.NestedSlice(workload.Object, "spec", "template", "spec", "volumes")
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		claim, _, _ := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		if claim != "" {
			return claim
		}
	}
	t.Fatalf("the workload mounts no claim: %v", volumes)
	return ""
}

// phaseOf reads the stored application phase.
func phaseOf(t *testing.T, cluster *harness.Cluster, name string) string {
	t.Helper()
	phase, _, err := unstructured.NestedString(cluster.GetIn(t, namespace, "v1beta1", "Application", name).Object, "status", "phase")
	if err != nil {
		t.Fatalf("read application %q phase: %v", name, err)
	}
	return phase
}
