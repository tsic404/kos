package harness

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/catalogue"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/settings"
)

// RegistrySource is the contribution projection every surface of the shell
// reads. One API process outlives any single install pass, so the projection is
// replaced in place whenever a pass finishes (C-28).
type RegistrySource struct {
	mu       sync.RWMutex
	registry *extension.Registry
}

// Publish replaces the projection with the one an install pass produced.
func (r *RegistrySource) Publish(registry *extension.Registry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.registry = registry
}

// current reads the published projection.
func (r *RegistrySource) current() *extension.Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.registry
}

// ObjectClass resolves one served extension class (C-19).
func (r *RegistrySource) ObjectClass(group, kind string) (controlplane.ExtensionObjectClass, bool) {
	return r.current().ObjectClass(group, kind)
}

// Settings returns the loaded setting items.
func (r *RegistrySource) Settings() []extension.Setting {
	return r.current().Settings()
}

// Setting resolves one setting item.
func (r *RegistrySource) Setting(id string) (extension.Setting, bool) {
	return r.current().Setting(id)
}

// CatalogueSources returns the loaded catalogue sources.
func (r *RegistrySource) CatalogueSources() []extension.CatalogueSource {
	return r.current().CatalogueSources()
}

// Capabilities returns the capability catalogue.
func (r *RegistrySource) Capabilities() []extension.Capability {
	return r.current().Capabilities()
}

// PreferenceKeys returns the preference keys one extension declared.
func (r *RegistrySource) PreferenceKeys(identifier string) ([]string, bool) {
	return r.current().PreferenceKeys(identifier)
}

// Shell is the control plane the panel talks to: the account middleware guards
// the resource API, the settings domain and the catalogue over one database and
// one cluster (C-12, C-23, C-24, C-27, C-63).
type Shell struct {
	// Cluster is the acceptance cluster.
	Cluster *Cluster
	// Accounts is the account surface; its HTTP client reaches the shell.
	Accounts *Identity
	// Registry is the projection install passes publish into.
	Registry *RegistrySource
	// Server is the resource API.
	Server *controlplane.Server
	// Settings is the settings domain.
	Settings *settings.Service
	// Catalogue is the catalogue domain.
	Catalogue *catalogue.Service
}

// StartShell serves the composed control plane over the cluster, with no
// contributions active until a case publishes an install result.
func (c *Cluster) StartShell(t *testing.T) *Shell {
	t.Helper()
	registry := &RegistrySource{}
	surface := OpenIdentity(t, nil, func(config *identity.Config) {
		config.Preferences = registry
	})
	server, err := controlplane.New(controlplane.Config{
		Dynamic:    c.Dynamic,
		Extensions: registry,
		Authorizer: identity.Authorizer{},
	})
	if err != nil {
		t.Fatalf("build control plane: %v", err)
	}
	t.Cleanup(server.Close)

	settingsService := settings.New(registry, surface.Store)
	catalogueService := catalogue.New(registry, c.Dynamic)
	settingsHandler := settings.NewAPI(settingsService).Handler()
	catalogueHandler := catalogue.NewAPI(catalogueService).Handler()

	mux := http.NewServeMux()
	mux.Handle("/api/v1/settings", settingsHandler)
	mux.Handle("/api/v1/settings/", settingsHandler)
	mux.Handle("/api/v1/catalogue/", catalogueHandler)
	mux.Handle("/", server.Handler())
	surface.Serve(t, mux)

	return &Shell{
		Cluster:   c,
		Accounts:  surface,
		Registry:  registry,
		Server:    server,
		Settings:  settingsService,
		Catalogue: catalogueService,
	}
}

// Use publishes the projection one install pass produced; every surface reads
// the contributions active now.
func (s *Shell) Use(installation *extension.Installation) {
	s.Registry.Publish(installation.Registry())
}

// Retention is the sink an install pass settles preference data through.
func (s *Shell) Retention() extension.PreferenceRetention { return s.Accounts.Service }

// SignInOwner bootstraps the first Owner, replaces its one-time password and
// returns a session token.
func (s *Shell) SignInOwner(t *testing.T, username, password string) string {
	t.Helper()
	oneTime := s.Accounts.BootstrapOwner(t, username)
	token, _ := s.Accounts.SignIn(t, username, oneTime)
	s.Accounts.ChangePassword(t, token, oneTime, password)
	return token
}

// Call sends one authenticated request to the shell.
func (s *Shell) Call(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	headers := map[string]string{"Authorization": "Bearer " + token}
	return s.Accounts.HTTP.Call(t, method, path, body, headers)
}

// SyncCatalogue writes the definitions the active sources offer.
func (s *Shell) SyncCatalogue(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Catalogue.Sync(ctx); err != nil {
		t.Fatalf("catalogue sync: %v", err)
	}
}
