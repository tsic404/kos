package harness

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	_ "modernc.org/sqlite"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/backup"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
)

// WorkerImage is the digest-pinned component reference the acceptance cases
// hand the controller; the kernel refuses a mutable tag (C-17, C-41).
const WorkerImage = "example.test/kos-backup@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// BackupPolicyManifest returns a namespaced backup policy with the given spec
// and no status, so the backup reconciler is its only status writer.
func BackupPolicyManifest(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	manifest := Manifest("BackupPolicy", namespace, name, map[string]string{"managed-by": "acceptance"}, nil)
	manifest["spec"] = spec
	return &unstructured.Unstructured{Object: manifest}
}

// RestoreJobManifest returns a namespaced restore job with the given spec and no
// status, so the restore reconciler is its only status writer.
func RestoreJobManifest(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	manifest := Manifest("RestoreJob", namespace, name, map[string]string{"managed-by": "acceptance"}, nil)
	manifest["spec"] = spec
	return &unstructured.Unstructured{Object: manifest}
}

// BackupSurface is one backup controller and the worker it hands long actions
// to. Reconcile waits for the worker, so the next call observes the outcome the
// way a later poll would.
type BackupSurface struct {
	Controller *backup.Controller
	Worker     *backup.LocalWorker
}

// BackupSurface builds the real backup controller over the cluster, reading
// volume data under dataRoot.
func (c *Cluster) BackupSurface(t *testing.T, dataRoot string) *BackupSurface {
	t.Helper()
	plane := backup.DirDataPlane{Root: dataRoot}
	worker := backup.NewLocalWorker(plane)
	store := backup.NewWriterStore(c.Writer(), write.SubjectBackupReconciler)
	controller := backup.NewController(store, worker, backup.Options{WorkerImage: WorkerImage})
	return &BackupSurface{Controller: controller, Worker: worker}
}

// Reconcile runs one backup policy round, then lets the handed-off action
// finish so the next round can observe it.
func (s *BackupSurface) Reconcile(t *testing.T, namespace, name string) reconcile.Result {
	t.Helper()
	result, err := s.Controller.Reconcile(context.Background(), reconcile.Request{Kind: "BackupPolicy", Namespace: namespace, Name: name})
	if err != nil {
		t.Fatalf("reconcile backup policy %s/%s: %v", namespace, name, err)
	}
	s.Worker.Wait()
	return result
}

// RestoreSurface is one restore controller and its worker.
type RestoreSurface struct {
	Controller *backup.RestoreController
	Worker     *backup.LocalWorker
}

// RestoreSurface builds the real restore controller over the cluster.
func (c *Cluster) RestoreSurface(t *testing.T, dataRoot string) *RestoreSurface {
	t.Helper()
	plane := backup.DirDataPlane{Root: dataRoot}
	worker := backup.NewLocalWorker(plane)
	store := backup.NewWriterStore(c.Writer(), write.SubjectMigrationReconciler)
	controller := backup.NewRestoreController(store, plane, worker, backup.Options{})
	return &RestoreSurface{Controller: controller, Worker: worker}
}

// Reconcile runs one restore round, then lets the handed-off action finish so
// the next round can observe it.
func (s *RestoreSurface) Reconcile(t *testing.T, namespace, name string) reconcile.Result {
	t.Helper()
	result, err := s.Controller.Reconcile(context.Background(), reconcile.Request{Kind: "RestoreJob", Namespace: namespace, Name: name})
	if err != nil {
		t.Fatalf("reconcile restore job %s/%s: %v", namespace, name, err)
	}
	s.Worker.Wait()
	return result
}

// ScheduleCronJob plays the cluster's cron controller: it writes the newest
// scheduled instant on the policy's CronJob, which is what asks the kernel for
// one run (C-04, C-41).
func (c *Cluster) ScheduleCronJob(t *testing.T, namespace, name string, at time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	resource := c.Direct(t, "batch", "v1", "CronJob", namespace)
	stored, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cron job %s/%s: %v", namespace, name, err)
	}
	if err := unstructured.SetNestedField(stored.Object, at.UTC().Format(time.RFC3339), "status", "lastScheduleTime"); err != nil {
		t.Fatalf("set lastScheduleTime of %s: %v", name, err)
	}
	if _, err := resource.UpdateStatus(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("write status of cron job %s: %v", name, err)
	}
}

// CronJob reads the schedule object of one policy; a missing object fails the
// case, so a case that expects none uses MissingCronJob.
func (c *Cluster) CronJob(t *testing.T, namespace, name string) *unstructured.Unstructured {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	object, err := c.Direct(t, "batch", "v1", "CronJob", namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cron job %s/%s: %v", namespace, name, err)
	}
	return object
}

