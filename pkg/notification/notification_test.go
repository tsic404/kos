package notification

import (
	"testing"
	"time"

	"github.com/tsic404/kos/pkg/controlplane"
)

// TestParseChannelsRejectsMalformedValues checks that a setting the user has to
// correct is reported instead of delivering nothing silently.
func TestParseChannelsRejectsMalformedValues(t *testing.T) {
	if channels, err := ParseChannels(""); err != nil || channels != nil {
		t.Fatalf("empty value = %v, %v; want no channel and no error", channels, err)
	}
	channels, err := ParseChannels(`{"channels":[{"id":"ops","kind":"webhook","endpoint":"https://hooks.example.test/notify"}]}`)
	if err != nil {
		t.Fatalf("parse one channel: %v", err)
	}
	if len(channels) != 1 || channels[0].ID != "ops" {
		t.Fatalf("channels = %+v, want the declared channel", channels)
	}
	for name, value := range map[string]string{
		"no id":       `{"channels":[{"kind":"webhook","endpoint":"https://hooks.example.test"}]}`,
		"no kind":     `{"channels":[{"id":"ops","endpoint":"https://hooks.example.test"}]}`,
		"no endpoint": `{"channels":[{"id":"ops","kind":"webhook"}]}`,
		"bad scheme":  `{"channels":[{"id":"ops","kind":"webhook","endpoint":"file:///etc/passwd"}]}`,
		"duplicate":   `{"channels":[{"id":"ops","kind":"webhook","endpoint":"https://a.example.test"},{"id":"ops","kind":"webhook","endpoint":"https://b.example.test"}]}`,
		"not json":    `ops=webhook`,
	} {
		if _, err := ParseChannels(value); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// TestParseSubscriptionsRejectsMalformedValues checks the subscription document.
func TestParseSubscriptionsRejectsMalformedValues(t *testing.T) {
	subscriptions, err := ParseSubscriptions(`{"subscriptions":[{"id":"warnings","types":["Warning"],"channels":["ops"]}]}`)
	if err != nil {
		t.Fatalf("parse one subscription: %v", err)
	}
	if len(subscriptions) != 1 || subscriptions[0].Channels[0] != "ops" {
		t.Fatalf("subscriptions = %+v, want the declared rule", subscriptions)
	}
	for name, value := range map[string]string{
		"no id":      `{"subscriptions":[{"channels":["ops"]}]}`,
		"no channel": `{"subscriptions":[{"id":"warnings"}]}`,
		"duplicate":  `{"subscriptions":[{"id":"w","channels":["ops"]},{"id":"w","channels":["ops"]}]}`,
	} {
		if _, err := ParseSubscriptions(value); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// TestSubscriptionMatches applies the type and reason filters.
func TestSubscriptionMatches(t *testing.T) {
	event := controlplane.Event{Type: "Warning", Reason: "BackOff"}
	cases := []struct {
		name         string
		subscription Subscription
		wanted       bool
	}{
		{"no filter", Subscription{ID: "all", Channels: []string{"ops"}}, true},
		{"type matches", Subscription{ID: "warnings", Types: []string{"Warning"}, Channels: []string{"ops"}}, true},
		{"type differs", Subscription{ID: "normal", Types: []string{"Normal"}, Channels: []string{"ops"}}, false},
		{"reason matches", Subscription{ID: "backoff", Reasons: []string{"BackOff"}, Channels: []string{"ops"}}, true},
		{"reason differs", Subscription{ID: "other", Reasons: []string{"FailedMount"}, Channels: []string{"ops"}}, false},
	}
	for _, test := range cases {
		if observed := test.subscription.matches(event); observed != test.wanted {
			t.Fatalf("%s matched = %v, want %v", test.name, observed, test.wanted)
		}
	}
}

// TestRecordNameIsStableAndDistinct checks that a repeated read creates no
// second record while another channel still gets its own.
func TestRecordNameIsStableAndDistinct(t *testing.T) {
	event := controlplane.Event{
		Name: "backoff", Namespace: "default", Type: "Warning", Reason: "BackOff",
		LastSeen: time.Date(2026, 3, 1, 11, 55, 0, 0, time.UTC),
		InvolvedObject: controlplane.ObjectRef{
			Group: "kos.tsic.top", Kind: "Application", Namespace: "default", Name: "shop",
		},
	}
	first := recordName(event, "warnings", "ops")
	if second := recordName(event, "warnings", "ops"); second != first {
		t.Fatalf("the same delivery got two names: %q and %q", first, second)
	}
	if other := recordName(event, "warnings", "chat"); other == first {
		t.Fatal("two channels share one record name")
	}
	if other := recordName(event, "digest", "ops"); other == first {
		t.Fatalf("two subscriptions share one record name: %q", first)
	}
}

// TestDecodeEventNeedsASubjectAndATime checks what the window read skips.
func TestDecodeEventNeedsASubjectAndATime(t *testing.T) {
	valid := map[string]any{
		"metadata":      map[string]any{"name": "backoff", "namespace": "default"},
		"type":          "Warning",
		"reason":        "BackOff",
		"message":       "restarting",
		"lastTimestamp": "2026-03-01T11:55:00Z",
		"involvedObject": map[string]any{
			"apiVersion": "kos.tsic.top/v1beta1", "kind": "Application",
			"namespace": "default", "name": "shop",
		},
	}
	event, ok := decodeEvent(valid)
	if !ok {
		t.Fatal("the complete event was skipped")
	}
	if !event.LastSeen.Equal(time.Date(2026, 3, 1, 11, 55, 0, 0, time.UTC)) {
		t.Fatalf("last seen = %s", event.LastSeen)
	}
	if event.InvolvedObject.Group != "kos.tsic.top" || event.InvolvedObject.Kind != "Application" {
		t.Fatalf("involved object = %+v", event.InvolvedObject)
	}
	withoutSubject := map[string]any{"metadata": map[string]any{"name": "backoff"}, "lastTimestamp": "2026-03-01T11:55:00Z"}
	if _, ok := decodeEvent(withoutSubject); ok {
		t.Fatal("an event without a subject was read")
	}
	withoutTime := map[string]any{
		"metadata":       map[string]any{"name": "backoff"},
		"involvedObject": map[string]any{"kind": "Application", "name": "shop"},
	}
	if _, ok := decodeEvent(withoutTime); ok {
		t.Fatal("an event without a time was read")
	}
}
