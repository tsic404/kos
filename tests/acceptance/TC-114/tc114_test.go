// Package tc114 checks AC-56 for the notification and file manager products:
// each product takes effect only through its declared contributions, the kernel
// kind set, capability baseline, page shell and writer table stay as they were,
// a third-party package with the same kinds of contributions behaves alike, and
// removing one product leaves the others serving (TC-114).
package tc114

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/tsic404/kos/api"
	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
	"github.com/tsic404/kos/pkg/filemanager"
	"github.com/tsic404/kos/pkg/notification"
	"github.com/tsic404/kos/pkg/reconcile"
	"github.com/tsic404/kos/pkg/write"
	"github.com/tsic404/kos/tests/acceptance/harness"
)

const (
	notificationComponent = "kos-notification"
	filemanagerComponent  = "kos-filemanager"
	thirdPartyComponent   = "third-party-notifier"
	thirdPartyGroup       = "notify.example.test"
	thirdPartyVersion     = "v1"
	thirdPartyKind        = "Notice"
	thirdPartyResource    = "notices"
	thirdPartyController  = "notifier"

	okChannel      = "ops"
	brokenChannel  = "broken"
	subscriptionID = "warnings"

	channelCredential = "control-plane-secret-ops"
	brokenCredential  = "control-plane-secret-broken"
)

// deliveryTime is the clock the delivery case reads on, so the event fixtures
// and the window agree.
var deliveryTime = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// thirdPartyDeclaration is an extension outside the distribution that
// contributes the same kinds of entries as the notification product: one object
// class, one controller and one setting (C-18).
func thirdPartyDeclaration() extension.Declaration {
	return extension.Declaration{
		Identifier:       "thirdparty.notifier",
		Version:          "1.0.0",
		FrameworkVersion: ">=1.0 <2.0",
		APIGroup:         thirdPartyGroup,
		Contributions: extension.Contributions{
			ObjectClasses: []extension.ObjectClass{{
				Group: thirdPartyGroup, Kind: thirdPartyKind, Resource: thirdPartyResource,
				Versions: []string{thirdPartyVersion},
			}},
			Controllers: []extension.Controller{{
				ID:      thirdPartyController,
				Manages: []extension.GroupKind{{Group: thirdPartyGroup, Kind: thirdPartyKind}},
				Scope:   extension.WriteScope{Groups: []string{thirdPartyGroup}},
			}},
			Settings: []extension.Setting{{
				ID: "thirdparty.notifier.channels", Scope: extension.SettingScopeCluster,
				Writer: extension.SettingWriterUser, Editable: true,
			}},
		},
	}
}

