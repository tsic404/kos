// Package pages renders the pages active extensions declare (C-22, C-53). The
// shell exposes the page registry trimmed to the caller's role, resolves every
// declared item by its source type, marks reads beyond the one freshness bound
// C-13 fixes as stale, and carries the control-plane write entry of each
// action. A read-only page states why and exposes no modification entry.
package pages

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/write"
)

// Declarations reports the pages active components declare (C-22). A page
// absent from the projection is treated as absent for every caller, so a
// disabled component withdraws its pages with it (F16).
type Declarations interface {
	// Pages returns every declared page.
	Pages() []extension.Page
	// PageFor resolves one page a session role may see; false covers both an
	// unknown page and one the role may not see.
	PageFor(id string, role write.Role) (extension.Page, bool)
}

// Reading is one item's current value and the moment its source observed it
// (C-53). A source that reads at request time stamps the read time, which is
// what C-13's stale marker compares against StaleThreshold.
type Reading struct {
	// Value is the item's current value as its source judges it.
	Value any
	// ObservedAt is when the source observed the value; it must be set.
	ObservedAt time.Time
}

// Readings resolves the current value of a server-source item. Local items
// never reach it: browser storage is not a control-plane projection (C-53).
// The session is the caller the page was rendered for, so a reader can judge
// the same visibility the read path does (C-59).
type Readings interface {
	// Read returns the value of one declared item.
	Read(ctx context.Context, session identity.Session, item extension.PageItem) (Reading, *controlplane.Error)
}

// Config carries what the page shell needs besides its declarations.
type Config struct {
	// Readings resolves server-source items. A nil value serves pages made of
	// local items only; a page declaring any server source then fails with a
	// capability missing rather than showing a made-up value.
	Readings Readings
	// Now supplies the read clock the stale marker compares against; defaults
	// to time.Now.
	Now func() time.Time
}

// Service renders the declared pages over a contribution projection.
type Service struct {
	declarations Declarations
	readings     Readings
	now          func() time.Time
}

// New builds the page shell.
func New(declarations Declarations, config Config) *Service {
	service := &Service{declarations: declarations, readings: config.Readings, now: config.Now}
	if service.now == nil {
		service.now = time.Now
	}
	return service
}

// Entry names the control-plane API write that carries out an action (C-22).
type Entry struct {
	// Method is the HTTP method of the write.
	Method string `json:"method"`
	// Path is the control-plane API path of the write.
	Path string `json:"path"`
}

// Action is one executable page action as the shell offers it.
type Action struct {
	// ID is the action identity the page declared.
	ID string `json:"id"`
	// Entry is the control-plane write the browser submits.
	Entry Entry `json:"entry"`
}

// ItemSummary is one declared display item without its resolved value; the
// page list exposes the registry, the single page read resolves values.
type ItemSummary struct {
	// ID is the item identity the page declared.
	ID string `json:"id"`
	// Label is the display label.
	Label string `json:"label"`
	// Source is one of C-53's seven source types.
	Source string `json:"source"`
	// Store is the browser storage key of a local item.
	Store string `json:"store,omitempty"`
}

// Summary is one page entry as the registry lists it.
type Summary struct {
	// ID is the page identity.
	ID string `json:"id"`
	// Title is the display title.
	Title string `json:"title"`
	// Location is where the shell shows the page.
	Location string `json:"location"`
	// RequiredRole is the session role the page requires; empty admits every
	// signed-in role.
	RequiredRole string `json:"requiredRole,omitempty"`
	// ReadOnly marks a page that offers no modification entry.
	ReadOnly bool `json:"readOnly"`
	// ReadOnlyReason explains a read-only page (C-53).
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
	// Sources are the distinct source types the page's items declare.
	Sources []string `json:"sources"`
	// Items describes the declared display items.
	Items []ItemSummary `json:"items"`
	// Actions are the executable actions; a read-only page has none.
	Actions []Action `json:"actions,omitempty"`
}

// Item is one display item with its current value (C-53).
type Item struct {
	// ID is the item identity the page declared.
	ID string `json:"id"`
	// Label is the display label.
	Label string `json:"label"`
	// Source is one of C-53's seven source types.
	Source string `json:"source"`
	// Value is the current value of a server source; it is null for a local
	// item, which the browser resolves from Store.
	Value any `json:"value"`
	// Store is the browser storage key of a local item.
	Store string `json:"store,omitempty"`
	// ObservedAt is when the source observed a server value (C-13); a local
	// item has no control-plane observation.
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// Stale marks a server value observed beyond StaleThreshold (C-13).
	Stale bool `json:"stale"`
	// Entry is the modification entry; a read-only page and an item without an
	// action have none.
	Entry *Entry `json:"entry,omitempty"`
	// Reason explains an item without a modification entry.
	Reason string `json:"reason,omitempty"`
}

// Page is one page with its items rendered.
type Page struct {
	// ID is the page identity.
	ID string `json:"id"`
	// Title is the display title.
	Title string `json:"title"`
	// Location is where the shell shows the page.
	Location string `json:"location"`
	// RequiredRole is the session role the page requires; empty admits every
	// signed-in role.
	RequiredRole string `json:"requiredRole,omitempty"`
	// ReadOnly marks a page that offers no modification entry.
	ReadOnly bool `json:"readOnly"`
	// ReadOnlyReason explains a read-only page (C-53).
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
	// Sources are the distinct source types the page's items declare.
	Sources []string `json:"sources"`
	// Items are the declared items with their current values.
	Items []Item `json:"items"`
	// Actions are the executable actions; a read-only page has none.
	Actions []Action `json:"actions,omitempty"`
}

