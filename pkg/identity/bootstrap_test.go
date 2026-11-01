package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/write"
)

// bootstrapHarness is one control-plane database a case can restart.
type bootstrapHarness struct {
	service *Service
	clock   *testClock
	store   *Store
	path    string
}

// openBootstrap opens the database at path and the service over it.
func openBootstrap(t *testing.T, path string, clock *testClock) *bootstrapHarness {
	t.Helper()
	store, err := Open(context.Background(), OpenOptions{Path: path, Now: clock.Now})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return &bootstrapHarness{
		service: New(store, Config{Now: clock.Now, Owned: &fakeOwned{}, Auditor: AuditTrail{Store: store}}),
		clock:   clock,
		store:   store,
		path:    path,
	}
}

// newBootstrapHarness opens a private database.
func newBootstrapHarness(t *testing.T) *bootstrapHarness {
	t.Helper()
	return openBootstrap(t, filepath.Join(t.TempDir(), "kos.db"), newTestClock())
}

// TestClaimWindowDeliversThePasswordOnce covers C-68's one delivery: the
// external form prepares the Owner without a password, the window serves it
// once, and a second claim returns none.
func TestClaimWindowDeliversThePasswordOnce(t *testing.T) {
	harness := newBootstrapHarness(t)
	service := harness.service
	ctx := context.Background()

	// Without a window nothing is delivered.
	if _, err := service.ClaimFirstOwner(ctx); !errors.Is(err, ErrBootstrapClosed) {
		t.Fatalf("claim before the window opened = %v, want a closed window", err)
	}

	// The external form creates the Owner, and its password is nobody's until
	// the claim: the account exists and cannot log in.
	prepared, err := service.PrepareFirstOwner(ctx, "owner")
	if err != nil {
		t.Fatalf("prepare the first Owner: %v", err)
	}
	if !prepared.Created || prepared.OneTimePassword != "" {
		t.Fatalf("prepare = %+v, want a created account with no password handed out", prepared)
	}
	if _, err := service.Login(ctx, "owner", "any-password"); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("login before the claim = %v, want a plain login failure", err)
	}

	if err := service.OpenBootstrapWindow(ctx); err != nil {
		t.Fatalf("open the window: %v", err)
	}
	opened, err := harness.store.BootstrapWindow(ctx)
	if err != nil {
		t.Fatalf("read the window: %v", err)
	}
	if window := opened.Deadline.Sub(opened.OpenedAt); window != BootstrapWindowLength {
		t.Fatalf("window length = %s, want %s", window, BootstrapWindowLength)
	}

	claimed, err := service.ClaimFirstOwner(ctx)
	if err != nil {
		t.Fatalf("claim inside the window: %v", err)
	}
	if claimed.Username != "owner" || claimed.OneTimePassword == "" {
		t.Fatalf("claim = %+v, want the first Owner and its one-time password", claimed)
	}
	login, err := service.Login(ctx, claimed.Username, claimed.OneTimePassword)
	if err != nil {
		t.Fatalf("log in with the claimed password: %v", err)
	}
	if !login.Session.MustChangePassword {
		t.Fatal("the claimed password did not require a change at first login")
	}

	// The second claim is refused and carries no password.
	if _, err := service.ClaimFirstOwner(ctx); !errors.Is(err, ErrBootstrapClosed) {
		t.Fatalf("second claim = %v, want a closed window", err)
	}
}

// TestClaimWindowDeadlineSurvivesRestartAndReinstall covers the two ways the
// window must not reopen: a control-plane restart and a reinstall that kept the
// database (C-68).
func TestClaimWindowDeadlineSurvivesRestartAndReinstall(t *testing.T) {
	harness := newBootstrapHarness(t)
	ctx := context.Background()
	if _, err := harness.service.PrepareFirstOwner(ctx, "owner"); err != nil {
		t.Fatalf("prepare the first Owner: %v", err)
	}
	if err := harness.service.OpenBootstrapWindow(ctx); err != nil {
		t.Fatalf("open the window: %v", err)
	}
	opened, err := harness.store.BootstrapWindow(ctx)
	if err != nil {
		t.Fatalf("read the window: %v", err)
	}

	// A restart reads the same deadline instead of starting a new one.
	harness.clock.Advance(10 * time.Minute)
	if err := harness.store.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}
	restarted := openBootstrap(t, harness.path, harness.clock)
	afterRestart, err := restarted.store.BootstrapWindow(ctx)
	if err != nil {
		t.Fatalf("read the window after the restart: %v", err)
	}
	if !afterRestart.Deadline.Equal(opened.Deadline) {
		t.Fatalf("deadline after the restart = %s, want the recorded %s", afterRestart.Deadline, opened.Deadline)
	}

	// A reinstall that kept the database does not open a second window.
	harness.clock.Advance(BootstrapWindowLength)
	if err := restarted.service.OpenBootstrapWindow(ctx); err != nil {
		t.Fatalf("reopen the window: %v", err)
	}
	afterReinstall, err := restarted.store.BootstrapWindow(ctx)
	if err != nil {
		t.Fatalf("read the window after the reinstall: %v", err)
	}
	if !afterReinstall.Deadline.Equal(opened.Deadline) || !afterReinstall.OpenedAt.Equal(opened.OpenedAt) {
		t.Fatalf("reinstall moved the window to %+v, want %+v", afterReinstall, opened)
	}
	if _, err := restarted.service.ClaimFirstOwner(ctx); !errors.Is(err, ErrBootstrapClosed) {
		t.Fatalf("claim after the deadline = %v, want a closed window", err)
	}
}

