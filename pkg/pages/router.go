package pages

import (
	"context"
	"strings"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
)

// ReferencePrefix returns the product identifier and the key of one declared
// item reference. A first-party product declares its items as
// "<product-identifier>/<key>", which is what lets several products render into
// one panel (C-22, C-53).
func ReferencePrefix(reference string) (product, key string, ok bool) {
	product, key, found := strings.Cut(reference, "/")
	if !found || product == "" || key == "" {
		return "", "", false
	}
	return product, key, true
}

// Router dispatches each page item to the product that owns it, keyed by the
// product identifier the item's reference starts with. One panel renders the
// pages of every active product, so the shell holds one reader while each
// product keeps its own source vocabulary (C-22, C-53).
type Router map[string]Readings

// Read implements Readings. An item whose reference names no routed product is
// refused exactly like a source the control plane has not loaded, so a page no
// product backs stays unreadable rather than showing an invented value.
func (r Router) Read(ctx context.Context, session identity.Session, item extension.PageItem) (Reading, *controlplane.Error) {
	product, _, ok := ReferencePrefix(item.Reference)
	if !ok {
		return Reading{}, unroutedItem(item.ID)
	}
	reader, known := r[product]
	if !known || reader == nil {
		return Reading{}, unroutedItem(item.ID)
	}
	return reader.Read(ctx, session, item)
}

// unroutedItem marks an item no product reader answers.
func unroutedItem(id string) *controlplane.Error {
	return &controlplane.Error{
		Code:        controlplane.CodeCapabilityMissing,
		Message:     "页面条目 " + id + " 的来源没有接入控制面",
		Stage:       controlplane.StageCapability,
		SideEffects: controlplane.SideEffectsNone,
	}
}
