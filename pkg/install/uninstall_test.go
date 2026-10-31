package install

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/extension"
)

// TestUninstallRefusesUnreadableExtensionClasses pins C-54: a leftover list
// that cannot know the installed extension classes fails instead of silently
// dropping the adopted extension instances; an explicitly configured list is
// the fallback.
func TestUninstallRefusesUnreadableExtensionClasses(t *testing.T) {
	resource := schema.GroupVersionResource{Group: api.Group, Version: "v1beta1", Resource: "systemcomponents"}
	client := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{resource: "SystemComponentList"},
	)
	client.PrependReactor("list", "systemcomponents", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("declaration store unavailable")
	})
	uninstaller := &Uninstaller{config: UninstallConfig{
		Dynamic:   client,
		Artifacts: extension.MemoryArtifacts{},
	}}
	if _, err := uninstaller.extensionClasses(context.Background()); err == nil {
		t.Fatal("an unreadable declaration store was accepted without a fallback")
	}

	uninstaller.config.ExtensionClasses = []schema.GroupKind{{Group: "widgets.example.test", Kind: "Widget"}}
	classes, err := uninstaller.extensionClasses(context.Background())
	if err != nil {
		t.Fatalf("configured classes were not used as the fallback: %v", err)
	}
	if len(classes) != 1 || classes[0].Kind != "Widget" {
		t.Fatalf("classes = %v, want the configured fallback", classes)
	}
}