// TestTC114ContributionsLoadAndWithdraw walks the install order and the removal
// order of the three packages, checking after each step what is served and that
// the kernel did not change.
func TestTC114ContributionsLoadAndWithdraw(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, notification.APIGroup, notification.ObjectVersion, notification.Kind, notification.Resource, false)
	cluster.InstallCRD(t, thirdPartyGroup, thirdPartyVersion, thirdPartyKind, thirdPartyResource, false)
	artifacts := extension.MemoryArtifacts{}

	kindsBefore := append([]string(nil), api.Kinds()...)
	capabilitiesBefore := extension.BaselineCapabilities()
	ownedBefore := write.OwnedKinds()
	denialBefore := deniedKernelWrite(t, cluster)

	// The notification product: its class, controller and two setting items,
	// and no page of its own.
	installed := cluster.InstallPackage(t, notificationComponent, notification.Declaration(), artifacts)
	requireReady(t, installed, notificationComponent)
	registry := installed.Registry()
	if _, served := registry.ObjectClass(notification.APIGroup, notification.Kind); !served {
		t.Fatal("the delivery record class is not served after the install")
	}
	requireController(t, registry, notification.ControllerID, notification.APIGroup, notification.Kind)
	for _, id := range []string{notification.SettingChannels, notification.SettingSubscriptions} {
		if _, found := registry.Setting(id); !found {
			t.Fatalf("the notification setting %q is not visible", id)
		}
	}
	requireNoPages(t, registry)
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// The file manager product: its user setting and its two preference keys,
	// and no page either (the interface opens as an application window).
	installed = cluster.InstallPackage(t, filemanagerComponent, filemanager.Declaration(), artifacts)
	requireReady(t, installed, filemanagerComponent)
	registry = installed.Registry()
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); !found {
		t.Fatal("the exposed-volume setting is not visible after the file manager install")
	}
	keys, found := registry.PreferenceKeys(filemanager.Identifier)
	if !found || !reflect.DeepEqual(keys, []string{filemanager.PreferenceView, filemanager.PreferenceInitialPath}) {
		t.Fatalf("preference keys = %v (found %v), want the view and initial path", keys, found)
	}
	if _, found := registry.Setting(notification.SettingChannels); !found {
		t.Fatal("the notification setting disappeared when the second product installed")
	}
	requireNoPages(t, registry)
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// A third-party package with the same kinds of contributions loads the same
	// way (C-17: the kernel loads declarations, not names).
	installed = cluster.InstallPackage(t, thirdPartyComponent, thirdPartyDeclaration(), artifacts)
	registry = installed.Registry()
	firstParty, found := installed.Component(notificationComponent)
	if !found {
		t.Fatal("the notification component left the install result")
	}
	thirdParty, found := installed.Component(thirdPartyComponent)
	if !found {
		t.Fatal("the third-party component is missing from the install result")
	}
	if firstParty.Phase != thirdParty.Phase || len(firstParty.Problems) != len(thirdParty.Problems) {
		t.Fatalf("first-party %+v and third-party %+v loaded differently", firstParty, thirdParty)
	}
	if _, served := registry.ObjectClass(thirdPartyGroup, thirdPartyKind); !served {
		t.Fatal("the third-party class is not served after the install")
	}
	requireController(t, registry, thirdPartyController, thirdPartyGroup, thirdPartyKind)
	if _, found := registry.Setting("thirdparty.notifier.channels"); !found {
		t.Fatal("the third-party setting is not visible")
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// Removing the file manager withdraws its setting and preferences and
	// leaves both notification packages serving.
	cluster.DeleteComponent(t, filemanagerComponent)
	registry = cluster.ExtensionState(t, artifacts).Registry()
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); found {
		t.Fatal("the exposed-volume setting survived the uninstall")
	}
	if _, found := registry.PreferenceKeys(filemanager.Identifier); found {
		t.Fatal("the file manager preference schema survived the uninstall")
	}
	if _, found := registry.Setting(notification.SettingChannels); !found {
		t.Fatal("the notification setting disappeared with another product's uninstall")
	}
	if _, served := registry.ObjectClass(thirdPartyGroup, thirdPartyKind); !served {
		t.Fatal("the third-party class disappeared with another product's uninstall")
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)

	// Removing the notification product withdraws its class and settings, and
	// the control plane stops serving its class (C-19).
	cluster.DeleteComponent(t, notificationComponent)
	registry = cluster.ExtensionState(t, artifacts).Registry()
	if _, served := registry.ObjectClass(notification.APIGroup, notification.Kind); served {
		t.Fatal("the delivery record class is still served after the uninstall")
	}
	for _, id := range []string{notification.SettingChannels, notification.SettingSubscriptions} {
		if _, found := registry.Setting(id); found {
			t.Fatalf("the notification setting %q survived the uninstall", id)
		}
	}
	if _, served := registry.ObjectClass(thirdPartyGroup, thirdPartyKind); !served {
		t.Fatal("the third-party class disappeared with the first-party uninstall")
	}
	if _, found := registry.Setting("thirdparty.notifier.channels"); !found {
		t.Fatal("the third-party setting disappeared with the first-party uninstall")
	}
	control := cluster.StartControlPlane(t, func(config *controlplane.Config) {
		config.Extensions = registry
	})
	status, body := control.Call(t, http.MethodGet,
		"/api/v1/resources/"+notification.Kind+"?apiVersion="+notification.APIGroup+"/"+notification.ObjectVersion, nil, nil)
	if status != http.StatusUnprocessableEntity || harness.String(body, "error", "code") != controlplane.CodeCapabilityMissing {
		t.Fatalf("read the withdrawn class = %d %v, want capability_missing", status, body)
	}
	requireKernelUnchanged(t, kindsBefore, capabilitiesBefore, ownedBefore, denialBefore, cluster)
}

