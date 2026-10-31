// Package tc67 injects one failure into each of the five phases that have a
// rollback: Preflight, Quiesce, Copy, Verify and Cutover. Every attempt must
// return the binding to the source claim, come back with the application
// Ready, record the failed phase and its reason, delete the target claim it
// created, and leave the source claim alone (AC-33, TC-67).
package tc67

import (
	"context"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/migration"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	namespace   = "data"
	sourceClass = "local"
	targetClass = "network"
)

// winner is the deadline the window gives; a case that waits for it advances
// the clock past this value.
const window = 30 * time.Minute

func TestTC67PhaseFailuresRollBack(t *testing.T) {
	cluster := harness.Start(t)
	tests := []struct {
		name   string
		phase  string
		inject func(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld) bool
	}{
		{
			name:  "preflight",
			phase: migration.PhasePreflight,
			inject: func(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld) bool {
				// The target storage class does not exist, so the request is
				// refused before anything is changed.
				if err := cluster.ExtensionResource("storage.k8s.io", "v1", "storageclasses", "").
					Delete(context.Background(), targetClass, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("remove storage class %q: %v", targetClass, err)
				}
				for range 4 {
					world.MigrationRound(t)
				}
				return world.Phase(t, cluster) == migration.PhaseFailed
			},
		},
		{
			name:  "quiesce",
			phase: migration.PhaseQuiesce,
			inject: func(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld) bool {
				// The application never exits: its own round is not run, so the
				// workloads stay at one and the window runs out.
				for range 4 {
					world.MigrationRound(t)
					if world.Phase(t, cluster) == migration.PhaseQuiesce {
						world.Clock.Advance(window + time.Minute)
					}
					if world.Phase(t, cluster) == migration.PhaseFailed {
						return true
					}
				}
				return false
			},
		},
		{
			name:  "copy",
			phase: migration.PhaseCopy,
			inject: func(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld) bool {
				// The target storage class refuses writes during the copy.
				for range 10 {
					world.Round(t)
					world.FailStep(t, cluster, migration.StepCopy, "the target storage class refuses writes")
					if world.Phase(t, cluster) == migration.PhaseFailed {
						return true
					}
				}
				return false
			},
		},
		{
			name:  "verify",
			phase: migration.PhaseVerify,
			inject: func(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld) bool {
				// The target holds an extra file, so the verification fails.
				for range 10 {
					world.Round(t)
					world.CompleteStep(t, cluster, migration.StepCopy)
					world.FailStep(t, cluster, migration.StepVerify, "the target holds an extra file")
					if world.Phase(t, cluster) == migration.PhaseFailed {
						return true
					}
				}
				return false
			},
		},
		{
			name:  "cutover",
			phase: migration.PhaseCutover,
			inject: func(t *testing.T, cluster *harness.Cluster, world *harness.MigrationWorld) bool {
				// The target claim never binds, so the binding cannot move and
				// the window runs out.
				for range 10 {
					world.Round(t)
					world.CompleteStep(t, cluster, migration.StepCopy)
					world.CompleteStep(t, cluster, migration.StepVerify)
					if world.Phase(t, cluster) == migration.PhaseCutover {
						world.Clock.Advance(window + time.Minute)
					}
					if world.Phase(t, cluster) == migration.PhaseFailed {
						return true
					}
				}
				return false
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			world := harness.StartMigrationWorld(t, cluster, namespace, test.name, sourceClass, targetClass)
			before := harness.ShapeOf(cluster.GetIn(t, namespace, "v1beta1", "Application", world.Application))
			world.RequestMigration(t, cluster, targetClass)

			if !test.inject(t, cluster, world) {
				t.Fatalf("migration phase = %q, want %q after the injected failure",
					world.Phase(t, cluster), migration.PhaseFailed)
			}
			stored := cluster.MigrationOf(t, namespace, world.Volume)
			if stored.Status.Failure == nil {
				t.Fatalf("the failed attempt records no failure: %+v", stored.Status)
			}
			if stored.Status.Failure.Phase != test.phase {
				t.Fatalf("failure phase = %q, want %q", stored.Status.Failure.Phase, test.phase)
			}
			if stored.Status.Failure.Message == "" {
				t.Fatal("the failure records no reason")
			}
			if stored.Status.RollbackPoint != "" {
				t.Fatalf("rollback point = %q, want it cleared after the rollback", stored.Status.RollbackPoint)
			}

			// The binding returned to the source claim. A preflight refusal
			// changes nothing, so it never wrote a binding at all.
			volume := cluster.GetIn(t, namespace, "v1beta1", "Volume", world.Volume)
			bound, _, _ := unstructured.NestedString(volume.Object, "status", "boundClaim")
			if bound == world.TargetClaim {
				t.Fatalf("the binding stayed on the target claim %q after the rollback", bound)
			}
			if test.phase != migration.PhasePreflight && bound != world.SourceClaim {
				t.Fatalf("bound claim = %q, want the source claim %q after the rollback", bound, world.SourceClaim)
			}

			// The flow deleted the target claim it created and never touched
			// the source claim.
			if !cluster.ClaimDeleted(t, namespace, world.TargetClaim) {
				t.Fatalf("the target claim %q survived the rollback", world.TargetClaim)
			}
			harness.ReleaseClaimDeletion(t, cluster, namespace, world.TargetClaim)
			if cluster.ClaimExists(t, namespace, world.TargetClaim) {
				t.Fatalf("the target claim %q is still stored after the rollback", world.TargetClaim)
			}
			source := cluster.Claim(t, namespace, world.SourceClaim)
			if source.GetDeletionTimestamp() != nil {
				t.Fatal("the source claim is being deleted; no phase may delete it")
			}
			if source.GetLabels()["released"] != "" {
				t.Fatalf("a failed attempt released the source claim: %v", source.GetLabels())
			}

			// The application comes back Ready and its object is unchanged.
			for range 6 {
				world.ApplicationRound(t)
				if phaseOf(t, cluster, world.Application) == app.PhaseReady {
					break
				}
			}
			if phase := phaseOf(t, cluster, world.Application); phase != app.PhaseReady {
				t.Fatalf("application phase = %q, want %q after the rollback", phase, app.PhaseReady)
			}
			workload := cluster.Workload(t, "Deployment", namespace, world.Application)
			replicas, _, _ := unstructured.NestedInt64(workload.Object, "spec", "replicas")
			if replicas != 1 {
				t.Fatalf("workload replicas = %d, want the declared 1 after the rollback", replicas)
			}
			after := harness.ShapeOf(cluster.GetIn(t, namespace, "v1beta1", "Application", world.Application))
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("the application object changed across the failed attempt:\nbefore %v\nafter  %v", before, after)
			}
		})
	}
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

