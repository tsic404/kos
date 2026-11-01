package controlplane

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/state"
)

// TestAcceptActionConfirmationGate covers C-35: a confirmation is only accepted
// while an external change holds the application paused, and the pause is the
// one reason the reconciler writes.
func TestAcceptActionConfirmationGate(t *testing.T) {
	paused := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{
			Phase: app.PhaseReady,
			Conditions: []metav1.Condition{
				state.Blocked(1, app.ReasonExternalChangePending, "external changes await confirmation"),
			},
		}},
	}
	if failure := acceptAction(actionRequest{Action: app.ActionConfirm}, paused); failure != nil {
		t.Fatalf("a paused application refused the confirmation: %+v", failure)
	}

	running := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{
			Phase:      app.PhaseReady,
			Conditions: []metav1.Condition{state.Applied(1, "desired state applied")},
		}},
	}
	if failure := acceptAction(actionRequest{Action: app.ActionConfirm}, running); failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("a running application accepted the confirmation: %+v", failure)
	}

	failed := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{Phase: app.PhaseFailed}},
	}
	if failure := acceptAction(actionRequest{Action: app.ActionConfirm}, failed); failure == nil {
		t.Fatal("a failed application accepted the confirmation")
	}
}

// TestAcceptActionRetriesARolledBackUpgrade covers C-34: a reverted upgrade is
// the user's to retry, and that retry is accepted without the application ever
// entering Failed.
func TestAcceptActionRetriesARolledBackUpgrade(t *testing.T) {
	rolledBack := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{
			ObjectStatus: v1beta1.ObjectStatus{Phase: app.PhaseReady},
			Upgrade:      &v1beta1.UpgradeStatus{Target: "sha256:one", RolledBack: true},
		},
	}
	if failure := acceptAction(actionRequest{Action: app.ActionRetry}, rolledBack); failure != nil {
		t.Fatalf("a rolled-back upgrade refused the retry: %+v", failure)
	}

	ready := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{Phase: app.PhaseReady}},
	}
	if failure := acceptAction(actionRequest{Action: app.ActionRetry}, ready); failure == nil {
		t.Fatal("a ready application with no reverted upgrade accepted the retry")
	}
}

// TestAcceptActionReinstall covers C-56's reinstall entry: a ready application
// whose provider crashed and a failed install are reinstalled from the current
// spec, while an application that never installed is refused.
func TestAcceptActionReinstall(t *testing.T) {
	if !recordAction(app.ActionReinstall) {
		t.Fatal("reinstall is a one-shot action, not a spec write")
	}
	for _, phase := range []string{app.PhaseReady, app.PhaseFailed} {
		application := &v1beta1.Application{
			Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{Phase: phase}},
		}
		if failure := acceptAction(actionRequest{Action: app.ActionReinstall}, application); failure != nil {
			t.Fatalf("a %s application refused the reinstall: %+v", phase, failure)
		}
	}
	for _, phase := range []string{"", app.PhasePending, app.PhaseInstalling, app.PhaseUninstalling} {
		application := &v1beta1.Application{
			Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{Phase: phase}},
		}
		if failure := acceptAction(actionRequest{Action: app.ActionReinstall}, application); failure == nil || failure.Code != CodeInvalidField {
			t.Fatalf("a %q application accepted the reinstall: %+v", phase, failure)
		}
	}
}
