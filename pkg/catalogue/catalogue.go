// Package catalogue projects the application catalogue active extensions offer
// (C-24): each source contributes entries, an entry carries display
// information, version, dependencies, install parameters and a package digest,
// and its definition is written by the catalogue sync only when the package
// matches the digest and the dependencies are met. Removing a source removes its
// entries; the applications already installed from them stay, and their upgrade
// path is marked as missing its source.
package catalogue

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/app"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// Labels the catalogue sync writes on the definitions it creates (C-24). They
// are plain labels, not C-02 markers: they say which source and entry produced
// the definition, which is how the upgrade path is resolved.
const (
	// SourceLabel names the catalogue source of one definition.
	SourceLabel = "kos.tsic.top/catalogue-source"
	// EntryLabel names the entry inside that source.
	EntryLabel = "kos.tsic.top/catalogue-entry"
)

// Upgrade path states (C-24).
const (
	// UpgradeAvailable means the definition is still offered by an active
	// source.
	UpgradeAvailable = "Available"
	// UpgradeSourceMissing means the definition is no longer offered, so the
	// application keeps running but cannot be upgraded from the catalogue.
	UpgradeSourceMissing = "SourceMissing"
)

// Declarations reports the catalogue sources and capabilities active
// components contribute (C-24, C-21).
type Declarations interface {
	// CatalogueSources returns the declared sources.
	CatalogueSources() []extension.CatalogueSource
	// Capabilities returns the capability catalogue, baseline first.
	Capabilities() []extension.Capability
}

// Entry is one catalogue entry as the panel reads it (C-24).
type Entry struct {
	// Source is the source identity that offers the entry.
	Source string `json:"source"`
	// ID is the entry identity; it names the definition the entry installs.
	ID string `json:"id"`
	// Name is the display name.
	Name string `json:"name"`
	// Description is the display description.
	Description string `json:"description,omitempty"`
	// Icon is the display icon.
	Icon string `json:"icon,omitempty"`
	// Version is the package version.
	Version string `json:"version"`
	// Dependencies are the capability requirements the definition declares.
	Dependencies []v1beta1.CapabilityRequirement `json:"dependencies,omitempty"`
	// Config is the install parameter structure (C-32).
	Config []v1beta1.ConfigField `json:"config,omitempty"`
	// Digest is the package digest.
	Digest string `json:"digest"`
	// Definition is the application definition installing the entry creates.
	Definition string `json:"definition"`
	// Installable tells whether the entry may be installed now.
	Installable bool `json:"installable"`
	// Reason explains why the entry cannot be installed.
	Reason string `json:"reason,omitempty"`
}

// Upgrade is the upgrade path of one installed application (C-24, C-31).
type Upgrade struct {
	// Namespace is the application's namespace.
	Namespace string `json:"namespace"`
	// Name is the application's name.
	Name string `json:"name"`
	// Definition is the definition the application was installed from.
	Definition string `json:"definition"`
	// State is Available or SourceMissing.
	State string `json:"state"`
}

// Service is the catalogue domain over the declarations and the cluster.
type Service struct {
	declarations Declarations
	dynamic      dynamic.Interface
}

// New builds the catalogue domain.
func New(declarations Declarations, client dynamic.Interface) *Service {
	return &Service{declarations: declarations, dynamic: client}
}

// Entries returns every entry the active sources offer, judged for install. An
// entry whose package does not match its digest or whose required capabilities
// are missing stays visible and carries the reason it cannot be installed
// (C-24).
func (s *Service) Entries() []Entry {
	available := catalogueCapabilities(s.declarations.Capabilities())
	entries := []Entry{}
	for _, source := range s.declarations.CatalogueSources() {
		for _, offer := range source.Entries {
			entries = append(entries, judgeEntry(source.ID, offer, available))
		}
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].Source != entries[right].Source {
			return entries[left].Source < entries[right].Source
		}
		return entries[left].ID < entries[right].ID
	})
	return entries
}

