package harness

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/access"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// Access is one booted system for the role cases: the cluster, the control
// plane behind the account surface, the control-plane database and the audit
// stream, with C-59's policy and C-14's audit sink wired where the real binary
// wires them.
type Access struct {
	// Cluster is the API server the objects live on.
	Cluster *Cluster
	// ControlPlane is the domain API.
	ControlPlane *ControlPlane
	// Identity is the account and session surface in front of it.
	Identity *Identity
	// Logs is everything the control plane logged during the case; C-14's
	// credential boundary reads it.
	Logs *bytes.Buffer
}

// StartAccess boots the account surface in front of the domain API with the
// C-59 policy over this cluster. extensions is the declared-role seam (C-18);
// nil means no extension class is declared. configure may change the domain API
// configuration before it starts.
func (c *Cluster) StartAccess(t *testing.T, extensions access.ExtensionRoles, configure func(*controlplane.Config)) *Access {
	t.Helper()
	clock := NewClock(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	databasePath := filepath.Join(t.TempDir(), "kos.db")
	store, err := identity.Open(context.Background(), identity.OpenOptions{Path: databasePath, Now: clock.Now})
	if err != nil {
		t.Fatalf("open identity database: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	logs := &bytes.Buffer{}
	config := controlplane.Config{
		Dynamic:    c.Dynamic,
		Authorizer: identity.Authorizer{Policy: &access.Policy{Objects: controlplane.NewClusterSource(c.Dynamic), Extensions: extensions}},
		Auditor:    identity.AuditTrail{Store: store},
		Log:        slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:        clock.Now,
	}
	if configure != nil {
		configure(&config)
	}
	server, err := controlplane.New(config)
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	service := identity.New(store, identity.Config{
		Now:     clock.Now,
		Owned:   identity.ClusterOwnedObjects{Dynamic: c.Dynamic},
		Auditor: identity.AuditTrail{Store: store},
	})
	api := identity.NewAPI(service)
	httpServer := httptest.NewServer(api.Handler(server.Handler()))
	t.Cleanup(func() {
		httpServer.Close()
		server.Close()
	})
	return &Access{
		Cluster:      c,
		ControlPlane: &ControlPlane{Server: server, URL: httpServer.URL},
		Identity: &Identity{
			Store:        store,
			Service:      service,
			API:          api,
			HTTP:         &HTTP{URL: httpServer.URL},
			Clock:        clock,
			DatabasePath: databasePath,
		},
		Logs: logs,
	}
}

// Call sends one authenticated request.
func (a *Access) Call(t *testing.T, token, method, path string, body any, headers map[string]string) (int, map[string]any) {
	t.Helper()
	if headers == nil {
		headers = map[string]string{}
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return a.Identity.HTTP.Call(t, method, path, body, headers)
}

// Owner creates the first Owner account and returns a usable token.
func (a *Access) Owner(t *testing.T, username, password string) string {
	t.Helper()
	oneTime := a.Identity.BootstrapOwner(t, username)
	return a.ChangeablePassword(t, username, oneTime, password)
}

// Account creates a further account with a role and returns a usable token.
func (a *Access) Account(t *testing.T, ownerToken, username, password string, role write.Role) string {
	t.Helper()
	status, body := a.Call(t, ownerToken, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": username, "role": string(role),
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create account %s: status %d, body %v", username, status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	if oneTime == "" {
		t.Fatalf("create account %s returned no one-time password: %v", username, body)
	}
	return a.ChangeablePassword(t, username, oneTime, password)
}

// ChangeablePassword replaces a one-time password and returns a token issued
// after the change, which is the token every later request uses.
func (a *Access) ChangeablePassword(t *testing.T, username, current, next string) string {
	t.Helper()
	token, _ := a.Identity.SignIn(t, username, current)
	a.Identity.ChangePassword(t, token, current, next)
	token, _ = a.Identity.SignIn(t, username, next)
	return token
}

// Bearer is the header carrying a token.
func Bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// Contains reports whether the control plane logged one string.
func (a *Access) Contains(text string) bool {
	return strings.Contains(a.Logs.String(), text)
}
