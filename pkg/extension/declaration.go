package extension

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/api/v1beta1"
	"github.com/tsic404/kos/pkg/write"
)

// Contribution class names: the unit of activation. One class fails without
// taking the others down (C-28).
const (
	ClassObjectClasses      = "objectClasses"
	ClassControllers        = "controllers"
	ClassCapabilities       = "capabilities"
	ClassPages              = "pages"
	ClassSettings           = "settings"
	ClassCatalogueSources   = "catalogueSources"
	ClassDiscoveryAdapters  = "discoveryAdapters"
	ClassDiagnosticAdapters = "diagnosticAdapters"
	ClassPreferences        = "preferences"
)

// Setting scopes and writers a declaration names (C-23).
const (
	SettingScopeCluster = "Cluster"
	SettingScopeNode    = "Node"
	SettingScopeUser    = "User"

	SettingWriterController = "Controller"
	SettingWriterUser       = "User"
	SettingWriterKernel     = "Kernel"
)

// Declaration is what an extension package declares (C-18). The kernel loads
// contributions from it and never identifies an extension by name (C-17).
type Declaration struct {
	// Identifier is the extension's identity across versions.
	Identifier string `json:"identifier"`
	// Version is the package version this declaration belongs to.
	Version string `json:"version"`
	// FrameworkVersion is the target framework API version range. It is
	// carried, not judged: framework compatibility is a separate contract.
	FrameworkVersion string `json:"frameworkVersion,omitempty"`
	// APIGroup is the extension's own API group. Every object class it
	// declares belongs to it, and it is never the kernel group (C-19).
	APIGroup string `json:"apiGroup,omitempty"`
	// Contributions is the nine-class contribution list.
	Contributions Contributions `json:"contributions,omitempty"`
	// Dependencies are the edges that gate activation (C-28).
	Dependencies Dependencies `json:"dependencies,omitempty"`
	// Permissions are the declared read scopes, control-plane roles and object
	// write roles (C-18). Carried, not judged: C-59 owns enforcement.
	Permissions []Permission `json:"permissions,omitempty"`
	// Configuration is the component configuration structure and the status
	// interface extension objects expose (C-18). Carried, not judged.
	Configuration Configuration `json:"configuration,omitempty"`
	// Retention is the uninstall retention semantics of each contribution
	// (C-18). Carried, not judged: uninstall is a separate contract.
	Retention []Retention `json:"retention,omitempty"`
}

// Contributions is the contribution list C-18 fixes: nine classes, each
// loaded on its own.
type Contributions struct {
	ObjectClasses      []ObjectClass           `json:"objectClasses,omitempty"`
	Controllers        []Controller            `json:"controllers,omitempty"`
	Capabilities       CapabilityContributions `json:"capabilities,omitempty"`
	Pages              []Page                  `json:"pages,omitempty"`
	Settings           []Setting               `json:"settings,omitempty"`
	CatalogueSources   []CatalogueSource       `json:"catalogueSources,omitempty"`
	DiscoveryAdapters  []DiscoveryAdapter      `json:"discoveryAdapters,omitempty"`
	DiagnosticAdapters []DiagnosticAdapter     `json:"diagnosticAdapters,omitempty"`
	Preferences        []PreferenceSchema      `json:"preferences,omitempty"`
}

// GroupKind names one object class; an empty group is the run kernel's core
// group.
type GroupKind struct {
	Group string `json:"group,omitempty"`
	Kind  string `json:"kind"`
}

// Schema returns the class as the key the write path uses.
func (g GroupKind) Schema() schema.GroupKind {
	return schema.GroupKind{Group: g.Group, Kind: g.Kind}
}

// String renders the class for messages.
func (g GroupKind) String() string {
	if g.Group == "" {
		return g.Kind
	}
	return g.Group + "/" + g.Kind
}

// ObjectClass is an extension object class (C-19). It belongs to the
// extension's own group and is served only while its component is active.
type ObjectClass struct {
	Group      string   `json:"group"`
	Kind       string   `json:"kind"`
	Resource   string   `json:"resource"`
	Versions   []string `json:"versions"`
	Namespaced bool     `json:"namespaced,omitempty"`
	// References are the dotted field paths inside an object of this class that
	// carry declarative references to other objects (C-19). Only object fields
	// are followed: bracket indexes (items[0]) and numeric segments (items.0)
	// are rejected, and the kernel does not interpret the rest of the spec.
	References []string `json:"references,omitempty"`
}

// GroupKind returns the class identity.
func (c ObjectClass) GroupKind() GroupKind {
	return GroupKind{Group: c.Group, Kind: c.Kind}
}

// Controller is a controller contribution and the write scope it was granted
// (C-18, C-20).
type Controller struct {
	ID string `json:"id"`
	// Manages lists the declared object classes this controller serves.
	Manages []GroupKind `json:"manages,omitempty"`
	// Scope is the write scope declared for it.
	Scope WriteScope `json:"scope,omitempty"`
}

// WriteScope is the declared reach of an extension controller: API groups,
// downstream kinds and namespaces (C-04, C-18, C-20).
type WriteScope struct {
	Groups     []string    `json:"groups,omitempty"`
	Kinds      []GroupKind `json:"kinds,omitempty"`
	Namespaces []string    `json:"namespaces,omitempty"`
}

