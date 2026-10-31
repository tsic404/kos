// Package tc63 verifies C-23's single writer under concurrent writes: a user
// writing an item an extension controller owns is refused and told who owns it,
// while the declared writer's writes land, so the value reflects only the
// declared writer (AC-31, TC-63).
package tc63

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/identity"
	"github.com/tsic404/kos/pkg/settings"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	controllerItem = "cluster-endpoint"
	kernelItem     = "cluster-minimum-version"
	attempts       = 8
)

// declaration declares one controller-written and one kernel-written item, both
// of which the user may read but not change.
func declaration() extension.Declaration {
	return extension.Declaration{
		Identifier: "kos-endpoint-settings",
		Version:    "1.0.0",
		Contributions: extension.Contributions{
			Settings: []extension.Setting{
				{ID: controllerItem, Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterController},
				{ID: kernelItem, Scope: extension.SettingScopeCluster, Writer: extension.SettingWriterKernel},
			},
		},
	}
}

// result is one concurrent attempt: the status and the error the caller read.
type result struct {
	status  int
	code    string
	message string
}

// TestTC63OnlyTheDeclaredWriterWrites races the user surface against the
// declared writer and checks the declaration decides every attempt.
func TestTC63OnlyTheDeclaredWriterWrites(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	token := shell.SignInOwner(t, "owner", "owner-password")
	installation := cluster.InstallPackageWith(t, "endpoint-settings", declaration(), extension.MemoryArtifacts{}, shell.Retention())
	shell.Use(installation)

	// Both items are readable, name their writer, and offer no entry (C-23).
	_, body := shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	for id, writer := range map[string]string{controllerItem: "Controller", kernelItem: "Kernel"} {
		item := findSetting(t, body, id)
		if source := harness.String(item, "source"); source != writer {
			t.Fatalf("setting %q source = %q, want %q", id, source, writer)
		}
		if entry := harness.Nested(item, "entry"); entry != nil {
			t.Fatalf("setting %q offers an entry: %v", id, item)
		}
		if reason := harness.String(item, "reason"); reason == "" {
			t.Fatalf("setting %q carries no reason: %v", id, item)
		}
	}

	// The user surface and the declared controller write the same item at the
	// same time. Every attempt is recorded where it belongs.
	users := make([]result, attempts)
	controllerFailures := make([]string, attempts)
	controllerValues := make([]string, attempts)
	var wait sync.WaitGroup
	for index := range attempts {
		wait.Add(2)
		go func(index int) {
			defer wait.Done()
			status, payload, err := shell.Accounts.HTTP.Raw(http.MethodPut, "/api/v1/settings/"+controllerItem,
				map[string]any{"value": "user-" + strconv.Itoa(index)}, harness.Bearer(token))
			if err != nil {
				users[index] = result{message: err.Error()}
				return
			}
			answer := map[string]any{}
			_ = json.Unmarshal(payload, &answer)
			users[index] = result{
				status:  status,
				code:    harness.String(answer, "error", "code"),
				message: harness.String(answer, "error", "message"),
			}
		}(index)
		go func(index int) {
			defer wait.Done()
			value := "controller-" + strconv.Itoa(index)
			if _, failure := shell.Settings.SetAs(context.Background(),
				settings.Subject{Writer: extension.SettingWriterController}, controllerItem, "", value); failure != nil {
				controllerFailures[index] = failure.Message
				return
			}
			controllerValues[index] = value
		}(index)
	}
	wait.Wait()

	for index, attempt := range users {
		if attempt.status == 0 {
			t.Fatalf("user attempt %d did not reach the API: %s", index, attempt.message)
		}
		if attempt.status != http.StatusBadRequest {
			t.Fatalf("user attempt %d = %d %+v, want a refusal", index, attempt.status, attempt)
		}
		if attempt.code != controlplane.CodeInvalidField {
			t.Fatalf("user attempt %d failed with %q, want %q", index, attempt.code, controlplane.CodeInvalidField)
		}
		// The refusal names the writer the declaration fixed.
		if !strings.Contains(attempt.message, "扩展控制器") {
			t.Fatalf("user attempt %d message = %q, want the declared writer named", index, attempt.message)
		}
		if controllerFailures[index] != "" {
			t.Fatalf("controller attempt %d failed: %s", index, controllerFailures[index])
		}
		if controllerValues[index] == "" {
			t.Fatalf("controller attempt %d wrote nothing", index)
		}
	}

	// The current value is the declared writer's, never a refused user value.
	_, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	value := harness.String(findSetting(t, body, controllerItem), "value")
	if !strings.HasPrefix(value, "controller-") {
		t.Fatalf("current value = %q, want only the declared writer's result", value)
	}

	// The kernel-written item follows the same rule in the other direction: the
	// controller may not claim it and the kernel may.
	if _, failure := shell.Settings.SetAs(context.Background(),
		settings.Subject{Writer: extension.SettingWriterController}, kernelItem, "", "1"); failure == nil ||
		!strings.Contains(failure.Message, "内核校验器") {
		t.Fatalf("controller write to a kernel item = %v, want a refusal naming the kernel validator", failure)
	}
	if _, failure := shell.Settings.SetAs(context.Background(),
		settings.Subject{Writer: extension.SettingWriterKernel}, kernelItem, "", "v1.37.0"); failure != nil {
		t.Fatalf("kernel write: %v", failure)
	}
	_, body = shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if value := harness.String(findSetting(t, body, kernelItem), "value"); value != "v1.37.0" {
		t.Fatalf("kernel item value = %q, want the kernel's value", value)
	}

	// Every attempt of the race reached the trail: one record per user refusal
	// and one per accepted controller write, and nothing was written twice.
	entries, err := shell.Accounts.Trail.Store.AuditEntries(context.Background(), 4*attempts+16)
	if err != nil {
		t.Fatalf("read the audit trail: %v", err)
	}
	rejected, accepted := 0, 0
	for _, entry := range entries {
		if entry.Object != api.Group+"/Setting/"+controllerItem {
			continue
		}
		switch entry.Result {
		case controlplane.ResultRejected:
			rejected++
		case controlplane.ResultAccepted:
			accepted++
		}
	}
	if rejected != attempts || accepted != attempts {
		t.Fatalf("trail for %q = %d rejected / %d accepted, want %d / %d",
			controllerItem, rejected, accepted, attempts, attempts)
	}
	if entry := lastAccepted(entries, controllerItem); entry.Actor != extension.SettingWriterController {
		t.Fatalf("accepted record actor = %q, want the declared writer", entry.Actor)
	}
}

// lastAccepted returns the newest trail record of one accepted write to an
// item.
func lastAccepted(entries []identity.AuditEntry, id string) identity.AuditEntry {
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.Object == api.Group+"/Setting/"+id && entry.Result == controlplane.ResultAccepted {
			return entry
		}
	}
	return identity.AuditEntry{}
}

// findSetting returns one item of the settings response.
func findSetting(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	items, _ := body["items"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if harness.String(item, "id") == id {
			return item
		}
	}
	t.Fatalf("setting %q is absent from %v", id, body["items"])
	return nil
}
