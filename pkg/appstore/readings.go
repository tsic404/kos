// Readings resolves the app-center page's catalogue items (C-22, C-53): the
// entries item lays the install parameters out as the form fields the config
// structure generates (C-31, C-32), the upgrades item carries the upgrade paths.
// The value is read at request time, so the observation time is the read time and
// the shell's stale marker compares against it (C-13).
package appstore

import (
	"context"
	"time"

	"github.com/tsic404/kos/pkg/catalogue"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
)

// Readings serves the app-center page over the catalogue domain.
type Readings struct {
	catalogue *catalogue.Service
	now       func() time.Time
}

// NewReadings builds the page reader. now supplies the observation time; nil
// reads the clock at request time.
func NewReadings(service *catalogue.Service, now func() time.Time) Readings {
	if now == nil {
		now = time.Now
	}
	return Readings{catalogue: service, now: now}
}

// offer is one catalogue entry as the app-center page renders it (C-24, C-31).
// The catalogue's judgement travels unchanged, so an entry that cannot be
// installed carries the reason and missing capabilities the write path refuses it
// with; the install parameters are the form fields generated from the entry's
// config structure, which the shell lays out as the submission form (C-32).
type offer struct {
	catalogue.Entry
	// Config replaces the raw structure the catalogue read model carries with the
	// fields generated from it; the catalogue API still serves that structure.
	// encoding/json prefers this shallower field over the embedded one.
	Config []pages.FormField `json:"config"`
}

// Read resolves one declared item of the app-center page. The session is not
// consulted: the catalogue lists what every signed-in role may read, and the
// page itself is already trimmed to the caller's role (C-59).
func (r Readings) Read(ctx context.Context, _ identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	observedAt := r.now()
	switch item.Reference {
	case EntriesRef:
		return pages.Reading{Value: r.offers(), ObservedAt: observedAt}, nil
	case UpgradesRef:
		upgrades, failure := r.catalogue.Upgrades(ctx)
		if failure != nil {
			return pages.Reading{}, failure
		}
		return pages.Reading{Value: upgrades, ObservedAt: observedAt}, nil
	default:
		return pages.Reading{}, unresolved(item.Reference)
	}
}

// offers renders every catalogue entry with the install form its config
// structure generates. An entry nothing offers stays out, and an empty catalogue
// renders an empty list rather than a missing value.
func (r Readings) offers() []offer {
	entries := r.catalogue.Entries()
	offers := make([]offer, 0, len(entries))
	for _, entry := range entries {
		offers = append(offers, offer{Entry: entry, Config: pages.FormOf(entry.Config)})
	}
	return offers
}

// unresolved names a page reference this reader does not serve.
func unresolved(reference string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "应用中心页的来源 " + reference + " 无法解析",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}