// Admit converts the declared scope into the allocation the write path checks.
func (s WriteScope) Admit() write.Scope {
	scope := write.Scope{
		Groups:     map[string]bool{},
		Kinds:      map[schema.GroupKind]bool{},
		Namespaces: map[string]bool{},
	}
	for _, group := range s.Groups {
		scope.Groups[group] = true
	}
	for _, kind := range s.Kinds {
		scope.Kinds[schema.GroupKind{Group: kind.Group, Kind: kind.Kind}] = true
	}
	for _, namespace := range s.Namespaces {
		scope.Namespaces[namespace] = true
	}
	return scope
}

// Permission is one declared permission (C-18): what a contribution may read,
// the control-plane role it needs, and the roles that may write the extension's
// objects. The kernel carries it here; C-59 judges it.
type Permission struct {
	// Contribution names the page or object class the entry belongs to.
	Contribution string `json:"contribution"`
	// ReadScope is the API groups and kinds the contribution reads.
	ReadScope ReadScope `json:"readScope,omitempty"`
	// RequiredRole is the control-plane session role it needs.
	RequiredRole string `json:"requiredRole,omitempty"`
	// ObjectWriters lists the session roles that may write the extension's
	// objects.
	ObjectWriters []write.Role `json:"objectWriters,omitempty"`
}

// ReadScope is the read side of a declared permission: API groups and kinds.
type ReadScope struct {
	Groups []string    `json:"groups,omitempty"`
	Kinds  []GroupKind `json:"kinds,omitempty"`
}

// Configuration is the declared component configuration structure and the
// status interface its objects expose (C-18).
type Configuration struct {
	// Schema is the extension's configuration structure, carried verbatim.
	Schema map[string]any `json:"schema,omitempty"`
	// StatusWriters lists the session roles that may write extension object
	// status.
	StatusWriters []write.Role `json:"statusWriters,omitempty"`
	// Conditions are the condition types the extension's objects carry (C-07).
	Conditions []string `json:"conditions,omitempty"`
}

// Retention is the declared fate of one contribution's data when its component
// is uninstalled (C-18, C-29).
type Retention struct {
	// Resource names the contribution the entry covers.
	Resource string `json:"resource"`
	// Keep tells whether the data survives the uninstall.
	Keep bool `json:"keep"`
	// ExternalReference names data held outside the cluster, which KOS never
	// deletes.
	ExternalReference string `json:"externalReference,omitempty"`
}

// CapabilityContributions is the capability contribution class: the capability
// entries a component contributes together with the provider registration C-47
// derives from them (C-18).
type CapabilityContributions struct {
	// Items are the contributed capability entries.
	Items []Capability `json:"items,omitempty"`
	// StorageClasses are the storage classes the provider supplies (C-47).
	StorageClasses []string `json:"storageClasses,omitempty"`
	// SelfReported is the provider's own conformance result; it is a
	// declaration and never verification (C-10).
	SelfReported []SelfReport `json:"selfReported,omitempty"`
}

// Capability is a capability an extension contributes. A new capability carries
// the extension identifier as a prefix; a kernel baseline name is contributed as
// it is (C-09, C-21).
type Capability struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// Scope is where the capability's guarantee holds (C-09). A baseline
	// capability may omit it and take the kernel baseline's value; an
	// extension capability must declare one.
	// +optional
	Scope v1beta1.CapabilityScope `json:"scope,omitempty"`
	// Parameters is the parameter structure the capability accepts (C-09).
	Parameters []v1beta1.CapabilityParameter `json:"parameters,omitempty"`
	// Guarantees are the boolean and enum values the capability promises
	// (C-09).
	Guarantees []v1beta1.CapabilityGuarantee `json:"guarantees,omitempty"`
	// CaseSet names the conformance case set (C-09).
	CaseSet string `json:"caseSet,omitempty"`
	// CaseSetDigest pins the case set contents (C-10).
	CaseSetDigest string `json:"caseSetDigest,omitempty"`
}

// SelfReport is one conformance result a component declares about itself
// (C-10).
type SelfReport struct {
	Capability  string   `json:"capability"`
	Version     string   `json:"version"`
	FailedCases []string `json:"failedCases,omitempty"`
}

// Page is a page and its actions (C-22). A page declares where the shell shows
// it, who may see it, the items it displays with their source types (C-53), and
// the actions that write through the control plane. A read-only page states why
// and carries no action.
type Page struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Location       string     `json:"location"`
	RequiredRole   string     `json:"requiredRole,omitempty"`
	ReadOnly       bool       `json:"readOnly,omitempty"`
	ReadOnlyReason string     `json:"readOnlyReason,omitempty"`
	Items          []PageItem `json:"items,omitempty"`
	Actions        []Action   `json:"actions,omitempty"`
}

