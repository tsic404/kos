package communitycatalogue

import (
	"context"
	"time"

	"github.com/tsic404/kos/pkg/catalogue"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/pages"
)

// Readings resolves the two items the community page declares (C-22, C-53) over
// the catalogue domain: the entries this source offers, with their install
// parameters laid out as form fields (C-32), and the upgrade paths of the
// applications installed from this source.
type Readings struct {
	// Catalogue is the catalogue domain the page reads; required.
	Catalogue *catalogue.Service
	// Now supplies the read clock; defaults to time.Now.
	Now func() time.Time
}

// Read implements pages.Readings. The page is a panel page, so it takes no
// target.
func (r Readings) Read(ctx context.Context, _ identity.Session, item extension.PageItem, _ pages.Target) (pages.Reading, *controlplane.Error) {
	observedAt := r.now()
	switch item.Reference {
	case EntriesRef:
		return pages.Reading{Value: r.entries(), ObservedAt: observedAt}, nil
	case UpgradesRef:
		upgrades, failure := r.upgrades(ctx)
		if failure != nil {
			return pages.Reading{}, failure
		}
		return pages.Reading{Value: upgrades, ObservedAt: observedAt}, nil
	default:
		return pages.Reading{}, unresolved(item.Reference)
	}
}

// now reads the clock the observation time comes from.
func (r Readings) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

// entry is one catalogue entry as the page renders it: the catalogue's own
// judgement travels unchanged, and the config structure is replaced by the form
// fields generated from it, exactly as the first-party app centre renders its
// entries (C-31, C-32).
type entry struct {
	catalogue.Entry
	// Config is the install form the config structure generates.
	Config []pages.FormField `json:"config"`
}

// entries renders this source's entries. An empty source renders an empty list
// rather than a missing value.
func (r Readings) entries() []entry {
	if r.Catalogue == nil {
		return []entry{}
	}
	views := make([]entry, 0)
	for _, item := range r.Catalogue.Entries() {
		if item.Source != SourceID {
			continue
		}
		views = append(views, entry{Entry: item, Config: pages.FormOf(item.Config)})
	}
	return views
}

// upgrades renders the upgrade paths of the applications installed from this
// source. An application installed from another source is that source's upgrade
// path, not this one's.
func (r Readings) upgrades(ctx context.Context) ([]catalogue.Upgrade, *controlplane.Error) {
	if r.Catalogue == nil {
		return []catalogue.Upgrade{}, nil
	}
	paths, failure := r.Catalogue.Upgrades(ctx)
	if failure != nil {
		return nil, failure
	}
	mine := offered()
	upgrades := make([]catalogue.Upgrade, 0, len(paths))
	for _, path := range paths {
		if !mine[path.Definition] {
			continue
		}
		upgrades = append(upgrades, path)
	}
	return upgrades, nil
}

// unresolved names a page reference this reader does not serve.
func unresolved(reference string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "社区应用中心页的来源 " + reference + " 无法解析",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}