// TestTC114NotificationDeliversRecentEvents installs the notification product,
// configures channels and subscriptions from the control plane, feeds the
// window's events and checks the record each delivery leaves behind.
func TestTC114NotificationDeliversRecentEvents(t *testing.T) {
	cluster := harness.Start(t)
	cluster.InstallCRD(t, notification.APIGroup, notification.ObjectVersion, notification.Kind, notification.Resource, false)
	shell := cluster.StartShell(t)
	ownerToken := shell.SignInOwner(t, "owner", "owner-password")
	artifacts := extension.MemoryArtifacts{}
	declaration := notification.Declaration()
	installed := cluster.InstallPackage(t, notificationComponent, declaration, artifacts)
	shell.Use(installed)
	requireReady(t, installed, notificationComponent)
	contribution := declaration.Contributions.Controllers[0]

	hook := &webhookRecorder{}
	hookServer := httptest.NewServer(http.HandlerFunc(hook.serve))
	t.Cleanup(hookServer.Close)
	okEndpoint := hookServer.URL
	brokenEndpoint := "http://127.0.0.1:1/notify"

	logs := &bytes.Buffer{}
	controllers := extension.NewControllerSet(2)
	writer := notificationWriter(t, cluster, contribution.Scope)
	controller := &notification.Controller{
		Writer:   writer,
		Settings: shell.Accounts.Store,
		Credentials: notification.StaticCredentials{
			okChannel:     {Endpoint: okEndpoint, Secret: channelCredential},
			brokenChannel: {Endpoint: brokenEndpoint, Secret: brokenCredential},
		},
		Senders: map[string]notification.Sender{"webhook": notification.Webhook{}},
		Log:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:     func() time.Time { return deliveryTime },
	}
	if err := controllers.Register(notificationComponent, contribution, controller); err != nil {
		t.Fatalf("register the delivery controller: %v", err)
	}
	runContext, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		controllers.Run(runContext)
	}()
	defer func() {
		stop()
		<-done
	}()

	// The control plane holds the channel shape; the secret never enters the
	// setting value (C-14).
	writeSetting(t, shell, ownerToken, notification.SettingChannels, `{"channels":[`+
		`{"id":"`+okChannel+`","kind":"webhook","endpoint":"`+okEndpoint+`"},`+
		`{"id":"`+brokenChannel+`","kind":"webhook","endpoint":"`+brokenEndpoint+`"}]}`)
	writeSetting(t, shell, ownerToken, notification.SettingSubscriptions,
		`{"subscriptions":[{"id":"`+subscriptionID+`","types":["Warning"],"channels":["`+okChannel+`","`+brokenChannel+`"]}]}`)

	// The window's events: two matches, one event of another type and one event
	// from before the hour C-64 fixes.
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		eventManifest("warn-newest", "Warning", "BackOff", deliveryTime.Add(-5*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		eventManifest("warn-recent", "Warning", "FailedMount", deliveryTime.Add(-30*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		eventManifest("normal-recent", "Normal", "Scheduled", deliveryTime.Add(-15*time.Minute)))
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		eventManifest("warn-old", "Warning", "BackOff", deliveryTime.Add(-3*time.Hour)))

	service := &notification.Service{
		Dynamic:  cluster.Dynamic,
		Writer:   writer,
		Settings: shell.Accounts.Store,
		Queue: queueFunc(func(request reconcile.Request) error {
			return controllers.Enqueue(notificationComponent, notification.ControllerID, request)
		}),
		Now: func() time.Time { return deliveryTime },
	}
	report, err := service.Deliver(context.Background())
	if err != nil {
		t.Fatalf("deliver the recent events: %v", err)
	}
	if len(report.Created) != 4 {
		t.Fatalf("created %d records (%+v), want two matches on two channels", len(report.Created), report)
	}
	if len(report.Problems) != 0 {
		t.Fatalf("delivery problems = %v, want none", report.Problems)
	}
	// The read is newest first, and the event older than the hour never matched.
	if newest := readRecord(cluster, report.Created[0].Name); newest == nil || recordEventName(newest) != "warn-newest" {
		t.Fatalf("the first record is %v, want the newest matching event", newest)
	}

	// Every record reaches its terminal status: the reachable channel accepts,
	// the unreachable one fails and keeps the failure visible.
	deadline := time.Now().Add(30 * time.Second)
	for {
		delivered, failed := 0, 0
		phases := make(map[string]string, len(report.Created))
		for _, request := range report.Created {
			record := readRecord(cluster, request.Name)
			if record == nil {
				phases[request.Name] = "missing"
				continue
			}
			phases[request.Name] = notificationRecordPhase(record)
			switch notificationRecordPhase(record) {
			case notification.PhaseDelivered:
				delivered++
			case notification.PhaseFailed:
				failed++
			}
		}
		if delivered == 2 && failed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the delivery records did not settle: %v (webhook messages %d, log %s)", phases, hook.count(), logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	recordByEventAndChannel := func(eventName, channel string) *unstructured.Unstructured {
		for _, request := range report.Created {
			record := readRecord(cluster, request.Name)
			if notificationRecordChannel(record) == channel && recordEventName(record) == eventName {
				return record
			}
		}
		return nil
	}
	delivered := recordByEventAndChannel("warn-newest", okChannel)
	failed := recordByEventAndChannel("warn-recent", brokenChannel)
	if delivered == nil || failed == nil {
		t.Fatal("the records of both channels are not readable")
	}
	if attempts := recordAttempts(delivered); attempts != 1 {
		t.Fatalf("delivered record attempts = %d, want one", attempts)
	}
	requireReconciled(t, delivered)
	requireCondition(t, failed, notification.ConditionDeliveryFailed)
	if reason := recordConditionReason(t, failed, notification.ConditionDeliveryFailed); reason != "ChannelRejected" {
		t.Fatalf("the refused delivery reason = %q, want the channel's rejection", reason)
	}

	// The external channel received the two matches with the control plane's
	// credential, and nothing else.
	if hook.count() != 2 {
		t.Fatalf("the webhook received %d messages, want the two matches", hook.count())
	}
	events := hook.eventNames()
	for _, name := range []string{"warn-newest", "warn-recent"} {
		if _, seen := events[name]; !seen {
			t.Fatalf("the webhook messages cover %v, want %s", events, name)
		}
	}
	for _, authorization := range hook.authorizations() {
		if authorization != "Bearer "+channelCredential {
			t.Fatalf("authorization = %q, want the control plane's channel credential", authorization)
		}
	}
	if !strings.Contains(logs.String(), "notification delivery failed") {
		t.Fatalf("the failed delivery was not logged: %s", logs.String())
	}

	// The credential is in the control plane alone: not in the records, the log
	// or the setting projection (C-14).
	projection := settingsProjection(t, shell, ownerToken)
	for _, request := range report.Created {
		record := readRecord(cluster, request.Name)
		if record == nil {
			t.Fatalf("record %s is not readable", request.Name)
		}
		encoded, err := json.Marshal(record.Object)
		if err != nil {
			t.Fatalf("encode the record: %v", err)
		}
		for _, secret := range []string{channelCredential, brokenCredential} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("the delivery record carries the channel credential: %s", encoded)
			}
			if strings.Contains(logs.String(), secret) {
				t.Fatalf("the product log carries the channel credential: %s", logs.String())
			}
			if strings.Contains(projection, secret) {
				t.Fatalf("the settings projection carries the channel credential: %s", projection)
			}
		}
	}
	if !strings.Contains(projection, okChannel) {
		t.Fatalf("the settings projection does not show the configured channel: %s", projection)
	}

	// A repeated read creates nothing new and posts nothing again.
	repeated, err := service.Deliver(context.Background())
	if err != nil {
		t.Fatalf("deliver again: %v", err)
	}
	if len(repeated.Created) != 0 || repeated.Skipped != 4 {
		t.Fatalf("repeated delivery = %+v, want four skipped matches", repeated)
	}
	if hook.count() != 2 {
		t.Fatalf("the repeated read posted %d messages, want no new one", hook.count())
	}

	// A member may write the cluster-scoped channel setting, so a channel id's
	// endpoint can be pointed somewhere else. The control plane registered the
	// credential for the original address; the rewritten one gets no request
	// and no credential, and the record says why (C-14).
	attacker := &webhookRecorder{}
	attackerServer := httptest.NewServer(http.HandlerFunc(attacker.serve))
	t.Cleanup(attackerServer.Close)
	member := createAccount(t, shell, ownerToken, "carol", "carol-password", write.RoleMember)
	writeSetting(t, shell, member, notification.SettingChannels, `{"channels":[`+
		`{"id":"`+okChannel+`","kind":"webhook","endpoint":"`+attackerServer.URL+`"},`+
		`{"id":"`+brokenChannel+`","kind":"webhook","endpoint":"`+brokenEndpoint+`"}]}`)
	cluster.CreateClusterObject(t, "", "v1", "events", harness.Namespace,
		eventManifest("warn-redirect", "Warning", "BackOff", deliveryTime.Add(-2*time.Minute)))
	redirected, err := service.Deliver(context.Background())
	if err != nil {
		t.Fatalf("deliver after the endpoint rewrite: %v", err)
	}
	if len(redirected.Created) != 2 {
		t.Fatalf("created %d records after the rewrite, want one per channel", len(redirected.Created))
	}
	for _, request := range redirected.Created {
		record := waitForFailedRecord(t, cluster, request.Name)
		reason := recordConditionReason(t, record, notification.ConditionDeliveryFailed)
		wanted := "ChannelRejected"
		if notificationRecordChannel(record) == okChannel {
			wanted = "ChannelEndpointMismatch"
		}
		if reason != wanted {
			t.Fatalf("record of channel %s failed with %q, want %q", notificationRecordChannel(record), reason, wanted)
		}
	}
	if attacker.count() != 0 {
		t.Fatalf("the rewritten endpoint received %d messages, want none", attacker.count())
	}
}

