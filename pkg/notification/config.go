package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/tsic404/kos/pkg/controlplane"
	"github.com/tsic404/kos/pkg/extension"
)

// Channel is one external destination a delivery may target. The credential is
// not part of it: it is read from the control plane by channel id when the
// message is sent (C-14).
type Channel struct {
	// ID names the channel inside the subscription setting.
	ID string `json:"id"`
	// Kind selects the sender, for example webhook.
	Kind string `json:"kind"`
	// Endpoint is the address the sender posts to.
	Endpoint string `json:"endpoint"`
}

// Subscription maps matching events to channels. An empty Types or Reasons
// admits every value; Channels must name at least one channel.
type Subscription struct {
	// ID names the subscription, and is carried on the delivery record.
	ID string `json:"id"`
	// Types matches the event type, Normal or Warning.
	Types []string `json:"types,omitempty"`
	// Reasons matches the event reason.
	Reasons []string `json:"reasons,omitempty"`
	// Channels lists the channel ids one match is delivered to.
	Channels []string `json:"channels"`
}

// matches reports whether the subscription wants the event.
func (s Subscription) matches(event controlplane.Event) bool {
	return matchesAny(s.Types, event.Type) && matchesAny(s.Reasons, event.Reason)
}

// matchesAny applies one filter list; an empty list admits every value.
func matchesAny(wanted []string, value string) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, candidate := range wanted {
		if candidate == value {
			return true
		}
	}
	return false
}

// channelsDocument is the shape of the channels setting value.
type channelsDocument struct {
	Channels []Channel `json:"channels"`
}

// subscriptionsDocument is the shape of the subscriptions setting value.
type subscriptionsDocument struct {
	Subscriptions []Subscription `json:"subscriptions"`
}

// ParseChannels reads the channels setting value. An empty value declares no
// channel; any other value has to be a well-formed document, because a
// mistyped setting must not silently deliver nothing.
func ParseChannels(raw string) ([]Channel, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var document channelsDocument
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return nil, fmt.Errorf("channels setting is not a JSON document: %w", err)
	}
	seen := map[string]bool{}
	for index, channel := range document.Channels {
		switch {
		case channel.ID == "":
			return nil, fmt.Errorf("channels[%d] names no id", index)
		case channel.Kind == "":
			return nil, fmt.Errorf("channel %q names no kind", channel.ID)
		case channel.Endpoint == "":
			return nil, fmt.Errorf("channel %q names no endpoint", channel.ID)
		case !isWebhookEndpoint(channel.Endpoint):
			return nil, fmt.Errorf("channel %q endpoint is not an http or https URL", channel.ID)
		case seen[channel.ID]:
			return nil, fmt.Errorf("channel %q is declared twice", channel.ID)
		}
		seen[channel.ID] = true
	}
	return document.Channels, nil
}

// ParseSubscriptions reads the subscriptions setting value under the same rule.
func ParseSubscriptions(raw string) ([]Subscription, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var document subscriptionsDocument
	if err := json.Unmarshal([]byte(raw), &document); err != nil {
		return nil, fmt.Errorf("subscriptions setting is not a JSON document: %w", err)
	}
	seen := map[string]bool{}
	for index, subscription := range document.Subscriptions {
		switch {
		case subscription.ID == "":
			return nil, fmt.Errorf("subscriptions[%d] names no id", index)
		case len(subscription.Channels) == 0:
			return nil, fmt.Errorf("subscription %q names no channel", subscription.ID)
		case seen[subscription.ID]:
			return nil, fmt.Errorf("subscription %q is declared twice", subscription.ID)
		}
		seen[subscription.ID] = true
	}
	return document.Subscriptions, nil
}

// isWebhookEndpoint reports whether the endpoint is a URL a webhook can post
// to; a channel address of another scheme is a setting mistake.
func isWebhookEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// Store reads the setting values the control plane holds (C-23).
type Store interface {
	SettingValues(ctx context.Context, scope, target string) (map[string]string, error)
}

// Credential is the control plane's registration of one channel: the secret and
// the endpoint it was registered for. The channel setting is written by users,
// so the endpoint travels with the secret: a delivery only presents the secret
// to the address the control plane registered, which keeps a rewritten endpoint
// from receiving it (C-14).
type Credential struct {
	// Endpoint is the address the secret was registered for.
	Endpoint string
	// Secret is the value the channel authenticates with.
	Secret string
}

// Credentials is the control plane's own credential store (C-14): a channel
// secret is held by the process and handed to the sender at delivery time, so
// it never reaches a cluster object, the declaration or a log.
type Credentials interface {
	// ChannelCredential returns the registration of one channel; ok is false
	// when the channel has none, which is a channel without authentication.
	ChannelCredential(ctx context.Context, channel string) (credential Credential, ok bool, err error)
}

// StaticCredentials is a credential store held by the process that runs the
// control plane, keyed by channel id.
type StaticCredentials map[string]Credential

// ChannelCredential returns the registration of one channel.
func (c StaticCredentials) ChannelCredential(_ context.Context, channel string) (Credential, bool, error) {
	credential, ok := c[channel]
	return credential, ok, nil
}

// settingValues reads both settings in one call.
func settingValues(ctx context.Context, store Store) (string, string, error) {
	if store == nil {
		return "", "", nil
	}
	values, err := store.SettingValues(ctx, extension.SettingScopeCluster, "")
	if err != nil {
		return "", "", fmt.Errorf("read notification settings: %w", err)
	}
	return values[SettingChannels], values[SettingSubscriptions], nil
}