// judgeEntry validates one offer and renders it as the catalogue reads it.
func judgeEntry(sourceID string, offer extension.CatalogueEntry, available []app.Capability) Entry {
	entry := Entry{
		Source:       sourceID,
		ID:           offer.ID,
		Name:         offer.Display.Name,
		Description:  offer.Display.Description,
		Icon:         offer.Display.Icon,
		Version:      offer.Definition.Package.Version,
		Dependencies: offer.Definition.Requires,
		Config:       offer.Definition.Config,
		Digest:       offer.Definition.Package.Digest,
		Definition:   offer.ID,
		Installable:  true,
	}
	if err := app.ValidateDefinition(offer.Definition); err != nil {
		entry.Installable = false
		entry.Reason = "包内容与摘要不一致或定义不合法：" + err.Error()
		return entry
	}
	comparison, err := app.Compare(offer.Definition.Requires, available)
	if err != nil {
		entry.Installable = false
		entry.Reason = "依赖声明无法判定：" + err.Error()
		return entry
	}
	if len(comparison.Missing) > 0 {
		entry.Installable = false
		entry.Reason = "缺少依赖：" + strings.Join(comparison.Missing, ", ")
	}
	return entry
}

// Sync writes the definitions the active sources offer (C-24). An entry is
// written only when it is installable; an entry that stopped being installable
// leaves the definition it wrote earlier in place, so installed applications
// keep the package they were installed from.
func (s *Service) Sync(ctx context.Context) error {
	if s.dynamic == nil {
		return nil
	}
	for _, entry := range s.Entries() {
		if !entry.Installable {
			continue
		}
		offer, found := s.offer(entry.Source, entry.ID)
		if !found {
			continue
		}
		if err := s.writeDefinition(ctx, entry, offer); err != nil {
			return err
		}
	}
	return nil
}

// offer resolves the declared entry behind one read-model entry.
func (s *Service) offer(sourceID, entryID string) (extension.CatalogueEntry, bool) {
	for _, source := range s.declarations.CatalogueSources() {
		if source.ID != sourceID {
			continue
		}
		for _, offer := range source.Entries {
			if offer.ID == entryID {
				return offer, true
			}
		}
	}
	return extension.CatalogueEntry{}, false
}

// writeDefinition applies the definition of one entry as the catalogue sync. A
// definition that already carries the same spec is left alone, so a repeated
// sync writes nothing.
func (s *Service) writeDefinition(ctx context.Context, entry Entry, offer extension.CatalogueEntry) error {
	current, err := s.dynamic.Resource(applicationDefinitions()).Get(ctx, entry.Definition, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	definition := definitionObject(entry, offer)
	if err == nil && sameSpec(current, definition) {
		return nil
	}
	writer := &write.Writer{Dynamic: s.dynamic, Resources: kernelResources()}
	_, err = writer.Apply(ctx, write.SubjectCatalogueSync, write.Session{}, write.PartSpec, definition)
	return err
}

// definitionObject renders the definition one entry installs.
func definitionObject(entry Entry, offer extension.CatalogueEntry) *unstructured.Unstructured {
	definition := &v1beta1.ApplicationDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: api.Group + "/" + v1beta1.GroupVersion.Version,
			Kind:       "ApplicationDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: entry.Definition,
			Labels: map[string]string{
				SourceLabel: entry.Source,
				EntryLabel:  entry.ID,
			},
		},
		Spec: offer.Definition,
	}
	document, err := runtime.DefaultUnstructuredConverter.ToUnstructured(definition)
	if err != nil {
		return &unstructured.Unstructured{Object: map[string]any{}}
	}
	return &unstructured.Unstructured{Object: document}
}

// sameSpec reports whether a stored definition already carries the desired spec.
func sameSpec(stored, desired *unstructured.Unstructured) bool {
	currentSpec, _, err := unstructured.NestedMap(stored.Object, "spec")
	if err != nil {
		return false
	}
	desiredSpec, _, err := unstructured.NestedMap(desired.Object, "spec")
	if err != nil {
		return false
	}
	currentJSON, currentErr := json.Marshal(currentSpec)
	desiredJSON, desiredErr := json.Marshal(desiredSpec)
	if currentErr != nil || desiredErr != nil {
		return false
	}
	return string(currentJSON) == string(desiredJSON)
}

