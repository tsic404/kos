package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// startAPI serves the identity routes over a private database.
func startAPI(t *testing.T, owned OwnedObjects) (*Service, *testClock, *Store, string) {
	t.Helper()
	clock := newTestClock()
	store, err := Open(context.Background(), OpenOptions{
		Path: filepath.Join(t.TempDir(), "kos.db"),
		Now:  clock.Now,
	})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service := New(store, Config{Now: clock.Now, Owned: owned})
	server := httptest.NewServer(NewAPI(service).Handler(nil))
	t.Cleanup(server.Close)
	return service, clock, store, server.URL
}

// apiCall sends one JSON request and decodes the answer.
func apiCall(t *testing.T, base, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, base+path, payload)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	decoded := map[string]any{}
	if len(bytes.TrimSpace(answer)) > 0 {
		if err := json.Unmarshal(answer, &decoded); err != nil {
			t.Fatalf("%s %s answered %q: %v", method, path, answer, err)
		}
	}
	return response.StatusCode, decoded
}

// signIn logs in over HTTP and returns the token.
func signIn(t *testing.T, base, username, password string) string {
	t.Helper()
	status, body := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
		"username": username, "password": password,
	}, "")
	if status != http.StatusOK {
		t.Fatalf("login %s = %d %v", username, status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("login %s returned no token: %v", username, body)
	}
	return token
}

// TestLoginSessionAndFirstPassword covers the response a login carries, the
// first-login gate, and the role the next request sees (C-63).
func TestLoginSessionAndFirstPassword(t *testing.T) {
	owned := &fakeOwned{objects: map[string][]OwnedObject{}}
	service, _, _, base := startAPI(t, owned)
	bootstrap, err := service.BootstrapOwner(context.Background(), "owner")
	if err != nil {
		t.Fatalf("bootstrap owner: %v", err)
	}
	status, body := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
		"username": "owner", "password": bootstrap.OneTimePassword,
	}, "")
	if status != http.StatusOK {
		t.Fatalf("first login = %d %v", status, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("the first login returned no token")
	}
	if mustChange, _ := body["mustChangePassword"].(bool); !mustChange {
		t.Fatalf("first login = %v, want the password-change flag", body)
	}
	if remaining := int64(body["remainingSeconds"].(float64)); remaining != int64(SessionIdleTimeout/time.Second) {
		t.Fatalf("remainingSeconds = %d, want %d", remaining, int64(SessionIdleTimeout/time.Second))
	}
	if absolute := int64(body["absoluteRemainingSeconds"].(float64)); absolute != int64(SessionAbsoluteTimeout/time.Second) {
		t.Fatalf("absoluteRemainingSeconds = %d, want %d", absolute, int64(SessionAbsoluteTimeout/time.Second))
	}

	status, body = apiCall(t, base, http.MethodGet, sessionPath, nil, token)
	if status != http.StatusOK {
		t.Fatalf("session read = %d %v", status, body)
	}
	if role, _ := body["role"].(string); role != string(write.RoleOwner) {
		t.Fatalf("session role = %q, want Owner", role)
	}
	status, body = apiCall(t, base, http.MethodGet, "/api/v1/accounts", nil, token)
	if status != http.StatusForbidden || body["error"].(map[string]any)["code"] != controlplane.CodePasswordChangeRequired {
		t.Fatalf("an account read before the change = %d %v, want the change gate", status, body)
	}

	newPassword := "owner-password"
	if status, body = apiCall(t, base, http.MethodPut, passwordPath, map[string]any{
		"currentPassword": bootstrap.OneTimePassword, "newPassword": newPassword,
	}, token); status != http.StatusNoContent {
		t.Fatalf("change password = %d %v", status, body)
	}
	if status, body = apiCall(t, base, http.MethodGet, sessionPath, nil, token); status != http.StatusOK {
		t.Fatalf("session after the change = %d %v", status, body)
	} else if mustChange, _ := body["mustChangePassword"].(bool); mustChange {
		t.Fatal("the session still asks for a password change")
	}

	status, body = apiCall(t, base, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "alice", "role": string(write.RoleMember),
	}, token)
	if status != http.StatusCreated {
		t.Fatalf("create account = %d %v", status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	if oneTime == "" {
		t.Fatalf("create account returned no one-time password: %v", body)
	}
	aliceToken := signIn(t, base, "alice", oneTime)
	if status, body = apiCall(t, base, http.MethodPut, passwordPath, map[string]any{
		"currentPassword": oneTime, "newPassword": "alice-password",
	}, aliceToken); status != http.StatusNoContent {
		t.Fatalf("change alice password = %d %v", status, body)
	}

	status, body = apiCall(t, base, http.MethodPut, "/api/v1/accounts/alice", map[string]any{
		"role": string(write.RoleGuest),
	}, token)
	if status != http.StatusOK {
		t.Fatalf("demote alice = %d %v", status, body)
	}
	session, err := service.Authenticate(context.Background(), aliceToken)
	if err != nil {
		t.Fatalf("authenticate alice: %v", err)
	}
	if session.Role != write.RoleGuest {
		t.Fatalf("alice role on the next request = %q, want Guest", session.Role)
	}
	ctx := withSession(context.Background(), session)
	if err := (Authorizer{}).Decide(ctx, controlplane.Request{Verb: controlplane.VerbRead}); err != nil {
		t.Fatalf("a Guest read = %v", err)
	}
	if err := (Authorizer{}).Decide(ctx, controlplane.Request{Verb: controlplane.VerbCreate}); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("a Guest write = %v, want a refusal", err)
	}
	if err := (Authorizer{}).Decide(context.Background(), controlplane.Request{Verb: controlplane.VerbRead}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a read without a session = %v, want unauthenticated", err)
	}
}

