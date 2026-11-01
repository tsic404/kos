package kosdiagnostics

import (
	"github.com/tsic404/kos/pkg/extension"
)

// PageNativeView is the read-only native view (C-52): one native resource's raw
// fields and the objects related to it. The view is the browser's only interface
// to native resources and it writes nothing: a change goes through the CLI with
// a kubeconfig, which is why the view states its read-only reason (C-53, NG).
const PageNativeView = "native-view"

// NativeViewReason explains why the view offers no modification entry (C-53).
const NativeViewReason = "本视图只读：展示原生资源的原始字段与关联对象，原生资源的写操作走 CLI 与 kubeconfig"

// Item identities of the native view (C-52).
const (
	ItemRawFields = "raw-fields"
	ItemRelated   = "related-objects"
)

// nativeViewPage declares the two items C-52 puts on the native view. Both read
// an object, so both are object sources.
func nativeViewPage() extension.Page {
	return extension.Page{
		ID:             PageNativeView,
		Title:          "原生视图",
		Location:       "panel",
		ReadOnly:       true,
		ReadOnlyReason: NativeViewReason,
		Items: []extension.PageItem{
			{ID: ItemRawFields, Label: "原始字段", Source: extension.PageSourceObject, Reference: reference(ItemRawFields)},
			{ID: ItemRelated, Label: "关联对象", Source: extension.PageSourceObject, Reference: reference(ItemRelated)},
		},
	}
}