// Source types a page item declares (C-53): the shell renders every item by the
// type its source belongs to, and applies one freshness bound to all of them
// (C-13).
const (
	// PageSourceObject reads a cluster object or its projection.
	PageSourceObject = "object"
	// PageSourceComputed reads a value the control plane computes from objects.
	PageSourceComputed = "computed"
	// PageSourceMetric reads a resource metric inside its sampling window.
	PageSourceMetric = "metric"
	// PageSourceProbe reads the result of a probe performed at read time.
	PageSourceProbe = "probe"
	// PageSourceRead reads at read time; logs belong here.
	PageSourceRead = "read"
	// PageSourceLocal reads browser storage; it never enters a cluster
	// projection.
	PageSourceLocal = "local"
	// PageSourceDatabase reads control-plane database records.
	PageSourceDatabase = "database"
)

// pageSources is the source vocabulary C-53 fixes.
var pageSources = map[string]bool{
	PageSourceObject:   true,
	PageSourceComputed: true,
	PageSourceMetric:   true,
	PageSourceProbe:    true,
	PageSourceRead:     true,
	PageSourceLocal:    true,
	PageSourceDatabase: true,
}

// IsPageSource reports whether source is one of C-53's seven source types.
func IsPageSource(source string) bool { return pageSources[source] }