// TestLoginFailureResponsesAreIndistinguishable checks that the HTTP answers of
// an existing and an unknown name match attempt by attempt, and that both lock
// (C-63).
func TestLoginFailureResponsesAreIndistinguishable(t *testing.T) {
	service, clock, _, base := startAPI(t, &fakeOwned{})
	ownerName, ownerPassword := bootstrapOwner(t, service)
	for attempt := range LoginFailureLimit {
		existingStatus, existingBody := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
			"username": ownerName, "password": "wrong-password",
		}, "")
		unknownStatus, unknownBody := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
			"username": "nobody", "password": "wrong-password",
		}, "")
		if existingStatus != http.StatusUnauthorized || unknownStatus != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d and %d, want 401", attempt+1, existingStatus, unknownStatus)
		}
		if !reflect.DeepEqual(existingBody, unknownBody) {
			t.Fatalf("attempt %d answers differ:\nexisting %v\nunknown  %v", attempt+1, existingBody, unknownBody)
		}
		if code := existingBody["error"].(map[string]any)["code"]; code != controlplane.CodeLoginFailed {
			t.Fatalf("attempt %d code = %v, want %s", attempt+1, code, controlplane.CodeLoginFailed)
		}
	}
	existingStatus, existingBody := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
		"username": ownerName, "password": "wrong-password",
	}, "")
	unknownStatus, unknownBody := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
		"username": "nobody", "password": "wrong-password",
	}, "")
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
	if remaining := int64(locked["retryAfterSeconds"].(float64)); remaining != int64(LoginLockDuration/time.Second) {
		t.Fatalf("retryAfterSeconds = %d, want %d", remaining, int64(LoginLockDuration/time.Second))
	}

	// The window ends and both names get their attempts back.
	clock.Advance(LoginLockDuration + time.Second)
	for attempt := range LoginFailureLimit {
		if status, body := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
			"username": ownerName, "password": "wrong-password",
		}, ""); status != http.StatusUnauthorized {
			t.Fatalf("attempt %d after the window = %d %v", attempt+1, status, body)
		}
	}
	if status, _ := apiCall(t, base, http.MethodPost, loginPath, map[string]any{
		"username": ownerName, "password": "wrong-password",
	}, ""); status != http.StatusLocked {
		t.Fatalf("the sixth attempt after the window = %d, want 423", status)
	}

	clock.Advance(LoginLockDuration + time.Second)
	token := signIn(t, base, ownerName, ownerPassword)
	if status, body := apiCall(t, base, http.MethodPost, logoutPath, nil, token); status != http.StatusNoContent {
		t.Fatalf("logout = %d %v", status, body)
	}
	status, body := apiCall(t, base, http.MethodGet, sessionPath, nil, token)
	if status != http.StatusUnauthorized || body["error"].(map[string]any)["code"] != controlplane.CodeUnauthenticated {
		t.Fatalf("replaying a logged-out session = %d %v, want 401", status, body)
	}
}

