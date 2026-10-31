// Readings resolves the app-center page's catalogue items (C-22, C-53): the
// entries item carries the install parameter structure each form field is
// generated from (C-32), the upgrades item carries the upgrade paths. The value
// is read at request time, so the observation time is the read time and the
// shell's stale marker compares against it (C-13).
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

// Read resolves one declared item of the app-center page. The session is not
// consulted: the catalogue lists what every signed-in role may read, and the
// page itself is already trimmed to the caller's role (C-59).
func (r Readings) Read(ctx context.Context, _ identity.Session, item extension.PageItem) (pages.Reading, *controlplane.Error) {
	observedAt := r.now()
	switch item.Reference {
	case EntriesRef:
		return pages.Reading{Value: r.catalogue.Entries(), ObservedAt: observedAt}, nil
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

// unresolved names a page reference this reader does not serve.
func unresolved(reference string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeInternal,
		Message:     "应用中心页的来源 " + reference + " 无法解析",
		Stage:       controlplane.StageRead,
		SideEffects: controlplane.SideEffectsNone,
	}
}