// PageItem is one display item a page promises (C-53): its label, the source
// type that judges it, and the input that source resolves. Object, computed,
// metric, probe, read and database items carry a reference the control plane
// resolves; a local item carries the browser storage key instead. Action names
// the page action that modifies the item; a read-only page declares none.
type PageItem struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Source    string `json:"source"`
	Reference string `json:"reference,omitempty"`
	Store     string `json:"store,omitempty"`
	Action    string `json:"action,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Action is one executable action of a page (C-22). It names the control-plane
// API write the shell offers; the browser submits it with its own session, so
// the shell never proxies a native resource write.
type Action struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Path   string `json:"path"`
}

// roleRank orders the three session roles; an unknown or empty role is
// unsigned and sees no page (C-59).
var roleRank = map[write.Role]int{
	write.RoleGuest:  0,
	write.RoleMember: 1,
	write.RoleOwner:  2,
}

// Allows reports whether a session role sees this page (C-59). An empty
// required role admits every signed-in role.
func (p Page) Allows(role write.Role) bool {
	session, isSignedIn := roleRank[role]
	if !isSignedIn {
		return false
	}
	required := write.RoleGuest
	if p.RequiredRole != "" {
		required = write.Role(p.RequiredRole)
	}
	minimum, known := roleRank[required]
	if !known {
		return false
	}
	return session >= minimum
}

// Setting is an extension-contributed setting item (C-23).
type Setting struct {
	ID       string `json:"id"`
	Scope    string `json:"scope"`
	Writer   string `json:"writer"`
	Editable bool   `json:"editable,omitempty"`
}

// CatalogueSource is an application catalogue source (C-24): the entries it
// offers and how it keeps them current.
type CatalogueSource struct {
	ID      string           `json:"id"`
	Sync    string           `json:"sync,omitempty"`
	Entries []CatalogueEntry `json:"entries,omitempty"`
}

// CatalogueEntry is one installable offer of a catalogue source (C-24). The
// definition is what installing the entry writes; Display is what the catalogue
// shows for it. A malformed definition is not a declaration mistake: the entry
// stays visible and reports why it cannot be installed.
type CatalogueEntry struct {
	ID         string                            `json:"id"`
	Display    CatalogueDisplay                  `json:"display"`
	Definition v1beta1.ApplicationDefinitionSpec `json:"definition"`
}

// CatalogueDisplay is the display information of one catalogue entry (C-24).
type CatalogueDisplay struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
}

// DiscoveryAdapter is a discovery adapter contribution (C-25).
type DiscoveryAdapter struct {
	ID     string      `json:"id"`
	Covers []GroupKind `json:"covers,omitempty"`
	// Scope limits the objects the adapter reports.
	Scope DiscoveryScope `json:"scope,omitempty"`
	// Collector is the collection identity the adapter writes device facts as.
	Collector string `json:"collector,omitempty"`
	// Fields is the reported field set, as dotted paths inside each object.
	Fields []string `json:"fields,omitempty"`
}

// DiscoveryScope is the reach of one discovery adapter (C-25): an empty
// namespace list means every namespace, and an empty selector matches every
// object of a covered class.
type DiscoveryScope struct {
	Namespaces    []string `json:"namespaces,omitempty"`
	LabelSelector string   `json:"labelSelector,omitempty"`
}

// DiagnosticAdapter is a diagnostic adapter contribution (C-26). It names what
// it observes, where its evidence segment sits and what source its evidence
// carries.
type DiagnosticAdapter struct {
	ID     string      `json:"id"`
	Covers []GroupKind `json:"covers,omitempty"`
	// Components lists the extension components the adapter reports on; each
	// covered component contributes a segment of its own state.
	Components []string `json:"components,omitempty"`
	// Segment is the reference-chain link the evidence is inserted at; empty
	// means the dependency segment.
	Segment string `json:"segment,omitempty"`
	// Source is the data source every evidence entry names; empty means the
	// adapter identifier.
	Source string `json:"source,omitempty"`
}

// DiagnosticSegments returns the reference-chain links a diagnostic adapter may
// target (C-26). The dependency segment is the default.
func DiagnosticSegments() []string {
	return append([]string(nil), diagnosticSegments...)
}

// diagnosticSegments is the closed set of chain links an adapter may name.
var diagnosticSegments = []string{
	"application", "endpoint", "route", "backend", "workload", "node", "extension",
}

// PreferenceSchema is the preference key set an extension declares (C-27).
type PreferenceSchema struct {
	ID   string   `json:"id"`
	Keys []string `json:"keys"`
}

// Dependencies are the capability and component edges gating activation
// (C-18, C-28). Framework-version edges are declared in the declaration but not
// judged here.
type Dependencies struct {
	Capabilities []CapabilityRequirement `json:"capabilities,omitempty"`
	Components   []ComponentRequirement  `json:"components,omitempty"`
}

// CapabilityRequirement is one capability edge; an empty Versions admits every
// version.
type CapabilityRequirement struct {
	Name     string `json:"name"`
	Versions string `json:"versions,omitempty"`
}

// ComponentRequirement is one component edge; an empty Versions admits every
// version.
type ComponentRequirement struct {
	Identifier string `json:"identifier"`
	Versions   string `json:"versions,omitempty"`
}

// ClassProblem names one contribution class that failed to load and why
// (C-28).
type ClassProblem struct {
	Class string
	Items []string
}

// Loaded is the activatable part of a declaration. Entries that failed
// validation are absent; Problems names each failed class.
type Loaded struct {
	Identifier       string
	Version          string
	FrameworkVersion string
	APIGroup         string
	Contributions    Contributions
	Dependencies     Dependencies
	Permissions      []Permission
	Configuration    Configuration
	Retention        []Retention
	Problems         []ClassProblem
}

// Marshal encodes a declaration as the artifact a package carries.
func Marshal(declaration Declaration) ([]byte, error) {
	encoded, err := json.Marshal(declaration)
	if err != nil {
		return nil, fmt.Errorf("encode declaration: %w", err)
	}
	return append(encoded, '\n'), nil
}

// Parse reads a package artifact. Unknown fields are rejected, so a mistyped
// declaration is reported instead of silently dropped.
func Parse(content []byte) (Declaration, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	declaration := Declaration{}
	if err := decoder.Decode(&declaration); err != nil {
		return Declaration{}, fmt.Errorf("read declaration: %w", err)
	}
	return declaration, nil
}

// Load validates a declaration class by class (C-28). A declaration-level
// failure rejects the package; a class failure leaves the other classes
// loadable.
func (d Declaration) Load() (Loaded, error) {
	if strings.TrimSpace(d.Identifier) == "" || strings.TrimSpace(d.Identifier) != d.Identifier {
		return Loaded{}, errors.New("declaration names no identifier")
	}
	if strings.TrimSpace(d.Version) == "" || strings.TrimSpace(d.Version) != d.Version {
		return Loaded{}, errors.New("declaration names no version")
	}
	apiGroup := strings.TrimSpace(d.APIGroup)
	if apiGroup != d.APIGroup {
		return Loaded{}, errors.New("declaration apiGroup must not carry whitespace")
	}
	if apiGroup != "" {
		if problems := validation.IsDNS1123Subdomain(apiGroup); len(problems) > 0 {
			return Loaded{}, fmt.Errorf("apiGroup %q is not a DNS subdomain: %s", apiGroup, problems[0])
		}
		if apiGroup == api.Group {
			return Loaded{}, fmt.Errorf("apiGroup %s is the kernel group", apiGroup)
		}
	}
	dependencies, err := d.Dependencies.load()
	if err != nil {
		return Loaded{}, err
	}
	contributions, problems := d.Contributions.load(d.Identifier, apiGroup)
	return Loaded{
		Identifier:       d.Identifier,
		Version:          d.Version,
		FrameworkVersion: strings.TrimSpace(d.FrameworkVersion),
		APIGroup:         apiGroup,
		Contributions:    contributions,
		Dependencies:     dependencies,
		Permissions:      append([]Permission(nil), d.Permissions...),
		Configuration:    d.Configuration,
		Retention:        append([]Retention(nil), d.Retention...),
		Problems:         problems,
	}, nil
}

// load validates every contribution class independently (C-28).
func (c Contributions) load(identifier, apiGroup string) (Contributions, []ClassProblem) {
	loaded := Contributions{}
	var problems []ClassProblem

	objectClasses, classProblems := c.loadObjectClasses(apiGroup)
	loaded.ObjectClasses = objectClasses
	problems = appendProblem(problems, ClassObjectClasses, classProblems)

	controllers, controllerProblems := c.loadControllers(objectClasses)
	loaded.Controllers = controllers
	problems = appendProblem(problems, ClassControllers, controllerProblems)

	capabilities, capabilityProblems := c.loadCapabilities(identifier)
	loaded.Capabilities = CapabilityContributions{
		Items:          capabilities,
		StorageClasses: append([]string(nil), c.Capabilities.StorageClasses...),
		SelfReported:   append([]SelfReport(nil), c.Capabilities.SelfReported...),
	}
	problems = appendProblem(problems, ClassCapabilities, capabilityProblems)

	pages, pageProblems := loadPages(c.Pages)
	loaded.Pages = pages
	problems = appendProblem(problems, ClassPages, pageProblems)

	settings, settingProblems := loadSettings(c.Settings)
	loaded.Settings = settings
	problems = appendProblem(problems, ClassSettings, settingProblems)

	sources, sourceProblems := loadCatalogueSources(c.CatalogueSources)
	loaded.CatalogueSources = sources
	problems = appendProblem(problems, ClassCatalogueSources, sourceProblems)

	discovery, discoveryProblems := loadDiscoveryAdapters(c.DiscoveryAdapters)
	loaded.DiscoveryAdapters = discovery
	problems = appendProblem(problems, ClassDiscoveryAdapters, discoveryProblems)

	diagnostics, diagnosticProblems := loadDiagnosticAdapters(c.DiagnosticAdapters)
	loaded.DiagnosticAdapters = diagnostics
	problems = appendProblem(problems, ClassDiagnosticAdapters, diagnosticProblems)

	preferences, preferenceProblems := loadPreferences(c.Preferences)
	loaded.Preferences = preferences
	problems = appendProblem(problems, ClassPreferences, preferenceProblems)

	return loaded, problems
}

// appendProblem records a class only when it has items.
func appendProblem(problems []ClassProblem, class string, items []string) []ClassProblem {
	if len(items) == 0 {
		return problems
	}
	return append(problems, ClassProblem{Class: class, Items: items})
}

// where prefixes every item of one entry with its position.
func where(prefix string, items []string) []string {
	located := make([]string, 0, len(items))
	for _, item := range items {
		located = append(located, prefix+": "+item)
	}
	return located
}

// loadObjectClasses validates the object class declarations (C-19).
func (c Contributions) loadObjectClasses(apiGroup string) ([]ObjectClass, []string) {
	kept := make([]ObjectClass, 0, len(c.ObjectClasses))
	problems := []string{}
	seen := map[GroupKind]bool{}
	for index, entry := range c.ObjectClasses {
		entryProblems := checkObjectClass(entry, apiGroup)
		key := entry.GroupKind()
		if seen[key] {
			entryProblems = append(entryProblems, "class "+key.String()+" is declared twice")
		}
		seen[key] = true
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("objectClasses[%d]", index), entryProblems)...)
			continue
		}
		kept = append(kept, entry)
	}
	return kept, problems
}

// checkObjectClass reports the rules C-19 puts on one class: it belongs to the
// extension's own API group and never to the kernel group or a kernel kind.
func checkObjectClass(entry ObjectClass, apiGroup string) []string {
	var problems []string
	switch {
	case entry.Group == "":
		problems = append(problems, "group is required")
	case entry.Group == api.Group:
		problems = append(problems, "group "+api.Group+" is the kernel group")
	case apiGroup == "":
		problems = append(problems, "class group "+entry.Group+" needs the declaration's apiGroup")
	case entry.Group != apiGroup:
		problems = append(problems, "group "+entry.Group+" must equal the declaration's apiGroup "+apiGroup)
	}
	if entry.Kind == "" {
		problems = append(problems, "kind is required")
	} else if _, isKernel := api.ScopeOf(entry.Kind); isKernel {
		problems = append(problems, "kind "+entry.Kind+" is a kernel kind")
	}
	switch {
	case entry.Resource == "":
		problems = append(problems, "resource is required")
	case len(validation.IsDNS1123Label(entry.Resource)) > 0:
		problems = append(problems, "resource "+entry.Resource+" is not a lowercase plural name")
	}
	if len(entry.Versions) == 0 {
		problems = append(problems, "at least one version is required")
	}
	seen := map[string]bool{}
	for _, version := range entry.Versions {
		switch {
		case strings.TrimSpace(version) == "" || strings.TrimSpace(version) != version:
			problems = append(problems, "version entries must be non-empty")
		case seen[version]:
			problems = append(problems, "version "+version+" is listed twice")
		}
		seen[version] = true
	}
	paths := map[string]bool{}
	for _, path := range entry.References {
		problem := checkReferencePath(path)
		if problem != "" {
			problems = append(problems, problem)
			continue
		}
		if paths[path] {
			problems = append(problems, "reference path "+path+" is declared twice")
		}
		paths[path] = true
	}
	return problems
}

// checkReferencePath reports a reference path that is not a dotted spec path
// (C-19). The kernel follows only these declared locations.
func checkReferencePath(path string) string {
	if strings.TrimSpace(path) != path || path == "" {
		return "reference paths must be non-empty and carry no surrounding whitespace"
	}
	segments := strings.Split(path, ".")
	if len(segments) < 2 || segments[0] != "spec" {
		return "reference path " + path + " must start with spec."
	}
	for _, segment := range segments {
		if segment == "" {
			return "reference path " + path + " has an empty segment"
		}
		if strings.ContainsAny(segment, "[]") || isNumericSegment(segment) {
			// The reader walks object fields only; an indexed path would pass
			// validation and never resolve, silently disabling C-19.
			return "reference path " + path + " must name object fields only; index segments are not followed"
		}
	}
	return ""
}

// isNumericSegment reports whether a path segment is a bare array index. The
// reader cannot walk a list by index, so such a segment is a declaration error.
func isNumericSegment(segment string) bool {
	for _, character := range segment {
		if character < '0' || character > '9' {
			return false
		}
	}
	return segment != ""
}

// loadControllers validates controller declarations and their scope (C-20).
func (c Contributions) loadControllers(objectClasses []ObjectClass) ([]Controller, []string) {
	declared := map[GroupKind]bool{}
	for _, class := range objectClasses {
		declared[class.GroupKind()] = true
	}
	served := map[GroupKind]bool{}
	ids := map[string]bool{}
	kept := make([]Controller, 0, len(c.Controllers))
	problems := []string{}
	for index, entry := range c.Controllers {
		entryProblems := checkController(entry, declared, served)
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "controller "+entry.ID+" is declared twice")
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("controllers[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		for _, managed := range entry.Manages {
			served[managed] = true
		}
		kept = append(kept, entry)
	}
	return kept, problems
}

// checkController reports the rules C-20 puts on one controller.
func checkController(entry Controller, declared, served map[GroupKind]bool) []string {
	var problems []string
	if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
		problems = append(problems, "id is required")
	}
	for _, managed := range entry.Manages {
		switch {
		case managed.Kind == "":
			problems = append(problems, "managed class names no kind")
		case !declared[managed]:
			problems = append(problems, "manages undeclared class "+managed.String())
		case served[managed]:
			problems = append(problems, "class "+managed.String()+" is already managed by another controller")
		}
	}
	for _, group := range entry.Scope.Groups {
		if strings.TrimSpace(group) == "" {
			problems = append(problems, "scope names an empty group")
		}
	}
	for _, kind := range entry.Scope.Kinds {
		if kind.Kind == "" {
			problems = append(problems, "scope names a downstream kind without a kind")
		}
	}
	for _, namespace := range entry.Scope.Namespaces {
		if strings.TrimSpace(namespace) == "" {
			problems = append(problems, "scope names an empty namespace")
		}
	}
	return problems
}

// loadCapabilities validates capability declarations. A new capability carries
// the extension identifier as a prefix; a kernel baseline name is contributed
// as it is, because the baseline belongs to no extension (C-09, C-21).
func (c Contributions) loadCapabilities(identifier string) ([]Capability, []string) {
	prefix := identifier + "/"
	seen := map[string]bool{}
	kept := make([]Capability, 0, len(c.Capabilities.Items))
	problems := []string{}
	for index, entry := range c.Capabilities.Items {
		var entryProblems []string
		switch {
		case strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.Name) != entry.Name:
			entryProblems = append(entryProblems, "name is required")
		case !strings.HasPrefix(entry.Name, prefix) && !isBaselineCapability(entry.Name):
			entryProblems = append(entryProblems, "name must be prefixed with "+prefix+" or be a kernel baseline capability")
		}
		if entry.Version == "" {
			entryProblems = append(entryProblems, "version is required")
		}
		key := entry.Name + "@" + entry.Version
		if seen[key] {
			entryProblems = append(entryProblems, "capability "+entry.Name+"@"+entry.Version+" is declared twice")
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("capabilities[%d]", index), entryProblems)...)
			seen[key] = true
			continue
		}
		seen[key] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// loadPages validates page, item and action declarations (C-22, C-53).
func loadPages(entries []Page) ([]Page, []string) {
	ids := map[string]bool{}
	kept := make([]Page, 0, len(entries))
	problems := []string{}
	for index, entry := range entries {
		var entryProblems []string
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
			entryProblems = append(entryProblems, "id is required")
		}
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "page "+entry.ID+" is declared twice")
		}
		if strings.TrimSpace(entry.Title) == "" {
			entryProblems = append(entryProblems, "title is required")
		}
		if strings.TrimSpace(entry.Location) == "" || strings.TrimSpace(entry.Location) != entry.Location {
			entryProblems = append(entryProblems, "location is required")
		}
		if entry.RequiredRole != "" {
			if _, known := roleRank[write.Role(entry.RequiredRole)]; !known {
				entryProblems = append(entryProblems, "required role "+entry.RequiredRole+" is not a session role")
			}
		}
		switch {
		case entry.ReadOnly && strings.TrimSpace(entry.ReadOnlyReason) == "":
			entryProblems = append(entryProblems, "a read-only page must state why")
		case !entry.ReadOnly && entry.ReadOnlyReason != "":
			entryProblems = append(entryProblems, "readOnlyReason requires readOnly")
		}
		if entry.ReadOnly && len(entry.Actions) > 0 {
			entryProblems = append(entryProblems, "a read-only page declares no action")
		}
		if len(entry.Items) == 0 {
			entryProblems = append(entryProblems, "at least one item is required")
		}
		actionIDs := map[string]bool{}
		for actionIndex, action := range entry.Actions {
			var actionProblems []string
			if strings.TrimSpace(action.ID) == "" {
				actionProblems = append(actionProblems, "id is required")
			}
			if actionIDs[action.ID] {
				actionProblems = append(actionProblems, "action "+action.ID+" is declared twice")
			}
			actionIDs[action.ID] = true
			if !isWriteMethod(action.Method) {
				actionProblems = append(actionProblems, "method "+action.Method+" is not a control-plane write method")
			}
			if !isControlPlanePath(action.Path) {
				actionProblems = append(actionProblems, "path "+action.Path+" is not a control-plane API path")
			}
			entryProblems = append(entryProblems, where(fmt.Sprintf("actions[%d]", actionIndex), actionProblems)...)
		}
		itemIDs := map[string]bool{}
		for itemIndex, item := range entry.Items {
			var itemProblems []string
			if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.ID) != item.ID {
				itemProblems = append(itemProblems, "id is required")
			}
			if itemIDs[item.ID] {
				itemProblems = append(itemProblems, "item "+item.ID+" is declared twice")
			}
			itemIDs[item.ID] = true
			if strings.TrimSpace(item.Label) == "" {
				itemProblems = append(itemProblems, "label is required")
			}
			if !IsPageSource(item.Source) {
				itemProblems = append(itemProblems, "source "+item.Source+" is not a page source type")
			}
			itemProblems = append(itemProblems, checkItemReference(item)...)
			if entry.ReadOnly {
				if item.Action != "" {
					itemProblems = append(itemProblems, "a read-only page item has no modification entry")
				}
			} else {
				switch {
				case item.Action != "" && item.Reason != "":
					itemProblems = append(itemProblems, "an item with a modification entry must not declare a reason")
				case item.Action == "" && strings.TrimSpace(item.Reason) == "":
					itemProblems = append(itemProblems, "a writable page item needs a modification entry or a reason")
				case strings.TrimSpace(item.Reason) != item.Reason:
					itemProblems = append(itemProblems, "reason must not carry surrounding whitespace")
				}
				if item.Action != "" && !actionIDs[item.Action] {
					itemProblems = append(itemProblems, "item action "+item.Action+" is not a declared action")
				}
			}
			entryProblems = append(entryProblems, where(fmt.Sprintf("items[%d]", itemIndex), itemProblems)...)
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("pages[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// checkItemReference reports how one item's declared source names its input:
// a local item holds a browser storage key and no control-plane reference, and
// every server source holds a reference and no browser key.
func checkItemReference(item PageItem) []string {
	var problems []string
	switch item.Source {
	case PageSourceLocal:
		if strings.TrimSpace(item.Store) == "" {
			problems = append(problems, "a local item needs the browser storage key")
		}
		if item.Reference != "" {
			problems = append(problems, "a local item has no control-plane reference")
		}
	default:
		if strings.TrimSpace(item.Reference) == "" {
			problems = append(problems, "a server item needs a reference")
		}
		if item.Store != "" {
			problems = append(problems, "only a local item names browser storage")
		}
	}
	return problems
}

// isWriteMethod reports whether method is one of the write verbs a control-plane
// action may use (C-22).
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

// isControlPlanePath reports whether path addresses the control-plane API
// itself. An action can never name an external URL or a bare native resource
// path, so the page cannot smuggle a proxy for native writes (F9).
func isControlPlanePath(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Fragment != "" {
		return false
	}
	if !strings.HasPrefix(parsed.Path, "/api/v1/") || path.Clean(parsed.Path) != parsed.Path {
		return false
	}
	return true
}

// loadSettings validates setting declarations (C-23).
func loadSettings(entries []Setting) ([]Setting, []string) {
	ids := map[string]bool{}
	scopes := map[string]bool{SettingScopeCluster: true, SettingScopeNode: true, SettingScopeUser: true}
	writers := map[string]bool{SettingWriterController: true, SettingWriterUser: true, SettingWriterKernel: true}
	kept := make([]Setting, 0, len(entries))
	problems := []string{}
	for index, entry := range entries {
		var entryProblems []string
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
			entryProblems = append(entryProblems, "id is required")
		}
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "setting "+entry.ID+" is declared twice")
		}
		if !scopes[entry.Scope] {
			entryProblems = append(entryProblems, "scope "+entry.Scope+" is not a setting scope")
		}
		if !writers[entry.Writer] {
			entryProblems = append(entryProblems, "writer "+entry.Writer+" is not a setting writer")
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("settings[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// loadCatalogueSources validates catalogue source declarations (C-24). Entry
// identity is checked here because an entry names an application definition; its
// package content is judged by the catalogue, which reports a bad package as an
// entry that cannot be installed.
func loadCatalogueSources(entries []CatalogueSource) ([]CatalogueSource, []string) {
	ids := map[string]bool{}
	kept := make([]CatalogueSource, 0, len(entries))
	problems := []string{}
	for index, entry := range entries {
		var entryProblems []string
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
			entryProblems = append(entryProblems, "id is required")
		}
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "source "+entry.ID+" is declared twice")
		}
		entryIDs := map[string]bool{}
		for entryIndex, offer := range entry.Entries {
			if strings.TrimSpace(offer.ID) == "" || strings.TrimSpace(offer.ID) != offer.ID {
				entryProblems = append(entryProblems, fmt.Sprintf("entries[%d]: id is required", entryIndex))
			} else if problems := validation.IsDNS1123Subdomain(offer.ID); len(problems) > 0 {
				entryProblems = append(entryProblems, fmt.Sprintf("entries[%d]: id %q is not a DNS subdomain: %s", entryIndex, offer.ID, problems[0]))
			}
			if entryIDs[offer.ID] {
				entryProblems = append(entryProblems, fmt.Sprintf("entries[%d]: entry %s is declared twice", entryIndex, offer.ID))
			}
			entryIDs[offer.ID] = true
			if strings.TrimSpace(offer.Display.Name) == "" {
				entryProblems = append(entryProblems, fmt.Sprintf("entries[%d]: display.name is required", entryIndex))
			}
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("catalogueSources[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// loadDiscoveryAdapters validates discovery adapter declarations (C-25).
func loadDiscoveryAdapters(entries []DiscoveryAdapter) ([]DiscoveryAdapter, []string) {
	ids := map[string]bool{}
	kept := make([]DiscoveryAdapter, 0, len(entries))
	problems := []string{}
	for index, entry := range entries {
		var entryProblems []string
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
			entryProblems = append(entryProblems, "id is required")
		}
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "adapter "+entry.ID+" is declared twice")
		}
		for _, cover := range entry.Covers {
			if cover.Kind == "" {
				entryProblems = append(entryProblems, "covers names a class without a kind")
			}
		}
		for _, namespace := range entry.Scope.Namespaces {
			if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
				entryProblems = append(entryProblems, fmt.Sprintf("scope.namespaces names an invalid namespace %q", namespace))
			}
		}
		if entry.Scope.LabelSelector != "" {
			if _, err := labels.Parse(entry.Scope.LabelSelector); err != nil {
				entryProblems = append(entryProblems, fmt.Sprintf("scope.labelSelector is invalid: %v", err))
			}
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("discoveryAdapters[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// loadDiagnosticAdapters validates diagnostic adapter declarations (C-26).
func loadDiagnosticAdapters(entries []DiagnosticAdapter) ([]DiagnosticAdapter, []string) {
	ids := map[string]bool{}
	kept := make([]DiagnosticAdapter, 0, len(entries))
	problems := []string{}
	for index, entry := range entries {
		var entryProblems []string
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
			entryProblems = append(entryProblems, "id is required")
		}
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "adapter "+entry.ID+" is declared twice")
		}
		for _, cover := range entry.Covers {
			if cover.Kind == "" {
				entryProblems = append(entryProblems, "covers names a class without a kind")
			}
		}
		for _, component := range entry.Components {
			if strings.TrimSpace(component) == "" || strings.TrimSpace(component) != component {
				entryProblems = append(entryProblems, "components entries must be non-empty identifiers")
			}
		}
		if entry.Segment != "" && !slices.Contains(diagnosticSegments, entry.Segment) {
			entryProblems = append(entryProblems, "segment is not a reference-chain link")
		}
		if entry.Source != "" && strings.TrimSpace(entry.Source) != entry.Source {
			entryProblems = append(entryProblems, "source must be trimmed")
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("diagnosticAdapters[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// loadPreferences validates preference schema declarations (C-27).
func loadPreferences(entries []PreferenceSchema) ([]PreferenceSchema, []string) {
	ids := map[string]bool{}
	kept := make([]PreferenceSchema, 0, len(entries))
	problems := []string{}
	for index, entry := range entries {
		var entryProblems []string
		if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.ID) != entry.ID {
			entryProblems = append(entryProblems, "id is required")
		}
		if ids[entry.ID] {
			entryProblems = append(entryProblems, "preference "+entry.ID+" is declared twice")
		}
		if len(entry.Keys) == 0 {
			entryProblems = append(entryProblems, "at least one key is required")
		}
		keys := map[string]bool{}
		for _, key := range entry.Keys {
			if strings.TrimSpace(key) == "" {
				entryProblems = append(entryProblems, "key entries must be non-empty")
				continue
			}
			if keys[key] {
				entryProblems = append(entryProblems, "key "+key+" is declared twice")
			}
			keys[key] = true
		}
		if len(entryProblems) > 0 {
			problems = append(problems, where(fmt.Sprintf("preferences[%d]", index), entryProblems)...)
			ids[entry.ID] = true
			continue
		}
		ids[entry.ID] = true
		kept = append(kept, entry)
	}
	return kept, problems
}

// load validates dependency edges and their version ranges (C-28).
func (d Dependencies) load() (Dependencies, error) {
	loaded := Dependencies{}
	seenCapabilities := map[string]bool{}
	for _, requirement := range d.Capabilities {
		if strings.TrimSpace(requirement.Name) == "" || strings.TrimSpace(requirement.Name) != requirement.Name {
			return Dependencies{}, errors.New("capability dependency names no capability")
		}
		if _, err := ParseConstraint(requirement.Versions); err != nil {
			return Dependencies{}, fmt.Errorf("capability %s: %w", requirement.Name, err)
		}
		key := requirement.Name + "@" + requirement.Versions
		if seenCapabilities[key] {
			continue
		}
		seenCapabilities[key] = true
		loaded.Capabilities = append(loaded.Capabilities, requirement)
	}
	seenComponents := map[string]bool{}
	for _, requirement := range d.Components {
		if strings.TrimSpace(requirement.Identifier) == "" || strings.TrimSpace(requirement.Identifier) != requirement.Identifier {
			return Dependencies{}, errors.New("component dependency names no component")
		}
		if _, err := ParseConstraint(requirement.Versions); err != nil {
			return Dependencies{}, fmt.Errorf("component %s: %w", requirement.Identifier, err)
		}
		key := requirement.Identifier + "@" + requirement.Versions
		if seenComponents[key] {
			continue
		}
		seenComponents[key] = true
		loaded.Components = append(loaded.Components, requirement)
	}
	return loaded, nil
}