// TestAccountDeleteRefusesOwnedObjects checks that the refusal lists what the
// account still owns and that a transfer lets the delete through (C-60, C-63).
func TestAccountDeleteRefusesOwnedObjects(t *testing.T) {
	owned := &fakeOwned{objects: map[string][]OwnedObject{}}
	service, _, _, base := startAPI(t, owned)
	ownerName, ownerPassword := bootstrapOwner(t, service)
	token := signIn(t, base, ownerName, ownerPassword)
	status, body := apiCall(t, base, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "bob", "role": string(write.RoleMember),
	}, token)
	if status != http.StatusCreated {
		t.Fatalf("create bob = %d %v", status, body)
	}
	bobToken := signIn(t, base, "bob", body["oneTimePassword"].(string))
	owned.setOwned("bob",
		OwnedObject{Kind: "Application", Namespace: "default", Name: "web"},
		OwnedObject{Kind: "Volume", Namespace: "default", Name: "data"},
	)
	status, body = apiCall(t, base, http.MethodDelete, "/api/v1/accounts/bob", nil, token)
	if status != http.StatusConflict {
		t.Fatalf("delete with owned objects = %d %v, want 409", status, body)
	}
	failure := body["error"].(map[string]any)
	if failure["code"] != controlplane.CodeAccountHasObjects {
		t.Fatalf("delete code = %v, want %s", failure["code"], controlplane.CodeAccountHasObjects)
	}
	objects, _ := failure["ownedObjects"].([]any)
	if len(objects) != 2 {
		t.Fatalf("ownedObjects = %v, want the two objects", failure["ownedObjects"])
	}
	first := objects[0].(map[string]any)
	if first["kind"] != "Application" || first["name"] != "web" || first["group"] != "kos.tsic.top" {
		t.Fatalf("first owned object = %v", first)
	}
	owned.setOwned("bob")
	if status, body := apiCall(t, base, http.MethodDelete, "/api/v1/accounts/bob", nil, token); status != http.StatusNoContent {
		t.Fatalf("delete after transfer = %d %v", status, body)
	}
	if status, _ := apiCall(t, base, http.MethodGet, sessionPath, nil, bobToken); status != http.StatusUnauthorized {
		t.Fatalf("the deleted account's session = %d, want 401", status)
	}
}

// TestDatabaseUnavailableStopsWrites covers the C-61 split at the HTTP gate:
// a signed-in read continues, a write and a login stop.
func TestDatabaseUnavailableStopsWrites(t *testing.T) {
	service, _, store, base := startAPI(t, &fakeOwned{})
	ownerName, ownerPassword := bootstrapOwner(t, service)
	token := signIn(t, base, ownerName, ownerPassword)
	if err := store.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	if status, body := apiCall(t, base, http.MethodGet, sessionPath, nil, token); status != http.StatusOK {
		t.Fatalf("a signed-in read with the database down = %d %v, want 200", status, body)
	}
	status, body := apiCall(t, base, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": "later", "role": string(write.RoleMember),
	}, token)
	if status != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != controlplane.CodeDatabaseUnavailable {
		t.Fatalf("a write with the database down = %d %v, want 503", status, body)
	}
	status, body = apiCall(t, base, http.MethodPost, loginPath, map[string]any{
		"username": ownerName, "password": ownerPassword,
	}, "")
	if status != http.StatusServiceUnavailable || body["error"].(map[string]any)["code"] != controlplane.CodeDatabaseUnavailable {
		t.Fatalf("a login with the database down = %d %v, want 503", status, body)
	}
}