// Upgrades returns the upgrade path of every installed application whose
// definition came from the catalogue (C-24, C-31). A definition an active
// source still offers is Available; one whose source is gone, or whose
// definition no longer exists, is SourceMissing.
func (s *Service) Upgrades(ctx context.Context) ([]Upgrade, *controlplane.Error) {
	if s.dynamic == nil {
		return []Upgrade{}, nil
	}
	active := map[string]bool{}
	for _, source := range s.declarations.CatalogueSources() {
		active[source.ID] = true
	}
	definitions, err := s.dynamic.Resource(applicationDefinitions()).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, unavailable()
	}
	sources := make(map[string]string, len(definitions.Items))
	for _, definition := range definitions.Items {
		sources[definition.GetName()] = definition.GetLabels()[SourceLabel]
	}
	applications, err := s.dynamic.Resource(applications()).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, unavailable()
	}
	upgrades := make([]Upgrade, 0, len(applications.Items))
	for _, application := range applications.Items {
		ref, _, err := unstructured.NestedString(application.Object, "spec", "definitionRef")
		if err != nil || ref == "" {
			continue
		}
		source, fromCatalogue := sources[ref]
		if fromCatalogue && source == "" {
			// A definition installed outside the catalogue has no catalogue
			// upgrade path to report.
			continue
		}
		state := UpgradeAvailable
		if !fromCatalogue || !active[source] {
			state = UpgradeSourceMissing
		}
		upgrades = append(upgrades, Upgrade{
			Namespace:  application.GetNamespace(),
			Name:       application.GetName(),
			Definition: ref,
			State:      state,
		})
	}
	sort.Slice(upgrades, func(left, right int) bool {
		if upgrades[left].Namespace != upgrades[right].Namespace {
			return upgrades[left].Namespace < upgrades[right].Namespace
		}
		return upgrades[left].Name < upgrades[right].Name
	})
	return upgrades, nil
}

// catalogueCapabilities renders the registry capability list for the C-11
// comparison.
func catalogueCapabilities(capabilities []extension.Capability) []app.Capability {
	available := make([]app.Capability, 0, len(capabilities))
	for _, capability := range capabilities {
		available = append(available, app.Capability{Name: capability.Name, Version: capability.Version})
	}
	return available
}

// applicationDefinitions is the served resource of the definitions the catalogue
// writes.
func applicationDefinitions() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: "applicationdefinitions",
	}
}

// applications is the served resource of installed applications.
func applications() schema.GroupVersionResource {
	return schema.GroupVersionResource{
		Group: api.Group, Version: v1beta1.GroupVersion.Version, Resource: "applications",
	}
}

// kernelResources maps the kernel kinds the catalogue touches to their resource
// names.
func kernelResources() map[schema.GroupKind]string {
	return map[schema.GroupKind]string{
		{Group: api.Group, Kind: "ApplicationDefinition"}: "applicationdefinitions",
	}
}

// unavailable is the failure of a cluster read (C-13).
func unavailable() *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "读取目录失败，请稍后重试",
		Stage:       controlplane.StageRead,
		Retryable:   true,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// API is the HTTP surface of the catalogue.
type API struct {
	service *Service
}

// NewAPI builds the HTTP surface over the domain.
func NewAPI(service *Service) *API { return &API{service: service} }

// Handler serves the catalogue routes. It expects the identity middleware in
// front of it: every request carries a resolved session (C-63).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/catalogue/entries", a.listEntries)
	mux.HandleFunc("GET /api/v1/catalogue/upgrades", a.listUpgrades)
	return mux
}

// listEntries returns every entry the active sources offer.
func (a *API) listEntries(w http.ResponseWriter, r *http.Request) {
	if _, ok := identity.FromContext(r.Context()); !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": a.service.Entries()})
}

// listUpgrades returns the upgrade path of every catalogue-installed
// application.
func (a *API) listUpgrades(w http.ResponseWriter, r *http.Request) {
	if _, ok := identity.FromContext(r.Context()); !ok {
		controlplane.WriteError(w, unauthenticated())
		return
	}
	upgrades, failure := a.service.Upgrades(r.Context())
	if failure != nil {
		controlplane.WriteError(w, failure)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": upgrades})
}

// unauthenticated is the answer to a request without a session (C-63).
func unauthenticated() *controlplane.Error {
	return &controlplane.Error{
		Code:               controlplane.CodeUnauthenticated,
		Message:            "会话无效或已过期，请重新登录",
		Stage:              controlplane.StageAuthorize,
		RequiresUserAction: true,
		SideEffects:        controlplane.SideEffectsNone,
	}
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
