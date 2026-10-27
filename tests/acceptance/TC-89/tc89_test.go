// Package tc89 verifies that the diagnostic bundle exports as a downloadable
// file with the five content classes C-26 and C-56 name, that a degradation
// reason points at a concrete object, that the event window and order hold, and
// that neither credentials nor logs reach the bundle (AC-44, TC-89).
package tc89

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/diagnostics"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

func TestTC89DiagnosticBundleContents(t *testing.T) {
	cluster := harness.Start(t)
	world := harness.StartDiagnosticWorld(t, cluster)
	service := cluster.DiagnosticService(world.Installation)
	api := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Diagnostics = service
	})

	status, headers, body, err := api.RawWithHeaders(http.MethodGet,
		"/api/v1/diagnostics/"+world.Namespace+"/"+world.Application+"/bundle", nil, nil)
	if err != nil {
		t.Fatalf("export the diagnostic bundle: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("export the diagnostic bundle answered %d: %s", status, body)
	}
	disposition := headers.Get("Content-Disposition")
	if !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, "diagnostic-bundle-shop.json") {
		t.Fatalf("Content-Disposition = %q, want an attachment named for the application", disposition)
	}

	// The export is a file a user keeps.
	target := filepath.Join(t.TempDir(), "diagnostic-bundle.json")
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatalf("write the diagnostic bundle: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() == 0 {
		t.Fatalf("the diagnostic bundle file is missing or empty: %v", err)
	}

	document := map[string]any{}
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("decode the diagnostic bundle: %v", err)
	}
	for _, key := range []string{"application", "exportedAt", "evidence"} {
		if _, found := document[key]; !found {
			t.Fatalf("the bundle carries no %q", key)
		}
	}
	for _, key := range []string{"conditionSnapshot", "recentEvents", "renderDiff", "capabilities", "versionMatrix"} {
		if _, found := document[key]; !found {
			t.Fatalf("the bundle carries no %q content class", key)
		}
	}
	for _, forbidden := range []string{"logs", "log"} {
		if _, found := document[forbidden]; found {
			t.Fatalf("the bundle carries %q; logs never enter the bundle", forbidden)
		}
	}

	bundle := diagnostics.Bundle{}
	if err := json.Unmarshal(body, &bundle); err != nil {
		t.Fatalf("decode the typed bundle: %v", err)
	}
	if len(bundle.ConditionSnapshot) == 0 {
		t.Fatal("the condition snapshot is empty")
	}
	if len(bundle.Capabilities) == 0 {
		t.Fatal("the capability list is empty")
	}
	if len(bundle.VersionMatrix.KernelAPIVersions) == 0 || len(bundle.VersionMatrix.Extensions) == 0 ||
		len(bundle.VersionMatrix.Nodes) == 0 {
		t.Fatalf("the version matrix is incomplete: %+v", bundle.VersionMatrix)
	}

	// The event class covers the last hour only, newest first (C-64). The
	// oldest fixture event is outside the window and must be absent; a node
	// event outside the application namespace must still be readable.
	if bundle.RecentEvents.Window != "1h0m0s" {
		t.Fatalf("event window = %q, want one hour", bundle.RecentEvents.Window)
	}
	if !bundle.RecentEvents.Available {
		t.Fatalf("the recent events are unreadable: %s", bundle.RecentEvents.Reason)
	}
	names := make([]string, 0, len(bundle.RecentEvents.Events))
	for _, event := range bundle.RecentEvents.Events {
		names = append(names, event.Name)
	}
	if len(names) != 3 || names[0] != harness.EventNewest || names[1] != harness.EventNode ||
		names[2] != harness.EventWithinWindow {
		t.Fatalf("events = %v, want [%s %s %s] newest first", names,
			harness.EventNewest, harness.EventNode, harness.EventWithinWindow)
	}

	if !bundle.RenderDiff.Available || len(bundle.RenderDiff.Entries) == 0 {
		t.Fatalf("the render diff is unavailable or empty: %+v", bundle.RenderDiff)
	}

	// The render diff names drifted values and residual fields by path, never
	// by value, and reports a rendered Secret as omitted (C-14). The Deployment
	// entry's field set is exact: a field the API server defaults that leaked
	// into the diff would read as a false positive on every export.
	wantFields := []string{
		"metadata.labels.legacy",
		"spec.template.spec.containers[0].env[0].value",
	}
	deploymentFields := []string{}
	hasOmittedSecret := false
	for _, entry := range bundle.RenderDiff.Entries {
		switch {
		case entry.Object.Kind == "Secret":
			if entry.Difference == diagnostics.DifferenceOmitted && entry.Reason != "" {
				hasOmittedSecret = true
			}
		case entry.Object.Kind == "Deployment" && entry.Difference == diagnostics.DifferenceDrifted:
			deploymentFields = append(deploymentFields, entry.Fields...)
		}
	}
	if !slices.Equal(deploymentFields, wantFields) {
		t.Fatalf("Deployment drift fields = %v, want exactly %v", deploymentFields, wantFields)
	}
	if !hasOmittedSecret {
		t.Fatalf("the render diff reports no omitted Secret: %+v", bundle.RenderDiff.Entries)
	}

	// The backend segment follows the service the Ingress names, not the
	// application name.
	backend := ""
	for _, segment := range bundle.Evidence.Segments {
		if segment.Link == diagnostics.LinkBackend && segment.Object.Kind == "Service" {
			backend = segment.Object.Name
		}
	}
	if backend != "shop-backend" {
		t.Fatalf("backend segment object = %q, want the service the Ingress names", backend)
	}

	// The degradation reason resolves to the object and the segment that prove
	// it (C-26).
	conclusion := bundle.Evidence.Conclusion
	if conclusion.Segment != diagnostics.LinkNode || conclusion.Object.Kind != "Node" ||
		conclusion.Object.Name != "node-a" {
		t.Fatalf("conclusion = %+v, want the NotReady node segment", conclusion)
	}

	// No credential value and no log line reaches the export: the sensitive
	// config value, the stored Secret and its encoded form are all absent.
	encoded := string(body)
	for _, secret := range []string{
		world.ConfigSecret,
		world.CredentialSecret,
		base64.StdEncoding.EncodeToString([]byte(world.CredentialSecret)),
	} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("the bundle carries the credential value %q", secret)
		}
	}
}