// MissingCronJob reports whether a policy materialized no schedule object.
func (c *Cluster) MissingCronJob(t *testing.T, namespace, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	_, err := c.Direct(t, "batch", "v1", "CronJob", namespace).Get(ctx, name, metav1.GetOptions{})
	return err != nil
}

// PersistentVolumeOnNode writes a persistent volume pinned to one node, so the
// failure-domain preflight of C-41 has a domain to compare against.
func (c *Cluster) PersistentVolumeOnNode(t *testing.T, name, storageClass, driver, reclaimPolicy, node string) {
	t.Helper()
	object := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"capacity":                      map[string]any{"storage": "1Gi"},
			"accessModes":                   []any{"ReadWriteOnce"},
			"persistentVolumeReclaimPolicy": reclaimPolicy,
			"storageClassName":              storageClass,
			"csi":                           map[string]any{"driver": driver, "volumeHandle": name},
			"nodeAffinity": map[string]any{"required": map[string]any{"nodeSelectorTerms": []any{
				map[string]any{"matchExpressions": []any{map[string]any{
					"key": "kubernetes.io/hostname", "operator": "In", "values": []any{node},
				}}},
			}}},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if _, err := c.Direct(t, "", "v1", "PersistentVolume", "").Create(ctx, &unstructured.Unstructured{Object: object}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create persistent volume %s: %v", name, err)
	}
}

// SeedVolumeData writes one volume's data: a database with two tables and two
// files, so each baseline item of C-42 has a value to compare.
func SeedVolumeData(t *testing.T, directory string) {
	t.Helper()
	WriteFixtureFile(t, filepath.Join(directory, "notes.txt"), "hello")
	WriteFixtureFile(t, filepath.Join(directory, "assets", "logo.bin"), "1234567890")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create %s: %v", directory, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "app.db"))
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT)",
		"INSERT INTO users (id, name) VALUES (1, 'ada'), (2, 'grace')",
		"CREATE TABLE posts (id INTEGER PRIMARY KEY, body TEXT)",
		"INSERT INTO posts (id, body) VALUES (1, 'first')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("exec %q: %v", statement, err)
		}
	}
}

// WriteFixtureFile writes one file below a case's data directory.
func WriteFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// DecodeBackupPolicy reads one stored policy as its typed form.
func (c *Cluster) DecodeBackupPolicy(t *testing.T, namespace, name string) *v1beta1.BackupPolicy {
	t.Helper()
	stored := c.GetIn(t, namespace, "v1beta1", "BackupPolicy", name)
	policy := &v1beta1.BackupPolicy{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, policy); err != nil {
		t.Fatalf("decode backup policy %s: %v", name, err)
	}
	return policy
}

// DecodeRestoreJob reads one stored restore job as its typed form.
func (c *Cluster) DecodeRestoreJob(t *testing.T, namespace, name string) *v1beta1.RestoreJob {
	t.Helper()
	stored := c.GetIn(t, namespace, "v1beta1", "RestoreJob", name)
	job := &v1beta1.RestoreJob{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(stored.Object, job); err != nil {
		t.Fatalf("decode restore job %s: %v", name, err)
	}
	return job
}

// KernelApplication is the namespaced application with a backup label and an
// owner, as a case that a policy covers writes it.
func KernelApplication(namespace, name, definitionRef, owner, label string) *unstructured.Unstructured {
	application := ApplicationIn(namespace, name, nil)
	application.SetLabels(map[string]string{"managed-by": "acceptance", label: "true"})
	application.Object["spec"] = map[string]any{
		"definitionRef": definitionRef,
		"namespace":     namespace,
		"volumes":       []any{},
		"owner":         owner,
	}
	return application
}

// KernelVolume is the namespaced volume with a backup label and an owner.
func KernelVolume(namespace, name, mobility, capacity, storageClass, owner, label string) *unstructured.Unstructured {
	volume := Volume(namespace, name, map[string]any{
		"mobility": mobility, "capacity": capacity, "storageClass": storageClass, "owner": owner,
	})
	volume.SetLabels(map[string]string{"managed-by": "acceptance", label: "true"})
	return volume
}

// HookDefinition builds a definition whose package declares one hook, with the
// digest recomputed over the whole spec.
func HookDefinition(t *testing.T, name, template string, hooks ...v1beta1.Hook) *v1beta1.ApplicationDefinition {
	t.Helper()
	definition := TemplateDefinition(t, name, template, nil)
	definition.Spec.Hooks = hooks
	digest, err := app.DefinitionDigest(definition.Spec)
	if err != nil {
		t.Fatalf("digest definition %q: %v", name, err)
	}
	definition.Spec.Package.Digest = digest
	if err := app.ValidateDefinition(definition.Spec); err != nil {
		t.Fatalf("definition %q is invalid: %v", name, err)
	}
	return definition
}