// TestBootstrapClaimServesOnlyTheControlPlaneHost covers C-68's source rule:
// the transport address decides, and a refusal carries no password.
func TestBootstrapClaimServesOnlyTheControlPlaneHost(t *testing.T) {
	harness := newBootstrapHarness(t)
	ctx := context.Background()
	if _, err := harness.service.PrepareFirstOwner(ctx, "owner"); err != nil {
		t.Fatalf("prepare the first owner: %v", err)
	}
	if err := harness.service.OpenBootstrapWindow(ctx); err != nil {
		t.Fatalf("open the window: %v", err)
	}
	handler := NewAPI(harness.service).Handler(nil)

	// A request from another host is refused and leaves the window open.
	remote := httptest.NewRequest(http.MethodPost, BootstrapOwnerPath, nil)
	remote.RemoteAddr = "192.0.2.10:41234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, remote)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("claim from another host = %d, want 403", recorder.Code)
	}
	if body := recorder.Body.String(); strings.Contains(body, "oneTimePassword") {
		t.Fatalf("the refused claim carried a password: %s", body)
	}

	// The control-plane host itself claims the password once.
	server := httptest.NewServer(handler)
	defer server.Close()
	status, body := apiCall(t, server.URL, http.MethodPost, BootstrapOwnerPath, nil, "")
	if status != http.StatusOK {
		t.Fatalf("claim from the control-plane host = %d %v", status, body)
	}
	password, _ := body["oneTimePassword"].(string)
	if body["username"] != "owner" || password == "" {
		t.Fatalf("claim answered %v, want the first Owner and its password", body)
	}

	// The second claim is refused and carries no password.
	status, second := apiCall(t, server.URL, http.MethodPost, BootstrapOwnerPath, nil, "")
	if status != http.StatusForbidden {
		t.Fatalf("second claim = %d %v, want 403", status, second)
	}
	if code := second["error"].(map[string]any)["code"]; code != controlplane.CodeBootstrapClosed {
		t.Fatalf("second claim code = %v, want %q", code, controlplane.CodeBootstrapClosed)
	}
	if _, found := second["error"].(map[string]any)["oneTimePassword"]; found {
		t.Fatalf("the refused second claim carried a password: %v", second)
	}

	// Every attempt reaches the audit trail, and none of them records the
	// password (C-14).
	entries, err := harness.store.AuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	results := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Verb != "update" || !strings.HasSuffix(entry.Object, "/Account/owner") {
			t.Fatalf("audit entry %+v, want the first Owner's update", entry)
		}
		// The claim route carries no session, so every record names the control
		// plane acting on its own (C-14).
		if entry.Actor != string(write.SubjectAPIProcess) {
			t.Fatalf("audit actor = %q, want %q", entry.Actor, write.SubjectAPIProcess)
		}
		results = append(results, entry.Result)
	}
	want := []string{controlplane.ResultRejected, controlplane.ResultAccepted, controlplane.ResultRejected}
	if len(results) != len(want) {
		t.Fatalf("audit results = %v, want %v", results, want)
	}
	for index := range want {
		if results[index] != want[index] {
			t.Fatalf("audit results = %v, want %v", results, want)
		}
	}
}

// TestBootstrapClaimSkipsAnUnnamedAccount covers the other audit edge: a window
// with no prepared first Owner has no account to name, and the trail records no
// account rather than one with an empty name (C-14).
func TestBootstrapClaimSkipsAnUnnamedAccount(t *testing.T) {
	harness := newBootstrapHarness(t)
	ctx := context.Background()
	if err := harness.service.OpenBootstrapWindow(ctx); err != nil {
		t.Fatalf("open the window: %v", err)
	}
	server := httptest.NewServer(NewAPI(harness.service).Handler(nil))
	defer server.Close()

	status, body := apiCall(t, server.URL, http.MethodPost, BootstrapOwnerPath, nil, "")
	if status != http.StatusNotFound {
		t.Fatalf("claim without a first Owner = %d %v, want 404", status, body)
	}
	entries, err := harness.store.AuditEntries(ctx, 10)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("audit trail = %+v, want no record for an account that does not exist", entries)
	}
}