// TestTC67WindowIsHonouredBeforeQuiesce covers the two window rules of C-40: a
// start time in the future keeps Preflight waiting, and a window that closed
// before Quiesce fails the attempt without changing anything.
func TestTC67WindowIsHonouredBeforeQuiesce(t *testing.T) {
	cluster := harness.Start(t)
	for _, test := range []struct {
		name  string
		start time.Time
		want  string
	}{
		{name: "waiting", start: time.Now().Add(time.Hour), want: migration.PhasePreflight},
		{name: "closed", start: time.Now().Add(-2 * time.Hour), want: migration.PhaseFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := harness.StartMigrationWorld(t, cluster, namespace, test.name, sourceClass, targetClass)
			ctx := context.Background()
			volume := cluster.Direct(t, api.Group, "v1beta1", "Volume", namespace)
			stored, err := volume.Get(ctx, world.Volume, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("read volume %q: %v", world.Volume, err)
			}
			if err := unstructured.SetNestedField(stored.Object, targetClass, "spec", "storageClass"); err != nil {
				t.Fatalf("write the declared policy: %v", err)
			}
			if _, err := volume.Update(ctx, stored, metav1.UpdateOptions{}); err != nil {
				t.Fatalf("update volume %q: %v", world.Volume, err)
			}
			cluster.CreateIn(t, namespace, harness.Migration(namespace, world.Volume, map[string]any{
				"volume":             world.Volume,
				"targetStorageClass": targetClass,
				"window": map[string]any{
					"start":    metav1.NewTime(test.start).Format(time.RFC3339),
					"duration": "30m",
				},
			}))
			for range 3 {
				world.MigrationRound(t)
			}
			if phase := world.Phase(t, cluster); phase != test.want {
				t.Fatalf("phase = %q, want %q for the %s window", phase, test.want, test.name)
			}
			if cluster.ClaimExists(t, namespace, world.TargetClaim) {
				t.Fatalf("a %s window created the target claim", test.name)
			}
			if test.want != migration.PhaseFailed {
				return
			}
			storedMigration := cluster.MigrationOf(t, namespace, world.Volume)
			if storedMigration.Status.Failure == nil || storedMigration.Status.Failure.Phase != migration.PhasePreflight {
				t.Fatalf("a closed window does not report a Preflight failure: %+v", storedMigration.Status.Failure)
			}
			// Nothing was changed: the attempt never wrote a binding.
			bound, _, _ := unstructured.NestedString(cluster.GetIn(t, namespace, "v1beta1", "Volume", world.Volume).Object, "status", "boundClaim")
			if bound != "" {
				t.Fatalf("a refused window wrote the binding %q", bound)
			}
		})
	}
}