// List returns the pages the session role may see, ordered by identity. A page
// the role may not see is absent, exactly as if no component declared it
// (C-22, F16).
func (s *Service) List(session identity.Session) []Summary {
	declared := s.declared().Pages()
	summaries := make([]Summary, 0, len(declared))
	seen := map[string]bool{}
	for _, page := range declared {
		if !page.Allows(session.Role) || seen[page.ID] {
			continue
		}
		seen[page.ID] = true
		summaries = append(summaries, summaryOf(page))
	}
	sort.Slice(summaries, func(left, right int) bool {
		return summaries[left].ID < summaries[right].ID
	})
	return summaries
}

// Read renders one page for the session. A page the role may not see and a page
// no active component declares answer with the same failure, so an entry the
// caller cannot see is indistinguishable from one that does not exist (C-22).
func (s *Service) Read(ctx context.Context, session identity.Session, id string) (Page, *controlplane.Error) {
	page, found := s.declared().PageFor(id, session.Role)
	if !found {
		return Page{}, pageNotFound()
	}
	rendered := Page{
		ID:             page.ID,
		Title:          page.Title,
		Location:       page.Location,
		RequiredRole:   page.RequiredRole,
		ReadOnly:       page.ReadOnly,
		ReadOnlyReason: page.ReadOnlyReason,
	}
	entries := map[string]Entry{}
	if !page.ReadOnly {
		rendered.Actions = actionsOf(page.Actions)
		for _, action := range page.Actions {
			entries[action.ID] = Entry{Method: action.Method, Path: action.Path}
		}
	}
	now := s.now()
	sources := map[string]bool{}
	for _, item := range page.Items {
		if !sources[item.Source] {
			sources[item.Source] = true
			rendered.Sources = append(rendered.Sources, item.Source)
		}
		renderedItem := Item{ID: item.ID, Label: item.Label, Source: item.Source, Reason: item.Reason}
		if !page.ReadOnly {
			switch {
			case item.Action != "" && item.Reason != "":
				return Page{}, pageItemContradiction(item.ID)
			case item.Action == "" && strings.TrimSpace(item.Reason) == "":
				return Page{}, pageItemWithoutEntry(item.ID)
			}
		}
		if item.Source == extension.PageSourceLocal {
			renderedItem.Store = item.Store
		} else {
			if s.readings == nil {
				return Page{}, sourceUnavailable(item.Source)
			}
			reading, failure := s.readings.Read(ctx, session, item)
			if failure != nil {
				return Page{}, failure
			}
			if reading.ObservedAt.IsZero() {
				return Page{}, sourceWithoutObservation(item.Source)
			}
			observedAt := reading.ObservedAt.UTC()
			renderedItem.Value = reading.Value
			renderedItem.ObservedAt = &observedAt
			renderedItem.Stale = now.Sub(reading.ObservedAt) > controlplane.StaleThreshold
		}
		if entry, found := entries[item.Action]; found {
			renderedItem.Entry = &entry
		}
		rendered.Items = append(rendered.Items, renderedItem)
	}
	return rendered, nil
}

// declared reads the current projection; a shell without declarations serves no
// page.
func (s *Service) declared() Declarations {
	if s.declarations == nil {
		return emptyDeclarations{}
	}
	return s.declarations
}

// emptyDeclarations is the projection of a shell with no active component.
type emptyDeclarations struct{}

// Pages implements Declarations.
func (emptyDeclarations) Pages() []extension.Page { return nil }

// PageFor implements Declarations.
func (emptyDeclarations) PageFor(string, write.Role) (extension.Page, bool) {
	return extension.Page{}, false
}

// summaryOf describes one declared page without resolving item values.
func summaryOf(page extension.Page) Summary {
	summary := Summary{
		ID:             page.ID,
		Title:          page.Title,
		Location:       page.Location,
		RequiredRole:   page.RequiredRole,
		ReadOnly:       page.ReadOnly,
		ReadOnlyReason: page.ReadOnlyReason,
		Items:          make([]ItemSummary, 0, len(page.Items)),
	}
	seen := map[string]bool{}
	for _, item := range page.Items {
		if !seen[item.Source] {
			seen[item.Source] = true
			summary.Sources = append(summary.Sources, item.Source)
		}
		summary.Items = append(summary.Items, ItemSummary{
			ID: item.ID, Label: item.Label, Source: item.Source, Store: item.Store,
		})
	}
	if !page.ReadOnly {
		summary.Actions = actionsOf(page.Actions)
	}
	return summary
}

// actionsOf renders the control-plane write entry of every action.
func actionsOf(declared []extension.Action) []Action {
	actions := make([]Action, 0, len(declared))
	for _, action := range declared {
		actions = append(actions, Action{
			ID:    action.ID,
			Entry: Entry{Method: action.Method, Path: action.Path},
		})
	}
	return actions
}

// pageNotFound is the one answer for an unknown page and for a page the session
// role may not see (C-22).
func pageNotFound() *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeNotFound,
		Message:     "页面不存在，或提供它的组件未生效",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// sourceUnavailable marks a page whose declared source this shell cannot read.
func sourceUnavailable(source string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "页面来源 " + source + " 尚未接入控制面",
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// sourceWithoutObservation marks a reader that returned a value with no
// observation time, which C-13 requires of every projection.
func sourceWithoutObservation(source string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "页面来源 " + source + " 没有给出观测时间",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// pageItemWithoutEntry marks a writable page item that names neither a
// modification entry nor a reason, which C-53 does not allow.
func pageItemWithoutEntry(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "页面条目 " + id + " 既没有修改入口也没有原因",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}

// pageItemContradiction marks an item that declares a modification entry and a
// reason at once, which C-53 does not allow.
func pageItemContradiction(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "页面条目 " + id + " 同时声明修改入口与不可改原因",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}
