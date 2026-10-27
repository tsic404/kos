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
	if failure := acceptAction(app.ActionConfirm, paused); failure != nil {
		t.Fatalf("a paused application refused the confirmation: %+v", failure)
	}

	running := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{
			Phase:      app.PhaseReady,
			Conditions: []metav1.Condition{state.Applied(1, "desired state applied")},
		}},
	}
	if failure := acceptAction(app.ActionConfirm, running); failure == nil || failure.Code != CodeInvalidField {
		t.Fatalf("a running application accepted the confirmation: %+v", failure)
	}

	failed := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{Phase: app.PhaseFailed}},
	}
	if failure := acceptAction(app.ActionConfirm, failed); failure == nil {
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
	if failure := acceptAction(app.ActionRetry, rolledBack); failure != nil {
		t.Fatalf("a rolled-back upgrade refused the retry: %+v", failure)
	}

	ready := &v1beta1.Application{
		Status: v1beta1.ApplicationStatus{ObjectStatus: v1beta1.ObjectStatus{Phase: app.PhaseReady}},
	}
	if failure := acceptAction(app.ActionRetry, ready); failure == nil {
		t.Fatal("a ready application with no reverted upgrade accepted the retry")
	}
}
