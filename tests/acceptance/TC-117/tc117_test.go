// Package tc117 verifies the login, lock and logout contract: the three
// failures share one answer, an unknown name locks like an existing one, the
// lock expires with a cleared counter, and a logged-out session is rejected
// (AC-58, TC-117).
package tc117

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	ownerName     = "owner"
	ownerPassword = "owner-password"
	wrongPassword = "wrong-password"
)

// TestTC117LoginLockAndLogout runs the scenario against the account API.
func TestTC117LoginLockAndLogout(t *testing.T) {
	surface := harness.StartIdentity(t, nil, nil)
	oneTime := surface.BootstrapOwner(t, ownerName)
	token, _ := surface.SignIn(t, ownerName, oneTime)
	surface.ChangePassword(t, token, oneTime, ownerPassword)

	// Five failures for an existing account and for an unknown name produce the
	// same answer, attempt by attempt.
	for attempt := range identity.LoginFailureLimit {
		existingStatus, existingBody := failedLogin(t, surface, ownerName)
		unknownStatus, unknownBody := failedLogin(t, surface, "nobody")
		if existingStatus != http.StatusUnauthorized || unknownStatus != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d and %d, want 401", attempt+1, existingStatus, unknownStatus)
		}
		if !reflect.DeepEqual(existingBody, unknownBody) {
			t.Fatalf("attempt %d answers differ:\nexisting %v\nunknown  %v", attempt+1, existingBody, unknownBody)
		}
	}

	// The sixth attempt is locked, with the remaining time readable, for both.
	existingStatus, existingBody := failedLogin(t, surface, ownerName)
	unknownStatus, unknownBody := failedLogin(t, surface, "nobody")
	if existingStatus != http.StatusLocked || unknownStatus != http.StatusLocked {
		t.Fatalf("sixth attempt status = %d and %d, want 423", existingStatus, unknownStatus)
	}
	if !reflect.DeepEqual(existingBody, unknownBody) {
		t.Fatalf("locked answers differ:\nexisting %v\nunknown  %v", existingBody, unknownBody)
	}
	locked := existingBody["error"].(map[string]any)
	if locked["code"] != controlplane.CodeAccountLocked {
		t.Fatalf("locked code = %v, want %s", locked["code"], controlplane.CodeAccountLocked)
	}
	remaining, _ := locked["retryAfterSeconds"].(float64)
	if remaining != float64(identity.LoginLockDuration/time.Second) {
		t.Fatalf("retryAfterSeconds = %v, want %d", locked["retryAfterSeconds"], identity.LoginLockDuration/time.Second)
	}

	// Past the window both names get five fresh attempts.
	surface.Clock.Advance(identity.LoginLockDuration + time.Second)
	for attempt := range identity.LoginFailureLimit {
		if status, body := failedLogin(t, surface, ownerName); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d after the window = %d %v", attempt+1, status, body)
		}
		if status, body := failedLogin(t, surface, "nobody"); status != http.StatusUnauthorized {
			t.Fatalf("unknown attempt %d after the window = %d %v", attempt+1, status, body)
		}
	}
	if status, _ := failedLogin(t, surface, ownerName); status != http.StatusLocked {
		t.Fatalf("the sixth attempt after the window = %d, want 423", status)
	}
	if status, _ := failedLogin(t, surface, "nobody"); status != http.StatusLocked {
		t.Fatalf("the unknown sixth attempt after the window = %d, want 423", status)
	}

	// The right password signs in once the lock is over.
	surface.Clock.Advance(identity.LoginLockDuration + time.Second)
	token, _ = surface.SignIn(t, ownerName, ownerPassword)
	if status, body := surface.HTTP.Call(t, http.MethodPost, "/api/v1/auth/logout", nil, bearer(token)); status != http.StatusNoContent {
		t.Fatalf("logout = %d %v", status, body)
	}
	status, body := surface.HTTP.Call(t, http.MethodGet, "/api/v1/auth/session", nil, bearer(token))
	if status != http.StatusUnauthorized {
		t.Fatalf("replaying the logged-out session = %d %v, want 401", status, body)
	}
	if code := body["error"].(map[string]any)["code"]; code != controlplane.CodeUnauthenticated {
		t.Fatalf("replay code = %v, want %s", code, controlplane.CodeUnauthenticated)
	}
}

// failedLogin sends one wrong-password login and returns its answer.
func failedLogin(t *testing.T, surface *harness.Identity, username string) (int, map[string]any) {
	t.Helper()
	return surface.HTTP.Call(t, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": username, "password": wrongPassword,
	}, nil)
}

// bearer returns the header carrying a token.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}
