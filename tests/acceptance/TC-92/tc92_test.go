// Package tc92 verifies the rescue mode: after the control plane stops, rescue
// reads native resources directly and writes nothing — not to the cluster and
// not to the control-plane database (AC-46, TC-92).
package tc92

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/install"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC92RescueIsReadOnly(t *testing.T) {
	cluster := harness.StartAudited(t)
	installerIdentity := harness.InstallerIdentity()
	cluster.GrantClusterAdmin(t, installerIdentity)
	identityStore := harness.OpenIdentity(t, nil, nil)
	clients := cluster.InstallClients(t, installerIdentity)
	ctx := context.Background()

	runner, err := install.New(install.Config{
		Plan:          harness.InstallerPlan(nil),
		Dynamic:       clients.Dynamic,
		Kube:          clients.Kube,
		APIExtensions: clients.APIExtensions,
		Definitions:   harness.Definitions(t),
		Accounts:      identityStore.Service,
		QuietWindow:   200 * time.Millisecond,
		SettleTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("build installer: %v", err)
	}
	if _, err := runner.Install(ctx); err != nil {
		t.Fatalf("install: %v", err)
	}

	// Stop the control plane: the rescue reader takes over from a dead process.
	if err := cluster.Kube.AppsV1().Deployments(api.SystemNamespace).Delete(ctx, install.ControlPlaneName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("stop the control plane: %v", err)
	}
	harness.Eventually(t, 30*time.Second, "the control-plane deployment to stop", func() bool {
		_, err := cluster.Kube.AppsV1().Deployments(api.SystemNamespace).Get(ctx, install.ControlPlaneName, metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})

	snapshotter := &install.Snapshotter{Dynamic: cluster.Dynamic, Discovery: cluster.Kube.Discovery()}
	before, err := snapshotter.Take(ctx)
	if err != nil {
		t.Fatalf("snapshot before rescue: %v", err)
	}
	databaseBefore := fileDigest(t, identityStore.DatabasePath)
	rescueStarted := time.Now()

	rescue, err := install.NewRescue(cluster.Dynamic, cluster.Kube.Discovery())
	if err != nil {
		t.Fatalf("build rescue reader: %v", err)
	}
	views, err := rescue.Native(ctx)
	if err != nil {
		t.Fatalf("rescue reads native resources: %v", err)
	}
	if !fetchedSomething(views) {
		t.Fatal("rescue returned no native objects")
	}

	after, err := snapshotter.Take(ctx)
	if err != nil {
		t.Fatalf("snapshot after rescue: %v", err)
	}
	if !reflect.DeepEqual(before.Objects, after.Objects) {
		t.Fatal("rescue changed a cluster object")
	}
	if digest := fileDigest(t, identityStore.DatabasePath); digest != databaseBefore {
		t.Fatal("rescue changed the control-plane database")
	}
	events, err := cluster.AuditSource(t).Events(ctx, rescueStarted, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("read the audit log: %v", err)
	}
	for _, event := range events {
		if event.User == installerIdentity || event.User == harness.ControlPlaneIdentity() {
			t.Fatalf("rescue produced the write %s %s by %q", event.Verb, event.Object.Name, event.User)
		}
	}
}

// fetchedSomething reports whether rescue read at least one native object.
func fetchedSomething(views []install.NativeView) bool {
	for _, view := range views {
		if len(view.Objects) > 0 {
			return true
		}
	}
	return false
}

// fileDigest is the sha256 of one file, used to prove the database is untouched.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