// waitForFailedRecord waits until one delivery record carries a failed phase.
func waitForFailedRecord(t *testing.T, cluster *harness.Cluster, name string) *unstructured.Unstructured {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		record := readRecord(cluster, name)
		if record != nil && notificationRecordPhase(record) == notification.PhaseFailed {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("record %s did not reach a failed phase: %v", name, record)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTC114FileManagerServesOnlyExposedOwnedVolumes(t *testing.T) {
	cluster := harness.Start(t)
	shell := cluster.StartShell(t)
	ownerToken := shell.SignInOwner(t, "owner", "owner-password")
	alice := createAccount(t, shell, ownerToken, "alice", "alice-password", write.RoleMember)
	bob := createAccount(t, shell, ownerToken, "bob", "bob-password", write.RoleMember)
	artifacts := extension.MemoryArtifacts{}
	installed := cluster.InstallPackage(t, filemanagerComponent, filemanager.Declaration(), artifacts)
	shell.Use(installed)
	requireReady(t, installed, filemanagerComponent)

	// Two volumes with different owners and the directories the data plane
	// serves for them.
	root := t.TempDir()
	for _, volume := range []struct{ name, owner string }{
		{"alice-docs", "alice"},
		{"bob-data", "bob"},
	} {
		cluster.Create(t, harness.Volume(harness.Namespace, volume.name, map[string]any{
			"mobility":     "cluster-movable",
			"capacity":     "1Gi",
			"storageClass": "standard",
			"owner":        volume.owner,
		}))
	}
	aliceRoot := filepath.Join(root, harness.Namespace, "alice-docs")
	if err := os.MkdirAll(filepath.Join(aliceRoot, "photos"), 0o755); err != nil {
		t.Fatalf("prepare the volume directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, harness.Namespace, "bob-data"), 0o755); err != nil {
		t.Fatalf("prepare the second volume directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aliceRoot, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("prepare the volume file: %v", err)
	}

	service := filemanager.New(filemanager.Config{
		Settings:    shell.Accounts.Store,
		Preferences: shell.Accounts.Service,
		Volumes:     filemanager.ClusterVolumes{Writer: cluster.Writer()},
		Roots:       filemanager.DirectoryRoots{Base: root},
	})
	dataPlane := httptest.NewServer(shell.Accounts.API.Handler(service.Handler()))
	t.Cleanup(dataPlane.Close)

	// Alice exposes her own volume and stores the window's preferences.
	writeSetting(t, shell, alice, filemanager.SettingExposedVolumes, `["`+harness.Namespace+`/alice-docs"]`)
	writePreference(t, shell, alice, filemanager.PreferenceView, filemanager.ViewGrid)
	writePreference(t, shell, alice, filemanager.PreferenceInitialPath, "photos")

	status, body := dataCall(t, dataPlane.URL, http.MethodGet, "/files/window", nil, "", alice)
	if status != http.StatusOK {
		t.Fatalf("open the window = %d %s", status, body)
	}
	window := decodeWindow(t, body)
	if window.View != filemanager.ViewGrid || window.InitialPath != "photos" {
		t.Fatalf("window = %+v, want the stored view and initial path", window)
	}
	if len(window.Volumes) != 1 || window.Volumes[0].Name != "alice-docs" {
		t.Fatalf("window volumes = %+v, want the one exposed volume", window.Volumes)
	}

	// Browsing, download, upload, rename and delete all stay inside the volume.
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/alice-docs", nil, "", alice)
	if status != http.StatusOK {
		t.Fatalf("list the volume = %d %s", status, body)
	}
	if !strings.Contains(string(body), `"notes.txt"`) {
		t.Fatalf("listing = %s, want the file", body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/alice-docs/content?path=notes.txt", nil, "", alice)
	if status != http.StatusOK || string(body) != "hello" {
		t.Fatalf("download = %d %q, want the stored content", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodPut, "/files/"+harness.Namespace+"/alice-docs/content?path=photos/new.txt", []byte("photo"), "application/octet-stream", alice)
	if status != http.StatusCreated {
		t.Fatalf("upload = %d %s", status, body)
	}
	rename := []byte(`{"from":"photos/new.txt","to":"photos/renamed.txt"}`)
	status, body = dataCall(t, dataPlane.URL, http.MethodPost, "/files/"+harness.Namespace+"/alice-docs/rename", rename, "application/json", alice)
	if status != http.StatusOK {
		t.Fatalf("rename = %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(aliceRoot, "photos", "renamed.txt")); err != nil {
		t.Fatalf("the renamed file is missing: %v", err)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodDelete, "/files/"+harness.Namespace+"/alice-docs/content?path=photos/renamed.txt", nil, "", alice)
	if status != http.StatusNoContent {
		t.Fatalf("delete = %d %s", status, body)
	}

	// A path outside the volume is refused instead of normalised (NG10).
	for _, path := range []string{"../../etc/passwd", "/etc/passwd"} {
		status, body = dataCall(t, dataPlane.URL, http.MethodGet,
			"/files/"+harness.Namespace+"/alice-docs/content?path="+path, nil, "", alice)
		if status != http.StatusBadRequest {
			t.Fatalf("download %q = %d %s, want 400", path, status, body)
		}
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodPut,
		"/files/"+harness.Namespace+"/alice-docs/content?path=../escape.txt", []byte("x"), "application/octet-stream", alice)
	if status != http.StatusBadRequest {
		t.Fatalf("upload outside the volume = %d %s, want 400", status, body)
	}

	// A symbolic link inside the volume is not a way out of it: the data plane
	// addresses the volume through a root handle, which refuses a path that
	// leaves it (NG10).
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("outside-secret"), 0o644); err != nil {
		t.Fatalf("prepare the file outside the volume: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(aliceRoot, "link")); err != nil {
		t.Fatalf("plant the link inside the volume: %v", err)
	}
	linked := "/files/" + harness.Namespace + "/alice-docs/content?path=link/secret.txt"
	if status, body = dataCall(t, dataPlane.URL, http.MethodGet, linked, nil, "", alice); status == http.StatusOK {
		t.Fatalf("a link inside the volume served a file outside it: %d %s", status, body)
	}
	if status, body = dataCall(t, dataPlane.URL, http.MethodPut, linked, []byte("pwned"), "application/octet-stream", alice); status == http.StatusCreated {
		t.Fatalf("an upload through the link was accepted: %d %s", status, body)
	}
	if status, body = dataCall(t, dataPlane.URL, http.MethodDelete, linked, nil, "", alice); status == http.StatusNoContent {
		t.Fatalf("a delete through the link was accepted: %d %s", status, body)
	}
	if content, err := os.ReadFile(outsideFile); err != nil || string(content) != "outside-secret" {
		t.Fatalf("the file outside the volume changed: %q, %v", content, err)
	}
	if status, body = dataCall(t, dataPlane.URL, http.MethodGet,
		"/files/"+harness.Namespace+"/alice-docs?path=link", nil, "", alice); status == http.StatusOK {
		t.Fatalf("the link was listed as a directory of the volume: %d %s", status, body)
	}

	// A file name with a quote or a non-ASCII character still produces a header
	// the client can parse.
	oddName := `报价"单".txt`
	if err := os.WriteFile(filepath.Join(aliceRoot, oddName), []byte("odd"), 0o644); err != nil {
		t.Fatalf("prepare the odd file: %v", err)
	}
	disposition, downloaded := downloadHeader(t, dataPlane.URL, alice,
		"/files/"+harness.Namespace+"/alice-docs/content?path="+url.QueryEscape(oddName))
	if string(downloaded) != "odd" {
		t.Fatalf("download %q = %q", oddName, downloaded)
	}
	mediaType, parameters, err := mime.ParseMediaType(disposition)
	if err != nil || mediaType != "attachment" || parameters["filename"] != oddName {
		t.Fatalf("Content-Disposition = %q (%v), want the file name", disposition, err)
	}

	// A volume the account did not expose is simply absent; exposing another
	// account's volume is refused by the owner rule (C-60).
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", alice)
	if status != http.StatusNotFound {
		t.Fatalf("list an unexposed volume = %d %s, want 404", status, body)
	}
	writeSetting(t, shell, alice, filemanager.SettingExposedVolumes,
		`["`+harness.Namespace+`/alice-docs","`+harness.Namespace+`/bob-data"]`)
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", alice)
	if status != http.StatusForbidden || !strings.Contains(string(body), controlplane.CodePermissionDenied) {
		t.Fatalf("list another account's volume = %d %s, want permission_denied", status, body)
	}
	writeSetting(t, shell, bob, filemanager.SettingExposedVolumes, `["`+harness.Namespace+`/bob-data"]`)
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", bob)
	if status != http.StatusOK {
		t.Fatalf("list the second account's own volume = %d %s, want 200", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/"+harness.Namespace+"/bob-data", nil, "", alice)
	if status != http.StatusForbidden {
		t.Fatalf("list the second account's volume as another member = %d %s, want 403", status, body)
	}

	// The data plane authenticates the session itself (C-46): a request without
	// one is refused, and a forwarding header does not stand in for it.
	status, body = dataCall(t, dataPlane.URL, http.MethodGet, "/files/window", nil, "", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("open the window without a session = %d %s, want 401", status, body)
	}
	forged := map[string]string{"X-Forwarded-User": "alice", "X-Auth-Request-User": "alice"}
	status, body = dataCallWithHeaders(t, dataPlane.URL, http.MethodGet, "/files/window", nil, "", "", forged)
	if status != http.StatusUnauthorized {
		t.Fatalf("open the window with a forwarded user = %d %s, want 401", status, body)
	}

	// A Guest reads but never writes (C-59): upload, rename and delete are
	// refused.
	guest := createAccount(t, shell, ownerToken, "guest", "guest-password", write.RoleGuest)
	status, body = dataCall(t, dataPlane.URL, http.MethodPut,
		"/files/"+harness.Namespace+"/alice-docs/content?path=notes.txt", []byte("pwned"), "application/octet-stream", guest)
	if status != http.StatusForbidden {
		t.Fatalf("a guest upload = %d %s, want 403", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodPost, "/files/"+harness.Namespace+"/alice-docs/rename",
		[]byte(`{"from":"notes.txt","to":"moved.txt"}`), "application/json", guest)
	if status != http.StatusForbidden {
		t.Fatalf("a guest rename = %d %s, want 403", status, body)
	}
	status, body = dataCall(t, dataPlane.URL, http.MethodDelete,
		"/files/"+harness.Namespace+"/alice-docs/content?path=notes.txt", nil, "", guest)
	if status != http.StatusForbidden {
		t.Fatalf("a guest delete = %d %s, want 403", status, body)
	}
	if content, err := os.ReadFile(filepath.Join(aliceRoot, "notes.txt")); err != nil || string(content) != "hello" {
		t.Fatalf("a guest changed the file: %q, %v", content, err)
	}

	// The control plane offers no file interface: the data plane is the
	// product's own service (NG10).
	status, _ = shell.Call(t, http.MethodGet, "/files/window", nil, alice)
	if status != http.StatusNotFound {
		t.Fatalf("the control plane answered the data-plane path with %d, want 404", status)
	}
	if len(filemanager.Declaration().Contributions.Pages) != 0 {
		t.Fatal("the file manager declares a panel page; its interface is an application window")
	}

	// Removing the product withdraws the setting and the preference schema.
	cluster.DeleteComponent(t, filemanagerComponent)
	registry := cluster.ExtensionState(t, artifacts).Registry()
	if _, found := registry.Setting(filemanager.SettingExposedVolumes); found {
		t.Fatal("the exposed-volume setting survived the uninstall")
	}
	if _, found := registry.PreferenceKeys(filemanager.Identifier); found {
		t.Fatal("the file manager preference schema survived the uninstall")
	}
}

// requireReady checks one component installed with its contributions loaded.
func requireReady(t *testing.T, installation *extension.Installation, name string) {
	t.Helper()
	component, found := installation.Component(name)
	if !found {
		t.Fatalf("component %s is missing from the install result", name)
	}
	if component.Phase != extension.PhaseReady {
		t.Fatalf("component %s = %s (%s: %s), want ready", name, component.Phase, component.Reason, component.Message)
	}
}

// requireController checks one controller contribution is loaded for a class.
func requireController(t *testing.T, registry *extension.Registry, id, group, kind string) {
	t.Helper()
	for _, controller := range registry.Controllers() {
		if controller.ID != id {
			continue
		}
		for _, class := range controller.Manages {
			if class.Group == group && class.Kind == kind {
				return
			}
		}
	}
	t.Fatalf("controller %s managing %s/%s is not loaded", id, group, kind)
}

// requireNoPages checks the products added no page to the shell.
func requireNoPages(t *testing.T, registry *extension.Registry) {
	t.Helper()
	if len(registry.Pages()) != 0 {
		t.Fatalf("the shell gained pages %+v from products that declare none", registry.Pages())
	}
}

// requireKernelUnchanged compares the kernel sets with the ones recorded before
// the first install, including the refusal an extension controller gets on a
// kernel write (C-01, C-09, C-19).
func requireKernelUnchanged(t *testing.T, kinds []string, capabilities []extension.Capability, owned []string, denial string, cluster *harness.Cluster) {
	t.Helper()
	if !reflect.DeepEqual(api.Kinds(), kinds) {
		t.Fatalf("the kernel kind set changed: %v", api.Kinds())
	}
	if !reflect.DeepEqual(extension.BaselineCapabilities(), capabilities) {
		t.Fatal("the capability baseline changed")
	}
	if !reflect.DeepEqual(write.OwnedKinds(), owned) {
		t.Fatal("the writer table changed")
	}
	if observed := deniedKernelWrite(t, cluster); observed != denial {
		t.Fatalf("the kernel write refusal changed from %q to %q", denial, observed)
	}
}

// deniedKernelWrite attempts one extension controller write to a kernel class
// and returns the refusal, so the case can compare the writer table before and
// after installing products.
func deniedKernelWrite(t *testing.T, cluster *harness.Cluster) string {
	t.Helper()
	writer := cluster.Writer()
	writer.Scope = extension.WriteScope{Groups: []string{notification.APIGroup}}.Admit()
	_, err := writer.Create(context.Background(), write.SubjectExtensionReconciler, write.Session{}, &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": api.Group + "/v1beta1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "probe", "namespace": harness.Namespace},
		"spec":       map[string]any{"namespace": harness.Namespace},
	}})
	if err == nil {
		t.Fatal("an extension controller wrote a kernel object")
	}
	return err.Error()
}

// notificationWriter is the write path an installed delivery controller has:
// the declared scope plus the resource name of its own class.
func notificationWriter(t *testing.T, cluster *harness.Cluster, scope extension.WriteScope) *write.Writer {
	t.Helper()
	writer := cluster.Writer()
	writer.Scope = scope.Admit()
	writer.Resources[schema.GroupKind{Group: notification.APIGroup, Kind: notification.Kind}] = notification.Resource
	return writer
}

// queueFunc adapts a function to the delivery queue.
type queueFunc func(request reconcile.Request) error

// Enqueue asks for one delivery round.
func (q queueFunc) Enqueue(request reconcile.Request) error { return q(request) }

// writeSetting stores one setting value through the control-plane API.
func writeSetting(t *testing.T, shell *harness.Shell, token, id, value string) {
	t.Helper()
	status, body := shell.Call(t, http.MethodPut, "/api/v1/settings/"+id, map[string]any{"value": value}, token)
	if status != http.StatusOK {
		t.Fatalf("write setting %s = %d %v", id, status, body)
	}
}

// writePreference stores one preference value for the calling account.
func writePreference(t *testing.T, shell *harness.Shell, token, key, value string) {
	t.Helper()
	status, body := shell.Call(t, http.MethodPut,
		"/api/v1/preferences/"+filemanager.Identifier+"/"+key, map[string]any{"value": value}, token)
	if status != http.StatusNoContent {
		t.Fatalf("write preference %s = %d %v", key, status, body)
	}
}

// createAccount creates one account through the control-plane API and returns a
// token issued after its one-time password was replaced.
func createAccount(t *testing.T, shell *harness.Shell, ownerToken, username, password string, role write.Role) string {
	t.Helper()
	status, body := shell.Call(t, http.MethodPost, "/api/v1/accounts", map[string]any{
		"username": username, "role": string(role),
	}, ownerToken)
	if status != http.StatusCreated {
		t.Fatalf("create account %s = %d %v", username, status, body)
	}
	oneTime, _ := body["oneTimePassword"].(string)
	if oneTime == "" {
		t.Fatalf("create account %s returned no one-time password: %v", username, body)
	}
	token, _ := shell.Accounts.SignIn(t, username, oneTime)
	shell.Accounts.ChangePassword(t, token, oneTime, password)
	token, _ = shell.Accounts.SignIn(t, username, password)
	return token
}

// eventManifest builds one stored event of the window.
func eventManifest(name, eventType, reason string, lastSeen time.Time) map[string]any {
	return map[string]any{
		"apiVersion":    "v1",
		"kind":          "Event",
		"metadata":      map[string]any{"name": name, "namespace": harness.Namespace},
		"type":          eventType,
		"reason":        reason,
		"message":       reason + " on the shop application",
		"count":         int64(1),
		"lastTimestamp": lastSeen.UTC().Format(time.RFC3339Nano),
		"involvedObject": map[string]any{
			"apiVersion": api.Group + "/v1beta1", "kind": "Application",
			"namespace": harness.Namespace, "name": "shop",
		},
	}
}

// webhookRecorder is the external channel the case delivers to.
type webhookRecorder struct {
	mu       sync.Mutex
	messages []notification.Message
	tokens   []string
}

// serve records one posted message.
func (w *webhookRecorder) serve(response http.ResponseWriter, request *http.Request) {
	var message notification.Message
	if err := json.NewDecoder(request.Body).Decode(&message); err != nil {
		response.WriteHeader(http.StatusBadRequest)
		return
	}
	w.mu.Lock()
	w.messages = append(w.messages, message)
	w.tokens = append(w.tokens, request.Header.Get("Authorization"))
	w.mu.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

// count reports how many messages the channel accepted.
func (w *webhookRecorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.messages)
}

// eventNames reports the events the channel accepted.
func (w *webhookRecorder) eventNames() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make(map[string]bool, len(w.messages))
	for _, message := range w.messages {
		names[message.Event.Name] = true
	}
	return names
}

// authorizations returns the authorization header of every accepted message.
func (w *webhookRecorder) authorizations() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.tokens...)
}

// settingsProjection reads the settings projection as the owner sees it.
func settingsProjection(t *testing.T, shell *harness.Shell, token string) string {
	t.Helper()
	status, body := shell.Call(t, http.MethodGet, "/api/v1/settings", nil, token)
	if status != http.StatusOK {
		t.Fatalf("read the settings = %d %v", status, body)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the settings projection: %v", err)
	}
	return string(encoded)
}

// readRecord reads one delivery record; nil means the record is not readable.
func readRecord(cluster *harness.Cluster, name string) *unstructured.Unstructured {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	object, err := cluster.ExtensionResource(notification.APIGroup, notification.ObjectVersion, notification.Resource, "").
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return object
}

// notificationRecordChannel reads the channel a record targets.
func notificationRecordChannel(record *unstructured.Unstructured) string {
	value, _, _ := unstructured.NestedString(record.Object, "spec", "channel")
	return value
}

// notificationRecordPhase reads the record's own phase.
func notificationRecordPhase(record *unstructured.Unstructured) string {
	value, _, _ := unstructured.NestedString(record.Object, "status", "phase")
	return value
}

// recordEventName reads the event a record snapshots.
func recordEventName(record *unstructured.Unstructured) string {
	value, _, _ := unstructured.NestedString(record.Object, "spec", "event", "name")
	return value
}

// recordAttempts reads the attempts a record counted.
func recordAttempts(record *unstructured.Unstructured) int64 {
	value, _, _ := unstructured.NestedInt64(record.Object, "status", "attempts")
	return value
}

// requireReconciled checks a record carries Reconciled=True.
func requireReconciled(t *testing.T, record *unstructured.Unstructured) {
	t.Helper()
	for _, condition := range recordConditions(t, record) {
		if condition.Type == api.ConditionReconciled {
			if string(condition.Status) != "True" {
				t.Fatalf("Reconciled = %s, want True", condition.Status)
			}
			return
		}
	}
	t.Fatalf("record %s carries no Reconciled condition", record.GetName())
}

// requireCondition checks a record carries one condition of its own type.
func requireCondition(t *testing.T, record *unstructured.Unstructured, conditionType string) {
	t.Helper()
	for _, condition := range recordConditions(t, record) {
		if condition.Type == conditionType && string(condition.Status) == "True" {
			return
		}
	}
	t.Fatalf("record %s carries no %s=True condition", record.GetName(), conditionType)
}

// recordConditionReason reads the machine-readable reason of one condition.
func recordConditionReason(t *testing.T, record *unstructured.Unstructured, conditionType string) string {
	t.Helper()
	for _, condition := range recordConditions(t, record) {
		if condition.Type == conditionType {
			return condition.Reason
		}
	}
	t.Fatalf("record %s carries no %s condition", record.GetName(), conditionType)
	return ""
}

// recordConditions reads the conditions of one record.
func recordConditions(t *testing.T, record *unstructured.Unstructured) []metav1.Condition {
	t.Helper()
	value, found, err := unstructured.NestedSlice(record.Object, "status", "conditions")
	if err != nil || !found {
		t.Fatalf("record %s has no readable conditions: found=%v err=%v", record.GetName(), found, err)
	}
	conditions := make([]metav1.Condition, 0, len(value))
	for _, item := range value {
		fields, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("condition %v is not an object", item)
		}
		conditions = append(conditions, metav1.Condition{
			Type:   asString(fields["type"]),
			Status: metav1.ConditionStatus(asString(fields["status"])),
			Reason: asString(fields["reason"]),
		})
	}
	return conditions
}

// asString reads one string field of a decoded condition.
func asString(value any) string {
	text, _ := value.(string)
	return text
}

// dataCall sends one request to the product's data plane and returns the status
// and the raw body.
func dataCall(t *testing.T, base, method, path string, body []byte, contentType, token string) (int, []byte) {
	t.Helper()
	return dataCallWithHeaders(t, base, method, path, body, contentType, token, nil)
}

// downloadHeader sends one download and returns the Content-Disposition header
// with the body, so a case can read how the data plane presents a file name.
func downloadHeader(t *testing.T, base, token, path string) (string, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("build the download: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("download %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the download: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("download %s = %d %s", path, response.StatusCode, body)
	}
	return response.Header.Get("Content-Disposition"), body
}

// dataCallWithHeaders sends one request with extra headers, so a case can
// present a forwarded user the data plane must not trust (C-46).
func dataCallWithHeaders(t *testing.T, base, method, path string, body []byte, contentType, token string, headers map[string]string) (int, []byte) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, base+path, payload)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response.StatusCode, answer
}

// decodeWindow reads the window response.
func decodeWindow(t *testing.T, body []byte) filemanager.Window {
	t.Helper()
	var window filemanager.Window
	if err := json.Unmarshal(body, &window); err != nil {
		t.Fatalf("decode the window %q: %v", body, err)
	}
	return window
}
